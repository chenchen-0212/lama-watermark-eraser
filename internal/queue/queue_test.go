package queue

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeExecutor 测试用执行器：按钩子返回结果，记录调用轨迹。
// 钩子内如需阻塞，应同时监听 ctx.Done()，保证队列取消时执行器可退出。
type fakeExecutor struct {
	mu      sync.Mutex
	dlCalls []string // 依次执行的 download 任务 URL
	ipCalls []string // 依次执行的 inpaint 任务 InDir
	dlFn    func(ctx context.Context, t *BatchTask) (DownloadResult, error)
	ipFn    func(ctx context.Context, t *BatchTask) (InpaintResult, error)
	auFn    func(ctx context.Context, t *BatchTask) ([]string, string, error)
	started chan *BatchTask // 每次开始执行推入
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{started: make(chan *BatchTask, 64)}
}

func (f *fakeExecutor) RunDownload(ctx context.Context, t *BatchTask) (DownloadResult, error) {
	f.mu.Lock()
	f.dlCalls = append(f.dlCalls, t.URL)
	fn := f.dlFn
	f.mu.Unlock()
	f.started <- t
	if fn != nil {
		return fn(ctx, t)
	}
	return DownloadResult{
		Dir:     "/dl/" + t.URL,
		PostRef: "帖子A | 3 张",
		Files:   []string{"/dl/01.jpg", "/dl/02.jpg"},
	}, nil
}

func (f *fakeExecutor) ResolveTaskAudio(ctx context.Context, t *BatchTask) ([]string, string, error) {
	f.mu.Lock()
	fn := f.auFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, t)
	}
	return nil, "", nil
}

func (f *fakeExecutor) RunInpaint(ctx context.Context, t *BatchTask, onProgress ProgressFn) (InpaintResult, error) {
	f.mu.Lock()
	f.ipCalls = append(f.ipCalls, t.InDir)
	fn := f.ipFn
	f.mu.Unlock()
	f.started <- t
	if fn != nil {
		return fn(ctx, t)
	}
	if onProgress != nil {
		onProgress(1, 2, "a.png", "ok", "")
		onProgress(2, 2, "b.png", "fail", "boom")
	}
	return InpaintResult{OutDir: t.OutDir, OK: 1, Total: 2, FailList: []string{"b.png: boom"}}, nil
}

func (f *fakeExecutor) calls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dlCalls), len(f.ipCalls)
}

// waitFor 轮询等待条件成立（2s 超时则 Fatal）。
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg + "：等待超时")
}

func taskState(q *Queue, id string) string {
	for _, s := range q.Snapshot() {
		if s.ID == id {
			return s.State
		}
	}
	return ""
}

// newTestQueue 无持久化的队列。
func newTestQueue(exec Executor) *Queue {
	q := New(exec, nil, "")
	q.Start(context.Background())
	return q
}

// useFastBackoff 将退避间隔换成 5ms（测重试路径不等待真实 10s/60s/300s）。
func useFastBackoff(t *testing.T) {
	old := RetryBackoffs
	RetryBackoffs = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	t.Cleanup(func() { RetryBackoffs = old })
}

// TestCrossTypeParallel download 与 inpaint 并行执行（方案 O3 核心断言）：
// inpaint 阻塞运行时，download 照常被执行。
func TestCrossTypeParallel(t *testing.T) {
	exec := newFakeExecutor()
	block := make(chan struct{})
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return InpaintResult{}, ctx.Err()
	}
	q := newTestQueue(exec)
	defer func() {
		close(block)
		q.Stop()
	}()

	q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/slow", OutDir: "/slow_out"})
	<-exec.started // inpaint 开始阻塞

	// download 任务在 inpaint 阻塞期间应被另一个 worker 立即执行
	q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/fast"})
	select {
	case <-exec.started:
		// 已被调度执行
	case <-time.After(2 * time.Second):
		t.Fatal("inpaint 阻塞期间 download 未被执行——双 worker 并行失效")
	}
	dl, ip := exec.calls()
	if dl != 1 || ip != 1 {
		t.Fatalf("dl=%d ip=%d", dl, ip)
	}
}

// TestSameTypeSerial 同类任务仍串行：inpaint A 阻塞期间 B 不启动。
func TestSameTypeSerial(t *testing.T) {
	exec := newFakeExecutor()
	block := make(chan struct{})
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return InpaintResult{}, ctx.Err()
	}
	q := newTestQueue(exec)
	defer func() {
		close(block)
		q.Stop()
	}()

	q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/a", OutDir: "/a_out"})
	<-exec.started
	q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/b", OutDir: "/b_out"})
	time.Sleep(100 * time.Millisecond)
	for _, c := range exec.ipCalls {
		if c == "/b" {
			t.Error("同类任务不应并行：A 运行中 B 已启动")
		}
	}
}

// TestQueueCap 每类队列活跃上限 10 条（方案 O4）。
func TestQueueCap(t *testing.T) {
	exec := newFakeExecutor()
	block := make(chan struct{})
	exec.dlFn = func(ctx context.Context, t *BatchTask) (DownloadResult, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return DownloadResult{}, ctx.Err()
	}
	q := newTestQueue(exec)
	defer func() {
		close(block)
		q.Stop()
	}()

	// 首条进入 running，其余 9 条 pending → 10 条活跃
	for i := 0; i < MaxActivePerQueue; i++ {
		if _, err := q.Enqueue(&BatchTask{Type: TypeDownload, URL: fmt.Sprintf("https://x/%d", i)}); err != nil {
			t.Fatalf("第 %d 条不应被拒: %v", i+1, err)
		}
	}
	if _, err := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/over"}); err == nil {
		t.Error("第 11 条应被拒（下载队列已满）")
	} else if !strings.Contains(err.Error(), "下载队列已满") {
		t.Errorf("错误文案不符: %v", err)
	}
	// inpaint 队列独立计数，不受下载队列占满影响
	if _, err := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/a", OutDir: "/a_out"}); err != nil {
		t.Errorf("inpaint 队列应独立计数: %v", err)
	}
}

// TestRetryCap 重试同样受活跃上限约束；终态任务不计活跃。
// 构造：1 条永久失败（终态）+ 10 条阻塞活跃 → 重试被拒；
// 取消 1 条活跃后 → 重试放行。
func TestRetryCap(t *testing.T) {
	exec := newFakeExecutor()
	block := make(chan struct{})
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		if t.InDir == "/doomed" {
			return InpaintResult{}, Permanent(errors.New("doomed"))
		}
		select {
		case <-block:
		case <-ctx.Done():
		}
		return InpaintResult{}, ctx.Err()
	}
	q := newTestQueue(exec)
	defer func() {
		close(block)
		q.Stop()
	}()

	doomed, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/doomed", OutDir: "/doomed_out"})
	waitFor(t, func() bool { return taskState(q, doomed.ID) == StateFailed }, "永久失败任务应到 failed")

	// 填满 10 条活跃
	for i := 0; i < MaxActivePerQueue; i++ {
		tk, err := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: fmt.Sprintf("/b%d", i), OutDir: fmt.Sprintf("/b%d_out", i)})
		if err != nil {
			t.Fatalf("第 %d 条不应被拒: %v", i+1, err)
		}
		_ = tk
	}
	// 活跃已满：重试终态任务应被拒
	if err := q.Retry(doomed.ID); err == nil {
		t.Error("队列满载时重试应被拒绝")
	}
	// 取消一条活跃 → 腾出名额 → 重试放行
	var oneActive string
	for _, s := range q.Snapshot() {
		if s.State == StatePending && s.InDir == "/b9" {
			oneActive = s.ID
		}
	}
	if oneActive != "" {
		if err := q.Cancel(oneActive); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Retry(doomed.ID); err != nil {
		t.Fatalf("腾出名额后重试应放行: %v", err)
	}
	// 重试已入队（worker 被阻塞任务占住，不必等其执行完成）
	waitFor(t, func() bool { return taskState(q, doomed.ID) == StatePending }, "重试后应回到 pending")
}

// TestCancelRunningByType CancelRunning 按类型定向取消，互不影响。
func TestCancelRunningByType(t *testing.T) {
	exec := newFakeExecutor()
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
		}
		return InpaintResult{}, ctx.Err()
	}
	q := newTestQueue(exec)
	defer q.Stop()

	ip, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/a", OutDir: "/a_out"})
	<-exec.started
	// 取消 download 类型（无运行中）→ 不影响 inpaint
	q.CancelRunning(TypeDownload)
	time.Sleep(50 * time.Millisecond)
	if got := taskState(q, ip.ID); got != StateRunning {
		t.Errorf("CancelRunning(download) 不应影响 inpaint，实际 %s", got)
	}
	// 取消 inpaint 类型 → 生效
	q.CancelRunning(TypeInpaint)
	waitFor(t, func() bool { return taskState(q, ip.ID) == StateCanceled }, "定向取消未生效")
}

// TestSuccessDone 成功路径：done + 结果回填；部分单张失败不算任务失败（PRD §5.2）。
func TestSuccessDone(t *testing.T) {
	exec := newFakeExecutor()
	q := newTestQueue(exec)
	defer q.Stop()

	tk, err := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/in", OutDir: "/in_out"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateDone }, "任务未到 done")
	for _, s := range q.Snapshot() {
		if s.ID != tk.ID {
			continue
		}
		if s.Summary != "成功 1 / 失败或跳过 1 / 共 2" {
			t.Errorf("Summary = %q", s.Summary)
		}
		if len(s.FailList) != 1 {
			t.Errorf("FailList 应含 1 条，实际 %v", s.FailList)
		}
		if s.ResultDir != "/in_out" {
			t.Errorf("ResultDir = %q", s.ResultDir)
		}
		if s.FinishedAt == nil {
			t.Error("FinishedAt 未回填")
		}
	}
}

// TestTransientRetry 瞬时错误：失败两次后第三次成功 → done，attempts=2。
func TestTransientRetry(t *testing.T) {
	useFastBackoff(t)
	exec := newFakeExecutor()
	n := 0
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		n++
		if n < 3 {
			return InpaintResult{}, errors.New("网络超时")
		}
		return InpaintResult{OutDir: t.OutDir, OK: 2, Total: 2}, nil
	}
	q := newTestQueue(exec)
	defer q.Stop()

	tk, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/in", OutDir: "/in_out"})
	waitFor(t, func() bool {
		for _, s := range q.Snapshot() {
			if s.ID == tk.ID {
				return s.State == StateDone && s.Attempts == 2
			}
		}
		return false
	}, "重试后未到 done（attempts 应为 2）")
}

// TestRetryLimit 瞬时错误连续失败：重试 3 次后置 failed（共执行 4 次）。
func TestRetryLimit(t *testing.T) {
	useFastBackoff(t)
	exec := newFakeExecutor()
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		return InpaintResult{}, errors.New("引擎崩溃")
	}
	q := newTestQueue(exec)
	defer q.Stop()

	tk, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/in", OutDir: "/in_out"})
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateFailed }, "未按预期置 failed")
	for _, s := range q.Snapshot() {
		if s.ID == tk.ID && s.Attempts != MaxAutoRetries {
			t.Errorf("attempts = %d，期望 %d", s.Attempts, MaxAutoRetries)
		}
	}
}

// TestDownloadBackfillsAudio 下载任务完成后 BGM 取源随任务落库，
// 且 AudioChecked 置位（区分「确认无 BGM」与「未曾探测」）。
func TestDownloadBackfillsAudio(t *testing.T) {
	exec := newFakeExecutor()
	exec.dlFn = func(ctx context.Context, t *BatchTask) (DownloadResult, error) {
		return DownloadResult{
			Dir:      "/dl/x",
			PostRef:  "图文帖 | 100 张",
			Files:    []string{"/dl/x/01.jpg"},
			AudioURL: "https://cdn/a.mp3",
			Audio:    []string{"https://cdn/a.mp3", "https://cdn/b.mp3"},
			AudioNm:  "某曲目",
		}, nil
	}
	q := newTestQueue(exec)
	defer q.Stop()

	tk, _ := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/1"})
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateDone }, "下载任务应完成")

	var got BatchTask
	for _, s := range q.Snapshot() {
		if s.ID == tk.ID {
			got = s
		}
	}
	if got.AudioURL != "https://cdn/a.mp3" {
		t.Errorf("AudioURL = %q，期望候选链首项", got.AudioURL)
	}
	if len(got.AudioCandidates) != 2 {
		t.Errorf("AudioCandidates = %v，期望 2 条", got.AudioCandidates)
	}
	if got.AudioName != "某曲目" {
		t.Errorf("AudioName = %q", got.AudioName)
	}
	if !got.AudioChecked {
		t.Error("AudioChecked 应为 true：本轮已探测过 BGM（即便为空也要置位）")
	}
}

// TestDownloadAudioNotBackfilledOnFailure 失败任务不覆写既有 BGM 取源
// （重试前的有效结果不能被零值冲掉）。
func TestDownloadAudioNotBackfilledOnFailure(t *testing.T) {
	exec := newFakeExecutor()
	exec.dlFn = func(ctx context.Context, t *BatchTask) (DownloadResult, error) {
		return DownloadResult{}, Permanent(errors.New("该链接是视频内容，暂不支持"))
	}
	q := newTestQueue(exec)
	defer q.Stop()

	tk, _ := q.Enqueue(&BatchTask{
		Type:            TypeDownload,
		URL:             "https://x/v",
		AudioURL:        "https://cdn/keep.mp3",
		AudioCandidates: []string{"https://cdn/keep.mp3"},
		AudioChecked:    true,
	})
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateFailed }, "应 failed")

	for _, s := range q.Snapshot() {
		if s.ID != tk.ID {
			continue
		}
		if s.AudioURL != "https://cdn/keep.mp3" || len(s.AudioCandidates) != 1 {
			t.Errorf("失败任务不应覆写 BGM 取源，得到 url=%q cands=%v", s.AudioURL, s.AudioCandidates)
		}
	}
}

// TestResolveTaskAudio 历史任务（无 BGM 数据）回源补取一次并写回任务；
// 二次调用命中 AudioChecked 缓存，不再回源。
func TestResolveTaskAudio(t *testing.T) {
	exec := newFakeExecutor()
	resolveCalls := 0
	exec.auFn = func(ctx context.Context, t *BatchTask) ([]string, string, error) {
		resolveCalls++
		return []string{"https://cdn/late.mp3"}, "补取曲目", nil
	}
	q := newTestQueue(exec)
	defer q.Stop()

	// 模拟历史任务：入队时 AudioChecked 为未置位（该字段此前不存在）。
	// 但本轮下载会正常解析并置位，故先断言置位，再验证不重复回源。
	tk, _ := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/legacy"})
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateDone }, "应完成")

	cands, name, err := q.ResolveTaskAudio(context.Background(), tk.ID)
	if err != nil {
		t.Fatalf("ResolveTaskAudio 出错: %v", err)
	}
	if len(cands) != 1 || cands[0] != "https://cdn/late.mp3" || name != "补取曲目" {
		t.Errorf("补取结果 = %v / %q", cands, name)
	}
	for _, s := range q.Snapshot() {
		if s.ID == tk.ID && (s.AudioURL != "https://cdn/late.mp3" || !s.AudioChecked) {
			t.Errorf("补取结果未写回任务: url=%q checked=%v", s.AudioURL, s.AudioChecked)
		}
	}

	// 二次调用：AudioChecked 已置位，直接返回缓存，不再回源
	if _, _, err := q.ResolveTaskAudio(context.Background(), tk.ID); err != nil {
		t.Fatalf("二次调用出错: %v", err)
	}
	if resolveCalls != 1 {
		t.Errorf("回源调用 %d 次，期望 1 次（已探测过应走缓存）", resolveCalls)
	}
}

// TestResolveTaskAudioUncheckedLegacy 未探测过的历史任务（AudioChecked 未置位）
// 必须走回源补取，而不是把「空候选链」当成「确认无 BGM」。
func TestResolveTaskAudioUncheckedLegacy(t *testing.T) {
	exec := newFakeExecutor()
	resolveCalls := 0
	exec.auFn = func(ctx context.Context, t *BatchTask) ([]string, string, error) {
		resolveCalls++
		return []string{"https://cdn/legacy.mp3"}, "历史曲目", nil
	}
	q := newTestQueue(exec)
	defer q.Stop()

	// 直接构造一个未探测过的任务条目（模拟旧版本持久化数据）
	q.mu.Lock()
	q.tasks = append(q.tasks, &BatchTask{
		ID:        "legacy-1",
		Type:      TypeDownload,
		State:     StateDone,
		URL:       "https://x/legacy",
		ResultDir: "/dl/legacy",
		Files:     []string{"/dl/legacy/01.jpg"},
	})
	q.mu.Unlock()

	cands, _, err := q.ResolveTaskAudio(context.Background(), "legacy-1")
	if err != nil {
		t.Fatalf("ResolveTaskAudio 出错: %v", err)
	}
	if len(cands) != 1 || resolveCalls != 1 {
		t.Errorf("未探测过的历史任务应回源一次，cands=%v calls=%d", cands, resolveCalls)
	}
}

// TestDownloadAudioUnresolvedNotMarkedChecked 候选被健康检查全部过滤时
// 不置 AudioChecked（不代表「该帖没有 BGM」，应允许稍后重试补取）。
func TestDownloadAudioUnresolvedNotMarkedChecked(t *testing.T) {
	exec := newFakeExecutor()
	exec.dlFn = func(ctx context.Context, t *BatchTask) (DownloadResult, error) {
		return DownloadResult{
			Dir:     "/dl/x",
			PostRef: "图文帖 | 100 张",
			Files:   []string{"/dl/x/01.jpg"},
			Audio:   nil, // 候选全部被过滤
			// AudioResolved 保持 false：解析层拿到了候选但均不可用
		}, nil
	}
	q := newTestQueue(exec)
	defer q.Stop()

	tk, _ := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/1"})
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateDone }, "下载任务应完成")

	for _, s := range q.Snapshot() {
		if s.ID == tk.ID && s.AudioChecked {
			t.Error("候选全部不可用时不置 AudioChecked：这不是「该帖没有 BGM」")
		}
	}
}

// TestResolveTaskAudioRejectsNonDownload 非下载任务与不存在的任务应报错。
func TestResolveTaskAudioRejectsNonDownload(t *testing.T) {
	exec := newFakeExecutor()
	q := newTestQueue(exec)
	defer q.Stop()

	if _, _, err := q.ResolveTaskAudio(context.Background(), "不存在"); err == nil {
		t.Error("不存在的任务应报错")
	}

	tk, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/in", OutDir: "/in_out"})
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateDone }, "去水印任务应完成")
	if _, _, err := q.ResolveTaskAudio(context.Background(), tk.ID); err == nil {
		t.Error("去水印任务没有 BGM，应报错")
	}
}

// TestPermanentError 确定性错误不重试直接 failed。
func TestPermanentError(t *testing.T) {
	exec := newFakeExecutor()
	exec.dlFn = func(ctx context.Context, t *BatchTask) (DownloadResult, error) {
		return DownloadResult{}, Permanent(errors.New("该链接是视频内容，暂不支持"))
	}
	q := newTestQueue(exec)
	defer q.Stop()

	tk, _ := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/v"})
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateFailed }, "确定性错误应直接 failed")
	for _, s := range q.Snapshot() {
		if s.ID == tk.ID {
			if s.Attempts != 0 {
				t.Errorf("确定性错误不应计入重试，attempts=%d", s.Attempts)
			}
			if s.Err != "该链接是视频内容，暂不支持" {
				t.Errorf("Err = %q", s.Err)
			}
		}
	}
}

// TestCancelRunning 取消运行中任务 → canceled（执行器响应 ctx 取消）。
func TestCancelRunning(t *testing.T) {
	exec := newFakeExecutor()
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		select {
		case <-time.After(10 * time.Second): // 模拟长推理
		case <-ctx.Done():
		}
		return InpaintResult{}, ctx.Err()
	}
	q := newTestQueue(exec)
	defer q.Stop()

	tk, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/in", OutDir: "/in_out"})
	<-exec.started // 等任务开始执行
	if err := q.Cancel(tk.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateCanceled }, "取消后未到 canceled")
}

// TestCancelPending 取消排队中任务直接 canceled，不占用执行。
func TestCancelPending(t *testing.T) {
	exec := newFakeExecutor()
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
		}
		return InpaintResult{}, ctx.Err()
	}
	q := newTestQueue(exec)
	defer q.Stop()

	first, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/a", OutDir: "/a_out"})
	second, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/b", OutDir: "/b_out"})
	<-exec.started // first 在执行
	if err := q.Cancel(second.ID); err != nil {
		t.Fatal(err)
	}
	if got := taskState(q, second.ID); got != StateCanceled {
		t.Errorf("pending 取消后应直接 canceled，实际 %s", got)
	}
	// first 正常收尾后 second 不应被执行
	_ = first
	time.Sleep(50 * time.Millisecond)
	for _, c := range exec.ipCalls {
		if c == "/b" {
			t.Error("已取消的任务不应被执行")
		}
	}
}

// TestManualRetry 手动重试：自动重试耗尽 failed → Retry → done，attempts 清零。
func TestManualRetry(t *testing.T) {
	useFastBackoff(t)
	exec := newFakeExecutor()
	calls := 0
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		calls++
		if calls <= 4 { // 首次 + 3 次自动重试全部失败
			return InpaintResult{}, errors.New("boom")
		}
		return InpaintResult{OutDir: t.OutDir, OK: 1, Total: 1}, nil
	}
	q := newTestQueue(exec)
	defer q.Stop()

	tk, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/in", OutDir: "/in_out"})
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateFailed }, "应先到 failed")
	if err := q.Retry(tk.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, s := range q.Snapshot() {
			if s.ID == tk.ID {
				return s.State == StateDone && s.Attempts == 0
			}
		}
		return false
	}, "手动重试后未到 done")
}

// TestOutDirDedup 同一输出目录不允许两个未完成任务（避免互相清空产物）。
func TestOutDirDedup(t *testing.T) {
	exec := newFakeExecutor()
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
		}
		return InpaintResult{}, ctx.Err()
	}
	q := newTestQueue(exec)
	defer q.Stop()

	q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/a", OutDir: "/same_out"})
	if _, err := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/b", OutDir: "/same_out"}); err == nil {
		t.Error("重复 outDir 应被拒绝")
	}
}

// TestPersistenceRoundtrip 持久化 + 启动恢复：running/retry_wait → pending。
func TestPersistenceRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "queue.json")

	// 手工写入一个含 running / retry_wait / done 的队列文件
	now := time.Now()
	manual := []*BatchTask{
		{ID: "t1", Type: TypeInpaint, State: StateRunning, InDir: "/a", OutDir: "/a_out", CreatedAt: now, Progress: &TaskProgress{Index: 1, Total: 2}},
		{ID: "t2", Type: TypeDownload, State: StateRetryWait, URL: "https://x/b", CreatedAt: now, Attempts: 1},
		{ID: "t3", Type: TypeDownload, State: StateDone, URL: "https://x/c", CreatedAt: now},
	}
	if err := SaveQueue(path, manual); err != nil {
		t.Fatal(err)
	}

	exec := newFakeExecutor()
	q := New(exec, nil, path)
	q.Start(context.Background())
	defer q.Stop()

	waitFor(t, func() bool { dl, ip := exec.calls(); return dl+ip >= 2 }, "恢复的 pending 任务应被执行")

	// t1/t2 已被执行（done 或执行中），t3 终态记录保留
	for _, s := range q.Snapshot() {
		if s.ID == "t3" && s.State != StateDone {
			t.Errorf("终态记录应保留，t3=%s", s.State)
		}
	}
	// 恢复后回写文件应已剥离瞬态字段
	reloaded, err := LoadQueue(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range reloaded {
		if tk.Progress != nil || tk.NextRetryAt != nil {
			t.Errorf("持久化数据不应含瞬态字段: %s", tk.ID)
		}
	}
}

// TestDownloadURLDedup 同一链接不允许重复入队（未完成期间）。
func TestDownloadURLDedup(t *testing.T) {
	exec := newFakeExecutor()
	q := newTestQueue(exec)
	defer q.Stop()

	if _, err := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x.com/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://X.com/a/"}); err == nil {
		t.Error("同一链接（大小写/尾斜杠差异）应被拒绝重复入队")
	}
	// 不同链接不受影响
	if _, err := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x.com/b"}); err != nil {
		t.Errorf("不同链接应允许入队: %v", err)
	}
}

// TestClearFinishedByType 按类型清空终态任务（两类队列独立管理）。
func TestClearFinishedByType(t *testing.T) {
	exec := newFakeExecutor()
	q := newTestQueue(exec)
	defer q.Stop()

	dl, _ := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/a"})
	ip, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/a", OutDir: "/a_out"})
	waitFor(t, func() bool { return taskState(q, dl.ID) == StateDone && taskState(q, ip.ID) == StateDone }, "任务未完成")

	q.ClearFinished(TypeDownload)
	for _, s := range q.Snapshot() {
		if s.ID == dl.ID {
			t.Error("download 终态任务应被清空")
		}
	}
	if taskState(q, ip.ID) != StateDone {
		t.Error("inpaint 任务不应被 download 类型的清空操作影响")
	}
	q.ClearFinished("")
	if n := len(q.Snapshot()); n != 0 {
		t.Errorf("清空全部后应剩 0 条，实际 %d", n)
	}
}

// TestRemoveAndClearFinished 移除终态 / 清空已完成。
func TestRemoveAndClearFinished(t *testing.T) {
	exec := newFakeExecutor()
	q := newTestQueue(exec)
	defer q.Stop()

	tk, _ := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/a"})
	waitFor(t, func() bool { return taskState(q, tk.ID) == StateDone }, "任务未完成")

	if err := q.Remove(tk.ID); err != nil {
		t.Fatalf("移除终态任务失败: %v", err)
	}
	if err := q.Remove("no_such"); err == nil {
		t.Error("移除不存在的任务应报错")
	}
	q.ClearFinished("")
	if n := len(q.Snapshot()); n != 0 {
		t.Errorf("清空后应剩 0 条，实际 %d", n)
	}
}

// TestSnapshotSortedNewestFirst 快照按创建时刻倒序（前端分组展示依赖此顺序）。
func TestSnapshotSortedNewestFirst(t *testing.T) {
	exec := newFakeExecutor()
	// 串行执行：逐个入队并等完成，确保三条任务的 createdAt 严格递增
	exec.mu.Lock()
	exec.dlFn = func(ctx context.Context, task *BatchTask) (DownloadResult, error) {
		return DownloadResult{Dir: "/dl", PostRef: "p"}, nil
	}
	exec.mu.Unlock()
	q := newTestQueue(exec)
	defer q.Stop()

	var ids []string
	for i := 0; i < 3; i++ {
		tk, err := q.Enqueue(&BatchTask{Type: TypeDownload, URL: fmt.Sprintf("https://x/%d", i)})
		if err != nil {
			t.Fatalf("入队失败: %v", err)
		}
		ids = append(ids, tk.ID)
		waitFor(t, func() bool { return taskState(q, tk.ID) == StateDone }, "任务未完成")
		time.Sleep(2 * time.Millisecond) // 拉开 createdAt，避免同刻导致顺序不可判
	}

	snap := q.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("快照应有 3 条，实际 %d", len(snap))
	}
	for i := 0; i < len(snap)-1; i++ {
		if snap[i].CreatedAt.Before(snap[i+1].CreatedAt) {
			t.Errorf("快照应按创建时刻倒序：第 %d 条(%v) 早于第 %d 条(%v)",
				i, snap[i].CreatedAt, i+1, snap[i+1].CreatedAt)
		}
	}
	// 最新的任务应排在最前
	if snap[0].ID != ids[2] {
		t.Errorf("首条应为最后入队的任务，实际 %s（期望 %s）", snap[0].ID, ids[2])
	}
}

// TestTaskTimeUsesFinishedForTerminal 分组判据时刻：终态取完成时刻，其余取创建时刻。
func TestTaskTimeUsesFinishedForTerminal(t *testing.T) {
	created := time.Now().Add(-48 * time.Hour)
	finished := time.Now().Add(-1 * time.Hour)

	done := &BatchTask{State: StateDone, CreatedAt: created, FinishedAt: &finished}
	if got := TaskTime(done); !got.Equal(finished) {
		t.Errorf("终态应用完成时刻，实际 %v", got)
	}
	// 终态但无完成时刻（历史数据）：退回创建时刻，不能返回零值把任务归到「很久以前」
	doneNoFin := &BatchTask{State: StateDone, CreatedAt: created}
	if got := TaskTime(doneNoFin); !got.Equal(created) {
		t.Errorf("终态缺完成时刻应退回创建时刻，实际 %v", got)
	}
	for _, st := range []string{StatePending, StateRunning, StateRetryWait} {
		tk := &BatchTask{State: st, CreatedAt: created, FinishedAt: &finished}
		if got := TaskTime(tk); !got.Equal(created) {
			t.Errorf("状态 %s 应用创建时刻，实际 %v", st, got)
		}
	}
}

// TestClearFinishedFilteredByRange 按时间区间清空：只删区间内的终态任务，
// 永不删除非终态任务。
func TestClearFinishedFilteredByRange(t *testing.T) {
	exec := newFakeExecutor()
	q := newTestQueue(exec)
	defer q.Stop()

	dl, _ := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/a"})
	ip, _ := q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/a", OutDir: "/a_out"})
	waitFor(t, func() bool { return taskState(q, dl.ID) == StateDone && taskState(q, ip.ID) == StateDone }, "任务未完成")

	// 区间覆盖「过去 1 小时」→ 两条终态任务都在区间内；限定 download 类型
	now := time.Now()
	q.ClearFinishedFiltered(ClearFilter{Type: TypeDownload, Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
	for _, s := range q.Snapshot() {
		if s.ID == dl.ID {
			t.Error("区间内的 download 终态任务应被清空")
		}
	}
	if taskState(q, ip.ID) != StateDone {
		t.Error("类型不匹配的 inpaint 任务不应被清空")
	}

	// 区间落在「未来」→ 无任务命中，全部保留
	q.ClearFinishedFiltered(ClearFilter{Since: now.Add(time.Hour)})
	if n := len(q.Snapshot()); n != 1 {
		t.Errorf("未来区间不应清掉任何任务，剩余应为 1，实际 %d", n)
	}

	// 不限区间 + 不限类型 → 清空全部终态
	q.ClearFinishedFiltered(ClearFilter{})
	if n := len(q.Snapshot()); n != 0 {
		t.Errorf("清空全部后应剩 0 条，实际 %d", n)
	}
}

// TestClearGroupNeverRemovesActive 硬约束：任何筛选条件都不能删除非终态任务。
func TestClearGroupNeverRemovesActive(t *testing.T) {
	exec := newFakeExecutor()
	block := make(chan struct{})
	exec.mu.Lock()
	exec.dlFn = func(ctx context.Context, task *BatchTask) (DownloadResult, error) {
		select {
		case <-block:
		case <-ctx.Done():
			return DownloadResult{}, ctx.Err()
		}
		return DownloadResult{Dir: "/dl"}, nil
	}
	exec.mu.Unlock()
	q := newTestQueue(exec)
	defer q.Stop()
	defer close(block)

	running, _ := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/run"})
	waitFor(t, func() bool { return taskState(q, running.ID) == StateRunning }, "任务未进入执行中")

	// 即使筛选条件覆盖全部时间、不限类型，执行中的任务也必须留下
	q.ClearFinishedFiltered(ClearFilter{})
	if taskState(q, running.ID) != StateRunning {
		t.Error("执行中的任务绝不应被清空操作删除")
	}
	if n := len(q.Snapshot()); n != 1 {
		t.Errorf("执行中任务应保留，剩余应为 1，实际 %d", n)
	}
}

// TestNormalizeURL 链接规范化比较。
func TestNormalizeURL(t *testing.T) {
	cases := [][2]string{
		{"https://X.com/a/", "https://x.com/a"},
		{" http://x.com/b?id=1 ", "http://x.com/b?id=1"},
		{"https://x.com/A/1", "https://x.com/A/1"}, // 路径大小写敏感，不应归一
	}
	for _, c := range cases {
		if normalizeURL(c[0]) != normalizeURL(c[1]) {
			t.Errorf("normalizeURL(%q) != normalizeURL(%q)", c[0], c[1])
		}
	}
	if normalizeURL("https://x.com/A/1") == normalizeURL("https://x.com/a/1") {
		t.Error("路径大小写不应被归一化")
	}
}

// TestPermanentClassify IsPermanent / errText / Permanent(nil) 行为。
func TestPermanentClassify(t *testing.T) {
	if !IsPermanent(Permanent(errors.New("x"))) {
		t.Error("Permanent 错误应被识别")
	}
	if IsPermanent(errors.New("x")) {
		t.Error("普通错误不应被识别为确定性错误")
	}
	if got := errText(Permanent(errors.New("提示A"))); got != "提示A" {
		t.Errorf("errText = %q", got)
	}
	if Permanent(nil) != nil {
		t.Error("Permanent(nil) 应为 nil")
	}
}

// TestLoadQueueMissingFile 不存在的文件返回空列表而非错误。
func TestLoadQueueMissingFile(t *testing.T) {
	tasks, err := LoadQueue(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("应返回空列表，实际 %d", len(tasks))
	}
}

// TestWouldClearMatchesActual 预演（WouldClear）与实际清空必须严格一致。
//
// 上层要在删记录前先用 WouldClear 收集文件路径，若两者判据漂移，
// 就会出现「预演说会删、实际没删」或反之——后者会删掉仍被引用的文件。
func TestWouldClearMatchesActual(t *testing.T) {
	exec := newFakeExecutor()
	block := make(chan struct{})
	// inpaint 阻塞在途 → 制造一条「非终态」任务，用于验证它永不被牵连
	exec.ipFn = func(ctx context.Context, t *BatchTask) (InpaintResult, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return InpaintResult{}, ctx.Err()
	}
	q := newTestQueue(exec)
	defer func() {
		close(block)
		q.Stop()
	}()

	q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/slow", OutDir: "/slow_out"})
	<-exec.started // 该 inpaint 已进入 running，处于非终态

	dl, _ := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/a"})
	waitFor(t, func() bool { return taskState(q, dl.ID) == StateDone }, "download 任务未完成")

	// 只清 download 类型：预演应只含那一条 download
	f := ClearFilter{Type: TypeDownload}
	predicted := q.WouldClear(f)
	if len(predicted) != 1 || predicted[0].ID != dl.ID {
		t.Fatalf("预演应只含 download 终态任务，实际 %v", ids(predicted))
	}

	before := len(q.Snapshot())
	q.ClearFinishedFiltered(f)
	after := len(q.Snapshot())

	if after != before-len(predicted) {
		t.Errorf("实际删除数应与预演一致：before=%d after=%d predicted=%d",
			before, after, len(predicted))
	}
	// 非终态任务在任何筛选下都必须存活
	for _, s := range q.Snapshot() {
		if s.Type == TypeInpaint && s.ID != dl.ID && terminalState(s.State) {
			t.Errorf("在途 inpaint 任务不应被清空，实际 state=%s", s.State)
		}
	}
	if n := len(q.Snapshot()); n != 1 {
		t.Errorf("应仅剩在途的 inpaint 任务，实际 %d 条", n)
	}

	// 不限条件的预演必须跳过非终态
	if got := len(q.WouldClear(ClearFilter{})); got != 0 {
		t.Errorf("非终态任务不应被预演为可清空，实际 %d", got)
	}
}

// TestWouldClearDoesNotMutate WouldClear 是纯查询，调用后任务数不变。
func TestWouldClearDoesNotMutate(t *testing.T) {
	exec := newFakeExecutor()
	q := newTestQueue(exec)
	defer q.Stop()

	dl, _ := q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/a"})
	waitFor(t, func() bool { return taskState(q, dl.ID) == StateDone }, "任务未完成")

	before := len(q.Snapshot())
	twice := len(q.WouldClear(ClearFilter{}))
	if got := len(q.Snapshot()); got != before {
		t.Errorf("WouldClear 不应改变任务数：before=%d after=%d", before, got)
	}
	if twice != 1 {
		t.Errorf("应预演出 1 条，实际 %d", twice)
	}
}

func ids(ts []BatchTask) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

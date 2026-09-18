package queue

import (
	"context"
	"errors"
	"path/filepath"
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
	dlFn    func(ctx context.Context, t *BatchTask) (string, string, []string, error)
	ipFn    func(ctx context.Context, t *BatchTask) (InpaintResult, error)
	started chan *BatchTask // 每次开始执行推入
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{started: make(chan *BatchTask, 64)}
}

func (f *fakeExecutor) RunDownload(ctx context.Context, t *BatchTask) (string, string, []string, error) {
	f.mu.Lock()
	f.dlCalls = append(f.dlCalls, t.URL)
	fn := f.dlFn
	f.mu.Unlock()
	f.started <- t
	if fn != nil {
		return fn(ctx, t)
	}
	return "/dl/" + t.URL, "帖子A | 3 张", []string{"/dl/01.jpg", "/dl/02.jpg"}, nil
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

// TestDownloadPriority download 优先于 inpaint（PRD §3.2）。
func TestDownloadPriority(t *testing.T) {
	exec := newFakeExecutor()
	q := newTestQueue(exec)
	defer q.Stop()

	q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/in1", OutDir: "/in1_out"})
	q.Enqueue(&BatchTask{Type: TypeInpaint, InDir: "/in2", OutDir: "/in2_out"})
	q.Enqueue(&BatchTask{Type: TypeDownload, URL: "https://x/a"})

	waitFor(t, func() bool { dl, ip := exec.calls(); return dl == 1 && ip == 2 }, "任务未全部执行")
	if exec.dlCalls[0] != "https://x/a" {
		t.Errorf("download 应最先执行，实际顺序 dl=%v ip=%v", exec.dlCalls, exec.ipCalls)
	}
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

// TestPermanentError 确定性错误不重试直接 failed。
func TestPermanentError(t *testing.T) {
	exec := newFakeExecutor()
	exec.dlFn = func(ctx context.Context, t *BatchTask) (string, string, []string, error) {
		return "", "", nil, Permanent(errors.New("该链接是视频内容，暂不支持"))
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

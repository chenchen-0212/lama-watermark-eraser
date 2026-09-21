package downloader

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain 关闭重试退避：这些用例关心的是「试了几次」，不是「等了多久」。
// 需要验证退避本身的用例会临时把基准调回来。
func TestMain(m *testing.M) {
	audioBackoffBase = 0
	os.Exit(m.Run())
}

// audioPayload 带 ID3 头的伪音频。只需通过魔数校验，无需真实可解码。
var audioPayload = []byte("ID3\x03\x00" + strings.Repeat("payload-bytes-", 6))

// resetAudioState 清理包级状态（候选源偏好 + 候选链注册表）。
//
// 偏好键是「候选链的 path 签名」——不同用例若复用同一组 path（测试里很常见），
// 就会命中上一个用例留下的偏好，把候选顺序搅乱。真实场景中不同曲目 path 不同，
// 只有同一首 BGM 才会共享偏好，那时共享正是期望行为。
func resetAudioState(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		audioPrefs.mu.Lock()
		audioPrefs.items = map[string]*AudioSourcePreference{}
		audioPrefs.path = ""
		audioPrefs.mu.Unlock()

		audioChainRegistry.mu.Lock()
		audioChainRegistry.items = map[string]chainRecord{}
		audioChainRegistry.order = nil
		audioChainRegistry.mu.Unlock()
	})
}

// audioServer 可编程的音频服务：behavior 收到「该地址的第几次请求」，
// 返回状态码、Content-Type 与响应体。hits 记录累计请求次数。
func audioServer(t *testing.T, behavior func(n int) (int, string, []byte)) (*httptest.Server, *int32) {
	t.Helper()
	resetAudioState(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(&hits, 1))
		status, ct, body := behavior(n)
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// okAudio 标准成功响应。
func okAudio(int) (int, string, []byte) {
	return http.StatusOK, "audio/mpeg", audioPayload
}

// htmlRisk 风控页：HTTP 200 且 Content-Type 看似正常，但内容不是音频。
func htmlRisk(int) (int, string, []byte) {
	return http.StatusOK, "text/html; charset=utf-8", []byte("<html><body>访问过于频繁</body></html>")
}

// ---------------------------------------------------------- ProbeAudio

// ProbeAudio 对可用地址返回 nil，且只读头部（不把整个音频拉下来）。
func TestProbeAudioOK(t *testing.T) {
	var gotRange string
	var sent int64
	payload := append(append([]byte{}, audioPayload...), bytes.Repeat([]byte("x"), 4096)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusPartialContent)
		n, _ := w.Write(payload)
		atomic.StoreInt64(&sent, int64(n))
	}))
	defer srv.Close()

	if err := ProbeAudio(context.Background(), srv.URL+"/obj/ies-music/a.mp3"); err != nil {
		t.Fatalf("可用地址应探测通过: %v", err)
	}
	if gotRange != "bytes=0-63" {
		t.Errorf("Range = %q，期望只取头部 64 字节", gotRange)
	}
}

// 风控页（200 + 非音频内容）必须判为不可用：只看状态码会误判。
func TestProbeAudioRejectsRiskPage(t *testing.T) {
	srv, _ := audioServer(t, htmlRisk)
	if err := ProbeAudio(context.Background(), srv.URL+"/a.mp3"); err == nil {
		t.Error("200 + HTML 风控页应判为不可用")
	}
}

// 4xx/5xx 判为不可用。
func TestProbeAudioRejectsHTTPError(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		srv, _ := audioServer(t, func(int) (int, string, []byte) {
			return code, "text/plain", []byte("nope")
		})
		if err := ProbeAudio(context.Background(), srv.URL+"/a.mp3"); err == nil {
			t.Errorf("HTTP %d 应判为不可用", code)
		}
		srv.Close()
	}
}

// 空响应体判为不可用。
func TestProbeAudioRejectsEmptyBody(t *testing.T) {
	srv, _ := audioServer(t, func(int) (int, string, []byte) {
		return http.StatusOK, "audio/mpeg", nil
	})
	if err := ProbeAudio(context.Background(), srv.URL+"/a.mp3"); err == nil {
		t.Error("空响应体应判为不可用")
	}
}

// 空地址直接失败，不发请求。
func TestProbeAudioRejectsBlankURL(t *testing.T) {
	if err := ProbeAudio(context.Background(), "   "); err == nil {
		t.Error("空地址应报错")
	}
}

// 场景 1：单候选成功 —— 应只发一次请求，内容完整落盘。
func TestAudioDownloadSingleCandidateSuccess(t *testing.T) {
	srv, hits := audioServer(t, okAudio)
	dir := t.TempDir()
	cands := []AudioCandidate{{URL: srv.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList}}

	saved, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm")
	if err != nil {
		t.Fatalf("下载应成功: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("成功路径应只发 1 次请求，实际 %d 次", got)
	}
	data, err := os.ReadFile(saved)
	if err != nil {
		t.Fatalf("读取产物: %v", err)
	}
	if !bytes.Equal(data, audioPayload) {
		t.Errorf("落盘内容不完整: %d 字节，期望 %d", len(data), len(audioPayload))
	}
}

// 场景 2：候选 A 首次连接中断 → 重试同一地址 → 成功。
// 验证候选级重试对网络类错误生效。
func TestAudioDownloadRetryOnNetworkError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			// 直接断开连接，制造 connection reset / unexpected EOF 类故障
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
			panic("测试服务不支持 Hijack")
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(audioPayload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cands := []AudioCandidate{{URL: srv.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList}}
	if _, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm"); err != nil {
		t.Fatalf("网络类故障应重试同一地址并成功: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("应重试一次（共 2 次请求），实际 %d 次", got)
	}
}

// 场景 2b：候选 A 首请求超时 → 重试 → 成功。
func TestAudioDownloadRetryOnTimeout(t *testing.T) {
	old := client.Timeout
	client.Timeout = 150 * time.Millisecond
	t.Cleanup(func() { client.Timeout = old })

	srv, hits := audioServer(t, func(n int) (int, string, []byte) {
		if n == 1 {
			time.Sleep(400 * time.Millisecond) // 超过 client 超时
		}
		return okAudio(n)
	})
	dir := t.TempDir()
	cands := []AudioCandidate{{URL: srv.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList}}

	if _, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm"); err != nil {
		t.Fatalf("超时后应重试并成功: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Errorf("应重试一次（共 2 次请求），实际 %d 次", got)
	}
}

// 场景 3：候选 A 403 → 允许一次额外尝试 → 仍失败 → 切到 B 成功。
// 403 属瞬时风控，重试预算为 2（首次 + 一次）。
func TestAudioDownload403FallsToNextCandidate(t *testing.T) {
	a, aHits := audioServer(t, func(int) (int, string, []byte) {
		return http.StatusForbidden, "text/html", []byte("forbidden")
	})
	b, bHits := audioServer(t, okAudio)

	dir := t.TempDir()
	cands := []AudioCandidate{
		{URL: a.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList},
		{URL: b.URL + "/obj/ies-music/b.mp3", Source: SourceDouyinMusicDetail},
	}
	if _, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm"); err != nil {
		t.Fatalf("应落到第二个候选: %v", err)
	}
	if got := atomic.LoadInt32(aHits); got != 2 {
		t.Errorf("403 应尝试 2 次（首次 + 一次重试），实际 %d 次", got)
	}
	if got := atomic.LoadInt32(bHits); got != 1 {
		t.Errorf("第二候选应只请求 1 次，实际 %d 次", got)
	}
}

// 场景 4：候选 A 404 → 不可恢复 → 不得重复请求 A → 切 B。
func TestAudioDownload404DoesNotRetrySameURL(t *testing.T) {
	a, aHits := audioServer(t, func(int) (int, string, []byte) {
		return http.StatusNotFound, "text/plain", []byte("not found")
	})
	b, _ := audioServer(t, okAudio)

	dir := t.TempDir()
	cands := []AudioCandidate{
		{URL: a.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList},
		{URL: b.URL + "/obj/ies-music/b.mp3", Source: SourceDouyinMusicDetail},
	}
	if _, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm"); err != nil {
		t.Fatalf("应落到第二个候选: %v", err)
	}
	if got := atomic.LoadInt32(aHits); got != 1 {
		t.Errorf("404 不可恢复，应只请求 1 次，实际 %d 次", got)
	}
}

// 场景 5：候选 A 返回 HTML 风控页（HTTP 200）→ 不得重试 → 切 B 成功。
func TestAudioDownloadHTMLFallsToNextCandidate(t *testing.T) {
	a, aHits := audioServer(t, htmlRisk)
	b, _ := audioServer(t, okAudio)

	dir := t.TempDir()
	cands := []AudioCandidate{
		{URL: a.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList},
		{URL: b.URL + "/obj/ies-music/b.mp3", Source: SourceDouyinMusicDetail},
	}
	if _, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm"); err != nil {
		t.Fatalf("应落到第二个候选: %v", err)
	}
	if got := atomic.LoadInt32(aHits); got != 1 {
		t.Errorf("内容非音频不可恢复，应只请求 1 次，实际 %d 次", got)
	}
}

// 场景 6：候选 A 首次 429 → 退避后重试 → 成功。同时验证退避确实发生。
func TestAudioDownload429BackoffThenRetry(t *testing.T) {
	audioBackoffBase = 30 * time.Millisecond
	t.Cleanup(func() { audioBackoffBase = 0 })

	srv, hits := audioServer(t, func(n int) (int, string, []byte) {
		if n == 1 {
			return http.StatusTooManyRequests, "text/plain", []byte("slow down")
		}
		return okAudio(n)
	})

	dir := t.TempDir()
	cands := []AudioCandidate{{URL: srv.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList}}
	started := time.Now()
	if _, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm"); err != nil {
		t.Fatalf("429 退避后重试应成功: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Errorf("限流应重试一次（共 2 次请求），实际 %d 次", got)
	}
	// 429 的退避基准是普通错误的 4 倍
	if elapsed := time.Since(started); elapsed < 30*time.Millisecond {
		t.Errorf("429 应先退避再重试，实际仅耗时 %v", elapsed)
	}
}

// 场景 7：所有候选失败 —— 错误须能指明每个候选的失败类型。
func TestAudioDownloadAllCandidatesFail(t *testing.T) {
	a, _ := audioServer(t, func(int) (int, string, []byte) {
		return http.StatusNotFound, "text/plain", []byte("gone")
	})
	b, _ := audioServer(t, htmlRisk)

	dir := t.TempDir()
	cands := []AudioCandidate{
		{URL: a.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList},
		{URL: b.URL + "/obj/ies-music/b.mp3", Source: SourceDouyinMusicDetail},
	}
	_, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm")
	if err == nil {
		t.Fatal("全部候选失败时必须报错")
	}
	// 已尝试 2 个地址，错误信息要能让用户/日志看清规模
	if !strings.Contains(err.Error(), "已尝试 2 个地址") {
		t.Errorf("错误信息应说明尝试规模，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "不是音频") {
		t.Errorf("末尾错误应保留「内容不是音频」的真实原因，实际: %v", err)
	}
}

// 场景 8：候选 URL 去重 —— 同一资源的多个 CDN 节点/签名只应请求一次。
func TestAudioDownloadDedupCandidates(t *testing.T) {
	srv, hits := audioServer(t, okAudio)
	dir := t.TempDir()

	// 同一 path、不同签名与主机参数：实为同一文件
	cands := []AudioCandidate{
		{URL: srv.URL + "/obj/ies-music/7286.mp3?x-expires=1&sign=aa", Source: SourceDouyinSSRURLList},
		{URL: srv.URL + "/obj/ies-music/7286.mp3?x-expires=2&sign=bb", Source: SourceDouyinSSRURI},
		{URL: srv.URL + "/obj/ies-music/7286.mp3?x-expires=3&sign=cc", Source: SourceDouyinMusicDetail},
	}
	if _, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm"); err != nil {
		t.Fatalf("下载: %v", err)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("等价候选应合并为 1 条，实际请求 %d 次", got)
	}

	// 不同 path 必须保留（不能粗暴地删掉全部查询参数）
	kept := DedupCandidates([]AudioCandidate{
		{URL: "https://x/a.mp3?q=high"},
		{URL: "https://x/a.mp3?q=low"},
		{URL: "https://x/b.mp3"},
	})
	if len(kept) != 3 {
		t.Errorf("非等价候选不应被合并，实际保留 %d 条: %v", len(kept), candidateURLs(kept))
	}
}

// 场景 9 + 10：下载过程中存在 .part，完成后被改名为最终文件。
func TestAudioPartFileLifecycle(t *testing.T) {
	resetAudioState(t)
	dir := t.TempDir()
	release := make(chan struct{})
	started := make(chan struct{})
	var sawPart int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		// 先写满校验所需的头部并 flush，客户端此时会创建 .part
		_, _ = w.Write(audioPayload)
		w.(http.Flusher).Flush()
		close(started)
		<-release // 挂住，给测试观察窗口
		_, _ = w.Write([]byte("tail-bytes"))
	}))
	defer srv.Close()

	go func() {
		<-started
		time.Sleep(100 * time.Millisecond)
		if m, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(m) > 0 {
			atomic.StoreInt32(&sawPart, 1)
		}
		close(release)
	}()

	cands := []AudioCandidate{{URL: srv.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList}}
	saved, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm")
	if err != nil {
		t.Fatalf("下载: %v", err)
	}
	if atomic.LoadInt32(&sawPart) != 1 {
		t.Error("下载过程中应存在 .part 临时文件")
	}
	if parts, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(parts) != 0 {
		t.Errorf("完成后 .part 应已被改名，实际残留: %v", parts)
	}
	if filepath.Ext(saved) != ".mp3" {
		t.Errorf("最终文件扩展名应为 .mp3，实际 %s", saved)
	}
}

// 场景 9b：下载失败时必须清理 .part，不留半成品。
func TestAudioPartCleanedUpOnFailure(t *testing.T) {
	resetAudioState(t)
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(audioPayload) // 魔数校验会通过，文件已创建
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close() // 中途断开：io.Copy 失败
			}
		}
	}))
	defer srv.Close()

	client.Timeout = 2 * time.Second
	defer func() { client.Timeout = 60 * time.Second }()

	cands := []AudioCandidate{{URL: srv.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList}}
	if _, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm"); err == nil {
		t.Fatal("连接中断应导致失败")
	}
	if parts, _ := filepath.Glob(filepath.Join(dir, "*.part")); len(parts) != 0 {
		t.Errorf("失败后 .part 应被清理，实际残留: %v", parts)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*")); len(files) != 0 {
		t.Errorf("失败不应留下任何产物，实际: %v", files)
	}
}

// 场景 11：HTTP 200 但内容不是音频 —— 必须拒绝，绝不落盘。
func TestAudioRejects200WithNonAudioBody(t *testing.T) {
	srv, _ := audioServer(t, htmlRisk)
	dir := t.TempDir()
	cands := []AudioCandidate{{URL: srv.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList}}

	_, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm")
	if err == nil {
		t.Fatal("非音频内容必须报错")
	}
	if k := kindOf(err); k != AudioErrNotAudio {
		t.Errorf("错误分类应为 %s，实际 %s（%v）", AudioErrNotAudio, k, err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "*")); len(files) != 0 {
		t.Errorf("非音频内容不得落盘，实际: %v", files)
	}
}

// 场景 11b：空响应体单独归类，便于与「非音频」区分。
func TestAudioEmptyBodyKind(t *testing.T) {
	srv, _ := audioServer(t, func(int) (int, string, []byte) {
		return http.StatusOK, "audio/mpeg", nil
	})
	dir := t.TempDir()
	cands := []AudioCandidate{{URL: srv.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList}}

	_, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm")
	if err == nil {
		t.Fatal("空响应体必须报错")
	}
	if k := kindOf(err); k != AudioErrEmptyBody {
		t.Errorf("错误分类应为 %s，实际 %s（%v）", AudioErrEmptyBody, k, err)
	}
}

// 无分类错误（本地 IO 失败）必须直接终止，不得继续尝试其余候选。
func TestAudioFatalErrorStopsChain(t *testing.T) {
	a, aHits := audioServer(t, okAudio)
	// 目标目录指向一个不存在的路径下的父级为「文件」的场景：
	// 用不存在的目录让 os.Create 必然失败
	dir := filepath.Join(t.TempDir(), "not-a-dir", "sub")

	cands := []AudioCandidate{
		{URL: a.URL + "/obj/ies-music/a.mp3", Source: SourceDouyinSSRURLList},
		{URL: a.URL + "/obj/ies-music/b.mp3", Source: SourceDouyinMusicDetail},
	}
	_, err := DownloadAudioWithFallback(context.Background(), cands, dir, "bgm")
	if err == nil {
		t.Fatal("目录不可写应报错")
	}
	if k := kindOf(err); k != "" {
		t.Errorf("本地 IO 错误不应被归类为链路错误，实际 %s", k)
	}
	if got := atomic.LoadInt32(aHits); got == 0 {
		t.Error("应至少尝试过一次请求")
	}
}

// 错误分类与脱敏的单元覆盖。
func TestAudioErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   AudioErrorKind
	}{
		{403, AudioErr403},
		{404, AudioErr404},
		{429, AudioErr429},
		{500, AudioErr5xx},
		{503, AudioErr5xx},
		{401, AudioErr403}, // 其余 4xx 按「被拒绝」处理
	}
	for _, c := range cases {
		if got := classifyHTTPStatus(c.status); got != c.want {
			t.Errorf("classifyHTTPStatus(%d) = %s, want %s", c.status, got, c.want)
		}
	}

	// 不可恢复类错误的重试预算为 1（不重试）
	for _, k := range []AudioErrorKind{AudioErr404, AudioErrNotAudio, AudioErrInvalidURL} {
		if got := k.maxAttemptsFor(audioAttemptLimit); got != 1 {
			t.Errorf("%s 的重试预算应为 1，实际 %d", k, got)
		}
	}
	// 403 / 空响应最多两次
	for _, k := range []AudioErrorKind{AudioErr403, AudioErrEmptyBody} {
		if got := k.maxAttemptsFor(audioAttemptLimit); got != 2 {
			t.Errorf("%s 的重试预算应为 2，实际 %d", k, got)
		}
	}
}

// 日志与错误信息中的 URL 必须脱敏，签名参数不得外泄。
func TestRedactURL(t *testing.T) {
	raw := "https://sf3-cdn-tos.douyinstatic.com/obj/ies-music/7286.mp3?x-expires=1700000000&sign=SECRETSIGNATURE"
	got := redactURL(raw)
	if strings.Contains(got, "SECRETSIGNATURE") || strings.Contains(got, "1700000000") {
		t.Errorf("签名/时效参数不得出现在脱敏结果中: %s", got)
	}
	if !strings.HasPrefix(got, "https://sf3-cdn-tos.douyinstatic.com/obj/ies-music/7286.mp3") {
		t.Errorf("host 与 path 应保留: %s", got)
	}
	if !strings.HasSuffix(got, "?...redacted") {
		t.Errorf("查询串应整体打码: %s", got)
	}
	// 无查询串：原样保留；非 URL：占位
	if got := redactURL("https://x/a.mp3"); got != "https://x/a.mp3" {
		t.Errorf("无查询串应原样返回，实际 %s", got)
	}
	if got := redactURL("not a url"); got != "(invalid-url)" {
		t.Errorf("非法地址应返回占位，实际 %s", got)
	}
}

// 整条链失败后是否值得重跑：只有瞬态类错误才算。
func TestAudioErrorRetryableAcrossChain(t *testing.T) {
	yes := []AudioErrorKind{AudioErrNetwork, AudioErrTimeout, AudioErr429, AudioErr5xx}
	no := []AudioErrorKind{AudioErr404, AudioErrNotAudio, AudioErrInvalidURL, AudioErr403}

	for _, k := range yes {
		err := newAudioError(k, 0, "https://x/a.mp3", "", 0, 1, nil)
		if !AudioErrorRetryableAcrossChain(err) {
			t.Errorf("%s 属于瞬态错误，应允许链级重试", k)
		}
	}
	for _, k := range no {
		err := newAudioError(k, 0, "https://x/a.mp3", "", 0, 1, nil)
		if AudioErrorRetryableAcrossChain(err) {
			t.Errorf("%s 不可恢复，不应触发链级重试", k)
		}
	}
	// 取消与无分类错误都不重试
	if AudioErrorRetryableAcrossChain(context.Canceled) {
		t.Error("上下文取消不应触发重试")
	}
	if AudioErrorRetryableAcrossChain(nil) {
		t.Error("nil 不应触发重试")
	}
}

// 偏好记录：成功来源前移，偏好来源失败后自动恢复完整顺序。
func TestRankAudioCandidatesPreference(t *testing.T) {
	resetAudioState(t)
	InitAudioPreferences("")

	cands := []AudioCandidate{
		{URL: "https://x/obj/ies-music/1.mp3", Source: SourceDouyinSSRURLList, Priority: 0},
		{URL: "https://x/obj/ies-music/2.mp3", Source: SourceDouyinMusicDetail, Priority: 1},
	}
	chainKey := CandidateChainKey(candidateURLs(cands))

	// 无偏好：顺序不变
	if got := candidateSources(RankAudioCandidates(cands)); got[0] != SourceDouyinSSRURLList {
		t.Errorf("无偏好时应保持原顺序，实际 %v", got)
	}

	// 记录 detail 成功 → 下次它排在最前
	recordAudioSuccess(chainKey, SourceDouyinMusicDetail)
	if got := candidateSources(RankAudioCandidates(cands)); got[0] != SourceDouyinMusicDetail {
		t.Errorf("历史成功来源应前移，实际 %v", got)
	}
	// 候选集合不得改变（只是顺序变化）
	if got := RankAudioCandidates(cands); len(got) != 2 {
		t.Errorf("排序不得增删候选，实际 %d 条", len(got))
	}

	// 偏好来源失败 → 清除偏好 → 恢复完整顺序（历史不是绝对规则）
	recordAudioFailure(chainKey, SourceDouyinMusicDetail, AudioErr404)
	if got := candidateSources(RankAudioCandidates(cands)); got[0] != SourceDouyinSSRURLList {
		t.Errorf("偏好源失败后应恢复原顺序，实际 %v", got)
	}

	// 按来源的统计须落到具体来源上，才能回答「是候选源的问题还是策略的问题」
	st := audioPrefs.items[chainKey]
	if st == nil {
		t.Fatal("偏好记录应存在")
	}
	if got := st.Sources[SourceDouyinMusicDetail]; got == nil || got.Success != 1 || got.Failure != 1 {
		t.Errorf("detail 来源统计应为 1 成功 / 1 失败，实际 %+v", got)
	}
	if got := st.Sources[SourceDouyinMusicDetail].LastFailureKind; got != string(AudioErr404) {
		t.Errorf("应记录最近失败分类，实际 %q", got)
	}
}

// 候选链注册表：解析阶段的来源信息应能在下载阶段被恢复（前端只回传地址列表）。
func TestCandidatesFromURLsRestoresSource(t *testing.T) {
	resetAudioState(t)
	urls := []string{
		"https://sf3-cdn-tos.douyinstatic.com/obj/ies-music/9201.mp3?sign=aa",
		"https://sf11-cdn-tos.douyinstatic.com/obj/tos-cn-ve-2774/9201x?sign=bb",
	}
	RegisterAudioChain(urls, []AudioCandidate{
		{URL: urls[0], Source: SourceDouyinSSRURLList, Priority: 0},
		{URL: urls[1], Source: SourceDouyinMusicDetail, Priority: 1},
	}, "9201")

	got := CandidatesFromURLs(urls)
	want := []string{SourceDouyinSSRURLList, SourceDouyinMusicDetail}
	if !equalStrings(candidateSources(got), want) {
		t.Errorf("来源应被恢复为 %v，实际 %v", want, candidateSources(got))
	}
	if id := ChainMusicID(urls); id != "9201" {
		t.Errorf("music_id 应被恢复，实际 %q", id)
	}

	// 未登记的链：不报错，来源退化为推断值
	unknown := CandidatesFromURLs([]string{"https://example.com/obj/ies-music/zz.mp3"})
	if len(unknown) != 1 || unknown[0].Source == "" {
		t.Errorf("未登记链应退化为推断来源，实际 %+v", unknown)
	}
}

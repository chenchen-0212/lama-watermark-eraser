package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"lama-watermark-eraser/internal/downloader"
	"lama-watermark-eraser/internal/inpaint"
	"lama-watermark-eraser/internal/queue"
	"lama-watermark-eraser/internal/ziputil"
)

// TaskParams 前端提交的去水印参数。
type TaskParams struct {
	Boxes    [][4]float64 `json:"boxes"`    // 多个框选区域（原图坐标或 0..1 比例）
	Relative bool         `json:"relative"` // 按比例适配不同尺寸
	Dilate   int          `json:"dilate"`   // 边缘外扩 px
	Margin   int          `json:"margin"`   // 上下文边距 px
	MaskPath string       `json:"maskPath"` // 可选掩膜文件（CLI 用）
	Strategy string       `json:"strategy"` // original（默认整图）| crop（逐框裁剪）
}

// 引擎生命周期状态（engine:status 事件的 state 字段）。
const (
	engineIdle     = "idle"
	engineStarting = "starting"
	engineReady    = "ready"
	engineError    = "error"
)

// ThumbInfo 缩略图信息：原图尺寸 + 缩放后的 data URL。
// 前端据 Width/Height 把框选坐标换算回原图像素空间。
type ThumbInfo struct {
	Width  int    `json:"width"`  // 原图宽
	Height int    `json:"height"` // 原图高
	Thumb  string `json:"thumb"`  // data:image/jpeg;base64,…
}

// App Wails 绑定与流水线调度。
type App struct {
	ctx context.Context

	// 引擎字段经 engineMu 保护；engineDone 在每次启动尝试进入终态时关闭，
	// 供 StartBatch 在预热进行中时阻塞等待。
	engineMu    sync.Mutex
	engine      *inpaint.PythonEngine
	engineState string        // idle | starting | ready | error
	engineErr   error         // 最近一次启动失败原因
	engineDone  chan struct{} // starting -> 终态 时关闭

	// 批处理任务队列（PRD §3）：单 worker 串行调度，承载 download/inpaint
	// 两类任务；StartBatch 与队列共用同一执行链路，取消与引擎 Kill 双保险
	// 语义不变。
	q *queue.Queue
}

// NewApp 构造；引擎在 startup 后经 LocatePythonEngine 定位并后台预热。
func NewApp() *App {
	return &App{engineState: engineIdle}
}

// startup Wails 生命周期：保存 ctx、恢复持久化的平台 Cookie，启动批处理任务
// 队列（加载 queue.json 并串行调度），并后台预热 Python 引擎（结果经
// engine:status 事件推送前端，失败不 panic）。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	downloader.LoadPlatformCookiePersist(appDataDir())

	// 批处理任务队列（PRD §3）：任务持久化到 workspace/queue.json；
	// 运行中任务被取消时终止引擎进程树以中断推理（与旧 CancelBatch 一致）。
	a.q = queue.New(&queueExecutor{app: a}, a.emitQueueEvent, filepath.Join(workspaceDir(), "queue.json"))
	a.q.SetOnCancel(func(t *queue.BatchTask) {
		if t.Type == queue.TypeInpaint {
			a.engineMu.Lock()
			e := a.engine
			a.engineMu.Unlock()
			if e != nil {
				_ = e.Kill()
			}
		}
	})
	a.q.Start(ctx)

	go func() {
		// 预热挂载在应用 ctx 上：应用退出时自动中断
		_, _ = a.ensureEngine(a.ctx)
	}()
}

// shutdown Wails 生命周期：应用退出时先停队列（持久化现场），再优雅关闭引擎
// 子进程（Bug B 修复之一）。与 KILL_ON_JOB_CLOSE 作业对象互为双保险：正常关窗
// 走此处（发送 shutdown 消息、等待退出并回收进程）；崩溃 / taskkill /F 等异常
// 退出由作业对象在内核层兜底杀树。
func (a *App) shutdown(_ context.Context) {
	if a.q != nil {
		a.q.Stop()
	}
	a.engineMu.Lock()
	e := a.engine
	a.engineMu.Unlock()
	if e == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.Close()
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		// Close 长时间未返回（如预热进行中持有引擎锁）：不阻塞退出路径，
		// 进程退出后作业对象会终止引擎进程树。
		go func() { _ = e.Kill() }()
	}
}

// emitQueueEvent 队列事件出口：queue:updated / queue:progress / queue:done，
// 以及队列执行任务时双路推送的 batch:progress / batch:done（PRD §4.3）。
func (a *App) emitQueueEvent(event string, payload interface{}) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, event, payload)
}

// ensureEngine 返回可用引擎。状态机：
//   - ready：直接返回；
//   - starting：阻塞等待本次启动尝试结束（尊重 ctx 取消）；
//   - idle / error：发起一次新的启动尝试（同步执行，完成后更新状态）。
//
// 启动失败后状态记为 error，下次调用（如用户重试 StartBatch）会重新尝试启动。
func (a *App) ensureEngine(ctx context.Context) (*inpaint.PythonEngine, error) {
	a.engineMu.Lock()
	switch a.engineState {
	case engineReady:
		e := a.engine
		a.engineMu.Unlock()
		return e, nil
	case engineStarting:
		done := a.engineDone
		a.engineMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, fmt.Errorf("等待引擎启动已取消")
		}
		a.engineMu.Lock()
		e, st, err := a.engine, a.engineState, a.engineErr
		a.engineMu.Unlock()
		if st == engineReady {
			return e, nil
		}
		return e, err
	}
	// idle / error：开始一次新的启动尝试
	a.engineState = engineStarting
	a.engineDone = make(chan struct{})
	a.engineMu.Unlock()

	e, err := a.startEngine(ctx)

	a.engineMu.Lock()
	if err != nil {
		a.engine = nil
		a.engineErr = err
		a.engineState = engineError
	} else {
		a.engine = e
		a.engineErr = nil
		a.engineState = engineReady
	}
	close(a.engineDone)
	a.engineMu.Unlock()
	return e, err
}

// startEngine 定位并启动引擎子进程，向前端推送 starting/ready/error 状态。
func (a *App) startEngine(ctx context.Context) (*inpaint.PythonEngine, error) {
	a.emitEngineStatus(engineStarting, "正在启动 AI 引擎（加载 big-lama 模型，首次约需数十秒）…")
	e, err := inpaint.NewEngine()
	if err != nil {
		a.emitEngineStatus(engineError, err.Error())
		return nil, err
	}
	if err := e.Start(ctx); err != nil {
		msg := err.Error()
		if tail := stderrTail(e, 3); tail != "" {
			msg += " | 引擎日志: " + tail
		}
		a.emitEngineStatus(engineError, msg)
		_ = e.Close() // 清理半启动的子进程（下次重试时重新拉起）
		return nil, err
	}
	a.emitEngineStatus(engineReady, "AI 引擎已就绪")
	return e, nil
}

// emitEngineStatus 推送 engine:status {state, message}。
func (a *App) emitEngineStatus(state, message string) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "engine:status", map[string]string{
		"state":   state,
		"message": message,
	})
}

// GetEngineStatus 返回当前引擎状态，供前端启动时同步（弥补订阅前错过的事件）。
func (a *App) GetEngineStatus() map[string]string {
	a.engineMu.Lock()
	defer a.engineMu.Unlock()
	msg := ""
	if a.engineErr != nil {
		msg = a.engineErr.Error()
	}
	return map[string]string{"state": a.engineState, "message": msg}
}

// GetAppVersion 返回当前应用版本号（如 "1.1.7"），供前端底部签名处展示。
//
// 单一来源约定：版本号在构建时经 -ldflags -X main.appVersion 注入（打包脚本读
// wails.json 的 productVersion 后传入），因此与安装包版本、exe 版本资源三者一致。
// 未注入时（例如 `go run` 直接调试）回退为 dev，避免界面显示一个错误的版本号。
func (a *App) GetAppVersion() string {
	if appVersion == "" {
		return "dev"
	}
	return appVersion
}

// stderrTail 摘取引擎 stderr 最近 max 行（诊断信息附加到错误消息）。
func stderrTail(e *inpaint.PythonEngine, max int) string {
	lines := e.LastStderr()
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return strings.Join(lines, " | ")
}

// ---------------------------------------------------------- 步骤1/2 链接与下载

// DetectURL 预检社媒链接平台；视频链接直接返回 ErrVideoNotSupported。
func (a *App) DetectURL(url string) (string, error) {
	platform, _, err := downloader.DetectPlatform(url)
	return platform, err
}

// DownloadSocial 下载社媒图文图片，返回帖子信息（含本地图片路径）。
func (a *App) DownloadSocial(url string) (*downloader.Post, error) {
	emitStatus := func(msg string) {
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "download:progress", map[string]string{"msg": msg})
		}
	}
	post, err := a.downloadPost(a.ctx, url, emitStatus)
	if err != nil {
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "download:error", map[string]string{"msg": err.Error()})
		}
		return nil, err
	}
	emitStatus(fmt.Sprintf("下载完成：%d 张图片", post.Count))
	return post, nil
}

// downloadMu 串行化下载：目录清理与写入不允许手动下载与队列任务并发同目录
// 互踩（队列内部已串行，此锁覆盖「手动下载 ↔ 队列任务」并发场景）。
var downloadMu sync.Mutex

// downloadPost 平台识别 + 分发下载（emit 为 nil 时静默）。
// DownloadSocial 与队列的 RunDownload 共用，保证两条路径行为一致。
// 修复「框选页相同图片重复出现」的两道防线：
//  1. 下载前清理该目录的旧图片文件——同一帖子的重复下载可能因 CDN 返回的
//     Content-Type 不同（jpeg/webp）把同一张图落成 01.jpg 与 01.webp 两个
//     文件（内容哈希不同、无法去重），清目录保证目录内只有本次下载的图；
//  2. 下载后按内容 SHA1 去重——解析层重复收集同一图（封面/正文同图、平台
//     images 数组重复项）时落成的同内容文件。
func (a *App) downloadPost(ctx context.Context, url string, emit func(string)) (*downloader.Post, error) {
	downloadMu.Lock()
	defer downloadMu.Unlock()

	platform, clean, err := downloader.DetectPlatform(url)
	if err != nil {
		return nil, err
	}
	if emit != nil {
		emit("已识别平台: " + platform)
	}
	workspace := workspaceDir()
	outDir := filepath.Join(workspace, "downloads", platform, idFor(clean))
	cleanDownloadDirImages(outDir)
	var post *downloader.Post
	switch platform {
	case "wechat":
		post, err = downloader.DownloadWeChat(ctx, clean, outDir)
	case "xhs":
		post, err = downloader.DownloadXHS(ctx, clean, outDir)
	case "douyin":
		post, err = downloader.DownloadDouyin(ctx, clean, outDir)
	default:
		err = downloader.ErrUnsupportedPlatform
	}
	if err != nil {
		return nil, err
	}
	if ded := dedupeImagesByContent(post.Files); len(ded) != len(post.Files) {
		removed := len(post.Files) - len(ded)
		post.Files = ded
		post.Count = len(ded)
		if emit != nil {
			emit(fmt.Sprintf("已去除 %d 张重复图片", removed))
		}
	}
	return post, nil
}

// cleanDownloadDirImages 清理下载目录中的旧图片文件（含 .part 半成品）。
// 音频（BGM bundle 模式存入的 mp3 等）不受影响。
func cleanDownloadDirImages(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".part") {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if inpaint.SupportedExts[strings.ToLower(filepath.Ext(name))] {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// dedupeImagesByContent 按内容 SHA1 去重图片：后出现的重复文件从磁盘删除并
// 剔除出清单，保证 post.Files 与目录内容都不含重复图片。
func dedupeImagesByContent(files []string) []string {
	if len(files) < 2 {
		return files
	}
	seen := make(map[string]bool, len(files))
	out := make([]string, 0, len(files))
	for _, p := range files {
		h, err := fileSHA1(p)
		if err != nil {
			out = append(out, p) // 读取失败的文件不处理，交由后续流程报错
			continue
		}
		if seen[h] {
			_ = os.Remove(p)
			continue
		}
		seen[h] = true
		out = append(out, p)
	}
	return out
}

// fileSHA1 计算文件内容 SHA1。
func fileSHA1(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// downloadAudioWithRetry 带一次自动重试的音频下载。社媒 CDN 存在偶发限流/
// 风控（典型表现：同一链接同一 Cookie 昨日可取、今日失败），短暂退避后
// 重走完整候选链（每次都会重新逐条探测，不复用失败结果）。
func downloadAudioWithRetry(ctx context.Context, candidates []string, dir, filename string) (string, error) {
	if len(candidates) == 0 {
		return "", fmt.Errorf("该帖子没有可下载的 BGM")
	}
	saved, err := downloader.DownloadAudioWithFallback(ctx, candidates, dir, filename)
	if err == nil {
		return saved, nil
	}
	// 退避 1.5s 后重试一次（尊重 ctx 取消）
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-ctx.Done():
		return "", err
	}
	if saved2, err2 := downloader.DownloadAudioWithFallback(ctx, candidates, dir, filename); err2 == nil {
		return saved2, nil
	}
	return "", fmt.Errorf(
		"BGM 获取失败（已自动重试仍失败，多为平台风控或 CDN 临时限制）：%w。"+
			"建议：① 稍后重试；② 抖音帖子请配置登录 Cookie（首页「抖音 Cookie 设置」）后再试；"+
			"③ 图片不受影响，可先正常去水印", err)
}

// DownloadBGM 下载帖子 BGM 音频到指定目录（用户在弹窗中自定义文件名）。
// filename 为空或仅含非法字符时兜底 bgm.mp3；返回保存的完整路径。
// candidates 为按优先级排序的候选地址链（取自 Post.AudioCandidates），
// 逐条尝试直到成功——单个 CDN 地址失效不再导致整体失败。
// BGM 属可选项，失败由前端提示，不影响图片流水线。
func (a *App) DownloadBGM(candidates []string, dir, filename string) (string, error) {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return downloadAudioWithRetry(ctx, candidates, dir, filename)
}

// DownloadBGMStandalone 单独下载 BGM：先下到系统临时目录（拿到按 Content-Type
// 解析出的真实扩展名），再弹「另存为」对话框让用户指定位置，复制过去后清理临时文件。
//
// 与 DownloadBGM 的区别：全程不写入源图目录，因此不会随「源图打包」进 zip，
// 适合只想单独留存 BGM 的场景。用户取消对话框返回空字符串且不报错（取消不是失败）。
func (a *App) DownloadBGMStandalone(candidates []string, filename string) (string, error) {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("该帖子没有可下载的 BGM")
	}

	tmpDir, err := os.MkdirTemp("", "lama-bgm-*")
	if err != nil {
		return "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpFile, err := downloader.DownloadAudioWithFallback(ctx, candidates, tmpDir, filename)
	if err != nil {
		return "", err
	}

	dst, err := runtime.SaveFileDialog(ctx, runtime.SaveDialogOptions{
		Title:                "另存背景音乐到…",
		DefaultFilename:      filepath.Base(tmpFile),
		CanCreateDirectories: true,
		Filters: []runtime.FileFilter{
			{DisplayName: "音频文件", Pattern: "*.mp3;*.m4a;*.aac;*.wav;*.ogg;*.flac"},
			{DisplayName: "所有文件", Pattern: "*.*"},
		},
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dst) == "" {
		return "", nil // 用户取消保存
	}
	// 用户未填扩展名时补上源文件扩展名，避免存出无后缀文件
	if filepath.Ext(dst) == "" {
		dst += filepath.Ext(tmpFile)
	}
	if err := copyFileStream(tmpFile, dst); err != nil {
		return "", fmt.Errorf("保存失败: %w", err)
	}
	return dst, nil
}

// copyFileStream 流式复制，避免把音频整段读进内存。
func copyFileStream(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// ---------------------------------------------------------- 步骤3 BGM 试听

// maxAudioPreviewBytes 试听音频体积上限。超过则拒绝：音频要整体读进内存再
// base64（约 1.33 倍膨胀）经 WebView 桥传入前端，过大既慢又占内存。
const maxAudioPreviewBytes = 24 << 20 // 24 MiB

// audioExtMime 音频扩展名 → MIME（决定 data URL 的媒体类型，浏览器按此选解码器）。
var audioExtMime = map[string]string{
	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".flac": "audio/flac",
}

// GetBGMAudio 读取 BGM 音频并返回可直接喂给前端 <audio> 的 data URL（第三步 BGM 试听）。
//
// 取源优先级：
//  1. localPath（前端 store.bgmSaved）——用户已下载到本机的 BGM 文件，直接读取；
//  2. candidates——帖子自带的候选音频地址链，依次探测并把首个可用地址下到本机
//     试听缓存后读取（缓存键取候选链稳定标识，同一曲目命中复用）。
//
// 为什么不把远端地址直接挂到前端 <audio>：社媒音频 CDN 有防盗链（Referer / Cookie），
// WebView 直连大概率 403；走后端下载可复用既有的请求头策略，且本地文件可重复播放。
func (a *App) GetBGMAudio(candidates []string, localPath string) (string, error) {
	// 1) 已下载的本地文件优先
	if p := strings.TrimSpace(localPath); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return audioDataURL(p)
		}
	}
	// 2) 下载到试听缓存（按稳定标识分目录，命中直接复用）
	if len(candidates) == 0 {
		return "", fmt.Errorf("该帖子没有可试听的 BGM")
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// 缓存键用候选链的规范化签名而非原始 URL：CDN 地址带 x-expires/sign 等
	// 时效参数，同一曲目每次解析出的 URL 都不同，直接哈希 URL 会导致缓存永不命中。
	cacheDir := filepath.Join(appDataDir(), "bgm-cache", shortHash(audioCacheKey(candidates)))
	if hit := firstAudioFile(cacheDir); hit != "" {
		return audioDataURL(hit)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("创建试听缓存目录失败: %w", err)
	}
	saved, err := downloadAudioWithRetry(ctx, candidates, cacheDir, "bgm")
	if err != nil {
		return "", err
	}
	return audioDataURL(saved)
}

// audioCacheKey 由候选链生成稳定缓存键：去掉查询串（签名/时效参数）与
// 主机名差异，只保留各候选的路径部分、去重后排序，使同一曲目无论 CDN 域或
// 签名如何变化、候选数量多少，都落到同一缓存目录。
func audioCacheKey(candidates []string) string {
	paths := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, u := range candidates {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		if i := strings.Index(u, "?"); i >= 0 {
			u = u[:i]
		}
		if i := strings.Index(u, "://"); i >= 0 {
			if j := strings.IndexByte(u[i+3:], '/'); j >= 0 {
				u = u[i+3+j:]
			}
		}
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		paths = append(paths, u)
	}
	if len(paths) == 0 {
		return strings.Join(candidates, "|")
	}
	sort.Strings(paths)
	return strings.Join(paths, "|")
}

// audioDataURL 读取本地音频并编码为 data URL。
func audioDataURL(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("音频文件不存在: %s", filepath.Base(path))
	}
	if st.Size() > maxAudioPreviewBytes {
		return "", fmt.Errorf("音频过大（%d MB），暂不支持在线试听", st.Size()>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mime := audioExtMime[strings.ToLower(filepath.Ext(path))]
	if mime == "" {
		mime = "audio/mpeg" // 未知扩展名按 mp3 兜底（WebView 仍会嗅探内容）
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// firstAudioFile 返回目录下第一个完整音频文件（缓存命中判断）；无则空串。
// 跳过 .part：SaveAudio 下载中途的临时文件不可用于播放。
func firstAudioFile(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		if _, ok := audioExtMime[strings.ToLower(filepath.Ext(e.Name()))]; ok {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// shortHash 音频地址 → 试听缓存目录名（避免 URL 中的非法字符）。
func shortHash(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:8])
}

// ---------------------------------------------------------- 步骤4 去水印

// StartBatch 批量去水印（前台单任务便捷入口）：同步校验参数后包装成一个
// inpaint 任务交给队列串行执行——与批量队列任务共用同一调度与互斥，
// 消除旧实现「手动批次与队列任务抢占」的双重互斥漏洞（PRD §3.2/§6.2）。
// title 为展示名（来源下载任务的帖子标题），空时由后端按目录反查兜底。
// 进度照旧经 batch:progress 事件推送（附带 taskId 供前端防串扰），
// 可被 CancelBatch / CancelQueueTask 取消。
func (a *App) StartBatch(inDir, outDir string, p TaskParams, title string) (*queue.BatchTask, error) {
	if p.MaskPath == "" && !hasValidBoxes(p.Boxes) {
		return nil, fmt.Errorf("请先在预览图上框选水印区域")
	}
	if files := inpaint.CollectImages(inDir, outDir, false); len(files) == 0 {
		return nil, fmt.Errorf("输入目录中没有可处理的图片")
	}
	if a.q == nil {
		return nil, fmt.Errorf("任务队列未就绪，请重启应用")
	}
	// 标题兜底：按源目录反查已完成的下载任务（覆盖无前端上下文的入队路径）
	if strings.TrimSpace(title) == "" {
		for _, t := range a.q.Snapshot() {
			if t.Type == queue.TypeDownload && t.ResultDir != "" && strings.HasPrefix(inDir, t.ResultDir) {
				title = t.PostRef
				break
			}
		}
	}
	return a.q.Enqueue(&queue.BatchTask{
		Type:   queue.TypeInpaint,
		Title:  strings.TrimSpace(title),
		InDir:  inDir,
		OutDir: outDir,
		Params: &inpaint.TaskParams{
			Boxes:    p.Boxes,
			Relative: p.Relative,
			Dilate:   p.Dilate,
			Margin:   p.Margin,
			MaskPath: p.MaskPath,
			Strategy: p.Strategy,
		},
	})
}

// EnqueueInpaint 固化当前框选参数，建去水印任务入队（PRD §4.2 入口三）。
// 与 StartBatch 同一实现：手动「立即执行」与「加入队列」已统一走队列串行调度。
func (a *App) EnqueueInpaint(inDir, outDir string, p TaskParams, title string) (*queue.BatchTask, error) {
	return a.StartBatch(inDir, outDir, p, title)
}

// CancelBatch 取消当前正在执行的批次任务：先取消任务 ctx（阻止后续图片继续
// 推理/落盘），再立即终止引擎进程树以中断进行中的推理请求；引擎在下次使用时
// 懒重启。无运行任务时为空操作（保持旧语义：仅一个批次可能处于执行中）。
func (a *App) CancelBatch() error {
	if a.q != nil {
		a.q.CancelRunning()
	}
	a.engineMu.Lock()
	e := a.engine
	a.engineMu.Unlock()
	if e != nil {
		_ = e.Kill()
	}
	return nil
}

// runInpaint 执行一次批量去水印（同步，直到批次结束才返回）。
// 进度经 onProgress 上报；引擎未就绪时等待/预热，可被 ctx 取消。
// 队列 worker 的 inpaint 任务收敛到此处执行，与手动路径共享引擎互斥、
// 输出目录清理与事件语义（PRD §6.2 关键改造点）。
func (a *App) runInpaint(ctx context.Context, inDir, outDir string, p *TaskParams,
	onProgress func(i, total int, name, status, info string)) (queue.InpaintResult, error) {
	if p == nil || (p.MaskPath == "" && !hasValidBoxes(p.Boxes)) {
		return queue.InpaintResult{}, queue.Permanent(fmt.Errorf("请先在预览图上框选水印区域"))
	}
	files := inpaint.CollectImages(inDir, outDir, false)
	if len(files) == 0 {
		return queue.InpaintResult{}, queue.Permanent(fmt.Errorf("输入目录中没有可处理的图片"))
	}
	// 启动/等待引擎（可能阻塞至预热完成；取消后会以错误返回）
	engine, err := a.ensureEngine(ctx)
	if err != nil {
		return queue.InpaintResult{}, err
	}
	// 清空输出目录，避免上一次运行的结果残留（否则结果页会混入旧图）
	if err := prepareOutputDir(outDir); err != nil {
		return queue.InpaintResult{}, err
	}
	runtime.EventsEmit(a.ctx, "pipe:stage", map[string]string{"stage": "inpainting"})
	results := inpaint.ProcessBatch(engine, files, outDir, inpaint.TaskParams{
		Boxes:    p.Boxes,
		Relative: p.Relative,
		Dilate:   p.Dilate,
		Margin:   p.Margin,
		MaskPath: p.MaskPath,
		Strategy: p.Strategy,
	}, onProgress, ctx)
	res := queue.InpaintResult{OutDir: outDir, Total: len(files)}
	for _, r := range results {
		switch r.Status {
		case "ok":
			res.OK++
		case "fail":
			res.FailList = append(res.FailList, r.Name+": "+r.Info)
		}
	}
	return res, nil
}

// ---------------------------------------------------------- 批处理任务队列绑定（PRD §4.2）

// queueExecutor 队列执行器：复用既有下载/去水印链路，队列层不含业务逻辑。
type queueExecutor struct{ app *App }

// RunDownload 执行下载任务。平台不支持/视频链接/无图属确定性错误（重试无益），
// 其余（网络超时、风控拦截、CDN 波动）按瞬时错误走自动重试——重试会重新
// 解析链接、重走候选链，不复用可能已过期的旧地址。
func (e *queueExecutor) RunDownload(ctx context.Context, t *queue.BatchTask) (string, string, []string, error) {
	emit := func(msg string) {
		if e.app.ctx != nil {
			runtime.EventsEmit(e.app.ctx, "download:progress", map[string]string{"msg": msg})
		}
	}
	post, err := e.app.downloadPost(ctx, t.URL, emit)
	if err != nil {
		if errors.Is(err, downloader.ErrUnsupportedPlatform) ||
			errors.Is(err, downloader.ErrVideoNotSupported) ||
			errors.Is(err, downloader.ErrNoImages) {
			return "", "", nil, queue.Permanent(err)
		}
		return "", "", nil, err
	}
	return post.Dir, fmt.Sprintf("%s | %d 张", post.Title, post.Count), post.Files, nil
}

// RunInpaint 执行去水印任务（单张失败不视为任务错误，结果经汇总传达）。
func (e *queueExecutor) RunInpaint(ctx context.Context, t *queue.BatchTask, onProgress queue.ProgressFn) (queue.InpaintResult, error) {
	var params *TaskParams
	if t.Params != nil {
		params = &TaskParams{
			Boxes:    t.Params.Boxes,
			Relative: t.Params.Relative,
			Dilate:   t.Params.Dilate,
			Margin:   t.Params.Margin,
			MaskPath: t.Params.MaskPath,
			Strategy: t.Params.Strategy,
		}
	}
	return e.app.runInpaint(ctx, t.InDir, t.OutDir, params, onProgress)
}

// AddQueueTasks 批量建下载任务：逐条平台预检，失败条目不入队并附原因
// （PRD §3.1 手动批量触发 / §8 混入视频链接即时反馈）。
func (a *App) AddQueueTasks(urls []string) (*queue.EnqueueReport, error) {
	if a.q == nil {
		return nil, fmt.Errorf("任务队列未就绪，请重启应用")
	}
	report := &queue.EnqueueReport{}
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		platform, _, err := downloader.DetectPlatform(u)
		if err != nil {
			report.Failures = append(report.Failures, queue.EnqueueFailure{Input: u, Reason: err.Error()})
			continue
		}
		tk, err := a.q.Enqueue(&queue.BatchTask{Type: queue.TypeDownload, URL: u, Platform: platform})
		if err != nil {
			report.Failures = append(report.Failures, queue.EnqueueFailure{Input: u, Reason: err.Error()})
			continue
		}
		report.Tasks = append(report.Tasks, tk)
	}
	if len(report.Tasks) == 0 && len(report.Failures) == 0 {
		return nil, fmt.Errorf("没有可用的链接")
	}
	return report, nil
}

// ListQueueTasks 返回队列全量快照（面板打开时拉取 + queue:updated 增量同步）。
func (a *App) ListQueueTasks() ([]queue.BatchTask, error) {
	if a.q == nil {
		return []queue.BatchTask{}, nil
	}
	return a.q.Snapshot(), nil
}

// RetryQueueTask 手动重试：failed/canceled → pending（重试计数清零）。
func (a *App) RetryQueueTask(id string) error {
	if a.q == nil {
		return fmt.Errorf("任务队列未就绪")
	}
	return a.q.Retry(id)
}

// CancelQueueTask 取消指定任务（pending 直接取消；running 取消 ctx 并按任务
// 类型终止引擎）。
func (a *App) CancelQueueTask(id string) error {
	if a.q == nil {
		return fmt.Errorf("任务队列未就绪")
	}
	return a.q.Cancel(id)
}

// RemoveQueueTask 移除终态任务记录。
func (a *App) RemoveQueueTask(id string) error {
	if a.q == nil {
		return fmt.Errorf("任务队列未就绪")
	}
	return a.q.Remove(id)
}

// ClearFinishedTasks 清空终态任务记录。taskType 为空清全部；否则仅清该类型
// （download | inpaint）——两类队列独立管理。
func (a *App) ClearFinishedTasks(taskType string) error {
	if a.q == nil {
		return fmt.Errorf("任务队列未就绪")
	}
	a.q.ClearFinished(taskType)
	return nil
}

// ---------------------------------------------------------- 预览与输出

// GetImageBase64 读取图片缩放到 maxW 宽，返回 data URL（JPEG）。
func (a *App) GetImageBase64(path string, maxW int) (string, error) {
	t, err := a.GetThumb(path, maxW)
	if err != nil {
		return "", err
	}
	return t.Thumb, nil
}

// GetThumb 读取图片并返回原图尺寸与缩放后的 JPEG data URL。
// 缩放仅在原图宽超过 maxW 时进行，保持宽高比。
func (a *App) GetThumb(path string, maxW int) (*ThumbInfo, error) {
	img, err := inpaint.DecodeImage(path)
	if err != nil {
		return nil, err
	}
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	if maxW > 0 && w > maxW {
		nh := h * maxW / w
		if nh < 1 {
			nh = 1
		}
		img = toUniformNRGBA(imaging.Resize(img, maxW, nh, imaging.Lanczos))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return &ThumbInfo{
		Width:  w,
		Height: h,
		Thumb:  "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

// ZipDirectory 打包目录为 zip，返回生成路径。
func (a *App) ZipDirectory(dir string) (string, error) {
	dst := filepath.Join(workspaceDir(), fmt.Sprintf("去水印结果_%s.zip", time.Now().Format("20060102_150405")))
	if err := ziputil.ZipDir(dir, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// ExportSourceZip 打包源图目录（未去水印原图）为 zip，返回生成路径。
// 交付链路与结果页 ZipDirectory 完全一致：生成 zip → 前端 PickSaveFile → CopyFile；
// 命名沿用同一时间戳规则（源图_<时间戳>.zip），避免重复导出重名冲突。
func (a *App) ExportSourceZip(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("源图目录为空")
	}
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("源图目录不存在: %s", dir)
	}
	dst := filepath.Join(workspaceDir(), fmt.Sprintf("源图_%s.zip", time.Now().Format("20060102_150405")))
	if err := ziputil.ZipDir(dir, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// PickDirectory 目录选择对话框。
func (a *App) PickDirectory() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: "选择文件夹"})
}

// PickSaveFile 保存文件对话框，返回所选路径。
func (a *App) PickSaveFile(defaultName string) (string, error) {
	if strings.TrimSpace(defaultName) == "" {
		defaultName = "result.zip"
	}
	return runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "保存到…",
		DefaultFilename: defaultName,
	})
}

// CopyFile 复制文件（用于把生成的 zip 保存到用户指定位置）。
func (a *App) CopyFile(src, dst string) error {
	if src == "" || dst == "" {
		return fmt.Errorf("路径为空")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

// ListImages 列出目录下的图片文件（供前端拉取结果列表）。
func (a *App) ListImages(dir string) ([]string, error) {
	return inpaint.CollectImages(dir, "", false), nil
}

// PrepareSubset 将勾选的图片复制到独立目录，用于「仅处理勾选项」。
func (a *App) PrepareSubset(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("未勾选任何图片")
	}
	dir := filepath.Join(workspaceDir(), "selected", time.Now().Format("20060102_150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for i, src := range paths {
		data, err := os.ReadFile(src)
		if err != nil {
			return "", err
		}
		dst := filepath.Join(dir, fmt.Sprintf("%03d_%s", i+1, filepath.Base(src)))
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// ---------------------------------------------------------- 平台 Cookie 设置

// SetDouyinCookie 保存抖音 Cookie（传空白串清除并删除持久化文件）。
// Cookie 保存到 %LOCALAPPDATA%/LaMaWatermarkRemover/douyin_cookie.txt，启动时自动恢复。
func (a *App) SetDouyinCookie(raw string) error {
	return downloader.SetPlatformCookiePersist(appDataDir(), "douyin", raw)
}

// GetDouyinCookie 读取当前生效的抖音 Cookie（未配置返回空串）。
func (a *App) GetDouyinCookie() string {
	return downloader.PlatformCookie("douyin")
}

// ---------------------------------------------------------- 工具

func toUniformNRGBA(img image.Image) *image.NRGBA {
	if n, ok := img.(*image.NRGBA); ok && n.Stride == n.Bounds().Dx()*4 {
		return n
	}
	dst := image.NewNRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
	draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Src)
	return dst
}

func workspaceDir() string {
	dir := filepath.Join(appDataDir(), "workspace")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// appDataDir 应用数据目录，用于 workspace、平台 Cookie 持久化等：
//   - Windows: %LOCALAPPDATA%\LaMaWatermarkRemover
//   - macOS:   ~/Library/Application Support/LaMaWatermarkRemover
//   - 其他:    用户缓存目录/LaMaWatermarkRemover
//
// 注意：本文件已导入 Wails 的 runtime 包，故不引入标准库 runtime，
// 改用「环境变量优先 + 平台标准目录回退」的等价判定。
func appDataDir() string {
	base := os.Getenv("LOCALAPPDATA") // 仅 Windows 有值
	if base == "" {
		if cfg, err := os.UserConfigDir(); err == nil {
			base = cfg
		} else if cache, err := os.UserCacheDir(); err == nil {
			base = cache
		} else {
			base = "."
		}
	}
	dir := filepath.Join(base, "LaMaWatermarkRemover")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// prepareOutputDir 清空并重建输出目录，避免旧结果残留（旧文件会因 UniqueDst 加
// (n) 后缀而累积，导致结果页混入上一轮图片）。
func prepareOutputDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// hasValidBoxes 判断是否至少有一个有效框选（x2>x1 且 y2>y1）。
func hasValidBoxes(boxes [][4]float64) bool {
	for _, b := range boxes {
		if b[2] > b[0] && b[3] > b[1] {
			return true
		}
	}
	return false
}

// idFor 从 URL 生成安全的目录名片段。
func idFor(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(u), "https://"), "http://")
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, s)
	if len(s) > 60 {
		s = s[len(s)-60:]
	}
	if s == "" {
		return "item"
	}
	return s
}

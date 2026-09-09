package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"lama-watermark-eraser/internal/downloader"
	"lama-watermark-eraser/internal/inpaint"
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

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
}

// NewApp 构造；引擎在 startup 后经 LocatePythonEngine 定位并后台预热。
func NewApp() *App {
	return &App{engineState: engineIdle}
}

// startup Wails 生命周期：保存 ctx、恢复持久化的平台 Cookie，并后台预热
// Python 引擎（结果经 engine:status 事件推送前端，失败不 panic）。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	downloader.LoadPlatformCookiePersist(appDataDir())
	go func() {
		// 预热挂载在应用 ctx 上：应用退出时自动中断
		_, _ = a.ensureEngine(a.ctx)
	}()
}

// shutdown Wails 生命周期：应用退出时优雅关闭引擎子进程（Bug B 修复之一）。
// 与 KILL_ON_JOB_CLOSE 作业对象互为双保险：正常关窗走此处（发送 shutdown
// 消息、等待退出并回收进程）；崩溃 / taskkill /F 等异常退出由作业对象在
// 内核层兜底杀树。
func (a *App) shutdown(_ context.Context) {
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
	platform, clean, err := downloader.DetectPlatform(url)
	if err != nil {
		return nil, err
	}
	emitStatus := func(msg string) {
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "download:progress", map[string]string{"msg": msg})
		}
	}
	emitStatus("已识别平台: " + platform)

	workspace := workspaceDir()
	outDir := filepath.Join(workspace, "downloads", platform, idFor(clean))
	var post *downloader.Post
	switch platform {
	case "wechat":
		post, err = downloader.DownloadWeChat(a.ctx, clean, outDir)
	case "xhs":
		post, err = downloader.DownloadXHS(a.ctx, clean, outDir)
	case "douyin":
		post, err = downloader.DownloadDouyin(a.ctx, clean, outDir)
	default:
		err = downloader.ErrUnsupportedPlatform
	}
	if err != nil {
		if a.ctx != nil {
			runtime.EventsEmit(a.ctx, "download:error", map[string]string{"msg": err.Error()})
		}
		return nil, err
	}
	emitStatus(fmt.Sprintf("下载完成：%d 张图片", post.Count))
	return post, nil
}

// ---------------------------------------------------------- 步骤4 去水印

// StartBatch 批量去水印（异步执行，进度经 batch:progress 事件推送）。
// 引擎未就绪时本调用会等待/触发预热，期间可被 CancelBatch 取消。
func (a *App) StartBatch(inDir, outDir string, p TaskParams) error {
	if p.MaskPath == "" && !hasValidBoxes(p.Boxes) {
		return fmt.Errorf("请先在预览图上框选水印区域")
	}
	files := inpaint.CollectImages(inDir, outDir, false)
	if len(files) == 0 {
		return fmt.Errorf("输入目录中没有可处理的图片")
	}

	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return fmt.Errorf("已有任务在执行中")
	}
	batchCtx, cancel := context.WithCancel(a.ctx)
	a.running = true
	a.cancel = cancel
	a.mu.Unlock()

	fail := func(err error) error {
		a.mu.Lock()
		a.running = false
		a.cancel = nil
		a.mu.Unlock()
		cancel()
		return err
	}

	// 启动/等待引擎（可能阻塞至预热完成；取消后会以错误返回）
	engine, err := a.ensureEngine(batchCtx)
	if err != nil {
		return fail(err)
	}

	// 清空输出目录，避免上一次运行的结果残留（否则结果页会混入旧图）
	if err := prepareOutputDir(outDir); err != nil {
		return fail(err)
	}

	total := len(files)
	go func() {
		defer func() {
			cancel() // 释放 batchCtx 资源
			a.mu.Lock()
			a.running = false
			a.cancel = nil
			a.mu.Unlock()
		}()
		runtime.EventsEmit(a.ctx, "pipe:stage", map[string]string{"stage": "inpainting"})
		cb := func(i, t int, name, status, info string) {
			runtime.EventsEmit(a.ctx, "batch:progress", map[string]interface{}{
				"index": i, "total": t, "name": name, "status": status, "info": info,
			})
		}
		results := inpaint.ProcessBatch(engine, files, outDir, inpaint.TaskParams{
			Boxes:    p.Boxes,
			Relative: p.Relative,
			Dilate:   p.Dilate,
			Margin:   p.Margin,
			MaskPath: p.MaskPath,
			Strategy: p.Strategy,
		}, cb, batchCtx)
		ok := 0
		for _, r := range results {
			if r.Status == "ok" {
				ok++
			}
		}
		runtime.EventsEmit(a.ctx, "batch:done", map[string]interface{}{
			"ok": ok, "total": total, "outDir": outDir, "canceled": batchCtx.Err() != nil,
		})
	}()
	return nil
}

// CancelBatch 取消当前批处理：先取消 ctx（阻止后续图片继续推理/落盘），
// 再立即终止引擎进程树以中断进行中的推理请求；引擎在下次使用时懒重启。
func (a *App) CancelBatch() error {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	a.engineMu.Lock()
	e := a.engine
	a.engineMu.Unlock()
	if e != nil {
		_ = e.Kill()
	}
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

// appDataDir 应用数据目录：%LOCALAPPDATA%/LaMaWatermarkRemover。
// 用于 workspace、平台 Cookie 持久化等。
func appDataDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base, _ = os.UserCacheDir()
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

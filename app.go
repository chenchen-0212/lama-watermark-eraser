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
	Boxes    [][4]float64 `json:"boxes"`   // 多个框选区域（原图坐标或 0..1 比例）
	Relative bool         `json:"relative"` // 按比例适配不同尺寸
	Dilate   int          `json:"dilate"`   // 边缘外扩 px
	Margin   int          `json:"margin"`   // 上下文边距 px
	MaskPath string       `json:"maskPath"` // 可选掩膜文件（CLI 用）
}

// ThumbInfo 缩略图信息：原图尺寸 + 缩放后的 data URL。
// 前端据 Width/Height 把框选坐标换算回原图像素空间。
type ThumbInfo struct {
	Width  int    `json:"width"`  // 原图宽
	Height int    `json:"height"` // 原图高
	Thumb  string `json:"thumb"`  // data:image/jpeg;base64,…
}

// App Wails 绑定与流水线调度。
type App struct {
	ctx       context.Context
	dllPath   string
	modelPath string
	engine    *inpaint.Engine

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
}

// NewApp 构造；资源已由 main 完成解压并传入路径。
func NewApp(dllPath, modelPath string) *App {
	return &App{dllPath: dllPath, modelPath: modelPath}
}

// startup Wails 生命周期：保存 ctx 并后台预热模型。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go func() {
		if _, err := a.ensureEngine(); err != nil {
			runtime.EventsEmit(a.ctx, "pipe:error", map[string]string{
				"msg": "模型加载失败: " + err.Error(),
			})
		}
	}()
}

func (a *App) ensureEngine() (*inpaint.Engine, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.engine != nil {
		return a.engine, nil
	}
	e, err := inpaint.NewEngine(a.modelPath, a.dllPath)
	if err != nil {
		return nil, err
	}
	a.engine = e
	return e, nil
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
func (a *App) StartBatch(inDir, outDir string, p TaskParams) error {
	if p.MaskPath == "" && !hasValidBoxes(p.Boxes) {
		return fmt.Errorf("请先在预览图上框选水印区域")
	}
	engine, err := a.ensureEngine()
	if err != nil {
		return err
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
	ctx, cancel := context.WithCancel(a.ctx)
	a.running = true
	a.cancel = cancel
	a.mu.Unlock()

	// 清空输出目录，避免上一次运行的结果残留（否则结果页会混入旧图）
	if err := prepareOutputDir(outDir); err != nil {
		return err
	}

	total := len(files)
	go func() {
		defer func() {
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
		}, cb, ctx)
		ok := 0
		for _, r := range results {
			if r.Status == "ok" {
				ok++
			}
		}
		runtime.EventsEmit(a.ctx, "batch:done", map[string]interface{}{
			"ok": ok, "total": total, "outDir": outDir, "canceled": ctx.Err() != nil,
		})
	}()
	return nil
}

// CancelBatch 取消当前批处理。
func (a *App) CancelBatch() error {
	a.mu.Lock()
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
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
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base, _ = os.UserCacheDir()
	}
	dir := filepath.Join(base, "LaMaWatermarkRemover", "workspace")
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

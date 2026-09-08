package inpaint

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"time"
)

// 推理策略（TaskParams.Strategy）。
const (
	// StrategyOriginal 整图推理（默认）：多框先 MergeMasks 合并为单掩膜，
	// 整图单次前向。与 IOPaint 默认行为一致（P0 红线：效果对齐 IOPaint）。
	StrategyOriginal = "original"
	// StrategyCrop 逐框裁剪推理：每个框单独裁剪（±margin）后前向，
	// 大图多水印场景速度更快，但上下文受限、效果略逊于整图。
	StrategyCrop = "crop"
)

// TaskParams 去水印任务参数（与 Python 版 CLI/GUI 参数对齐）。
type TaskParams struct {
	Boxes    [][4]float64 // 多个框选 x1,y1,x2,y2（像素或 0..1 比例）
	Relative bool
	Dilate   int
	Margin   int
	MaskPath string
	Strategy string // original（默认）| crop
}

// normalizeStrategy 空值/未知策略回退为默认整图策略。
func normalizeStrategy(s string) string {
	if s == StrategyCrop {
		return StrategyCrop
	}
	return StrategyOriginal
}

// Result 单张处理结果。
type Result struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | fail | skip
	Info   string `json:"info"`
}

// ProgressFn 进度回调：i/total 为 1-based；status ∈ ok|fail|skip。
type ProgressFn func(i, total int, name, status, info string)

// InpaintImage 对 img（就地修改）按 mask 做 LaMa 修复。
//
// mask 为 W*H 的 0/255 单通道掩膜；margin 仅在 crop 策略下作为裁剪上下文边距。
// Go 侧不做任何缩放/填充：整图（或裁剪区）RGB888 + mask 经 PythonEngine.Infer
// 原样交给 Python 子进程；mod 8 对称填充由 IOPaint 对齐的 inpaint_core.forward
// 完成（numpy pad mode="symmetric"），输出经 composite 写回时保证
// 「非掩膜区像素保持原图不变」（PRD 贴回语义）。
func InpaintImage(ctx context.Context, e *PythonEngine, img *image.NRGBA, mask []byte, margin int, strategy string) error {
	if e == nil {
		return newEngineError(CodeEngineStartFailed, "引擎未启动，无法执行推理")
	}
	W := img.Bounds().Dx()
	H := img.Bounds().Dy()
	if len(mask) != W*H {
		return newEngineError(CodeInvalidImage, "掩膜尺寸不符: 期望 %d 实得 %d", W*H, len(mask))
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if normalizeStrategy(strategy) == StrategyCrop {
		bbox, ok := maskBBox(mask, W, H)
		if !ok {
			return nil // 空掩膜，原样返回
		}
		m := margin
		if m < 0 {
			m = 0
		}
		cx1 := clampInt(bbox.X1-m, 0, W)
		cy1 := clampInt(bbox.Y1-m, 0, H)
		cx2 := clampInt(bbox.X2+m, 0, W)
		cy2 := clampInt(bbox.Y2+m, 0, H)
		if cx2-cx1 <= 0 || cy2-cy1 <= 0 {
			return nil
		}
		crop := cropNRGBA(img, cx1, cy1, cx2, cy2)
		mcrop := cropMask(mask, W, cx1, cy1, cx2, cy2)
		out, err := inferNRGBA(ctx, e, crop, mcrop)
		if err != nil {
			return err
		}
		// 贴回时仅覆写掩膜区，裁剪区内掩膜外像素保持原图
		compositeMaskedAt(img, out, mcrop, cx1, cy1)
		return nil
	}

	// 整图策略（默认）：全图单次前向，无缩放无裁剪，与 IOPaint 行为一致
	out, err := inferNRGBA(ctx, e, img, mask)
	if err != nil {
		return err
	}
	compositeMasked(img, out, mask)
	return nil
}

// inferNRGBA 将 NRGBA 图像与单通道掩膜编码为 RGB888 交给引擎推理，
// 返回同尺寸的修复结果 NRGBA（stride 恒为 w*4）。
func inferNRGBA(ctx context.Context, e *PythonEngine, img *image.NRGBA, mask []byte) (*image.NRGBA, error) {
	rgb, w, h := NRGBAToRGB(img)
	out, err := e.Infer(ctx, rgb, mask, w, h)
	if err != nil {
		return nil, err
	}
	return RGBToNRGBA(out, w, h), nil
}

// compositeMasked 将修复结果 fixed 覆写进 img 的掩膜区域（掩膜外保留原像素）。
// 两图同尺寸；逐行使用各自 stride，正确处理非 4 对齐行宽。
func compositeMasked(img, fixed *image.NRGBA, mask []byte) {
	W := img.Bounds().Dx()
	H := img.Bounds().Dy()
	for y := 0; y < H; y++ {
		dstRow := img.Pix[y*img.Stride:]
		srcRow := fixed.Pix[y*fixed.Stride:]
		for x := 0; x < W; x++ {
			if mask[y*W+x] > 0 {
				d := dstRow[x*4 : x*4+4]
				s := srcRow[x*4 : x*4+4]
				d[0], d[1], d[2] = s[0], s[1], s[2]
			}
		}
	}
}

// compositeMaskedAt 将 fixed（裁剪区结果）中掩膜为 1 的像素写回 img 的 (ox,oy) 偏移处。
func compositeMaskedAt(img, fixed *image.NRGBA, mcrop []byte, ox, oy int) {
	w := fixed.Bounds().Dx()
	h := fixed.Bounds().Dy()
	for y := 0; y < h; y++ {
		dstRow := img.Pix[(oy+y)*img.Stride:]
		srcRow := fixed.Pix[y*fixed.Stride:]
		for x := 0; x < w; x++ {
			if mcrop[y*w+x] > 0 {
				d := dstRow[(ox+x)*4 : (ox+x)*4+4]
				s := srcRow[x*4 : x*4+4]
				d[0], d[1], d[2] = s[0], s[1], s[2]
			}
		}
	}
}

// ProcessBatch 批量去水印。单张失败不中断；ctx 取消后剩余标记为 skip。
func ProcessBatch(e *PythonEngine, files []string, outDir string, params TaskParams,
	cb ProgressFn, ctx context.Context) []Result {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return []Result{}
	}
	results := make([]Result, 0, len(files))
	total := len(files)
	for i, p := range files {
		name := filepath.Base(p)
		if canceled(ctx) {
			res := Result{Name: name, Status: "skip", Info: "已取消"}
			results = append(results, res)
			if cb != nil {
				cb(i+1, total, name, res.Status, res.Info)
			}
			continue
		}
		t0 := time.Now()
		res := processOne(e, p, outDir, params, ctx)
		if res.Status == "ok" {
			res.Info = fmt.Sprintf("%.1fs", time.Since(t0).Seconds())
		}
		results = append(results, res)
		if cb != nil {
			cb(i+1, total, name, res.Status, res.Info)
		}
	}
	return results
}

func processOne(e *PythonEngine, path, outDir string, params TaskParams, ctx context.Context) (res Result) {
	name := filepath.Base(path)
	defer func() {
		if r := recover(); r != nil {
			res = Result{Name: name, Status: "fail", Info: fmt.Sprint(r)}
		}
	}()
	// 取消检查点：开始处理前
	if canceled(ctx) {
		return Result{Name: name, Status: "skip", Info: "已取消"}
	}
	img, err := DecodeImage(path)
	if err != nil {
		return Result{Name: name, Status: "fail", Info: err.Error()}
	}
	W := img.Bounds().Dx()
	H := img.Bounds().Dy()

	if params.MaskPath != "" {
		// 掩膜文件模式：单掩膜直接推理（策略照常生效）
		mask, err := MaskFromFile(params.MaskPath, W, H)
		if err != nil {
			return Result{Name: name, Status: "fail", Info: err.Error()}
		}
		if err := InpaintImage(ctx, e, img, mask, params.Margin, params.Strategy); err != nil {
			return canceledOr(ctx, name, err.Error())
		}
	} else if normalizeStrategy(params.Strategy) == StrategyCrop {
		// crop 策略：逐框独立裁剪推理（框间可取消）
		for _, box := range params.Boxes {
			if canceled(ctx) {
				return Result{Name: name, Status: "skip", Info: "已取消"}
			}
			mask := BuildBoxMask(W, H, box, params.Relative, params.Dilate)
			if err := InpaintImage(ctx, e, img, mask, params.Margin, StrategyCrop); err != nil {
				return canceledOr(ctx, name, err.Error())
			}
		}
	} else {
		// original 策略（默认）：多框按「或」合并为单掩膜，整图单次前向
		mask := make([]byte, W*H)
		for _, box := range params.Boxes {
			MergeMasks(mask, BuildBoxMask(W, H, box, params.Relative, params.Dilate), W, H)
		}
		if err := InpaintImage(ctx, e, img, mask, params.Margin, StrategyOriginal); err != nil {
			return canceledOr(ctx, name, err.Error())
		}
	}

	// 取消检查点：落盘前（关键——即使推理已完成，取消后也不保存、不产出结果）
	if canceled(ctx) {
		return Result{Name: name, Status: "skip", Info: "已取消"}
	}
	dst := UniqueDst(filepath.Join(outDir, name))
	if _, err := SaveImage(img, dst); err != nil {
		return Result{Name: name, Status: "fail", Info: err.Error()}
	}
	return Result{Name: name, Status: "ok", Info: ""}
}

// canceled ctx 是否已取消。
func canceled(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil
}

// canceledOr 取消期间产生的失败统一记为 skip，避免取消批次出现误导性 fail
// （CancelBatch 会先取消 ctx 再 Kill 引擎，因此推理中断总是伴随 ctx 取消）。
func canceledOr(ctx context.Context, name, errInfo string) Result {
	if canceled(ctx) {
		return Result{Name: name, Status: "skip", Info: "已取消"}
	}
	return Result{Name: name, Status: "fail", Info: errInfo}
}

// ------------------------------------------------------------- 图像工具

func cropNRGBA(img *image.NRGBA, x1, y1, x2, y2 int) *image.NRGBA {
	w, h := x2-x1, y2-y1
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		copy(out.Pix[y*out.Stride:(y+1)*out.Stride],
			img.Pix[(y1+y)*img.Stride+x1*4:])
	}
	return out
}

func cropMask(mask []byte, W, x1, y1, x2, y2 int) []byte {
	w := x2 - x1
	out := make([]byte, w*(y2-y1))
	for y := y1; y < y2; y++ {
		copy(out[(y-y1)*w:(y-y1+1)*w], mask[y*W+x1:y*W+x2])
	}
	return out
}

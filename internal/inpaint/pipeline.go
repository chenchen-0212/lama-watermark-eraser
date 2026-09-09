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
	// StrategyAuto 智能策略（默认）：逐图按合并掩膜 bbox 外扩 margin 后的
	// 面积占比自动选择 crop（占比小）或 original（占比大），见 autoDecision。
	StrategyAuto = "auto"
	// StrategyOriginal 整图推理：多框先 MergeMasks 合并为单掩膜，
	// 整图单次前向。与 IOPaint 整图行为一致（P0 红线：效果对齐 IOPaint）。
	StrategyOriginal = "original"
	// StrategyCrop 逐框裁剪推理：每个框单独裁剪（±margin）后前向，
	// 大图多水印场景速度更快，但上下文受限、效果略逊于整图。
	StrategyCrop = "crop"
	// autoCropRatio auto 决策阈值：合并掩膜 bbox 外扩 margin 后面积占整图
	// 面积比例不超过该值时走 crop（多次小前向更快），否则 original。
	autoCropRatio = 0.6
	// cropMarginFloor 裁剪路径上下文边距下限，对齐 IOPaint CROP 默认
	// crop_margin=256，保证裁剪模式上下文质量。
	cropMarginFloor = 256
)

// TaskParams 去水印任务参数（与 Python 版 CLI/GUI 参数对齐）。
type TaskParams struct {
	Boxes    [][4]float64 // 多个框选 x1,y1,x2,y2（像素或 0..1 比例）
	Relative bool
	Dilate   int
	Margin   int
	MaskPath string
	Strategy string // auto（默认，智能选择）| original（整图）| crop（逐框裁剪）
}

// normalizeStrategy 空值/未知策略回退为智能默认策略 auto。
func normalizeStrategy(s string) string {
	switch s {
	case StrategyOriginal:
		return StrategyOriginal
	case StrategyCrop:
		return StrategyCrop
	default:
		return StrategyAuto
	}
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
// mask 为 W*H 的 0/255 单通道掩膜；margin 仅在 crop 策略下作为裁剪上下文
// 边距（实际取 effectiveCropMargin，下限 cropMarginFloor）。
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
		m := effectiveCropMargin(margin)
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

	// 整图策略：全图单次前向，无缩放无裁剪，与 IOPaint 整图行为一致
	out, err := inferNRGBA(ctx, e, img, mask)
	if err != nil {
		return err
	}
	compositeMasked(img, out, mask)
	return nil
}

// effectiveCropMargin 裁剪路径的实际上下文边距：不低于 cropMarginFloor。
// 仅作用于 crop 路径（original 整图前向不使用 margin），不改变该参数其他语义。
func effectiveCropMargin(margin int) int {
	if margin < cropMarginFloor {
		return cropMarginFloor
	}
	return margin
}

// autoDecision 智能策略决策：合并掩膜 bbox 外扩 margin 后面积占整图面积
// 比例 ≤ autoCropRatio → crop（逐框小前向更快），否则 → original（整图单次
// 前向更稳）。决定性因素是掩膜占比而非图片尺寸：大掩膜时裁剪无收益且多次
// 前向更慢。返回 (策略, 占比 0..1)，占比用于日志展示。
func autoDecision(mask []byte, w, h, margin int) (string, float64) {
	bbox, ok := maskBBox(mask, w, h)
	if !ok || w <= 0 || h <= 0 {
		return StrategyOriginal, 0
	}
	x1 := clampInt(bbox.X1-margin, 0, w)
	y1 := clampInt(bbox.Y1-margin, 0, h)
	x2 := clampInt(bbox.X2+margin, 0, w)
	y2 := clampInt(bbox.Y2+margin, 0, h)
	ratio := float64((x2-x1)*(y2-y1)) / float64(w*h)
	if ratio <= autoCropRatio {
		return StrategyCrop, ratio
	}
	return StrategyOriginal, ratio
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
			d := fmt.Sprintf("%.1fs", time.Since(t0).Seconds())
			if res.Info != "" {
				res.Info = d + " " + res.Info // 保留 auto 决策说明
			} else {
				res.Info = d
			}
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

	note := "" // auto 决策说明（成功时并入结果 Info，形如 auto→crop (掩膜bbox占比 12%)）
	if params.MaskPath != "" {
		// 掩膜文件模式：单掩膜直接推理（策略照常生效）
		mask, err := MaskFromFile(params.MaskPath, W, H)
		if err != nil {
			return Result{Name: name, Status: "fail", Info: err.Error()}
		}
		strategy := normalizeStrategy(params.Strategy)
		if strategy == StrategyAuto {
			strategy, ratio := autoDecision(mask, W, H, effectiveCropMargin(params.Margin))
			note = fmt.Sprintf("auto→%s (掩膜bbox占比 %.0f%%)", strategy, ratio*100)
		}
		if err := InpaintImage(ctx, e, img, mask, params.Margin, strategy); err != nil {
			return canceledOr(ctx, name, err.Error())
		}
	} else {
		// 多框模式：先按「或」合并为单掩膜（auto 决策与 original 路径共用）
		merged := make([]byte, W*H)
		for _, box := range params.Boxes {
			MergeMasks(merged, BuildBoxMask(W, H, box, params.Relative, params.Dilate), W, H)
		}
		strategy := normalizeStrategy(params.Strategy)
		if strategy == StrategyAuto {
			strategy, ratio := autoDecision(merged, W, H, effectiveCropMargin(params.Margin))
			note = fmt.Sprintf("auto→%s (掩膜bbox占比 %.0f%%)", strategy, ratio*100)
		}
		if strategy == StrategyCrop {
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
			// original 策略：合并掩膜整图单次前向
			if err := InpaintImage(ctx, e, img, merged, params.Margin, StrategyOriginal); err != nil {
				return canceledOr(ctx, name, err.Error())
			}
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
	return Result{Name: name, Status: "ok", Info: note}
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

package inpaint

import (
	"context"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/disintegration/imaging"
)

// TaskParams 去水印任务参数（与 Python 版 CLI/GUI 参数对齐）。
type TaskParams struct {
	Boxes    [][4]float64 // 多个框选 x1,y1,x2,y2（像素或 0..1 比例）
	Relative bool
	Dilate   int
	Margin   int
	MaskPath string
}

// Result 单张处理结果。
type Result struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | fail | skip
	Info   string `json:"info"`
}

// ProgressFn 进度回调：i/total 为 1-based；status ∈ ok|fail|skip。
type ProgressFn func(i, total int, name, status, info string)

// InpaintImage 对 img（会被就地修改）按 mask 做局部修复并返回。
// 512 适配策略：裁剪≤512 时反射填充到 512x512 不缩放；>512 时等比缩放到 512 内切。
func InpaintImage(e *Engine, img *image.NRGBA, mask []byte, margin int) error {
	W := img.Bounds().Dx()
	H := img.Bounds().Dy()
	bbox, ok := maskBBox(mask, W, H)
	if !ok {
		return nil // 空掩膜，原样返回
	}
	cx1 := clampInt(bbox.X1-margin, 0, W)
	cy1 := clampInt(bbox.Y1-margin, 0, H)
	cx2 := clampInt(bbox.X2+margin, 0, W)
	cy2 := clampInt(bbox.Y2+margin, 0, H)
	cw, ch := cx2-cx1, cy2-cy1
	if cw <= 0 || ch <= 0 {
		return nil
	}

	if cw > ModelDim || ch > ModelDim {
		// >512：等比缩放到 512 内切，推理后缩回贴回
		s := math.Min(float64(ModelDim)/float64(ch), float64(ModelDim)/float64(cw))
		sw := clampInt(int(math.Round(float64(cw)*s)), 1, ModelDim)
		sh := clampInt(int(math.Round(float64(ch)*s)), 1, ModelDim)
		crop := cropNRGBA(img, cx1, cy1, cx2, cy2)
		rz := toNRGBA(imaging.Resize(crop, sw, sh, imaging.Linear))
		mcrop := cropMask(mask, W, cx1, cy1, cx2, cy2)
		mrz := resizeMaskNearest(mcrop, cw, ch, sw, sh)
		small := e.runCanvas(rz, mrz, sw, sh)
		back := toNRGBA(imaging.Resize(small, cw, ch, imaging.Linear))
		pasteInto(img, back, cx1, cy1)
		return nil
	}

	// ≤512：反射/边缘填充到 512，不缩放，保持原生分辨率质量
	crop := cropNRGBA(img, cx1, cy1, cx2, cy2)
	mcrop := cropMask(mask, W, cx1, cy1, cx2, cy2)
	out := e.runCanvas(crop, mcrop, cw, ch)
	pasteInto(img, out, cx1, cy1)
	return nil
}

// runCanvas 将 crop(含掩膜) 填入 512 张量画布（NCHW 布局），推理后取回有效区域。
// 返回 cw x ch 的修复结果。
func (e *Engine) runCanvas(crop *image.NRGBA, mcrop []byte, cw, ch int) *image.NRGBA {
	stride4 := crop.Stride
	plane := ModelDim * ModelDim
	for y := 0; y < ModelDim; y++ {
		inY := y < ch
		sy := y
		if !inY {
			sy = reflectIdx(y, ch)
		}
		srcRow := crop.Pix[sy*stride4 : sy*stride4+cw*4]
		mRow := e.maskData[y*ModelDim : (y+1)*ModelDim]
		rowBase := y * ModelDim
		for x := 0; x < ModelDim; x++ {
			inX := x < cw
			sx := x
			if !inX {
				sx = reflectIdx(x, cw)
			}
			p := srcRow[sx*4 : sx*4+3]
			e.imgData[rowBase+x] = float32(p[0]) / 255.0
			e.imgData[plane+rowBase+x] = float32(p[1]) / 255.0
			e.imgData[2*plane+rowBase+x] = float32(p[2]) / 255.0
			// 图像画布：反射填充；掩膜画布：仅真实裁剪区有效，填充区恒 0
			if inY && inX {
				if mcrop[sy*cw+sx] > 0 {
					mRow[x] = 1
				} else {
					mRow[x] = 0
				}
			} else {
				mRow[x] = 0
			}
		}
	}
	if err := e.Run(); err != nil {
		// 推理失败时返回原裁剪内容，保证贴回后图像完整
		out := image.NewNRGBA(image.Rect(0, 0, cw, ch))
		copyNRGBARows(out, crop)
		return out
	}
	out := image.NewNRGBA(image.Rect(0, 0, cw, ch))
	f := e.outFactor
	for y := 0; y < ch; y++ {
		drow := out.Pix[y*out.Stride : y*out.Stride+cw*4]
		rowBase := y * ModelDim
		for x := 0; x < cw; x++ {
			drow[x*4+0] = clampByte(e.outData[rowBase+x] * f)
			drow[x*4+1] = clampByte(e.outData[plane+rowBase+x] * f)
			drow[x*4+2] = clampByte(e.outData[2*plane+rowBase+x] * f)
			drow[x*4+3] = 255
		}
	}
	return out
}

// ProcessBatch 批量去水印。单张失败不中断；ctx 取消后剩余标记为 skip。
func ProcessBatch(e *Engine, files []string, outDir string, params TaskParams,
	cb ProgressFn, ctx context.Context) []Result {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return []Result{}
	}
	results := make([]Result, 0, len(files))
	total := len(files)
	for i, p := range files {
		name := filepath.Base(p)
		if ctx != nil && ctx.Err() != nil {
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

func processOne(e *Engine, path, outDir string, params TaskParams, ctx context.Context) (res Result) {
	name := filepath.Base(path)
	defer func() {
		if r := recover(); r != nil {
			res = Result{Name: name, Status: "fail", Info: fmt.Sprint(r)}
		}
	}()
	// 取消检查点：开始处理前
	if ctx != nil && ctx.Err() != nil {
		return Result{Name: name, Status: "skip", Info: "已取消"}
	}
	img, err := DecodeImage(path)
	if err != nil {
		return Result{Name: name, Status: "fail", Info: err.Error()}
	}
	W := img.Bounds().Dx()
	H := img.Bounds().Dy()
	// 多框选：逐框独立修复（各自裁剪，避免相距过远导致整图缩放降质）
	if params.MaskPath != "" {
		mask, err := MaskFromFile(params.MaskPath, W, H)
		if err != nil {
			return Result{Name: name, Status: "fail", Info: err.Error()}
		}
		if err := InpaintImage(e, img, mask, params.Margin); err != nil {
			return Result{Name: name, Status: "fail", Info: err.Error()}
		}
	} else {
		for _, box := range params.Boxes {
			// 取消检查点：每个框之间（多框时可提前中止后续框的推理）
			if ctx != nil && ctx.Err() != nil {
				return Result{Name: name, Status: "skip", Info: "已取消"}
			}
			mask := BuildBoxMask(W, H, box, params.Relative, params.Dilate)
			if err := InpaintImage(e, img, mask, params.Margin); err != nil {
				return Result{Name: name, Status: "fail", Info: err.Error()}
			}
		}
	}
	// 取消检查点：落盘前（关键——即使推理已完成，取消后也不保存、不产出结果）
	if ctx != nil && ctx.Err() != nil {
		return Result{Name: name, Status: "skip", Info: "已取消"}
	}
	dst := UniqueDst(filepath.Join(outDir, name))
	if _, err := SaveImage(img, dst); err != nil {
		return Result{Name: name, Status: "fail", Info: err.Error()}
	}
	return Result{Name: name, Status: "ok", Info: ""}
}

// ------------------------------------------------------------- 图像工具

func clampByte(v float32) byte {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return byte(v + 0.5)
}

// reflectIdx numpy pad mode="reflect" 的索引映射（不重复边缘）。
func reflectIdx(j, n int) int {
	if n <= 1 {
		return 0
	}
	m := 2*n - 2
	r := j % m
	if r < 0 {
		r += m
	}
	if r >= n {
		r = m - r
	}
	return r
}

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

func pasteInto(dst, src *image.NRGBA, ox, oy int) {
	w := src.Bounds().Dx()
	for y := 0; y < src.Bounds().Dy(); y++ {
		copy(dst.Pix[(oy+y)*dst.Stride+ox*4:], src.Pix[y*src.Stride:y*src.Stride+w*4])
	}
}

func copyNRGBARows(dst, src *image.NRGBA) {
	w := src.Bounds().Dx()
	for y := 0; y < src.Bounds().Dy() && y < dst.Bounds().Dy(); y++ {
		copy(dst.Pix[y*dst.Stride:y*dst.Stride+w*4], src.Pix[y*src.Stride:y*src.Stride+w*4])
	}
}

func resizeMaskNearest(src []byte, sw, sh, dw, dh int) []byte {
	out := make([]byte, dw*dh)
	for y := 0; y < dh; y++ {
		sy := y * sh / dh
		row := src[sy*sw : sy*sw+sw]
		for x := 0; x < dw; x++ {
			out[y*dw+x] = row[x*sw/dw]
		}
	}
	return out
}

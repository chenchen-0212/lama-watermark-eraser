package inpaint

import (
	"context"
	"image"
	"os"
	"testing"
	"time"
)

// TestPythonEngineIntegration 端到端验证 Go 客户端与 worker.py 的 JSONL 往返。
// 需设置环境变量（未设置时跳过）：
//
//	INPAINT_ENGINE_PYTHON  例如 C:\Users\...\python.exe
//	INPAINT_ENGINE_WORKER  worker.py 路径
//	INPAINT_ENGINE_MODEL   big-lama.pt 路径
func TestPythonEngineIntegration(t *testing.T) {
	py := os.Getenv("INPAINT_ENGINE_PYTHON")
	worker := os.Getenv("INPAINT_ENGINE_WORKER")
	model := os.Getenv("INPAINT_ENGINE_MODEL")
	if py == "" || worker == "" || model == "" {
		t.Skip("未设置 INPAINT_ENGINE_PYTHON/WORKER/MODEL，跳过集成测试")
	}

	e := NewPythonEngine(py)
	e.Args = []string{worker, "--model", model}

	ctx, cancel := context.WithTimeout(context.Background(), warmupTimeout+30*time.Second)
	defer cancel()

	if err := e.Start(ctx); err != nil {
		t.Fatalf("Start 失败: %v\nstderr:\n%s", err, joinLines(e.LastStderr()))
	}
	defer e.Close()
	if !e.IsReady() {
		t.Fatal("Start 成功后 IsReady 应为 true")
	}

	const w, h = 64, 64
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		img.Pix[i*4] = byte(i)
		img.Pix[i*4+1] = byte(i >> 8)
		img.Pix[i*4+2] = byte(i >> 16)
		img.Pix[i*4+3] = 255
	}

	// RGB <-> NRGBA 往返一致
	rgb, rw, rh := NRGBAToRGB(img)
	if rw != w || rh != h {
		t.Fatalf("NRGBAToRGB 尺寸错误: %dx%d", rw, rh)
	}
	back, _, _ := NRGBAToRGB(RGBToNRGBA(rgb, w, h))
	if string(back) != string(rgb) {
		t.Fatal("RGB <-> NRGBA 往返不一致")
	}

	mask := make([]byte, w*h)
	for y := 16; y < 48; y++ {
		for x := 16; x < 48; x++ {
			mask[y*w+x] = 255
		}
	}

	out, err := e.Infer(ctx, rgb, mask, w, h)
	if err != nil {
		t.Fatalf("Infer 失败: %v\nstderr:\n%s", err, joinLines(e.LastStderr()))
	}
	if len(out) != w*h*3 {
		t.Fatalf("输出字节数不符: 期望 %d 实得 %d", w*h*3, len(out))
	}

	// 掩膜区输出应与输入不同（模型产生修复结果）
	changed := false
	for y := w / 4; y < 3*w/4 && !changed; y++ {
		for x := h / 4; x < 3*h/4; x++ {
			o := (y*w + x) * 3
			if out[o] != rgb[o] || out[o+1] != rgb[o+1] || out[o+2] != rgb[o+2] {
				changed = true
				break
			}
		}
	}
	if !changed {
		t.Log("警告: 掩膜区输出未变化（可能模型返回恒等，需人工确认）")
	}
}

// TestNRGBARGBRoundTripNonAlignedStride 验证 stride 非 4 对齐（子图）情形下的往返一致。
func TestNRGBARGBRoundTripNonAlignedStride(t *testing.T) {
	// 父图宽 100 -> Stride=400；子图宽 30 -> Stride=400 != 30*4，覆盖非 4 对齐情形。
	parent := image.NewNRGBA(image.Rect(0, 0, 100, 50))
	for i := range parent.Pix {
		parent.Pix[i] = byte(i)
	}
	sub := parent.SubImage(image.Rect(5, 5, 35, 45)).(*image.NRGBA)

	rgb, w, h := NRGBAToRGB(sub)
	if w != 30 || h != 40 {
		t.Fatalf("子图尺寸错误: %dx%d（期望 30x40）", w, h)
	}
	// 子图像素应与父图对应位置一致
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 3
			pr := parent.Pix[(y+5)*parent.Stride+(x+5)*4 : (y+5)*parent.Stride+(x+5)*4+3]
			if rgb[o] != pr[0] || rgb[o+1] != pr[1] || rgb[o+2] != pr[2] {
				t.Fatalf("子图像素不一致 @(%d,%d)", x, y)
			}
		}
	}
	// RGB <-> NRGBA 往返一致
	back, _, _ := NRGBAToRGB(RGBToNRGBA(rgb, w, h))
	if string(back) != string(rgb) {
		t.Fatal("非 4 对齐 stride 下 RGB <-> NRGBA 往返不一致")
	}
}

func joinLines(lines []string) string {
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

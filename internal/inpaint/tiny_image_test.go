package inpaint

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestPythonEngineTinyImageError 验证 ≤8px 极小图返回可读的单行 INVALID_IMAGE
// 错误（完整 TorchScript 栈仅留在 LastStderr，不进入协议消息）；
// 且单张失败后引擎不退出，可继续正常推理。环境变量同 TestPythonEngineIntegration。
func TestPythonEngineTinyImageError(t *testing.T) {
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

	// 6x6 极小图：应返回 INVALID_IMAGE + 可读单行消息
	const w, h = 6, 6
	rgb := make([]byte, w*h*3)
	mask := make([]byte, w*h)
	_, err := e.Infer(ctx, rgb, mask, w, h)
	if err == nil {
		t.Fatal("6x6 极小图推理应失败，实际成功")
	}
	if code := EngineErrorCode(err); code != "INVALID_IMAGE" {
		t.Fatalf("错误码应为 INVALID_IMAGE，实得 %q (%v)", code, err)
	}
	if !strings.Contains(err.Error(), "尺寸过小") {
		t.Fatalf("错误信息应包含「尺寸过小」，实得: %v", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("协议错误信息应为单行，实得: %q", err.Error())
	}
	if strings.Contains(err.Error(), "Traceback") || strings.Contains(err.Error(), "RuntimeError") {
		t.Fatalf("协议错误信息不应包含调用栈内容: %q", err.Error())
	}

	// 完整调用栈应留在 stderr 环形缓冲（LastStderr）
	stackFound := false
	for _, l := range e.LastStderr() {
		if strings.Contains(l, "Traceback") || strings.Contains(l, "尺寸过小") {
			stackFound = true
			break
		}
	}
	if !stackFound {
		t.Fatal("LastStderr 中应包含完整异常记录")
	}

	// 引擎应仍可用：单张异常不退出进程，紧接一次正常尺寸推理应成功
	const W, H = 16, 16
	rgb2 := make([]byte, W*H*3)
	mask2 := make([]byte, W*H)
	out, err := e.Infer(ctx, rgb2, mask2, W, H)
	if err != nil {
		t.Fatalf("极小图失败后引擎应可继续推理: %v\nstderr:\n%s", err, joinLines(e.LastStderr()))
	}
	if len(out) != W*H*3 {
		t.Fatalf("恢复推理输出字节数不符: 期望 %d 实得 %d", W*H*3, len(out))
	}
}

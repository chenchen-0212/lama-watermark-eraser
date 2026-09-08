package inpaint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lama-watermark-eraser/resources"
)

// 开发期引擎覆盖环境变量（生产环境使用伴生 lamacore/ 目录，无需设置）。
const (
	// EnvEnginePython 开发期指定 python.exe 绝对路径（需已安装 torch/numpy），
	// 与 EnvEngineWorker 配合直接运行 worker.py。
	EnvEnginePython = "LAMA_ENGINE_PYTHON"
	// EnvEngineWorker 开发期指定 worker.py 路径（缺省尝试 <cwd>/python_engine/worker.py）。
	EnvEngineWorker = "LAMA_ENGINE_WORKER"
	// EnvEngineModel 开发期指定 big-lama.pt 路径（缺省由 worker.py 自动定位）。
	EnvEngineModel = "LAMA_ENGINE_MODEL"
)

// NewEngine 构造去水印引擎客户端（不启动；调用方需 Start 完成预热）。
//
// 生产环境：定位主程序同级的伴生引擎 lamacore/lamacore.exe（PyInstaller 打包
// 的 Python worker，T05 产出）；缺失时返回可读错误。
//
// 开发环境：lamacore 尚未打包时，可经环境变量驱动本机 Python：
//
//	LAMA_ENGINE_PYTHON  python.exe 绝对路径（必需）
//	LAMA_ENGINE_WORKER  worker.py 路径（可选，默认 <cwd>/python_engine/worker.py）
//	LAMA_ENGINE_MODEL   big-lama.pt 路径（可选，经 --model 传给 worker）
func NewEngine() (*PythonEngine, error) {
	if py := strings.TrimSpace(os.Getenv(EnvEnginePython)); py != "" {
		worker := strings.TrimSpace(os.Getenv(EnvEngineWorker))
		if worker == "" {
			worker = filepath.Join("python_engine", "worker.py")
		}
		if _, err := os.Stat(worker); err != nil {
			return nil, fmt.Errorf(
				"开发期引擎配置不完整: 找不到 worker 脚本 %q（请设置 %s 指向 python_engine/worker.py）",
				worker, EnvEngineWorker)
		}
		e := NewPythonEngine(py)
		e.Args = append(e.Args, worker)
		if model := strings.TrimSpace(os.Getenv(EnvEngineModel)); model != "" {
			e.Args = append(e.Args, "--model", model)
		}
		return e, nil
	}
	exe, err := resources.LocatePythonEngine()
	if err != nil {
		return nil, err
	}
	return NewPythonEngine(exe), nil
}

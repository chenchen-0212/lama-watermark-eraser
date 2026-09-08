// Package resources 内嵌 onnxruntime 运行时与 LaMa 模型，首次运行解压到本地缓存目录。
package resources

import (
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

//go:embed onnxruntime.dll
//go:embed lama_fp32.onnx
//go:embed version.txt
var files embed.FS

// Version 返回内嵌资源版本标识。
func Version() string {
	b, _ := files.ReadFile("version.txt")
	return strings.TrimSpace(string(b))
}

// EnsureAssets 确保资源已解压到 %LOCALAPPDATA%\LaMaWatermarkRemover\<version>\。
// 版本标记命中且文件有效时直接复用。返回 (dllPath, modelPath, err)。
func EnsureAssets() (dllPath, modelPath string, err error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base, err = os.UserCacheDir()
		if err != nil {
			return "", "", err
		}
	}
	dir := filepath.Join(base, "LaMaWatermarkRemover", Version())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	dllPath = filepath.Join(dir, "onnxruntime.dll")
	modelPath = filepath.Join(dir, "lama_fp32.onnx")
	marker := filepath.Join(dir, "version.txt")
	if fileOK(dllPath) && fileOK(modelPath) {
		if b, e := os.ReadFile(marker); e == nil && string(b) == Version() {
			return dllPath, modelPath, nil
		}
	}
	pairs := []struct{ name, dst string }{
		{"onnxruntime.dll", dllPath},
		{"lama_fp32.onnx", modelPath},
	}
	for _, p := range pairs {
		if err := extract(p.name, p.dst); err != nil {
			return "", "", fmt.Errorf("解压资源 %s 失败: %w", p.name, err)
		}
	}
	if err := os.WriteFile(marker, []byte(Version()), 0o644); err != nil {
		return "", "", err
	}
	return dllPath, modelPath, nil
}

func fileOK(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Size() > 0
}

func extract(name, dst string) error {
	src, err := files.Open(name)
	if err != nil {
		return fmt.Errorf("内嵌资源缺失: %w", err)
	}
	defer src.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Package resources 提供引擎可执行文件定位与资源版本标识。
package resources

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed version.txt
var files embed.FS

// Version 返回内嵌资源版本标识。
func Version() string {
	b, _ := files.ReadFile("version.txt")
	return strings.TrimSpace(string(b))
}

// LocatePythonEngine 定位伴生引擎可执行文件 lamacore/lamacore.exe。
// 优先 os.Executable() 同级目录；缺失时返回可读错误。
func LocatePythonEngine() (string, error) {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "lamacore", "lamacore.exe"),
			filepath.Join(dir, "lamacore.exe"),
		)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "lamacore", "lamacore.exe"),
			filepath.Join(cwd, "lamacore.exe"),
		)
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"未找到 AI 引擎可执行文件（应位于主程序同级的 lamacore/lamacore.exe）。已尝试: %s",
		strings.Join(candidates, "; "),
	)
}

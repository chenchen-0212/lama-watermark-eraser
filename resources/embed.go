// Package resources 提供引擎可执行文件定位与资源版本标识。
package resources

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed version.txt
var files embed.FS

// Version 返回内嵌资源版本标识。
func Version() string {
	b, _ := files.ReadFile("version.txt")
	return strings.TrimSpace(string(b))
}

// EngineBinaryName 返回当前平台的伴生引擎可执行文件名。
// Windows 为 lamacore.exe；macOS / Linux 为 lamacore。
func EngineBinaryName() string {
	if runtime.GOOS == "windows" {
		return "lamacore.exe"
	}
	return "lamacore"
}

// engineSearchDirs 返回引擎查找目录（按优先级）：
//   - 主程序同级目录
//   - macOS .app 包内的 Contents/Resources（主程序位于 Contents/MacOS）
//   - 当前工作目录
func engineSearchDirs() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		dirs = append(dirs, dir, filepath.Join(dir, "..", "Resources"))
	}
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, cwd)
	}
	return dirs
}

// isEngineFile 判断路径是否为可用的引擎可执行文件。
// 非 Windows 平台额外校验执行位，避免定位到同名普通文件。
func isEngineFile(p string) bool {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode()&0o111 != 0
}

// LocatePythonEngine 定位伴生引擎可执行文件 lamacore/lamacore(.exe)。
// 布局约定：<目录>/lamacore/<引擎名> 优先，其次 <目录>/<引擎名>。
// macOS 打包为 .app 时把 lamacore/ 整目录放入 Contents/Resources/ 即可被找到。
func LocatePythonEngine() (string, error) {
	name := EngineBinaryName()
	var candidates []string
	for _, dir := range engineSearchDirs() {
		candidates = append(candidates,
			filepath.Join(dir, "lamacore", name),
			filepath.Join(dir, name),
		)
	}
	for _, p := range candidates {
		if isEngineFile(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"未找到 AI 引擎可执行文件（应位于主程序同级的 lamacore/%s）。已尝试: %s",
		name, strings.Join(candidates, "; "),
	)
}

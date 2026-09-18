package main

import (
	"embed"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"lama-watermark-eraser/internal/cli"
)

//go:embed all:frontend/dist
var assets embed.FS

// appVersion 由构建时注入：wails build 的 -ldflags "-X main.appVersion=x.y.z"。
// 打包脚本从 wails.json 的 productVersion 读取后传入，保证界面展示的版本号
// 与安装包版本、exe 版本资源同源。未注入（如 go run 调试）时为空，
// GetAppVersion 会回退为 "dev"。
var appVersion string

func main() {
	// CLI 模式分流（带 --input 或 --url 时跳过 GUI）
	if len(os.Args) > 1 {
		fs := flag.NewFlagSet("args", flag.ContinueOnError)
		fs.SetOutput(new(strings.Builder))
		fs.Bool("h", false, "")
		fs.Bool("help", false, "")
		_ = fs.Parse(os.Args[1:])
		if hasFlag(os.Args, "--input") || hasFlag(os.Args, "--url") {
			cli.Run(os.Args[1:])
			return
		}
	}

	// 引擎资源说明：ONNX 内嵌资源已移除，改为伴生 lamacore/lamacore(.exe)
	// （Python big-lama 子进程，见 resources.LocatePythonEngine）。
	// 引擎定位与预热在 app.startup 中后台完成，状态经 engine:status 事件推送前端。

	app := NewApp()

	opts := &options.App{
		Title:     "社媒图文去水印工作台",
		Width:     1080,
		Height:    760,
		MinWidth:  900,
		MinHeight: 640,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 245, G: 245, B: 247, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
	}

	// 平台专属选项：Windows 显式指定 WebView2 数据目录（避免 APPDATA 受限时
	// 创建失败）；macOS 提供「关于」信息。二者互不影响，跨平台编译均合法。
	if runtime.GOOS == "windows" {
		opts.Windows = &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			DisableWindowIcon:    false,
			WebviewUserDataPath:  webviewDataDir(),
		}
	} else {
		opts.Mac = &mac.Options{
			About: &mac.AboutInfo{
				Title:   "社媒图文水印抹除工具",
				Message: "Go + Wails + Vue3 + big-lama 去水印流水线",
			},
		}
	}

	if err := wails.Run(opts); err != nil {
		println("Error:", err.Error())
	}
}

// webviewDataDir 返回 WebView2 用户数据目录（仅 Windows 使用；显式指定可避免
// APPDATA 缺失/受限环境下的创建失败）。
func webviewDataDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = "."
	}
	dir := filepath.Join(base, "LaMaWatermarkRemover", "webview2")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

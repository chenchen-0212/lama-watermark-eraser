package main

import (
	"embed"
	"flag"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"lama-watermark-eraser/internal/cli"
)

//go:embed all:frontend/dist
var assets embed.FS

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

	// 引擎资源说明：ONNX 内嵌资源已移除，改为伴生 lamacore/lamacore.exe
	// （Python big-lama 子进程，见 resources.LocatePythonEngine）。
	// 引擎定位与预热在 app.startup 中后台完成，状态经 engine:status 事件推送前端。

	// WebView2 用户数据目录显式指定，避免 APPDATA 缺失/受限环境下的创建失败
	wvDataDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "LaMaWatermarkRemover", "webview2")
	if os.Getenv("LOCALAPPDATA") == "" {
		wvDataDir = filepath.Join(".", "webview2-data")
	}
	_ = os.MkdirAll(wvDataDir, 0o755)

	app := NewApp()

	err := wails.Run(&options.App{
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
		Bind: []interface{}{
			app,
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			DisableWindowIcon:    false,
			WebviewUserDataPath:  wvDataDir,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

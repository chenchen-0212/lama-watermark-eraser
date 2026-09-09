// Package cli 命令行模式：社媒图文下载 + 批量去水印（与 GUI 共用引擎）。
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lama-watermark-eraser/internal/downloader"
	"lama-watermark-eraser/internal/inpaint"
	"lama-watermark-eraser/internal/ziputil"
)

// Run 执行 CLI 并决定进程退出码（全成功 0，有失败 2，参数/资源错误 1 或 2）。
func Run(args []string) {
	fs := flag.NewFlagSet("LaMaWatermarkRemover", flag.ExitOnError)
	urlFlag := fs.String("url", "", "社媒图文链接（先下载图片再可继续去水印）")
	input := fs.String("input", "", "输入图片文件夹")
	output := fs.String("output", "", "输出文件夹")
	box := fs.String("box", "", "水印区域绝对像素坐标 x1,y1,x2,y2")
	relBox := fs.String("relative-box", "", "水印区域比例坐标 0..1: x1,y1,x2,y2")
	boxes := fs.String("boxes", "", "多个水印区域（绝对坐标），用 | 分隔: x1,y1,x2,y2|x1,y1,x2,y2")
	relBoxes := fs.String("relative-boxes", "", "多个水印区域（比例坐标 0..1），用 | 分隔")
	mask := fs.String("mask", "", "掩膜 PNG（白色=水印区域）")
	dilate := fs.Int("dilate", 12, "掩膜边缘外扩像素")
	margin := fs.Int("margin", 64, "修复上下文边距")
	fast := fs.Bool("fast", false, "快速模式：逐框裁剪推理（等价 --strategy crop）")
	strategy := fs.String("strategy", "auto", "处理策略: auto(智能)|original(整图)|crop(快速裁剪)")
	recursive := fs.Bool("recursive", false, "递归子文件夹")
	zipOut := fs.Bool("zip", false, "完成后打包 zip（输出目录旁生成 .zip）")
	_ = fs.String("device", "", "兼容参数（当前仅 CPU）")
	_ = fs.String("model", "", "兼容参数（模型路径由伴生引擎决定）")
	_ = fs.Parse(args)

	// --mask 与框选参数描述同一水印区域，同时给出时语义冲突，直接报错退出
	if merr := checkRegionMutex(*mask, *box, *relBox, *boxes, *relBoxes); merr != nil {
		fmt.Fprintln(os.Stderr, merr)
		os.Exit(2)
	}

	ctx := context.Background()

	var inputDir string
	if *urlFlag != "" {
		dlDir := *output
		if dlDir == "" {
			dlDir = "."
		}
		dlDir = filepath.Join(dlDir, "_downloaded")
		fmt.Println("下载社媒内容:", *urlFlag)
		post, derr := downloader.Download(ctx, *urlFlag, dlDir)
		if derr != nil {
			fmt.Fprintln(os.Stderr, "下载失败:", derr)
			os.Exit(2)
		}
		fmt.Printf("下载完成: %s | %s | %d 张图片\n输出目录: %s\n",
			post.Platform, post.Title, post.Count, post.Dir)
		if *input == "" && *output == "" {
			return // 仅下载模式
		}
		inputDir = post.Dir
	}
	if inputDir == "" {
		inputDir = *input
	}
	if inputDir == "" {
		fmt.Fprintln(os.Stderr, "需要 --url 或 --input 提供图片来源")
		os.Exit(2)
	}
	if *output == "" {
		fmt.Fprintln(os.Stderr, "需要 --output 指定输出目录")
		os.Exit(2)
	}

	// 参数解析（与 Python 版一致的三选一）
	var params inpaint.TaskParams
	params.Dilate = *dilate
	params.Margin = *margin
	params.MaskPath = *mask
	// --fast 为 --strategy crop 的兼容别名：两者同时显式给出视为参数冲突
	strategySet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "strategy" {
			strategySet = true
		}
	})
	if *fast && strategySet {
		fmt.Fprintln(os.Stderr, "--fast 与 --strategy 不可同时使用（--fast 等价于 --strategy crop）")
		os.Exit(2)
	}
	switch {
	case *fast:
		params.Strategy = inpaint.StrategyCrop
	case strategySet:
		switch *strategy {
		case inpaint.StrategyAuto, inpaint.StrategyOriginal, inpaint.StrategyCrop:
			params.Strategy = *strategy
		default:
			fmt.Fprintf(os.Stderr, "无效 --strategy: %s（可选 auto|original|crop）\n", *strategy)
			os.Exit(2)
		}
	default:
		params.Strategy = inpaint.StrategyAuto
	}
	var err error
	boxSpec := 0
	switch {
	case *box != "":
		b, e := parseBox(*box, false)
		if e != nil {
			err = e
			break
		}
		params.Boxes = [][4]float64{b}
		boxSpec = 1
	case *relBox != "":
		b, e := parseBox(*relBox, true)
		if e != nil {
			err = e
			break
		}
		params.Relative = true
		params.Boxes = [][4]float64{b}
		boxSpec = 2
	case *boxes != "":
		params.Boxes, err = parseBoxes(*boxes, false)
		boxSpec = 1
	case *relBoxes != "":
		params.Boxes, err = parseBoxes(*relBoxes, true)
		params.Relative = true
		boxSpec = 2
	case *mask != "":
		if _, err = os.Stat(*mask); err != nil {
			fmt.Fprintln(os.Stderr, "掩膜文件不存在:", *mask)
			os.Exit(2)
		}
		boxSpec = 3
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if boxSpec == 0 {
		fmt.Fprintln(os.Stderr, "需要 --box / --relative-box / --boxes / --relative-boxes / --mask 指定水印区域")
		os.Exit(2)
	}

	files := inpaint.CollectImages(inputDir, *output, *recursive)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "输入目录中没有可处理的图片")
		os.Exit(2)
	}

	fmt.Printf("待处理: %d 张 | 启动引擎…\n", len(files))
	engine, err := inpaint.NewEngine()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer engine.Close()
	if err := engine.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "引擎启动失败:", err)
		if lines := engine.LastStderr(); len(lines) > 0 {
			if len(lines) > 10 {
				lines = lines[len(lines)-10:]
			}
			fmt.Fprintln(os.Stderr, "引擎日志（最近）:")
			for _, l := range lines {
				fmt.Fprintln(os.Stderr, "  "+l)
			}
		}
		os.Exit(1)
	}

	t0 := time.Now()
	results := inpaint.ProcessBatch(engine, files, *output, params,
		func(i, total int, name, status, info string) {
			mark := "√"
			if status != "ok" {
				mark = "×"
			}
			fmt.Printf("%s [%d/%d] %s %s\n", mark, i, total, name, info)
		}, ctx)
	ok := 0
	for _, r := range results {
		if r.Status == "ok" {
			ok++
		}
	}
	fmt.Printf("=== 完成: 成功 %d / 共 %d | 总耗时 %.1fs | 输出目录: %s ===\n",
		ok, len(results), time.Since(t0).Seconds(), *output)

	if *zipOut {
		z := strings.TrimSuffix(*output, string(filepath.Separator)) + ".zip"
		if err := ziputil.ZipDir(*output, z); err != nil {
			fmt.Fprintln(os.Stderr, "打包失败:", err)
			os.Exit(2)
		}
		fmt.Println("已打包:", z)
	}
	if ok != len(results) {
		os.Exit(2)
	}
}

// checkRegionMutex 校验 --mask 与框选参数不可同时使用。
// 掩膜与坐标框描述的是同一个水印区域，同时给出时无法判定以谁为准，
// 返回错误并指明检测到的冲突参数，提示调用方只保留一种区域描述方式。
func checkRegionMutex(mask, box, relBox, boxes, relBoxes string) error {
	if mask == "" {
		return nil
	}
	pairs := []struct {
		name  string
		value string
	}{
		{"--box", box},
		{"--relative-box", relBox},
		{"--boxes", boxes},
		{"--relative-boxes", relBoxes},
	}
	for _, p := range pairs {
		if p.value != "" {
			return fmt.Errorf("--mask 与 --box/--boxes 不可同时使用（同时检测到 %s）", p.name)
		}
	}
	return nil
}

func parseBox(s string, relative bool) ([4]float64, error) {
	var v [4]float64
	var x1, y1, x2, y2 float64
	n, err := fmt.Sscanf(strings.ReplaceAll(s, "，", ","), "%f,%f,%f,%f", &x1, &y1, &x2, &y2)
	if err != nil || n != 4 {
		return v, fmt.Errorf("坐标格式错误: %s (应为 x1,y1,x2,y2)", s)
	}
	if x2 <= x1 || y2 <= y1 {
		return v, fmt.Errorf("坐标无效: x2>x1、y2>y1 必须成立")
	}
	if relative {
		for _, val := range []float64{x1, y1, x2, y2} {
			if val < 0 || val > 1 {
				return v, fmt.Errorf("比例坐标必须在 0..1 之间")
			}
		}
	}
	return [4]float64{x1, y1, x2, y2}, nil
}

// parseBoxes 解析用 | 分隔的多个框选坐标。
func parseBoxes(s string, relative bool) ([][4]float64, error) {
	parts := strings.Split(s, "|")
	out := make([][4]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		b, err := parseBox(p, relative)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("未解析到有效框选坐标")
	}
	return out, nil
}

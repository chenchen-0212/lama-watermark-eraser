package main

import (
	"archive/zip"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// writeTestPNG 写一张 8x8 纯色 PNG 作为测试夹具。
func writeTestPNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 %s: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("编码 PNG %s: %v", path, err)
	}
}

// TestExportSourceZip 源图打包绑定：3 张顶层图入包；子目录干扰项验证
// ziputil.ZipDir 既有递归语义（嵌套文件以相对路径 sub/d.png 入包）；
// zip 命名沿用「源图_<时间戳>.zip」规则（时间戳防重复导出重名）。
func TestExportSourceZip(t *testing.T) {
	src := t.TempDir()
	writeTestPNG(t, filepath.Join(src, "a.png"))
	writeTestPNG(t, filepath.Join(src, "b.png"))
	writeTestPNG(t, filepath.Join(src, "c.png"))
	sub := filepath.Join(src, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestPNG(t, filepath.Join(sub, "d.png"))

	a := &App{} // ExportSourceZip 不依赖 Wails ctx，零值可直接调用
	zipPath, err := a.ExportSourceZip(src)
	if err != nil {
		t.Fatalf("ExportSourceZip: %v", err)
	}
	defer os.Remove(zipPath)

	base := filepath.Base(zipPath)
	if prefix := "源图_"; len(base) < len(prefix) || base[:len(prefix)] != prefix {
		t.Errorf("zip 命名前缀错误: %s（期望前缀 %s）", base, prefix)
	}

	f, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("打开 zip: %v", err)
	}
	defer f.Close()
	got := map[string]bool{}
	for _, zf := range f.File {
		got[zf.Name] = true
	}
	for _, want := range []string{"a.png", "b.png", "c.png", "sub/d.png"} {
		if !got[want] {
			t.Errorf("zip 缺少 %s；实际清单 %v", want, mapKeys(got))
		}
	}
	if len(got) != 4 {
		t.Errorf("zip 条目数错误: %d（期望 4）", len(got))
	}
}

// TestExportSourceZipEmptyDir 空目录/不存在目录应报错而非生成空包。
func TestExportSourceZipEmptyDir(t *testing.T) {
	a := &App{}
	if _, err := a.ExportSourceZip(""); err == nil {
		t.Error("空目录参数应返回错误")
	}
	if _, err := a.ExportSourceZip(filepath.Join(t.TempDir(), "not_exist")); err == nil {
		t.Error("不存在的目录应返回错误")
	}
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Package inpaint 提供基于 LaMa(ONNX) 的批量去水印推理引擎。
// 等价移植自 lama/watermark_tool/inpaint_core.py（Python 版）。
package inpaint

import (
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/disintegration/imaging"
	xbmp "golang.org/x/image/bmp"
	xtiff "golang.org/x/image/tiff"
	xwebp "golang.org/x/image/webp"
)

// SupportedExts 支持处理的图片扩展名。
var SupportedExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".bmp": true,
	".webp": true, ".tif": true, ".tiff": true,
}

// DecodeImage 解码图片并按 EXIF 摆正方向，统一转为 *image.NRGBA（RGB 语义）。
func DecodeImage(path string) (*image.NRGBA, error) {
	ext := strings.ToLower(filepath.Ext(path))
	var img image.Image
	var err error
	if ext == ".webp" {
		f, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		img, err = xwebp.Decode(f)
		if err != nil {
			return nil, fmt.Errorf("webp 解码失败: %w", err)
		}
	} else {
		// imaging.Open 自动应用 JPEG EXIF 方向
		img, err = imaging.Open(path, imaging.AutoOrientation(true))
		if err != nil {
			return nil, err
		}
	}
	return toNRGBA(img), nil
}

func toNRGBA(img image.Image) *image.NRGBA {
	if n, ok := img.(*image.NRGBA); ok && n.Stride == n.Bounds().Dx()*4 {
		return n
	}
	dst := image.NewNRGBA(image.Rect(0, 0, img.Bounds().Dx(), img.Bounds().Dy()))
	draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Src)
	return dst
}

// SaveImage 按扩展名保存；webp 无无依赖编码器，自动回退为 png。
// 返回实际写出的文件路径。
func SaveImage(img image.Image, path string) (string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".webp" {
		path = strings.TrimSuffix(path, ext) + ".png"
		ext = ".png"
	}
	var err error
	switch ext {
	case ".jpg", ".jpeg":
		err = encodeFile(path, func(f *os.File) error {
			return jpeg.Encode(f, img, &jpeg.Options{Quality: 95})
		})
	case ".png":
		err = encodeFile(path, func(f *os.File) error { return png.Encode(f, img) })
	case ".bmp":
		err = encodeFile(path, func(f *os.File) error { return xbmp.Encode(f, img) })
	case ".tif", ".tiff":
		err = encodeFile(path, func(f *os.File) error { return xtiff.Encode(f, img, nil) })
	default:
		err = encodeFile(path, func(f *os.File) error { return png.Encode(f, img) })
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

func encodeFile(path string, enc func(*os.File) error) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := enc(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// UniqueDst 目标文件已存在时自动加 (n) 序号，避免覆盖历史结果。
func UniqueDst(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	base := strings.TrimSuffix(path, filepath.Ext(path))
	ext := filepath.Ext(path)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s(%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}

// CollectImages 列出待处理图片；excludeDir 用于排除输出目录（含其子目录）。
func CollectImages(inputDir, excludeDir string, recursive bool) []string {
	var files []string
	exclAbs := ""
	if excludeDir != "" {
		if a, err := filepath.Abs(excludeDir); err == nil {
			exclAbs = a
		}
	}
	accept := func(p string) bool {
		if !SupportedExts[strings.ToLower(filepath.Ext(p))] {
			return false
		}
		if exclAbs != "" {
			// 仅排除输出目录"直属"文件；输出目录的子目录（如 _downloaded）仍可处理
			if a, err := filepath.Abs(p); err == nil && filepath.Dir(a) == exclAbs {
				return false
			}
		}
		return true
	}
	if recursive {
		_ = filepath.WalkDir(inputDir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if accept(p) {
				files = append(files, p)
			}
			return nil
		})
	} else {
		entries, err := os.ReadDir(inputDir)
		if err != nil {
			return nil
		}
		for _, d := range entries {
			if d.IsDir() {
				continue
			}
			p := filepath.Join(inputDir, d.Name())
			if accept(p) {
				files = append(files, p)
			}
		}
	}
	sort.Strings(files)
	return files
}

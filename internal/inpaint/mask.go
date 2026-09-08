package inpaint

import (
	"fmt"
	"image"
	"image/draw"
	"math"

	xdraw "golang.org/x/image/draw"
)

// Rect 整数矩形，半开区间 [X1,X2) x [Y1,Y2)。
type Rect struct{ X1, Y1, X2, Y2 int }

// BuildBoxMask 生成 0/255 掩膜。box 为 (x1,y1,x2,y2)；relative=true 时为 0..1 比例坐标。
// 矩形掩膜经方窗 MaxFilter 膨胀等价于矩形各边外扩 dilate 像素，直接扩展以保证与 Python 版一致。
func BuildBoxMask(w, h int, box [4]float64, relative bool, dilate int) []byte {
	var x1, y1, x2, y2 int
	if relative {
		x1 = int(math.Round(box[0] * float64(w)))
		x2 = int(math.Round(box[2] * float64(w)))
		y1 = int(math.Round(box[1] * float64(h)))
		y2 = int(math.Round(box[3] * float64(h)))
	} else {
		x1, y1, x2, y2 = int(box[0]), int(box[1]), int(box[2]), int(box[3])
	}
	x1 = clampInt(x1, 0, w-1)
	x2 = clampInt(x2, x1+1, w)
	y1 = clampInt(y1, 0, h-1)
	y2 = clampInt(y2, y1+1, h)
	if dilate > 0 {
		x1 = clampInt(x1-dilate, 0, w)
		x2 = clampInt(x2+dilate, x1+1, w)
		y1 = clampInt(y1-dilate, 0, h)
		y2 = clampInt(y2+dilate, y1+1, h)
	}
	mask := make([]byte, w*h)
	for y := y1; y < y2; y++ {
		row := mask[y*w : (y+1)*w]
		for x := x1; x < x2; x++ {
			row[x] = 255
		}
	}
	return mask
}

// MaskFromFile 从掩膜图片生成 0/255 掩膜（白色=水印区域），尺寸不符时最近邻缩放。
func MaskFromFile(path string, w, h int) ([]byte, error) {
	img, err := DecodeImage(path)
	if err != nil {
		return nil, fmt.Errorf("读取掩膜失败: %w", err)
	}
	if img.Bounds().Dx() != w || img.Bounds().Dy() != h {
		dst := image.NewNRGBA(image.Rect(0, 0, w, h))
		xdraw.NearestNeighbor.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
		img = dst
	}
	mask := make([]byte, w*h)
	pix := img.Pix
	for i := 0; i < w*h; i++ {
		lum := int(0.299*float64(pix[i*4]) + 0.587*float64(pix[i*4+1]) + 0.114*float64(pix[i*4+2]))
		if lum > 127 {
			mask[i] = 255
		}
	}
	return mask, nil
}

// MergeMasks 将 src 掩膜按「或」合并进 dst（任一非零即为 255）。
// 用于多框选 original 策略：逐框生成掩膜后合并为单张掩膜，整图单次前向。
// 长度不符时静默返回（调用方均为内部固定 W*H 数据流，不应触发）。
func MergeMasks(dst, src []byte, w, h int) {
	if len(dst) != w*h || len(src) != w*h {
		return
	}
	for i, v := range src {
		if v > 0 {
			dst[i] = 255
		}
	}
}

// DilateRegion 对掩膜中包围盒 (bx1,by1,bx2,by2) 区域做 MaxFilter(2d+1) 膨胀，
// 等价于 PIL ImageFilter.MaxFilter，仅处理框附近子区域以保证大图性能。
func DilateRegion(mask []byte, w, h int, bx1, by1, bx2, by2, d int) {
	ex1, ey1 := clampInt(bx1-d, 0, w), clampInt(by1-d, 0, h)
	ex2, ey2 := clampInt(bx2+d, 0, w), clampInt(by2+d, 0, h)
	sw, sh := ex2-ex1, ey2-ey1
	if sw <= 0 || sh <= 0 {
		return
	}
	sub := make([]byte, sw*sh)
	for y := 0; y < sh; y++ {
		copy(sub[y*sw:(y+1)*sw], mask[(ey1+y)*w+ex1:(ey1+y)*w+ex1+sw])
	}
	out := maxFilter2D(sub, sw, sh, d)
	for y := 0; y < sh; y++ {
		copy(mask[(ey1+y)*w+ex1:], out[y*sw:(y+1)*sw])
	}
}

// maxFilter2D 方窗最大值滤波（水平+垂直两遍分离实现）。
func maxFilter2D(src []byte, w, h, d int) []byte {
	tmp := make([]byte, len(src))
	out := make([]byte, len(src))
	for y := 0; y < h; y++ {
		row := src[y*w : (y+1)*w]
		t := tmp[y*w : (y+1)*w]
		for x := 0; x < w; x++ {
			var m byte
			lo, hi := x-d, x+d
			if lo < 0 {
				lo = 0
			}
			if hi >= w {
				hi = w - 1
			}
			for xx := lo; xx <= hi; xx++ {
				if row[xx] > m {
					m = row[xx]
				}
			}
			t[x] = m
		}
	}
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			var m byte
			lo, hi := y-d, y+d
			if lo < 0 {
				lo = 0
			}
			if hi >= h {
				hi = h - 1
			}
			for yy := lo; yy <= hi; yy++ {
				if v := tmp[yy*w+x]; v > m {
					m = v
				}
			}
			out[y*w+x] = m
		}
	}
	return out
}

// maskBBox 掩膜非零区域的包围盒；无内容返回 false。
func maskBBox(mask []byte, w, h int) (Rect, bool) {
	minX, minY, maxX, maxY := w, h, -1, -1
	for y := 0; y < h; y++ {
		row := mask[y*w : (y+1)*w]
		for x := 0; x < w; x++ {
			if row[x] > 0 {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxX < 0 {
		return Rect{}, false
	}
	return Rect{minX, minY, maxX + 1, maxY + 1}, true
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

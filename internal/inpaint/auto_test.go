package inpaint

import "testing"

// buildTestMask 按矩形列表生成 0/255 掩膜（Rect 为半开区间）。
func buildTestMask(w, h int, rects []Rect) []byte {
	m := make([]byte, w*h)
	for _, r := range rects {
		for y := r.Y1; y < r.Y2; y++ {
			row := m[y*w : (y+1)*w]
			for x := r.X1; x < r.X2; x++ {
				row[x] = 255
			}
		}
	}
	return m
}

// TestAutoDecision auto 策略决策表：小掩膜大图→crop、大掩膜→original、
// 60% 边界（≤ 走 crop）、多框合并 bbox 口径、空掩膜兜底 original。
func TestAutoDecision(t *testing.T) {
	cases := []struct {
		name      string
		w, h      int
		margin    int
		rects     []Rect
		want      string
		wantRatio float64 // 期望占比（1e-9 精度）；<0 表示不校验
	}{
		{
			// (144,44)-(756,656)=612x612=374544 / 800000 ≈ 0.468
			name: "小掩膜大图→crop", w: 1000, h: 800, margin: 256,
			rects: []Rect{{400, 300, 500, 400}}, want: StrategyCrop, wantRatio: 374544.0 / 800000.0,
		},
		{
			// 掩膜铺满整图 → 外扩后仍为整图，占比 1.0
			name: "大掩膜→original", w: 1000, h: 800, margin: 256,
			rects: []Rect{{0, 0, 1000, 800}}, want: StrategyOriginal, wantRatio: 1.0,
		},
		{
			// margin=0 时 bbox 即外扩结果：600x1000=600000/1000000=0.6 → 边界走 crop
			name: "60%边界→crop", w: 1000, h: 1000, margin: 0,
			rects: []Rect{{200, 0, 800, 1000}}, want: StrategyCrop, wantRatio: 0.6,
		},
		{
			// 0.601 → 超阈值走 original
			name: "60%边界之上→original", w: 1000, h: 1000, margin: 0,
			rects: []Rect{{200, 0, 801, 1000}}, want: StrategyOriginal, wantRatio: 0.601,
		},
		{
			// 多框合并 bbox 口径：两框 (100,100)-(200,200) 与 (800,600)-(900,700)
			// margin=0 → 合并 bbox (100,100)-(900,700)=800x600=480000/800000=0.6 → crop
			name: "多框合并bbox边界→crop", w: 1000, h: 800, margin: 0,
			rects: []Rect{{100, 100, 200, 200}, {800, 600, 900, 700}}, want: StrategyCrop, wantRatio: 0.6,
		},
		{
			// 同上但 margin=256：外扩后铺满整图 → original（决策用有效 margin 口径）
			name: "多框外扩铺满→original", w: 1000, h: 800, margin: 256,
			rects: []Rect{{100, 100, 200, 200}, {800, 600, 900, 700}}, want: StrategyOriginal, wantRatio: 1.0,
		},
		{
			// 边缘框外扩被 clamp 到图像边界：(0,0)-(306,306)=93636/800000≈0.117
			name: "边缘框外扩clamp→crop", w: 1000, h: 800, margin: 256,
			rects: []Rect{{0, 0, 50, 50}}, want: StrategyCrop, wantRatio: 93636.0 / 800000.0,
		},
		{
			name: "空掩膜→original兜底", w: 1000, h: 800, margin: 256,
			rects: nil, want: StrategyOriginal, wantRatio: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mask := buildTestMask(c.w, c.h, c.rects)
			got, ratio := autoDecision(mask, c.w, c.h, c.margin)
			if got != c.want {
				t.Fatalf("策略错误: 期望 %s 实得 %s (ratio=%f)", c.want, got, ratio)
			}
			if c.wantRatio >= 0 {
				if d := ratio - c.wantRatio; d < -1e-9 || d > 1e-9 {
					t.Fatalf("占比错误: 期望 %f 实得 %f", c.wantRatio, ratio)
				}
			}
		})
	}
}

// TestEffectiveCropMargin 裁剪边距下限：64 抬到 256，256 保持，300 保持。
func TestEffectiveCropMargin(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 256}, {64, 256}, {255, 256}, {256, 256}, {300, 300}, {1024, 1024},
	}
	for _, c := range cases {
		if got := effectiveCropMargin(c.in); got != c.want {
			t.Fatalf("effectiveCropMargin(%d)=%d, 期望 %d", c.in, got, c.want)
		}
	}
}

// TestNormalizeStrategy 策略归一：合法值保留，空值/未知回退 auto。
func TestNormalizeStrategy(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", StrategyAuto}, {"auto", StrategyAuto}, {"original", StrategyOriginal},
		{"crop", StrategyCrop}, {"junk", StrategyAuto},
	}
	for _, c := range cases {
		if got := normalizeStrategy(c.in); got != c.want {
			t.Fatalf("normalizeStrategy(%q)=%q, 期望 %q", c.in, got, c.want)
		}
	}
}

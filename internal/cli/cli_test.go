package cli

import (
	"strings"
	"testing"
)

// TestCheckRegionMutex 校验 --mask 与框选参数的互斥规则：
// mask 与任一框选参数同时给出时报错（信息包含固定文案），单独给出或全空时合法。
func TestCheckRegionMutex(t *testing.T) {
	const want = "--mask 与 --box/--boxes 不可同时使用"

	cases := []struct {
		name     string
		mask     string
		box      string
		relBox   string
		boxes    string
		relBoxes string
		wantErr  bool
	}{
		{"mask+box 冲突", "m.png", "1,1,2,2", "", "", "", true},
		{"mask+relative-box 冲突", "m.png", "", "0.1,0.1,0.2,0.2", "", "", true},
		{"mask+boxes 冲突", "m.png", "", "", "1,1,2,2", "", true},
		{"mask+relative-boxes 冲突", "m.png", "", "", "", "0.1,0.1,0.2,0.2", true},
		{"仅 mask 合法", "m.png", "", "", "", "", false},
		{"仅 box 合法", "", "1,1,2,2", "", "", "", false},
		{"仅 relative-box 合法", "", "", "0.1,0.1,0.2,0.2", "", "", false},
		{"仅 boxes 合法", "", "", "", "1,1,2,2", "", false},
		{"仅 relative-boxes 合法", "", "", "", "", "0.1,0.1,0.2,0.2", false},
		{"全部为空合法", "", "", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkRegionMutex(c.mask, c.box, c.relBox, c.boxes, c.relBoxes)
			if c.wantErr {
				if err == nil {
					t.Fatal("期望报错，实际为 nil")
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误信息未包含 %q，实得: %v", want, err)
				}
			} else if err != nil {
				t.Fatalf("期望合法，实际报错: %v", err)
			}
		})
	}
}

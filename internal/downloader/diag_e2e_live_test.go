package downloader

// 端到端诊断：完整走 DownloadDouyin + BGM 候选链，定位用户实际遇到的失败点。
//
// 用法：
//   LIVE=1 DIAG_URL="https://v.douyin.com/Y-dKHMoB8_g/" DIAG_COOKIE_FILE="<path>" \
//     go test ./internal/downloader/ -run TestDiagDouyinE2E -v -timeout 180s

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDiagDouyinE2E(t *testing.T) {
	if os.Getenv("LIVE") != "1" {
		t.Skip("需要外网，设置 LIVE=1 开启")
	}
	u := os.Getenv("DIAG_URL")
	if u == "" {
		u = diagDefaultURL
	}
	// 注入 Cookie（若提供）
	if cf := os.Getenv("DIAG_COOKIE_FILE"); cf != "" {
		raw, err := os.ReadFile(cf)
		if err != nil {
			t.Fatalf("读取 Cookie 失败: %v", err)
		}
		SetPlatformCookie("douyin", strings.TrimSpace(string(raw)))
		t.Logf("已注入抖音 Cookie（%d 字符）", len(strings.TrimSpace(string(raw))))
	} else {
		t.Logf("未注入 Cookie（当前 = %v）", PlatformCookie("douyin") != "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	dst := t.TempDir()
	post, err := Download(ctx, u, dst)
	if err != nil {
		t.Logf("✗ DownloadDouyin 失败: %v", err)
		t.Fatalf("下载层就已失败 —— 用户看到的就是这个错误")
	}
	t.Logf("✓ DownloadDouyin 成功")
	t.Logf("    ID       = %s", post.ID)
	t.Logf("    Title    = %s", post.Title)
	t.Logf("    Author   = %s", post.Author)
	t.Logf("    Count    = %d", post.Count)
	t.Logf("    AudioURL = %q", post.AudioURL)
	t.Logf("    AudioName= %q", post.AudioName)
	t.Logf("    AudioCandidates (%d 条):", len(post.AudioCandidates))
	for i, c := range post.AudioCandidates {
		t.Logf("      [%d] %s", i, truncateURL(c))
	}

	// BGM 下载
	if len(post.AudioCandidates) == 0 {
		t.Logf("✗✗ 候选链为空 —— 这就是「确认有 BGM 却取不到」的直接原因")
		t.Fatalf("候选链为空")
	}
	saved, err := DownloadAudioWithFallback(ctx, CandidatesFromURLs(post.AudioCandidates), dst, "bgm")
	if err != nil {
		t.Logf("✗ BGM 下载失败: %v", err)
		t.Fatalf("BGM 下载失败")
	}
	fi, _ := os.Stat(saved)
	t.Logf("✓ BGM 下载成功: %d 字节", fi.Size())
}

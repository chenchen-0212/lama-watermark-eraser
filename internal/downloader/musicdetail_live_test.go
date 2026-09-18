package downloader

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// TestLiveDouyinMusicDetail 真实网络验证 music/detail 接口兜底能力。
//
// 这是 v1.1.7 修复的核心验收用例：注入一份**SSR 解析降级**的 music 节点
// （url_list 为空、uri 为资源标识），确认候选链能通过 music/detail 接口
// 拿到可下载的真实音频地址，且该地址能通过 probeAudio 探测。
//
// 默认跳过（需外网）；用 LIVE=1 显式开启：
//
//	LIVE=1 go test ./internal/downloader/ -run TestLiveDouyinMusicDetail -v
func TestLiveDouyinMusicDetail(t *testing.T) {
	if os.Getenv("LIVE") != "1" {
		t.Skip("需要外网，设置 LIVE=1 开启")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 复现故障机器场景：SSR 段解析不出任何可用地址
	// music.id 取自用户上报的失败链接（7679463410022550307 是 music.id）
	item := gjson.Parse(`{"music":{"id":"7679463410022550307","title":"","play_url":{"url_list":[],"uri":""}}}`)
	name, urls := douyinMusicCandidates(ctx, item, "")

	t.Logf("name=%q candidates=%d", name, len(urls))
	for i, u := range urls {
		t.Logf("  [%d] %s", i, u)
	}
	if len(urls) == 0 {
		t.Fatal("候选链为空 —— music/detail 兜底未生效")
	}

	// 候选链中的地址必须真实可探测（前 16 字节是音频魔数）
	var okURL string
	for _, u := range urls {
		if err := probeAudio(ctx, u); err != nil {
			t.Logf("probe 失败 %s: %v", u, err)
			continue
		}
		okURL = u
		break
	}
	if okURL == "" {
		t.Fatalf("候选链中无可用地址（共 %d 条）：%v", len(urls), urls)
	}
	t.Logf("PASS: 可用地址 = %s", okURL)

	// 端到端下载验证
	dst := t.TempDir()
	saved, err := DownloadAudioWithFallback(ctx, urls, dst, "bgm")
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	fi, err := os.Stat(saved)
	if err != nil {
		t.Fatalf("产物不存在: %v", err)
	}
	if fi.Size() < 1024 {
		t.Fatalf("产物过小（%d 字节），可能是错误响应", fi.Size())
	}
	t.Logf("PASS: 下载成功 %s（%d 字节）", saved, fi.Size())
}

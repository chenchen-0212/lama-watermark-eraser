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
	name, cands := douyinMusicCandidates(ctx, item, "")

	t.Logf("name=%q candidates=%d", name, len(cands))
	for i, c := range cands {
		t.Logf("  [%d] source=%s %s", i, c.Source, c.URL)
	}
	if len(cands) == 0 {
		t.Fatal("候选链为空 —— music/detail 兜底未生效")
	}

	// 端到端下载验证：下载过程本身即探测（先读头部做魔数校验，再继续写入
	// 同一个响应体），无需额外发一轮 Range 预探测。
	dst := t.TempDir()
	saved, err := DownloadAudioWithFallback(ctx, cands, dst, "bgm")
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

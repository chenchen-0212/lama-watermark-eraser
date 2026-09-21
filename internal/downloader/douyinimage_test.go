package downloader

// 抖音图文帖 BGM 取源回归测试。
//
// 背景（2026-09-21 实测 https://v.douyin.com/Y-dKHMoB8_g/ 定位）：
// 图文帖（aweme_type=2）的 SSR music 节点可能**只有 mid、完全没有 play_url**，
// 此时旧实现收集到的候选链长度为 0，BGM 必然失败。真实音频挂在
// video.play_addr.uri（形如 ies-music-hj/{fileID}.mp3 的对象存储直链）。
//
// 同时锁住另一个坑：分享页 loaderData 的键名是字面量 "note_(id)/page"，
// 其中 (id) 不是占位符，而是真实键名字符串本身。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"net/http"
	"net/http/httptest"
)

// douyinImageItemJSON 按实测分享页 item[0] 的结构裁剪：
// music 只有 mid（无 id、无 play_url），BGM 在 video.play_addr.uri。
const douyinImageItemJSON = `{
  "aweme_id": "7686009647208381819",
  "desc": "晚秋黑珠。#画一个故事 #悬疑 #漫画",
  "author": {"nickname": "秋晚"},
  "aweme_type": 2,
  "images": [{"url_list": ["https://p5-ex-gddgtc-sign.douyinpic.com/tos-cn-i-0813c000-ce/img1.webp"]}],
  "music": {
    "mid": "7505383933425879858",
    "title": "@晴南创作的原声一晴南（原声中的歌曲：活下去（纯音乐）-Eliezer）",
    "author": "晴南",
    "duration": 122,
    "status": 1
  },
  "video": {
    "play_addr": {
      "uri": "https://lf26-music-east.douyinstatic.com/obj/ies-music-hj/7505383972596108090.mp3",
      "url_list": ["https://aweme.snssdk.com/aweme/v1/playwm/?video_id=https://lf26-music-east.douyinstatic.com/obj/ies-music-hj/7505383972596108090.mp3&ratio=720p&line=0"]
    }
  }
}`

// TestDouyinImageBGMFromVideoPlayAddr 图文帖 music 无 play_url 时，
// 候选链必须能从 video.play_addr.uri 拿到可下载地址。
func TestDouyinImageBGMFromVideoPlayAddr(t *testing.T) {
	resetAudioState(t)
	item := gjson.Parse(douyinImageItemJSON)

	// 前提确认：music 节点确实没有 play_url（这是本用例的立足点）
	if item.Get("music.play_url").Exists() {
		t.Fatal("样本结构不符合前提：music.play_url 竟然存在，用例失去意义")
	}
	if musicID(item.Get("music")) == "" {
		t.Fatal("样本应有 mid 可供回落")
	}

	name, cands := douyinMusicCandidates(context.Background(), item, "7686009647208381819")
	if name == "" {
		t.Error("曲名不应为空")
	}
	if len(cands) == 0 {
		t.Fatal("候选链为空 —— 图文帖 BGM 未从 video.play_addr 收录（回归）")
	}

	want := "https://lf26-music-east.douyinstatic.com/obj/ies-music-hj/7505383972596108090.mp3"
	if cands[0].URL != want {
		t.Errorf("链首候选 = %q，期望 %q", cands[0].URL, want)
	}
	if cands[0].Source != SourceDouyinImagePlayAddr {
		t.Errorf("链首来源 = %q，期望 %q", cands[0].Source, SourceDouyinImagePlayAddr)
	}
	// 链首应为 0 优先级，确保 failover 先试它
	if cands[0].Priority != 0 {
		t.Errorf("链首优先级 = %d，期望 0", cands[0].Priority)
	}
}

// TestDouyinImagePlayAddrSkippedForVideoPost 视频帖不得收录 video.play_addr，
// 否则会把视频本体当 BGM 下载。
func TestDouyinImagePlayAddrSkippedForVideoPost(t *testing.T) {
	resetAudioState(t)
	// 去掉 images → 成为视频帖形态
	item := gjson.Parse(douyinImageItemJSON)
	obj := item.Value().(map[string]any)
	delete(obj, "images")
	raw, _ := json.Marshal(obj)
	vidItem := gjson.ParseBytes(raw)

	if vidItem.Get("images").Exists() {
		t.Fatal("构造失败：images 应已被删除")
	}
	_, cands := douyinMusicCandidates(context.Background(), vidItem, "7686009647208381819")
	for _, c := range cands {
		if c.Source == SourceDouyinImagePlayAddr {
			t.Fatalf("视频帖不应收录 video.play_addr（会下成视频），实际收录 %q", c.URL)
		}
	}
}

// TestDouyinMusicPlayURLStillPreferredWhenPresent 有 play_url 时，
// 官方 SSR 地址仍应可用（不得因新增来源而丢弃原有路径）。
func TestDouyinMusicPlayURLStillPreferredWhenPresent(t *testing.T) {
	resetAudioState(t)
	item := gjson.Parse(`{
	  "images": [{"url_list": ["https://p3.douyinpic.com/a.webp"]}],
	  "music": {
	    "id": "7505383933425879858",
	    "mid": "7505383933425879858",
	    "title": "原声",
	    "play_url": {
	      "uri": "https://lf26-music-east.douyinstatic.com/obj/ies-music-hj/9999999.mp3",
	      "url_list": ["https://lf26-music-east.douyinstatic.com/obj/ies-music-hj/8888888.mp3"]
	    }
	  },
	  "video": {
	    "play_addr": {"uri": "https://lf26-music-east.douyinstatic.com/obj/ies-music-hj/7777777.mp3"}
	  }
	}`)

	_, cands := douyinMusicCandidates(context.Background(), item, "7686009647208381819")
	foundSSRList := false
	foundImageAddr := false
	for _, c := range cands {
		switch c.Source {
		case SourceDouyinSSRURLList:
			foundSSRList = true
		case SourceDouyinImagePlayAddr:
			foundImageAddr = true
		}
	}
	if !foundSSRList {
		t.Error("有 play_url 时应收录 SSR url_list 来源")
	}
	if !foundImageAddr {
		t.Error("图文帖即使有 play_url，也应并入 video.play_addr（多一层兜底）")
	}
}

// TestFindDouyinItemSharePageLiteralKey 分享页键名是字面量 "note_(id)/page"，
// (id) 不是占位符。旧实现只拼真实 id，永远匹配不上。
func TestFindDouyinItemSharePageLiteralKey(t *testing.T) {
	// 复刻实测分享页结构：键名就是字面量 note_(id)/page
	root := gjson.Parse(`{
	  "loaderData": {
	    "note_layout": null,
	    "note_(id)/page": {
	      "ua": "Mozilla/5.0 (iPhone)",
	      "itemId": "7686009647208381819",
	      "videoInfoRes": {
	        "status_code": 0,
	        "item_list": [ {"aweme_id": "7686009647208381819", "desc": "晚秋黑珠"} ]
	      }
	    }
	  }
	}`)

	got := findDouyinItem(root, "7686009647208381819")
	if !got.Exists() {
		t.Fatal("字面量键 note_(id)/page 未被命中（回归）")
	}
	if got.Get("aweme_id").String() != "7686009647208381819" {
		t.Errorf("取到错误 item: %s", got.Raw)
	}
}

// TestFindDouyinItemDesktopPageRealIDKey 新版桌面页键名嵌真实 id，仍须命中。
func TestFindDouyinItemDesktopPageRealIDKey(t *testing.T) {
	const id = "7686009647208381819"
	root := gjson.Parse(`{
	  "loaderData": {
	    "note_(` + id + `)/page": {
	      "videoInfoRes": {"item_list": [ {"aweme_id": "` + id + `"} ]}
	    }
	  }
	}`)
	got := findDouyinItem(root, id)
	if !got.Exists() {
		t.Fatal("真实 id 键名未被命中")
	}
}

// TestFindDouyinItemLegacyItemListPath 兼容旧版直接挂在节点下的 item_list。
func TestFindDouyinItemLegacyItemListPath(t *testing.T) {
	const id = "1234567890123456789"
	root := gjson.Parse(`{
	  "loaderData": {
	    "note_(id)/page": {
	      "item_list": [ {"aweme_id": "` + id + `", "desc": "legacy"} ]
	    }
	  }
	}`)
	got := findDouyinItem(root, id)
	if !got.Exists() {
		t.Fatal("旧版 item_list 直挂路径未被命中")
	}
	if got.Get("desc").String() != "legacy" {
		t.Errorf("取到错误 item: %s", got.Raw)
	}
}

// TestFindDouyinItemRejectsQuotedKeyPath 锁住 gjson 路径的引号陷阱。
//
// 实测：`loader.Get(`"note_(id)/page".videoInfoRes...`)`（键名带引号）恒为 false，
// 因为 ( ) / 都不是 gjson 保留字符，双引号反倒成了键名的一部分。
// 这是曾经长期潜伏的实现 bug——精确查询全部失效，只靠遍历兜底才没暴露。
func TestFindDouyinItemRejectsQuotedKeyPath(t *testing.T) {
	root := gjson.Parse(`{
	  "loaderData": {
	    "note_(id)/page": {
	      "videoInfoRes": {"item_list": [ {"aweme_id": "111"} ]}
	    }
	  }
	}`)
	loader := root.Get("loaderData")

	// 引号写法必须失败（若这条断言失效，说明 gjson 语义变了，需重新评估 findDouyinItem）
	if loader.Get(`"note_(id)/page".videoInfoRes.item_list.0`).Exists() {
		t.Error("引号键路径竟然命中——gjson 语义可能已变化，请复核 findDouyinItem 的实现前提")
	}
	// 无引号写法必须成功（这是 findDouyinItem 依赖的正确形式）
	if !loader.Get("note_(id)/page.videoInfoRes.item_list.0").Exists() {
		t.Fatal("无引号键路径未命中——findDouyinItem 的精确查询将全部失效")
	}
	// 端到端：findDouyinItem 应走精确命中而非兜底
	if got := findDouyinItem(root, "111"); !got.Exists() {
		t.Fatal("findDouyinItem 未命中")
	}
}

// TestFindDouyinItemExactKeyBeforeFallback 确保精确键优先于遍历兜底命中。
// 构造两个键：字面量键含正确 item，另一键含干扰 item；
// 若精确查询生效，必须取到字面量键里的那个。
func TestFindDouyinItemExactKeyBeforeFallback(t *testing.T) {
	root := gjson.Parse(`{
	  "loaderData": {
	    "note_(id)/page": {
	      "videoInfoRes": {"item_list": [ {"aweme_id": "exact"} ]}
	    },
	    "commentListData": {
	      "videoInfoRes": {"item_list": [ {"aweme_id": "decoy"} ]}
	    }
	  }
	}`)
	got := findDouyinItem(root, "7686009647208381819")
	if !got.Exists() {
		t.Fatal("未命中")
	}
	if got.Get("aweme_id").String() != "exact" {
		t.Errorf("取到 %q，期望 exact —— 说明精确键未生效、退到了遍历兜底",
			got.Get("aweme_id").String())
	}
}

// TestAudioImageBGMFullDownload 端到端：图文帖候选（来自 video.play_addr）
// 能真实下载落盘，验证该来源与下载器链路兼容。
func TestAudioImageBGMFullDownload(t *testing.T) {
	resetAudioState(t)
	payload := []byte("ID3\x03\x00" + strings.Repeat("bgm-", 400))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	item := gjson.Parse(`{
	  "images": [{"url_list": ["https://p3.douyinpic.com/a.webp"]}],
	  "music": {"mid": "7505383933425879858", "title": "原声"},
	  "video": {"play_addr": {"uri": "` + srv.URL + `/obj/ies-music-hj/7505383972596108090.mp3"}}
	}`)

	_, cands := douyinMusicCandidates(context.Background(), item, "7686009647208381819")
	if len(cands) == 0 {
		t.Fatal("候选链为空")
	}
	dst := t.TempDir()
	saved, err := DownloadAudioWithFallback(context.Background(), cands, dst, "bgm")
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	fi, err := os.Stat(saved)
	if err != nil {
		t.Fatalf("产物不存在: %v", err)
	}
	if fi.Size() != int64(len(payload)) {
		t.Errorf("产物大小 = %d，期望 %d", fi.Size(), len(payload))
	}
	if filepath.Base(saved) != "bgm.mp3" {
		t.Errorf("产物名 = %q，期望 bgm.mp3", filepath.Base(saved))
	}
}

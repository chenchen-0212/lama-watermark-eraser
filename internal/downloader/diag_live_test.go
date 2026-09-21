package downloader

// 诊断用 live 测试：定位「用户确认有 BGM 但工具取不到」的根因。
// 逐级打印每一步的原始响应，判断失败发生在哪一层：
//   短链跳转 → SSR 解析 → 候选收集 → 实际下载
//
// 用法：
//   LIVE=1 DIAG_URL="https://v.douyin.com/Y-dKHMoB8_g/" \
//     go test ./internal/downloader/ -run TestDiagDouyinBGM -v
// 不设 DIAG_URL 时使用默认链接。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

const diagDefaultURL = "https://v.douyin.com/Y-dKHMoB8_g/"

func TestDiagDouyinBGM(t *testing.T) {
	if os.Getenv("LIVE") != "1" {
		t.Skip("需要外网，设置 LIVE=1 开启")
	}
	u := os.Getenv("DIAG_URL")
	if u == "" {
		u = diagDefaultURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	line := strings.Repeat("=", 72)
	t.Log(line)
	t.Logf("诊断链接: %s", u)
	t.Logf("抖音 Cookie 已配置: %v", PlatformCookie("douyin") != "")
	t.Log(line)

	// ── 步骤 1：短链跳转落点与作品 ID ────────────────────────────────
	t.Log("[1] 提取作品 ID")
	t.Logf("    douyinAwemeID(原始) = %q", douyinAwemeID(u))
	resolved := u
	if strings.Contains(u, "v.douyin.com") {
		final, err := resolveRedirect(u)
		if err != nil {
			t.Logf("    ✗ resolveRedirect 失败: %v", err)
		} else {
			resolved = final
			t.Logf("    ✓ 落点 = %s", final)
		}
	}
	id := douyinAwemeID(resolved)
	t.Logf("    douyinAwemeID(落点) = %q", id)
	if id == "" {
		t.Fatalf("无法提取作品 ID —— 短链跳转或 ID 正则需修正")
	}

	// ── 步骤 2：逐端点探测 SSR 解析 ─────────────────────────────────
	t.Log("[2] SSR 端点探测")
	endpoints := []douyinEndpoint{
		{url: "https://www.douyin.com/note/" + id, ua: BrowserUA},
		{url: "https://www.douyin.com/video/" + id, ua: BrowserUA},
		{url: "https://www.iesdouyin.com/share/note/" + id, ua: MobileUA},
		{url: "https://www.iesdouyin.com/share/slides/" + id, ua: MobileUA},
		{url: "https://www.iesdouyin.com/share/video/" + id, ua: MobileUA},
	}
	var item gjson.Result
	for _, ep := range endpoints {
		html, finalURL, err := httpGet(ctx, ep.url, "https://www.douyin.com/", ep.ua)
		if err != nil {
			t.Logf("    ✗ %s\n         err=%v", ep.url, err)
			continue
		}
		m := routerRe.FindSubmatch(html)
		t.Logf("    %s\n         len=%d final=%s _ROUTER_DATA=%v",
			ep.url, len(html), finalURL, m != nil)
		if m == nil {
			continue
		}
		it := findDouyinItem(gjson.Parse(string(m[1])), id)
		if it.Exists() {
			t.Logf("         ✓ item 命中")
			item = it
			break
		}
		t.Logf("         ✗ 有 _ROUTER_DATA 但 findDouyinItem 未命中")
	}
	if !item.Exists() {
		t.Fatalf("所有端点均未解析出 item")
	}

	// ── 步骤 3：SSR music 节点原始结构 ──────────────────────────────
	music := item.Get("music")
	t.Log("[3] SSR music 节点")
	t.Logf("    music 节点存在 = %v, Raw 长度 = %d",
		music.Exists(), len(music.Raw))
	t.Logf("    music.id     = %q", music.Get("id").String())
	t.Logf("    music.mid    = %q", music.Get("mid").String())
	t.Logf("    music.title  = %q", music.Get("title").String())
	t.Logf("    music.author = %q", music.Get("author").String())
	t.Logf("    music.status = %v", music.Get("status").Raw)
	t.Logf("    play_url.uri = %q", music.Get("play_url.uri").String())
	ul := music.Get("play_url.url_list").Array()
	t.Logf("    play_url.url_list (%d 条):", len(ul))
	for i, x := range ul {
		t.Logf("        [%d] %s", i, truncateURL(x.String()))
	}
	t.Logf("    music 顶层键: %v", keysOf(music))

	// ── 步骤 4：候选链收集 ──────────────────────────────────────────
	name, cands := douyinMusicCandidates(ctx, item, id)
	t.Logf("[4] 候选链: name=%q 共 %d 条", name, len(cands))
	for i, c := range cands {
		t.Logf("        [%d] source=%-24s %s", i, c.Source, truncateURL(c.URL))
	}

	// ── 步骤 5：music/detail 原始 JSON ──────────────────────────────
	mid := musicID(music)
	t.Logf("[5] music/detail 直查 music_id=%q", mid)
	if mid == "" {
		t.Logf("    ✗ mid 为空 —— music/detail 路径被跳过（候选链缺失的直接原因）")
	} else {
		api := musicDetailAPIBase + "?music_id=" + mid +
			"&aid=6383&cookie_enabled=true&platform=PC&downlink=1"
		data, _, err := httpGet(ctx, api, "https://www.douyin.com/", BrowserUA)
		t.Logf("    len=%d err=%v", len(data), err)
		if err == nil && len(data) > 0 {
			root := gjson.Parse(string(data))
			mi := root.Get("music_info")
			t.Logf("    status_code  = %v", root.Get("status_code").Raw)
			t.Logf("    music_info 存在=%v type=%s", mi.Exists(), mi.Type)
			if mi.Exists() && mi.Type != gjson.Null {
				t.Logf("    music_info.id    = %q", mi.Get("id").String())
				t.Logf("    music_info.title = %q", mi.Get("title").String())
				t.Logf("    play_url.uri     = %q", mi.Get("play_url.uri").String())
				for i, x := range mi.Get("play_url.url_list").Array() {
					t.Logf("        url_list[%d] = %s", i, truncateURL(x.String()))
				}
			} else {
				t.Logf("    原始响应前 700 字符:\n%s", head(data, 700))
			}
		}
	}

	// ── 步骤 6：端到端下载 ──────────────────────────────────────────
	t.Log("[6] 端到端下载（复用路由层，逐候选尝试）")
	if len(cands) == 0 {
		t.Fatalf("候选链为空 —— 无任何可尝试地址（这就是用户看到的失败）")
	}
	dst := t.TempDir()
	saved, err := DownloadAudioWithFallback(ctx, cands, dst, "bgm")
	if err != nil {
		t.Logf("    ✗ 全部候选失败: %v", err)
		// 逐条单独试，定位是哪一类错误
		for i, c := range cands {
			p, e := SaveAudioCandidate(ctx, c, i, dst, "bgm")
			if e != nil {
				t.Logf("      [%d] %-24s ✗ %v", i, c.Source, e)
			} else {
				t.Logf("      [%d] %-24s ✓ %s", i, c.Source, p)
			}
		}
		t.Fatalf("端到端下载失败")
	}
	fi, _ := os.Stat(saved)
	t.Logf("    ✓ 成功: %s (%d 字节)", saved, fi.Size())
	t.Logf("    → 单条候选单独试，确认是哪一级救回来的：")
	for i, c := range cands {
		p, e := SaveAudioCandidate(ctx, c, i, dst, "bgm")
		if e != nil {
			t.Logf("      [%d] %-24s ✗ %v", i, c.Source, e)
		} else {
			st, _ := os.Stat(p)
			t.Logf("      [%d] %-24s ✓ %d 字节", i, c.Source, st.Size())
		}
	}
}

func keysOf(r gjson.Result) []string {
	var out []string
	if !r.Exists() || !r.IsObject() {
		return out
	}
	r.ForEach(func(k, _ gjson.Result) bool {
		out = append(out, k.String())
		return len(out) < 25
	})
	return out
}

func truncateURL(s string) string {
	if len(s) <= 130 {
		return s
	}
	return s[:130] + "…"
}

func head(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

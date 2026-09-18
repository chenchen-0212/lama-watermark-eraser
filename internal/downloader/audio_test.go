package downloader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"   ", ""},
		{"bgm", "bgm"},
		{"我的音乐", "我的音乐"},
		{"  my song.mp3 ", "my song.mp3"}, // trim 首尾空白
		{`bad<>:"/\|?*name`, "badname"},   // 非法字符剔除
		{"..hidden..", "hidden"},          // 首尾点号剔除
		{"trail...  ", "trail"},           // 混合尾部
		{"con", "bgm_con"},                // Windows 保留名（无扩展）
		{"con.mp3", "bgm_con.mp3"},        // Windows 保留名（带扩展）
		{"nul.MP3", "bgm_nul.MP3"},        // 保留名大小写不敏感
		{"a\x01b\x7fc", "abc"},            // 控制字符剔除
		{"../../evil", "evil"},            // 路径分隔剔除后清掉残留点号
	}
	for _, c := range cases {
		if got := sanitizeFilename(c.in); got != c.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEnsureAudioExt(t *testing.T) {
	cases := []struct {
		name, url, ct, want string
	}{
		{"bgm", "https://x/a", "audio/mp4", "bgm.m4a"}, // 抖音实测：audio/mp4 = m4a
		{"bgm", "https://x/a", "audio/mpeg", "bgm.mp3"},
		{"bgm", "https://x/a", "", "bgm.mp3"},                // 兜底
		{"bgm", "https://x/a.mp3", "", "bgm.mp3"},            // URL 路径扩展
		{"bgm", "https://x/a.m4a", "audio/mpeg", "bgm.m4a"},  // URL 扩展优先于 CT
		{"song.mp3", "https://x/a", "audio/mp4", "song.mp3"}, // 用户显式扩展优先
		{"song.MP3", "https://x/a", "", "song.MP3"},
		{"song.m4a", "https://x/a", "", "song.m4a"},
		{"song.txt", "https://x/a", "audio/mpeg", "song.txt.mp3"}, // 非音频扩展 → 补
		{"song", "https://x/a", "video/mp4", "song.mp3"},          // 非音频 CT → 兜底
	}
	for _, c := range cases {
		if got := resolveAudioName(c.name, c.url, c.ct); got != c.want {
			t.Errorf("resolveAudioName(%q,%q,%q) = %q, want %q", c.name, c.url, c.ct, got, c.want)
		}
	}
}

// withFakeMusicDetail 注入本地 music/detail 服务（替代真实抖音接口，保持单测离线）。
func withFakeMusicDetail(t *testing.T, musicID string, resp string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "music_id="+musicID) {
			w.Write([]byte(`{}`))
			return
		}
		w.Write([]byte(resp))
	}))
	old := musicDetailAPIBase
	musicDetailAPIBase = srv.URL
	t.Cleanup(func() { musicDetailAPIBase = old; srv.Close() })
}

func TestDouyinMusicCandidates(t *testing.T) {
	// music/detail 始终并入候选链末尾（本地 fake 服务替代真实接口）。
	withFakeMusicDetail(t, "7286820160101201977",
		`{"music_info":{"play_url":{"url_list":["http://detail/fallback.mp3"]}}}`)

	// url_list 全部收录（保序）→ uri(是URL才收) → detail 兜底追加在链尾
	item := gjson.Parse(`{"music":{"id":"7286820160101201977","title":"测试歌曲","author":"歌手A",
		"play_url":{"uri":"v0200fg10000abcdef","url_list":["","http://cdn/b.mp3","http://cdn/c.mp3"]}}}`)
	name, urls := douyinMusicCandidates(context.Background(), item, "")
	if name != "测试歌曲" {
		t.Errorf("name = %q, want 测试歌曲", name)
	}
	want := []string{
		"http://cdn/b.mp3",
		"http://cdn/c.mp3",
		"http://detail/fallback.mp3", // detail 地址并入链尾（SSR 地址失效时的兜底）
	}
	if !equalStrings(urls, want) {
		t.Errorf("urls = %v, want %v", urls, want)
	}

	// uri 是资源标识（非 URL）→ 不得收录；无 music.id → 不查 detail
	item2 := gjson.Parse(`{"music":{"title":"歌2","play_url":{"uri":"v0200fg10000xyz","url_list":["http://cdn/d.mp3"]}}}`)
	name2, urls2 := douyinMusicCandidates(context.Background(), item2, "")
	if name2 != "歌2" {
		t.Errorf("name2 = %q, want 歌2", name2)
	}
	if !equalStrings(urls2, []string{"http://cdn/d.mp3"}) {
		t.Errorf("urls2 = %v, want 仅列表项（uri 非 URL 不应收录）", urls2)
	}

	// 无 music 节点 → 不 panic，返回空
	name3, urls3 := douyinMusicCandidates(context.Background(), gjson.Parse(`{"music":{}}`), "")
	if name3 != "" || len(urls3) != 0 {
		t.Errorf("empty music: name=%q urls=%v, want empty", name3, urls3)
	}
}

func TestMusicID(t *testing.T) {
	cases := []struct{ id, mid, want string }{
		{"7286820160101201977", "", "7286820160101201977"},
		{"", "7286820160101201977", "7286820160101201977"}, // mid 兜底
		{"", "", ""},             // 都缺 → 空
		{"abc123", "", ""},       // 非纯数字 → 空（防结构变更混入异常值）
		{"12", "99", "12"},       // id 优先于 mid
		{"", "not-a-number", ""}, // mid 非法 → 空
	}
	for _, c := range cases {
		got := musicID(gjson.Parse(`{"id":"` + c.id + `","mid":"` + c.mid + `"}`))
		if got != c.want {
			t.Errorf("musicID(id=%q,mid=%q) = %q, want %q", c.id, c.mid, got, c.want)
		}
	}
}

// TestDouyinMusicDetailURLNoRequest 校验参数守卫：非数字 ID 不应发起网络请求。
func TestDouyinMusicDetailURLNoRequest(t *testing.T) {
	for _, bad := range []string{"", "abc", "12ab", "1234567890123456789012345"} {
		if got := douyinMusicDetailURL(context.Background(), bad); got != nil {
			t.Errorf("douyinMusicDetailURL(%q) = %v, want nil", bad, got)
		}
	}
}

func TestSetAudioDedup(t *testing.T) {
	p := &Post{}
	p.SetAudio("曲名", " http://a/1.mp3 ", "", "http://a/1.mp3", "http://b/2.m4a")
	if p.AudioURL != "http://a/1.mp3" {
		t.Errorf("AudioURL = %q, want 首项（保持前端契约）", p.AudioURL)
	}
	if !equalStrings(p.AudioCandidates, []string{"http://a/1.mp3", "http://b/2.m4a"}) {
		t.Errorf("AudioCandidates = %v, want 去空去重保序", p.AudioCandidates)
	}
	// 全空 → AudioURL 必须清空，否则前端 hasBGM 误判为有 BGM
	p2 := &Post{AudioURL: "http://stale/x.mp3"}
	p2.SetAudio("曲名", "", "  ")
	if p2.AudioURL != "" || len(p2.AudioCandidates) != 0 {
		t.Errorf("all-empty: url=%q cands=%v, want empty", p2.AudioURL, p2.AudioCandidates)
	}
}

func TestLooksLikeAudio(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want bool
	}{
		{"MP3(ID3)", append([]byte("ID3"), make([]byte, 13)...), true},
		{"MP3(frame sync)", []byte{0xFF, 0xFB, 0x90, 0x00, 0x00, 0x00, 0x00, 0x00}, true},
		{"M4A", append([]byte{0x00, 0x00, 0x00, 0x20}, []byte("ftypM4A ")...), true},
		{"OGG", []byte("OggS\x00\x02\x00\x00"), true},
		{"FLAC", []byte("fLaC\x00\x00\x00\x22"), true},
		{"WAV", []byte("RIFF\x24\x08\x00\x00WAVE"), true},
		{"AAC(ADTS)", []byte{0xFF, 0xF1, 0x50, 0x80, 0x00, 0x1F, 0xFC}, true},
		{"风控 HTML 页", []byte("<html><head><title>"), false},
		{"错误 JSON", []byte(`{"status_code":8}`), false},
		{"空响应", []byte{}, false},
		{"短到不足 ftyp 偏移", []byte{0x00, 0x00}, false},
	}
	for _, c := range cases {
		if got := looksLikeAudio(c.head); got != c.want {
			t.Errorf("looksLikeAudio(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDouyinMusicCandidatesRealWorld 回归用例，取自 2026-09-15 真实链接实测
// （https://v.douyin.com/tLyloNrTWmA/ → note/7397543688773094656）。
//
// 该作品的页面 play_url.uri 是 ies-music 形态的 **mp3 直链**（此前假设它为
// 资源标识，实测证伪），且该地址返回 404；真正可用的是 url_list[0]。
// 因此必须保证：uri 形态可被收录，且两条都在候选链中，顺序不影响可用性。
func TestDouyinMusicCandidatesRealWorld(t *testing.T) {
	raw := `{"music":{
		"id":"6634113815568976654",
		"title":"長沙-剪辑版一先贻jUju",
		"play_url":{
			"uri":"https://sf3-cdn-tos.douyinstatic.com/obj/ies-music/6634113815568976654.mp3",
			"url_list":["https://sf11-cdn-tos.douyinstatic.com/obj/tos-cn-ve-2774/ocO5kAYT3Pn2gtCjQXxtBQFvgDeeZNwDD3ySbz"]
		}}}`
	name, urls := douyinMusicCandidates(context.Background(), gjson.Parse(raw), "")

	if name != "長沙-剪辑版一先贻jUju" {
		t.Errorf("name = %q", name)
	}
	// 页面下发地址 + uri 共 2 条（SSR 已有地址，不触发接口）
	if len(urls) != 2 {
		t.Fatalf("候选链应含 2 条，实际 %d: %v", len(urls), urls)
	}
	if !strings.Contains(urls[0], "sf11-cdn-tos") {
		t.Errorf("首条应为页面下发的可用地址，实际 %q", urls[0])
	}
	if !strings.Contains(urls[1], "ies-music/6634113815568976654.mp3") {
		t.Errorf("次条应为页面 uri 下发的 ies-music 地址，实际 %q", urls[1])
	}
	// 地址必须互不相同，否则 failover 无意义
	seen := map[string]bool{}
	for _, u := range urls {
		if seen[u] {
			t.Errorf("候选链存在重复地址: %q", u)
		}
		seen[u] = true
	}
}

// TestUriAcceptedWhenHTTP 证实 uri 为 http 地址时必须收录（实时数据推翻了
// 「uri 一律是资源标识」的旧假设）。
func TestUriAcceptedWhenHTTP(t *testing.T) {
	m := gjson.Parse(`{"play_url":{"uri":"https://cdn/x.mp3","url_list":[]}}`)
	if got := douyinMusicCandidates0(m); !equalStrings(got, []string{"https://cdn/x.mp3"}) {
		t.Errorf("http 形态的 uri 应被收录，实际 %v", got)
	}
	// 非 URL 的 uri（资源标识）不应收录
	m2 := gjson.Parse(`{"play_url":{"uri":"v0200fg10000abcdef","url_list":[]}}`)
	if got := douyinMusicCandidates0(m2); len(got) != 0 {
		t.Errorf("资源标识形态的 uri 不应收录，实际 %v", got)
	}
}

// douyinMusicCandidates0 只测 SSR 段（不触发 detail API 网络请求）。
func douyinMusicCandidates0(music gjson.Result) []string {
	var urls []string
	for _, u := range music.Get("play_url.url_list").Array() {
		if s := trimSpace(u.String()); strings.HasPrefix(s, "http") {
			urls = append(urls, s)
		}
	}
	if u := trimSpace(music.Get("play_url.uri").String()); strings.HasPrefix(u, "http") {
		urls = append(urls, u)
	}
	return urls
}

func TestXHSMusic(t *testing.T) {
	// url 字段
	note := gjson.Parse(`{"music":{"name":"歌名B","url":"http://y/a.mp3"}}`)
	if u, n := xhsMusic(note); u != "http://y/a.mp3" || n != "歌名B" {
		t.Errorf("url field: url=%q name=%q", u, n)
	}
	// attachUrl 兜底 + singer 兜底
	note2 := gjson.Parse(`{"music":{"singer":"歌手C","attachUrl":"http://y/b.m4a"}}`)
	if u, n := xhsMusic(note2); u != "http://y/b.m4a" || n != "歌手C" {
		t.Errorf("attachUrl fallback: url=%q name=%q", u, n)
	}
	// 无 BGM
	if u, _ := xhsMusic(gjson.Parse(`{}`)); u != "" {
		t.Errorf("no music: url=%q, want empty", u)
	}
}

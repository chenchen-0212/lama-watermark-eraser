package downloader

import (
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
		{"  my song.mp3 ", "my song.mp3"},           // trim 首尾空白
		{`bad<>:"/\|?*name`, "badname"},             // 非法字符剔除
		{"..hidden..", "hidden"},                    // 首尾点号剔除
		{"trail...  ", "trail"},                     // 混合尾部
		{"con", "bgm_con"},                          // Windows 保留名（无扩展）
		{"con.mp3", "bgm_con.mp3"},                  // Windows 保留名（带扩展）
		{"nul.MP3", "bgm_nul.MP3"},                  // 保留名大小写不敏感
		{"a\x01b\x7fc", "abc"},                      // 控制字符剔除
		{"../../evil", "evil"},                      // 路径分隔剔除后清掉残留点号
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
		{"bgm", "https://x/a", "audio/mp4", "bgm.m4a"},      // 抖音实测：audio/mp4 = m4a
		{"bgm", "https://x/a", "audio/mpeg", "bgm.mp3"},
		{"bgm", "https://x/a", "", "bgm.mp3"},               // 兜底
		{"bgm", "https://x/a.mp3", "", "bgm.mp3"},           // URL 路径扩展
		{"bgm", "https://x/a.m4a", "audio/mpeg", "bgm.m4a"}, // URL 扩展优先于 CT
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

func TestDouyinMusic(t *testing.T) {
	item := gjson.Parse(`{"music":{"title":"测试歌曲","author":"歌手A",
		"play_url":{"uri":"http://cdn/a.mp3","url_list":["","http://cdn/b.mp3"]}}}`)
	url, name := douyinMusic(item)
	if url != "http://cdn/b.mp3" {
		t.Errorf("url = %q, want http://cdn/b.mp3（url_list 应取第一个非空）", url)
	}
	if name != "测试歌曲" {
		t.Errorf("name = %q, want 测试歌曲", name)
	}
	// 无音乐节点 → 空串
	if u, n := douyinMusic(gjson.Parse(`{"music":{}}`)); u != "" || n != "" {
		t.Errorf("empty music: url=%q name=%q, want empty", u, n)
	}
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

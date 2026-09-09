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
	cases := []struct{ in, want string }{
		{"bgm", "bgm.mp3"},
		{"", ".mp3"},
		{"song.mp3", "song.mp3"},
		{"song.MP3", "song.MP3"},
		{"song.m4a", "song.m4a"},
		{"song.M4A", "song.M4A"},
		{"song.txt", "song.txt.mp3"}, // 非音频扩展 → 追加
	}
	for _, c := range cases {
		if got := ensureAudioExt(c.in); got != c.want {
			t.Errorf("ensureAudioExt(%q) = %q, want %q", c.in, got, c.want)
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

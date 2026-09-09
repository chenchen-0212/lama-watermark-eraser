package downloader

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

var (
	xhsStateRe    = regexp.MustCompile(`window\.__INITIAL_STATE__\s*=\s*(\{.*?\})\s*</script>`)
	xhsNotePathRe = regexp.MustCompile(`/(?:explore|discovery/item)/([0-9a-fA-F]+)`)
)

// DownloadXHS 下载小红书图文笔记的图片（读取服务端渲染的 __INITIAL_STATE__，匿名可抓公开笔记）。
func DownloadXHS(ctx context.Context, url, outDir string) (*Post, error) {
	id := ""
	if m := xhsNotePathRe.FindStringSubmatch(url); m != nil {
		id = m[1]
	}
	if id == "" {
		return nil, fmt.Errorf("无法从小红书链接解析笔记 ID，请使用笔记详情页完整链接（含 xsec_token 参数更佳）")
	}

	html, _, err := httpGet(ctx, url, "https://www.xiaohongshu.com/", BrowserUA)
	if err != nil {
		return nil, fmt.Errorf("获取笔记页面失败: %w", err)
	}
	m := xhsStateRe.FindSubmatch(html)
	if m == nil {
		if strings.Contains(string(html), "当前笔记暂时无法浏览") || strings.Contains(string(html), "login") {
			return nil, fmt.Errorf("该笔记需要登录或访问受限：请使用小红书 App/网页的完整分享链接，或稍后重试")
		}
		return nil, fmt.Errorf("页面中未找到笔记数据（可能需要登录或链接不完整）")
	}
	// 笔记不存在/已删除/token 失效：页面仍带 INITIAL_STATE 骨架但无笔记内容
	if strings.Contains(string(html), "你访问的页面不见了") {
		return nil, fmt.Errorf("笔记不存在、已删除或链接 token 已失效——请从小红书 App 重新分享获取最新链接")
	}
	// XHS 的 INITIAL_STATE 含 undefined 字面量，替换为 null 后才是合法 JSON
	fixed := strings.ReplaceAll(string(m[1]), "undefined", "null")
	state := gjson.Parse(fixed)

	current := state.Get("note.currentNoteId").String()
	if current == "" {
		current = id
	}
	note := state.Get(fmt.Sprintf("note.noteDetailMap.%s.note", current))
	if !note.Exists() {
		// 兜底：取 noteDetailMap 中第一个 note
		for _, v := range state.Get("note.noteDetailMap").Map() {
			if n := v.Get("note"); n.Exists() {
				note = n
				break
			}
		}
	}
	if !note.Exists() {
		return nil, fmt.Errorf("笔记数据解析失败（可能需要登录）")
	}
	noteType := strings.ToLower(note.Get("type").String())
	if noteType == "video" {
		return nil, ErrVideoNotSupported
	}

	title := strings.TrimSpace(note.Get("title").String())
	if title == "" {
		title = strings.TrimSpace(note.Get("desc").String())
	}
	if len([]rune(title)) > 40 {
		title = string([]rune(title)[:40])
	}
	author := note.Get("user.nickname").String()

	var imgURLs []string
	for _, it := range note.Get("imageList").Array() {
		u := it.Get("urlDefault").String()
		if u == "" {
			// infoList 中通常最后一项为原图尺寸
			list := it.Get("infoList").Array()
			for i := len(list) - 1; i >= 0; i-- {
				if v := list[i].Get("url").String(); v != "" {
					u = v
					break
				}
			}
		}
		if u != "" {
			imgURLs = append(imgURLs, u)
		}
	}
	if len(imgURLs) == 0 {
		return nil, ErrNoImages
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	post := &Post{Platform: "xhs", ID: id, Title: title, Author: author, Dir: outDir}
	for i, u := range imgURLs {
		if ctx.Err() != nil {
			break
		}
		p, err := downloadFile(ctx, u, "https://www.xiaohongshu.com/", BrowserUA, outDir, i+1, imgExtFromURL(u))
		if err != nil {
			continue
		}
		post.Files = append(post.Files, p)
	}
	post.Count = len(post.Files)
	if post.Count == 0 {
		return nil, fmt.Errorf("图片下载全部失败（可能触发风控，请稍后重试或配置 Cookie）")
	}
	post.AudioURL, post.AudioName = xhsMusic(note)
	return post, nil
}

// xhsMusic 提取笔记 BGM（音频 URL 与曲目名）。部分笔记无 BGM，返回空串。
// URL 字段在不同版本页面中可能是 url / musicUrl / attachUrl，逐一尝试。
func xhsMusic(note gjson.Result) (audioURL, audioName string) {
	for _, f := range []string{"url", "musicUrl", "attachUrl"} {
		if u := strings.TrimSpace(note.Get("music." + f).String()); u != "" {
			audioURL = u
			break
		}
	}
	audioName = strings.TrimSpace(note.Get("music.name").String())
	if audioName == "" {
		audioName = strings.TrimSpace(note.Get("music.singer").String())
	}
	return audioURL, audioName
}

func imgExtFromURL(u string) string {
	p := strings.ToLower(trimQuery(u))
	switch {
	case strings.HasSuffix(p, ".png"):
		return ".png"
	case strings.HasSuffix(p, ".webp"):
		return ".webp"
	case strings.HasSuffix(p, ".jpg"), strings.HasSuffix(p, ".jpeg"):
		return ".jpg"
	default:
		return "" // 交给 Content-Type 推断
	}
}

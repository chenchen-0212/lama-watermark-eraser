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
	douyinIDRe      = regexp.MustCompile(`/(?:video|note|slides)/(\d+)`)
	douyinModalIDRe = regexp.MustCompile(`[?&]modal_id=(\d+)`)
	routerRe        = regexp.MustCompile(`window\._ROUTER_DATA\s*=\s*(\{.*?\})\s*</script>`)
)

// douyinAwemeID 从抖音链接中提取作品 ID。
// 支持 /video/{id}、/note/{id}、/slides/{id} 路径形态与 ?modal_id={id} 弹窗形态
// （后者常见于从抖音 Web 端复制的链接，modal_id 位于 RawQuery 而非 Path）。
func douyinAwemeID(u string) string {
	if m := douyinModalIDRe.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	if m := douyinIDRe.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return ""
}

// douyinEndpoint 单个解析端点及其所需 UA。
type douyinEndpoint struct {
	url string
	ua  string
}

// DownloadDouyin 下载抖音图文。
// 解析策略（无签名方案）：优先请求新版桌面页 www.douyin.com/note|video/{id}
// （页面内含 window._ROUTER_DATA，但需要 ttwid 等 Cookie，未配置 Cookie 时可能被风控拦截），
// 失败后回落旧版 iesdouyin.com/share/* 分享页。
func DownloadDouyin(ctx context.Context, url, outDir string) (*Post, error) {
	if isDouyinVideoURL(url) {
		return nil, ErrVideoNotSupported
	}
	id := douyinAwemeID(url)
	if id == "" {
		return nil, fmt.Errorf("无法从抖音链接解析作品 ID，请使用作品详情页链接（支持 /note/{id}、/video/{id} 或 ?modal_id={id} 形态）")
	}

	endpoints := []douyinEndpoint{
		// 新版桌面页（需 Cookie + 桌面 UA）
		{url: "https://www.douyin.com/note/" + id, ua: BrowserUA},
		{url: "https://www.douyin.com/video/" + id, ua: BrowserUA},
		// 旧版分享页（移动 UA，已部分失效，保留兜底）
		{url: "https://www.iesdouyin.com/share/note/" + id, ua: MobileUA},
		{url: "https://www.iesdouyin.com/share/slides/" + id, ua: MobileUA},
		{url: "https://www.iesdouyin.com/share/video/" + id, ua: MobileUA},
	}

	var item gjson.Result
	for _, ep := range endpoints {
		if ctx.Err() != nil {
			break
		}
		html, _, err := httpGet(ctx, ep.url, "https://www.douyin.com/", ep.ua)
		if err != nil {
			continue
		}
		m := routerRe.FindSubmatch(html)
		if m == nil {
			continue
		}
		item = findDouyinItem(gjson.Parse(string(m[1])), id)
		if item.Exists() {
			break
		}
	}
	if !item.Exists() {
		msg := "抖音页面未能返回作品数据（可能被风控拦截或接口变更）。" +
			"建议：① 点击初始页「抖音 Cookie」按指引配置浏览器登录 Cookie 后重试；" +
			"② 或直接保存图片后使用「本地文件夹」模式去水印"
		if PlatformCookie("douyin") == "" {
			msg = "抖音页面未能返回作品数据（当前未配置 Cookie，容易被风控拦截）。" +
				"建议：点击初始页「抖音 Cookie」按钮，按指引配置浏览器登录 Cookie 后重试；" +
				"或直接保存图片后使用「本地文件夹」模式去水印"
		}
		return nil, fmt.Errorf("%s", msg)
	}

	author := item.Get("author.nickname").String()
	desc := strings.TrimSpace(item.Get("desc").String())
	title := desc
	if len([]rune(title)) > 40 {
		title = string([]rune(title)[:40])
	}

	images := item.Get("images").Array()
	var imgURLs []string
	for _, im := range images {
		if u := firstURL(im.Get("url_list")); u != "" {
			imgURLs = append(imgURLs, u)
		}
	}
	if len(imgURLs) == 0 {
		// 无 images 且有视频 → 视频内容
		if item.Get("video.play_addr.url_list").Exists() || item.Get("video").Exists() {
			return nil, ErrVideoNotSupported
		}
		return nil, ErrNoImages
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	post := &Post{Platform: "douyin", ID: id, Title: title, Author: author, Dir: outDir}
	for i, u := range imgURLs {
		if ctx.Err() != nil {
			break
		}
		p, err := downloadFile(ctx, u, "https://www.douyin.com/", BrowserUA, outDir, i+1, "")
		if err != nil {
			continue
		}
		post.Files = append(post.Files, p)
	}
	post.Count = len(post.Files)
	if post.Count == 0 {
		return nil, fmt.Errorf("图片下载全部失败（可能触发风控，请稍后重试）")
	}
	return post, nil
}

// findDouyinItem 在 _ROUTER_DATA 的 loaderData 中定位 item_list 第一项。
func findDouyinItem(root gjson.Result, id string) gjson.Result {
	loader := root.Get("loaderData")
	if !loader.Exists() {
		return gjson.Result{}
	}
	// 优先精确键。新版桌面页键名形如 "note_(id)/page"（id 带括号），
	// 旧版分享页为 "note_{id}/page"（无括号），两种都试。
	for _, prefix := range []string{
		"note_(" + id + ")/page",
		"note_" + id + "/page",
		"video_(" + id + ")/page",
		"video_" + id + "/page",
		"slides_" + id + "/page",
	} {
		if v := loader.Get(fmt.Sprintf(`"%s".videoInfoRes.item_list.0`, prefix)); v.Exists() {
			return v
		}
	}
	// 兜底：遍历所有键
	for _, v := range loader.Map() {
		if item := v.Get("videoInfoRes.item_list.0"); item.Exists() {
			return item
		}
		if item := v.Get("item_list.0"); item.Exists() {
			return item
		}
	}
	return gjson.Result{}
}

// firstURL 取 url_list 数组第一个非空地址。
func firstURL(list gjson.Result) string {
	for _, u := range list.Array() {
		if s := u.String(); s != "" {
			return s
		}
	}
	return ""
}

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
	douyinIDRe = regexp.MustCompile(`/(?:video|note|slides)/(\d+)`)
	routerRe   = regexp.MustCompile(`window\._ROUTER_DATA\s*=\s*(\{.*?\})\s*</script>`)
)

// DownloadDouyin 下载抖音图文（无签名方案：iesdouyin 分享页 _ROUTER_DATA 解析）。
// 注：抖音风控较强，分享页接口失效时会返回可读错误提示。
func DownloadDouyin(ctx context.Context, url, outDir string) (*Post, error) {
	if isDouyinVideoURL(url) {
		return nil, ErrVideoNotSupported
	}
	id := ""
	if m := douyinIDRe.FindStringSubmatch(url); m != nil {
		id = m[1]
	}
	if id == "" {
		return nil, fmt.Errorf("无法从抖音链接解析作品 ID，请使用作品详情页链接")
	}

	var item gjson.Result
	// 依序尝试两个分享页端点（图文优先 note 端点）
	for _, u := range []string{
		"https://www.iesdouyin.com/share/note/" + id,
		"https://www.iesdouyin.com/share/slides/" + id,
		"https://www.iesdouyin.com/share/video/" + id,
	} {
		if ctx.Err() != nil {
			break
		}
		html, _, err := httpGet(ctx, u, "https://www.douyin.com/", MobileUA)
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
		return nil, fmt.Errorf("抖音分享页接口未能返回作品数据（风控或接口变更）。" +
			"建议：直接保存图片后使用「本地文件夹」模式去水印")
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
	// 优先精确键 note_{id}/page 或 video_{id}/page
	for _, prefix := range []string{"note_" + id + "/page", "video_" + id + "/page", "slides_" + id + "/page"} {
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

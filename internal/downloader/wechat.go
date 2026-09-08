package downloader

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	wechatDataSrcRe = regexp.MustCompile(`(?:data-src|src)="(https?://mmbiz\.qpic\.cn[^"]+)"`)
	wechatTitleRe   = regexp.MustCompile(`<meta property="og:title" content="([^"]*)"`)
	wechatH1Re      = regexp.MustCompile(`(?s)<h1[^>]*id="activity-name"[^>]*>(.*?)</h1>`)
	wechatWxFmtRe   = regexp.MustCompile(`wx_fmt=(\w+)`)
	wechatTagRe     = regexp.MustCompile(`<[^>]+>`)
)

// DownloadWeChat 下载公众号文章内的图片（零签名，纯 HTML 解析）。
func DownloadWeChat(ctx context.Context, url, outDir string) (*Post, error) {
	html, finalURL, err := httpGet(ctx, url, "", BrowserUA)
	if err != nil {
		return nil, fmt.Errorf("获取文章失败: %w", err)
	}
	_ = finalURL
	title := extractWeChatTitle(string(html))
	id := wechatID(url)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	imgURLs := filterWeChatImages(wechatDataSrcRe.FindAllStringSubmatch(string(html), -1))
	if len(imgURLs) == 0 {
		return nil, ErrNoImages
	}
	post := &Post{Platform: "wechat", ID: id, Title: title, Dir: outDir}
	for i, u := range imgURLs {
		if ctx.Err() != nil {
			break
		}
		ext := wechatExt(u)
		p, err := downloadFile(ctx, u, "https://mp.weixin.qq.com/", BrowserUA, outDir, i+1, ext)
		if err != nil {
			continue // 单张失败跳过
		}
		post.Files = append(post.Files, p)
	}
	post.Count = len(post.Files)
	if post.Count == 0 {
		return nil, ErrNoImages
	}
	return post, nil
}

func extractWeChatTitle(html string) string {
	if m := wechatTitleRe.FindStringSubmatch(html); m != nil {
		return strings.TrimSpace(m[1])
	}
	if m := wechatH1Re.FindStringSubmatch(html); m != nil {
		return strings.TrimSpace(wechatTagRe.ReplaceAllString(m[1], ""))
	}
	return "公众号文章"
}

func wechatID(u string) string {
	if i := strings.LastIndex(strings.Split(u, "?")[0], "/"); i >= 0 {
		id := strings.Split(u, "?")[0][i+1:]
		if id != "" {
			return id
		}
	}
	return hashString(u)
}

// filterWeChatImages 过滤出静态图片（跳过动图/矢量图/小图标）。
func filterWeChatImages(matches [][]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range matches {
		u := htmlUnescape(m[1])
		if seen[u] {
			continue
		}
		l := strings.ToLower(u)
		if strings.Contains(l, "wx_fmt=gif") || strings.Contains(l, "wx_fmt=svg") {
			continue
		}
		// 过滤明显的小图（公众号表情/装饰图标常带 wx_fmt=png 且有 resize 参数，保持简单：全部保留）
		seen[u] = true
		out = append(out, u)
	}
	return out
}

func wechatExt(u string) string {
	m := wechatWxFmtRe.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	switch strings.ToLower(m[1]) {
	case "jpeg", "jpg":
		return ".jpg"
	case "png":
		return ".png"
	case "bmp":
		return ".bmp"
	case "webp":
		return ".webp"
	default:
		return ""
	}
}

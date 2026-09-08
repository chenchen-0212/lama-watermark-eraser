package downloader

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// DetectPlatform 识别平台并规范化 URL；抖音短链/小红书短链先展开。
// 返回 (platform, cleanURL, error)。
func DetectPlatform(rawURL string) (platform, clean string, err error) {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return "", "", ErrUnsupportedPlatform
	}
	if !strings.Contains(u, "://") {
		u = "https://" + u
	}
	if _, e := url.Parse(u); e != nil {
		return "", "", fmt.Errorf("无效链接: %w", e)
	}
	// 短链展开
	if strings.Contains(u, "v.douyin.com") || strings.Contains(u, "xhslink.com") {
		final, e := resolveRedirect(u)
		if e != nil {
			return "", "", fmt.Errorf("短链接解析失败: %w", e)
		}
		u = final
	}
	switch {
	case strings.Contains(u, "mp.weixin.qq.com"):
		return "wechat", u, nil
	case strings.Contains(u, "xiaohongshu.com"):
		return "xhs", u, nil
	case strings.Contains(u, "douyin.com"):
		if isDouyinVideoURL(u) {
			return "", "", ErrVideoNotSupported
		}
		return "douyin", u, nil
	default:
		return "", "", ErrUnsupportedPlatform
	}
}

// Download 统一入口：识别平台并下载图文图片。
func Download(ctx context.Context, rawURL, outDir string) (*Post, error) {
	platform, clean, err := DetectPlatform(rawURL)
	if err != nil {
		return nil, err
	}
	switch platform {
	case "wechat":
		return DownloadWeChat(ctx, clean, outDir)
	case "xhs":
		return DownloadXHS(ctx, clean, outDir)
	case "douyin":
		return DownloadDouyin(ctx, clean, outDir)
	default:
		return nil, ErrUnsupportedPlatform
	}
}

// isDouyinVideoURL 判断是否为明确的视频详情页链接（/video/{id}）。
// 注意：?modal_id={id} 形态的 ID 在查询串中、无法从 URL 判断内容类型，
// 因此不在此拦截——交由 DownloadDouyin 解析后按实际数据判定
//（图文正常下载，视频内容因无 images 返回 ErrVideoNotSupported）。
func isDouyinVideoURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	return strings.Contains(p.Path, "/video/")
}

// resolveRedirect 跟随重定向取最终 URL（不下载 body）。
func resolveRedirect(u string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := newRequest(ctx, u, "", BrowserUA)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.Request != nil && resp.Request.URL != nil {
		return resp.Request.URL.String(), nil
	}
	return u, nil
}

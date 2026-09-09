package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BrowserUA 桌面浏览器 UA。
const BrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// MobileUA 移动端 UA（抖音分享页使用）。
const MobileUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"

var client = &http.Client{
	Timeout: 60 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 8 {
			return fmt.Errorf("重定向次数过多")
		}
		return nil
	},
}

// 平台级 Cookie（可选兜底：小红书/抖音遇到风控时由用户提供）。
var platformCookies = map[string]string{}

// SetPlatformCookie 设置某平台的原始 Cookie 头（形如 "a=1; b=2"）。
func SetPlatformCookie(platform, raw string) { platformCookies[platform] = raw }

// PlatformCookie 读取平台 Cookie。
func PlatformCookie(platform string) string { return platformCookies[platform] }

// platformFromURL 根据 URL 主机名映射平台标识，用于按平台注入 Cookie。
// 未匹配任何已知平台时返回空串（不注入）。
func platformFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case strings.Contains(host, "douyin"),
		strings.Contains(host, "zjcdn"),   // 抖音图片 CDN
		strings.Contains(host, "byteimg"), // 抖音图片 CDN
		strings.Contains(host, "douyinpic"),
		strings.Contains(host, "douyinvod"),
		strings.Contains(host, "pstatp"): // 抖音音乐/媒体 CDN（字节跳动）
		return "douyin"
	case strings.Contains(host, "xiaohongshu"),
		strings.Contains(host, "xhscdn"):
		return "xhs"
	case strings.Contains(host, "weixin"),
		strings.Contains(host, "qq.com"),
		strings.Contains(host, "qpic.cn"):
		return "wechat"
	default:
		return ""
	}
}

func newRequest(ctx context.Context, url, referer, ua string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if ua == "" {
		ua = BrowserUA
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	// 按平台注入用户配置的 Cookie（抖音等平台风控必需 ttwid 等凭据）
	if p := platformFromURL(url); p != "" {
		if c := PlatformCookie(p); c != "" {
			req.Header.Set("Cookie", c)
		}
	}
	return req, nil
}

// httpGet GET 页面内容，返回 (body, 最终URL, error)。自动跟随重定向。
func httpGet(ctx context.Context, url, referer, ua string) ([]byte, string, error) {
	req, err := newRequest(ctx, url, referer, ua)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", err
	}
	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return data, final, nil
}

// sniffExt 根据 Content-Type 推断扩展名。
func sniffExt(contentType string) string {
	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch ct {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	default:
		return ""
	}
}

// audioExts 可接受的音频文件扩展名（小写，含点）。
var audioExts = map[string]bool{
	".mp3": true, ".m4a": true, ".aac": true, ".wav": true, ".flac": true, ".ogg": true,
}

// ensureAudioExt 文件名无音频扩展名时补 .mp3（用户明确要求 mp3 交付形态）。
// 已带合法音频扩展名（如 .m4a）时保持不变。
func ensureAudioExt(name string) string {
	if audioExts[strings.ToLower(filepath.Ext(name))] {
		return name
	}
	return name + ".mp3"
}

// audioRequestMeta 按音频 CDN 主机选择 Referer 与 UA（复用浏览器指纹降低风控概率）。
func audioRequestMeta(u string) (referer, ua string) {
	ua = BrowserUA
	switch {
	case strings.Contains(u, "xiaohongshu"), strings.Contains(u, "xhscdn"):
		return "https://www.xiaohongshu.com/", ua
	case strings.Contains(u, "douyin"), strings.Contains(u, "douyinvod"),
		strings.Contains(u, "dycdn"), strings.Contains(u, "pstatp"),
		strings.Contains(u, "zjcdn"), strings.Contains(u, "byteimg"):
		return "https://www.douyin.com/", ua
	default:
		return "", ua
	}
}

// SaveAudio 下载音频文件到 dstDir（自定义文件名，缺省/非法时兜底 bgm.mp3）。
// 与图片下载不同：不按序号命名、扩展名显式控制，避免音频被误存为 .jpg。
func SaveAudio(ctx context.Context, audioURL, dstDir, filename string) (string, error) {
	audioURL = strings.TrimSpace(audioURL)
	if audioURL == "" {
		return "", fmt.Errorf("音频地址为空（该帖子可能没有 BGM）")
	}
	if !strings.HasPrefix(audioURL, "http://") && !strings.HasPrefix(audioURL, "https://") {
		return "", fmt.Errorf("音频地址无效: %s", audioURL)
	}
	if dstDir == "" {
		return "", fmt.Errorf("保存目录为空")
	}
	name := sanitizeFilename(filename)
	if name == "" {
		name = "bgm"
	}
	name = ensureAudioExt(name)

	referer, ua := audioRequestMeta(audioURL)
	req, err := newRequest(ctx, audioURL, referer, ua)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d 音频下载失败: %s", resp.StatusCode, audioURL)
	}
	dst := filepath.Join(dstDir, name)
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

// downloadFile 下载图片到 dstDir，按序号命名；ext 为空时按 Content-Type 推断。
// 返回实际文件路径。
func downloadFile(ctx context.Context, url, referer, ua, dstDir string, idx int, extHint string) (string, error) {
	req, err := newRequest(ctx, url, referer, ua)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d 下载失败: %s", resp.StatusCode, url)
	}
	ext := extHint
	if ext == "" {
		ext = sniffExt(resp.Header.Get("Content-Type"))
	}
	if ext == "" {
		ext = ".jpg"
	}
	dst := fmt.Sprintf("%s/%02d%s", dstDir, idx, ext)
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

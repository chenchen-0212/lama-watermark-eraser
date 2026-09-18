package downloader

import (
	"bytes"
	"context"
	"errors"
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
		strings.Contains(host, "douyinstatic"), // 音乐对象存储（ies-music）
		strings.Contains(host, "pstatp"),       // 抖音音乐/媒体 CDN（字节跳动）
		strings.Contains(host, "snssdk"),       // 字节系 CDN 泛域名
		strings.Contains(host, "amemv"),        // 字节系 CDN 泛域名
		strings.Contains(host, "muscdn"):       // 字节音乐 CDN
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

// sniffAudioExt 按音频 Content-Type 推断扩展名（抖音实测返回 audio/mp4 即 m4a）。
func sniffAudioExt(contentType string) string {
	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch ct {
	case "audio/mp4", "audio/x-m4a", "audio/m4a":
		return ".m4a"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/aac":
		return ".aac"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "audio/flac":
		return ".flac"
	default:
		return ""
	}
}

// resolveAudioName 决定音频最终文件名：用户显式音频扩展 > URL 路径扩展 >
// Content-Type 推断 > 兜底 .mp3。用户名总是保留（扩展缺失时补）。
func resolveAudioName(name, audioURL, contentType string) string {
	if audioExts[strings.ToLower(filepath.Ext(name))] {
		return name
	}
	if e := strings.ToLower(filepath.Ext(trimQuery(audioURL))); audioExts[e] {
		return name + e
	}
	if e := sniffAudioExt(contentType); e != "" {
		return name + e
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
		strings.Contains(u, "douyinstatic"), strings.Contains(u, "dycdn"),
		strings.Contains(u, "pstatp"), strings.Contains(u, "zjcdn"),
		strings.Contains(u, "byteimg"), strings.Contains(u, "snssdk"),
		strings.Contains(u, "amemv"), strings.Contains(u, "muscdn"):
		return "https://www.douyin.com/", ua
	default:
		return "", ua
	}
}

// audioMagic 音频容器魔数（前缀 → 说明）。用于识别响应体是否为真实音频：
// 社媒 CDN 被限流时多返回 HTML 风控页或 JSON 错误体，若不加校验会被
// 原样存成 .mp3，产出无法播放的坏文件且对用户无任何提示。
var audioMagic = []struct {
	prefix []byte
	label  string
}{
	{[]byte("ID3"), "MP3(ID3)"},
	{[]byte{0xFF, 0xFB}, "MP3"},
	{[]byte{0xFF, 0xF3}, "MP3"},
	{[]byte{0xFF, 0xF2}, "MP3"},
	{[]byte("ftyp"), "M4A/MP4"}, // 偏移 4 字节处
	{[]byte("OggS"), "OGG"},
	{[]byte("fLaC"), "FLAC"},
	{[]byte("RIFF"), "WAV"},
	{[]byte("ADTS"), "AAC"},
	{[]byte{0xFF, 0xF1}, "AAC"}, // ADTS 无 syncword 时
}

// looksLikeAudio 判断响应首字节是否匹配已知音频容器。
// head 为响应体开头若干字节（至少 12 字节时才能覆盖 ftyp 分支）。
func looksLikeAudio(head []byte) bool {
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		return true // ISO BMFF：m4a/mp4
	}
	for _, m := range audioMagic {
		if bytes.HasPrefix(head, m.prefix) {
			return true
		}
	}
	return false
}

// maxAudioTries 单个音频地址的最大尝试次数（首次 + 续传重试）。
const maxAudioTries = 3

// SaveAudio 下载音频文件到 dstDir（自定义文件名，缺省/非法时兜底 bgm）。
// 扩展名按「用户显式指定 > URL 路径 > Content-Type > .mp3」解析，避免把
// m4a 音频误存为 .mp3。
//
// 落盘前用魔数校验响应体：社媒 CDN 限流时返回的 HTML/JSON 错误页会被
// 识别并拒绝，不会静默产出无法播放的坏文件。
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

	referer, ua := audioRequestMeta(audioURL)
	var lastErr error
	for try := 0; try < maxAudioTries; try++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		dst, err := saveAudioOnce(ctx, audioURL, dstDir, name, referer, ua)
		if err == nil {
			return dst, nil
		}
		lastErr = err
		// 风控/内容异常类错误重试无意义，直接返回
		if errors.Is(err, errNotAudio) {
			return "", err
		}
	}
	return "", lastErr
}

// errNotAudio 响应体不是音频（多为风控页或错误 JSON）。
var errNotAudio = errors.New("响应内容不是音频")

// saveAudioOnce 单次下载尝试，失败时返回可重试的错误。
func saveAudioOnce(ctx context.Context, audioURL, dstDir, name, referer, ua string) (string, error) {
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

	// 魔数校验：读满头部再决定是否继续落盘
	head := make([]byte, 16)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	head = head[:n]
	if !looksLikeAudio(head) {
		ct := resp.Header.Get("Content-Type")
		return "", fmt.Errorf("%w（Content-Type=%s，可能是风控拦截或链接已失效）: %s",
			errNotAudio, ct, audioURL)
	}

	dst := filepath.Join(dstDir, resolveAudioName(name, audioURL, resp.Header.Get("Content-Type")))
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(head); err != nil {
		f.Close()
		os.Remove(tmp)
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

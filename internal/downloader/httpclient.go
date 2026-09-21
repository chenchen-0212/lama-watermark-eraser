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
	{[]byte("ADIF"), "AAC(ADIF)"},
	{[]byte("#!AMR"), "AMR"},
	{[]byte{0x30, 0x26, 0xB2, 0x75}, "WMA/ASF"},
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

// probeReadBytes 探测时实际读取的字节数：Range 只取头部，等价于一次很小的
// 分段请求，不会把整个音频拉下来。
const probeReadBytes = 64

// ProbeAudio 轻量探测候选音频地址是否仍然可用（可达且内容确为音频）。
//
// 与下载的区别：不落盘、不重试、不分级——只回答「这个地址现在能不能用」。
// 用途：下载任务完成时过滤掉已过期的 BGM 直链（抖音图文帖的音频是带时效的
// 对象存储地址，下载到用户点「去框选」可能相隔数小时），避免前端把一个点了
// 必然失败的「下载BGM」按钮显示出来。
//
// 判定标准与下载链路一致：HTTP 2xx/206 且响应体首字节匹配音频魔数——
// 风控常返回 200 + HTML 页面，只看状态码会误判为可用。
// 返回 nil 表示可用；非 nil 为不可用原因（调用方通常只需布尔语义）。
func ProbeAudio(ctx context.Context, audioURL string) error {
	u := trimSpace(audioURL)
	if u == "" {
		return fmt.Errorf("空地址")
	}
	referer, ua := audioRequestMeta(u)
	req, err := newRequest(ctx, u, referer, ua)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", probeReadBytes-1))

	resp, err := client.Do(req)
	if err != nil {
		if isCanceled(err) {
			return ctx.Err()
		}
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	// 200（忽略 Range 全量返回）与 206（正常分段）都算可达
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, probeReadBytes))
	if err != nil && !isEOF(err) {
		if isCanceled(err) {
			return ctx.Err()
		}
		return err
	}
	if len(head) == 0 {
		return fmt.Errorf("响应体为空")
	}
	if !looksLikeAudio(head) {
		return fmt.Errorf("响应内容不是音频（Content-Type=%s）", resp.Header.Get("Content-Type"))
	}
	return nil
}

// errNotAudio 响应体不是音频（多为风控页或错误 JSON）。
// 作为哨兵保留：结构化错误把它挂在 Unwrap 链上，errors.Is 仍然可用。
var errNotAudio = errors.New("响应内容不是音频")

// audioProbeBytes 落盘前用于魔数校验的头部字节数。
// 12 字节即可覆盖 ISO BMFF 的 ftyp（偏移 4）；取 32 留出余量。
const audioProbeBytes = 32

// audioBackoffBase 同一地址重试前的退避基准。置零可让测试免于等待。
var audioBackoffBase = 300 * time.Millisecond

// audioAttempt 一次音频下载尝试的全部输入。
type audioAttempt struct {
	Candidate AudioCandidate
	Index     int    // 候选在链中的序号（0 起），仅用于错误报告与日志
	Attempt   int    // 该候选的第几次尝试（1 起）
	DstDir    string // 目标目录
	Name      string // 已清洗的基名（不含扩展名）
}

// SaveAudio 下载音频文件到 dstDir（自定义文件名，缺省/非法时兜底 bgm）。
//
// 兼容入口：无候选来源信息时使用，内部转交 SaveAudioCandidate。
func SaveAudio(ctx context.Context, audioURL, dstDir, filename string) (string, error) {
	return SaveAudioCandidate(ctx, AudioCandidate{URL: audioURL}, 0, dstDir, filename)
}

// SaveAudioCandidate 下载单个候选地址，按错误类型决定是否对同一地址重试。
//
// 与旧实现的关键差异：失败不再是裸 error，而是 *AudioDownloadError——上层据此
// 判断「重试当前地址 / 换下一个候选 / 直接放弃」。不可恢复的错误（404、内容非音频、
// 地址非法）只尝试一次，不浪费请求次数；可恢复的网络类错误按退避重试。
//
// 扩展名按「用户显式指定 > URL 路径 > Content-Type > .mp3」解析，避免把 m4a
// 误存为 .mp3。落盘前用魔数校验：社媒 CDN 限流时返回的 HTML/JSON 错误页会被
// 识别并拒绝，不会静默产出无法播放的坏文件。
func SaveAudioCandidate(ctx context.Context, cand AudioCandidate, index int, dstDir, filename string) (string, error) {
	audioURL := strings.TrimSpace(cand.URL)
	if audioURL == "" {
		return "", newAudioError(AudioErrInvalidURL, 0, "", cand.Source, index, 1,
			fmt.Errorf("音频地址为空（该帖子可能没有 BGM）"))
	}
	if !strings.HasPrefix(audioURL, "http://") && !strings.HasPrefix(audioURL, "https://") {
		return "", newAudioError(AudioErrInvalidURL, 0, audioURL, cand.Source, index, 1, nil)
	}
	if dstDir == "" {
		return "", fmt.Errorf("保存目录为空")
	}
	name := sanitizeFilename(filename)
	if name == "" {
		name = "bgm"
	}
	cand.URL = audioURL

	var lastErr error
	for attempt := 1; attempt <= audioAttemptLimit; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		at := audioAttempt{Candidate: cand, Index: index, Attempt: attempt, DstDir: dstDir, Name: name}
		dst, err := saveAudioOnce(ctx, at)
		if err == nil {
			return dst, nil
		}
		lastErr = err
		audioLog(fmt.Sprintf(
			"audio download attempt candidate_index=%d source=%s attempt=%d host=%s status=%d error_kind=%s retryable=%v",
			index, sourceLabel(cand.Source), attempt, hostOf(audioURL),
			statusOf(err), kindOf(err), audioErrorRetryable(err)))
		// 上下文取消，或本地致命错误（磁盘/权限，换地址也没用）：立即放弃
		if ctx.Err() != nil || kindOf(err) == "" {
			return "", err
		}
		// 该错误类型已用完重试预算（如 404、非音频内容）：交给上层换候选
		if !audioErrorRetryable(err) {
			return "", err
		}
		if d := audioRetryBackoff(kindOf(err), attempt); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
	}
	return "", lastErr
}

// audioErrorRetryable 结构化错误是否还允许对同一地址再试一次。
func audioErrorRetryable(err error) bool {
	var ae *AudioDownloadError
	if errors.As(err, &ae) {
		return ae.Retryable
	}
	return false
}

// audioRetryBackoff 同一地址重试前的退避时长。限流与瞬时风控给更长等待。
func audioRetryBackoff(kind AudioErrorKind, attempt int) time.Duration {
	if audioBackoffBase <= 0 {
		return 0
	}
	base := audioBackoffBase
	if kind == AudioErr429 {
		base = audioBackoffBase * 4
	}
	if attempt > 1 {
		base *= time.Duration(attempt)
	}
	return base
}

// saveAudioOnce 单次 HTTP 尝试：一次 GET 同时完成「探测 + 下载」。
//
// 先读前 audioProbeBytes 字节做魔数校验，通过后继续消费同一个 Response.Body
// 写盘——不发 Range 预探测，也不为同一地址发第二次请求。
// 只有全部数据写入成功才把 .part 改名为最终文件，因此试听逻辑永远不会读到半成品。
func saveAudioOnce(ctx context.Context, at audioAttempt) (string, error) {
	audioURL := at.Candidate.URL
	fail := func(kind AudioErrorKind, status int, err error) (string, error) {
		return "", newAudioError(kind, status, audioURL, at.Candidate.Source, at.Index, at.Attempt, err)
	}

	referer, ua := audioRequestMeta(audioURL)
	if at.Candidate.Referer != "" {
		referer = at.Candidate.Referer
	}
	if at.Candidate.UA != "" {
		ua = at.Candidate.UA
	}

	req, err := newRequest(ctx, audioURL, referer, ua)
	if err != nil {
		return fail(AudioErrInvalidURL, 0, err)
	}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if isCanceled(err) {
			return "", ctx.Err()
		}
		return fail(classifyNetError(err), 0, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fail(classifyHTTPStatus(resp.StatusCode), resp.StatusCode, nil)
	}
	contentType := resp.Header.Get("Content-Type")

	// 魔数校验：读满头部再决定是否继续落盘
	head := make([]byte, audioProbeBytes)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && !isEOF(err) {
		if isCanceled(err) {
			return "", ctx.Err()
		}
		return fail(classifyNetError(err), resp.StatusCode, err)
	}
	head = head[:n]
	if n == 0 {
		return fail(AudioErrEmptyBody, resp.StatusCode, nil)
	}
	if !looksLikeAudio(head) {
		// Content-Type 不足为凭：风控页常带看似正常的 audio/* 头，
		// 最终判定以魔数为准。
		return fail(AudioErrNotAudio, resp.StatusCode,
			fmt.Errorf("%w（Content-Type=%s，可能是风控拦截或链接已失效）", errNotAudio, contentType))
	}

	dst := filepath.Join(at.DstDir, resolveAudioName(at.Name, audioURL, contentType))
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		// 本地 IO 失败（磁盘/权限）：换地址或重试都无意义，
		// 返回无分类错误，由上层识别为致命并直接放弃。
		return "", err
	}
	if _, err := f.Write(head); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	written := int64(len(head))
	copied, err := io.Copy(f, resp.Body)
	written += copied
	if err != nil {
		f.Close()
		os.Remove(tmp)
		if isCanceled(err) {
			return "", ctx.Err()
		}
		return fail(classifyNetError(err), resp.StatusCode, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	// 内容写完才改名：.part 之下要么是完整文件，要么不存在
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}

	audioLog(fmt.Sprintf(
		"audio download success candidate_index=%d source=%s attempt=%d host=%s duration=%s size=%d format=%s",
		at.Index, sourceLabel(at.Candidate.Source), at.Attempt, hostOf(audioURL),
		time.Since(started).Round(time.Millisecond), written,
		strings.TrimPrefix(filepath.Ext(dst), ".")))
	return dst, nil
}

// sourceLabel 日志用的来源名（空值统一显示 unknown）。
func sourceLabel(src string) string {
	if trimSpace(src) == "" {
		return SourceUnknown
	}
	return src
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

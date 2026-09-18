package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
// （图文正常下载，视频内容因无 images 返回 ErrVideoNotSupported）。
func isDouyinVideoURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	return strings.Contains(p.Path, "/video/")
}

// probeAudio 探测单个音频地址是否返回真实音频（只读响应头与前 16 字节）。
func probeAudio(ctx context.Context, audioURL string) error {
	if strings.TrimSpace(audioURL) == "" {
		return fmt.Errorf("音频地址为空")
	}
	if !strings.HasPrefix(audioURL, "http://") && !strings.HasPrefix(audioURL, "https://") {
		return fmt.Errorf("音频地址无效: %s", audioURL)
	}
	referer, ua := audioRequestMeta(audioURL)
	req, err := newRequest(ctx, audioURL, referer, ua)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-15")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 200（不支持 Range 时返回全量）与 206（部分内容）均可接受
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, audioURL)
	}
	head := make([]byte, 16)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	if !looksLikeAudio(head[:n]) {
		return fmt.Errorf("%w（Content-Type=%s）: %s", errNotAudio, resp.Header.Get("Content-Type"), audioURL)
	}
	return nil
}

// usableCandidates 过滤掉空白候选，返回真正可尝试的地址列表。
func usableCandidates(candidates []string) []string {
	out := make([]string, 0, len(candidates))
	for _, u := range candidates {
		if strings.TrimSpace(u) != "" {
			out = append(out, u)
		}
	}
	return out
}

// PickAudioSource 按候选链顺序尝试音频源，返回首个可用的真实音频地址。
//
// 存在的意义：社媒音频 CDN 的单个地址随时可能 403 或过期，而同一曲目在解析
// 结果中往往有多个备用地址。此前只取第一条，单点失败即整体失败。
//
// 采用「探测」而非「下载后判定」：探针只读 16 字节（配合 Range 请求），校验
// 通过即返回该地址交给后续完整下载，避免把整段音频读两遍。全部候选失败时
// 返回最后一个错误，供上层给出可操作的提示。
func PickAudioSource(ctx context.Context, candidates []string) (string, error) {
	list := usableCandidates(candidates)
	if len(list) == 0 {
		return "", fmt.Errorf("未解析到 BGM 音频地址（可能该帖无 BGM，或页面结构变更导致解析降级）")
	}
	var lastErr error
	for _, u := range list {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err := probeAudio(ctx, u); err != nil {
			lastErr = err
			continue
		}
		return u, nil
	}
	return "", fmt.Errorf("BGM 地址全部探测失败（%d 个候选，最后错误：%v）", len(list), lastErr)
}

// DownloadAudioWithFallback 依次尝试候选地址，返回首个下载成功的文件路径。
func DownloadAudioWithFallback(ctx context.Context, candidates []string, dstDir, filename string) (string, error) {
	list := usableCandidates(candidates)
	if len(list) == 0 {
		return "", fmt.Errorf("未解析到 BGM 音频地址（可能该帖无 BGM，或页面结构变更导致解析降级）")
	}
	var lastErr error
	for _, u := range list {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		saved, err := SaveAudio(ctx, u, dstDir, filename)
		if err == nil {
			return saved, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("BGM 下载失败（已尝试 %d 个地址，最后错误：%w）", len(list), lastErr)
}

// resolveRedirect 跟随重定向取最终 URL（不下载 body）。
//
// 抖音短链（v.douyin.com）的跳转目标与 UA 有关，且策略随时间变化（风控的
// 典型表现，2026-09-18 实测）：桌面 UA 下三条真实短链全部被 302 到
// `https://www.douyin.com` 首页——作品 ID 丢失，后续解析必然失败；移动 UA
// 则通常跳 iesdouyin.com/share/* 分享页（ID 保留）。
// 策略：桌面 UA 优先（保持既有行为），若最终 URL 已解析不出作品 ID，
// 自动换移动 UA 重试，取能保住 ID 的结果；两者都失败时返回桌面 UA 的
// 最终 URL（由上层给出含指引的错误）。
func resolveRedirect(u string) (string, error) {
	final, err := resolveRedirectWithUA(u, BrowserUA)
	if err != nil {
		return "", err
	}
	if !strings.Contains(u, "v.douyin.com") || douyinAwemeID(final) != "" {
		return final, nil
	}
	if alt, err2 := resolveRedirectWithUA(u, MobileUA); err2 == nil && douyinAwemeID(alt) != "" {
		return alt, nil
	}
	return final, nil
}

// resolveRedirectWithUA 以指定 UA 跟随重定向。
func resolveRedirectWithUA(u, ua string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := newRequest(ctx, u, "", ua)
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

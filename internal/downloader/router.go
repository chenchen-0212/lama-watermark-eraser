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
// （图文正常下载，视频内容因无 images 返回 ErrVideoNotSupported）。
func isDouyinVideoURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	return strings.Contains(p.Path, "/video/")
}

// usableAudioCandidates 过滤空地址并按等价标识去重。
//
// 去重不只比对字面量：同一 BGM 常被下发到多个 CDN 节点（host 与 sign 不同、
// path 相同），字面量不同却实为同一文件，重复尝试只是白费请求。
func usableAudioCandidates(cands []AudioCandidate) []AudioCandidate {
	out := make([]AudioCandidate, 0, len(cands))
	for _, c := range cands {
		if trimSpace(c.URL) == "" {
			continue
		}
		out = append(out, c)
	}
	return DedupCandidates(out)
}

// DownloadAudioWithFallback 依次尝试候选地址，返回首个下载成功的文件路径。
//
// 重试分两层，职责不重叠：
//   - 候选级：同一地址内由 SaveAudioCandidate 按错误类型决定重试次数
//     （404、非音频内容只试一次；网络类错误退避后重试）；
//   - 链级：本函数只在候选之间推进，某个候选判定为不可恢复即立刻换下一个。
//
// 与旧实现的关键差异：不再「整条链失败 → 睡一会 → 整条链重跑一遍」。
// 链级重跑改由上层在确认错误可恢复时发起，避免对必然失败的地址重复施压。
//
// 候选顺序先经 RankAudioCandidates 调整（历史成功来源前移），失败原因逐条记录。
func DownloadAudioWithFallback(ctx context.Context, candidates []AudioCandidate, dstDir, filename string) (string, error) {
	list := usableAudioCandidates(candidates)
	if len(list) == 0 {
		return "", fmt.Errorf("未解析到 BGM 音频地址（可能该帖无 BGM，或页面结构变更导致解析降级）")
	}
	list = RankAudioCandidates(list)

	chainKey := CandidateChainKey(candidateURLs(list))
	var failures []string
	var lastErr error
	for i, cand := range list {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		saved, err := SaveAudioCandidate(ctx, cand, i, dstDir, filename)
		if err == nil {
			recordAudioSuccess(chainKey, cand.Source)
			audioLog(fmt.Sprintf("audio download done candidate_count=%d used_index=%d used_source=%s",
				len(list), i, sourceLabel(cand.Source)))
			return saved, nil
		}
		recordAudioFailure(chainKey, cand.Source, kindOf(err))
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// 无分类错误 = 本地致命问题（磁盘/权限），换源无意义
		if kindOf(err) == "" {
			return "", err
		}
		failures = append(failures,
			fmt.Sprintf("candidate[%d] source=%s error=%s", i, sourceLabel(cand.Source), kindOf(err)))
		lastErr = err
	}
	audioLog(fmt.Sprintf("audio download failed candidate_count=%d detail=%s",
		len(list), strings.Join(failures, "; ")))
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

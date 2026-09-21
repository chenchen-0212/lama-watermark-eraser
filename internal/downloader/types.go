// Package downloader 纯 Go 社媒图文下载器（公众号 / 小红书 / 抖音）。
// 设计参考 zinan92/content-downloader 的能力边界，仅聚焦图文图片下载。
package downloader

import "errors"

// Post 下载结果。
type Post struct {
	Platform string   `json:"platform"` // wechat | xhs | douyin
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Author   string   `json:"author"`
	Dir      string   `json:"dir"`   // 图片本地目录
	Files    []string `json:"files"` // 已下载图片路径（按序）
	Count    int      `json:"count"`
	// AudioURL 帖子 BGM 音频源地址（无则空；不自动下载，由前端用户确认后经 DownloadBGM 保存）
	AudioURL string `json:"audioUrl"`
	// AudioName 平台提供的曲目名（用作展示/命名参考，可为空）
	AudioName string `json:"audioName"`
	// AudioCandidates BGM 候选地址链（按优先级排序，可能为空）。
	// 社媒音频 CDN 单条地址随时可能 403/过期，前端下载时逐条尝试直到成功。
	// 首项与 AudioURL 相同（AudioURL 保留以兼容既有前端契约）。
	AudioCandidates []string `json:"audioCandidates,omitempty"`
}

// SetAudio 设置 BGM 取源（仅地址，无来源信息）。
// 首项写入 AudioURL 保持兼容，全量写入 AudioCandidates。
// 入参会做 trim、去空、去重（保序）。新代码应优先用 SetAudioCandidates。
func (p *Post) SetAudio(name string, urls ...string) {
	p.AudioName = name
	seen := make(map[string]bool, len(urls))
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		u = trimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	p.AudioCandidates = out
	if len(out) > 0 {
		p.AudioURL = out[0]
	} else {
		p.AudioURL = ""
	}
	registerAudioChain(out, "", nil)
}

// SetAudioCandidates 设置带来源信息的 BGM 候选链（抖音 / 小红书统一走这里）。
//
// 对外仍只暴露 []string——AudioCandidates 是前端既有契约，不能改成对象数组，
// 否则前端的候选链校验会失效。候选链同时登记到进程内注册表：下载时前端只回传
// 地址列表，来源信息无法随参数往返，只能靠注册表恢复，供日志与排序使用。
func (p *Post) SetAudioCandidates(name, musicID string, cands ...AudioCandidate) {
	deduped := DedupCandidates(cands)
	urls := make([]string, 0, len(deduped))
	for _, c := range deduped {
		urls = append(urls, c.URL)
	}
	p.AudioName = name
	p.AudioCandidates = urls
	if len(urls) > 0 {
		p.AudioURL = urls[0]
	} else {
		p.AudioURL = ""
	}
	registerAudioChain(urls, musicID, deduped)
}

var (
	// ErrUnsupportedPlatform 不支持的平台。
	ErrUnsupportedPlatform = errors.New("不支持的平台链接（当前支持：微信公众号文章、小红书图文、抖音图文）")
	// ErrVideoNotSupported 视频内容拦截。
	ErrVideoNotSupported = errors.New("该链接是视频内容，暂不支持——请提供图文链接")
	// ErrNoImages 未找到图片。
	ErrNoImages = errors.New("未在该链接中找到可下载的图片")
)

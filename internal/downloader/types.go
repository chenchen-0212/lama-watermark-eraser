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
}

var (
	// ErrUnsupportedPlatform 不支持的平台。
	ErrUnsupportedPlatform = errors.New("不支持的平台链接（当前支持：微信公众号文章、小红书图文、抖音图文）")
	// ErrVideoNotSupported 视频内容拦截。
	ErrVideoNotSupported = errors.New("该链接是视频内容，暂不支持——请提供图文链接")
	// ErrNoImages 未找到图片。
	ErrNoImages = errors.New("未在该链接中找到可下载的图片")
)

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
	douyinIDRe      = regexp.MustCompile(`/(?:video|note|slides)/(\d+)`)
	douyinModalIDRe = regexp.MustCompile(`[?&]modal_id=(\d+)`)
	routerRe        = regexp.MustCompile(`window\._ROUTER_DATA\s*=\s*(\{.*?\})\s*</script>`)

	// musicDetailAPIBase music/detail 接口地址抽成变量：单测可注入本地
	// httptest，避免网络依赖。实测（2026-09-15）该接口无需 a_bogus 签名
	// 即可返回完整 music_info。
	musicDetailAPIBase = "https://www.douyin.com/aweme/v1/web/music/detail/"
)

// douyinAwemeID 从抖音链接中提取作品 ID。
// 支持 /video/{id}、/note/{id}、/slides/{id} 路径形态与 ?modal_id={id} 弹窗形态
// （后者常见于从抖音 Web 端复制的链接，modal_id 位于 RawQuery 而非 Path）。
func douyinAwemeID(u string) string {
	if m := douyinModalIDRe.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	if m := douyinIDRe.FindStringSubmatch(u); m != nil {
		return m[1]
	}
	return ""
}

// douyinEndpoint 单个解析端点及其所需 UA。
type douyinEndpoint struct {
	url string
	ua  string
}

// DownloadDouyin 下载抖音图文。
// 解析策略（无签名方案）：优先请求新版桌面页 www.douyin.com/note|video/{id}
// （页面内含 window._ROUTER_DATA，但需要 ttwid 等 Cookie，未配置 Cookie 时可能被风控拦截），
// 失败后回落旧版 iesdouyin.com/share/* 分享页。
func DownloadDouyin(ctx context.Context, url, outDir string) (*Post, error) {
	if isDouyinVideoURL(url) {
		return nil, ErrVideoNotSupported
	}
	id := douyinAwemeID(url)
	if id == "" {
		return nil, fmt.Errorf("无法从抖音链接解析作品 ID，请使用作品详情页链接（支持 /note/{id}、/video/{id} 或 ?modal_id={id} 形态）")
	}

	endpoints := []douyinEndpoint{
		// 新版桌面页（需 Cookie + 桌面 UA）
		{url: "https://www.douyin.com/note/" + id, ua: BrowserUA},
		{url: "https://www.douyin.com/video/" + id, ua: BrowserUA},
		// 旧版分享页（移动 UA，已部分失效，保留兜底）
		{url: "https://www.iesdouyin.com/share/note/" + id, ua: MobileUA},
		{url: "https://www.iesdouyin.com/share/slides/" + id, ua: MobileUA},
		{url: "https://www.iesdouyin.com/share/video/" + id, ua: MobileUA},
	}

	var item gjson.Result
	for _, ep := range endpoints {
		if ctx.Err() != nil {
			break
		}
		html, _, err := httpGet(ctx, ep.url, "https://www.douyin.com/", ep.ua)
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
		msg := "抖音页面未能返回作品数据（可能被风控拦截或接口变更）。" +
			"建议：① 点击初始页「抖音 Cookie」按指引配置浏览器登录 Cookie 后重试；" +
			"② 或直接保存图片后使用「本地文件夹」模式去水印"
		if PlatformCookie("douyin") == "" {
			msg = "抖音页面未能返回作品数据（当前未配置 Cookie，容易被风控拦截）。" +
				"建议：点击初始页「抖音 Cookie」按钮，按指引配置浏览器登录 Cookie 后重试；" +
				"或直接保存图片后使用「本地文件夹」模式去水印"
		}
		return nil, fmt.Errorf("%s", msg)
	}

	author := item.Get("author.nickname").String()
	desc := strings.TrimSpace(item.Get("desc").String())
	title := desc
	if len([]rune(title)) > 40 {
		title = string([]rune(title)[:40])
	}

	images := item.Get("images").Array()
	var imgURLs []string
	seenImg := map[string]bool{}
	for _, im := range images {
		u := firstURL(im.Get("url_list"))
		// 去重：平台数据偶发重复条目，重复 URL 会落成 02d.ext 两个同名内容文件，
		// 进框选页后表现为「相同图片重复出现」
		if u == "" || seenImg[u] {
			continue
		}
		seenImg[u] = true
		imgURLs = append(imgURLs, u)
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
	name, cands := douyinMusicCandidates(ctx, item, id)
	// music_id 一并登记：它是比 CDN 地址稳定得多的业务标识，
	// 供试听缓存优先采用（避免签名/主机变化导致缓存永不命中）。
	post.SetAudioCandidates(name, musicID(item.Get("music")), cands...)
	return post, nil
}

// douyinMusicCandidates 汇总抖音 BGM 的全部取源并按「实测可靠性」排序。
//
// 候选优先级（2026-09-21 按真实链接重新校准）：
//  1. video.play_addr.uri —— 图文帖（aweme_type=2）的 BGM 实际挂载点。
//     图文帖的 music 节点常常只有 mid、**完全没有 play_url**（实测
//     https://v.douyin.com/Y-dKHMoB8_g/ 即为此形态），此时 ①②③④ 全空，
//     唯一可用的地址就在这里。uri 形如
//     https://lf26-music-east.douyinstatic.com/obj/ies-music-hj/{fileID}.mp3
//     实测无 Referer/无 Cookie 即可 GET 到 audio/mpeg，故置于链首。
//  2. SSR music.play_url.url_list 中的 http(s) 地址 —— 官方下发。
//  3. music.play_url.uri —— 仅当它本身就是 http(s) 地址时收录。
//  4. music/detail 接口（按 music.id 查询）。
//     注意：该接口自 2026-09-21 实测已被 Argus 风控拦截，返回
//     403 "Blocked by ArgusSecurityPlugin Uifid Not Found"，无论是否带
//     Cookie。保留它仅作为接口策略可能回滚的兜底，不再视为主力。
//  5. aweme/detail 接口（需 a_bogus/msToken，最后手段）。
//
// 为什么 ③④ 仍始终并入链尾：SSR 下发的地址带时效签名/地域限制，存在
// 「同一链接昨日可用、今日全部 403/404」的偶发失效。failover 逐条尝试、
// 命中即停，并入链尾不增加成功路径的成本（仅当 ①② 全部失效时才真正请求）。
//
// uri 字段通常是资源标识（形如 v0200fg10000...）而非可 GET 的 URL，
// 只有它是 http(s) 地址时才收录，避免把它当 URL 传给下载器后报「地址无效」，
// 把真正的失败原因（风控拦截）掩盖掉。
//
// 历史教训（v1.1.7 修复）：曾用 `ies-music/{music.id}.mp3` 直接拼接对象存储
// 直链，实测该路径对绝大多数曲目返回 404——对象存储的文件名 ID 与 music.id
// 是**两个不同的资源 ID**（例：music.id=7679463410022550307 对应
// ies-music-hj/7679463571897502513.mp3），无法自行推导，只能走接口获取。
// 本次新收录的 video.play_addr.uri 再次印证该规律：music.mid=7505383933425879858
// 对应的实际文件是 ies-music-hj/7505383972596108090.mp3，仍然不同。
func douyinMusicCandidates(ctx context.Context, item gjson.Result, id string) (name string, cands []AudioCandidate) {
	music := item.Get("music")
	name = trimSpace(music.Get("title").String())
	if name == "" {
		name = trimSpace(music.Get("author").String())
	}

	// 本地去重：SSR 的 uri 与实际可用地址可能重复，
	// 保留重复项会让 failover 白跑一次探测。
	add := func(u, source string) {
		u = trimSpace(u)
		if u == "" {
			return
		}
		for _, e := range cands {
			if e.URL == u {
				return
			}
		}
		cands = append(cands, AudioCandidate{URL: u, Source: source, Priority: len(cands)})
	}

	// ① 图文帖 BGM：music 节点无 play_url 时，真实音频挂在 video.play_addr。
	// 优先取 uri（对象存储直链，实测免签名可直下），其次取 url_list。
	// 仅对图文帖（有 images）启用：视频帖的 video.play_addr 是视频本体而非 BGM，
	// 收录它会把整个视频当音频下载。
	if len(item.Get("images").Array()) > 0 {
		if u := trimSpace(item.Get("video.play_addr.uri").String()); strings.HasPrefix(u, "http") {
			add(u, SourceDouyinImagePlayAddr)
		}
	}

	for _, u := range music.Get("play_url.url_list").Array() {
		if s := trimSpace(u.String()); strings.HasPrefix(s, "http") {
			add(s, SourceDouyinSSRURLList)
		}
	}
	if u := trimSpace(music.Get("play_url.uri").String()); strings.HasPrefix(u, "http") {
		add(u, SourceDouyinSSRURI)
	}

	// ④ 始终并入链尾（见函数头注释）。musicID 为空时 douyinMusicDetailURL
	// 直接返回 nil（不发请求）。
	for _, u := range douyinMusicDetailURL(ctx, musicID(music)) {
		add(u, SourceDouyinMusicDetail)
	}

	// 末位兜底：aweme/detail（需签名，被风控时静默返回空）
	if u, n := douyinDetailMusicURL(ctx, id); u != "" {
		add(u, SourceDouyinAwemeDetail)
		if name == "" {
			name = n
		}
	}
	return name, cands
}

// musicID 取 music 节点的数字 ID，缺失时回落 mid。
func musicID(music gjson.Result) string {
	if s := trimSpace(music.Get("id").String()); isNumericID(s) {
		return s
	}
	if s := trimSpace(music.Get("mid").String()); isNumericID(s) {
		return s
	}
	return ""
}

// douyinMusicDetailURL 通过 music/detail 接口按 music_id 查询真实音频地址。
//
// 实测（2026-09-15）：该接口**无需 a_bogus 签名**即可返回完整 music_info，
// 是 SSR 解析降级时的主力兜底。返回的 url_list 中的文件名 ID 与 music_id
// 不同，因此该地址无法由 music_id 自行拼接，必须走接口。
//
// 解析失败（风控/结构变更/非音乐 ID）时返回 nil，不返回错误——
// 调用方把它当作候选链的一环，失败即继续降级。
func douyinMusicDetailURL(ctx context.Context, musicID string) []string {
	if musicID == "" {
		return nil
	}
	api := musicDetailAPIBase + "?music_id=" + musicID +
		"&aid=6383&cookie_enabled=true&platform=PC&downlink=1"
	data, _, err := httpGet(ctx, api, "https://www.douyin.com/", BrowserUA)
	if err != nil {
		return nil
	}
	mi := gjson.Parse(string(data)).Get("music_info")
	if !mi.Exists() || mi.Type == gjson.Null {
		return nil
	}
	var urls []string
	seen := map[string]bool{}
	add := func(u string) {
		u = trimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		urls = append(urls, u)
	}
	for _, u := range mi.Get("play_url.url_list").Array() {
		if s := trimSpace(u.String()); strings.HasPrefix(s, "http") {
			add(s)
		}
	}
	if u := trimSpace(mi.Get("play_url.uri").String()); strings.HasPrefix(u, "http") {
		add(u)
	}
	return urls
}

// isNumericID 判断是否为纯数字串（抖音资源 id 均为十进制数字，
// 借此排除字段缺失或结构变更时混入的异常值）。
func isNumericID(s string) bool {
	if s == "" || len(s) > 24 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// douyinDetailMusicURL 通过 detail API 获取 BGM 播放地址与曲目名。
// 注意：该接口需 a_bogus/msToken/ttwid 齐备才稳定可用，本函数未做签名，
// 仅作为候选链的最后一级兜底；被风控时返回空串。
func douyinDetailMusicURL(ctx context.Context, id string) (audioURL, audioName string) {
	// 空 ID 直接返回：避免构造出 aweme_id= 的无效请求——既无意义，
	// 又会让不依赖网络的单测意外打到真实接口。
	if trimSpace(id) == "" {
		return "", ""
	}
	api := "https://www.douyin.com/aweme/v1/web/aweme/detail/?aweme_id=" + id +
		"&aid=6383&cookie_enabled=true&platform=PC&downlink=1"
	data, _, err := httpGet(ctx, api, "https://www.douyin.com/", BrowserUA)
	if err != nil {
		return "", ""
	}
	aweme := gjson.Parse(string(data)).Get("aweme_detail")
	if !aweme.Exists() {
		return "", ""
	}
	m := aweme.Get("music")
	audioURL = firstURL(m.Get("play_url.url_list"))
	if audioURL == "" {
		if u := trimSpace(m.Get("play_url.uri").String()); strings.HasPrefix(u, "http") {
			audioURL = u
		}
	}
	audioName = trimSpace(m.Get("title").String())
	return audioURL, audioName
}

// findDouyinItem 在 _ROUTER_DATA 的 loaderData 中定位 item_list 第一项。
//
// 两条取数路径（实测 2026-09-21）：
//   - 分享页：loaderData["note_(id)/page"].videoInfoRes.item_list
//   - 桌面页/新版：loaderData["note_(<真实id>)/page"].videoInfoRes.item_list
//
// 关键坑一：分享页的键名里 **(id) 是字面文本**，不是占位符——
// 真实键名字节就是 `note_(id)/page`（实测 hex: 6e 6f 74 65 5f 28 69 64 29 2f 70 61 67 65），
// 而**不是** `note_(<作品ID>)/page`。早期只用后者拼接，分享页永远匹配不上。
//
// 关键坑二：gjson 路径里**不要给键名加引号**。`(` `)` `/` 都不是 gjson 的
// 保留字符，无需转义；写成 `"note_(id)/page"` 反而会把双引号当成键名的一部分，
// 导致 Exists 恒为 false。实测：
//
//	loader.Get(`"note_(id)/page".videoInfoRes.item_list.0`)  // false ✗
//	loader.Get("note_(id)/page.videoInfoRes.item_list.0")    // true  ✓
//
// 该写法 bug 曾长期存在，只因下方遍历兜底能救回来而未暴露；代价是每次解析
// 白跑 9 次必然失败的精确查询。现已修正。
func findDouyinItem(root gjson.Result, id string) gjson.Result {
	loader := root.Get("loaderData")
	if !loader.Exists() {
		return gjson.Result{}
	}
	for _, prefix := range []string{
		// 分享页字面量键（(id) 为字面文本，注意与下方拼接键区分）
		"note_(id)/page",
		"video_(id)/page",
		"slides_(id)/page",
		// 新版桌面页：键名嵌真实 id
		"note_(" + id + ")/page",
		"note_" + id + "/page",
		"video_(" + id + ")/page",
		"video_" + id + "/page",
		"slides_" + id + "/page",
		"slides_(" + id + ")/page",
	} {
		if v := loader.Get(prefix + ".videoInfoRes.item_list.0"); v.Exists() {
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

package downloader

import (
	"net/url"
	"sort"
	"strings"
	"sync"
)

// 候选来源标识。写入日志与偏好记录，使「哪个源在失败」可以直接读出，
// 而不必从 URL 反推。
const (
	SourceDouyinSSRURLList  = "douyin_ssr_url_list" // 页面 SSR 下发的 play_url.url_list
	SourceDouyinSSRURI      = "douyin_ssr_uri"      // 页面 SSR 下发的 play_url.uri
	SourceDouyinMusicDetail = "douyin_music_detail" // /aweme/v1/web/music/detail/
	SourceDouyinAwemeDetail = "douyin_aweme_detail" // /aweme/v1/web/aweme/detail/
	// SourceDouyinImagePlayAddr 图文帖（aweme_type=2）的 BGM 实际挂载点：
	// video.play_addr.uri。图文帖没有真实视频，抖音复用 video 节点承载音频，
	// 其 uri 是 ies-music-hj/*.mp3 形式的对象存储直链（无签名、可直下）。
	// 详见 douyinMusicCandidates 中的说明。
	SourceDouyinImagePlayAddr = "douyin_image_play_addr"
	SourceXHSMusic            = "xhs_music" // 小红书 note.music.*
	SourceUnknown             = "unknown"   // 无法确定来源
)

// AudioCandidate 一个 BGM 候选地址及其元信息。
type AudioCandidate struct {
	URL      string // 音频地址
	Source   string // 来源标识（SourceXxx 常量）
	Priority int    // 越小越优先；由候选链生成顺序写入（SSR 地址最前）
	// Referer / UA 留空时沿用 audioRequestMeta 按 CDN 主机推断的既有策略，
	// 仅在确需覆盖某个 CDN 的请求头时填写。
	Referer string
	UA      string
}

// dynamicQueryKeys 视为动态凭据/时效参数的查询键（小写）。
//
// 去重时只忽略这些键，其余查询参数参与等价判定——避免把
// ?quality=high 与 ?quality=low 这类指向不同资源的地址误合并。
var dynamicQueryKeys = map[string]bool{
	"sign": true, "sig": true, "signature": true, "_signature": true,
	"x-sign": true, "x-signature": true, "token": true, "auth": true,
	"expire": true, "expires": true, "x-expires": true,
	"ms_token": true, "mstoken": true, "ttwid": true,
	"ts": true, "timestamp": true, "nonce": true, "verify": true,
}

// candidateIdentity 候选的等价标识：忽略 CDN 主机与动态凭据参数，保留 path
// 及其余查询参数。
//
// 依据：同一个 BGM 会被下发到多个边缘节点，host 与 sign 不同而 path 相同，
// 实际是同一个文件。仅当 path 与非动态查询参数完全一致时才判定等价，
// 不做「去掉全部查询参数」的粗暴合并。
func candidateIdentity(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw) // 解析失败：退化为原串，宁可不去重也不误合并
	}
	var kept []string
	for k, vs := range u.Query() {
		if dynamicQueryKeys[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			kept = append(kept, k+"="+v)
		}
	}
	sort.Strings(kept)
	id := u.Path
	if len(kept) > 0 {
		id += "?" + strings.Join(kept, "&")
	}
	return id
}

// DedupCandidates 按等价标识去重，保留链中先出现者（即优先级更高的一条），
// 同时丢弃空地址。SetAudio 已做精确去重；此处额外合并同一资源的不同
// CDN 节点/签名，避免同一文件被候选链试两遍。
func DedupCandidates(cands []AudioCandidate) []AudioCandidate {
	out := make([]AudioCandidate, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		u := strings.TrimSpace(c.URL)
		if u == "" {
			continue
		}
		c.URL = u
		id := candidateIdentity(u)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, c)
	}
	return out
}

// CandidateChainKey 候选链的稳定标识：忽略主机名与查询串（签名/时效参数），
// 只保留各候选的 path，去重排序后拼接。
//
// 用于三处共享同一份签名：试听缓存目录、偏好记录键、候选链注册表键。
// 此前该逻辑位于 app 层（audioCacheKey），下沉以避免规则漂移。
func CandidateChainKey(urls []string) string {
	paths := make([]string, 0, len(urls))
	seen := make(map[string]bool, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if i := strings.Index(u, "?"); i >= 0 {
			u = u[:i]
		}
		if i := strings.Index(u, "://"); i >= 0 {
			if j := strings.IndexByte(u[i+3:], '/'); j >= 0 {
				u = u[i+3+j:]
			}
		}
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		paths = append(paths, u)
	}
	if len(paths) == 0 {
		return strings.Join(urls, "|")
	}
	sort.Strings(paths)
	return strings.Join(paths, "|")
}

// chainRecord 注册表中的一条候选链记录。
type chainRecord struct {
	Candidates []AudioCandidate
	MusicID    string
}

// candidateRegistry 记录最近解析出的候选链，供下载阶段恢复 Source 与 music_id。
//
// 为什么需要它：候选链要经前端往返（Post.audioCandidates → 前端 →
// DownloadBGM(candidates)），[]string 无法携带 Source；而 Source 正是日志与
// 排序所需的信息。解析与下载在同一次会话中一前一后发生，进程内有界缓存即可覆盖。
type candidateRegistry struct {
	mu    sync.RWMutex
	order []string
	items map[string]chainRecord
}

// candidateRegistryCap 注册表容量上限（超出按插入顺序淘汰最旧记录）。
const candidateRegistryCap = 128

var audioChainRegistry = &candidateRegistry{items: map[string]chainRecord{}}

func (r *candidateRegistry) put(key string, rec chainRecord) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.items[key]; !exists {
		r.order = append(r.order, key)
	}
	r.items[key] = rec
	for len(r.order) > candidateRegistryCap {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.items, oldest)
	}
}

func (r *candidateRegistry) get(key string) (chainRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.items[key]
	return rec, ok
}

// RegisterAudioChain 记录候选链（解析阶段调用）。urls 与 cands 应一一对应。
func RegisterAudioChain(urls []string, cands []AudioCandidate, musicID string) {
	audioChainRegistry.put(CandidateChainKey(urls), chainRecord{Candidates: cands, MusicID: musicID})
}

// registerAudioChain 登记候选链；cands 为空时按地址特征补齐来源。
func registerAudioChain(urls []string, musicID string, cands []AudioCandidate) {
	if len(urls) == 0 {
		return
	}
	if len(cands) == 0 {
		cands = make([]AudioCandidate, 0, len(urls))
		for i, u := range urls {
			cands = append(cands, AudioCandidate{URL: u, Source: guessSource(u), Priority: i})
		}
	}
	RegisterAudioChain(urls, cands, musicID)
}

// ChainMusicID 取候选链关联的平台音乐 ID（未记录时返回空串）。
func ChainMusicID(urls []string) string {
	if rec, ok := audioChainRegistry.get(CandidateChainKey(urls)); ok {
		return rec.MusicID
	}
	return ""
}

// guessSource 注册表未命中时按地址特征推断来源。只用于日志可读性，
// 准确性低于注册表，不参与排序决策。
func guessSource(raw string) string {
	low := strings.ToLower(raw)
	switch {
	case strings.Contains(low, "douyinstatic"), strings.Contains(low, "douyin"),
		strings.Contains(low, "pstatp"), strings.Contains(low, "muscdn"),
		strings.Contains(low, "amemv"), strings.Contains(low, "snssdk"):
		return "douyin_cdn"
	case strings.Contains(low, "xiaohongshu"), strings.Contains(low, "xhscdn"):
		return SourceXHSMusic
	default:
		return SourceUnknown
	}
}

// CandidatesFromURLs 把前端回传的 []string 还原为带来源信息的候选链。
//
// 优先用注册表恢复精确来源；未命中（如用户跨会话重试、或链已淘汰）时按
// 地址特征推断，并在索引层面重建优先级。
func CandidatesFromURLs(urls []string) []AudioCandidate {
	key := CandidateChainKey(urls)
	rec, ok := audioChainRegistry.get(key)

	byURL := map[string]string{}
	if ok {
		for _, c := range rec.Candidates {
			byURL[c.URL] = c.Source
		}
	}

	out := make([]AudioCandidate, 0, len(urls))
	index := map[string]int{}
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		src := byURL[u]
		if src == "" {
			src = guessSource(u)
		}
		// 注册表命中时沿用原优先级，否则按回传顺序编号。
		prio := len(out)
		if ok {
			for _, c := range rec.Candidates {
				if c.URL == u {
					prio = c.Priority
					break
				}
			}
		}
		if _, dup := index[u]; dup {
			continue
		}
		index[u] = len(out)
		out = append(out, AudioCandidate{URL: u, Source: src, Priority: prio})
	}
	return DedupCandidates(out)
}

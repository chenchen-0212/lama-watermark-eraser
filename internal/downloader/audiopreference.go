package downloader

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------- 日志出口

var (
	audioLogMu sync.RWMutex
	audioLogfn = func(string) {}
)

// SetAudioLogger 注入音频下载链路的日志出口（app 层接到既有事件通道）。
// 未注入时静默，避免库层强依赖 UI。
func SetAudioLogger(fn func(msg string)) {
	audioLogMu.Lock()
	defer audioLogMu.Unlock()
	if fn == nil {
		audioLogfn = func(string) {}
		return
	}
	audioLogfn = fn
}

// audioLog 输出一行结构化日志。调用方负责拼接已脱敏的字段。
func audioLog(msg string) {
	audioLogMu.RLock()
	fn := audioLogfn
	audioLogMu.RUnlock()
	fn(msg)
}

// ---------------------------------------------------------------- 成功源偏好

// SourceStat 单个候选来源的成败统计。
//
// 保留这一层是为了回答「到底是候选源的问题，还是请求策略的问题」——
// 只统计链路整体成败无法区分某级候选是否长期不可用（PRD §18）。
type SourceStat struct {
	Success         int    `json:"success"`
	Failure         int    `json:"failure"`
	LastFailureKind string `json:"last_failure_kind,omitempty"` // 最近一次失败的分类
}

// AudioSourcePreference 某条候选链上各来源的历史成败，作为下次排序的依据。
//
// 注意它只影响顺序、不影响候选集合：偏好源失败会立即清除偏好，
// 下一次请求恢复完整候选链。历史成功是排序依据，不是绝对规则。
type AudioSourcePreference struct {
	ChainKey      string                 `json:"chain_key"`       // 候选链标识（CandidateChainKey）
	Preferred     string                 `json:"preferred"`       // 最近成功的来源；空表示无偏好
	SuccessCount  int                    `json:"success_count"`   // 该链累计成功次数
	FailureCount  int                    `json:"failure_count"`   // 该链累计失败次数
	LastSuccessAt time.Time              `json:"last_success_at"` // 最近一次成功时间
	Sources       map[string]*SourceStat `json:"sources,omitempty"`
}

// audioPreferenceCap 偏好记录条数上限（超出淘汰最久未成功的记录）。
const audioPreferenceCap = 256

type audioPrefStore struct {
	mu    sync.Mutex
	path  string // 持久化文件路径；空表示仅内存
	items map[string]*AudioSourcePreference
}

var audioPrefs = &audioPrefStore{items: map[string]*AudioSourcePreference{}}

// InitAudioPreferences 指定偏好文件所在目录并载入既有记录。
// dir 为空时仅保留内存态（功能不受影响，只是不跨会话记忆）。
func InitAudioPreferences(dir string) {
	audioPrefs.mu.Lock()
	defer audioPrefs.mu.Unlock()
	audioPrefs.items = map[string]*AudioSourcePreference{}
	audioPrefs.path = ""
	dir = trimSpace(dir)
	if dir == "" {
		return
	}
	path := filepath.Join(dir, "bgm-source-prefs.json")
	data, err := os.ReadFile(path)
	if err != nil {
		audioPrefs.path = path // 首次运行：文件还不存在
		return
	}
	var list []*AudioSourcePreference
	if json.Unmarshal(data, &list) != nil {
		audioPrefs.path = path
		return
	}
	for _, p := range list {
		if p != nil && p.ChainKey != "" {
			audioPrefs.items[p.ChainKey] = p
		}
	}
	audioPrefs.path = path
}

// preferredSource 返回某条链当前的偏好来源（无则空串）。
func preferredSource(chainKey string) string {
	if chainKey == "" {
		return ""
	}
	audioPrefs.mu.Lock()
	defer audioPrefs.mu.Unlock()
	if p := audioPrefs.items[chainKey]; p != nil {
		return p.Preferred
	}
	return ""
}

// preferenceLocked 取或新建某条链的偏好记录。调用方须持锁。
func preferenceLocked(chainKey string) *AudioSourcePreference {
	p := audioPrefs.items[chainKey]
	if p == nil {
		p = &AudioSourcePreference{ChainKey: chainKey}
		audioPrefs.items[chainKey] = p
	}
	return p
}

// sourceStatLocked 取或新建某来源的统计。调用方须持锁。
func sourceStatLocked(p *AudioSourcePreference, source string) *SourceStat {
	if p.Sources == nil {
		p.Sources = map[string]*SourceStat{}
	}
	st := p.Sources[source]
	if st == nil {
		st = &SourceStat{}
		p.Sources[source] = st
	}
	return st
}

// recordAudioSuccess 记录某来源成功，并把它设为该链的偏好来源。
func recordAudioSuccess(chainKey, source string) {
	if chainKey == "" || source == "" {
		return
	}
	audioPrefs.mu.Lock()
	p := preferenceLocked(chainKey)
	p.Preferred = source
	p.SuccessCount++
	p.LastSuccessAt = time.Now()
	sourceStatLocked(p, source).Success++
	evictAudioPreferencesLocked()
	audioPrefs.mu.Unlock()
	persistAudioPreferences()
}

// recordAudioFailure 记录某来源失败及其分类。若失败的正是当前偏好来源则立即
// 清除偏好，使下一次请求回到完整候选链顺序（避免被一个已不可用的源长期拖住）。
func recordAudioFailure(chainKey, source string, kind AudioErrorKind) {
	if chainKey == "" || source == "" {
		return
	}
	audioPrefs.mu.Lock()
	p := preferenceLocked(chainKey)
	p.FailureCount++
	if p.Preferred == source {
		p.Preferred = ""
	}
	st := sourceStatLocked(p, source)
	st.Failure++
	if kind != "" {
		st.LastFailureKind = string(kind)
	}
	evictAudioPreferencesLocked()
	audioPrefs.mu.Unlock()
	persistAudioPreferences()
}

// evictAudioPreferencesLocked 超限时淘汰最久未成功的记录。调用方须持锁。
func evictAudioPreferencesLocked() {
	if len(audioPrefs.items) <= audioPreferenceCap {
		return
	}
	type kv struct {
		key string
		at  time.Time
	}
	all := make([]kv, 0, len(audioPrefs.items))
	for k, v := range audioPrefs.items {
		all = append(all, kv{key: k, at: v.LastSuccessAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for i := 0; i < len(all)-audioPreferenceCap; i++ {
		delete(audioPrefs.items, all[i].key)
	}
}

// persistAudioPreferences 原子写回偏好文件（先写 .tmp 再改名）。
func persistAudioPreferences() {
	audioPrefs.mu.Lock()
	path := audioPrefs.path
	if path == "" {
		audioPrefs.mu.Unlock()
		return
	}
	list := make([]*AudioSourcePreference, 0, len(audioPrefs.items))
	for _, p := range audioPrefs.items {
		list = append(list, p)
	}
	audioPrefs.mu.Unlock()

	sort.Slice(list, func(i, j int) bool { return list[i].ChainKey < list[j].ChainKey })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// ---------------------------------------------------------------- 候选排序

// candidateURLs 抽取候选地址列表（偏好键与注册表键共用）。
func candidateURLs(cands []AudioCandidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.URL
	}
	return out
}

// RankAudioCandidates 对候选链重排：历史成功来源整体前移，其余保持原有相对顺序。
//
// 它取代了旧的 PickAudioSource（Range 探测）——排序过程不发任何 HTTP 请求，
// 「这个地址到底能不能用」交由下载本身回答（探测已是下载的副产物，见 saveAudioOnce）。
// 只改顺序、不改集合，因此偏好源失效时其余候选仍会依次尝试。
func RankAudioCandidates(cands []AudioCandidate) []AudioCandidate {
	if len(cands) <= 1 {
		return cands
	}
	pref := preferredSource(CandidateChainKey(candidateURLs(cands)))
	if pref == "" {
		return cands
	}
	out := make([]AudioCandidate, 0, len(cands))
	for _, c := range cands {
		if c.Source == pref {
			out = append(out, c)
		}
	}
	if len(out) == 0 || len(out) == len(cands) {
		return cands // 无同源候选或全部同源：顺序无意义
	}
	for _, c := range cands {
		if c.Source != pref {
			out = append(out, c)
		}
	}
	return out
}

package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// AudioErrorKind 音频下载失败的原因分类。
//
// 分类的唯一目的是决定后续动作：重试当前地址、切换到下一个候选、还是直接放弃。
// 此前所有失败都退化成裸 error，上层无法区分 403（值得换源）、404（换源但别重试）、
// timeout（值得退避后重试），只能一律重跑整条候选链——既浪费请求次数，也把真实
// 原因掩盖成一句「下载失败」。
type AudioErrorKind string

const (
	// AudioErrInvalidURL 地址为空或非 http(s)。属于配置错误，重试无意义。
	AudioErrInvalidURL AudioErrorKind = "invalid_url"
	// AudioErrNetwork 连接失败 / connection reset / broken pipe / DNS 解析失败。
	AudioErrNetwork AudioErrorKind = "network"
	// AudioErrTimeout 请求超时或上下文到期。
	AudioErrTimeout AudioErrorKind = "timeout"
	// AudioErr403 防盗链或风控直接拒绝（含其余 4xx：401/400 等一律按「被拒绝」处理）。
	AudioErr403 AudioErrorKind = "403"
	// AudioErr404 资源不存在——链接已过期，或该 CDN 节点上没有这个文件。
	AudioErr404 AudioErrorKind = "404"
	// AudioErr429 限流。
	AudioErr429 AudioErrorKind = "429"
	// AudioErr5xx 服务端错误。
	AudioErr5xx AudioErrorKind = "5xx"
	// AudioErrNotAudio HTTP 200 但内容不是音频（HTML 风控页 / 错误 JSON）。
	AudioErrNotAudio AudioErrorKind = "not_audio"
	// AudioErrEmptyBody HTTP 200 但响应体为空。
	AudioErrEmptyBody AudioErrorKind = "empty_body"
)

// audioAttemptLimit 单个候选地址的最大尝试次数（含首次）。
//
// 放在包级变量而非散落在各处的字面量，便于统一调整与测试注入。
var audioAttemptLimit = 2

// SetAudioAttemptLimit 调整单个候选地址的最大尝试次数（含首次），下限 1。
func SetAudioAttemptLimit(n int) {
	if n < 1 {
		n = 1
	}
	audioAttemptLimit = n
}

// maxAttemptsFor 该类错误下「同一个候选地址」允许的最大尝试次数。
//
// 取舍依据：明确不可恢复的错误绝不浪费请求次数。
//   - 404 / 非音频内容 / 地址非法：重试必然得到同样结果，只试一次；
//   - 403 / 空响应：常是瞬时风控或 CDN 抖动，允许一次额外尝试；
//   - 其余（网络、超时、429、5xx）：沿用调用方给定的上限。
func (k AudioErrorKind) maxAttemptsFor(limit int) int {
	switch k {
	case AudioErr404, AudioErrNotAudio, AudioErrInvalidURL:
		return 1
	case AudioErr403, AudioErrEmptyBody:
		if limit > 2 {
			return 2
		}
		return limit
	default:
		return limit
	}
}

// AudioDownloadError 单次音频下载尝试失败的结构化错误。
//
// 让上层能够回答：哪个候选、什么地址、HTTP 状态多少、属于哪类错误、还值不值得再试。
type AudioDownloadError struct {
	Kind       AudioErrorKind // 失败的分类
	StatusCode int            // HTTP 状态码；非 HTTP 层错误为 0
	URL        string         // 已脱敏的地址（见 redactURL）
	Host       string         // 地址主机（脱敏后仍保留，便于按 CDN 定位）
	Source     string         // 候选来源标识（如 douyin_music_detail）
	Index      int            // 该候选在链中的序号（0 起）
	Attempt    int            // 这是该候选的第几次尝试（1 起）
	Retryable  bool           // 本次失败后，是否还允许对同一地址再试
	Err        error          // 底层错误
}

func (e *AudioDownloadError) Error() string {
	var b strings.Builder
	b.WriteString("音频下载失败[")
	b.WriteString(string(e.Kind))
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " HTTP %d", e.StatusCode)
	}
	if e.Source != "" {
		fmt.Fprintf(&b, " source=%s", e.Source)
	}
	fmt.Fprintf(&b, " 候选#%d 第%d次", e.Index, e.Attempt)
	b.WriteString("]")

	switch e.Kind {
	case AudioErrNotAudio:
		// 保留既有文案：上层（app 层测试与用户提示）依赖「不是音频」这一措辞。
		b.WriteString("响应内容不是音频")
	case AudioErrEmptyBody:
		b.WriteString("响应内容为空")
	case AudioErrInvalidURL:
		b.WriteString("音频地址无效")
	}
	if e.URL != "" {
		fmt.Fprintf(&b, ": %s", e.URL)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, "（%v）", e.Err)
	}
	return b.String()
}

// Unwrap 暴露底层错误，使 errors.Is/As 可穿透（含 errNotAudio 哨兵）。
func (e *AudioDownloadError) Unwrap() error { return e.Err }

// newAudioError 构造结构化错误，并按错误类型与已尝试次数推导 Retryable。
func newAudioError(kind AudioErrorKind, statusCode int, rawURL, source string, index, attempt int, err error) *AudioDownloadError {
	return &AudioDownloadError{
		Kind:       kind,
		StatusCode: statusCode,
		URL:        redactURL(rawURL),
		Host:       hostOf(rawURL),
		Source:     source,
		Index:      index,
		Attempt:    attempt,
		Retryable:  attempt < kind.maxAttemptsFor(audioAttemptLimit),
		Err:        err,
	}
}

// kindOf 提取错误的分类；非结构化错误返回空串。
func kindOf(err error) AudioErrorKind {
	var ae *AudioDownloadError
	if errors.As(err, &ae) {
		return ae.Kind
	}
	return ""
}

// statusOf 提取错误的 HTTP 状态码；非 HTTP 层错误返回 0。
func statusOf(err error) int {
	var ae *AudioDownloadError
	if errors.As(err, &ae) {
		return ae.StatusCode
	}
	return 0
}

// AudioErrorRetryableAcrossChain 判断「整条候选链都失败」之后是否值得重跑一遍。
//
// 只有瞬态类失败才值得：网络、超时、限流、5xx——这些可能是 CDN 此刻的抖动。
// 若失败是 404、内容非音频、地址非法，或错误无分类（本地磁盘/权限等致命问题），
// 重跑整条链只会重复同样的失败，此时应当直接返回，把真实原因交给用户。
func AudioErrorRetryableAcrossChain(err error) bool {
	if err == nil {
		return false
	}
	// 用户主动取消：不是链路问题
	if isCanceled(err) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch kindOf(err) {
	case AudioErrNetwork, AudioErrTimeout, AudioErr429, AudioErr5xx:
		return true
	default:
		return false
	}
}

// classifyHTTPStatus 非 200 响应 → 错误分类。
// 404/429/5xx 单独归类；401/403 及其余 4xx 一律按「被拒绝」处理（可换源、限一次重试）。
func classifyHTTPStatus(code int) AudioErrorKind {
	switch {
	case code == http.StatusNotFound:
		return AudioErr404
	case code == http.StatusTooManyRequests:
		return AudioErr429
	case code >= 500:
		return AudioErr5xx
	case code >= 400:
		return AudioErr403
	default:
		// 3xx 已被 client 跟随；其余（含 1xx/2xx 异常值）按服务端问题处理。
		return AudioErr5xx
	}
}

// classifyNetError transport 层错误（client.Do 返回）→ 错误分类。
//
// 不引用 syscall 常量：ECONNRESET 之类仅在类 Unix 构建标签下存在，Windows 上
// 对应的 WSA 错误名不同。这里以 net.Error / context 判定为主，字符串匹配兜底，
// 未识别的错误默认归入 network（可重试）——对瞬态故障宁可多试一次。
func classifyNetError(err error) AudioErrorKind {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return AudioErrTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return AudioErrTimeout
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"),
		strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "connection timed out"):
		return AudioErrTimeout
	case strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "forcibly closed"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "no route to host"),
		strings.Contains(msg, "network is unreachable"),
		strings.Contains(msg, "unexpected eof"),
		strings.Contains(msg, "wsarecv"),
		strings.Contains(msg, "wsasend"):
		return AudioErrNetwork
	}
	return AudioErrNetwork
}

// hostOf 取地址主机名（脱敏前），失败返回空串。
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// redactURL 生成可安全写入日志的地址：保留 scheme/host/path，查询串整体打码。
//
// CDN 地址的 sign / x-expires / token / auth 等参数是敏感凭据，完整落日志既不安全，
// 对定位问题也无帮助——真正有用的是 host 与 path。
func redactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "(invalid-url)"
	}
	out := u.Scheme + "://" + u.Host + u.Path
	if u.RawQuery != "" {
		out += "?...redacted"
	}
	return out
}

// isCanceled 判断错误是否源于上下文取消（用户主动中断，不应重试也不应换源）。
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// isEOF 判断是否读到流尾部（用于区分「空响应体」与真正的读取失败）。
func isEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

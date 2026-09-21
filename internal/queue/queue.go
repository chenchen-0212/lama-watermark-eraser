// Package queue 提供批处理任务队列：任务模型、调度器、失败重试与持久化。
//
// 设计要点（对应《自动入队产品优化方案》v1.2）：
//   - 两类任务：download（社媒链接下载）/ inpaint（去水印，入队时固化参数）；
//   - **双 worker 并行**：download 与 inpaint 各一个 worker，两类之间互不阻塞
//     （网络 I/O 与引擎 CPU 资源不冲突）；同类任务仍串行——引擎单实例红线
//     只约束 inpaint 之间，下载目录互斥由 App 层 downloadMu 保护；
//   - 每类队列活跃任务（pending+running+retry_wait）上限 MaxActivePerQueue 条，
//     超出拒绝入队（防堆积）；
//   - 失败三分类：瞬时错误指数退避自动重试 ≤MaxAutoRetries 次；确定性错误
//     （Permanent 包装）不重试直接 failed；取消不计失败；
//   - 任务列表原子持久化到 queue.json，应用重启后 running/retry_wait 降级为
//     pending 续跑；
//   - 事件经 Notifier 回调推送（app.go 注入 Wails EventsEmit），本包不依赖
//     Wails，便于单测（对齐 internal/downloader 的分层风格）。
package queue

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"lama-watermark-eraser/internal/inpaint"
)

// 任务类型。
const (
	TypeDownload = "download" // 社媒链接下载素材
	TypeInpaint  = "inpaint"  // 批量去水印（参数入队时固化）
)

// 任务状态机：
//
//	pending → running → done
//	running → retry_wait → pending（退避到期自动重排）
//	pending / running / retry_wait → canceled（用户取消）
//	running → failed（终态，可手动重试）
const (
	StatePending   = "pending"
	StateRunning   = "running"
	StateRetryWait = "retry_wait"
	StateDone      = "done"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
)

// MaxAutoRetries 瞬时错误自动重试上限（不含首次执行，即最多执行 1+3=4 次）。
const MaxAutoRetries = 3

// MaxActivePerQueue 每类队列的活跃任务上限（pending+running+retry_wait），
// 超出拒绝入队/重试（防连续提交堆积，方案 O4）。
const MaxActivePerQueue = 10

// RetryBackoffs 自动重试指数退避间隔（按第 n 次重试取下标，越界取末项）。
var RetryBackoffs = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}

// terminalState 是否终态。
func terminalState(s string) bool {
	return s == StateDone || s == StateFailed || s == StateCanceled
}

// ---------------------------------------------------------- 错误分类

// permanentError 确定性错误包装：失败原因固定（平台不支持、视频链接、
// 无图可下、参数无效等），重试不会好转，调度器见此类型直接置 failed。
type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// Permanent 将 err 标记为确定性错误（err 为 nil 时返回 nil）。
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent 判断 err 是否为确定性错误。
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

// errText 取用户可读的错误文案（解包 permanentError）。
func errText(err error) string {
	if err == nil {
		return ""
	}
	var p permanentError
	if errors.As(err, &p) {
		return p.err.Error()
	}
	return err.Error()
}

// ---------------------------------------------------------- 任务模型

// TaskProgress 运行中进度快照（队列面板实时刷新；持久化时清除，重启后无意义）。
type TaskProgress struct {
	Index  int    `json:"index"`
	Total  int    `json:"total"`
	Name   string `json:"name"`
	Status string `json:"status"` // ok | fail | skip
}

// BatchTask 队列任务（PRD §4.1）。
type BatchTask struct {
	ID       string              `json:"id"`                 // 唯一标识，前端操作凭据
	Type     string              `json:"type"`               // download | inpaint
	State    string              `json:"state"`              // pending|running|retry_wait|done|failed|canceled
	Platform string              `json:"platform,omitempty"` // download：平台标识（wechat|xhs|douyin）
	URL      string              `json:"url,omitempty"`      // download 载荷
	InDir    string              `json:"inDir,omitempty"`    // inpaint 载荷：源图目录
	OutDir   string              `json:"outDir,omitempty"`   // inpaint 载荷：输出目录
	Params   *inpaint.TaskParams `json:"params,omitempty"`   // inpaint 固化参数

	Attempts   int        `json:"attempts"` // 已自动重试次数
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Err        string     `json:"err,omitempty"` // 最近一次失败原因（用户可读）

	// 结果回填
	ResultDir string   `json:"resultDir,omitempty"` // download：素材目录；inpaint：输出目录
	PostRef   string   `json:"postRef,omitempty"`   // download：标题/张数摘要
	Title     string   `json:"title,omitempty"`     // 展示名：download=帖子标题；inpaint=来源下载任务的标题（入队时固化）
	Files     []string `json:"files,omitempty"`     // download：本次下载的图片清单（按序，「去框选」据此载入，
	//                                                    避免列目录把历史残留/重下文件混入导致图片重复）
	Summary  string   `json:"summary,omitempty"`  // inpaint：成功 x / 失败 y / 跳过 z
	FailList []string `json:"failList,omitempty"` // inpaint：失败文件清单（文件名 + 原因）

	// 下载任务的 BGM 取源（方案 v1.2）：自动入队后「去框选」只能凭任务记录还原
	// 帖子上下文，帖子自带的 BGM 若不随任务落库就会在框选页彻底丢失——用户看到
	// 的「界面上不出现 BGM 下载按钮」正是此因。
	// 三个字段与 downloader.Post 的前端契约同名同义，前端可直接平铺还原。
	AudioURL        string   `json:"audioUrl,omitempty"`        // 首选音频地址（=候选链首项）
	AudioCandidates []string `json:"audioCandidates,omitempty"` // 候选链（按优先级，逐条尝试）
	AudioName       string   `json:"audioName,omitempty"`       // 平台曲目名（展示/命名参考）
	// AudioChecked 本轮是否已真正探测过 BGM（含「确认无 BGM」）。
	// 用于区分「确实没有 BGM」与「历史任务未曾探测」：前者不再补取，后者按需回源。
	AudioChecked bool `json:"audioChecked,omitempty"`

	Progress    *TaskProgress `json:"progress,omitempty"`    // 运行中进度（不持久化）
	NextRetryAt *time.Time    `json:"nextRetryAt,omitempty"` // retry_wait 的预计重试时刻
}

// InpaintResult 去水印任务执行结果汇总。
type InpaintResult struct {
	OutDir   string   `json:"outDir"`
	OK       int      `json:"ok"`
	Total    int      `json:"total"`
	FailList []string `json:"failList,omitempty"` // "name: info"
}

// DownloadResult 下载任务执行结果。
//
// 不复用返回值列表（曾为 (resultDir, postRef, files, err)）而改为结构体：BGM
// 取源字段与图片清单同源产出、必须一同回填，返回值列表再加字段易错位。
type DownloadResult struct {
	Dir      string   // 素材目录
	PostRef  string   // 展示摘要（标题 | 张数）
	Files    []string // 本次下载的图片清单（按序）
	AudioURL string   // BGM 首选地址（无则空）
	Audio    []string // BGM 候选链（按优先级；可能为空）
	AudioNm  string   // BGM 曲目名
	// AudioResolved 本轮是否真正解析过帖子的 BGM 字段。
	// 用于区分两种「候选链为空」：帖子本身没有 BGM（true，无需再补取），
	// 与候选地址被健康检查过滤干净（false，可稍后重试补取）。
	AudioResolved bool
}

// EnqueueFailure 批量入队时单条链接的失败记录（PRD §4.2：失败原因即时反馈）。
type EnqueueFailure struct {
	Input  string `json:"input"`  // 原始输入（链接）
	Reason string `json:"reason"` // 预检/入队失败原因
}

// EnqueueReport 批量入队结果：成功任务 + 失败明细。
type EnqueueReport struct {
	Tasks    []*BatchTask     `json:"tasks"`
	Failures []EnqueueFailure `json:"failures,omitempty"`
}

// ProgressFn 去水印单张进度回调（i/total 为 1-based；status ∈ ok|fail|skip）。
type ProgressFn func(i, total int, name, status, info string)

// Executor 队列任务的执行器（由 app.go 实现，复用既有下载/推理链路）。
type Executor interface {
	// RunDownload 执行下载任务，返回素材目录、展示摘要、图片清单与 BGM 取源。
	RunDownload(ctx context.Context, t *BatchTask) (DownloadResult, error)
	// RunInpaint 执行去水印任务，返回结果汇总。部分单张失败不视为任务错误
	// （结果经 InpaintResult 传达）；err 非 nil 表示任务级失败。
	RunInpaint(ctx context.Context, t *BatchTask, onProgress ProgressFn) (InpaintResult, error)
	// ResolveTaskAudio 回源补取历史下载任务的 BGM（该任务入队时尚无 BGM 落库能力）。
	// 返回候选链与曲目名；未找到 BGM 时返回空链且 err 为 nil。
	ResolveTaskAudio(ctx context.Context, t *BatchTask) (cands []string, name string, err error)
}

// Notifier 事件回调：event 为 Wails 事件名，payload 为随事件发送的载荷。
type Notifier func(event string, payload interface{})

// ---------------------------------------------------------- 队列

// Queue 批处理任务队列（download / inpaint 双 worker 并行，同类串行）。
type Queue struct {
	mu     sync.Mutex
	tasks  []*BatchTask
	path   string // 持久化文件路径；空串=不持久化（测试用）
	exec   Executor
	notify Notifier

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wakeD  chan struct{} // 容量 1：唤醒 download worker
	wakeI  chan struct{} // 容量 1：唤醒 inpaint worker

	runningD    string                        // 当前 running 的 download 任务 ID（空=空闲）
	runningI    string                        // 当前 running 的 inpaint 任务 ID（空=空闲）
	taskCancels map[string]context.CancelFunc // 运行中任务的 ctx 取消函数
	retryTimer  map[string]*time.Timer        // retry_wait 任务的退避定时器
	onCancel    func(*BatchTask)              // 运行中任务被取消时的回调（App 层终止引擎）
}

// New 构造队列。path 为空时不持久化；notify 可为 nil。
// NewWithTasks 用既有任务列表构造队列（不启动 worker），仅供测试夹具使用。
//
// Enqueue 会把任务重置为 pending，无法用来灌入「已完成/已失败」的记录；
// 缓存清理等逻辑需要终态任务才能验证，故提供该直通构造器。
func NewWithTasks(tasks []BatchTask, path string) *Queue {
	q := New(nil, nil, path)
	q.mu.Lock()
	for i := range tasks {
		t := tasks[i]
		q.tasks = append(q.tasks, &t)
	}
	q.mu.Unlock()
	return q
}

func New(exec Executor, notify Notifier, path string) *Queue {
	return &Queue{
		exec:        exec,
		notify:      notify,
		path:        path,
		wakeD:       make(chan struct{}, 1),
		wakeI:       make(chan struct{}, 1),
		taskCancels: make(map[string]context.CancelFunc),
		retryTimer:  make(map[string]*time.Timer),
	}
}

// SetOnCancel 注册运行中任务被取消时的回调（App 层用于终止引擎进程树以中断
// 进行中的推理——与旧 CancelBatch 的 Kill 双保险语义一致）。
func (q *Queue) SetOnCancel(fn func(t *BatchTask)) { q.onCancel = fn }

// Start 启动调度：加载持久化任务（running/retry_wait → pending）并拉起
// download / inpaint 两个 worker。ctx 取消（应用退出）后 worker 退出，
// 队列状态已持久化。
func (q *Queue) Start(ctx context.Context) {
	if tasks, err := LoadQueue(q.path); err == nil {
		q.mu.Lock()
		q.tasks = tasks
		q.recoverLocked()
		q.mu.Unlock()
		q.save()
	}
	q.ctx, q.cancel = context.WithCancel(ctx)
	q.wg.Add(2)
	go q.workerLoop(TypeDownload, q.wakeD)
	go q.workerLoop(TypeInpaint, q.wakeI)
	q.notifyUpdate() // 前端启动时同步一次队列快照
	q.kickD()
	q.kickI()
}

// Stop 停止调度并等待 worker 退出（应用 shutdown 时调用）。
func (q *Queue) Stop() {
	if q.cancel != nil {
		q.cancel()
	}
	q.wg.Wait()
}

// recoverLocked 启动恢复规则（PRD §3.3）：
//   - running → pending（进程曾被中断）；
//   - retry_wait → pending（退避状态无意义，直接重排）；
//   - 清除运行中进度；终态记录原样保留供查看。
func (q *Queue) recoverLocked() {
	for _, t := range q.tasks {
		t.Progress = nil
		t.NextRetryAt = nil
		if t.State == StateRunning || t.State == StateRetryWait {
			t.State = StatePending // 进程曾被中断 / 退避状态无意义，重新排队
		}
	}
}

// activeCountLocked 统计某类型的活跃任务数（pending+running+retry_wait；
// excludeID 用于 Retry 场景排除自身——终态任务本就不计活跃）。
func (q *Queue) activeCountLocked(taskType, excludeID string) int {
	n := 0
	for _, t := range q.tasks {
		if t.Type != taskType || t.ID == excludeID {
			continue
		}
		if !terminalState(t.State) {
			n++
		}
	}
	return n
}

// Enqueue 追加任务入队（默认 pending）。task.ID 由本方法生成。
func (q *Queue) Enqueue(t *BatchTask) (*BatchTask, error) {
	if t == nil {
		return nil, errors.New("任务为空")
	}
	if t.Type != TypeDownload && t.Type != TypeInpaint {
		return nil, fmt.Errorf("未知任务类型: %s", t.Type)
	}
	q.mu.Lock()
	// 容量上限：每类队列活跃任务数封顶（方案 O4，防连续提交堆积）
	if n := q.activeCountLocked(t.Type, ""); n >= MaxActivePerQueue {
		q.mu.Unlock()
		return nil, fmt.Errorf("%s队列已满（上限 %d 条），请先处理或清理现有任务", taskTypeName(t.Type), MaxActivePerQueue)
	}
	t.ID = newID()
	t.State = StatePending
	t.Attempts = 0
	t.Err = ""
	t.CreatedAt = time.Now()
	// outDir 去重：同一输出目录不允许两个 pending/running 任务（避免互相清空）
	if t.Type == TypeInpaint && t.OutDir != "" {
		for _, e := range q.tasks {
			if e.Type == TypeInpaint && e.OutDir == t.OutDir &&
				(e.State == StatePending || e.State == StateRunning || e.State == StateRetryWait) {
				q.mu.Unlock()
				return nil, fmt.Errorf("该目录已在队列中（去水印输出目录不能重复）")
			}
		}
	}
	// 下载链接去重：同一链接不允许两个未完成任务（重复入队会重复下载同一目录，
	// 是「框选页出现重复图片」的诱因之一）
	if t.Type == TypeDownload {
		u := normalizeURL(t.URL)
		if u != "" {
			for _, e := range q.tasks {
				if e.Type == TypeDownload && normalizeURL(e.URL) == u &&
					(e.State == StatePending || e.State == StateRunning || e.State == StateRetryWait) {
					q.mu.Unlock()
					return nil, fmt.Errorf("该链接已在队列中，请勿重复添加")
				}
			}
		}
	}
	q.tasks = append(q.tasks, t)
	q.mu.Unlock()
	q.save()
	q.notifyUpdate()
	q.kickByType(t.Type)
	return t, nil
}

// Retry 手动重试：failed/canceled → pending，重试计数清零，按 FIFO 重新排队。
// 重试同样受每队列活跃上限约束（方案 O4）。
func (q *Queue) Retry(id string) error {
	q.mu.Lock()
	t := q.byIDLocked(id)
	if t == nil {
		q.mu.Unlock()
		return fmt.Errorf("任务不存在")
	}
	if !terminalState(t.State) {
		q.mu.Unlock()
		return fmt.Errorf("该任务未在终态，无法重试")
	}
	// 终态任务不计活跃，转 pending 后自身即占 1 个名额
	if n := q.activeCountLocked(t.Type, t.ID); n >= MaxActivePerQueue {
		typeName := taskTypeName(t.Type)
		q.mu.Unlock()
		return fmt.Errorf("%s队列已满（上限 %d 条），请先处理或清理现有任务再重试", typeName, MaxActivePerQueue)
	}
	t.State = StatePending
	t.Attempts = 0
	t.Err = ""
	t.FinishedAt = nil
	t.Progress = nil
	t.NextRetryAt = nil
	q.mu.Unlock()
	q.save()
	q.notifyUpdate()
	q.kickByType(t.Type)
	return nil
}

// Cancel 取消任务：
//   - pending / retry_wait → 直接置 canceled；
//   - running → 取消其 ctx（下载中断 / 推理中断 + 引擎 Kill 由 App 层处理），
//     终态由 execute 在执行器返回后落为 canceled。
func (q *Queue) Cancel(id string) error {
	q.mu.Lock()
	t := q.byIDLocked(id)
	if t == nil {
		q.mu.Unlock()
		return fmt.Errorf("任务不存在")
	}
	switch t.State {
	case StatePending, StateRetryWait:
		if timer := q.retryTimer[id]; timer != nil {
			timer.Stop()
			delete(q.retryTimer, id)
		}
		now := time.Now()
		t.State = StateCanceled
		t.FinishedAt = &now
		t.Progress = nil
		q.mu.Unlock()
		q.save()
		q.notifyUpdate()
		return nil
	case StateRunning:
		cancel := q.taskCancels[id]
		q.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if q.onCancel != nil {
			q.onCancel(t)
		}
		return nil
	default:
		q.mu.Unlock()
		return fmt.Errorf("任务已结束，无需取消")
	}
}

// CancelRunning 取消指定类型的运行中任务（taskType=TypeDownload/TypeInpaint）。
// CancelBatch 用 TypeInpaint 对齐旧「取消当前批次」语义；无运行任务时为空操作。
func (q *Queue) CancelRunning(taskType string) {
	q.mu.Lock()
	var id string
	switch taskType {
	case TypeDownload:
		id = q.runningD
	case TypeInpaint:
		id = q.runningI
	}
	q.mu.Unlock()
	if id != "" {
		_ = q.Cancel(id)
	}
}

// Remove 移除终态任务。
func (q *Queue) Remove(id string) error {
	q.mu.Lock()
	idx := -1
	for i, t := range q.tasks {
		if t.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		q.mu.Unlock()
		return fmt.Errorf("任务不存在")
	}
	if !terminalState(q.tasks[idx].State) {
		q.mu.Unlock()
		return fmt.Errorf("仅已结束的任务可移除")
	}
	q.tasks = append(q.tasks[:idx], q.tasks[idx+1:]...)
	q.mu.Unlock()
	q.save()
	q.notifyUpdate()
	return nil
}

// ClearFilter 清空筛选条件：按任务类型 + 时间区间定位要清除的终态任务。
//
// 空值语义（可自由组合）：
//   - Type 为空     → 不限类型（download / inpaint 都清）；
//   - Since 为零值  → 不限起始时间；
//   - Until 为零值  → 不限结束时间。
//
// 仅终态（done/failed/canceled）可被清除：这是硬约束，任何筛选条件都不能
// 删掉排队中/执行中的任务——否则用户点「清空」会静默丢掉正在跑的任务。
// 时间比较用**任务时刻**（见 TaskTime），与前端分组口径完全一致，
// 保证「界面上看到某组有几条」与「清空该组删了几条」两件事不会打架。
type ClearFilter struct {
	Type  string
	Since time.Time
	Until time.Time
}

// TaskTime 返回任务归属分组的判据时刻：终态用完成时刻，其余用创建时刻。
//
// 与前端 App.vue 的 taskTime 语义严格对齐（终态显示完成时刻、运行中显示
// 已执行时长、排队中显示创建时刻）——分组与卡片时间必须同一口径，
// 否则会出现「卡片写着昨天、却分在今天的组里」。
func TaskTime(t *BatchTask) time.Time {
	if terminalState(t.State) && t.FinishedAt != nil {
		return *t.FinishedAt
	}
	return t.CreatedAt
}

// ClearFinished 清空终态任务记录（兼容旧签名）。taskType 为空清全部；
// 否则仅清该类型（download | inpaint）——前端两类队列独立管理。
func (q *Queue) ClearFinished(taskType string) {
	q.ClearFinishedFiltered(ClearFilter{Type: taskType})
}

// matchedForClear 判断任务是否会被该筛选条件清除（纯判断，不改状态）。
//
// 与 ClearFinishedFiltered 共用同一套判据，避免两处逻辑漂移——预演结果
// 与实际删除结果必须严格一致，否则会误删「本来会保留」的任务文件。
func matchedForClear(t *BatchTask, f ClearFilter) bool {
	if !terminalState(t.State) {
		return false // 非终态永不删除
	}
	if f.Type != "" && t.Type != f.Type {
		return false
	}
	if !f.Since.IsZero() || !f.Until.IsZero() {
		at := TaskTime(t)
		if !f.Since.IsZero() && at.Before(f.Since) {
			return false
		}
		if !f.Until.IsZero() && !at.Before(f.Until) {
			return false // 左闭右开
		}
	}
	return true
}

// WouldClear 返回该筛选条件下将被清除的任务快照（不修改任何状态）。
//
// 供上层在真正清空前收集这些任务引用的本地文件路径——先算后删，
// 避免「记录已删、文件列表拿不到」的时序问题。
func (q *Queue) WouldClear(f ClearFilter) []BatchTask {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []BatchTask
	for _, t := range q.tasks {
		if matchedForClear(t, f) {
			out = append(out, *t)
		}
	}
	return out
}

// ClearFinishedFiltered 按筛选条件清空终态任务记录。
func (q *Queue) ClearFinishedFiltered(f ClearFilter) {
	q.mu.Lock()
	keep := q.tasks[:0]
	for _, t := range q.tasks {
		if !matchedForClear(t, f) {
			keep = append(keep, t) // 不匹配（含非终态）一律保留
		}
		// 匹配 → 移除
	}
	q.tasks = keep
	q.mu.Unlock()
	q.save()
	q.notifyUpdate()
}

// normalizeURL 规范化链接用于去重比较：去空白、去尾斜杠、统一小写主机部分。
// 仅用于队列内比较，不改变实际下载使用的原始链接。
func normalizeURL(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimRight(u, "/")
	if i := strings.Index(u, "://"); i >= 0 {
		head := strings.ToLower(u[:i+3])
		rest := u[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			u = strings.ToLower(rest[:j]) + rest[j:]
		} else {
			u = strings.ToLower(rest)
		}
		u = head + u
	}
	return u
}

// Snapshot 返回任务列表快照（值拷贝，调用方修改不影响队列内部状态）。
//
// 排序：按创建时刻**倒序**（最新在前）。前端按时间分组展示，分组内顺序
// 直接沿用本顺序（新→旧），无需再排一次。队列内部仍保持「入队先后」的
// 原始顺序——nextPendingByType 取最早 pending 依赖该顺序（FIFO），故只在
// 快照这一层排序，不动 q.tasks 本体。
//
// 同刻创建（同毫秒入队的批量链接）用 ID 兜底比较，保证顺序稳定可复现。
func (q *Queue) Snapshot() []BatchTask {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]BatchTask, len(q.tasks))
	for i, t := range q.tasks {
		out[i] = *t
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// ---------------------------------------------------------- 内部实现

// ResolveTaskAudio 回源补取指定下载任务的 BGM 并写回任务记录。
//
// 存在的意义：BGM 取源字段是随本次改造新增的，此前完成的任务（以及由旧版本
// 持久化文件恢复的任务）没有 BGM 数据，前端「去框选」时无从还原帖子上下文。
// 本方法按需重新解析该任务的链接并把结果落库——同一任务只补取一次
// （AudioChecked 置位），失败不影响下载内容，重试由前端触发。
//
// 返回该任务的候选链与曲目名；无 BGM 时返回空链且 err == nil。
func (q *Queue) ResolveTaskAudio(ctx context.Context, id string) ([]string, string, error) {
	q.mu.Lock()
	t := q.byIDLocked(id)
	if t == nil {
		q.mu.Unlock()
		return nil, "", fmt.Errorf("任务不存在")
	}
	if t.Type != TypeDownload {
		q.mu.Unlock()
		return nil, "", fmt.Errorf("仅下载任务有 BGM")
	}
	if t.AudioChecked {
		cands, name := t.AudioCandidates, t.AudioName
		q.mu.Unlock()
		return cands, name, nil
	}
	q.mu.Unlock()

	if q.exec == nil {
		return nil, "", fmt.Errorf("执行器未就绪")
	}
	cands, name, err := q.exec.ResolveTaskAudio(ctx, t)
	if err != nil {
		return nil, "", err
	}

	q.mu.Lock()
	if cur := q.byIDLocked(id); cur != nil {
		cur.AudioCandidates = cands
		cur.AudioName = name
		if len(cands) > 0 {
			cur.AudioURL = cands[0]
		} else {
			cur.AudioURL = ""
		}
		cur.AudioChecked = true
	}
	q.mu.Unlock()
	q.save()
	q.notifyUpdate()
	return cands, name, nil
}

// byIDLocked 按 ID 查任务（须持锁）。
func (q *Queue) byIDLocked(id string) *BatchTask {
	for _, t := range q.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// taskTypeName 任务类型的中文展示名（容量错误文案用）。
func taskTypeName(t string) string {
	if t == TypeInpaint {
		return "去水印"
	}
	return "下载"
}

// kickD / kickI 唤醒对应 worker（非阻塞，容量 1 合并多次唤醒）。
func (q *Queue) kickD() {
	select {
	case q.wakeD <- struct{}{}:
	default:
	}
}

func (q *Queue) kickI() {
	select {
	case q.wakeI <- struct{}{}:
	default:
	}
}

// kickByType 按任务类型唤醒对应 worker。
func (q *Queue) kickByType(taskType string) {
	if taskType == TypeInpaint {
		q.kickI()
		return
	}
	q.kickD()
}

// notifyUpdate 广播全量快照（队列规模小，免增量合并复杂度，PRD §4.3）。
func (q *Queue) notifyUpdate() {
	if q.notify == nil {
		return
	}
	q.notify("queue:updated", map[string]interface{}{"tasks": q.Snapshot()})
}

// nextPendingByType 取该类型队列中最早的 pending 任务（FIFO）。
func (q *Queue) nextPendingByType(taskType string) *BatchTask {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, t := range q.tasks {
		if t.Type == taskType && t.State == StatePending {
			return t
		}
	}
	return nil
}

// workerLoop 调度主循环（每类型一个）：唤醒后持续取本类型 pending 任务执行，
// 直到无任务或 ctx 取消。同类任务串行、跨类并行。
func (q *Queue) workerLoop(taskType string, wake chan struct{}) {
	defer q.wg.Done()
	for {
		select {
		case <-q.ctx.Done():
			return
		case <-wake:
		}
		for {
			if q.ctx.Err() != nil {
				return
			}
			t := q.nextPendingByType(taskType)
			if t == nil {
				break
			}
			q.execute(t)
		}
	}
}

// execute 执行单个任务并落终态（PRD §5 状态机与重试策略）。
func (q *Queue) execute(t *BatchTask) {
	taskCtx, cancel := context.WithCancel(q.ctx)

	q.mu.Lock()
	t.State = StateRunning
	t.Progress = nil
	t.Err = ""
	now := time.Now()
	t.StartedAt = &now
	t.FinishedAt = nil
	if t.Type == TypeInpaint {
		q.runningI = t.ID
	} else {
		q.runningD = t.ID
	}
	q.taskCancels[t.ID] = cancel
	q.mu.Unlock()
	q.save()
	q.notifyUpdate()

	var (
		err    error
		result InpaintResult
		dlRes  DownloadResult
	)
	switch t.Type {
	case TypeDownload:
		dlRes, err = q.exec.RunDownload(taskCtx, t)
	case TypeInpaint:
		onProgress := func(i, total int, name, status, info string) {
			q.mu.Lock()
			t.Progress = &TaskProgress{Index: i, Total: total, Name: name, Status: status}
			q.mu.Unlock()
			if q.notify != nil {
				// 双路推送：batch:* 供第三步页面内嵌进度（带 taskId 供前端防串扰），
				// queue:* 供队列面板（PRD §4.3）。
				q.notify("batch:progress", map[string]interface{}{
					"taskId": t.ID, "index": i, "total": total, "name": name, "status": status, "info": info,
				})
				q.notify("queue:progress", map[string]interface{}{
					"id": t.ID, "index": i, "total": total, "name": name, "status": status, "info": info,
				})
			}
		}
		result, err = q.exec.RunInpaint(taskCtx, t, onProgress)
	}

	// 读取取消状态必须在 cancel() 之前（cancel 会置 ctx.Err）
	wasCanceled := taskCtx.Err() != nil
	cancel()

	q.mu.Lock()
	delete(q.taskCancels, t.ID)
	if t.Type == TypeInpaint && q.runningI == t.ID {
		q.runningI = ""
	}
	if t.Type == TypeDownload && q.runningD == t.ID {
		q.runningD = ""
	}
	t.Progress = nil
	fin := time.Now()
	t.FinishedAt = &fin
	switch t.Type {
	case TypeDownload:
		t.ResultDir, t.PostRef, t.Files = dlRes.Dir, dlRes.PostRef, dlRes.Files
		// BGM 取源随任务落库。失败/取消时不写入（dlRes 为零值），避免用空值
		// 把重试前的既有结果覆盖掉。
		//
		// AudioChecked 只在「解析出了候选链」或「本轮确实解析过帖子、只是没有
		// BGM」时置位（后者由 dlRes.AudioResolved 标记）。仅在候选被健康检查
		// 全部过滤掉时保持未置位——这不是「该帖没有 BGM」，而是「暂时取不到
		// 可用地址」，应允许用户稍后重试补取。
		if err == nil && !wasCanceled {
			t.AudioURL, t.AudioCandidates, t.AudioName = dlRes.AudioURL, dlRes.Audio, dlRes.AudioNm
			if len(dlRes.Audio) > 0 || dlRes.AudioResolved {
				t.AudioChecked = true
			}
		}
	case TypeInpaint:
		if result.OutDir != "" {
			t.ResultDir = result.OutDir
		}
		t.Summary = fmt.Sprintf("成功 %d / 共 %d", result.OK, result.Total)
		if n := result.Total - result.OK; n > 0 {
			t.Summary = fmt.Sprintf("成功 %d / 失败或跳过 %d / 共 %d", result.OK, n, result.Total)
		}
		t.FailList = result.FailList
	}
	if wasCanceled {
		t.State = StateCanceled
		t.Err = "已取消"
	} else if err != nil {
		t.Err = errText(err)
		if IsPermanent(err) || t.Attempts >= MaxAutoRetries {
			t.State = StateFailed
		} else {
			// 瞬时错误：指数退避自动重试（重试 = 重走完整执行链，不复用旧地址）
			t.Attempts++
			t.State = StateRetryWait
			delay := RetryBackoffs[len(RetryBackoffs)-1]
			if t.Attempts-1 < len(RetryBackoffs) {
				delay = RetryBackoffs[t.Attempts-1]
			}
			due := time.Now().Add(delay)
			t.NextRetryAt = &due
			t.FinishedAt = nil
			id := t.ID
			q.retryTimer[id] = time.AfterFunc(delay, func() { q.retryDue(id) })
		}
	} else {
		t.State = StateDone
	}
	// 终态事件载荷：锁内取值拷贝，避免解锁后与其他 goroutine 竞争
	final := struct {
		id, typ, state, err, outDir, summary string
		ok, total                            int
	}{
		id: t.ID, typ: t.Type, state: t.State, err: t.Err, outDir: t.ResultDir, summary: t.Summary,
		ok: result.OK, total: result.Total,
	}
	q.mu.Unlock()
	q.save()
	q.notifyUpdate()

	// 终态双路推送（PRD §4.3）：queue:done 供队列面板；inpaint 任务同时发
	// batch:done（载荷与旧协议同构 + taskId），供第三步页面推进流水线状态机。
	if final.state == StateDone || final.state == StateFailed || final.state == StateCanceled {
		if q.notify != nil {
			q.notify("queue:done", map[string]interface{}{
				"id": final.id, "type": final.typ, "state": final.state,
				"err": final.err, "outDir": final.outDir, "summary": final.summary,
			})
			if final.typ == TypeInpaint {
				q.notify("batch:done", map[string]interface{}{
					"taskId": final.id, "ok": final.ok, "total": final.total,
					"outDir": final.outDir, "canceled": final.state == StateCanceled,
					"err": final.err, // 任务级失败原因（全部失败/执行出错），前端据此提示而非静默回退
				})
			}
		}
	}
}

// retryDue 退避到期：retry_wait → pending 并唤醒 worker（任务已不在 retry_wait
// 状态——如被取消/手动重试——则忽略）。
func (q *Queue) retryDue(id string) {
	q.mu.Lock()
	t := q.byIDLocked(id)
	if t == nil || t.State != StateRetryWait {
		q.mu.Unlock()
		return
	}
	t.State = StatePending
	t.NextRetryAt = nil
	delete(q.retryTimer, id)
	taskType := t.Type
	q.mu.Unlock()
	q.save()
	q.notifyUpdate()
	q.kickByType(taskType)
}

// save 持久化（调用方须在状态变更后调用；内部自行加锁避免与读竞争）。
func (q *Queue) save() {
	if q.path == "" {
		return
	}
	q.mu.Lock()
	tasks := make([]*BatchTask, len(q.tasks))
	copy(tasks, q.tasks)
	// 持久化前剥离瞬态字段（进度/退避时刻），恢复时无意义
	clean := make([]*BatchTask, len(tasks))
	for i, t := range tasks {
		cp := *t
		cp.Progress = nil
		cp.NextRetryAt = nil
		clean[i] = &cp
	}
	q.mu.Unlock()
	_ = SaveQueue(q.path, clean)
}

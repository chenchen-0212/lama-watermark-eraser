package inpaint

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	warmupTimeout = 180 * time.Second // 引擎预热（模型加载）超时
	inferTimeout  = 120 * time.Second // 单张推理超时
	killGrace     = 5 * time.Second   // 进程强杀后的回收兜底
	closeGrace    = 2 * time.Second   // 优雅关闭宽限期
	stderrRingCap = 200               // stderr 环形缓冲行数
)

// PythonEngine 管理一个常驻的 Python 去水印子进程，经 stdin/stdout JSONL 同步通信。
//
// 半双工同步：任意时刻至多一个请求在途（mu 串行化），写请求后阻塞读响应，
// 读写不交错，避免管道互锁。stderr 由独立 goroutine 持续排空防死锁。
type PythonEngine struct {
	exe  string
	Args []string // 引擎命令行参数（生产为空；开发期可传 worker.py --model ...）

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdoutRaw io.ReadCloser
	stdout    *bufio.Reader
	stderrRaw io.ReadCloser
	stderr    *stderrRing
	seq       int64
	ready     bool
	closed    bool
}

// NewPythonEngine 构造客户端。exe 为引擎可执行文件（生产环境为伴生
// lamacore/lamacore.exe；开发期可指向 python.exe 并配合 Args 指定 worker.py）。
func NewPythonEngine(exe string) *PythonEngine {
	return &PythonEngine{exe: exe, stderr: newStderrRing(stderrRingCap)}
}

// Start 拉起子进程、发送 hello 并等待 ready（预热超时 180s）。
// 已就绪时直接返回 nil；启动/预热失败会清理进程并返回带错误码的错误。
func (e *PythonEngine) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("引擎已关闭")
	}
	if e.ready {
		return nil
	}
	return e.startLocked(ctx)
}

// startLocked 在持有 mu 的前提下启动子进程并完成预热。
func (e *PythonEngine) startLocked(ctx context.Context) error {
	if e.cmd != nil {
		e.terminateLocked(0) // 清理上一个未就绪/已崩溃的进程
	}

	cmd := exec.Command(e.exe, e.Args...)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("创建引擎 stdin 管道失败: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("创建引擎 stdout 管道失败: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return fmt.Errorf("创建引擎 stderr 管道失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return fmt.Errorf("启动引擎失败(%s): %w", e.exe, err)
	}

	e.cmd = cmd
	e.stdin = stdin
	e.stdoutRaw = stdout
	e.stdout = bufio.NewReaderSize(stdout, 256*1024)
	e.stderrRaw = stderr
	e.stderr.reset()
	go e.drainStderr(stderr)

	id := atomic.AddInt64(&e.seq, 1)
	hello := NewRequest(MsgHello, id, map[string]any{
		"device":      "cpu",
		"num_threads": 0,
	})
	if err := e.writeLocked(hello); err != nil {
		e.terminateLocked(0)
		return newEngineError(CodeEngineStartFailed, "发送 hello 失败: %v", err)
	}

	resp, err := e.readResponseLocked(ctx, warmupTimeout)
	if err != nil {
		e.terminateLocked(0)
		return newEngineError(CodeEngineStartFailed, "引擎预热失败: %v", err)
	}
	if resp.ID != id || resp.Type != MsgHello || !resp.OK {
		e.terminateLocked(0)
		code := resp.ErrorCode()
		if code == "" {
			code = CodeEngineStartFailed
		}
		return newEngineError(code, "引擎预热失败: %s", resp.ErrorMessage())
	}
	e.ready = true
	return nil
}

// Infer 同步单张推理：rgb 为 w*h*3 RGB888，mask 为 w*h {0,255}；返回 w*h*3 RGB888。
// ctx 取消或超时(120s) -> 杀进程树并返回错误；下次调用懒重启。
func (e *PythonEngine) Infer(ctx context.Context, rgb, mask []byte, w, h int) ([]byte, error) {
	if w <= 0 || h <= 0 {
		return nil, newEngineError(CodeInvalidImage, "无效图像尺寸: %dx%d", w, h)
	}
	if len(rgb) != w*h*3 {
		return nil, newEngineError(CodeInvalidImage, "图像字节数不符: 期望 %d 实得 %d", w*h*3, len(rgb))
	}
	if len(mask) != w*h {
		return nil, newEngineError(CodeInvalidImage, "掩膜字节数不符: 期望 %d 实得 %d", w*h, len(mask))
	}
	if ctx == nil {
		ctx = context.Background()
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, fmt.Errorf("引擎已关闭")
	}
	if !e.ready {
		if err := e.startLocked(ctx); err != nil {
			return nil, err
		}
	}

	id := atomic.AddInt64(&e.seq, 1)
	req := NewRequest(MsgInfer, id, map[string]any{
		"width":     w,
		"height":    h,
		"image_b64": base64.StdEncoding.EncodeToString(rgb),
		"mask_b64":  base64.StdEncoding.EncodeToString(mask),
	})
	if err := e.writeLocked(req); err != nil {
		e.terminateLocked(0)
		return nil, newEngineError(CodeEngineCrashed, "写入推理请求失败: %v", err)
	}

	resp, err := e.readResponseLocked(ctx, inferTimeout)
	if err != nil {
		// 超时/取消/读失败（崩溃）统一杀进程树，下次懒重启
		e.terminateLocked(0)
		return nil, err
	}
	if resp.ID != id {
		e.terminateLocked(0)
		return nil, newEngineError(CodeEngineCrashed, "引擎响应 ID 不匹配: 期望 %d 实得 %d", id, resp.ID)
	}
	if !resp.OK {
		code := resp.ErrorCode()
		if code == "" {
			code = CodeInferFailed
		}
		return nil, newEngineError(code, "推理失败: %s", resp.ErrorMessage())
	}

	out, err := base64.StdEncoding.DecodeString(resp.StringData("image_b64"))
	if err != nil {
		e.terminateLocked(0)
		return nil, newEngineError(CodeEngineCrashed, "解码引擎输出失败: %v", err)
	}
	if len(out) != w*h*3 {
		e.terminateLocked(0)
		return nil, newEngineError(CodeEngineCrashed, "引擎输出尺寸不符: 期望 %d 实得 %d", w*h*3, len(out))
	}
	return out, nil
}

// Kill 立即终止引擎进程树（Windows taskkill /F /T /PID，兜底 Process.Kill）。
func (e *PythonEngine) Kill() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.terminateLocked(0)
	return nil
}

// Close 优雅关闭：发送 shutdown 后短暂等待，超时则强杀进程树。
func (e *PythonEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.ready && e.stdin != nil {
		id := atomic.AddInt64(&e.seq, 1)
		_ = e.writeLocked(NewRequest(MsgShutdown, id, nil))
	}
	e.terminateLocked(closeGrace)
	return nil
}

// IsReady 返回引擎是否已预热完成。
func (e *PythonEngine) IsReady() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ready
}

// LastStderr 返回最近的 stderr 日志（诊断用，最多 stderrRingCap 条）。
func (e *PythonEngine) LastStderr() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stderr.snapshot()
}

// writeLocked 序列化并写入一行 JSON（含换行）。调用方需持有 mu。
func (e *PythonEngine) writeLocked(m *Message) error {
	b, err := EncodeMessage(m)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if e.stdin == nil {
		return fmt.Errorf("引擎 stdin 未就绪")
	}
	if _, err := e.stdin.Write(b); err != nil {
		return err
	}
	// StdinPipe 底层为无缓冲 *os.File，单次 Write 即到达子进程；
	// 若未来替换为带缓冲 writer，再补充 Flush。
	if f, ok := e.stdin.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

// readResponseLocked 读取并解析一行 JSONL 响应（带超时与取消，backstop 读 deadline）。
// 读 goroutine 捕获 reader/raw 局部变量，避免 terminateLocked 替换字段后的数据竞争。
func (e *PythonEngine) readResponseLocked(ctx context.Context, timeout time.Duration) (*Message, error) {
	reader := e.stdout
	raw := e.stdoutRaw

	// backstop：确保读 goroutine 最终一定能解除阻塞（即使未及时 kill）
	if f, ok := raw.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = f.SetReadDeadline(time.Now().Add(timeout + killGrace))
		defer func() { _ = f.SetReadDeadline(time.Time{}) }()
	}

	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := reader.ReadBytes('\n')
		ch <- result{line, err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("读取引擎响应失败: %w", r.err)
		}
		line := bytes.TrimSpace(r.line)
		if len(line) == 0 {
			return nil, fmt.Errorf("引擎返回空响应")
		}
		msg, err := DecodeMessage(line)
		if err != nil {
			return nil, fmt.Errorf("解析引擎响应失败: %w", err)
		}
		return msg, nil
	case <-timer.C:
		return nil, newEngineError(CodeEngineTimeout, "引擎响应超时(%s)", timeout)
	case <-ctx.Done():
		return nil, newEngineError(CodeCanceled, "操作已取消: %v", ctx.Err())
	}
}

// terminateLocked 终止并回收当前子进程。grace 为优雅等待时长（0 表示立即强杀）。
// 必须在持有 e.mu 时调用；保证 cmd.Wait 只被调用一次。
func (e *PythonEngine) terminateLocked(grace time.Duration) {
	cmd := e.cmd
	if cmd == nil || cmd.Process == nil {
		e.resetPipesLocked()
		return
	}
	e.cmd = nil // 立即置空，防重入

	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()

	select {
	case <-reaped:
		// 已在宽限期内自行退出
	case <-time.After(grace):
		killProcessTree(cmd.Process.Pid)
		_ = cmd.Process.Kill()
		select {
		case <-reaped:
		case <-time.After(killGrace):
		}
	}

	e.resetPipesLocked()
}

// resetPipesLocked 关闭所有管道并复位状态。调用方需持有 mu。
func (e *PythonEngine) resetPipesLocked() {
	if e.stdin != nil {
		_ = e.stdin.Close()
		e.stdin = nil
	}
	if e.stdoutRaw != nil {
		_ = e.stdoutRaw.Close()
		e.stdoutRaw = nil
	}
	if e.stderrRaw != nil {
		_ = e.stderrRaw.Close()
		e.stderrRaw = nil
	}
	e.stdout = nil
	e.ready = false
}

// drainStderr 持续排空 stderr，防子进程写满管道阻塞。
func (e *PythonEngine) drainStderr(r io.Reader) {
	br := bufio.NewReaderSize(r, 16*1024)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			e.stderr.add(strings.TrimRight(line, "\r\n"))
		}
		if err != nil {
			return
		}
	}
}

// killProcessTree 尽力杀净整棵进程树（Windows taskkill /F /T；其余平台兜底 Kill）。
func killProcessTree(pid int) {
	if runtime.GOOS == "windows" {
		c := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
		c.Stdout = io.Discard
		c.Stderr = io.Discard
		_ = c.Run()
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// engineError 携带协议错误码的引擎错误。
type engineError struct {
	Code string
	Msg  string
}

func (e *engineError) Error() string { return e.Msg }

func newEngineError(code, format string, args ...any) error {
	return &engineError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// EngineErrorCode 提取引擎错误码（非引擎错误返回空串）。
func EngineErrorCode(err error) string {
	var ee *engineError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return ""
}

// stderrRing 线程安全的环形日志缓冲，保存最近 N 行 stderr 用于诊断。
type stderrRing struct {
	mu    sync.Mutex
	lines []string
	cap   int
}

func newStderrRing(cap int) *stderrRing {
	if cap <= 0 {
		cap = stderrRingCap
	}
	return &stderrRing{cap: cap}
}

func (r *stderrRing) add(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.cap {
		r.lines = r.lines[len(r.lines)-r.cap:]
	}
}

func (r *stderrRing) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = nil
}

func (r *stderrRing) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

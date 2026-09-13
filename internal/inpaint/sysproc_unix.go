//go:build unix

package inpaint

import "syscall"

// hideEngineWindowSysProcAttr 类 Unix 平台无控制台窗口概念；这里利用
// SysProcAttr 把引擎子进程放入独立进程组（Setpgid），使退出/取消时可按
// 进程组整组回收 —— 对应 Windows 侧 Job Object 的兜底能力（macOS/Linux
// 无 Job Object，弱化为进程组 kill）。
func hideEngineWindowSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 向子进程所在进程组发送 SIGKILL。
// pid 为进程组组长（子进程 Setpgid 后 pgid == pid），负号表示"整个进程组"。
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

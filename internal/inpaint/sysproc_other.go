//go:build !windows && !unix

package inpaint

import "syscall"

// hideEngineWindowSysProcAttr 非 Windows / 非 Unix 平台（如 plan9）既无控制台
// 窗口概念也无进程组，返回空属性。
func hideEngineWindowSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// killProcessGroup 该平台无进程组语义，为空实现（调用方另有兜底 Kill）。
func killProcessGroup(pid int) {}

//go:build !windows

package inpaint

import "syscall"

// hideEngineWindowSysProcAttr 非 Windows 平台无控制台窗口概念，返回空属性。
func hideEngineWindowSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

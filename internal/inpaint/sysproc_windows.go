//go:build windows

package inpaint

import "syscall"

// hideEngineWindowSysProcAttr 返回隐藏子进程控制台窗口的进程属性。
//
// GUI 模式下引擎为控制台子系统程序（lamacore.exe 因 stdio 协议必须保留
// console 子系统），不设置时会在桌面弹出黑色控制台窗口。
// CREATE_NO_WINDOW (0x08000000) 使子进程不创建控制台；HideWindow 进一步
// 兜底隐藏窗口。二者均不影响 stdin/stdout/stderr 管道。
func hideEngineWindowSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

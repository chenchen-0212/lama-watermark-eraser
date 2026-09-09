//go:build windows

package inpaint

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobHandle 引擎进程所属 Job Object 的内核句柄（Windows 实现）。
type jobHandle = windows.Handle

// createKillOnCloseJob 创建带 KILL_ON_JOB_CLOSE 限定的作业对象。
//
// KILL_ON_JOB_CLOSE：宿主进程退出（含崩溃、taskkill /F 强杀）时，内核关闭
// 其持有的作业句柄并终止作业内全部进程 —— 这是引擎子进程不残留的内核级
// 兜底（第一重保险为 wails OnShutdown 钩子的优雅关闭）。
// Win8+ 支持嵌套作业；Win7 上若父进程已处于不允许 breakaway 的作业内会失败，
// 调用方降级为记日志忽略。
func createKillOnCloseJob() (jobHandle, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(h)
		return 0, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return h, nil
}

// assignProcessToJob 将子进程加入作业。句柄需 PROCESS_SET_QUOTA |
// PROCESS_TERMINATE 访问权限；子进程加入后，其后续派生的子进程树均继承
// 作业成员资格。
func assignProcessToJob(h jobHandle, pid int) error {
	const access = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE
	ph, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(ph) }()
	if err := windows.AssignProcessToJobObject(h, ph); err != nil {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", pid, err)
	}
	return nil
}

// closeJob 关闭作业句柄。作业内若仍有残留进程，KILL_ON_JOB_CLOSE 会将其
// 终止；作业已空时为无害操作。
func closeJob(h jobHandle) {
	if h != 0 {
		_ = windows.CloseHandle(h)
	}
}

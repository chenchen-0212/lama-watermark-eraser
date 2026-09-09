//go:build !windows

package inpaint

// jobHandle 非 Windows 平台的占位句柄类型（无 Job Object 机制）。
type jobHandle uintptr

// createKillOnCloseJob 非 Windows 平台无作业对象，恒返回零值句柄（未托管）。
func createKillOnCloseJob() (jobHandle, error) { return 0, nil }

// assignProcessToJob 非 Windows 平台为空实现。
func assignProcessToJob(h jobHandle, pid int) error { return nil }

// closeJob 非 Windows 平台为空实现。
func closeJob(h jobHandle) {}

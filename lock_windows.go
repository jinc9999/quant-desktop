//go:build windows

package main

import "golang.org/x/sys/windows"

// stillActive 是 GetExitCodeProcess 在进程仍在运行时返回的退出码（259）。
const stillActive uint32 = 259

// isProcessAlive 探测指定 PID 的进程是否存活。
// Windows 实现：OpenProcess 打开进程句柄（仅查询权限），再用 GetExitCodeProcess
// 判断退出码是否为 STILL_ACTIVE(259)。进程不存在或已退出时返回 false。
func isProcessAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}

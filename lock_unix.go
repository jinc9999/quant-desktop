//go:build !windows

package main

import (
	"os"
	"syscall"
)

// isProcessAlive 探测指定 PID 的进程是否存活。
// Unix 实现：信号 0 仅探测进程存活，不发送任何信号。
func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

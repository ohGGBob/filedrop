//go:build windows

package core

import (
	"os/exec"
	"syscall"
)

// hideWindow 隐藏子进程控制台窗口，避免弹出黑色命令行窗口闪烁。
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

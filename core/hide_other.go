//go:build !windows

package core

import "os/exec"

// hideWindow 非 Windows 平台无需处理，空实现。
func hideWindow(cmd *exec.Cmd) {}

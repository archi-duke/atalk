//go:build windows

package procs

import (
	"os/exec"
	"strconv"
	"syscall"
)

// KillTree 는 pid 와 그 아래 프로세스(도구로 띄운 셸·빌드 등)를 모두 강제로 끝낸다.
func KillTree(pid int) error {
	cmd := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return cmd.Run()
}

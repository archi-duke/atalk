//go:build !windows

package procs

import (
	"syscall"
	"time"
)

// KillTree 는 pid 와 그 아래 프로세스를 모두 끝낸다. 헤드리스 턴은 새 세션(Setsid)으로 띄우므로 프로세스 그룹 전체에 보낸다.
// 먼저 TERM 으로 정리할 틈을 주고, 3초 안에 끝나지 않으면 KILL.
func KillTree(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		return syscall.Kill(pid, syscall.SIGKILL)
	}
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if syscall.Kill(-pid, 0) != nil {
			return nil
		}
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

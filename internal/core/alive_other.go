//go:build !windows

package core

import (
	"errors"
	"syscall"
)

// DefaultIsAlive 는 프로세스가 아직 실행 중인지 확인한다.
func DefaultIsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

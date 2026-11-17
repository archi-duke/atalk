//go:build !windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func detach(cmd *exec.Cmd, _ bool) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func lockFile(f *os.File) error   { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) }
func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

func AutostartCommand() (string, error) {
	return "", errors.New("자동 시작 등록은 아직 Windows 만 지원함")
}

// SetAutostart: macOS(launchd)/Linux(systemd --user) 는 아직 미지원.
func SetAutostart(on bool) error {
	return errors.New("자동 시작 등록은 아직 Windows 만 지원함 (launchd / systemd --user 로 `atalk daemon start` 를 등록하세요)")
}

func Autostart() string { return "" }

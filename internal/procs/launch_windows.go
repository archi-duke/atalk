//go:build windows

package procs

import (
	"os/exec"
	"strings"
	"syscall"
)

// LaunchTerminal 은 cwd 에서 새 터미널 창을 열어 argv 를 실행한다.
// Windows Terminal(wt)이 있으면 새 창(탭)으로, 없으면 새 콘솔 창의 PowerShell 로. 명령이 끝나도 창은 남는다.
// argv 는 ResumeCommand 가 만든 안전한 토큰만 (따옴표·공백 없음).
func LaunchTerminal(cwd string, argv []string) error {
	ps := []string{"powershell.exe", "-NoLogo", "-NoExit", "-Command", strings.Join(argv, " ")}
	var cmd *exec.Cmd
	if wt, err := exec.LookPath("wt.exe"); err == nil {
		cmd = exec.Command(wt, append([]string{"-w", "new", "-d", cwd}, ps...)...)
	} else {
		cmd = exec.Command("cmd.exe", "/c", "start", "ATalk", "/D", cwd, ps[0], strings.Join(ps[1:], " "))
	}
	cmd.Dir = cwd
	// 데몬은 창 없이 돌므로, 새 창이 콘솔을 따로 갖게 한다
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010 /* CREATE_NEW_CONSOLE */}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

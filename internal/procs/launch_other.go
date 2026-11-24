//go:build !windows

package procs

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// LaunchTerminal 은 cwd 에서 새 터미널 창을 열어 argv 를 실행한다 (macOS: Terminal, Linux: x-terminal-emulator·gnome-terminal).
// argv 는 ResumeCommand 가 만든 안전한 토큰만.
func LaunchTerminal(cwd string, argv []string) error {
	q := "'" + strings.ReplaceAll(cwd, "'", `'\''`) + "'"
	line := "cd " + q + " && " + strings.Join(argv, " ")
	if runtime.GOOS == "darwin" {
		esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(line)
		return exec.Command("osascript", "-e", `tell application "Terminal" to do script "`+esc+`"`, "-e", `tell application "Terminal" to activate`).Start()
	}
	for _, t := range [][]string{{"x-terminal-emulator", "-e"}, {"gnome-terminal", "--"}, {"konsole", "-e"}, {"xterm", "-e"}} {
		if p, err := exec.LookPath(t[0]); err == nil {
			return exec.Command(p, t[1], "bash", "-lc", line+"; exec bash").Start()
		}
	}
	return errors.New("터미널 프로그램을 찾지 못함")
}

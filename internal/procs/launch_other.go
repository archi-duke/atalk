//go:build !windows

package procs

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// LaunchTerminal 은 cwd 에서 새 터미널 창을 열어 argv 를 실행한다 (macOS: Terminal, Linux: x-terminal-emulator·gnome-terminal).
// 인자는 하나씩 따옴표로 감싸므로 공백이 있는 글(첫 메시지)도 그대로 전달된다.
func LaunchTerminal(cwd string, argv []string) error {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shQuote(a)
	}
	line := "cd " + shQuote(cwd) + " && " + strings.Join(q, " ")
	if runtime.GOOS == "darwin" {
		esc := strings.NewReplacer(`\`, `\`, `"`, `\"`).Replace(line)
		return exec.Command("osascript", "-e", `tell application "Terminal" to do script "`+esc+`"`, "-e", `tell application "Terminal" to activate`).Start()
	}
	for _, t := range [][]string{{"x-terminal-emulator", "-e"}, {"gnome-terminal", "--"}, {"konsole", "-e"}, {"xterm", "-e"}} {
		if p, err := exec.LookPath(t[0]); err == nil {
			return exec.Command(p, t[1], "bash", "-lc", line+"; exec bash").Start()
		}
	}
	return errors.New("터미널 프로그램을 찾지 못함")
}

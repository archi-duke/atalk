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
func LaunchTerminal(cwd string, argv []string) error { return LaunchTerminalEnv(cwd, argv, nil) }

// LaunchTerminalEnv 는 LaunchTerminal 에 더해 새 창의 프로세스에 환경 변수를 준다 (atalk spawn 의 연결 표식).
func LaunchTerminalEnv(cwd string, argv []string, env map[string]string) error {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shQuote(a)
	}
	pre := "unset " + strings.Join(sessionEnv, " ") + " && " // 에이전트 세션 안에서 시작된 데몬의 환경이 새 창으로 넘어가지 않게
	for k, v := range env {
		pre += "export " + k + "=" + shQuote(v) + " && "
	}
	line := "cd " + shQuote(cwd) + " && " + pre + strings.Join(q, " ")
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

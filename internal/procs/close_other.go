//go:build !windows

package procs

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CloseAgentWindow 는 에이전트와 그것을 실행한 셸을 끝내고, macOS 면 그 터미널 창(탭 하나짜리)도 닫는다.
func CloseAgentWindow(pid int) error {
	tty := ""
	if b, err := exec.Command("ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output(); err == nil {
		tty = strings.TrimSpace(string(b))
	}
	var first error
	for _, p := range windowChain(pid) {
		if err := syscall.Kill(p, syscall.SIGKILL); err != nil && first == nil {
			first = err
		}
	}
	if runtime.GOOS == "darwin" && tty != "" && !strings.HasPrefix(tty, "?") {
		if !strings.HasPrefix(tty, "/dev/") {
			tty = "/dev/" + tty
		}
		time.Sleep(500 * time.Millisecond)
		script := `tell application "Terminal"
repeat with w in windows
if (count of tabs of w) is 1 and tty of tab 1 of w is "` + tty + `" then close w
end repeat
end tell`
		_ = exec.Command("osascript", "-e", script).Run()
	}
	return first
}

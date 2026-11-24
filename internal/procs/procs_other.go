//go:build !windows

package procs

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var psLine = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(.*)$`)

func list() (map[int]Proc, error) {
	b, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil, err
	}
	out := map[int]Proc{}
	for _, line := range strings.Split(string(b), "\n") {
		m := psLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		ppid, _ := strconv.Atoi(m[2])
		out[pid] = Proc{PID: pid, PPID: ppid, Name: filepath.Base(m[3])}
	}
	return out, nil
}

// exePath 는 프로세스 명령줄 (실행 파일 경로 + 인자). 모르면 "".
func exePath(pid int) string {
	b, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

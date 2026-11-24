package server

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"atalk/internal/core"
	"atalk/internal/daemon"
	"atalk/internal/procs"
)

// wakeWait 는 깨운 세션이 다시 등록(훅·플러그인)될 때까지 기다리는 시간. 그 뒤 창을 앞으로 가져온다.
const wakeWait = 60 * time.Second

// focusPid 는 그 프로세스의 터미널 창을 맨 앞으로 (도우미 프로세스 atalk focus-window).
func focusPid(pid int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "focus-window", "--pid", strconv.Itoa(pid))
	daemon.HideCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return &core.Error{Status: 409, Code: "focus_failed", Message: strings.TrimSpace(string(out))}
	}
	return nil
}

func (s *Server) wake(id string) (any, error) {
	c := s.Core
	v, err := c.GetAgent(id)
	if err != nil {
		return nil, err
	}
	if !v.Local {
		return nil, &core.Error{Status: 409, Code: "not_here", Message: "이 PC 의 에이전트만 깨울 수 있음 (다른 PC 는 그 PC 에서)"}
	}
	if v.Pid != nil && v.Status != "offline" { // 이미 떠 있음 → 창만 앞으로
		return map[string]any{"launched": false}, focusPid(*v.Pid)
	}
	if v.Cwd == nil || *v.Cwd == "" {
		return nil, &core.Error{Status: 409, Code: "no_cwd", Message: "작업 폴더를 몰라 다시 띄울 수 없음"}
	}
	if st, err := os.Stat(*v.Cwd); err != nil || !st.IsDir() {
		return nil, &core.Error{Status: 409, Code: "no_cwd", Message: "작업 폴더가 없음: " + *v.Cwd}
	}
	argv, err := procs.ResumeCommand(v.Kind, v.SessionID)
	if err != nil {
		return nil, &core.Error{Status: 409, Code: "wake_unsupported", Message: err.Error()}
	}
	before := 0
	if v.Pid != nil {
		before = *v.Pid
	}
	if err := procs.LaunchTerminal(*v.Cwd, argv); err != nil {
		return nil, &core.Error{Status: 500, Code: "launch_failed", Message: "터미널을 열 수 없음: " + err.Error()}
	}
	// 세션이 다시 등록되면(새 PID) 그 창을 앞으로. 새 창은 보통 이미 앞에 있지만, 백그라운드 데몬이 연 창은
	// Windows 가 앞으로 못 오게 막을 때가 있어 한 번 더 올린다.
	go func() {
		for deadline := time.Now().Add(wakeWait); time.Now().Before(deadline); time.Sleep(time.Second) {
			if a, err := c.GetAgent(id); err == nil && a.Pid != nil && *a.Pid != before && a.Status != "offline" {
				time.Sleep(500 * time.Millisecond)
				_ = focusPid(*a.Pid)
				return
			}
		}
	}()
	return map[string]any{"launched": true, "command": strings.Join(argv, " ")}, nil
}

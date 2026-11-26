package server

import (
	"os/exec"
	"runtime"
	"testing"
	"time"

	"atalk/internal/daemon"
)

// 급할 때 중단: 도는 턴의 프로세스(와 그 아래)를 끝내고, 중단 표시를 남긴다.
func TestStopTurn(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "ping -n 30 127.0.0.1 >NUL")
	}
	daemon.HideCmd(cmd)
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	s := &Server{}
	s.headless.mu.Lock()
	s.headless.init()
	s.headless.cmds["a"] = cmd
	s.headless.mu.Unlock()
	if err := s.stopTurn("a"); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("프로세스가 끝나지 않음")
	}
	if !s.headless.stopped["a"] {
		t.Fatal("중단 표시")
	}
	if err := s.stopTurn("없음"); err == nil {
		t.Fatal("도는 턴이 없으면 오류")
	}
}

package server

import (
	"strings"
	"testing"

	"atalk/internal/core"
	"atalk/internal/procs"
)

// 깨우기: 이어서 여는 명령, 그리고 띄우지 않고 거절하는 경우 (다른 PC, 지원하지 않는 종류, 작업 폴더 없음).
// 실제로 터미널을 여는 것은 수동으로 확인한다.
func TestWake(t *testing.T) {
	if a, err := procs.ResumeCommand("claude-code", "9e4a2798-f7a4"); err != nil || strings.Join(a, " ") != "claude --resume 9e4a2798-f7a4" {
		t.Fatalf("claude: %v %v", a, err)
	}
	if a, err := procs.ResumeCommand("opencode", "ses_abc"); err != nil || strings.Join(a, " ") != "opencode -s ses_abc" {
		t.Fatalf("opencode: %v %v", a, err)
	}
	if _, err := procs.ResumeCommand("claude-code", "x; rm -rf /"); err == nil {
		t.Fatal("세션 id 에 셸 문자가 있으면 거절")
	}
	if _, err := procs.ResumeCommand("gemini-cli", "x"); err == nil {
		t.Fatal("지원하지 않는 종류")
	}

	srv, err := New(Options{Root: t.TempDir(), Addr: "127.0.0.1:0", MachineID: "host-pc"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c := srv.Core
	dead := 999999 // 없는 PID → offline
	reg := func(in core.RegisterInput) string {
		t.Helper()
		in.Pid = &dead
		r, err := c.Register(in)
		if err != nil {
			t.Fatal(err)
		}
		return r.Agent.ID
	}
	missing := t.TempDir() + "/없는폴더"
	noDir := reg(core.RegisterInput{Kind: "claude-code", SessionID: "s1", Cwd: &missing})
	if _, err := srv.wake(noDir); err == nil || !strings.Contains(err.Error(), "작업 폴더") {
		t.Fatalf("작업 폴더가 없으면 거절: %v", err)
	}
	dir := t.TempDir()
	gem := reg(core.RegisterInput{Kind: "gemini-cli", SessionID: "s2", Cwd: &dir})
	if _, err := srv.wake(gem); err == nil || !strings.Contains(err.Error(), "지원") {
		t.Fatalf("지원하지 않는 종류: %v", err)
	}
	remote, _, err := c.PairRemote(core.RegisterInput{Kind: "claude-code", SessionID: "s3", Host: &core.Host{ID: "other", OS: "mac"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.wake(remote.Agent.ID); err == nil || !strings.Contains(err.Error(), "이 PC") {
		t.Fatalf("다른 PC 에이전트는 거절: %v", err)
	}
}

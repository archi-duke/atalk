package server

import (
	"os"
	"strings"
	"testing"

	"atalk/internal/core"
	"atalk/internal/procs"
)

// 시스템 확인 창: 뜨면 그 창을 띄운 에이전트(프로세스 조상)에게 🙋 + 방에 카드, 일을 이어 가도 남고, 창이 닫히면 풀림.
// 다른 확인 요청으로 바뀌었으면 창이 닫혀도 건드리지 않음.
func TestWatchPrompts(t *testing.T) {
	srv, err := New(Options{Root: t.TempDir(), Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c := srv.Core
	pid := os.Getpid()
	reg := func(sid string) string {
		t.Helper()
		r, err := c.Register(core.RegisterInput{Kind: "claude-code", SessionID: sid, Pid: &pid})
		if err != nil {
			t.Fatal(err)
		}
		return r.Agent.ID
	}
	a := reg("prompt-a")
	if _, err := c.CreateRoom(a, "r", "", 0); err != nil {
		t.Fatal(err)
	}

	var cur []procs.Prompt
	old := osPrompts
	osPrompts = func() []procs.Prompt { return cur }
	defer func() { osPrompts = old }()

	srv.watchPrompts() // 창 없음 → 아무것도 안 함
	cur = []procs.Prompt{{ID: "fw-1", Kind: "firewall", Program: "server.test.exe", Pid: pid}}
	srv.watchPrompts()
	v, _ := c.GetAgent(a)
	if !strings.Contains(v.NeedsInput, "방화벽") || !strings.Contains(v.NeedsInput, "server.test.exe") {
		t.Fatalf("확인 필요 표시: %q", v.NeedsInput)
	}
	srv.watchPrompts() // 같은 창은 한 번만
	n := 0
	msgs, _ := c.ReadMessages("r", 0, 100)
	for _, m := range msgs {
		if strings.Contains(m.Body, "방화벽 허용 창") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("방 카드 %d개", n)
	}
	_ = c.SetActivity(a, true) // 일을 이어 가도 창이 떠 있는 동안은 남음
	if v, _ := c.GetAgent(a); v.NeedsInput == "" {
		t.Fatal("일을 이어 가도 남아 있어야 함")
	}
	cur = nil
	srv.watchPrompts()
	if v, _ := c.GetAgent(a); v.NeedsInput != "" {
		t.Fatalf("창이 닫히면 풀려야 함: %q", v.NeedsInput)
	}

	// 창이 떠 있는 사이 다른 확인 요청(도구 허가)으로 바뀌면 창이 닫혀도 그대로
	cur = []procs.Prompt{{ID: "uac-9", Kind: "uac"}}
	srv.watchPrompts()
	if v, _ := c.GetAgent(a); !strings.Contains(v.NeedsInput, "관리자 권한") {
		t.Fatalf("UAC 알림 (프로세스 모름 → 방에 있는 이 PC 에이전트): %q", v.NeedsInput)
	}
	_ = c.SetNeedInput(a, "Bash 실행 허가: rm x")
	cur = nil
	srv.watchPrompts()
	if v, _ := c.GetAgent(a); v.NeedsInput != "Bash 실행 허가: rm x" {
		t.Fatalf("다른 확인 요청은 두어야 함: %q", v.NeedsInput)
	}
}

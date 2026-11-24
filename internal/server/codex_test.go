package server

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/procs"
)

// Codex 깨우기: 쉬고 있는 Codex 에이전트에게 온 메시지·인사 규칙을 codex queue 로 한 번만 넣고 읽음 처리한다.
func TestCodexWaker(t *testing.T) {
	if a, err := procs.ResumeCommand("codex", "01a20dae-34a7-7d02-b895-45abf1cd124c"); err != nil || strings.Join(a[:4], " ") != "codex resume --no-daemon 01a20dae-34a7-7d02-b895-45abf1cd124c" || a[4] != procs.CodexWakePrompt {
		t.Fatalf("codex resume: %v %v", a, err)
	}
	if !core.IsSystemPrompt("[ATalk] remote-test 방에서 새 메시지가 왔습니다") || !core.IsSystemPrompt("[ATalk 규칙] 인사") {
		t.Fatal("ATalk 알림은 CLI 입력 기록에서 빠져야 함")
	}

	srv, err := New(Options{Root: t.TempDir(), Addr: "127.0.0.1:0", MachineID: "pc"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	port, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	cl := client.New(fmt.Sprintf("http://127.0.0.1:%d", port))
	c := srv.Core
	pid := os.Getpid()
	dir := t.TempDir()
	reg := func(kind, sid string) string {
		t.Helper()
		r, err := c.Register(core.RegisterInput{Kind: kind, SessionID: sid, Cwd: &dir, Pid: &pid})
		if err != nil {
			t.Fatal(err)
		}
		return r.Agent.ID
	}
	owner, cx := reg("claude-code", "own"), reg("codex", "thr-1")
	if _, err := c.CreateRoom(owner, "r1", "시험", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Invite(owner, "r1", cx); err != nil { // 같은 PC → 바로 입장 (인사 규칙 대기)
		t.Fatal(err)
	}

	clock := time.Unix(1000, 0)
	var queued []string
	w := &CodexWaker{Now: func() time.Time { return clock }, Queue: func(sid, text string) error {
		if sid != "thr-1" {
			t.Fatalf("세션 id: %s", sid)
		}
		queued = append(queued, text)
		return nil
	}}
	check := func() bool {
		t.Helper()
		v, err := c.GetAgent(cx)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := w.Check(cl, v)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	// 처음 본 순간에는 넣지 않고, 3초 쉬고 나서 인사 규칙을 넣는다
	if check() {
		t.Fatal("쉬기 시작하자마자 넣음")
	}
	clock = clock.Add(4 * time.Second)
	if !check() || !strings.HasPrefix(queued[0], "[ATalk 규칙] r1 방에 새로 들어왔습니다") {
		t.Fatalf("인사 규칙: %q", queued)
	}
	clock = clock.Add(4 * time.Second)
	check()
	clock = clock.Add(4 * time.Second)
	if check() {
		t.Fatal("인사 규칙을 두 번 넣음")
	}

	// 작업 중이면 넣지 않는다 (Stop 훅이 턴 끝에서 처리)
	if _, err := c.Post(owner, "r1", core.PostInput{Body: "코덱스 안녕", To: []string{cx}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetActivity(cx, true); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Second)
	if check() {
		t.Fatal("작업 중에 넣음")
	}
	if err := c.SetActivity(cx, false); err != nil {
		t.Fatal(err)
	}
	check()
	clock = clock.Add(4 * time.Second)
	if !check() || len(queued) != 2 || !strings.Contains(queued[1], "코덱스 안녕") || !strings.HasPrefix(queued[1], "[ATalk] r1 방에서 새 메시지") {
		t.Fatalf("메시지: %q", queued)
	}
	if u, _ := cl.Unread(cx, false); len(u.Messages) != 0 {
		t.Fatalf("넣은 메시지는 읽음 처리: %v", u.Messages)
	}
	clock = clock.Add(4 * time.Second)
	check()
	clock = clock.Add(4 * time.Second)
	if check() {
		t.Fatal("같은 메시지를 다시 넣음")
	}
}

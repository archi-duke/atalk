package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"atalk/internal/client"
	"atalk/internal/core"
)

// 내보낸 HTML 에는 이 방과 관계있는 에이전트만, 경로·PID·PC 정보 없이 들어간다.
func TestExportHidesPaths(t *testing.T) {
	srv, err := New(Options{Root: t.TempDir(), Addr: "127.0.0.1:0", MachineID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c := srv.Core
	secret := `C:\Users\someone\secret-proj`
	pid := 4321
	a, err := c.Register(core.RegisterInput{Kind: "claude-code", SessionID: "a", Cwd: &secret, Pid: &pid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Register(core.RegisterInput{Kind: "codex", SessionID: "outsider", Cwd: &secret}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRoom(a.Agent.ID, "r", "", 0); err != nil {
		t.Fatal(err)
	}
	page, err := srv.ExportHTML("r")
	if err != nil {
		t.Fatal(err)
	}
	s := string(page)
	for _, bad := range []string{"secret-proj", "someone", "4321", "outsider", `"host"`, "workspace_id"} {
		if strings.Contains(s, bad) {
			t.Fatalf("내보낸 파일에 %q 가 들어 있음", bad)
		}
	}
	if !strings.Contains(s, a.Agent.ID) {
		t.Fatal("방 멤버는 들어 있어야 함")
	}
}

func TestHTTPFlow(t *testing.T) {
	srv, err := New(Options{Root: t.TempDir(), Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	port, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c := client.New(fmt.Sprintf("http://127.0.0.1:%d", port))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	f := false // 초대 → 수락 흐름을 직접 시험 (같은 PC 대기 중 자동 입장 끔)
	if _, err := srv.Core.SetConfig(core.Config{AutoAcceptInvites: &f}); err != nil {
		t.Fatal(err)
	}
	name := "Lead"
	ra, err := c.Register(core.RegisterInput{Kind: "claude-code", SessionID: "s1", Name: &name})
	must(err)
	rb, err := c.Register(core.RegisterInput{Kind: "codex", SessionID: "s2"})
	must(err)
	a, b := ra.Agent.ID, rb.Agent.ID
	_, err = c.CreateRoom(a, "한글방", "테스트")
	must(err)
	must(c.Invite(a, "한글방", b))
	p, err := c.Pending(b)
	must(err)
	if len(p.Invites) != 1 || p.Invites[0].Room != "한글방" {
		t.Fatal(p)
	}
	must(c.RespondInvite(b, "한글방", true))

	done := make(chan client.WaitResult)
	go func() {
		w, err := c.Wait(b, 10, true)
		if err != nil {
			t.Error(err)
		}
		done <- w
	}()
	time.Sleep(100 * time.Millisecond)
	_, err = c.Post(a, "한글방", core.PostInput{Type: "task", To: []string{b}, Body: "PR 리뷰"})
	must(err)
	w := <-done
	if w.Reason != "message" {
		t.Fatal(w.Reason)
	}
	var task *core.InboxMessage
	for i := range w.Messages {
		if w.Messages[i].ForMe {
			task = &w.Messages[i]
		}
	}
	if task == nil || task.Body != "PR 리뷰" {
		t.Fatal("task not delivered", w.Messages)
	}
	must(c.Log(b, "한글방", task.Seq, "received", ""))
	_, err = c.Post(b, "한글방", core.PostInput{Type: "result", ReplyTo: &task.Seq, Body: "LGTM"})
	must(err)
	must(c.Log(b, "한글방", task.Seq, "done", "리뷰 완료"))
	logs, err := c.Logs("한글방", "")
	must(err)
	if len(logs) != 2 || logs[0].State != "received" || logs[1].State != "done" {
		t.Fatal(logs)
	}
	tw, err := c.Wait(b, 1, true)
	must(err)
	if tw.Reason != "timeout" {
		t.Fatal(tw.Reason)
	}
	_, err = c.Post(b, "없는방", core.PostInput{Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "없는 대화방") {
		t.Fatal(err)
	}
	res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	must(err)
	html, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(html), "ATalk") {
		t.Fatal("viewer")
	}
}

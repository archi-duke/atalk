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

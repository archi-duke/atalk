package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type env struct {
	t     *testing.T
	root  string
	clock time.Time
	alive map[int]bool
}

func newEnv(t *testing.T) *env {
	return &env{t: t, root: t.TempDir(), clock: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), alive: map[int]bool{100: true, 200: true, 300: true}}
}

func (e *env) core() *Atalk {
	c, err := New(Options{Root: e.root, AwayGrace: time.Minute, IsAlive: func(p int) bool { return e.alive[p] }, Now: func() time.Time { return e.clock }})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// ok 은 오류가 있으면 panic 한다 (테스트 실패로 이어짐). Go 는 f(t, g()) 형태를 허용하지 않아 t 를 받지 않는다.
func ok[T any](v T, err error) T {
	if err != nil {
		panic("unexpected error: " + err.Error())
	}
	return v
}

func nok(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("expected error containing %q, got %v", contains, err)
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func s(v string) *string { return &v }
func i(v int) *int       { return &v }

func setup(t *testing.T, c *Atalk) (a, b, d string) {
	a = ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "aaa", Name: s("A"), Pid: i(100)})).Agent.ID
	b = ok(c.Register(RegisterInput{Kind: "codex", SessionID: "bbb", Name: s("B"), Pid: i(200)})).Agent.ID
	d = ok(c.Register(RegisterInput{Kind: "gemini-cli", SessionID: "ccc", Name: s("C"), Pid: i(300)})).Agent.ID
	return
}

func members(t *testing.T, c *Atalk, r string) []string {
	v := ok(c.GetRoom(r))
	out := []string{}
	for _, m := range v.Members {
		out = append(out, m.Agent)
	}
	return out
}

func TestRegisterCardFile(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, _, _ := setup(t, c)
	if a != "claude-code_aaa" {
		t.Fatal(a)
	}
	if _, err := os.Stat(filepath.Join(e.root, "agents", "claude-code_aaa.json")); err != nil {
		t.Fatal(err)
	}
	if ok(c.GetAgent(a)).Status != "waiting" {
		t.Fatal("not waiting")
	}
	if n := len(ok(c.ListAgents("waiting", ""))); n != 3 {
		t.Fatal(n)
	}
}

func TestInviteMessageUnread(t *testing.T) {
	c := newEnv(t).core()
	a, b, d := setup(t, c)
	ok(c.CreateRoom(a, "api", "API 작업", 0))
	if *ok(c.GetAgent(a)).Role != "owner" {
		t.Fatal("not owner")
	}
	_, err := c.Invite(b, "api", d)
	nok(t, err, "방장만")
	ok(c.Invite(a, "api", b))
	if len(ok(c.Pending(b)).Invites) != 1 {
		t.Fatal("no invite")
	}
	ok(c.RespondInvite(b, "api", true))
	if !slices.Equal(members(t, c, "api"), []string{a, b}) {
		t.Fatal(members(t, c, "api"))
	}
	m := ok(c.Post(a, "api", PostInput{Type: "task", To: []string{b}, Body: "리뷰해 줘"}))
	u := ok(c.Unread(b, false))
	var forMe []int
	for _, x := range u.Messages {
		if x.ForMe {
			forMe = append(forMe, x.Seq)
		}
	}
	if !slices.Equal(forMe, []int{m.Seq}) || !c.HasWork(b) {
		t.Fatal(forMe)
	}
	ok(c.Unread(b, true))
	if c.HasWork(b) {
		t.Fatal("still work after ack")
	}
	for _, x := range ok(c.Unread(a, false)).Messages {
		if x.ForMe {
			t.Fatal("own message is for_me")
		}
	}
	_, err = c.Post(d, "api", PostInput{Body: "끼어들기"})
	nok(t, err, "멤버가 아님")
	_, err = c.Post(a, "api", PostInput{Body: "x", To: []string{d}})
	nok(t, err, "멤버가 아님")
}

func TestRequestApprove(t *testing.T) {
	c := newEnv(t).core()
	a, _, d := setup(t, c)
	ok(c.CreateRoom(a, "docs", "", 0))
	ok(c.RequestJoin(d, "docs", "문서 도와줄게요"))
	if len(ok(c.Pending(a)).Requests) != 1 {
		t.Fatal("no request")
	}
	ok(c.RespondRequest(a, "docs", d, true))
	if *ok(c.GetAgent(d)).Room != "docs" || len(ok(c.Pending(a)).Requests) != 0 {
		t.Fatal("not joined")
	}
}

func TestOneRoomPerSession(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, d := setup(t, c)
	ok(c.CreateRoom(a, "r1", "", 0))
	ok(c.Invite(a, "r1", b))
	ok(c.RespondInvite(b, "r1", true))
	ok(c.CreateRoom(d, "r2", "", 0))
	ok(c.Invite(d, "r2", a))
	ok(c.RespondInvite(a, "r2", true))
	if *ok(c.GetAgent(a)).Room != "r2" || *ok(c.GetRoom("r1")).Owner != b {
		t.Fatal("move/owner transfer failed")
	}
	if !slices.Equal(members(t, c, "r1"), []string{b}) {
		t.Fatal(members(t, c, "r1"))
	}
	raw, _ := os.ReadFile(filepath.Join(e.root, "agents", a+".json"))
	var card map[string]any
	_ = json.Unmarshal(raw, &card)
	if _, has := card["room"]; has {
		t.Fatal("card must not contain room")
	}
}

func TestLastMemberClosesRoom(t *testing.T) {
	c := newEnv(t).core()
	a, _, _ := setup(t, c)
	ok(c.CreateRoom(a, "solo", "", 0))
	check(t, c.Leave(a, ""))
	if ok(c.GetRoom("solo")).ClosedAt == nil || len(ok(c.ListRooms(false))) != 0 {
		t.Fatal("not closed")
	}
	if ok(c.GetAgent(a)).Status != "waiting" {
		t.Fatal("not waiting")
	}
	_, err := c.RequestJoin(a, "solo", "")
	nok(t, err, "닫힌")
}

func TestCrashAwayAndRecovery(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	ok(c.CreateRoom(a, "w", "", 0))
	ok(c.Invite(a, "w", b))
	ok(c.RespondInvite(b, "w", true))
	t1 := ok(c.Post(a, "w", PostInput{Type: "task", To: []string{b}, Body: "작업1"}))
	ok(c.Unread(b, true))
	ok(c.Log(b, "w", t1.Seq, "working", "진행 중"))

	delete(e.alive, 200)
	check(t, c.Sweep())
	if ok(c.GetAgent(b)).Status != "away" {
		t.Fatal("not away")
	}
	t2 := ok(c.Post(a, "w", PostInput{Type: "task", To: []string{b}, Body: "작업2"}))
	e.alive[201] = true
	r := ok(c.Register(RegisterInput{Kind: "codex", SessionID: "bbb", Pid: i(201)}))
	if r.Agent.Status != "in_room" || *r.Agent.Pid != 201 {
		t.Fatal("not recovered", r.Agent.Status)
	}
	if len(r.Recovery.Unfinished) != 1 || r.Recovery.Unfinished[0].Seq != t1.Seq {
		t.Fatal(r.Recovery.Unfinished)
	}
	if !slices.ContainsFunc(r.Recovery.Unread, func(m InboxMessage) bool { return m.Seq == t2.Seq && m.ForMe }) {
		t.Fatal("missed message not in recovery")
	}
}

func TestAwayGraceExpire(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	ok(c.CreateRoom(a, "g", "", 0))
	ok(c.Invite(a, "g", b))
	ok(c.RespondInvite(b, "g", true))
	check(t, c.MarkAway(b, "session_end"))
	e.clock = e.clock.Add(30 * time.Second)
	check(t, c.Sweep())
	if ok(c.GetAgent(b)).Status != "away" {
		t.Fatal("expired too early")
	}
	e.clock = e.clock.Add(31 * time.Second)
	check(t, c.Sweep())
	if ok(c.GetAgent(b)).Status != "waiting" || !slices.Equal(members(t, c, "g"), []string{a}) {
		t.Fatal("not expired")
	}
}

func TestTakeover(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "aaa", Cwd: s("D:/proj")}))
	ok(c.CreateRoom(a, "t", "", 0))
	ok(c.Invite(a, "t", b))
	ok(c.RespondInvite(b, "t", true))
	task := ok(c.Post(b, "t", PostInput{Type: "task", To: []string{a}, Body: "해 줘"}))
	ok(c.Log(a, "t", task.Seq, "received", ""))

	e.alive[101] = true
	a2 := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "aaa2", Cwd: s("D:/proj"), Pid: i(101)})).Agent.ID
	_, err := c.Takeover(a2, a)
	nok(t, err, "살아 있음")
	delete(e.alive, 100)
	other := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "zzz", Cwd: s("D:/other")})).Agent.ID
	_, err = c.Takeover(other, a)
	nok(t, err, "작업 폴더")

	rec := ok(c.Takeover(a2, a))
	if rec.Room != "t" || *ok(c.GetRoom("t")).Owner != a2 || len(rec.Unfinished) != 1 || rec.Unfinished[0].Seq != task.Seq {
		t.Fatal("takeover wrong", rec)
	}
	if ok(c.GetAgent(a)).Status != "offline" {
		t.Fatal("old not offline")
	}
}

func TestRestartRecovery(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	ok(c.CreateRoom(a, "p", "", 0))
	ok(c.Invite(a, "p", b))
	ok(c.RespondInvite(b, "p", true))
	m1 := ok(c.Post(a, "p", PostInput{Body: "hello"}))
	f, _ := os.OpenFile(filepath.Join(e.root, "rooms", "p", "messages.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"seq":99,"bro`)
	f.Close()

	c = e.core()
	if *ok(c.GetRoom("p")).Owner != a || *ok(c.GetAgent(b)).Room != "p" {
		t.Fatal("state not restored")
	}
	m2 := ok(c.Post(b, "p", PostInput{Body: "world"}))
	if m2.Seq != m1.Seq+1 {
		t.Fatal(m2.Seq)
	}
	msgs := ok(c.ReadMessages("p", 0, 200))
	if msgs[len(msgs)-2].Body != "hello" || msgs[len(msgs)-1].Body != "world" {
		t.Fatal(msgs)
	}
}

func TestRestartReconcile(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	ok(c.CreateRoom(a, "q", "", 0))
	ok(c.Invite(a, "q", b))
	ok(c.RespondInvite(b, "q", true))
	check(t, os.Remove(filepath.Join(e.root, "agents", a+".json")))
	c = e.core()
	if *ok(c.GetRoom("q")).Owner != b || !slices.Equal(members(t, c, "q"), []string{b}) {
		t.Fatal("reconcile failed")
	}
}

func TestRoomIDValidation(t *testing.T) {
	c := newEnv(t).core()
	a, _, _ := setup(t, c)
	_, err := c.CreateRoom(a, "../x", "", 0)
	nok(t, err, "방 ID")
	_, err = c.CreateRoom(a, "CON", "", 0)
	nok(t, err, "방 ID")
	ok(c.CreateRoom(a, "리팩터링-1", "", 0))
	if *ok(c.GetAgent(a)).Room != "리팩터링-1" {
		t.Fatal("korean room")
	}
}

// TS 구현이 만든 데이터 폴더를 Go 구현이 그대로 읽을 수 있어야 한다 (형식 호환).
func TestReadsTSFormat(t *testing.T) {
	e := newEnv(t)
	must2 := func(p, s string) {
		check(t, os.MkdirAll(filepath.Dir(filepath.Join(e.root, p)), 0o755))
		check(t, os.WriteFile(filepath.Join(e.root, p), []byte(s), 0o644))
	}
	must2("agents/codex_x1.json", `{"id":"codex_x1","kind":"codex","session_id":"x1","name":"X","description":"","capabilities":[],"cwd":null,"pid":null,"registered_at":"2026-11-17T00:00:00.000Z","updated_at":"2026-11-17T00:00:00.000Z"}`)
	must2("rooms/r/room.json", `{"room_id":"r","title":"R","owner":"codex_x1","created_at":"2026-11-17T00:00:00.000Z","closed_at":null,"invites":[],"requests":[],"policy":{"max_messages":1000}}`)
	must2("rooms/r/messages.jsonl", `{"seq":1,"id":"m","ts":"2026-11-17T00:00:00.000Z","room":"r","from":"system","type":"system","to":[],"reply_to":null,"body":"X 입장 (방장)"}`+"\n")
	must2("rooms/r/cursors.json", `{"codex_x1":0}`)
	must2("memberships.json", `{"codex_x1":{"room":"r","role":"owner","state":"in_room","joined_at":"2026-11-17T00:00:00.000Z","last_seen":"2026-11-17T00:00:00.000Z","away_since":null,"away_reason":null}}`)
	c := e.core()
	m := ok(c.Post("codex_x1", "r", PostInput{Body: "go 에서 이어 씀"}))
	if m.Seq != 2 || *ok(c.GetAgent("codex_x1")).Room != "r" {
		t.Fatal("format compat failed")
	}
}

func TestHumanMessage(t *testing.T) {
	c := newEnv(t).core()
	ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "m"}))
	ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o"}))
	ok(c.CreateRoom("claude-code_m", "r", "", 0))
	ok(c.Invite("claude-code_m", "r", "opencode_o"))
	ok(c.RespondInvite("opencode_o", "r", true))

	// 별칭이 없으면 "사용자"
	m := ok(c.Post("claude-code_m", "r", PostInput{Body: "안녕?", Human: true}))
	if m.Human != "사용자" {
		t.Fatalf("기본 별칭: %q", m.Human)
	}
	// config 기본값 → 카드 덮어쓰기
	ok(c.SetConfig("대장"))
	if m = ok(c.Post("claude-code_m", "r", PostInput{Body: "x", Human: true})); m.Human != "대장" {
		t.Fatalf("config 별칭: %q", m.Human)
	}
	ok(c.UpdateCard("opencode_o", CardPatch{Human: ptr("부대장")}))
	if m = ok(c.Post("opencode_o", "r", PostInput{Body: "y", Human: true})); m.Human != "부대장" {
		t.Fatalf("카드 별칭: %q", m.Human)
	}
	// 에이전트 메시지에는 human 이 없다
	if m = ok(c.Post("claude-code_m", "r", PostInput{Body: "z"})); m.Human != "" {
		t.Fatalf("에이전트 메시지에 human: %q", m.Human)
	}
	_, err := c.SetConfig("대@장")
	nok(t, err, "별칭은")
	// 사람 메시지도 받는 쪽 for_me
	u := ok(c.Unread("opencode_o", false))
	if len(u.Messages) < 2 || u.Messages[1].Human != "사용자" || !u.Messages[1].ForMe {
		t.Fatalf("unread: %+v", u.Messages)
	}
}

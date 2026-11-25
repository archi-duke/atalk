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
	e.t.Cleanup(func() { _ = c.Close() })
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

// noAutoAccept 는 같은 PC 대기 중 자동 입장을 꺼서 초대 → 수락 흐름을 직접 시험하게 한다.
func noAutoAccept(c *Atalk) {
	f := false
	ok(c.SetConfig(Config{AutoAcceptInvites: &f}))
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
	if _, err := os.Stat(filepath.Join(e.root, "atalk.db")); err != nil {
		t.Fatal(err)
	}
	if v := ok(e.core().GetAgent(a)); v.Name != "A" { // 다시 열어도 그대로
		t.Fatalf("재시작 후 카드: %+v", v)
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
	noAutoAccept(c) // 초대 → 직접 수락 흐름을 본다
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
	noAutoAccept(c) // 초대 → 직접 수락 흐름을 본다
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
	// 카드가 사라진 상황 (예전엔 파일 삭제, 지금은 DB 행 삭제)
	check(t, c.guard(func() error { c.st.exec("DELETE FROM agents WHERE id = ?", a); return nil }))
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
	must2("rooms/r/logs/codex_x1.jsonl", `{"ts":"2026-11-17T00:00:00.000Z","agent":"codex_x1","seq":1,"state":"received","note":""}`+"\n")
	must2("trash/old-20261101-000000/room.json", `{"room_id":"old","title":"지운 방","owner":null,"created_at":"2026-11-01T00:00:00.000Z","closed_at":null,"invites":[],"requests":[],"policy":{"max_messages":1000}}`)
	must2("trash/old-20261101-000000/messages.jsonl", `{"seq":1,"id":"o","ts":"2026-11-01T00:00:00.000Z","room":"old","from":"system","type":"system","to":[],"reply_to":null,"body":"옛 메시지"}`+"\n")
	must2("config.json", `{"user":"대장"}`)
	must2("names.json", `{"gone_1":"사라진 에이전트"}`)
	c := e.core()
	m := ok(c.Post("codex_x1", "r", PostInput{Body: "go 에서 이어 씀"}))
	if m.Seq != 2 || *ok(c.GetAgent("codex_x1")).Room != "r" {
		t.Fatal("format compat failed")
	}
	if l := ok(c.ReadLogs("r", "")); len(l) != 1 || l[0].State != "received" {
		t.Fatalf("처리 기록: %+v", l)
	}
	if n, _ := c.TrashInfo(); n != 1 {
		t.Fatalf("휴지통: %d", n)
	}
	if cfg := ok(c.GetConfig()); cfg.User != "대장" {
		t.Fatal("설정")
	}
	if names := ok(c.Names()); names["gone_1"] != "사라진 에이전트" {
		t.Fatal("이름")
	}
	// 원래 파일은 legacy-<시각>/ 으로 옮겨지고, 다시 열어도 두 번 가져오지 않는다
	if _, err := os.Stat(filepath.Join(e.root, "agents")); !os.IsNotExist(err) {
		t.Fatal("예전 파일이 그대로 있음")
	}
	legacy, _ := filepath.Glob(filepath.Join(e.root, "legacy-*", "rooms", "r", "messages.jsonl"))
	if len(legacy) != 1 {
		t.Fatal("legacy 폴더 없음")
	}
	c2 := e.core()
	if msgs := ok(c2.ReadMessages("r", 0, 100)); len(msgs) != 2 {
		t.Fatalf("다시 열었을 때 메시지 %d개", len(msgs))
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
	ok(c.SetConfig(Config{User: "대장"}))
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
	_, err := c.SetConfig(Config{User: "대@장"})
	nok(t, err, "별칭은")
	// 사람 메시지도 받는 쪽 for_me
	u := ok(c.Unread("opencode_o", false))
	if len(u.Messages) < 2 || u.Messages[1].Human != "사용자" || !u.Messages[1].ForMe {
		t.Fatalf("unread: %+v", u.Messages)
	}
}

// 사람이 (방장 이름으로) 쓴 말에 대한 답은 사람에게 가는 것 — 방장 에이전트를 깨우지 않는다. 방장 자신이 보낸 일의 답은 그대로 깨운다.
func TestReplyToHumanNotForAgent(t *testing.T) {
	c := newEnv(t).core()
	ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "m"}))
	ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o"}))
	ok(c.CreateRoom("claude-code_m", "r", "", 0))
	ok(c.Invite("claude-code_m", "r", "opencode_o"))
	ok(c.RespondInvite("opencode_o", "r", true))
	ok(c.Unread("claude-code_m", true))

	h := ok(c.Post("claude-code_m", "r", PostInput{Body: "남은 작업?", Human: true, To: []string{"opencode_o"}}))
	ok(c.Post("opencode_o", "r", PostInput{Type: "result", Body: "없음", To: []string{"claude-code_m"}, ReplyTo: ptr(h.Seq)}))
	if c.HasWork("claude-code_m") {
		t.Fatal("사람 말에 대한 답이 방장 에이전트를 깨움")
	}
	// 진행 상황(status)은 reply_to 없이 방장에게 보내도 깨우지 않음
	ok(c.Post("opencode_o", "r", PostInput{Type: "status", Body: "#63 진행 중", To: []string{"claude-code_m"}}))
	if c.HasWork("claude-code_m") {
		t.Fatal("status 가 에이전트를 깨움")
	}
	ok(c.Unread("claude-code_m", true))
	task := ok(c.Post("claude-code_m", "r", PostInput{Type: "task", Body: "확인해 줘", To: []string{"opencode_o"}}))
	ok(c.Post("opencode_o", "r", PostInput{Type: "result", Body: "완료", To: []string{"claude-code_m"}, ReplyTo: ptr(task.Seq)}))
	if !c.HasWork("claude-code_m") {
		t.Fatal("방장이 보낸 일의 답은 방장에게 가야 함")
	}
}

func TestResumePrevious(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "aaa", Cwd: s("D:/proj")}))
	ok(c.CreateRoom(a, "t", "", 0))
	ok(c.Invite(a, "t", b))
	ok(c.RespondInvite(b, "t", true))

	// 이전 세션이 살아 있고 다른 프로세스면 이어받지 않는다
	e.alive[101] = true
	a2 := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "aaa2", Cwd: s("D:/proj"), Pid: i(101)})).Agent.ID
	_, _, err := c.ResumePrevious(a2)
	nok(t, err, "이어받을 이전 세션 없음")

	// 다른 폴더 세션은 후보가 아님
	delete(e.alive, 100)
	other := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "zzz", Cwd: s("D:/other")})).Agent.ID
	_, _, err = c.ResumePrevious(other)
	nok(t, err, "이어받을 이전 세션 없음")

	// 이전 세션 프로세스가 죽으면 자동 이어받기 → 방장 유지, 이전 카드는 offline(retired)
	rec, from := func() (*Recovery, string) { r, f, err := c.ResumePrevious(a2); check(t, err); return r, f }()
	if from != a || rec.Room != "t" || *ok(c.GetRoom("t")).Owner != a2 {
		t.Fatalf("resume wrong: from=%s rec=%+v", from, rec)
	}
	if v := ok(c.GetAgent(a)); v.Status != "offline" || v.RetiredBy == nil || *v.RetiredBy != a2 {
		t.Fatalf("old card: %+v", v)
	}
	if v := ok(c.GetAgent(a2)); v.Name != "A" {
		t.Fatalf("이름을 이어받지 않음: %q", v.Name)
	}

	// 같은 프로세스의 새 세션(Claude /clear, opencode 새 세션)은 살아 있는 이전 세션 자리도 이어받는다
	a3 := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "aaa3", Cwd: s("D:/proj"), Pid: i(101)})).Agent.ID
	if _, f, err := c.ResumePrevious(a3); err != nil || f != a2 {
		t.Fatalf("same pid resume: %v %s", err, f)
	}
	// 이미 방에 있으면 안 함
	_, _, err = c.ResumePrevious(a3)
	nok(t, err, "이미 방에 있음")
	// 이어받긴 세션이 같은 id 로 다시 등록되면 retired 해제 (방은 없음)
	if v := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "aaa2"})).Agent; v.RetiredBy != nil || v.Room != nil {
		t.Fatalf("re-register: %+v", v)
	}
}

func TestGraceConfig(t *testing.T) {
	e := newEnv(t)
	c := e.core() // 데몬 기본 1분
	a, b, _ := setup(t, c)
	ok(c.CreateRoom(a, "g", "", 0))
	ok(c.Invite(a, "g", b))
	ok(c.RespondInvite(b, "g", true))
	check(t, c.MarkAway(b, "test"))

	// 0 = 자동 퇴장 안 함
	ok(c.SetConfig(Config{AwayGraceMin: i(0)}))
	e.clock = e.clock.Add(10 * time.Hour)
	check(t, c.Sweep())
	if !slices.Contains(members(t, c, "g"), b) {
		t.Fatal("grace 0 인데 퇴장됨")
	}
	// 120분: 아직 안 지났으면 유지, 지나면 퇴장
	ok(c.SetConfig(Config{AwayGraceMin: i(120)}))
	check(t, c.Sweep())
	if slices.Contains(members(t, c, "g"), b) {
		t.Fatal("10시간 > 120분 인데 남아 있음")
	}
	_, err := c.SetConfig(Config{AwayGraceMin: i(-1)})
	nok(t, err, "away_grace_min")
}

// 초대 수락·신청 승인으로 새로 들어오면 인사·자기소개 전(Intro) — 방에 글을 쓰면 꺼진다. 방장·사람 메시지는 해당 없음.
func TestIntroOnJoin(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	noAutoAccept(c)
	a, b, d := setup(t, c)
	ok(c.CreateRoom(a, "w", "", 0))
	if ok(c.GetAgent(a)).Intro {
		t.Fatal("방을 만든 방장은 인사 대상이 아님")
	}
	ok(c.Invite(a, "w", b))
	ok(c.RespondInvite(b, "w", true))
	if !ok(c.GetAgent(b)).Intro {
		t.Fatal("초대 수락으로 들어오면 Intro")
	}
	ok(c.Say(b, "w", "사람이 쓴 말")) // 사람 메시지는 에이전트의 인사가 아님
	if !ok(c.GetAgent(b)).Intro {
		t.Fatal("사람 메시지로 Intro 가 꺼지면 안 됨")
	}
	// 데몬을 다시 시작해도 유지
	c = e.core()
	if !ok(c.GetAgent(b)).Intro {
		t.Fatal("재시작 뒤 Intro 유지")
	}
	ok(c.Post(b, "w", PostInput{Body: "안녕하세요, B 입니다."}))
	if ok(c.GetAgent(b)).Intro {
		t.Fatal("방에 글을 쓰면 Intro 꺼짐")
	}
	// 참여 신청 승인도 같다
	ok(c.RequestJoin(d, "w", ""))
	if ok(c.GetAgent(d)).Room != nil {
		t.Fatal("승인 전에 들어가면 안 됨")
	}
	ok(c.RespondRequest(a, "w", d, true))
	if !ok(c.GetAgent(d)).Intro {
		t.Fatal("신청 승인으로 들어오면 Intro")
	}
}

func TestHumanToSelf(t *testing.T) {
	c := newEnv(t).core()
	a, b, _ := setup(t, c)
	ok(c.CreateRoom(a, "w", "", 0))
	ok(c.Invite(a, "w", b))
	ok(c.RespondInvite(b, "w", true))
	ok(c.Ack(a, ok(c.GetRoom("w")).LastSeq))
	// 웹에서 "@A 해 줘" (통로 = A) → A 자신도 받는다
	m := ok(c.Say(a, "w", "@A 해 줘"))
	if m.Human == "" || len(m.To) != 1 || m.To[0] != a {
		t.Fatalf("say: %+v", m)
	}
	u := ok(c.Unread(a, false))
	if len(u.Messages) != 1 || !u.Messages[0].ForMe {
		t.Fatalf("A 가 자기 사람의 메시지를 못 받음: %+v", u.Messages)
	}
	// 웹에서 방 전체에 "모두 인사해 봐" (통로 = A) → A 도 받고 처리 책임이 있다. B 도 받는다
	ok(c.Ack(a, m.Seq))
	ok(c.Ack(b, m.Seq))
	all := ok(c.Say(a, "w", "모두 인사해 봐"))
	if u := ok(c.Unread(a, false)); len(u.Messages) != 1 || u.Messages[0].Seq != all.Seq || !u.Messages[0].ForMe {
		t.Fatalf("A 가 방 전체에 보낸 자기 사람의 메시지를 못 받음: %+v", u.Messages)
	}
	if u := ok(c.Unread(b, false)); len(u.Messages) != 1 || !u.Messages[0].ForMe {
		t.Fatalf("B: %+v", u.Messages)
	}
	// 다른 에이전트를 지정한 말("@B 해 줘")은 A 가 받지 않는다
	ok(c.Ack(a, all.Seq))
	ok(c.Say(a, "w", "@B 해 줘"))
	if u := ok(c.Unread(a, false)); len(u.Messages) != 0 {
		t.Fatalf("B 에게 보낸 말을 A 가 받음: %+v", u.Messages)
	}
	// 에이전트가 자기에게 보낸 일반 메시지는 여전히 받지 않는다
	ok(c.Ack(a, ok(c.GetRoom("w")).LastSeq))
	ok(c.Post(a, "w", PostInput{Body: "혼잣말", To: []string{a}}))
	if u := ok(c.Unread(a, false)); len(u.Messages) != 0 {
		t.Fatalf("에이전트 자기 메시지를 받음: %+v", u.Messages)
	}
	_, err := c.Say(a, "w", "@없음 hi")
	nok(t, err, "없음")
}

func TestRoomRenameDelete(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	ok(c.CreateRoom(a, "x", "처음 제목", 0))
	ok(c.Invite(a, "x", b))
	ok(c.RespondInvite(b, "x", true))

	v := ok(c.RenameRoom("x", "  새 제목 ", ""))
	if v.Title != "새 제목" {
		t.Fatalf("rename: %q", v.Title)
	}
	_, err := c.RenameRoom("x", "a\nb", "")
	nok(t, err, "제목")

	ok(c.DeleteRoom("x"))
	if n, size := c.TrashInfo(); n != 1 || size == 0 {
		t.Fatalf("휴지통: %d %d", n, size)
	}
	if ok(c.GetAgent(a)).Status != "waiting" || ok(c.GetAgent(b)).Room != nil {
		t.Fatal("멤버가 방에서 나오지 않음")
	}
	_, err = c.GetRoom("x")
	nok(t, err, "x")
	// 같은 ID 로 다시 만들 수 있고, 재시작해도 지운 방은 다시 나타나지 않는다
	ok(c.CreateRoom(b, "x", "", 0))
	c2 := e.core()
	if r := ok(c2.GetRoom("x")); *r.Owner != b || len(r.Members) != 1 {
		t.Fatalf("재생성/재시작: %+v", r)
	}
}

func TestOfflinePrune(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	ok(c.CreateRoom(a, "p", "", 0))
	ok(c.Post(a, "p", PostInput{Body: "기록"}))
	check(t, c.Leave(a, ""))
	delete(e.alive, 100) // a: 방 없음 + 프로세스 종료 → offline
	delete(e.alive, 200) // b: 방 없음 + 종료

	check(t, c.Sweep())
	if v := ok(c.GetAgent(a)); v.Status != "offline" || v.OfflineSince == nil {
		t.Fatalf("offline_since 없음: %+v", v)
	}
	// b 가 다시 살아나면 offline_since 해제
	e.alive[200] = true
	check(t, c.Sweep())
	if ok(c.GetAgent(b)).OfflineSince != nil {
		t.Fatal("살아났는데 offline_since 남음")
	}
	// 23시간: 유지 / 25시간: 삭제 (기본 24시간)
	e.clock = e.clock.Add(23 * time.Hour)
	check(t, c.Sweep())
	ok(c.GetAgent(a))
	e.clock = e.clock.Add(2 * time.Hour)
	check(t, c.Sweep())
	_, err := c.GetAgent(a)
	nok(t, err, a)
	// 지운 카드의 이름은 남는다 (지난 대화 표시)
	if names := ok(c.Names()); names[a] != "A" {
		t.Fatalf("이름 기록 없음: %v", names)
	}
	// 재시작 후에도
	if names := ok(e.core().Names()); names[a] != "A" {
		t.Fatal("재시작 후 이름 기록 없음")
	}
	// 0 = 지우지 않음
	ok(c.SetConfig(Config{OfflineTTLHours: i(0)}))
	delete(e.alive, 200)
	check(t, c.Sweep())
	e.clock = e.clock.Add(1000 * time.Hour)
	check(t, c.Sweep())
	ok(c.GetAgent(b))
}

func TestSettingsConfig(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, _, _ := setup(t, c)
	ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "aaa", Cwd: s("D:/p")}))
	ok(c.CreateRoom(a, "r1", "", 0))
	delete(e.alive, 100)
	e.alive[150] = true
	a2 := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "new", Cwd: s("D:/p"), Pid: i(150)})).Agent.ID

	// 자동 이어받기 끄기
	f := false
	ok(c.SetConfig(Config{AutoResume: &f}))
	_, _, err := c.ResumePrevious(a2)
	nok(t, err, "꺼져 있음")
	ok(c.SetConfig(Config{}))
	if _, from, err := c.ResumePrevious(a2); err != nil || from != a {
		t.Fatalf("다시 켜면 이어받아야 함: %v", err)
	}
	// 새 방 메시지 한도 기본값, 방 한도 변경
	ok(c.SetConfig(Config{DefaultMaxMessages: i(50)}))
	b2 := ok(c.Register(RegisterInput{Kind: "codex", SessionID: "z"})).Agent.ID
	if r := ok(c.CreateRoom(b2, "r2", "", 0)); r.Policy.MaxMessages != 50 {
		t.Fatalf("기본 한도: %d", r.Policy.MaxMessages)
	}
	if r := ok(c.SetRoomLimit("r2", 20)); r.Policy.MaxMessages != 20 {
		t.Fatal("한도 변경")
	}
	_, err = c.SetRoomLimit("r2", 5)
	nok(t, err, "10")
	// 휴지통 보관 기간
	ok(c.DeleteRoom("r2"))
	if n, _ := c.TrashInfo(); n != 1 {
		t.Fatalf("휴지통: %d", n)
	}
	ok(c.SetConfig(Config{TrashRetentionDays: i(7)}))
	e.clock = e.clock.Add(8 * 24 * time.Hour)
	check(t, c.Sweep())
	if n, _ := c.TrashInfo(); n != 0 {
		t.Fatalf("7일 지났는데 휴지통에 남음: %d", n)
	}
}

func TestSayAll(t *testing.T) {
	c := newEnv(t).core()
	a, b, _ := setup(t, c)
	ok(c.CreateRoom(a, "w", "", 0))
	ok(c.Invite(a, "w", b))
	ok(c.RespondInvite(b, "w", true))
	for _, txt := range []string{"@모두 안녕", "@all 안녕", "@B @모두 안녕"} {
		if m := ok(c.Say(a, "w", txt)); len(m.To) != 0 || m.Body != "안녕" {
			t.Fatalf("%q → to=%v body=%q", txt, m.To, m.Body)
		}
	}
	if m := ok(c.Say(a, "w", "@B 너만")); len(m.To) != 1 || m.To[0] != b {
		t.Fatalf("한 명: %v", m.To)
	}
}

func TestOwnerlessRoomAndDescription(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	off := false // 사람 초대 기록(보낸 이 표시)과 수락 흐름을 본다 (같은 PC 자동 입장 끔)
	ok(c.SetConfig(Config{User: "대장", AutoAcceptInvites: &off}))

	// 사람이 방장 없이, 목적과 함께 만든다
	r := ok(c.CreateRoomWith("", "h", "사람 방", "결제 API 를 v2 로", 0))
	if r.Owner != nil || !r.Persistent || r.Description != "결제 API 를 v2 로" || len(r.Members) != 0 {
		t.Fatalf("방장 없는 방: %+v", r)
	}
	// 방장이 없어도 닫히지 않고, 사람이 초대할 수 있다
	ok(c.Invite("", "h", a))
	if inv := ok(c.Pending(a)).Invites; len(inv) != 1 || inv[0].By != "대장 (사람)" {
		t.Fatalf("사람 초대: %+v", inv)
	}
	_, err := c.Invite(b, "h", a) // 에이전트는 방장이 아니면 초대 못 함
	nok(t, err, "방장")
	// 처음 들어온 에이전트가 방장
	ok(c.RespondInvite(a, "h", true))
	if r := ok(c.GetRoom("h")); r.Owner == nil || *r.Owner != a {
		t.Fatalf("첫 입장자 방장: %+v", r.Owner)
	}
	// 참여 신청은 사람이 승인할 수 있다
	ok(c.RequestJoin(b, "h", "도울게요"))
	ok(c.RespondRequest("", "h", b, true))
	if !slices.Contains(members(t, c, "h"), b) {
		t.Fatal("사람 승인")
	}
	// 모두 나가도 닫히지 않고 방장 없이 남는다 → 다음 입장자가 방장
	check(t, c.Leave(a, ""))
	check(t, c.Leave(b, ""))
	if r := ok(c.GetRoom("h")); r.ClosedAt != nil || r.Owner != nil {
		t.Fatalf("빈 방: closed=%v owner=%v", r.ClosedAt, r.Owner)
	}
	c2 := e.core() // 다시 열어도 그대로
	if r := ok(c2.GetRoom("h")); r.ClosedAt != nil || r.Description == "" {
		t.Fatal("재시작 후 빈 방")
	}
	ok(c2.Invite("", "h", b))
	ok(c2.RespondInvite(b, "h", true))
	if r := ok(c2.GetRoom("h")); *r.Owner != b {
		t.Fatal("다음 입장자 방장")
	}
	// 설명 변경 / 지우기 / 길이 제한
	if r := ok(c2.SetRoomDescription("h", "  새 목적 ", "")); r.Description != "새 목적" {
		t.Fatal(r.Description)
	}
	if r := ok(c2.SetRoomDescription("h", "", b)); r.Description != "" {
		t.Fatal("지우기")
	}
	_, err = c2.SetRoomDescription("h", strings.Repeat("가", 1001), "")
	nok(t, err, "1000")
	// 에이전트가 만든 방은 예전처럼 마지막 멤버가 나가면 닫힌다
	ok(c2.CreateRoom(a, "ag", "", 0))
	check(t, c2.Leave(a, ""))
	if ok(c2.GetRoom("ag")).ClosedAt == nil {
		t.Fatal("에이전트 방은 닫혀야 함")
	}
}

// 새로 실행한 세션은 방에 자동으로 들어가지 않는다. 이름표(이름·설명)는 이어받고,
// 입장 명령(request)을 받으면 승인 없이 이전 세션의 자리(역할·읽은 위치)로 들어간다.
func TestNoAutoJoinRejoinOnRequest(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	_, b, _ := setup(t, c)
	old := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "o", Name: s("마스터"), Description: s("설계 담당"), Pid: i(500), Cwd: s("D:/w")})).Agent.ID
	e.alive[500] = true
	ok(c.CreateRoom(old, "gwp", "", 0))
	ok(c.Invite(old, "gwp", b))
	ok(c.RespondInvite(b, "gwp", true))
	// 세션 종료 → 새 프로세스로 다시 실행
	check(t, c.MarkAway(old, "session_end"))
	e.alive[500] = false
	r := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "n", Pid: i(600), Cwd: s("D:/w")}))
	e.alive[600] = true
	nu := r.Agent.ID
	if r.Agent.Room != nil || r.Recovery != nil {
		t.Fatalf("새 세션은 방에 자동으로 들어가면 안 됨: %+v", r.Agent.Room)
	}
	if r.Agent.Name != "마스터" || r.Agent.Description != "설계 담당" || r.InheritedFrom != old || r.PreviousRoom != "gwp" {
		t.Fatalf("이름표를 이어받아야 함: %+v", r)
	}
	if v := ok(c.GetRoom("gwp")); v.Owner == nil || *v.Owner != old {
		t.Fatalf("예전 자리는 그대로(자리 비움) 남아야 함: %+v", v.Owner)
	}
	// 입장 명령 → 승인 없이 예전 자리(방장)로
	ok(c.RequestJoin(nu, "gwp", ""))
	if v := ok(c.GetRoom("gwp")); v.Owner == nil || *v.Owner != nu {
		t.Fatalf("이전 자리(방장)를 이어받아야 함: %+v", v.Owner)
	}
	if c.RoomOf(nu) != "gwp" {
		t.Fatal("바로 입장해야 함")
	}
	// 관계 없는 세션은 여전히 신청만 된다
	x := ok(c.Register(RegisterInput{Kind: "codex", SessionID: "x", Pid: i(300), Cwd: s("D:/other")})).Agent.ID
	ok(c.RequestJoin(x, "gwp", "참여"))
	if c.RoomOf(x) != "" {
		t.Fatal("다른 세션은 승인 전에는 들어가면 안 됨")
	}
	// 자리 비움인 내 자리 → 입장 명령으로 바로 복귀
	check(t, c.MarkAway(b, "session_end"))
	ok(c.RequestJoin(b, "gwp", ""))
	if c.RoomOf(b) != "gwp" {
		t.Fatal("자리 비움이던 내 자리는 바로 복귀")
	}
}

// PC 정보: 이 PC 카드는 cwd·pid 를 보이고 PC 이름은 설정을 따른다. 다른 PC 카드는 경로·PID 없이 보인다.
func TestHostAndWorkspace(t *testing.T) {
	e := newEnv(t)
	c, err := New(Options{Root: e.root, MachineID: "abcd1234ef", IsAlive: func(p int) bool { return e.alive[p] }, Now: func() time.Time { return e.clock }})
	check(t, err)
	t.Cleanup(func() { _ = c.Close() })

	repo := t.TempDir()
	check(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	sub := filepath.Join(repo, "internal", "core")
	check(t, os.MkdirAll(sub, 0o755))

	loc := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "l", Pid: i(100), Cwd: &sub})).Agent
	if !loc.Local || loc.Host == nil || loc.Host.Name != "PC-ABCD" || loc.Cwd == nil || loc.Pid == nil {
		t.Fatalf("이 PC 카드: %+v %+v", loc.Local, loc.Host)
	}
	if loc.Workspace == nil || *loc.Workspace != filepath.Base(repo) {
		t.Fatalf("작업 이름은 git 저장소 폴더 이름: %v", loc.Workspace)
	}
	if loc.WorkspaceID == "" || loc.WorkspaceID != WorkspaceID("abcd1234ef", LocalOS(), sub) {
		t.Fatalf("작업 위치 ID: %q", loc.WorkspaceID)
	}
	ok(c.SetConfig(Config{MachineName: "집 PC"}))
	if v := ok(c.GetAgent(loc.ID)); v.Host.Name != "집 PC" {
		t.Fatalf("PC 이름은 설정을 따름: %q", v.Host.Name)
	}
	// 작업 이름 바꾸기·숨기기
	if v := ok(c.UpdateCard(loc.ID, CardPatch{Workspace: s("")})); v.Workspace == nil || *v.Workspace != "" {
		t.Fatal("작업 이름 숨김")
	}

	// 다른 PC: 경로는 받지 않고, PID 는 보이지 않는다
	remote := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "r", Pid: i(4242), Cwd: s("/home/kim/secret-proj"),
		Host: &Host{ID: "other-pc", Name: "회사 노트북", OS: "linux"}, Workspace: s("api"), WorkspaceID: "ws-remote-1"})).Agent
	if remote.Status == "offline" {
		t.Fatal("다른 PC 카드를 이 PC 의 PID 로 오프라인 판정하면 안 됨")
	}
	if remote.Local || remote.Cwd != nil || remote.Pid != nil || remote.Host.Name != "회사 노트북" || *remote.Workspace != "api" {
		t.Fatalf("다른 PC 카드: local=%v cwd=%v pid=%v host=%+v", remote.Local, remote.Cwd, remote.Pid, remote.Host)
	}
	// 다른 PC 의 같은 작업 위치에서 새 세션 → 이름표 이어받기 (경로 없이 작업 위치 ID 로)
	ok(c.UpdateCard(remote.ID, CardPatch{Name: s("리뷰어")}))
	check(t, c.MarkAway(remote.ID, "x"))
	c.mu.Lock()
	c.agents[remote.ID].OfflineSince = ptr("2026-11-17T00:00:00Z")
	c.mu.Unlock()
	r2 := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "r2", Host: &Host{ID: "other-pc", Name: "회사 노트북", OS: "linux"}, WorkspaceID: "ws-remote-1"}))
	if r2.InheritedFrom != remote.ID || r2.Agent.Name != "리뷰어" {
		t.Fatalf("다른 PC 의 같은 작업 위치: %+v", r2)
	}
	// 이 PC 의 다른 폴더는 이어받지 않는다
	other := t.TempDir()
	if r := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "l2", Pid: i(200), Cwd: &other})); r.InheritedFrom != "" {
		t.Fatalf("다른 폴더는 이어받으면 안 됨: %s", r.InheritedFrom)
	}
}

// PC 정보가 생기기 전에 등록된 카드는 데몬이 다시 시작할 때 PC·작업 이름이 채워진다 (사용자가 숨긴 작업 이름은 그대로).
func TestBackfillHosts(t *testing.T) {
	e := newEnv(t)
	open := func() *Atalk {
		c, err := New(Options{Root: e.root, MachineID: "m1", IsAlive: func(p int) bool { return e.alive[p] }, Now: func() time.Time { return e.clock }})
		check(t, err)
		return c
	}
	c := open()
	dir := filepath.Join(t.TempDir(), "proj")
	check(t, os.MkdirAll(dir, 0o755))
	ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "old", Pid: i(100), Cwd: &dir}))
	ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "hid", Pid: i(200), Cwd: &dir}))
	ok(c.UpdateCard("opencode_hid", CardPatch{Workspace: s("")}))
	check(t, c.guard(func() error { // 예전 버전처럼 PC 정보 없이 저장된 상태로 만든다
		for _, a := range c.agents {
			a.Host, a.WorkspaceID = nil, ""
			if a.ID == "claude-code_old" {
				a.Workspace = nil
			}
			c.saveCard(a)
		}
		return nil
	}))
	check(t, c.Close())
	c = open()
	t.Cleanup(func() { _ = c.Close() })
	v := ok(c.GetAgent("claude-code_old"))
	if v.Host == nil || v.Host.ID != "m1" || v.Workspace == nil || *v.Workspace != "proj" || v.WorkspaceID == "" {
		t.Fatalf("예전 카드에 PC·작업 이름이 채워져야 함: %+v %v %q", v.Host, v.Workspace, v.WorkspaceID)
	}
	if h := ok(c.GetAgent("opencode_hid")); h.Workspace == nil || *h.Workspace != "" {
		t.Fatal("숨긴 작업 이름은 그대로 두어야 함")
	}
}

// 데몬을 다시 시작해도 응답 짝이 유지되고, 예전 버전이 남긴 알림 입력에는 응답이 붙지 않는다.
func TestEchoReplyAfterRestart(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, _, _ := setup(t, c)
	ok(c.CreateRoom(a, "r", "", 0))
	req := ok(c.EchoPrompt(a, "UI 에 모드 표시해 줘"))
	check(t, c.guard(func() error { // 예전 버전이 남긴 알림 입력 기록
		m := c.memberships[a]
		c.appendMessage(m.Room, Message{From: a, Type: "chat", To: []string{a}, Body: "<task-notification>x</task-notification>", Human: "사용자", Echo: true})
		return nil
	}))
	check(t, c.Close())
	c = e.core()
	if r := ok(c.EchoReply(a, []string{"표시했습니다"}, true, "")); len(r) != 1 || *r[0].ReplyTo != req.Seq {
		t.Fatalf("재시작 뒤에도 요청에 응답이 붙어야 함: %+v", r)
	}
	if r := ok(c.EchoReply(a, []string{"또"}, true, "")); len(r) != 0 {
		t.Fatal("턴을 닫은 뒤에는 붙으면 안 됨")
	}
	// 오래된 입력에는 붙이지 않는다
	ok(c.EchoPrompt(a, "옛 입력"))
	e.clock = e.clock.Add(13 * time.Hour)
	if r := ok(c.EchoReply(a, []string{"한참 뒤 응답"}, true, "")); len(r) != 0 {
		t.Fatal("12시간 넘은 입력에는 응답을 붙이지 않음")
	}
}

// 알림으로 깨어난 턴의 응답: 그 메시지의 답으로 기록. 📨 줄과 이미 send 한 답장과 같은 문장은 빼고, CLI 입력 큐는 그대로.
func TestEchoWake(t *testing.T) {
	c := newEnv(t).core()
	ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "m"}))
	ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o"}))
	ok(c.CreateRoom("claude-code_m", "r", "", 0))
	ok(c.Invite("claude-code_m", "r", "opencode_o"))
	ok(c.RespondInvite("opencode_o", "r", true))

	q := ok(c.Post("opencode_o", "r", PostInput{Type: "result", Body: "맥 반영 완료", To: []string{"claude-code_m"}}))
	ok(c.Post("claude-code_m", "r", PostInput{Type: "result", Body: "확인했습니다.", ReplyTo: ptr(q.Seq)})) // send 로 보낸 답장
	r := ok(c.EchoWake("claude-code_m", []string{"📨 #1 맥: 맥 반영 완료\n\n중앙 상태를 보겠습니다.", "확인했습니다.", "진행할까요?"}, true, []int{q.Seq}))
	if len(r) != 2 || r[0].Body != "중앙 상태를 보겠습니다." || r[1].Body != "진행할까요?" || *r[1].ReplyTo != q.Seq || !r[1].Echo {
		t.Fatalf("알림 턴 응답 기록: %+v", r)
	}
	if r := ok(c.EchoWake("claude-code_m", []string{"진행할까요?"}, true, []int{q.Seq})); len(r) != 0 {
		t.Fatal("같은 문장 두 번")
	}
	// 내 사람이 웹에서 쓴 말(내 이름)에 대한 답도
	h := ok(c.Post("claude-code_m", "r", PostInput{Body: "반영해봐", Human: true, To: []string{"claude-code_m"}}))
	if r := ok(c.EchoWake("claude-code_m", []string{"반영했습니다."}, true, []int{h.Seq})); len(r) != 1 || *r[0].ReplyTo != h.Seq {
		t.Fatalf("사람 메시지 답: %+v", r)
	}
	// 내가 보낸 메시지 번호뿐이면 기록 안 함
	mine := ok(c.Post("claude-code_m", "r", PostInput{Body: "x"}))
	if r := ok(c.EchoWake("claude-code_m", []string{"y"}, true, []int{mine.Seq})); len(r) != 0 {
		t.Fatal("내 메시지에 붙음")
	}
}

// 결과만 기록: 중간 문장은 빼고 턴이 끝날 때 마지막 답만.
func TestEchoFinalOnly(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, _, _ := setup(t, c)
	ok(c.CreateRoom(a, "r", "", 0))
	ok(c.SetConfig(Config{EchoFinalOnly: true}))
	q := ok(c.EchoPrompt(a, "팬 모드 보이게 해 줘"))
	if r := ok(c.EchoReply(a, []string{"먼저 설정 화면을 보겠습니다."}, false, "")); len(r) != 0 {
		t.Fatal("결과만: 중간 문장은 기록하지 않음")
	}
	r := ok(c.EchoReply(a, []string{"먼저 설정 화면을 보겠습니다.", "드롭다운을 추가합니다.", "다 됐습니다. 상단에 모드가 보입니다."}, true, ""))
	if len(r) != 1 || r[0].Body != "다 됐습니다. 상단에 모드가 보입니다." || *r[0].ReplyTo != q.Seq {
		t.Fatalf("결과만: 마지막 답 하나만: %+v", r)
	}
}

// 사람이 입력한 말이 대기열에 들어간 동안 끝난 앞선 턴의 문장은 그 입력의 응답으로 붙지 않고, 기록도 닫히지 않는다.
func TestEchoReplyQueuedPrompt(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, _, _ := setup(t, c)
	ok(c.CreateRoom(a, "r", "", 0))
	q := ok(c.EchoPrompt(a, "Apache-2.0, 매뉴얼은 CC BY 4.0")) // 대기열에 들어가는 순간 훅이 기록
	// 앞선 턴(이전 입력 '라이선스 뭐로 하면 좋을까?' 의 턴)이 끝남
	if r := ok(c.EchoReply(a, []string{"어떤 목적으로 공개하느냐에 따라…"}, true, "라이선스 뭐로 하면 좋을까?")); len(r) != 0 {
		t.Fatalf("앞선 턴의 문장이 붙으면 안 됨: %+v", r)
	}
	// 실제 그 입력의 턴
	if r := ok(c.EchoReply(a, []string{"라이선스를 정리했습니다."}, true, "Apache-2.0, 매뉴얼은 CC BY 4.0")); len(r) != 1 || *r[0].ReplyTo != q.Seq {
		t.Fatalf("그 입력의 턴 문장은 기록: %+v", r)
	}
	// prompt 를 모르는 경우(opencode)는 예전처럼
	q2 := ok(c.EchoPrompt(a, "다음"))
	if r := ok(c.EchoReply(a, []string{"네"}, true, "")); len(r) != 1 || *r[0].ReplyTo != q2.Seq {
		t.Fatalf("prompt 없이: %+v", r)
	}
}

func TestIsSystemPrompt(t *testing.T) {
	for _, s := range []string{
		"<task-notification>\n<task-id>x</task-id>\n</task-notification>",
		"  <system-reminder>hi</system-reminder>",
		"[SYSTEM NOTIFICATION - NOT USER INPUT] ...",
		"<bash-input>ls</bash-input>",
		"<local-command-stdout>ok</local-command-stdout>",
		"<some-new-tag attr=\"1\">\nbody\n</some-new-tag>",
	} {
		if !IsSystemPrompt(s) {
			t.Errorf("시스템 입력으로 봐야 함: %q", s)
		}
	}
	for _, s := range []string{
		"이 폴더 정리해 줘",
		"<div> 태그 고쳐 줘",
		"<pasted_content id=\"1\">붙여 넣은 글</pasted_content>",
		"설명 <b>굵게</b>",
	} {
		if IsSystemPrompt(s) {
			t.Errorf("사람 입력인데 시스템으로 봄: %q", s)
		}
	}
}

func TestWorkspaceLabelHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip()
	}
	if got := WorkspaceLabel(home); got != "~" {
		t.Fatalf("홈 폴더는 계정명 대신 ~: %q", got)
	}
}

// CLI 입력 기록: 방에 있을 때만 남고, 자신을 포함해 누구의 unread 에도 들어가지 않는다.
func TestEchoPrompt(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	if m := ok(c.EchoPrompt(a, "방 없을 때")); m != nil {
		t.Fatal("방에 없으면 기록하지 않음")
	}
	ok(c.CreateRoom(a, "r", "", 0))
	ok(c.Invite(a, "r", b))
	ok(c.RespondInvite(b, "r", true))
	ok(c.Unread(a, true))
	ok(c.Unread(b, true))
	m := ok(c.EchoPrompt(a, "  이 폴더 정리해 줘  "))
	if m == nil || !m.Echo || m.Human == "" || m.Body != "이 폴더 정리해 줘" || len(m.To) != 1 || m.To[0] != a {
		t.Fatalf("기록 내용: %+v", m)
	}
	for _, who := range []string{a, b} {
		if u := ok(c.Unread(who, true)); len(u.Messages) != 0 {
			t.Fatalf("%s 에게 전달되면 안 됨: %+v", who, u.Messages)
		}
	}
	if all := ok(c.ReadMessages("r", 0, 100)); all[len(all)-1].Seq != m.Seq {
		t.Fatal("대화 기록에는 남아야 함")
	}
	long := ok(c.EchoPrompt(a, strings.Repeat("가", EchoMax+10)))
	if n := len([]rune(long.Body)); n > EchoMax+20 || !strings.HasSuffix(long.Body, "(길어서 생략)") {
		t.Fatalf("긴 입력은 잘라야 함: %d", n)
	}
	// 응답 기록: 기록하지 않은 CLI 입력이 있을 때만, reply_to = 그 입력. 두 번째 응답(입력 없음)은 남기지 않는다
	q := ok(c.EchoPrompt(a, "상태 알려 줘"))
	// 실시간: 도구를 쓸 때마다 지금까지의 문장 전부를 보내도 새 문장만 하나씩 남는다
	if r := ok(c.EchoReply(a, []string{"  확인해 보겠습니다  "}, false, "")); len(r) != 1 || r[0].Body != "확인해 보겠습니다" || !r[0].Echo || r[0].Human != "" || *r[0].ReplyTo != q.Seq {
		t.Fatalf("첫 문장: %+v", r)
	}
	if r := ok(c.EchoReply(a, []string{"확인해 보겠습니다", "다 괜찮습니다"}, true, "")); len(r) != 1 || r[0].Body != "다 괜찮습니다" {
		t.Fatalf("새 문장만: %+v", r)
	}
	if again := ok(c.EchoReply(a, []string{"방 메시지 처리한 응답"}, true, "")); len(again) != 0 {
		t.Fatal("턴이 닫힌 뒤(방 메시지·알림 처리 응답)는 남기지 않음")
	}
	if again := ok(c.EchoReply(b, []string{"남의 응답"}, true, "")); len(again) != 0 {
		t.Fatal("CLI 입력이 없던 에이전트의 응답은 남기지 않음")
	}
	for _, who := range []string{a, b} {
		if u := ok(c.Unread(who, true)); len(u.Messages) != 0 {
			t.Fatalf("응답 기록도 전달되면 안 됨: %+v", u.Messages)
		}
	}
	// 시스템이 넣은 입력(Monitor 알림 등)은 기록하지 않는다. 작업 도중 알림이 끼어도 사람 요청의 응답은 그 요청에 붙는다
	req := ok(c.EchoPrompt(a, "진짜 입력"))
	notice := "<task-notification>\n<task-id>b1</task-id>\n<summary>Monitor event: \"온도\"</summary>\n</task-notification>"
	if m := ok(c.EchoPrompt(a, notice)); m != nil {
		t.Fatal("Monitor 알림은 기록하면 안 됨")
	}
	if r := ok(c.EchoReply(a, []string{"요청 처리 결과"}, true, "")); len(r) != 1 || *r[0].ReplyTo != req.Seq {
		t.Fatalf("중간에 알림이 와도 요청의 응답은 기록돼야 함: %+v", r)
	}
	if m := ok(c.EchoPrompt(a, "이거 봐 줘\n\n<pasted_content id=\"1\">\n<task-notification>x</task-notification>\n</pasted_content id=\"1\">")); m == nil {
		t.Fatal("사람이 붙여 넣은 내용이 있는 입력은 기록해야 함")
	}
	f := false
	ok(c.SetConfig(Config{EchoPrompts: &f}))
	if m := ok(c.EchoPrompt(a, "꺼짐")); m != nil {
		t.Fatal("설정이 꺼져 있으면 기록하지 않음")
	}
}

// 이름표를 이어받은 새 세션이 초대로 들어와도 이전 자리를 이어받는다 (같은 에이전트가 두 번 보이지 않게).
func TestInviteTakesOverPreviousSeat(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	owner := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "m", Pid: i(100), Cwd: s("D:/m")})).Agent.ID
	ok(c.CreateRoom(owner, "r", "", 0))
	old := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o1", Name: s("오1"), Pid: i(700), Cwd: s("D:/op1")})).Agent.ID
	e.alive[700] = true
	ok(c.Invite(owner, "r", old))
	ok(c.RespondInvite(old, "r", true))
	check(t, c.MarkAway(old, "opencode_exit"))
	e.alive[700] = false
	nu := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o2", Pid: i(800), Cwd: s("D:/op1")}))
	e.alive[800] = true
	if nu.InheritedFrom != old || nu.Agent.Name != "오1" {
		t.Fatalf("이름표 이어받기: %+v", nu)
	}
	ok(c.Invite(owner, "r", nu.Agent.ID))
	ok(c.RespondInvite(nu.Agent.ID, "r", true))
	if got := members(t, c, "r"); len(got) != 2 || !slices.Contains(got, nu.Agent.ID) || slices.Contains(got, old) {
		t.Fatalf("이전 자리를 이어받아 멤버는 방장+새 오1 둘이어야 함: %v", got)
	}
}

// 원칙: 이전 세션으로 복귀하면 그때 있던 방으로 들어간다. 새로 켠 세션(임시)은 쓰기 전까지 이전 세션을 밀어내지 않는다.
func TestResumeReturnsToRoom(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	owner := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "m", Pid: i(100), Cwd: s("D:/m")})).Agent.ID
	ok(c.CreateRoom(owner, "r", "", 0))
	old := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o1", Name: s("오1"), Pid: i(700), Cwd: s("D:/op1"), Provisional: true})).Agent.ID
	e.alive[700] = true
	ok(c.Invite(owner, "r", old))
	ok(c.RespondInvite(old, "r", true)) // 방에 들어가면 확정
	if ok(c.GetAgent(old)).Provisional {
		t.Fatal("방에 들어가면 확정돼야 함")
	}
	check(t, c.MarkAway(old, "opencode_exit"))
	e.alive[700] = false

	// opencode 를 다시 켬 → 빈 새 세션이 임시로 등록 (이름표는 미리 보이지만 이전 세션을 밀어내지 않음)
	e.alive[800] = true
	eager := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o2", Pid: i(800), Cwd: s("D:/op1"), Provisional: true}))
	if !eager.Agent.Provisional || eager.Agent.Name != "오1" || eager.PreviousRoom != "r" || eager.Agent.Room != nil {
		t.Fatalf("임시 새 세션: %+v", eager)
	}
	if v := ok(c.GetAgent(old)); v.RetiredBy != nil || v.Status != StateAway {
		t.Fatalf("임시 세션은 이전 세션을 넘겨받지 않음: %+v", v)
	}
	// 사용자가 이전 세션을 불러와 씀 → 이전 세션이 그 방 자리로 복귀, 쓰지 않은 빈 세션 카드는 정리
	back := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o1", Pid: i(800), Cwd: s("D:/op1"), Provisional: true}))
	if back.Agent.Room == nil || *back.Agent.Room != "r" || back.Agent.Status != StateInRoom {
		t.Fatalf("이전 세션은 그때 있던 방으로: %+v", back.Agent)
	}
	if _, err := c.GetAgent(eager.Agent.ID); err == nil {
		t.Fatal("쓰지 않은 빈 새 세션 카드는 지워져야 함")
	}
	if got := members(t, c, "r"); len(got) != 2 {
		t.Fatalf("오1 은 하나: %v", got)
	}

	// 자리 비움 유예가 지나 자동 퇴장된 뒤에 복귀해도 그 방으로
	check(t, c.MarkAway(old, "opencode_exit"))
	e.clock = e.clock.Add(2 * time.Minute) // AwayGrace 1분
	c.Sweep()
	if v := ok(c.GetAgent(old)); v.Room != nil {
		t.Fatal("유예가 지나면 퇴장")
	}
	if r := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o1", Pid: i(800), Cwd: s("D:/op1")})); r.Agent.Room == nil || *r.Agent.Room != "r" {
		t.Fatalf("자동 퇴장 뒤 복귀해도 그 방으로: %+v", r.Agent.Room)
	}
	// 직접 나간 방으로는 돌아가지 않는다
	check(t, c.Leave(old, ""))
	if r := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o1", Pid: i(800), Cwd: s("D:/op1")})); r.Agent.Room != nil {
		t.Fatal("직접 나간 방으로는 돌아가지 않음")
	}
}

// 새 세션을 실제로 쓰면(사람 입력) 그때 이전 세션을 넘겨받고, 입장 명령으로 이전 자리에 들어간다.
func TestProvisionalActivate(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	owner := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "m", Pid: i(100), Cwd: s("D:/m")})).Agent.ID
	ok(c.CreateRoom(owner, "r", "", 0))
	old := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o1", Name: s("오1"), Pid: i(700), Cwd: s("D:/op1")})).Agent.ID
	ok(c.Invite(owner, "r", old))
	ok(c.RespondInvite(old, "r", true))
	check(t, c.MarkAway(old, "exit"))
	nu := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o2", Pid: i(800), Cwd: s("D:/op1"), Provisional: true})).Agent.ID
	e.alive[800] = true
	check(t, c.Activate(nu))
	if v := ok(c.GetAgent(old)); v.RetiredBy == nil || *v.RetiredBy != nu {
		t.Fatalf("확정되면 이전 세션을 넘겨받음: %+v", v.RetiredBy)
	}
	if ok(c.GetAgent(nu)).Room != nil {
		t.Fatal("확정돼도 방에 자동으로 들어가지 않음")
	}
	ok(c.RequestJoin(nu, "r", ""))
	if got := members(t, c, "r"); len(got) != 2 || !slices.Contains(got, nu) {
		t.Fatalf("입장 명령으로 이전 자리에: %v", got)
	}
}

// Claude 를 켜자마자 /resume 으로 이전 세션을 열면 쓰지 않은 새 세션 카드는 남지 않는다.
func TestClaudeStartupThenResume(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	prev := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "prev", Name: s("마스터"), Pid: i(100), Cwd: s("D:/a"), Provisional: true})).Agent.ID
	ok(c.CreateRoom(prev, "t", "", 0))
	check(t, c.MarkAway(prev, "session_end"))
	e.alive[100] = false
	e.alive[600] = true
	fresh := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "fresh", Pid: i(600), Cwd: s("D:/a"), Provisional: true})).Agent.ID
	back := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "prev", Pid: i(600), Cwd: s("D:/a"), Provisional: true}))
	if back.Agent.Room == nil || *back.Agent.Room != "t" || back.Recovery == nil {
		t.Fatalf("/resume 한 이전 세션은 그 방으로: %+v", back.Agent.Room)
	}
	if _, err := c.GetAgent(fresh); err == nil {
		t.Fatal("켜자마자 생긴 새 세션 카드는 지워져야 함")
	}
}

// 이전 세션으로 복귀하면, 그 사이 이름표를 넘겨받아 방에 들어갔던 세션(지금 안 쓰임)의 자리를 되찾는다.
func TestResumeReclaimsSeatFromSuccessor(t *testing.T) {
	for _, viaRequest := range []bool{true, false} {
		e := newEnv(t)
		c := e.core()
		owner := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "m", Pid: i(100), Cwd: s("D:/m")})).Agent.ID
		ok(c.CreateRoom(owner, "r", "", 0))
		a := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "a", Name: s("오1"), Pid: i(700), Cwd: s("D:/op1")})).Agent.ID
		e.alive[700] = true
		ok(c.Invite(owner, "r", a))
		ok(c.RespondInvite(a, "r", true))
		check(t, c.MarkAway(a, "exit"))
		e.alive[700] = false
		if !viaRequest { // a 의 자리가 유예로 정리된 뒤 b 가 초대로 새 자리에 들어간 경우
			e.clock = e.clock.Add(2 * time.Minute)
			c.Sweep()
		}
		b := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "b", Pid: i(800), Cwd: s("D:/op1"), Provisional: true})).Agent.ID
		e.alive[800] = true
		if viaRequest {
			check(t, c.Activate(b))
			ok(c.RequestJoin(b, "r", "")) // a 의 자리를 넘겨받음
		} else {
			ok(c.Invite(owner, "r", b))
			ok(c.RespondInvite(b, "r", true))
		}
		check(t, c.MarkAway(b, "opencode_exit"))
		e.alive[800] = false
		// 사용자가 a 세션으로 복귀
		e.alive[900] = true
		back := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "a", Pid: i(900), Cwd: s("D:/op1"), Provisional: true}))
		if back.Agent.Room == nil || *back.Agent.Room != "r" || back.Agent.Status != StateInRoom {
			t.Fatalf("viaRequest=%v: 복귀한 a 가 그 방에: %+v", viaRequest, back.Agent)
		}
		got := members(t, c, "r")
		if len(got) != 2 || !slices.Contains(got, a) || slices.Contains(got, b) {
			t.Fatalf("viaRequest=%v: 오1 은 하나(a): %v", viaRequest, got)
		}
		if v := ok(c.GetAgent(b)); v.RetiredBy == nil || *v.RetiredBy != a {
			t.Fatalf("viaRequest=%v: b 는 a 에게 넘어감: %+v", viaRequest, v.RetiredBy)
		}
	}
}

// 쓰지 않은 임시 카드: 끝나면 지우고, 서로 이어받지 않고, 같은 프로세스에서 이전 세션으로 복귀하면 지운다.
func TestProvisionalCleanup(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	owner := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "m", Pid: i(100), Cwd: s("D:/m")})).Agent.ID
	ok(c.CreateRoom(owner, "r", "", 0))
	o1 := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o1", Name: s("오1"), Pid: i(700), Cwd: s("D:/op1")})).Agent.ID
	e.alive[700] = true
	ok(c.Invite(owner, "r", o1))
	ok(c.RespondInvite(o1, "r", true))
	check(t, c.MarkAway(o1, "opencode_exit"))
	e.alive[700] = false
	// opencode 를 켰다 끔 (빈 세션만 생김) → 끝나면 지워짐
	e.alive[801] = true
	p1 := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "p1", Pid: i(801), Cwd: s("D:/op1"), Provisional: true})).Agent.ID
	e.alive[801] = false
	c.Sweep()
	if _, err := c.GetAgent(p1); err == nil {
		t.Fatal("쓰지 않고 끝난 임시 카드는 지워져야 함")
	}
	// 다시 켬 → 빈 세션은 이전 임시 카드가 아니라 진짜 이전 세션(o1)을 가리킴
	e.alive[802] = true
	p2 := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "p2", Pid: i(802), Cwd: s("D:/op1"), Provisional: true}))
	if p2.InheritedFrom != o1 {
		t.Fatalf("임시 카드끼리 이어받으면 안 됨: %s", p2.InheritedFrom)
	}
	// 같은 opencode 에서 o1 세션으로 복귀 → o1 은 방으로, 같은 프로세스의 빈 세션 카드는 정리
	back := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "o1", Pid: i(802), Cwd: s("D:/op1"), Provisional: true}))
	if back.Agent.Room == nil || *back.Agent.Room != "r" {
		t.Fatalf("복귀: %+v", back.Agent.Room)
	}
	if _, err := c.GetAgent(p2.Agent.ID); err == nil {
		t.Fatal("같은 프로세스의 쓰지 않은 빈 세션은 지워져야 함")
	}
	// 이미 복귀한 뒤에 남아 있던 빈 세션(같은 프로세스)도 점검 때 지운다
	p3 := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "p3", Pid: i(802), Cwd: s("D:/op1"), Provisional: true})).Agent.ID
	c.Sweep()
	if _, err := c.GetAgent(p3); err == nil {
		t.Fatal("같은 프로세스가 방에 있으면 빈 세션은 점검 때 지워져야 함")
	}
}

// 같은 PC 에서 대기 중인 에이전트는 초대하면 바로 입장. 다른 방에 있거나 다른 PC 거나 설정을 끄면 초대만 남는다.
func TestLocalAutoAcceptInvite(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, d := setup(t, c)
	ok(c.CreateRoom(a, "r", "", 0))
	ok(c.Invite(a, "r", b))
	if ok(c.GetAgent(b)).Room == nil {
		t.Fatal("같은 PC·대기 중이면 바로 입장해야 함")
	}
	if p := ok(c.Pending(b)); len(p.Invites) != 0 {
		t.Fatalf("수락된 초대는 대기 목록에 없어야 함: %+v", p.Invites)
	}
	// 다른 방에 있는 에이전트 → 초대만
	ok(c.CreateRoom(d, "other", "", 0))
	ok(c.Invite(a, "r", d))
	if v := ok(c.GetAgent(d)); v.Room == nil || *v.Room != "other" {
		t.Fatal("다른 방에 있으면 자동으로 옮기지 않음")
	}
	// 사람이 화면에서 초대해도 같은 규칙
	x := ok(c.Register(RegisterInput{Kind: "codex", SessionID: "x", Pid: i(100)})).Agent.ID
	ok(c.Invite("", "r", x))
	if ok(c.GetAgent(x)).Room == nil {
		t.Fatal("사람 초대도 대기 중이면 바로 입장")
	}
	// 다른 PC 에이전트 → 초대만
	rmt := ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "rmt", Host: &Host{ID: "other-pc", Name: "노트북"}})).Agent.ID
	ok(c.Invite(a, "r", rmt))
	if ok(c.GetAgent(rmt)).Room != nil {
		t.Fatal("다른 PC 는 자동 입장하지 않음")
	}
	// 설정으로 끄면 초대만
	f := false
	ok(c.SetConfig(Config{AutoAcceptInvites: &f}))
	y := ok(c.Register(RegisterInput{Kind: "codex", SessionID: "y", Pid: i(200)})).Agent.ID
	ok(c.Invite(a, "r", y))
	if ok(c.GetAgent(y)).Room != nil {
		t.Fatal("auto_accept_invites=false 면 초대만")
	}
}

// 작업 중인 에이전트에게 지정해 보낸 chat·task 에는 데몬이 바로 "받았습니다" 상태 답장. 쉬고 있거나 방 전체 에이전트 메시지면 없음.
func TestBusyAck(t *testing.T) {
	c := newEnv(t).core()
	a, b, _ := setup(t, c)
	ok(c.CreateRoom(a, "r", "", 0))
	ok(c.Invite(a, "r", b)) // 같은 PC 대기 중 → 바로 입장
	acks := func(seq int) (n int) {
		for _, m := range ok(c.ReadMessages("r", 0, 1000)) {
			if m.Type == "status" && m.ReplyTo != nil && *m.ReplyTo == seq && m.Body == BusyAck {
				n++
			}
		}
		return
	}
	// 쉬고 있으면 없음
	m := ok(c.Post(a, "r", PostInput{Body: "해 줘", To: []string{b}}))
	if acks(m.Seq) != 0 {
		t.Fatal("쉬고 있으면 자동 답장 없음")
	}
	check(t, c.SetActivity(b, true))
	m = ok(c.Post(a, "r", PostInput{Type: "task", Body: "이것도", To: []string{b}}))
	if acks(m.Seq) != 1 {
		t.Fatal("작업 중이면 자동 답장")
	}
	// 방 전체에 보낸 에이전트 메시지, result 에는 없음
	if m = ok(c.Post(a, "r", PostInput{Body: "모두에게"})); acks(m.Seq) != 0 {
		t.Fatal("방 전체 에이전트 메시지에는 없음")
	}
	if m = ok(c.Post(a, "r", PostInput{Type: "result", Body: "결과", To: []string{b}})); acks(m.Seq) != 0 {
		t.Fatal("result 에는 없음")
	}
	// 내 사람이 웹에서 나에게(또는 방 전체에) 쓴 말
	if m = ok(c.Say(b, "r", "지금 뭐 해?")); acks(m.Seq) != 1 {
		t.Fatal("내 사람의 말에는 자동 답장")
	}
}

// 자동 수신 확인은 보낸 쪽에게만 가고(방 전체로 가서 다른 에이전트를 깨우지 않음), 같은 보낸 이에게는 작업 중 구간마다 한 번만.
func TestBusyAckTargetAndOnce(t *testing.T) {
	c := newEnv(t).core()
	noAutoAccept(c)
	a, b, d := setup(t, c)
	ok(c.CreateRoom(a, "r", "", 0))
	for _, x := range []string{b, d} {
		ok(c.RequestJoin(x, "r", ""))
		ok(c.RespondRequest(a, "r", x, true))
	}
	ok(c.Unread(d, true))
	check(t, c.SetActivity(b, true))
	m1 := ok(c.Post(a, "r", PostInput{Body: "하나", To: []string{b}}))
	ok(c.Post(a, "r", PostInput{Body: "둘", To: []string{b}}))
	var acks []Message
	for _, m := range ok(c.ReadMessages("r", m1.Seq-1, 1000)) {
		if m.Type == "status" && m.Body == BusyAck {
			acks = append(acks, m)
		}
	}
	if len(acks) != 1 || len(acks[0].To) != 1 || acks[0].To[0] != a || acks[0].From != b {
		t.Fatalf("보낸 쪽에게 한 번만: %+v", acks)
	}
	if u := ok(c.Unread(d, false)); slices.ContainsFunc(u.Messages, func(m InboxMessage) bool { return m.ForMe && m.Type == "status" }) {
		t.Fatal("다른 멤버(d)에게는 자동 수신 확인이 가지 않아야 함")
	}
	// 작업이 끝나고 다시 시작하면 또 보낼 수 있음
	check(t, c.SetActivity(b, false))
	check(t, c.SetActivity(b, true))
	m3 := ok(c.Post(a, "r", PostInput{Body: "셋", To: []string{b}}))
	n := 0
	for _, m := range ok(c.ReadMessages("r", m3.Seq, 1000)) {
		if m.Type == "status" && m.Body == BusyAck {
			n++
		}
	}
	if n != 1 {
		t.Fatal("새 작업 중 구간에는 다시 한 번")
	}
	// 내 사람이 웹에서 쓴 말에 대한 자동 수신 확인(From=To=나, 사람 표시 없음)은 나도 깨우지 않는다
	ok(c.Unread(b, true))
	h := ok(c.Say(b, "r", "지금 뭐 해?"))
	u := ok(c.Unread(b, false))
	for _, m := range u.Messages {
		if m.Type == "status" && m.ReplyTo != nil && *m.ReplyTo == h.Seq {
			t.Fatal("내 사람 말에 대한 자동 수신 확인이 나를 깨우면 안 됨")
		}
	}
	if !slices.ContainsFunc(u.Messages, func(m InboxMessage) bool { return m.Seq == h.Seq && m.ForMe }) {
		t.Fatal("내 사람의 말 자체는 나에게 와야 함")
	}
}

// 작업 중 표시: 훅·플러그인이 알린 대로, 방에 없어도(CLI 에서 일함) 보이고, 끝나거나 15분 신호가 없거나 자리 비움이면 꺼진다.
func TestActivityBusy(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	a, b, _ := setup(t, c)
	check(t, c.SetActivity(a, true))
	if !ok(c.GetAgent(a)).Busy {
		t.Fatal("방에 없어도 CLI 에서 일하면 작업 중")
	}
	ok(c.CreateRoom(a, "r", "", 0))
	ok(c.Invite(a, "r", b))
	if r := ok(c.GetRoom("r")); !r.Members[0].Busy && !r.Members[1].Busy {
		t.Fatalf("방 멤버에도 작업 중 표시: %+v", r.Members)
	}
	check(t, c.SetActivity(a, false))
	if ok(c.GetAgent(a)).Busy {
		t.Fatal("끝나면 대기")
	}
	check(t, c.SetActivity(b, true))
	e.clock = e.clock.Add(16 * time.Minute)
	if ok(c.GetAgent(b)).Busy {
		t.Fatal("15분 동안 신호가 없으면 대기로")
	}
	check(t, c.SetActivity(b, true))
	check(t, c.MarkAway(b, "exit"))
	if ok(c.GetAgent(b)).Busy {
		t.Fatal("자리 비움이면 작업 중 아님")
	}
}

func TestResumeSameProcessIdentity(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	// Claude 세션을 다시 열면 같은 프로세스(pid 100)가 새 세션 ID 로 등록된다
	old := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "old", Pid: i(100), Cwd: s("D:/p")})).Agent.ID
	nu := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "new", Pid: i(100), Cwd: s("D:/p")})).Agent.ID
	if v := ok(c.GetAgent(old)); v.RetiredBy == nil || *v.RetiredBy != nu || v.Status != "offline" {
		t.Fatalf("방 없는 예전 카드는 새 카드가 이어받아야 함: %+v", v)
	}
	// 방에 있는 예전 카드: 이름표는 이어받지만 방에는 들어가지 않는다 (예전 자리는 자리 비움)
	o2 := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "o2", Pid: i(200), Cwd: s("D:/q")})).Agent.ID
	ok(c.CreateRoom(o2, "r", "", 0))
	r2 := ok(c.Register(RegisterInput{Kind: "claude-code", SessionID: "n2", Pid: i(200), Cwd: s("D:/q")}))
	if r2.InheritedFrom != o2 || r2.PreviousRoom != "r" || r2.Agent.Room != nil {
		t.Fatalf("이름표만 이어받고 방에는 없어야 함: %+v", r2)
	}
	if ok(c.GetAgent(o2)).RetiredBy == nil {
		t.Fatal("예전 카드는 retired_by 로 표시돼야 함")
	}
	// opencode 는 한 프로세스에 세션이 여럿 → retire 하지 않음
	ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "a", Pid: i(300)}))
	ok(c.Register(RegisterInput{Kind: "opencode", SessionID: "b", Pid: i(300)}))
	if ok(c.GetAgent("opencode_a")).RetiredBy != nil {
		t.Fatal("opencode 는 retire 하면 안 됨")
	}
}

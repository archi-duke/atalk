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
	// 에이전트가 자기에게 보낸 일반 메시지는 여전히 받지 않는다
	ok(c.Ack(a, m.Seq))
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
	ok(c.SetConfig(Config{User: "대장"}))

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
	f := false
	ok(c.SetConfig(Config{EchoPrompts: &f}))
	if m := ok(c.EchoPrompt(a, "꺼짐")); m != nil {
		t.Fatal("설정이 꺼져 있으면 기록하지 않음")
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

package core

import (
	"testing"
	"time"
)

func remoteCard(sid string) RegisterInput {
	return RegisterInput{Kind: "claude-code", SessionID: sid, Name: s("R"), Host: &Host{ID: "other-pc", Name: "PC-B", OS: "mac"}, Cwd: s("/secret/path"), Pid: i(100), WorkspaceID: "ws1"}
}

// 원격 등록: 다른 PC 의 host 가 있어야 하고, 경로·PID 는 버리고 토큰을 발급한다.
func TestPairRemote(t *testing.T) {
	c := newEnv(t).core()
	_, _, err := c.PairRemote(RegisterInput{Kind: "claude-code", SessionID: "x"})
	nok(t, err, "host")
	local := c.Machine()
	_, _, err = c.PairRemote(RegisterInput{Kind: "claude-code", SessionID: "x", Host: &local})
	nok(t, err, "host")

	res, tok, err := c.PairRemote(remoteCard("r1"))
	check(t, err)
	if res.Agent.Local || res.Agent.Cwd != nil || res.Agent.Pid != nil || res.Agent.Status != "waiting" {
		t.Fatalf("원격 카드가 잘못됨: %+v", res.Agent)
	}
	if c.RemoteAgent(tok) != res.Agent.ID || c.RemoteAgent("wrong") != "" || c.RemoteAgent("") != "" {
		t.Fatal("토큰 확인 실패")
	}
	// 다시 연결하면 예전 토큰은 무효
	_, tok2, err := c.PairRemote(remoteCard("r1"))
	check(t, err)
	if c.RemoteAgent(tok) != "" || c.RemoteAgent(tok2) != res.Agent.ID {
		t.Fatal("다시 연결하면 예전 토큰이 무효여야 함")
	}
	// 카드를 지우면 토큰도 무효
	check(t, c.Unregister(res.Agent.ID))
	if c.RemoteAgent(tok2) != "" {
		t.Fatal("카드를 지우면 토큰도 무효여야 함")
	}
}

// 하트비트: 요청이 끊기면 offline / 방에 있으면 자리 비움, 다시 요청하면 복귀. 열린 요청(wait)이 있으면 살아 있다.
func TestRemoteHeartbeat(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	ok(c.SetConfig(Config{AwayGraceMin: i(0)})) // 자동 퇴장은 따로 시험함
	a, _, _ := setup(t, c)
	res, tok, err := c.PairRemote(remoteCard("r1"))
	check(t, err)
	r := res.Agent.ID

	e.clock = e.clock.Add(RemoteTTL + time.Second)
	if ok(c.GetAgent(r)).Status != "offline" {
		t.Fatal("하트비트가 끊기면 offline")
	}
	if ok(c.GetAgent(a)).Status != "waiting" {
		t.Fatal("로컬 카드는 PID 로 판단")
	}
	c.TouchRemote(r, 0)
	if ok(c.GetAgent(r)).Status != "waiting" {
		t.Fatal("요청이 오면 다시 살아 있음")
	}

	ok(c.CreateRoom(a, "g", "", 0))
	ok(c.RequestJoin(r, "g", ""))
	ok(c.RespondRequest(a, "g", r, true))
	e.clock = e.clock.Add(RemoteTTL + time.Second)
	check(t, c.Sweep())
	if v := ok(c.GetAgent(r)); v.Status != StateAway {
		t.Fatalf("방에 있는데 끊기면 자리 비움: %s", v.Status)
	}
	c.TouchRemote(r, 0)
	if ok(c.GetAgent(r)).Status != StateInRoom {
		t.Fatal("다시 요청하면 복귀")
	}

	// wait 롱폴링이 열려 있는 동안은 TTL 이 지나도 살아 있음
	c.TouchRemote(r, +1)
	e.clock = e.clock.Add(2 * RemoteTTL)
	check(t, c.Sweep())
	if ok(c.GetAgent(r)).Status != StateInRoom {
		t.Fatal("열린 요청이 있으면 살아 있음")
	}
	c.TouchRemote(r, -1)
	e.clock = e.clock.Add(RemoteTTL + time.Second)
	check(t, c.Sweep())
	if ok(c.GetAgent(r)).Status != StateAway {
		t.Fatal("열린 요청이 끝나고 TTL 이 지나면 자리 비움")
	}

	// 연결 끊기(철회): 토큰 무효, 자리 비움, 하트비트로 복귀하지 않음
	c.TouchRemote(r, 0)
	check(t, c.RevokeRemote(r))
	if c.RemoteAgent(tok) != "" {
		t.Fatal("철회한 토큰")
	}
	v := ok(c.GetAgent(r))
	if v.Status != StateAway {
		t.Fatalf("철회하면 자리 비움: %s", v.Status)
	}
	conns := ok(c.RemoteConns())
	if len(conns) != 0 {
		t.Fatal("철회하면 연결 목록에서 빠짐")
	}
	nok(t, c.RevokeRemote(r), "원격 연결이 없음")
}

// 토큰은 데몬을 다시 시작해도 유지되고, 재시작 직후에는 원격 에이전트를 바로 끊긴 것으로 보지 않는다.
func TestRemoteTokenPersists(t *testing.T) {
	e := newEnv(t)
	c := e.core()
	res, tok, err := c.PairRemote(remoteCard("r1"))
	check(t, err)
	check(t, c.Close())
	e.clock = e.clock.Add(time.Hour)
	c2 := e.core()
	if c2.RemoteAgent(tok) != res.Agent.ID {
		t.Fatal("재시작 후 토큰 유지")
	}
	if ok(c2.GetAgent(res.Agent.ID)).Status != "waiting" {
		t.Fatal("재시작 직후에는 시작 시각부터 센다")
	}
}

// 원격 에이전트가 자기 PC 에서 띄운 세션: 같은 PC 로, 띄운 에이전트로 표시되고, 그 방에 바로 들어간다.
func TestSpawnRemote(t *testing.T) {
	c := newEnv(t).core()
	res, _, err := c.PairRemote(remoteCard("r1"))
	check(t, err)
	by := res.Agent.ID
	if _, err := c.CreateRoom(by, "g", "", 0); err != nil {
		t.Fatal(err)
	}
	// 이 PC 의 에이전트는 쓸 수 없음
	loc, err := c.Register(RegisterInput{Kind: "claude-code", SessionID: "loc", Cwd: s("/w")})
	check(t, err)
	_, _, err = c.SpawnRemote(loc.Agent.ID, RegisterInput{Kind: "claude-code", SessionID: "n0"})
	nok(t, err, "원격으로 연결된")

	sp, tok, err := c.SpawnRemote(by, RegisterInput{Kind: "codex", SessionID: "n1", Name: s("보조"), Host: &Host{ID: "spoof", Name: "X", OS: "linux"}})
	check(t, err)
	a := sp.Agent
	if tok == "" || c.RemoteAgent(tok) != a.ID || a.SpawnedBy != by || a.Host == nil || a.Host.ID != "other-pc" || a.Local {
		t.Fatalf("띄운 원격 세션: %+v", a)
	}
	if a.Room == nil || *a.Room != "g" || a.Status != "in_room" {
		t.Fatalf("띄운 에이전트의 방에 들어가야 함: %+v", a)
	}
	_, _, err = c.SpawnRemote(by, RegisterInput{Kind: "claude-code", SessionID: "r1"})
	nok(t, err, "새 세션")
}

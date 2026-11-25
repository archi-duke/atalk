package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"atalk/internal/client"
	"atalk/internal/core"
)

// 원격 포트 전체 흐름: 참여 코드 → HTTPS 페어링(지문 확인) → 토큰으로 방 참여·메시지. 행위자 고정, 막힌 경로, 연결 끊기.
func TestRemoteFlow(t *testing.T) {
	t.Setenv("ATALK_CONFIG_DIR", t.TempDir()) // 원격 연결 파일 위치
	srv, err := New(Options{Root: t.TempDir(), Addr: "127.0.0.1:0", MachineID: "host-pc", RemoteDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	port, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	lc := client.New(fmt.Sprintf("http://127.0.0.1:%d", port))

	// 원격 포트가 꺼져 있으면 참여 코드를 줄 수 없음
	if _, err := lc.NewPairCode(); err == nil {
		t.Fatal("원격 포트가 꺼져 있으면 코드 발급 불가")
	}
	rp, err := srv.StartRemote(0)
	must(err)
	url := fmt.Sprintf("https://127.0.0.1:%d", rp)

	secret := "/Users/host/secret-proj"
	lead, err := lc.Register(core.RegisterInput{Kind: "claude-code", SessionID: "lead", Cwd: &secret})
	must(err)
	L := lead.Agent.ID
	_, err = lc.CreateRoom(L, "r", "원격 시험")
	must(err)

	pc, err := lc.NewPairCode()
	must(err)
	me := core.Host{ID: "guest-pc", Name: "PC-B", OS: "linux"}
	card := core.RegisterInput{Kind: "claude-code", SessionID: "guest"}

	// 지문이 다르면 연결하지 않는다 (다른 서버)
	sec, _, _ := strings.Cut(pc.Code, ".")
	if _, _, err := client.Pair(client.PairInput{URL: url, Code: sec + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Card: card, Host: me, Cwd: "/home/guest/proj"}); err == nil || !strings.Contains(err.Error(), "지문") {
		t.Fatalf("지문이 다르면 실패해야 함: %v", err)
	}
	rc, v, err := client.Pair(client.PairInput{URL: url, Code: pc.Code, Card: card, Host: me, Cwd: "/home/guest/proj"})
	must(err)
	G := v.ID
	if !v.Local || v.Cwd != nil || v.Host == nil || v.Host.ID != "guest-pc" || v.WorkspaceID == "" || v.Workspace == nil || *v.Workspace != "proj" {
		t.Fatalf("원격 카드: %+v", v)
	}
	if _, _, err := client.Pair(client.PairInput{URL: url, Code: pc.Code, Card: card, Host: me}); err == nil {
		t.Fatal("참여 코드는 한 번만")
	}
	if client.LoadConn(G) == nil {
		t.Fatal("연결 파일이 저장돼야 함")
	}
	rcl := client.ForAgent(G)
	if !rcl.Remote() {
		t.Fatal("ForAgent 는 원격 클라이언트")
	}

	// 디스커버리: 호스트 PC 에이전트의 경로는 보이지 않음
	agents, err := rcl.Agents("", "")
	must(err)
	for _, a := range agents {
		if a.Cwd != nil || a.Pid != nil {
			t.Fatalf("원격에 경로·PID 노출: %+v", a)
		}
		// "이 PC" 는 요청한 에이전트의 PC 기준: 호스트 PC 의 에이전트는 아니고 내 카드만
		if a.Local != (a.ID == G) {
			t.Fatalf("원격에서 본 local 이 잘못됨: %s local=%v", a.ID, a.Local)
		}
	}
	if me, err := rcl.Agent(L); err != nil || me.Local {
		t.Fatalf("호스트 에이전트를 원격에서 보면 이 PC 가 아님: %+v %v", me, err)
	}
	// 다시 등록(세션 시작 훅): 경로 대신 작업 위치 ID
	cwd := "/home/guest/proj"
	rr, err := rcl.Register(core.RegisterInput{Kind: "claude-code", SessionID: "guest", Cwd: &cwd})
	must(err)
	if rr.Agent.Cwd != nil || !rr.Agent.Local || rr.Agent.Host.ID != "guest-pc" || rr.Agent.WorkspaceID != v.WorkspaceID {
		t.Fatalf("다시 등록: %+v", rr.Agent)
	}
	if _, err := rcl.Register(core.RegisterInput{Kind: "claude-code", SessionID: "someone-else"}); err == nil {
		t.Fatal("다른 에이전트로 등록할 수 없음")
	}

	// 참여 신청 → 방장 승인 → 메시지
	if _, err := rcl.Messages("r", 0, 10); err == nil {
		t.Fatal("들어가지 않은 방의 대화는 볼 수 없음")
	}
	joined, err := rcl.RequestJoin(G, "r", "원격에서 왔어요")
	must(err)
	if joined {
		t.Fatal("원격은 승인이 필요함")
	}
	must(lc.RespondRequest(L, "r", G, true))
	m, err := rcl.Post(G, "r", core.PostInput{Body: "안녕하세요 (원격)"})
	must(err)
	if m.From != G {
		t.Fatal("보낸 이")
	}
	u, err := lc.Unread(L, true)
	must(err)
	if len(u.Messages) == 0 || u.Messages[len(u.Messages)-1].Body != "안녕하세요 (원격)" {
		t.Fatalf("방장이 원격 메시지를 받아야 함: %+v", u.Messages)
	}

	// 훅이 쓰는 자기 경로: 확인 필요(need-input), 자동 수신 기록(delivered) — 원격에서도 막히지 않아야 함
	must(rcl.NeedInput(G, "Bash 실행 허가 필요"))
	must(rcl.Delivered(G, "r", []int{m.Seq}))
	if err := rcl.NeedInput(L, "사칭"); err == nil {
		t.Fatal("다른 에이전트의 need-input 은 막아야 함")
	}

	raw := func(method, path, body, token string) int {
		req, _ := http.NewRequest(method, url+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := rcl.HTTP.Do(req)
		must(err)
		res.Body.Close()
		return res.StatusCode
	}
	// 행위자는 토큰의 에이전트로 고정
	if st := raw("POST", "/rooms/r/messages", `{"agent":"`+L+`","body":"사칭"}`, rc.Token); st != 403 {
		t.Fatalf("다른 에이전트로 보내기: %d", st)
	}
	if st := raw("GET", "/agents/"+L+"/unread", "", rc.Token); st != 403 {
		t.Fatalf("다른 에이전트의 unread: %d", st)
	}
	// 본문에 agent 가 없어도 사람(관리자)이 아니라 내 에이전트로 처리 → 방장이 아니므로 초대 불가
	if st := raw("POST", "/rooms/r/invites", `{"target":"`+L+`"}`, rc.Token); st < 400 {
		t.Fatalf("원격이 사람으로 초대: %d", st)
	}
	// 원격 포트에 없는 경로
	for _, p := range []string{"/events", "/admin/info", "/config", "/", "/rooms/r/export"} {
		if st := raw("GET", p, "", rc.Token); st != 404 {
			t.Fatalf("%s 는 원격에서 404 여야 함: %d", p, st)
		}
	}
	if st := raw("GET", "/rooms", "", ""); st != 401 {
		t.Fatalf("토큰 없이: %d", st)
	}
	if st := raw("GET", "/rooms", "", "bad"); st != 401 {
		t.Fatalf("잘못된 토큰: %d", st)
	}

	// 호스트 쪽 연결 목록
	st, err := lc.RemoteStatus()
	must(err)
	if !st.Enabled || len(st.Connections) != 1 || st.Connections[0].Agent != G || st.Fingerprint == "" {
		t.Fatalf("원격 상태: %+v", st)
	}

	// 연결 끊기 → 토큰 무효
	must(rcl.DisconnectRemote())
	if st := raw("GET", "/rooms", "", rc.Token); st != 401 {
		t.Fatalf("끊은 뒤: %d", st)
	}
	// 원격 포트 닫기
	_, err = lc.SetRemote(false, 0)
	must(err)
	if srv.RemotePort() != 0 {
		t.Fatal("닫혀야 함")
	}
}

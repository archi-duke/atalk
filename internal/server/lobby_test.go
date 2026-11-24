package server

import (
	"fmt"
	"strings"
	"testing"

	"atalk/internal/client"
	"atalk/internal/core"
)

// 대기실: 코드 없이 접속 → 네임카드만(아무 방도 못 씀) → 호스트에서 확인 숫자 대조 → 초대하면 토큰을 받고 그 방에 들어감.
func TestLobbyFlow(t *testing.T) {
	t.Setenv("ATALK_CONFIG_DIR", t.TempDir())
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
	rp, err := srv.StartRemote(0)
	must(err)
	url := fmt.Sprintf("https://127.0.0.1:%d", rp)
	lead, err := lc.Register(core.RegisterInput{Kind: "claude-code", SessionID: "lead"})
	must(err)
	L := lead.Agent.ID
	_, err = lc.CreateRoom(L, "r", "원격 시험")
	must(err)

	me := core.Host{ID: "guest-pc", Name: "PC-B", OS: "linux"}
	name := "손님"
	card := core.RegisterInput{Kind: "claude-code", SessionID: "guest", Name: &name}

	// 대기실을 끄면 코드 없이는 못 들어옴
	_, err = lc.SetLobby(false)
	must(err)
	if _, _, err := client.EnterLobby(client.PairInput{URL: url, Card: card, Host: me, Cwd: "/home/guest/proj"}); err == nil || !strings.Contains(err.Error(), "대기실") {
		t.Fatalf("대기실이 꺼져 있으면 실패: %v", err)
	}
	_, err = lc.SetLobby(true)
	must(err)

	lb, v, err := client.EnterLobby(client.PairInput{URL: url, Card: card, Host: me, Cwd: "/home/guest/proj"})
	must(err)
	G := v.ID
	if len(lb.Confirm) != 6 || client.LoadLobby(G) == nil || client.LoadConn(G) != nil {
		t.Fatalf("대기실 정보: %+v", lb)
	}
	// 호스트 화면: 대기실 카드에 같은 확인 숫자, 경로 없음
	st, err := lc.RemoteStatus()
	must(err)
	if len(st.Lobby) != 1 || st.Lobby[0].ID != G || st.Lobby[0].Lobby.Confirm != lb.Confirm || st.Lobby[0].Cwd != nil || st.Lobby[0].Name != "손님" {
		t.Fatalf("호스트에서 본 대기실: %+v", st.Lobby)
	}
	// 대기실에서는 토큰이 없어 아무것도 못 함
	if _, err := client.NewRemote(&client.RemoteConn{URL: url, Token: "x", Fingerprint: lb.Fingerprint}).Rooms(false); err == nil {
		t.Fatal("토큰 없이 방 목록을 보면 안 됨")
	}
	// 비밀값이 다르면 토큰을 못 받음
	fake := *lb
	fake.Secret = "wrong-secret-wrong-secret"
	if _, err := fake.Poll(); err == nil {
		t.Fatal("비밀값이 다르면 실패해야 함")
	}
	// 초대 전에는 아직 대기
	room, err := lb.Poll()
	must(err)
	if room != "" || client.LoadConn(G) != nil {
		t.Fatal("초대 전에는 대기")
	}
	// 방장이 초대 → 다음 확인에서 토큰을 받고 그 방에 들어가 있음 (인사·자기소개 전)
	must(lc.Invite(L, "r", G))
	room, err = lb.Poll()
	must(err)
	if room != "r" || client.LoadConn(G) == nil || client.LoadLobby(G) != nil {
		t.Fatalf("초대 뒤: room=%q", room)
	}
	rcl := client.ForAgent(G)
	got, err := rcl.Agent(G)
	must(err)
	if got.Room == nil || *got.Room != "r" || !got.Intro || got.Lobby != nil {
		t.Fatalf("원격에서 본 내 카드: %+v", got)
	}
	if st, _ := lc.RemoteStatus(); len(st.Lobby) != 0 || len(st.Connections) != 1 {
		t.Fatalf("초대 뒤 대기실은 비고 연결이 하나: %+v", st)
	}
	_, err = rcl.Post(G, "r", core.PostInput{Body: "안녕하세요, 손님입니다."})
	must(err)

	// 다른 손님: 대기실에서 나가면 카드도 지워짐
	card2 := core.RegisterInput{Kind: "claude-code", SessionID: "guest2"}
	lb2, v2, err := client.EnterLobby(client.PairInput{URL: url, Card: card2, Host: me})
	must(err)
	must(lb2.Leave())
	if _, err := lc.Agent(v2.ID); err == nil {
		t.Fatal("대기실에서 나가면 카드가 지워져야 함")
	}
}

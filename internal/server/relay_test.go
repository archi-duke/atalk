package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"atalk/internal/client"
	"atalk/internal/core"
)

// 중계: 플러그인이 이 PC 데몬의 /relay/<agent>/… 를 부르면, 원격에 연결된 에이전트는 원격 데몬으로, 아니면 이 PC 에서 처리.
func TestRelay(t *testing.T) {
	t.Setenv("ATALK_CONFIG_DIR", t.TempDir())
	start := func(machine string, remote bool) (*Server, string) {
		opts := Options{Root: t.TempDir(), Addr: "127.0.0.1:0", MachineID: machine}
		if remote {
			opts.RemoteDir = t.TempDir()
		}
		srv, err := New(opts)
		if err != nil {
			t.Fatal(err)
		}
		p, err := srv.Listen()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { srv.Close() })
		return srv, fmt.Sprintf("http://127.0.0.1:%d", p)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	host, hostURL := start("host-pc", true)
	_, guestURL := start("guest-pc", false)
	rp, err := host.StartRemote(0)
	must(err)
	hc := client.New(hostURL)
	lead, err := hc.Register(core.RegisterInput{Kind: "claude-code", SessionID: "lead"})
	must(err)
	_, err = hc.CreateRoom(lead.Agent.ID, "r", "")
	must(err)

	// 게스트 PC 의 opencode 세션 두 개: g1 은 호스트에 연결, g2 는 연결 안 함
	gc := client.New(guestURL)
	_, err = gc.Register(core.RegisterInput{Kind: "opencode", SessionID: "g2"})
	must(err)
	pc, err := hc.NewPairCode()
	must(err)
	_, _, err = client.Pair(client.PairInput{URL: fmt.Sprintf("https://127.0.0.1:%d", rp), Code: pc.Code,
		Card: core.RegisterInput{Kind: "opencode", SessionID: "g1"}, Host: core.Host{ID: "guest-pc", Name: "PC-G", OS: "linux"}, Cwd: "/home/g/proj"})
	must(err)

	call := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, guestURL+path, strings.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		must(err)
		defer res.Body.Close()
		var j map[string]any
		_ = json.NewDecoder(res.Body).Decode(&j)
		return res.StatusCode, j
	}

	// 연결 안 한 세션: 이 PC 데몬에서 처리
	if st, j := call("GET", "/relay/opencode_g2/agents/opencode_g2", ""); st != 200 || j["agent"].(map[string]any)["local"] != true {
		t.Fatalf("로컬 중계: %d %v", st, j)
	}
	// 연결한 세션: 등록(플러그인이 cwd·pid·provisional 을 보냄) → 원격 데몬에 경로 없이 등록
	st, j := call("POST", "/relay/opencode_g1/agents", `{"kind":"opencode","session_id":"g1","cwd":"/home/g/proj","pid":4242,"provisional":true}`)
	if st != 200 {
		t.Fatalf("원격 등록 중계: %d %v", st, j)
	}
	v, err := host.Core.GetAgent("opencode_g1")
	must(err)
	if v.Local || v.Cwd != nil || v.Pid != nil || v.Host.ID != "guest-pc" || v.Provisional || v.WorkspaceID == "" {
		t.Fatalf("호스트에 등록된 카드: %+v", v)
	}
	// 방 목록·참여 신청·메시지가 호스트 데몬으로
	if st, j := call("GET", "/relay/opencode_g1/rooms", ""); st != 200 || len(j["rooms"].([]any)) != 1 {
		t.Fatalf("원격 방 목록: %d %v", st, j)
	}
	if st, _ := call("POST", "/relay/opencode_g1/rooms/r/requests", `{"agent":"opencode_g1"}`); st != 200 {
		t.Fatalf("참여 신청: %d", st)
	}
	must(hc.RespondRequest(lead.Agent.ID, "r", "opencode_g1", true))
	if st, _ := call("POST", "/relay/opencode_g1/rooms/r/messages", `{"agent":"opencode_g1","type":"result","body":"중계로 보냄"}`); st != 200 {
		t.Fatalf("메시지: %d", st)
	}
	u, err := hc.Unread(lead.Agent.ID, true)
	must(err)
	if last := u.Messages[len(u.Messages)-1]; last.From != "opencode_g1" || last.Body != "중계로 보냄" {
		t.Fatalf("호스트가 받은 메시지: %+v", last)
	}
	// 중계로도 다른 에이전트를 사칭할 수 없음 (원격 데몬이 토큰으로 막음)
	if st, _ := call("POST", "/relay/opencode_g1/rooms/r/messages", `{"agent":"`+lead.Agent.ID+`","body":"사칭"}`); st != 403 {
		t.Fatalf("사칭: %d", st)
	}
	// 원격 데몬이 꺼지면 502
	host.StopRemote()
	if st, j := call("GET", "/relay/opencode_g1/rooms", ""); st != 502 || j["error"].(map[string]any)["code"] != "remote_unreachable" {
		t.Fatalf("원격 꺼짐: %d %v", st, j)
	}
}

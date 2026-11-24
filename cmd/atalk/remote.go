package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/paths"
)

// runRemote: 호스트 PC 에서 원격 포트를 관리한다 (로컬 데몬 관리 API).
func runRemote(lc *client.Client, at args, out func(any, string)) {
	sub := "status"
	if len(at.pos) > 0 {
		sub = at.pos[0]
	}
	switch sub {
	case "on", "off":
		st, err := lc.SetRemote(sub == "on", at.num("port", 0))
		check(err)
		out(st, remoteStatusText(st))
	case "status":
		st, err := lc.RemoteStatus()
		check(err)
		out(st, remoteStatusText(st))
	case "code":
		pc, err := lc.NewPairCode()
		check(err)
		var lines []string
		lines = append(lines, "참여 코드 (한 번만 쓸 수 있음, 10분 뒤 만료):", "  "+pc.Code, "", "연결할 PC 의 에이전트에서 실행:")
		if len(pc.URLs) == 0 {
			lines = append(lines, "  atalk connect https://<이 PC 주소>:<포트> --code "+pc.Code)
		}
		for _, u := range pc.URLs {
			lines = append(lines, "  atalk connect "+u+" --code "+pc.Code)
		}
		out(pc, strings.Join(lines, "\n"))
	case "revoke":
		agent := at.arg(1, "AGENT")
		check(lc.RevokeRemote(agent))
		out(map[string]bool{"ok": true}, agent+" 원격 연결을 끊음 (토큰 철회)")
	case "lobby":
		switch at.arg(1, "on|off|kick AGENT") {
		case "on", "off":
			st, err := lc.SetLobby(at.pos[1] == "on")
			check(err)
			out(st, remoteStatusText(st))
		case "kick":
			agent := at.arg(2, "AGENT")
			check(lc.KickLobby(agent))
			out(map[string]bool{"ok": true}, agent+" 를 대기실에서 내보냄")
		default:
			fail("bad_arg", "atalk remote lobby on|off|kick AGENT")
		}
	default:
		fail("bad_arg", "atalk remote on|off|status|code|revoke AGENT|lobby on|off|kick AGENT")
	}
}

func remoteStatusText(st client.RemoteStatus) string {
	var b strings.Builder
	if st.Enabled {
		fmt.Fprintf(&b, "원격 포트: 켜짐 (HTTPS %d)\n", st.Port)
		for _, a := range st.Addresses {
			fmt.Fprintf(&b, "  https://%s:%d\n", a, st.Port)
		}
	} else {
		fmt.Fprintf(&b, "원격 포트: 꺼짐 (켜기: atalk remote on [--port %d])\n", st.SavedPort)
	}
	if st.Fingerprint != "" {
		fmt.Fprintf(&b, "인증서 지문: %s\n", st.Fingerprint)
	}
	if st.LobbyOn {
		b.WriteString("대기실: 받음 (다른 PC 에서 코드 없이 atalk connect → 여기서 초대하면 들어옴. 끄기: atalk remote lobby off)\n")
	} else {
		b.WriteString("대기실: 받지 않음 (참여 코드로만 연결. 켜기: atalk remote lobby on)\n")
	}
	for _, a := range st.Lobby {
		where := ""
		if a.Host != nil {
			where = a.Host.Name
		}
		fmt.Fprintf(&b, "  대기 중 %s  %q  %s  확인 숫자 %s  주소 %s\n", a.ID, a.Name, where, a.Lobby.Confirm, a.Lobby.IP)
	}
	if len(st.Connections) == 0 {
		b.WriteString("연결된 원격 에이전트: 없음")
	} else {
		b.WriteString("연결된 원격 에이전트:")
		for _, c := range st.Connections {
			where := ""
			if c.Host != nil {
				where = c.Host.Name
			}
			fmt.Fprintf(&b, "\n  %s  %q  %s  [%s]  마지막 요청 %s", c.Agent, c.Name, where, c.Status, orText(c.LastSeen, "-"))
		}
	}
	return b.String()
}

// runConnect: 이 에이전트를 다른 PC 의 데몬에 연결한다. 네임카드(이름·설명·능력·작업 이름)를 원격에 등록하고 토큰을 저장한다.
func runConnect(lc *client.Client, a, at args, me func() string, out func(any, string)) {
	u := at.arg(0, "URL (https://PC:7421)")
	code := a.str("code") // 없으면 대기실로 (그 PC 에서 초대하면 들어감)
	id := me()
	if client.LoadConn(id) != nil {
		fail("already_connected", "이미 원격 데몬에 연결돼 있음 (atalk disconnect 뒤에 다시)")
	}
	if old := client.LoadLobby(id); old != nil {
		_ = old.Leave()
	}
	kind, sid, ok := strings.Cut(id, "_")
	if !ok || !core.IsValidKind(kind) || sid == "" {
		fail("bad_agent", "에이전트 id 형식이 아님: "+id)
	}
	card := core.RegisterInput{Kind: kind, SessionID: sid}
	cwd, _ := os.Getwd()
	if v, err := lc.Agent(id); err == nil { // 이 PC 에 등록된 네임카드를 그대로 가져간다
		card.Name, card.Description, card.Capabilities, card.Workspace = &v.Name, &v.Description, &v.Capabilities, v.Workspace
		if v.Cwd != nil && *v.Cwd != "" {
			cwd = *v.Cwd
		}
		if v.Room != nil { // 세션 하나 = 방 하나: 이 PC 의 방에서는 나온다
			_ = lc.Leave(id, "다른 PC 의 대화방으로 연결")
		}
	} else if v := os.Getenv("ATALK_NAME"); v != "" {
		card.Name = &v
	}
	mid := paths.MachineID()
	host := core.Host{ID: mid, Name: core.DefaultMachineName(mid), OS: core.LocalOS()}
	if cfg, err := lc.Config(); err == nil && cfg.MachineName != "" {
		host.Name = cfg.MachineName
	}
	if code == "" {
		lobby, v, err := client.EnterLobby(client.PairInput{URL: u, Card: card, Host: host, Cwd: cwd})
		var ce *client.Error
		if errors.As(err, &ce) && ce.Status == 0 && ce.Code == "unreachable" {
			fail("unreachable", ce.Message+"\n(호스트 PC 에서 atalk remote on 을 했는지, 방화벽이 포트를 막지 않는지 확인하세요)")
		}
		check(err)
		out(map[string]any{"ok": true, "lobby": true, "agent": v, "url": lobby.URL, "host": lobby.HostName, "confirm": lobby.Confirm},
			fmt.Sprintf("%s (%s) 의 대기실에 들어감: %s %q\n확인 숫자: %s\n  → 그 PC 의 사람에게 이 숫자를 알려 주세요. 그 PC 화면의 대기실 카드에 같은 숫자가 보이면 맞는 상대입니다.\n그 PC 에서 초대하면 자동으로 그 방에 들어갑니다 (이 PC 데몬이 3초마다 확인). 1시간 안에 초대가 없으면 대기실에서 빠집니다.\n그만두기: atalk disconnect",
				orText(lobby.HostName, "원격 PC"), lobby.URL, v.ID, v.Name, lobby.Confirm))
		return
	}
	rc, v, err := client.Pair(client.PairInput{URL: u, Code: code, Card: card, Host: host, Cwd: cwd})
	var ce *client.Error
	if errors.As(err, &ce) && ce.Status == 0 && ce.Code == "unreachable" {
		fail("unreachable", ce.Message+"\n(호스트 PC 에서 atalk remote on 을 했는지, 방화벽이 포트를 막지 않는지 확인하세요)")
	}
	check(err)
	out(map[string]any{"ok": true, "agent": v, "url": rc.URL, "host": rc.HostName},
		fmt.Sprintf("%s (%s) 에 연결함: %s %q\n이 세션의 atalk 명령과 훅은 이제 그 PC 의 데몬을 씁니다. 방 목록: atalk rooms / 참여: atalk request ROOM / 끊기: atalk disconnect",
			orText(rc.HostName, "원격 PC"), rc.URL, v.ID, v.Name))
}

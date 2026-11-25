package server

// 원격 연결 돌보기 (에이전트 쪽 PC). 3초마다:
//
//   - 대기실: 이 PC 의 에이전트가 다른 PC 의 대기실에 있으면(<설정 폴더>/remote/*.lobby.json) 그 PC 에 확인한다.
//     초대가 와서 토큰을 받으면 연결 파일로 바뀌고, 그 뒤로는 그 에이전트의 훅·플러그인(중계)이 원격 데몬을 쓴다.
//   - 하트비트: 다른 PC 데몬에 연결된 이 PC 에이전트의 프로세스가 살아 있으면 대신 하트비트를 보낸다.
//     원격 데몬은 요청이 3분 끊기면 자리 비움으로 보는데, Claude 의 대기 훅은 Claude Code 가 일정 시간 뒤 끝내 버려
//     쉬고 있는 세션의 요청이 끊긴다. 프로세스 생존은 이 PC 데몬만 알 수 있으므로 여기서 맡는다.

import (
	"log"
	"time"

	"atalk/internal/client"
)

const (
	lobbyPollEvery = 3 * time.Second
	heartbeatEvery = 60 * time.Second // core.RemoteTTL(3분) 안에 여러 번
)

func (s *Server) watchLobbies(stop <-chan struct{}) {
	t := time.NewTicker(lobbyPollEvery)
	defer t.Stop()
	lastBeat := time.Time{}
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		for _, lc := range client.Lobbies() {
			room, err := lc.Poll()
			if err == nil && room != "" {
				log.Printf("대기실: %s 가 %s (%s) 의 %s 방에 들어감", lc.Agent, lc.HostName, lc.URL, room)
			}
		}
		s.wakeCodex()
		s.Core.SweepListening() // 대기 훅이 끊긴 Claude 를 화면에 "알림 꺼짐" 으로
		if time.Since(lastBeat) >= heartbeatEvery {
			lastBeat = time.Now()
			s.beatRemotes()
		}
	}
}

// beatRemotes 는 다른 PC 데몬에 연결된 이 PC 에이전트 중 프로세스가 살아 있는 것의 하트비트를 보낸다.
func (s *Server) beatRemotes() {
	for _, rc := range client.Conns() {
		v, err := s.Core.GetAgent(rc.Agent) // 이 PC 데몬의 카드 (PID 로 생존 판단)
		if err != nil || !v.Local || v.Pid == nil || v.Status == "offline" {
			continue
		}
		_, _ = s.relayClients.get(rc).Agent(rc.Agent) // 아무 인증 요청이나 원격 데몬에서는 하트비트
	}
}

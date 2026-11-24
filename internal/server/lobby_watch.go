package server

// 대기실 확인 (에이전트 쪽 PC): 이 PC 의 에이전트가 다른 PC 의 대기실에 있으면(<설정 폴더>/remote/*.lobby.json)
// 3초마다 그 PC 에 확인한다 (하트비트 겸용). 초대가 와서 토큰을 받으면 연결 파일로 바뀌고,
// 그 뒤로는 그 에이전트의 훅·플러그인(중계)이 원격 데몬을 쓴다. Claude 훅·opencode 플러그인이 따로 확인할 필요가 없다.

import (
	"log"
	"time"

	"atalk/internal/client"
)

const lobbyPollEvery = 3 * time.Second

func (s *Server) watchLobbies(stop <-chan struct{}) {
	t := time.NewTicker(lobbyPollEvery)
	defer t.Stop()
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
	}
}

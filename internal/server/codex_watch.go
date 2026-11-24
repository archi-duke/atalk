package server

// Codex 깨우기 (에이전트 쪽 PC): 이 PC 의 Codex 에이전트가 쉬고 있을 때 나에게 온 메시지가 있으면
// `codex queue` 로 열린 TUI 에 넣어 새 턴을 시작하게 한다 (hook.CodexWaker). 원격 연결된 에이전트는 원격 데몬의 메시지를 본다.

import (
	"errors"
	"log"
	"strings"
	"time"

	"atalk/internal/client"
	"atalk/internal/daemon"
	"atalk/internal/procs"
)

const codexQueueTimeout = 30 * time.Second

func codexQueue(sessionID, text string) error {
	cmd, err := procs.CodexCommand("queue", "--thread", sessionID, "--message", text)
	if err != nil {
		return errors.New("codex 를 찾을 수 없음: " + err.Error())
	}
	daemon.HideCmd(cmd)
	done := make(chan error, 1)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return errors.New(strings.TrimSpace(out.String()))
		}
		return nil
	case <-time.After(codexQueueTimeout):
		_ = cmd.Process.Kill()
		return errors.New("codex queue 응답 없음")
	}
}

// wakeCodex 는 이 PC 의 Codex 에이전트들을 확인한다 (watchLobbies 가 3초마다 부름).
func (s *Server) wakeCodex() {
	list, err := s.Core.ListAgents("", "codex")
	if err != nil {
		return
	}
	for _, v := range list {
		if !v.Local || v.Pid == nil || v.Status == "offline" {
			s.codex.Check(nil, v) // 상태만 정리 (아래에서 c 를 쓰지 않음)
			continue
		}
		var c *client.Client
		if rc := client.LoadConn(v.ID); rc != nil { // 다른 PC 데몬에 연결됨 → 메시지·작업 중 표시는 그쪽에
			c = s.relayClients.get(rc)
			a, err := c.Agent(v.ID)
			if err != nil {
				continue
			}
			v.Busy, v.Room, v.Intro, v.Status = a.Busy, a.Room, a.Intro, a.Status
		} else {
			c = client.New("")
		}
		if ok, err := s.codex.Check(c, v); err != nil {
			log.Printf("codex 깨우기 실패 (%s): %v", v.ID, err)
		} else if ok {
			log.Printf("codex 깨우기: %s 에 새 메시지를 넣음", v.ID)
		}
	}
}


package server

// 시스템 확인 창 알림: 이 PC 화면에 방화벽 허용·관리자 권한(UAC) 창이 뜨면, 그 창을 띄운 에이전트(못 찾으면 일하는 중인
// 이 PC 에이전트, 그것도 없으면 방에 있는 이 PC 에이전트 모두)를 🙋 사용자 확인 필요 로 표시한다 → 방에 카드가 뜨고
// 휴대폰 웹 화면에서도 보인다. 창이 닫히면 풀린다. 창을 대신 누르지는 않는다 (보안 확인은 사람이).

import (
	"log"
	"slices"
	"sync"

	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/procs"
)

type promptWatch struct {
	mu   sync.Mutex
	seen map[string]promptNote // 알린 창 → 알린 에이전트와 글
}

type promptNote struct {
	agents []string
	text   string
}

// osPrompts 는 테스트에서 바꿔 끼운다.
var osPrompts = procs.SystemPrompts

// watchPrompts 는 watchLobbies 가 3초마다 부른다.
func (s *Server) watchPrompts() {
	ps := osPrompts()
	s.prompts.mu.Lock()
	defer s.prompts.mu.Unlock()
	if s.prompts.seen == nil {
		if len(ps) == 0 {
			return
		}
		s.prompts.seen = map[string]promptNote{}
	}
	now := map[string]bool{}
	for _, p := range ps {
		now[p.ID] = true
		if _, ok := s.prompts.seen[p.ID]; ok {
			continue
		}
		text := p.Describe()
		agents := s.promptAgents(p)
		for _, id := range agents {
			if err := s.needInputFor(id, text); err != nil {
				log.Printf("확인 창 알림 실패 (%s): %v", id, err)
			}
		}
		log.Printf("확인 창: %s → %v", text, agents)
		s.prompts.seen[p.ID] = promptNote{agents: agents, text: text}
	}
	for id, n := range s.prompts.seen {
		if now[id] {
			continue
		}
		for _, a := range n.agents { // 창이 닫힘 → 그 알림만 푼다 (그사이 다른 확인 요청으로 바뀌었으면 두기)
			if v, err := s.agentView(a); err == nil && v.NeedsInput == n.text {
				_ = s.needInputFor(a, "")
			}
		}
		delete(s.prompts.seen, id)
	}
}

// promptAgents 는 그 창을 알릴 이 PC 에이전트들 (방에 들어가 있는 것만).
func (s *Server) promptAgents(p procs.Prompt) []string {
	list, err := s.Core.ListAgents("", "")
	if err != nil {
		return nil
	}
	var inRoom []core.AgentView
	for _, v := range list {
		if !v.Local || v.Pid == nil || v.Status == "offline" || v.Lobby != nil {
			continue
		}
		if rv, err := s.agentView(v.ID); err == nil { // 다른 PC 데몬에 연결된 에이전트는 방·상태가 그쪽에
			v.Room, v.Busy = rv.Room, rv.Busy
		}
		if v.Room != nil {
			inRoom = append(inRoom, v)
		}
	}
	pick := func(ok func(core.AgentView) bool) (out []string) {
		for _, v := range inRoom {
			if ok(v) {
				out = append(out, v.ID)
			}
		}
		return out
	}
	if p.Pid > 0 {
		anc := append([]int{p.Pid}, procs.Ancestors(p.Pid)...)
		if out := pick(func(v core.AgentView) bool { return slices.Contains(anc, *v.Pid) }); len(out) > 0 {
			return out
		}
	}
	if out := pick(func(v core.AgentView) bool { return v.Busy }); len(out) > 0 {
		return out
	}
	return pick(func(core.AgentView) bool { return true })
}

// agentClient 는 그 에이전트의 방·메시지가 있는 데몬 (다른 PC 에 연결됐으면 그쪽).
func (s *Server) agentClient(id string) *client.Client {
	if rc := client.LoadConn(id); rc != nil {
		return s.relayClients.get(rc)
	}
	return nil
}

func (s *Server) agentView(id string) (core.AgentView, error) {
	if c := s.agentClient(id); c != nil {
		a, err := c.Agent(id)
		if err != nil {
			return core.AgentView{}, err
		}
		return a, nil
	}
	return s.Core.GetAgent(id)
}

func (s *Server) needInputFor(id, text string) error {
	if c := s.agentClient(id); c != nil {
		return c.HoldNeedInput(id, text)
	}
	return s.Core.HoldNeedInput(id, text)
}

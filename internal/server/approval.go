package server

// 헤드리스 허가 요청: 창 없이 도는 Claude Code 턴(claude -p)은 허가를 물을 창이 없으므로,
// --permission-prompt-tool 로 ATalk MCP 도구(atalk mcp-permission)에 묻는다. 그 도구가 여기로 요청하면
// 대화방에 확인 카드("🙋 사용자 확인 필요")를 올리고, 사람이 웹 카드의 [허용]/[거부]를 누르거나 시간이 지날 때까지 기다린다.

import (
	"net/http"
	"sync"
	"time"

	"atalk/internal/core"
)

// approvalWait 는 사람의 답을 기다리는 최대 시간. 지나면 거부한다.
const approvalWait = 10 * time.Minute

type approvals struct {
	mu sync.Mutex
	m  map[string]chan bool // 에이전트 → 답을 받을 통로 (한 에이전트에 하나)
}

func (s *Server) approvalRoutes(h func(string, handler)) {
	c := s.Core
	// 허가 요청 (MCP 도구가 부름): 답이 올 때까지 기다린다. agent 대신 spawn 표식으로 에이전트를 찾을 수도 있다 (헤드리스로 띄운 첫 턴)
	h("POST /approval", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Agent string `json:"agent"`
			Spawn string `json:"spawn"`
			Text  string `json:"text"` // 확인 카드에 보일 내용 (예: "Bash 실행 허가: go test ./...")
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		id := b.Agent
		if id == "" && b.Spawn != "" {
			id = c.AgentBySpawn(b.Spawn)
		}
		if _, err := c.GetAgent(id); err != nil {
			return map[string]any{"allow": false, "message": "ATalk 에서 이 세션을 찾지 못해 허가를 물을 수 없음"}, nil
		}
		ch := make(chan bool, 1)
		s.approvals.mu.Lock()
		if s.approvals.m == nil {
			s.approvals.m = map[string]chan bool{}
		}
		if old, ok := s.approvals.m[id]; ok { // 앞선 요청이 남아 있으면 거부로 끝낸다
			select {
			case old <- false:
			default:
			}
		}
		s.approvals.m[id] = ch
		s.approvals.mu.Unlock()
		_ = c.SetNeedInput(id, b.Text)
		c.SetApproval(id, true)
		defer func() {
			s.approvals.mu.Lock()
			if s.approvals.m[id] == ch {
				delete(s.approvals.m, id)
				c.SetApproval(id, false)
				_ = c.SetNeedInput(id, "")
			}
			s.approvals.mu.Unlock()
		}()
		select {
		case ok := <-ch:
			if ok {
				return map[string]any{"allow": true}, nil
			}
			return map[string]any{"allow": false, "message": "사용자가 거부함 (ATalk 웹 화면)"}, nil
		case <-time.After(approvalWait):
			return map[string]any{"allow": false, "message": "10분 동안 사용자의 답이 없어 거부함"}, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	// 사람의 답 (웹 카드의 [허용]/[거부])
	h("POST /agents/{id}/approval", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Allow bool `json:"allow"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		s.approvals.mu.Lock()
		ch, ok := s.approvals.m[r.PathValue("id")]
		s.approvals.mu.Unlock()
		if !ok {
			return nil, &core.Error{Status: 409, Code: "no_approval", Message: "기다리는 허가 요청이 없음 (이미 답했거나 시간이 지남)"}
		}
		select {
		case ch <- b.Allow:
		default:
		}
		return map[string]any{}, nil
	})
}

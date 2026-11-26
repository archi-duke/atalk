package server

// 웹에서 답하는 확인 요청: Claude Code 의 PermissionRequest 훅(atalk hook claude-code permission-request)은
// 허가 창(도구 실행 허가, 선택 질문)이 뜰 때 불리고, 그 창과 함께 기다리다 답을 돌려주면 그 답으로 창이 처리된다.
// 훅이 여기로 물으면 대화방에 확인 카드("🙋 사용자 확인 필요")를 올리고, 사람이 웹 카드에서 답하거나
// CLI 창에서 먼저 답할 때(에이전트가 일을 이어 가면 확인 필요 표시가 풀림)까지 기다린다. 창이 있든(CLI) 없든(헤드리스) 같다.

import (
	"net/http"
	"sync"
	"time"

	"atalk/internal/core"
)

// approvalWait 는 웹의 답을 기다리는 최대 시간. 지나면 답하지 않은 것으로 끝낸다 (CLI 창은 그대로 기다림, 헤드리스는 거부됨).
const approvalWait = 30 * time.Minute

type approvalAnswer struct {
	allow  bool
	answer string
}

type approvals struct {
	mu sync.Mutex
	m  map[string]chan approvalAnswer // 에이전트 → 답을 받을 통로 (한 에이전트에 하나)
}

func (s *Server) approvalRoutes(h func(string, handler)) {
	c := s.Core
	// 확인 요청 (훅이 부름): 웹 카드의 답, CLI 에서 먼저 답함, 시간 초과 중 먼저 오는 것으로 끝난다
	h("POST /approval", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Agent    string `json:"agent"`
			Text     string `json:"text"`     // 카드 내용 (예: "Bash 실행 허가: go test ./..." 또는 "질문 (A / B)")
			Question bool   `json:"question"` // 선택 질문이면 답(고른 선택지)을 돌려준다
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		id := b.Agent
		if _, err := c.GetAgent(id); err != nil {
			return map[string]any{"decided": false}, nil
		}
		ch := make(chan approvalAnswer, 1)
		s.approvals.mu.Lock()
		if s.approvals.m == nil {
			s.approvals.m = map[string]chan approvalAnswer{}
		}
		s.approvals.m[id] = ch // 앞선 요청이 남아 있었으면 그쪽은 아래 확인(표시 풀림)이나 시간 초과로 끝난다
		s.approvals.mu.Unlock()
		_ = c.SetNeedInput(id, b.Text)
		c.SetApproval(id, true)
		defer func() {
			s.approvals.mu.Lock()
			if s.approvals.m[id] == ch {
				delete(s.approvals.m, id)
				c.SetApproval(id, false)
			}
			s.approvals.mu.Unlock()
		}()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		deadline := time.After(approvalWait)
		for {
			select {
			case a := <-ch:
				_ = c.SetNeedInput(id, "")
				switch {
				case a.answer != "":
					c.NoteAskAnswer(id, a.answer+" (웹)")
				case a.allow:
					c.NoteAskAnswer(id, "허용 (웹)")
				default:
					c.NoteAskAnswer(id, "거부 (웹)")
				}
				if a.allow {
					return map[string]any{"decided": true, "allow": true, "answer": a.answer}, nil
				}
				return map[string]any{"decided": true, "allow": false, "message": "사용자가 거부함 (ATalk 웹 화면)"}, nil
			case <-tick.C:
				// CLI 창에서 먼저 답함 → 에이전트가 일을 이어 가면서 확인 필요 표시가 풀림 (또는 새 요청이 이어받음)
				v, err := c.GetAgent(id)
				s.approvals.mu.Lock()
				mine := s.approvals.m[id] == ch
				s.approvals.mu.Unlock()
				if err != nil || v.NeedsInput == "" || !mine {
					if err == nil && v.NeedsInput == "" && mine {
						c.NoteAskAnswer(id, "CLI 에서 답함")
					}
					return map[string]any{"decided": false}, nil
				}
			case <-deadline:
				return map[string]any{"decided": false, "message": "30분 동안 답이 없음"}, nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
	})
	// 사람의 답 (웹 카드의 [허용]/[거부] 또는 선택지)
	h("POST /agents/{id}/approval", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Allow  bool   `json:"allow"`
			Answer string `json:"answer"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		s.approvals.mu.Lock()
		ch, ok := s.approvals.m[r.PathValue("id")]
		s.approvals.mu.Unlock()
		if !ok {
			return nil, &core.Error{Status: 409, Code: "no_approval", Message: "기다리는 확인 요청이 없음 (이미 답했거나 CLI 에서 답함)"}
		}
		select {
		case ch <- approvalAnswer{allow: b.Allow || b.Answer != "", answer: b.Answer}:
		default:
		}
		return map[string]any{}, nil
	})
}

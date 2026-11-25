package hook

import (
	"encoding/json"
	"slices"
	"strings"

	"atalk/internal/core"
)

// Codex 의 대화 기록(rollout JSONL, CODEX_HOME/sessions/…) 읽기 — Claude Code transcript 와 형식이 달라 따로 읽는다.
//
//	{"type":"session_meta", …}                                                     세션 정보 (첫 줄)
//	{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"…"}]}}}   입력
//	{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"…"}]}}         응답 문장
//
// 시스템이 넣은 user 메시지(AGENTS.md 지시 등)는 response_item 의 user 로만 들어오고 UserMessage 이벤트는 사람 입력·queue 입력뿐이다.

func isCodexRollout(lines []string) bool {
	for _, l := range lines[:min(len(lines), 5)] {
		if strings.Contains(l, `"type":"session_meta"`) || strings.Contains(l, `"type":"response_item"`) || strings.Contains(l, `"type":"event_msg"`) {
			return true
		}
	}
	return false
}

// codexTurn 은 마지막 입력 이후의 응답 문장들과 그 입력을 돌려준다 (turnInfo 와 같은 뜻).
// 입력이 ATalk 알림(codex queue 로 넣은 방 메시지)이면 prompt 대신 wake 에 그 메시지 번호들.
func codexTurn(lines []string) (texts []string, prompt string, wake []int) {
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Item struct {
					Type    string `json:"type"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"item"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(lines[i]), &e) != nil {
			continue
		}
		switch {
		case e.Type == "response_item" && e.Payload.Type == "message" && e.Payload.Role == "assistant":
			var parts []string
			for _, c := range e.Payload.Content {
				if c.Type == "output_text" && strings.TrimSpace(c.Text) != "" {
					parts = append(parts, c.Text)
				}
			}
			if len(parts) > 0 {
				texts = append(texts, strings.TrimSpace(strings.Join(parts, "\n")))
			}
		case e.Type == "event_msg" && e.Payload.Type == "item_completed" && e.Payload.Item.Type == "UserMessage":
			var parts []string
			for _, c := range e.Payload.Item.Content {
				if c.Type == "text" {
					parts = append(parts, c.Text)
				}
			}
			text := strings.Join(parts, "\n")
			if seqs := noticeSeqs(text); len(seqs) > 0 {
				wake = seqs
			} else if !core.IsSystemPrompt(text) {
				prompt = text
			}
			slices.Reverse(texts)
			return texts, prompt, wake // 입력 하나가 한 턴의 시작
		}
	}
	slices.Reverse(texts)
	return texts, prompt, wake
}

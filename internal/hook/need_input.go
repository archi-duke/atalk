package hook

import (
	"encoding/json"

	"atalk/internal/client"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// needInputText: Notification 훅 내용 중 사람의 확인이 필요한 것만 (도구 허가, 질문 창). 오래 쉬는 중 알림 등은 "".
func needInputText(in Input) string {
	switch in.NotificationType {
	case "permission_prompt", "elicitation_dialog":
		return strings.TrimSpace(in.Message)
	case "":
		// 예전 Claude Code: 종류 없이 내용만 — 허가 요청 문구로 구분
		if m := strings.TrimSpace(in.Message); strings.Contains(strings.ToLower(m), "permission") {
			return m
		}
	}
	return ""
}

// askText: AskUserQuestion 도구 입력의 질문과 고를 수 있는 답을 한 덩어리 글로.
func askText(raw json.RawMessage) string {
	var in struct {
		Questions []struct {
			Question string `json:"question"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	var b strings.Builder
	for i, q := range in.Questions {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(q.Question)
		var opts []string
		for _, o := range q.Options {
			opts = append(opts, o.Label)
		}
		if len(opts) > 0 {
			b.WriteString(" (" + strings.Join(opts, " / ") + ")")
		}
	}
	return strings.TrimSpace(b.String())
}

// permissionText: 도구 허가 요청(Codex PermissionRequest)을 사람이 읽을 한 줄로 — 도구 이름과 명령(있으면).
func permissionText(in Input) string {
	var ti struct {
		Command  any    `json:"command"`
		Path     string `json:"path"`
		FilePath string `json:"file_path"`
	}
	_ = json.Unmarshal(in.ToolInput, &ti)
	what := ""
	switch v := ti.Command.(type) {
	case string:
		what = v
	case []any:
		var parts []string
		for _, p := range v {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		what = strings.Join(parts, " ")
	}
	if what == "" {
		what = ti.Path
	}
	if what == "" {
		what = ti.FilePath
	}
	if what == "" && len(in.ToolInput) > 0 && string(in.ToolInput) != "null" {
		what = string(in.ToolInput)
	}
	if r := []rune(what); len(r) > 300 {
		what = string(r[:300]) + "…"
	}
	name := in.ToolName
	if name == "" {
		name = "도구"
	}
	text := name + " 실행 허가"
	if what != "" {
		text += ": " + what
	}
	if m := strings.TrimSpace(in.Message); m != "" {
		text += " — " + m
	}
	return text
}

// 곧 쓸 도구 기억 (PreToolUse → Notification): Claude Code 의 허가 알림 문구에는 어떤 도구·명령인지가 없어서.
type lastTool struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	At        time.Time       `json:"at"`
}

func lastToolFile(id string) string { return filepath.Join(os.TempDir(), "atalk-pretool-"+id+".json") }

func rememberTool(id string, in Input) {
	b, _ := json.Marshal(lastTool{ToolName: in.ToolName, ToolInput: in.ToolInput, At: time.Now()})
	_ = os.WriteFile(lastToolFile(id), b, 0o644)
}

// recentTool: 2분 안에 쓰려던 도구 (허가 창은 도구 바로 앞에 뜬다).
func recentTool(id string) (lastTool, bool) {
	var t lastTool
	b, err := os.ReadFile(lastToolFile(id))
	if err != nil || json.Unmarshal(b, &t) != nil || t.ToolName == "" || time.Since(t.At) > 2*time.Minute {
		return t, false
	}
	return t, true
}

// permissionRequest 는 Claude Code 의 PermissionRequest 훅: 허가 창(또는 선택 질문 창)이 CLI 에 뜬 채로,
// 대화방 확인 카드에 올려 웹에서 답하기를 기다린다. 웹에서 답하면 그 답으로 창을 처리하고(decision),
// CLI 에서 먼저 답하거나 시간이 지나면 아무것도 돌려주지 않는다 (창은 CLI 에서 그대로). 헤드리스(-p)에서도 같다.
func permissionRequest(c *client.Client, id string, in Input) any {
	question := in.ToolName == "AskUserQuestion"
	text := permissionText(in)
	if question {
		text = askText(in.ToolInput)
	}
	if text == "" {
		return nil
	}
	// 데몬이 다시 시작되는 등으로 연결이 끊기면 다시 묻는다 (창은 CLI 에 그대로 떠 있음). 답이 없음(Decided=false)이면 끝
	var r client.ApprovalResult
	var err error
	for deadline := time.Now().Add(30 * time.Minute); ; {
		if r, err = c.Approval(id, text, question); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil || !r.Decided {
		return nil
	}
	decision := map[string]any{"behavior": "deny", "message": orDefault(r.Message, "사용자가 거부함")}
	if r.Allow {
		decision = map[string]any{"behavior": "allow"}
		if question { // 선택 질문: 원래 입력에 답(질문 → 고른 선택지)을 붙여 돌려준다
			var ti map[string]any
			if json.Unmarshal(in.ToolInput, &ti) == nil {
				answers := map[string]any{}
				if qs, ok := ti["questions"].([]any); ok {
					for _, q := range qs {
						if m, ok := q.(map[string]any); ok {
							if qt, ok := m["question"].(string); ok {
								answers[qt] = r.Answer
							}
						}
					}
				}
				ti["answers"] = answers
				decision["updatedInput"] = ti
			}
		}
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": decision}}
}

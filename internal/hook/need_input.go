package hook

import (
	"encoding/json"
	"strings"
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
		Command any    `json:"command"`
		Path    string `json:"path"`
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

package hook

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
)

// progressLine 은 PostToolUse 의 도구 이름·입력으로 진행 줄 한 줄을 만든다 ("Bash — 테스트 실행").
// 도구 출력은 쓰지 않는다. 경로는 파일 이름만 (다른 PC 의 방에도 남을 수 있으므로 전체 경로는 내보내지 않는다).
func progressLine(tool string, input json.RawMessage) string {
	if tool == "" {
		return ""
	}
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string {
		v, _ := in[k].(string)
		return strings.TrimSpace(v)
	}
	first := func(s string) string { // 첫 줄, 80자
		s, _, _ = strings.Cut(s, "\n")
		if r := []rune(s); len(r) > 80 {
			s = string(r[:80]) + "…"
		}
		return s
	}
	base := func(p string) string {
		if p == "" {
			return ""
		}
		return filepath.Base(filepath.FromSlash(strings.ReplaceAll(p, `\`, "/")))
	}
	name := tool
	if strings.HasPrefix(tool, "mcp__") { // mcp__서버__도구 → 서버:도구
		parts := strings.SplitN(strings.TrimPrefix(tool, "mcp__"), "__", 2)
		name = strings.Join(parts, ":")
	}
	detail := ""
	switch tool {
	case "Bash", "PowerShell":
		if detail = str("description"); detail == "" {
			detail = first(str("command"))
		}
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit":
		detail = base(str("file_path"))
		if detail == "" {
			detail = base(str("notebook_path"))
		}
	case "Grep", "Glob":
		detail = first(str("pattern"))
	case "WebFetch":
		if u, err := url.Parse(str("url")); err == nil {
			detail = u.Host
		}
	case "WebSearch":
		detail = first(str("query"))
	case "Task", "Agent":
		detail = first(str("description"))
	case "TodoWrite":
		detail = "할 일 목록 갱신"
	default:
		for _, k := range []string{"description", "title", "name", "query"} {
			if detail = first(str(k)); detail != "" {
				break
			}
		}
	}
	if detail == "" {
		return name
	}
	return name + " — " + detail
}

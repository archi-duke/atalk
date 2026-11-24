package install

// Codex: <CODEX_HOME>/hooks.json 의 SessionStart/Stop/SessionEnd/UserPromptSubmit 훅 + <CODEX_HOME>/skills/atalk/SKILL.md.
// Codex 훅은 쉬고 있는 세션을 깨우지 못하므로 대기 훅은 없다 — 쉬는 동안 온 메시지는 데몬이 `codex queue` 로 넣는다.
// Codex 는 새로 생기거나 바뀐 훅을 사람이 /hooks 에서 신뢰해야 실행한다.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CodexDir 는 Codex 설정 폴더 (CODEX_HOME, 없으면 ~/.codex).
func CodexDir() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

func (p Paths) codexHooksFile() string { return filepath.Join(p.Codex, "hooks.json") }
func (p Paths) codexSkillFile() string { return filepath.Join(p.Codex, "skills", "atalk", "SKILL.md") }

var codexHookEvents = []struct {
	event, arg string
	timeout    int
}{
	{"SessionStart", "session-start", 0},
	{"Stop", "stop", 0},
	{"SessionEnd", "session-end", 3}, // Codex 기본 1초, 최대 3초
	{"UserPromptSubmit", "prompt", 0},
}

func isOursCodex(cmd string) bool {
	return strings.Contains(cmd, "atalk") && strings.Contains(cmd, " hook codex ")
}

// codexHookCommand 는 훅 명령. Codex 가 어떤 셸로 실행하는지(Windows 에서 cmd·PowerShell) 문서에 없어서,
// 경로에 공백이 없으면 따옴표 없이 쓴다 (PowerShell 은 따옴표로 시작하는 명령을 실행하지 않음).
func codexHookCommand(arg string) string {
	exe := Exe()
	if strings.ContainsAny(exe, " \t") {
		exe = `"` + exe + `"`
	}
	return exe + " hook codex " + arg
}

func editCodexHooks(p Paths, fn func(hooks map[string]any)) error {
	path := p.codexHooksFile()
	s := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("%s 를 읽을 수 없음 (JSON 오류): %w", path, err)
			}
		}
		if err := os.WriteFile(path+".atalk-bak", raw, 0o644); err != nil {
			return err
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return err
	}
	hooks, _ := s["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	fn(hooks)
	if len(hooks) == 0 {
		delete(s, "hooks")
	} else {
		s["hooks"] = hooks
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, string(b)+"\n")
}

// removeOursCodex 는 훅 목록에서 atalk 항목을 뺀다.
func removeOursCodex(v any) []any {
	list, _ := v.([]any)
	out := []any{}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		inner, _ := m["hooks"].([]any)
		kept := []any{}
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			cmd, _ := hm["command"].(string)
			if !isOursCodex(cmd) {
				kept = append(kept, h)
			}
		}
		if len(kept) == 0 && len(inner) > 0 {
			continue
		}
		m["hooks"] = kept
		out = append(out, m)
	}
	return out
}

// Codex 는 Codex 용 스킬과 훅을 설치한다. 기존 hooks.json 은 hooks.json.atalk-bak 으로 백업한다.
func Codex(p Paths) ([]string, error) {
	var done []string
	if err := writeFile(p.codexSkillFile(), render(skillTmpl, "atalk")); err != nil {
		return done, err
	}
	done = append(done, "스킬: "+p.codexSkillFile())
	err := editCodexHooks(p, func(hooks map[string]any) {
		for _, h := range codexHookEvents {
			hooks[h.event] = removeOursCodex(hooks[h.event])
		}
		for _, h := range codexHookEvents {
			entry := map[string]any{"type": "command", "command": codexHookCommand(h.arg)}
			if h.timeout > 0 {
				entry["timeout"] = h.timeout
			}
			list, _ := hooks[h.event].([]any)
			hooks[h.event] = append(list, map[string]any{"hooks": []any{entry}})
		}
	})
	if err != nil {
		return done, err
	}
	done = append(done, "훅(SessionStart/Stop/SessionEnd/UserPromptSubmit): "+p.codexHooksFile(),
		"Codex 는 새 훅을 신뢰해야 실행합니다: Codex 를 연 뒤 /hooks 에서 ATalk 훅을 신뢰(trust)하세요.")
	return done, nil
}

func uninstallCodex(p Paths) []string {
	var done []string
	if err := os.RemoveAll(filepath.Dir(p.codexSkillFile())); err == nil {
		done = append(done, "스킬 삭제: "+p.codexSkillFile())
	}
	if _, err := os.Stat(p.codexHooksFile()); err == nil {
		err := editCodexHooks(p, func(hooks map[string]any) {
			for _, h := range codexHookEvents {
				if l := removeOursCodex(hooks[h.event]); len(l) > 0 {
					hooks[h.event] = l
				} else {
					delete(hooks, h.event)
				}
			}
		})
		if err == nil {
			done = append(done, "훅 삭제: "+p.codexHooksFile())
		}
	}
	return done
}

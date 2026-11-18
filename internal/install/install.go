// Package install 은 에이전트 도구에 ATalk 연동(스킬, 훅, 플러그인)을 설치/제거한다.
//
//	Claude Code: ~/.claude/skills/atalk/SKILL.md + settings.json 의 SessionStart/Stop/SessionEnd 훅 + atalk 명령 허용
//	opencode   : ~/.config/opencode/plugin/atalk.ts (스킬은 opencode 가 ~/.claude/skills 를 같이 읽으므로 공용)
package install

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"atalk/internal/paths"
)

//go:embed skill.md
var skillTmpl string

//go:embed opencode-plugin.ts
var pluginTmpl string

// Exe 는 atalk 실행 파일의 절대 경로 (슬래시 구분 — bash 와 JSON 에서 그대로 쓰기 위해).
func Exe() string {
	p, err := os.Executable()
	if err != nil {
		return "atalk"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.ToSlash(p)
}

type Paths struct {
	Claude   string // ~/.claude
	Opencode string // ~/.config/opencode
}

func DefaultPaths() Paths {
	home, _ := os.UserHomeDir()
	p := Paths{Claude: filepath.Join(home, ".claude"), Opencode: filepath.Join(home, ".config", "opencode")}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		p.Claude = v
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		p.Opencode = filepath.Join(v, "opencode")
	}
	return p
}

// render 는 템플릿의 {{ATALK}}(명령 이름)와 {{ATALK_FULL}}(전체 경로)을 채운다.
// 스킬은 PATH 에 등록된 `atalk` 를 쓰고, 플러그인은 PATH 와 무관하게 전체 경로를 쓴다.
func render(t, cmd string) string {
	return strings.NewReplacer("{{ATALK_FULL}}", Exe(), "{{ATALK_CONFIG}}", filepath.ToSlash(paths.ConfigDir()), "{{ATALK}}", cmd).Replace(t)
}

// Status 는 연동 설치 상태. *Current 는 지금 실행 파일 기준으로 다시 만든 내용과 같은지 (다르면 다시 설치 필요).
type Status struct {
	Exe                   string `json:"exe"`
	PathDir               string `json:"path_dir"`
	InPath                bool   `json:"in_path"`
	ClaudeSkill           bool   `json:"claude_skill"`
	ClaudeSkillCurrent    bool   `json:"claude_skill_current"`
	ClaudeHooks           bool   `json:"claude_hooks"`
	OpencodePlugin        bool   `json:"opencode_plugin"`
	OpencodePluginCurrent bool   `json:"opencode_plugin_current"`
	SkillFile             string `json:"skill_file"`
	SettingsFile          string `json:"settings_file"`
	PluginFile            string `json:"plugin_file"`
}

func Check(p Paths) Status {
	st := Status{Exe: Exe(), PathDir: PathDir(), InPath: InUserPath(PathDir()),
		SkillFile: p.skillFile(), SettingsFile: p.settingsFile(), PluginFile: p.pluginFile()}
	if b, err := os.ReadFile(p.skillFile()); err == nil {
		st.ClaudeSkill = true
		st.ClaudeSkillCurrent = string(b) == render(skillTmpl, "atalk")
	}
	if b, err := os.ReadFile(p.pluginFile()); err == nil {
		st.OpencodePlugin = true
		st.OpencodePluginCurrent = string(b) == render(pluginTmpl, Exe())
	}
	if b, err := os.ReadFile(p.settingsFile()); err == nil {
		var s map[string]any
		if json.Unmarshal(b, &s) == nil {
			hooks, _ := s["hooks"].(map[string]any)
			all := hooks != nil
			for _, h := range hookEvents {
				want := fmt.Sprintf(`"%s" hook claude-code %s`, Exe(), h.arg)
				found := false
				list, _ := hooks[h.event].([]any)
				for _, item := range list {
					m, _ := item.(map[string]any)
					inner, _ := m["hooks"].([]any)
					for _, x := range inner {
						if hm, _ := x.(map[string]any); hm["command"] == want {
							found = true
						}
					}
				}
				all = all && found
			}
			st.ClaudeHooks = all
		}
	}
	return st
}

func (p Paths) skillFile() string    { return filepath.Join(p.Claude, "skills", "atalk", "SKILL.md") }
func (p Paths) settingsFile() string { return filepath.Join(p.Claude, "settings.json") }
func (p Paths) pluginFile() string   { return filepath.Join(p.Opencode, "plugin", "atalk.ts") }

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// Skill 은 공용 스킬을 설치한다 (Claude Code 와 opencode 가 함께 읽음).
func Skill(p Paths) (string, error) {
	return p.skillFile(), writeFile(p.skillFile(), render(skillTmpl, "atalk"))
}

var hookEvents = []struct{ event, arg string }{
	{"SessionStart", "session-start"},
	{"Stop", "stop"},
	{"SessionEnd", "session-end"},
	{"UserPromptSubmit", "prompt"},
}

// isOurs 는 settings.json 의 훅 명령이 atalk 가 넣은 것인지 본다.
func isOurs(cmd string) bool {
	return strings.Contains(cmd, "atalk") && strings.Contains(cmd, " hook claude-code ")
}

// allowRules 는 settings.json 에 넣는 명령 허용 규칙 (PATH 의 atalk 와 전체 경로 둘 다).
func (p Paths) allowRules() []string { return []string{"Bash(atalk *)", "Bash(" + Exe() + " *)"} }

func isAllowRule(p Paths, v any) bool {
	for _, r := range p.allowRules() {
		if v == r {
			return true
		}
	}
	return false
}

// Claude 는 스킬 + 훅 + 명령 허용을 설치한다. 기존 settings.json 은 settings.json.atalk-bak 으로 백업한다.
func Claude(p Paths) ([]string, error) {
	var done []string
	f, err := Skill(p)
	if err != nil {
		return done, err
	}
	done = append(done, "스킬: "+f)
	err = editSettings(p, func(s map[string]any) {
		hooks, _ := s["hooks"].(map[string]any)
		if hooks == nil {
			hooks = map[string]any{}
		}
		for _, h := range hookEvents {
			list := removeOurs(hooks[h.event])
			list = append(list, map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": fmt.Sprintf(`"%s" hook claude-code %s`, Exe(), h.arg),
			}}})
			hooks[h.event] = list
		}
		s["hooks"] = hooks
		perms, _ := s["permissions"].(map[string]any)
		if perms == nil {
			perms = map[string]any{}
		}
		allow, _ := perms["allow"].([]any)
		for _, rule := range p.allowRules() {
			found := false
			for _, a := range allow {
				if a == rule {
					found = true
				}
			}
			if !found {
				allow = append(allow, rule)
			}
		}
		perms["allow"] = allow
		s["permissions"] = perms
	})
	if err != nil {
		return done, err
	}
	done = append(done, "훅(SessionStart/Stop/SessionEnd/UserPromptSubmit) + 허용 규칙: "+p.settingsFile())
	return done, nil
}

// removeOurs 는 훅 목록에서 atalk 항목을 뺀다 (다시 설치해도 중복되지 않게).
func removeOurs(v any) []any {
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
			if !isOurs(cmd) {
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

func editSettings(p Paths, fn func(map[string]any)) error {
	path := p.settingsFile()
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
	fn(s)
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, string(b)+"\n")
}

// Opencode 는 플러그인을 설치하고, 공용 스킬이 없으면 함께 설치한다.
func Opencode(p Paths) ([]string, error) {
	var done []string
	if err := writeFile(p.pluginFile(), render(pluginTmpl, Exe())); err != nil {
		return done, err
	}
	done = append(done, "플러그인: "+p.pluginFile())
	if _, err := os.Stat(p.skillFile()); err != nil {
		f, err := Skill(p)
		if err != nil {
			return done, err
		}
		done = append(done, "스킬: "+f+" (opencode 는 ~/.claude/skills 도 읽음)")
	}
	return done, nil
}

// Path 는 atalk 실행 파일 폴더를 사용자 PATH 에 등록한다.
func Path() ([]string, error) {
	dir := PathDir()
	added, err := AddToUserPath(dir)
	if err != nil {
		return nil, err
	}
	if !added {
		return []string{"PATH: 이미 등록됨 (" + dir + ")"}, nil
	}
	return []string{"PATH: " + dir + " 추가 (새로 여는 터미널부터 적용)"}, nil
}

// Uninstall 은 target("claude" | "opencode" | "path" | "all") 의 ATalk 연동을 지운다.
func Uninstall(p Paths, target string) ([]string, error) {
	var done []string
	if target == "claude" || target == "all" {
		if err := os.RemoveAll(filepath.Dir(p.skillFile())); err == nil {
			done = append(done, "스킬 삭제: "+p.skillFile())
		}
		if _, err := os.Stat(p.settingsFile()); err == nil {
			err := editSettings(p, func(s map[string]any) {
				if hooks, ok := s["hooks"].(map[string]any); ok {
					for _, h := range hookEvents {
						if l := removeOurs(hooks[h.event]); len(l) > 0 {
							hooks[h.event] = l
						} else {
							delete(hooks, h.event)
						}
					}
					if len(hooks) == 0 {
						delete(s, "hooks")
					}
				}
				if perms, ok := s["permissions"].(map[string]any); ok {
					allow, _ := perms["allow"].([]any)
					kept := []any{}
					for _, a := range allow {
						if !isAllowRule(p, a) {
							kept = append(kept, a)
						}
					}
					perms["allow"] = kept
				}
			})
			if err != nil {
				return done, err
			}
			done = append(done, "훅/허용 규칙 삭제: "+p.settingsFile())
		}
	}
	if target == "path" || target == "all" {
		if removed, err := RemoveFromUserPath(PathDir()); err == nil && removed {
			done = append(done, "PATH 에서 삭제: "+PathDir())
		}
	}
	if target == "opencode" || target == "all" {
		if err := os.Remove(p.pluginFile()); err == nil {
			done = append(done, "플러그인 삭제: "+p.pluginFile())
		}
	}
	return done, nil
}

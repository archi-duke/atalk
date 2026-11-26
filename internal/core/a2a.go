package core

// A2A(Agent2Agent) 호환 네임카드. ATalk 카드를 A2A 1.0 의 AgentCard 모양으로 내보낸다.
// 필수 필드(name·description·supportedInterfaces·version·capabilities·defaultInputModes·defaultOutputModes·skills)를 모두 채우고,
// A2A 에 없는 ATalk 정보(주의사항·종류·PC·작업 이름·상태·방)는 capabilities.extensions 의 ATalk 확장 params 에 넣는다.
// 메시지를 주고받는 통로는 A2A JSON-RPC 가 아니라 ATalk 데몬 API 이므로 interface 의 protocolBinding 은 "ATALK+HTTP" 다
// (A2A 1.0 은 바인딩 이름을 열어 둠). A2A 클라이언트는 이 카드로 에이전트를 찾고 고를 수 있다.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	A2AProtocolVersion = "1.0"
	A2ABinding         = "ATALK+HTTP"
	A2AExtensionURI    = "https://archi-duke.github.io/atalk-manual/#a2a-card"
	ManualURL          = "https://archi-duke.github.io/atalk-manual/"
)

type A2AInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

type A2AProvider struct {
	Organization string `json:"organization"`
	URL          string `json:"url"`
}

type A2AExtension struct {
	URI         string         `json:"uri"`
	Description string         `json:"description,omitempty"`
	Required    bool           `json:"required"`
	Params      map[string]any `json:"params,omitempty"`
}

type A2ACapabilities struct {
	Streaming         bool           `json:"streaming"`
	PushNotifications bool           `json:"pushNotifications"`
	Extensions        []A2AExtension `json:"extensions,omitempty"`
}

type A2ASkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
}

// A2ACard 는 A2A 1.0 AgentCard.
type A2ACard struct {
	Name                string          `json:"name"`
	Description         string          `json:"description"`
	SupportedInterfaces []A2AInterface  `json:"supportedInterfaces"`
	Provider            *A2AProvider    `json:"provider,omitempty"`
	Version             string          `json:"version"`
	DocumentationURL    string          `json:"documentationUrl,omitempty"`
	Capabilities        A2ACapabilities `json:"capabilities"`
	DefaultInputModes   []string        `json:"defaultInputModes"`
	DefaultOutputModes  []string        `json:"defaultOutputModes"`
	Skills              []A2ASkill      `json:"skills"`
	IconURL             string          `json:"iconUrl,omitempty"`
}

// ToA2A 는 카드를 A2A AgentCard 로 바꾼다. base 는 데몬 주소 (예: http://127.0.0.1:7420).
// 할 일(skills)을 아직 적지 않은 카드는 능력(capabilities) 태그로 대신 채운다 (A2A 는 skills 가 필수).
func ToA2A(v AgentView, base string) A2ACard {
	base = strings.TrimRight(base, "/")
	desc := strings.TrimSpace(v.Description)
	if desc == "" {
		desc = v.Name + " (" + v.Kind + " 세션)"
	}
	skills := make([]A2ASkill, 0, len(v.Skills))
	for _, s := range v.Skills {
		tags := s.Tags
		if tags == nil {
			tags = []string{}
		}
		skills = append(skills, A2ASkill{ID: s.ID, Name: s.Name, Description: s.Description, Tags: tags, Examples: s.Examples})
	}
	if len(skills) == 0 {
		for i, c := range v.Capabilities {
			id := slugID(c)
			if id == "" {
				id = fmt.Sprintf("skill-%d", i+1)
			}
			skills = append(skills, A2ASkill{ID: id, Name: c, Description: c, Tags: []string{c}})
		}
	}
	params := map[string]any{"agent_id": v.ID, "kind": v.Kind, "status": v.Status}
	if len(v.Cautions) > 0 {
		params["cautions"] = v.Cautions
	}
	if v.Host != nil {
		params["host"] = v.Host.Name
	}
	if v.Workspace != nil && *v.Workspace != "" {
		params["workspace"] = *v.Workspace
	}
	if v.Room != nil {
		params["room"] = *v.Room
	}
	if v.Human != nil {
		params["human"] = *v.Human
	}
	if v.Headless {
		params["headless"] = true
	}
	version := v.UpdatedAt
	if version == "" {
		version = v.RegisteredAt
	}
	card := A2ACard{
		Name:        v.Name,
		Description: desc,
		SupportedInterfaces: []A2AInterface{{
			URL: base + "/a2a/" + v.ID, ProtocolBinding: A2ABinding, ProtocolVersion: A2AProtocolVersion,
		}},
		Provider:         &A2AProvider{Organization: "ATalk", URL: base},
		Version:          version,
		DocumentationURL: ManualURL,
		Capabilities: A2ACapabilities{Extensions: []A2AExtension{{
			URI:         A2AExtensionURI,
			Description: "ATalk 확장: 주의사항(cautions)·에이전트 종류·PC·작업 이름·상태·대화방",
			Params:      params,
		}}},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain", "text/markdown"},
		Skills:             skills,
	}
	if strings.HasPrefix(v.Avatar, "data:image/") {
		card.IconURL = v.Avatar
	}
	return card
}

var skillIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// slugID: 자유 글을 skill id 로 (영문 소문자·숫자·._- 만 남김, 남는 게 없으면 "").
func slugID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '/':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-._")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// checkSkills: 할 일 목록을 다듬고 검사한다 (A2A 필수 필드: id·name·description·tags).
func checkSkills(in []Skill) ([]Skill, error) {
	if len(in) > 30 {
		return nil, errors.New("할 수 있는 일은 30개까지")
	}
	out := make([]Skill, 0, len(in))
	seen := map[string]bool{}
	for i, s := range in {
		s.Name = strings.TrimSpace(s.Name)
		s.Description = strings.TrimSpace(s.Description)
		if s.Name == "" || s.Description == "" {
			return nil, &Error{Status: 400, Code: "bad_skill", Message: fmt.Sprintf("%d번째 일: name·description 이 필요함", i+1)}
		}
		s.ID = strings.TrimSpace(s.ID)
		if s.ID == "" {
			s.ID = slugID(s.Name)
		}
		if s.ID == "" {
			s.ID = fmt.Sprintf("skill-%d", i+1)
		}
		if !skillIDRe.MatchString(s.ID) {
			return nil, &Error{Status: 400, Code: "bad_skill", Message: fmt.Sprintf("일 id %q: 영문 소문자·숫자·._- 로 (64자까지)", s.ID)}
		}
		if seen[s.ID] {
			return nil, &Error{Status: 400, Code: "bad_skill", Message: "일 id 가 겹침: " + s.ID}
		}
		seen[s.ID] = true
		s.Name = clip(s.Name, 100)
		s.Description = clip(s.Description, 1000)
		s.Tags = cleanList(s.Tags, 15, 40)
		if s.Tags == nil {
			s.Tags = []string{}
		}
		s.Examples = cleanList(s.Examples, 10, 300)
		out = append(out, s)
	}
	return out, nil
}

// cleanList: 빈 항목을 빼고 개수·길이를 자른다.
func cleanList(in []string, maxN, maxLen int) []string {
	var out []string
	for _, x := range in {
		if x = strings.TrimSpace(x); x != "" && len(out) < maxN {
			out = append(out, clip(x, maxLen))
		}
	}
	return out
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

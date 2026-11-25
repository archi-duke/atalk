package server

import (
	"fmt"
	"os"
	"strings"

	"atalk/internal/core"
	"atalk/internal/procs"
)

// SpawnInput 은 POST /spawn 본문: 에이전트(by)가 협업할 새 세션을 이 PC 의 새 터미널 창에서 연다.
type SpawnInput struct {
	By     string `json:"by"`               // 띄우는 에이전트
	Kind   string `json:"kind"`             // claude-code | codex | opencode
	Cwd    string `json:"cwd,omitempty"`    // 작업 폴더 (없으면 띄우는 에이전트의 폴더)
	Prompt string `json:"prompt"`           // 첫 지시
	Name   string `json:"name,omitempty"`   // 새 세션 이름 (없으면 기본)
	Room   string `json:"room,omitempty"`   // 들어갈 방 (없으면 띄우는 에이전트의 방, "-" 이면 방에 넣지 않음)
}

// spawnIntro 는 새 세션의 첫 지시 앞에 붙는 안내 — 누가 띄웠고, 결과를 어디로 보고할지.
func spawnIntro(byName, by, room string) string {
	where, greet := "", ""
	if room != "" {
		where = fmt.Sprintf(" ATalk 대화방 %s 에 들어가 있습니다.", room)
		// 방에 처음 들어오면 일보다 인사부터 (사용자 요청)
		greet = " 작업을 시작하기 전에 먼저 `atalk send` 로 방에 인사하면서 자기소개(이름, 맡은 일)를 한두 줄 올리세요."
	}
	return fmt.Sprintf("[ATalk 협업] %s 가 이 세션을 띄웠습니다.%s%s 아래 작업을 마치면 결과를 `atalk send - --type result --to %s` 로 보고하세요.\n\n", byName, where, greet, by)
}

func (s *Server) spawn(in SpawnInput) (any, error) {
	c := s.Core
	by, err := c.GetAgent(in.By)
	if err != nil {
		return nil, err
	}
	if !by.Local {
		return nil, &core.Error{Status: 409, Code: "not_here", Message: "이 PC 의 에이전트만 새 세션을 띄울 수 있음 (다른 PC 는 그 PC 에서)"}
	}
	switch in.Kind {
	case "claude", "":
		in.Kind = "claude-code"
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, &core.Error{Status: 400, Code: "missing_prompt", Message: "첫 지시(prompt)가 필요함"}
	}
	cwd := in.Cwd
	if cwd == "" && by.Cwd != nil {
		cwd = *by.Cwd
	}
	if st, err := os.Stat(cwd); cwd == "" || err != nil || !st.IsDir() {
		return nil, &core.Error{Status: 409, Code: "no_cwd", Message: "작업 폴더가 없음: " + cwd}
	}
	room := in.Room
	if room == "" && by.Room != nil {
		room = *by.Room
	}
	if room == "-" {
		room = ""
	}
	if room != "" {
		if _, err := c.GetRoom(room); err != nil {
			return nil, err
		}
	}
	prompt := spawnIntro(by.Name, by.ID, room) + in.Prompt
	argv, err := procs.SpawnCommand(in.Kind, prompt)
	if err != nil {
		return nil, &core.Error{Status: 409, Code: "spawn_unsupported", Message: err.Error()}
	}
	token, err := c.NewSpawn(by.ID, room, strings.TrimSpace(in.Name), prompt) // opencode 는 플러그인이 이 지시를 세션에 직접 보낸다
	if err != nil {
		return nil, err
	}
	if err := procs.LaunchTerminalEnv(cwd, argv, map[string]string{"ATALK_SPAWN": token}); err != nil {
		return nil, &core.Error{Status: 500, Code: "launch_failed", Message: "터미널을 열 수 없음: " + err.Error()}
	}
	return map[string]any{"launched": true, "kind": in.Kind, "cwd": cwd, "room": room}, nil
}

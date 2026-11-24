package procs

// 깨우기: 꺼진 에이전트 세션을 그 작업 폴더에서 새 터미널 창으로 다시 띄운다 (이전 세션 이어서 열기).

import (
	"errors"
	"regexp"
)

var safeArg = regexp.MustCompile(`^[A-Za-z0-9._:/@+=-]+$`)

// ResumeCommand 는 에이전트 종류별로 이전 세션을 이어서 여는 명령. 지원하지 않으면 nil.
func ResumeCommand(kind, sessionID string) ([]string, error) {
	if !safeArg.MatchString(sessionID) {
		return nil, errors.New("세션 id 형식이 올바르지 않음")
	}
	switch kind {
	case "claude-code":
		return []string{"claude", "--resume", sessionID}, nil
	case "opencode":
		return []string{"opencode", "-s", sessionID}, nil
	}
	return nil, errors.New(kind + " 는 아직 깨우기를 지원하지 않음 (Claude Code·opencode 만)")
}

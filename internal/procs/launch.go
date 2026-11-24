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
	case "codex":
		// Codex 는 다시 연 세션에 첫 입력이 있어야 SessionStart 훅이 돈다 (등록 → 그 뒤 Stop 훅이 쌓인 메시지를 전달).
		// --no-daemon: 공유 백그라운드 서버(app-server)가 아니라 이 창에서 직접 실행해야 훅이 찾는 PID 가 이 창이 된다 (Ctrl+클릭·생존 판단)
		return []string{"codex", "resume", "--no-daemon", sessionID, CodexWakePrompt}, nil
	}
	return nil, errors.New(kind + " 는 아직 깨우기를 지원하지 않음 (Claude Code·opencode·Codex 만)")
}

// CodexWakePrompt 는 깨우기로 다시 연 Codex 세션의 첫 입력. "[ATalk" 로 시작해 CLI 입력 기록에서 빠진다.
const CodexWakePrompt = "[ATalk] 이 세션을 다시 열었습니다. 대화방 알림이 오면 처리하고, 없으면 대기 중 이라고만 답하세요."

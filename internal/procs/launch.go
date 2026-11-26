package procs

// 깨우기: 꺼진 에이전트 세션을 그 작업 폴더에서 새 터미널 창으로 다시 띄운다 (이전 세션 이어서 열기).

import (
	"errors"
	"os"
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

// SpawnCommand 는 새 세션을 첫 지시와 함께 여는 명령 (atalk spawn). 지시는 따옴표로 감싸 그대로 전달된다.
func SpawnCommand(kind, prompt string) ([]string, error) {
	switch kind {
	case "claude-code":
		return []string{"claude", prompt}, nil
	case "opencode":
		// --prompt 는 켜지는 순서에 따라 입력·전송되지 않을 때가 있어, 지시 없이 열고 ATalk 플러그인이 세션에 직접 보낸다
		return []string{"opencode"}, nil
	case "codex":
		return []string{"codex", "--no-daemon", prompt}, nil // 이 창에서 직접 (훅이 찾는 PID 가 이 창이 되게)
	}
	return nil, errors.New(kind + " 는 띄우기를 지원하지 않음 (claude-code·opencode·codex 만)")
}

// CodexWakePrompt 는 깨우기로 다시 연 Codex 세션의 첫 입력. "[ATalk" 로 시작해 CLI 입력 기록에서 빠진다.
const CodexWakePrompt = "[ATalk] 이 세션을 다시 열었습니다. 대화방 알림이 오면 처리하고, 없으면 대기 중 이라고만 답하세요."

// SafeSession 은 세션 id 를 명령줄에 그대로 넣어도 되는지.
func SafeSession(id string) bool { return safeArg.MatchString(id) }

// sessionEnv 는 에이전트(Claude Code) 세션 안에서만 의미 있는 환경 변수. 데몬이 Claude 세션 안에서 시작되면 이것들을 물려받아,
// 데몬이 연 새 창의 Claude 가 자신을 "하위 세션" 으로 알고 대화 기록 저장을 끄는 등 다르게 동작한다 (기록이 없으면 CLI 기록·되살리기가 안 됨).
var sessionEnv = []string{
	"CLAUDECODE", "CLAUDE_PID", "CLAUDE_EFFORT",
	"CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXECPATH",
	"CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_BRIDGE_SESSION_ID",
	"CLAUDE_CODE_CHILD_NOTES_DIR", "CLAUDE_CODE_SESSION_ATTENDED",
	// ATalk 가 한 턴·한 창에만 주는 표시: 데몬이 그 안에서 다시 시작되면 물려받아, 데몬이 여는 창마다 헤드리스 턴이나 띄운 창으로 잘못 알았다
	"ATALK_HEADLESS", "ATALK_SPAWN", "ATALK_AGENT",
}

// ScrubSessionEnv 는 데몬이 시작할 때 물려받은 에이전트 세션 환경 변수를 지운다 (데몬이 여는 창·명령에 넘어가지 않게).
func ScrubSessionEnv() {
	for _, k := range sessionEnv {
		_ = os.Unsetenv(k)
	}
}

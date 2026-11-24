package procs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CodexCommand 는 codex 를 인자 그대로 실행하는 명령을 만든다.
// Windows 의 npm 설치본은 codex.cmd(배치 파일)라서, 메시지 같은 임의 글을 인자로 넘기면 cmd.exe 가 특수 문자를 해석한다.
// 그래서 배치 파일이면 그 안에서 실행하는 node + codex.js 를 직접 실행한다. ATALK_CODEX_BIN 으로 바꿀 수 있다.
func CodexCommand(args ...string) (*exec.Cmd, error) {
	bin := os.Getenv("ATALK_CODEX_BIN")
	if bin == "" {
		p, err := exec.LookPath("codex")
		if err != nil {
			return nil, err
		}
		bin = p
	}
	ext := strings.ToLower(filepath.Ext(bin))
	if ext != ".cmd" && ext != ".bat" {
		return exec.Command(bin, args...), nil
	}
	dir := filepath.Dir(bin)
	js := filepath.Join(dir, "node_modules", "@openai", "codex", "bin", "codex.js")
	if _, err := os.Stat(js); err != nil {
		return nil, err
	}
	node := filepath.Join(dir, "node.exe")
	if _, err := os.Stat(node); err != nil {
		if node, err = exec.LookPath("node"); err != nil {
			return nil, err
		}
	}
	return exec.Command(node, append([]string{js}, args...)...), nil
}

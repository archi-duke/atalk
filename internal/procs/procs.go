// Package procs 는 훅 프로세스에서 "에이전트 본체" 프로세스 PID 를 찾는다 (비정상 종료 감지용).
// 훅은 에이전트 → 셸 → atalk(훅) 형태로 실행되므로, 부모를 거슬러 올라가며 셸/래퍼를 건너뛴다.
package procs

import (
	"os"
	"regexp"
	"strconv"
)

type Proc struct {
	PID  int
	PPID int
	Name string
}

var wrappers = regexp.MustCompile(`(?i)^(bash|sh|zsh|dash|fish|cmd|powershell|pwsh|conhost|env|timeout|npx|npm|winpty|wsl|git-bash|mintty)(\.exe)?$`)

// Ancestors 는 pid 의 부모 체인 (가까운 것부터, 최대 20단계). 누가 이 에이전트를 띄웠는지 찾는 데 쓴다.
func Ancestors(pid int) []int {
	procs, err := list()
	if err != nil {
		return nil
	}
	var out []int
	p, ok := procs[pid]
	for i := 0; ok && i < 20 && p.PPID > 1; i++ {
		out = append(out, p.PPID)
		p, ok = procs[p.PPID]
	}
	return out
}

// FindAgentPID 는 ATALK_AGENT_PID 가 있으면 그것을, 없으면 부모 체인에서 셸/래퍼가 아닌 첫 프로세스를 돌려준다.
// node 는 래퍼로 보지 않는다: npm 으로 설치한 Claude Code 는 node.exe 자체가 에이전트 본체다.
// 찾지 못하면 0.
func FindAgentPID() int {
	if v, err := strconv.Atoi(os.Getenv("ATALK_AGENT_PID")); err == nil && v > 0 {
		return v
	}
	procs, err := list()
	if err != nil {
		return 0
	}
	pid := os.Getppid()
	for i := 0; i < 12 && pid > 1; i++ {
		p, ok := procs[pid]
		if !ok {
			return 0
		}
		if !wrappers.MatchString(p.Name) {
			return p.PID
		}
		pid = p.PPID
	}
	return 0
}

// Name 은 실행 중인 프로세스의 이름 (예: claude.exe, node). 없으면 "".
func Name(pid int) string {
	procs, err := list()
	if err != nil {
		return ""
	}
	return procs[pid].Name
}

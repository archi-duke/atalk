//go:build windows

package procs

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var (
	pEnumChildWindows = user32.NewProc("EnumChildWindows")

	// syscall.NewCallback 은 프로세스당 만들 수 있는 개수가 정해져 있어, 몇 초마다 부르는 곳에서는 한 번만 만든다.
	promptMu    sync.Mutex
	promptTops  []uintptr
	promptTexts []string
	promptTopCB = syscall.NewCallback(func(h, _ uintptr) uintptr {
		if visible(h) && className(h) == "#32770" {
			promptTops = append(promptTops, h)
		}
		return 1
	})
	promptChildCB = syscall.NewCallback(func(h, _ uintptr) uintptr {
		if t := strings.TrimSpace(windowTitle(h)); t != "" {
			promptTexts = append(promptTexts, t)
		}
		return 1
	})
)

// 방화벽 허용 창 제목 (한국어·영어 Windows)
var firewallTitles = []string{"Windows 보안 경고", "Windows Security Alert"}

var exeRe = regexp.MustCompile(`(?i)[a-z]:\\[^\r\n"]*?\.exe`)

func isFirewallTitle(t string) bool {
	for _, w := range firewallTitles {
		if strings.EqualFold(strings.TrimSpace(t), w) {
			return true
		}
	}
	return false
}

// SystemPrompts 는 지금 화면에 떠 있는 방화벽 허용 창과 관리자 권한 확인(UAC) 을 찾는다.
func SystemPrompts() []Prompt {
	promptMu.Lock()
	defer promptMu.Unlock()
	promptTops = promptTops[:0]
	pEnumWindows.Call(promptTopCB, 0)
	var out []Prompt
	var all map[int]Proc
	for _, h := range promptTops {
		title := windowTitle(h)
		if !isFirewallTitle(title) {
			continue
		}
		promptTexts = promptTexts[:0]
		pEnumChildWindows.Call(h, promptChildCB, 0)
		p := Prompt{ID: "fw-" + strconv.FormatUint(uint64(h), 16), Kind: "firewall", Title: title}
		// 창 안의 "경로: C:\...\x.exe" 에서 막힌 프로그램을 찾는다
		for _, t := range promptTexts {
			if m := exeRe.FindString(t); m != "" {
				p.Program = filepath.Base(m)
				if all == nil {
					all, _ = list()
				}
				p.Pid = runningPath(all, m)
				break
			}
		}
		out = append(out, p)
	}
	// UAC 창은 보안 데스크톱에 떠서 창으로는 안 보인다 → 확인 창을 그리는 consent.exe 가 떠 있는지로 본다
	if all == nil {
		all, _ = list()
	}
	for pid, pr := range all {
		if strings.EqualFold(pr.Name, "consent.exe") {
			out = append(out, Prompt{ID: "uac-" + strconv.Itoa(pid), Kind: "uac", Title: "사용자 계정 컨트롤"})
		}
	}
	return out
}

// runningPath 는 실행 파일 경로가 path 인 실행 중 프로세스 (없으면 0).
func runningPath(all map[int]Proc, path string) int {
	base := filepath.Base(path)
	for pid, pr := range all {
		if strings.EqualFold(pr.Name, base) && strings.EqualFold(exePath(pid), path) {
			return pid
		}
	}
	return 0
}

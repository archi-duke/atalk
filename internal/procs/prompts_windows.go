//go:build windows

package procs

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var (
	pEnumChildWindows   = user32.NewProc("EnumChildWindows")
	pOpenInputDesktop   = user32.NewProc("OpenInputDesktop")
	pCloseDesktop       = user32.NewProc("CloseDesktop")
	pGetUserObjectInfoW = user32.NewProc("GetUserObjectInformationW")

	// syscall.NewCallback 은 프로세스당 만들 수 있는 개수가 정해져 있어, 몇 초마다 부르는 곳에서는 한 번만 만든다.
	promptMu    sync.Mutex
	promptTops  []uintptr
	promptVis   = map[uint32]bool{} // 일반 화면에 보이는 창을 가진 프로세스
	promptTexts []string
	promptTopCB = syscall.NewCallback(func(h, _ uintptr) uintptr {
		if !visible(h) {
			return 1
		}
		var pid uint32
		pGetWindowThreadProcID.Call(h, uintptr(unsafe.Pointer(&pid)))
		promptVis[pid] = true
		if className(h) == "#32770" {
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
	clear(promptVis)
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
	// UAC 창은 보안 데스크톱에 떠서 창으로는 안 보인다 → 확인 창을 그리는 consent.exe 가 떠 있는지로 본다.
	// 단 consent.exe 가 창을 띄우다 멈춘 채 남는 일이 있어 (원격 접속으로 화면이 바뀔 때 등), 보안 데스크톱이 실제로
	// 떠 있거나 (기본 설정) consent.exe 창이 일반 화면에 보일 때만 (보안 데스크톱을 끈 설정) 알린다.
	if all == nil {
		all, _ = list()
	}
	desk := ""
	for pid, pr := range all {
		if !strings.EqualFold(pr.Name, "consent.exe") {
			continue
		}
		if desk == "" {
			desk = inputDesktop()
		}
		if uacShown(desk, promptVis[uint32(pid)]) {
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

// inputDesktop 은 지금 입력을 받는 데스크톱 이름 ("Default" = 일반 화면).
// 열 수 없으면 "" — UAC 보안 데스크톱(Winlogon)은 일반 프로그램이 열 수 없다.
func inputDesktop() string {
	h, _, _ := pOpenInputDesktop.Call(0, 0, 0x0001) // DESKTOP_READOBJECTS
	if h == 0 {
		return ""
	}
	defer pCloseDesktop.Call(h)
	buf := make([]uint16, 64)
	var need uint32
	if r, _, _ := pGetUserObjectInfoW.Call(h, 2 /* UOI_NAME */, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)*2), uintptr(unsafe.Pointer(&need))); r == 0 {
		return "Default"
	}
	return syscall.UTF16ToString(buf)
}

// uacShown: consent.exe 가 떠 있을 때 사람이 볼 수 있는 확인 창이 실제로 있는지.
// 입력 데스크톱이 일반 화면이 아니면 ("" = 열 수 없음 포함) 보안 데스크톱이 떠 있는 것,
// 일반 화면이면 consent.exe 창이 보여야 한다 (보안 데스크톱을 끈 설정). 둘 다 아니면 멈춰 남은 consent.exe.
func uacShown(desk string, consentWindow bool) bool {
	return !strings.EqualFold(desk, "Default") || consentWindow
}

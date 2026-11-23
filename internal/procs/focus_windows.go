//go:build windows

package procs

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                 = windows.NewLazySystemDLL("user32.dll")
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	pEnumWindows           = user32.NewProc("EnumWindows")
	pGetWindowThreadProcID = user32.NewProc("GetWindowThreadProcessId")
	pIsWindowVisible       = user32.NewProc("IsWindowVisible")
	pGetWindow             = user32.NewProc("GetWindow")
	pGetAncestor           = user32.NewProc("GetAncestor")
	pIsIconic              = user32.NewProc("IsIconic")
	pShowWindow            = user32.NewProc("ShowWindow")
	pSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	pBringWindowToTop      = user32.NewProc("BringWindowToTop")
	pKeybdEvent            = user32.NewProc("keybd_event")
	pGetWindowTextW        = user32.NewProc("GetWindowTextW")
	pAttachConsole         = kernel32.NewProc("AttachConsole")
	pFreeConsole           = kernel32.NewProc("FreeConsole")
	pGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	pGetConsoleTitleW      = kernel32.NewProc("GetConsoleTitleW")
	pGetClassNameW         = user32.NewProc("GetClassNameW")
)

const (
	gwOwner        = 4
	gaRootOwner    = 3
	swRestore      = 9
	vkMenu         = 0x12
	keyeventfKeyup = 0x2
)

func visible(h uintptr) bool { r, _, _ := pIsWindowVisible.Call(h); return r != 0 }

// consoleWindow 는 pid 의 콘솔 창을 찾는다 (AttachConsole → GetConsoleWindow). 이 프로세스의 콘솔을 잠깐 바꾸므로
// 데몬이 아니라 따로 띄운 도우미 프로세스(atalk focus-window)에서만 부른다.
// Windows Terminal(ConPTY) 이면 숨은 가짜 창이 나오는데, 그 루트 소유자가 터미널 창이다.
func consoleWindow(pid int) uintptr {
	h, _ := consoleInfo(pid)
	if h == 0 {
		return 0
	}
	if root, _, _ := pGetAncestor.Call(h, gaRootOwner); root != 0 && visible(root) {
		return root
	}
	if visible(h) {
		return h
	}
	return 0
}

// consoleInfo 는 pid 의 콘솔 창과 콘솔 제목(= Windows Terminal 탭 제목)을 돌려준다.
func consoleInfo(pid int) (uintptr, string) {
	pFreeConsole.Call()
	if r, _, _ := pAttachConsole.Call(uintptr(uint32(pid))); r == 0 {
		return 0, ""
	}
	defer pFreeConsole.Call()
	h, _, _ := pGetConsoleWindow.Call()
	buf := make([]uint16, 1024)
	n, _, _ := pGetConsoleTitleW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return h, windows.UTF16ToString(buf[:n])
}

func className(h uintptr) string {
	buf := make([]uint16, 256)
	pGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf)
}

// selectTerminalTab 은 Windows Terminal 창(h)에서 제목이 title 인 탭을 UI 자동화로 선택한다.
// (한 창에 탭으로 여러 에이전트가 있을 때 그 에이전트의 탭으로). 제목 앞 기호는 빼고 비교, 같은 제목이 없으면 앞부분이 같은 탭.
const selectTabScript = `
$ErrorActionPreference = "Stop"
Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes
$A = [System.Windows.Automation.AutomationElement]
$w = $A::FromHandle([IntPtr][long]$env:ATALK_HWND)
$cond = New-Object System.Windows.Automation.PropertyCondition($A::ControlTypeProperty, [System.Windows.Automation.ControlType]::TabItem)
$tabs = @($w.FindAll([System.Windows.Automation.TreeScope]::Descendants, $cond))
function Norm([string]$s) { return ($s -replace "^[^p{L}p{N}]+", "").Trim() }  # Claude 는 제목 앞 기호(✳ ◑ …)가 계속 바뀜
$want = Norm $env:ATALK_TAB_TITLE
$t = $tabs | Where-Object { (Norm $_.Current.Name) -eq $want } | Select-Object -First 1
if (-not $t) { $t = $tabs | Where-Object { $n = Norm $_.Current.Name; $n.StartsWith($want) -or $want.StartsWith($n) } | Select-Object -First 1 }
if (-not $t) { Write-Output "no tab"; exit 2 }
$t.GetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern).Select()
`

func selectTerminalTab(h uintptr, title string) error {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", selectTabScript)
	cmd.Env = append(os.Environ(), "ATALK_HWND="+strconv.FormatUint(uint64(h), 10), "ATALK_TAB_TITLE="+title)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("탭 선택 실패: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// topWindows 는 보이는 최상위 창(소유자 없음)을 프로세스별로 모은다.
func topWindows() map[int]uintptr {
	out := map[int]uintptr{}
	cb := syscall.NewCallback(func(h, _ uintptr) uintptr {
		if !visible(h) {
			return 1
		}
		if owner, _, _ := pGetWindow.Call(h, gwOwner); owner != 0 {
			return 1
		}
		var pid uint32
		pGetWindowThreadProcID.Call(h, uintptr(unsafe.Pointer(&pid)))
		if _, ok := out[int(pid)]; !ok {
			out[int(pid)] = h
		}
		return 1
	})
	pEnumWindows.Call(cb, 0)
	return out
}

// WindowTitle 은 창 제목.
func windowTitle(h uintptr) string {
	buf := make([]uint16, 256)
	pGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf)
}

// FindWindow 는 에이전트 프로세스(pid)의 터미널 창과 그 제목, 찾은 방법을 돌려준다 (확인용).
func FindWindow(pid int) (title, how string, err error) {
	h, how := findWindow(pid)
	if h == 0 {
		return "", "", fmt.Errorf("pid %d 의 창을 찾지 못함", pid)
	}
	if _, tab := consoleInfo(pid); tab != "" && className(h) == "CASCADIA_HOSTING_WINDOW_CLASS" {
		how += ", Windows Terminal 탭 [" + tab + "]"
	}
	return windowTitle(h), how, nil
}

func findWindow(pid int) (uintptr, string) {
	if h := consoleWindow(pid); h != 0 {
		return h, "console"
	}
	wins := topWindows()
	all, _ := list()
	for cur, i := pid, 0; cur > 4 && i < 16; i++ {
		if w, ok := wins[cur]; ok {
			return w, "ancestor " + all[cur].Name
		}
		p, ok := all[cur]
		if !ok {
			break
		}
		cur = p.PPID
	}
	return 0, ""
}

// FocusWindow 는 에이전트 프로세스(pid)가 돌고 있는 터미널 창을 맨 앞으로 올린다.
// 1) 콘솔 창(클래식 콘솔, Windows Terminal 의 가짜 창 → 터미널 창) 2) 부모를 거슬러 올라가며 보이는 창(Windows Terminal 등).
func FocusWindow(pid int) error {
	h, _ := findWindow(pid)
	if h == 0 {
		return fmt.Errorf("pid %d 의 창을 찾지 못함", pid)
	}
	// Windows Terminal 이면 그 에이전트의 탭을 먼저 고른다 (탭 제목 = 에이전트 콘솔 제목)
	if className(h) == "CASCADIA_HOSTING_WINDOW_CLASS" {
		if _, title := consoleInfo(pid); title != "" {
			_ = selectTerminalTab(h, title) // 실패해도 창은 올린다
		}
	}
	if r, _, _ := pIsIconic.Call(h); r != 0 {
		pShowWindow.Call(h, swRestore)
	}
	// 백그라운드 프로세스는 바로 앞으로 올릴 수 없어서, Alt 키를 한 번 눌렀다 떼어 포커스 제한을 푼다
	pKeybdEvent.Call(vkMenu, 0, 0, 0)
	pKeybdEvent.Call(vkMenu, 0, keyeventfKeyup, 0)
	pSetForegroundWindow.Call(h)
	pBringWindowToTop.Call(h)
	return nil
}

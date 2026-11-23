//go:build windows

package procs

import (
	"fmt"
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
	pFreeConsole.Call()
	if r, _, _ := pAttachConsole.Call(uintptr(uint32(pid))); r == 0 {
		return 0
	}
	h, _, _ := pGetConsoleWindow.Call()
	pFreeConsole.Call()
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

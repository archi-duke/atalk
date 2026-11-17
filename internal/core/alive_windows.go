//go:build windows

package core

import "golang.org/x/sys/windows"

const stillActive = 259

// DefaultIsAlive 는 프로세스가 아직 실행 중인지 확인한다.
// Windows 는 종료된 프로세스도 핸들이 남아 있으면 열리므로 exit code 로 판단한다.
func DefaultIsAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// 권한 거부는 "존재함" 으로 본다
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

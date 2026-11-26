//go:build windows

package procs

import "golang.org/x/sys/windows"

// CloseAgentWindow 는 에이전트를 종료하고 그 창(터미널 탭)도 닫는다: 에이전트를 실행한 셸까지 종료 코드 0 으로 끝내
// Windows Terminal 이 정상 종료로 보고 탭을 닫게 한다 (종료 코드 1 이면 "프로세스 종료됨" 으로 남는다).
func CloseAgentWindow(pid int) error {
	var first error
	for _, p := range windowChain(pid) {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(p))
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if err := windows.TerminateProcess(h, 0); err != nil && first == nil {
			first = err
		}
		windows.CloseHandle(h)
	}
	return first
}

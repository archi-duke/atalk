package procs

// windowChain 은 에이전트 pid 와, 그 에이전트를 실행한 셸·래퍼 부모들 (터미널 프로그램 앞에서 멈춤).
// 에이전트만 종료하면 그 창에 셸이 남으므로, 창을 닫으려면 셸까지 끝내야 한다.
func windowChain(pid int) []int {
	out := []int{pid}
	ps, err := list()
	if err != nil {
		return out
	}
	p, ok := ps[pid]
	for i := 0; ok && i < 8; i++ {
		q, ok2 := ps[p.PPID]
		if !ok2 || p.PPID <= 1 || !wrappers.MatchString(q.Name) {
			break
		}
		out = append(out, q.PID)
		p = q
	}
	return out
}

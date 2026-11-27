package procs

import "testing"

// 실제 화면에서 찾기: 몇 번 불러도 (콜백을 새로 만들지 않고) 문제없이 돈다. 지금 떠 있는 창은 기록만.
func TestSystemPromptsSmoke(t *testing.T) {
	for i := 0; i < 50; i++ {
		for _, p := range SystemPrompts() {
			if i == 0 {
				t.Logf("지금 떠 있는 확인 창: %+v — %s", p, p.Describe())
			}
		}
	}
}

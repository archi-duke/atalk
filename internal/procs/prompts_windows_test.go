//go:build windows

package procs

import "testing"

// UAC: consent.exe 가 있어도 보안 데스크톱이 떠 있거나 consent.exe 창이 보일 때만 알린다 (멈춰 남은 consent.exe 는 무시).
func TestUACShown(t *testing.T) {
	cases := []struct {
		desk   string
		window bool
		want   bool
	}{
		{"Winlogon", false, true}, // 보안 데스크톱에 확인 창
		{"", false, true},         // 입력 데스크톱을 열 수 없음 = 보안 데스크톱
		{"Default", true, true},   // 보안 데스크톱을 끈 설정: 일반 화면에 창
		{"Default", false, false}, // 멈춰 남은 consent.exe: 창 없음
		{"default", false, false},
	}
	for _, c := range cases {
		if got := uacShown(c.desk, c.window); got != c.want {
			t.Errorf("uacShown(%q, %v) = %v, want %v", c.desk, c.window, got, c.want)
		}
	}
	if d := inputDesktop(); d == "" {
		t.Log("입력 데스크톱을 열 수 없음 (보안 데스크톱이거나 대화형 세션이 아님)")
	} else {
		t.Logf("지금 입력 데스크톱: %s", d)
	}
}

package client

import (
	"strings"
	"testing"

	"atalk/internal/core"
)

func TestDigest(t *testing.T) {
	long := strings.Repeat("가", 130)
	msgs := []core.InboxMessage{
		{Message: core.Message{Seq: 1, Type: "system", Body: "입장"}, ForMe: true},
		{Message: core.Message{Seq: 2, Type: "chat", From: "a", Body: "줄\n바꿈"}, ForMe: true},
		{Message: core.Message{Seq: 3, Type: "chat", From: "b", Body: "참고용"}, ForMe: false},
		{Message: core.Message{Seq: 4, Type: "task", From: "a", Human: "사용자", Body: long}, ForMe: true},
	}
	got := strings.Split(Digest(msgs, map[string]string{"a": "마스터"}), "\n")
	if len(got) != 2 {
		t.Fatalf("나에게 온 것만, system 제외: %q", got)
	}
	if got[0] != "📨 #2 마스터: 줄 바꿈" {
		t.Fatalf("한 줄로: %q", got[0])
	}
	if !strings.HasPrefix(got[1], "📨 #4 사용자@마스터") || !strings.HasSuffix(got[1], "…") {
		t.Fatalf("사람 표시, 120자 자름: %q", got[1])
	}
}

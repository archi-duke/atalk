package core

import (
	"strings"
	"testing"
)

// replies 는 CLI 입력 seq 에 붙은 응답 본문들.
func replies(c *Atalk, room string, seq int) []string {
	var out []string
	for _, m := range ok(c.ReadMessages(room, seq, 1000)) {
		if m.Echo && m.Human == "" && m.ReplyTo != nil && *m.ReplyTo == seq {
			out = append(out, m.Body)
		}
	}
	return out
}

func echoSetup(t *testing.T) (*env, *Atalk, string) {
	e := newEnv(t)
	c := e.core()
	a, _, _ := setup(t, c)
	ok(c.CreateRoom(a, "r", "", 0))
	return e, c, a
}

// 앞선 턴이 진행 중일 때 다음 입력이 대기열에 들어와도 앞선 턴의 나머지·마지막 답이 버려지지 않는다.
func TestEchoQueueQueuedInput(t *testing.T) {
	_, c, a := echoSetup(t)
	p1 := ok(c.EchoPrompt(a, "라이선스 뭐로 하면 좋을까?")).Seq
	ok(c.EchoReply(a, []string{"검토해 보겠습니다"}, false, "라이선스 뭐로 하면 좋을까?"))
	p2 := ok(c.EchoPrompt(a, "Apache-2.0, 매뉴얼은 CC BY 4.0")).Seq // 턴 1 진행 중 대기열에 들어감
	ok(c.EchoReply(a, []string{"검토해 보겠습니다", "MIT 나 Apache-2.0 을 추천합니다"}, true, "라이선스 뭐로 하면 좋을까?"))
	ok(c.EchoReply(a, []string{"라이선스를 정리했습니다"}, true, "Apache-2.0, 매뉴얼은 CC BY 4.0"))

	if got := replies(c, "r", p1); strings.Join(got, "|") != "검토해 보겠습니다|MIT 나 Apache-2.0 을 추천합니다" {
		t.Fatalf("턴 1 응답: %v", got)
	}
	if got := replies(c, "r", p2); strings.Join(got, "|") != "라이선스를 정리했습니다" {
		t.Fatalf("턴 2 응답: %v", got)
	}
	if q := c.memberships[a].EchoQueue; len(q) != 0 {
		t.Fatalf("두 턴이 끝나면 큐가 비어야 함: %v", q)
	}
	// 다른 턴의 응답은 붙지 않음 (큐에 없는 입력)
	if ms := ok(c.EchoReply(a, []string{"엉뚱한 문장"}, true, "모르는 입력")); len(ms) != 0 {
		t.Fatal("기다리는 입력과 다른 턴의 응답은 기록하지 않음")
	}
}

// Esc 로 중단해 Stop 이 오지 않은 입력은 다음 입력의 턴이 끝날 때 함께 닫힌다. 남은 문장은 그 전에 남길 수 있다.
func TestEchoQueueInterruptedTurn(t *testing.T) {
	_, c, a := echoSetup(t)
	p1 := ok(c.EchoPrompt(a, "첫 질문")).Seq
	ok(c.EchoReply(a, []string{"중단 전 문장"}, false, "첫 질문")) // 다음 입력 훅이 앞 턴의 남은 문장을 보냄
	p2 := ok(c.EchoPrompt(a, "다음 질문")).Seq
	ok(c.EchoReply(a, []string{"답"}, true, "다음 질문"))
	if len(c.memberships[a].EchoQueue) != 0 {
		t.Fatal("중단된 앞 입력도 닫혀야 함")
	}
	if replies(c, "r", p1)[0] != "중단 전 문장" || replies(c, "r", p2)[0] != "답" {
		t.Fatal("응답")
	}
}

// opencode 는 prompt 를 보내지 않는다 → 가장 최근 입력. 예전 형식(EchoOpen 한 칸)도 읽는다.
func TestEchoQueueNoPromptAndLegacy(t *testing.T) {
	_, c, a := echoSetup(t)
	p1 := ok(c.EchoPrompt(a, "하나")).Seq
	c.memberships[a].EchoQueue, c.memberships[a].EchoOpen = nil, p1 // 예전 데몬이 저장한 형식
	ok(c.EchoReply(a, []string{"응답"}, true, ""))
	if got := replies(c, "r", p1); len(got) != 1 || c.memberships[a].EchoOpen != 0 || len(c.memberships[a].EchoQueue) != 0 {
		t.Fatalf("예전 형식: %v %+v", got, c.memberships[a])
	}
}

func TestMarkEvent(t *testing.T) {
	_, c, a := echoSetup(t)
	if !c.MarkEvent(a, "e1") || c.MarkEvent(a, "e1") || !c.MarkEvent(a, "e2") || !c.MarkEvent(a, "") || !c.MarkEvent(a, "") {
		t.Fatal("같은 eid 는 한 번만, 빈 eid 는 항상")
	}
}

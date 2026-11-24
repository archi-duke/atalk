package core

// CLI 입력·응답 기록의 짝 맞추기 (입력 큐), 진행 줄, 같은 기록을 두 번 받지 않기.
//
// Claude Code 는 앞선 턴이 진행 중일 때 사람이 입력하면 그 입력을 대기열에 넣고 입력 훅을 바로 실행한다.
// 그래서 응답을 기다리는 입력이 동시에 여럿일 수 있다 → 입력을 큐로 두고, 훅이 보내는 "이번 턴을 시작한 입력" 으로 짝을 찾는다.

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	echoQueueMax    = 10 // 응답을 기다리는 입력은 이만큼만 (넘으면 오래된 것부터 버림)
	progressMaxLine = 40 // 진행 줄 메시지 하나에 남기는 줄 수
	eventMemory     = 200
)

// normalizeEcho 는 예전 형식(EchoOpen 한 칸)을 큐로 옮긴다.
func (m *Membership) normalizeEcho() {
	if m.EchoOpen != 0 {
		if !slices.Contains(m.EchoQueue, m.EchoOpen) {
			m.EchoQueue = append(m.EchoQueue, m.EchoOpen)
		}
		m.EchoOpen = 0
	}
}

// echoTarget 은 응답을 붙일 CLI 입력의 seq 와 큐 안의 위치. 없으면 seq 0.
// prompt 가 있으면(Claude) 그 입력과 같은 말인 가장 오래된 입력, 없으면(opencode) 가장 최근 입력.
// 오래된 입력(12시간)과 방에 없는 seq 는 큐에서 뺀다.
func (c *Atalk) echoTarget(m *Membership, prompt string) (seq, idx int) {
	m.normalizeEcho()
	kept := m.EchoQueue[:0]
	var bodies []string
	if len(m.EchoQueue) > 0 {
		found := c.st.MessagesBySeq(m.Room, m.EchoQueue)
		for _, s := range m.EchoQueue {
			q, ok := found[s]
			if !ok || s > c.lastSeq[m.Room] {
				continue
			}
			if t, err := time.Parse(time.RFC3339Nano, q.TS); err == nil && c.now().Sub(t) > echoPendingMaxAge {
				continue
			}
			kept = append(kept, s)
			bodies = append(bodies, q.Body)
		}
	}
	if len(kept) != len(m.EchoQueue) {
		m.EchoQueue = kept
		c.saveMemberships()
	}
	if len(kept) == 0 {
		return 0, -1
	}
	if prompt == "" {
		return kept[len(kept)-1], len(kept) - 1
	}
	for i, b := range bodies {
		if samePrompt(b, prompt) {
			return kept[i], i
		}
	}
	return 0, -1
}

// EchoProgress 는 CLI 입력을 처리하며 쓴 도구를 진행 줄로 남긴다. 입력마다 메시지 하나에 줄을 덧붙여 갱신한다 (알림 없음).
// transcript 에 응답 문장이 빠지더라도 무슨 일을 했는지는 남는다. 도구 출력은 남기지 않는다.
func (c *Atalk) EchoProgress(agent, line, prompt string) (msg *Message, err error) {
	err = c.guard(func() error {
		line = strings.TrimSpace(strings.ReplaceAll(line, "\n", " "))
		if r := []rune(line); len(r) > 200 {
			line = string(r[:200]) + "…"
		}
		m, ok := c.memberships[agent]
		if !ok || line == "" || c.readConfig().EchoFinalOnly {
			return nil
		}
		seq, _ := c.echoTarget(m, prompt)
		if seq == 0 && prompt != "" {
			// 백그라운드 도구 훅이 Stop 보다 늦게 오면 그 입력은 이미 닫혀 있다 → 방금(10분 안) 닫힌 같은 입력에 붙인다
			seq = c.recentEchoInput(agent, m.Room, prompt, 10*time.Minute)
		}
		if seq == 0 {
			return nil
		}
		room, err := c.requireRoom(m.Room, true)
		if err != nil {
			return nil
		}
		for _, x := range c.st.Messages(m.Room, seq, 0) {
			if x.From == agent && x.Echo && x.Progress && x.ReplyTo != nil && *x.ReplyTo == seq {
				lines, skipped := strings.Split(x.Body, "\n"), 0
				if strings.HasPrefix(lines[0], "(앞 ") { // 이미 줄인 적이 있음
					fmt.Sscanf(lines[0], "(앞 %d개 생략)", &skipped)
					lines = lines[1:]
				}
				lines = append(lines, "· "+line)
				if n := len(lines) - progressMaxLine; n > 0 {
					skipped += n
					lines = lines[n:]
				}
				if skipped > 0 {
					lines = append([]string{fmt.Sprintf("(앞 %d개 생략)", skipped)}, lines...)
				}
				x.Body = strings.Join(lines, "\n")
				c.st.UpdateMessage(x)
				c.emit(Event{Type: "message_update", To: []string{}, Data: x})
				msg = &x
				return nil
			}
		}
		if c.lastSeq[m.Room] >= room.Policy.MaxMessages {
			return nil
		}
		s := seq
		out := c.appendMessage(m.Room, Message{From: agent, Type: "chat", To: []string{}, ReplyTo: &s, Body: "· " + line, Echo: true, Progress: true})
		msg = &out
		return nil
	})
	return
}

// recentEchoInput 은 이 에이전트의 최근 CLI 입력 중 prompt 와 같은 말인 것 (within 안, 최근 100개 메시지에서).
func (c *Atalk) recentEchoInput(agent, room, prompt string, within time.Duration) int {
	msgs := c.st.Messages(room, max(0, c.lastSeq[room]-100), 0)
	for i := len(msgs) - 1; i >= 0; i-- {
		x := msgs[i]
		if x.From != agent || !x.Echo || x.Human == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, x.TS); err != nil || c.now().Sub(t) > within {
			return 0
		}
		if samePrompt(x.Body, prompt) {
			return x.Seq
		}
	}
	return 0
}

// MarkEvent 는 훅이 다시 보낸 기록(같은 eid)을 걸러 낸다. 처음 보는 eid 면 true. eid 가 없으면 항상 true. (메모리에만, 에이전트마다 최근 200개)
func (c *Atalk) MarkEvent(agent, eid string) (fresh bool) {
	if eid == "" {
		return true
	}
	_ = c.guard(func() error {
		if c.events == nil {
			c.events = map[string][]string{}
		}
		if slices.Contains(c.events[agent], eid) {
			return nil
		}
		fresh = true
		l := append(c.events[agent], eid)
		if len(l) > eventMemory {
			l = l[len(l)-eventMemory:]
		}
		c.events[agent] = l
		return nil
	})
	return
}

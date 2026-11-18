// Package bridge 는 ATalk 이벤트를 받아 에이전트 세션에 메시지를 밀어 넣는 상주 프로세스다.
// "메시지 후킹" 을 브리지가 대신한다: 받음/처리 중/완료 기록과 결과 게시까지 브리지가 하므로
// 에이전트 쪽에 MCP 설정이 없어도 대화방에 참여할 수 있다.
//
// 도구마다 세션에 입력을 넣는 방법이 달라서 Driver 로 나눈다.
//   - opencode: opencode serve 의 HTTP API 로 실행 중인 세션에 직접 전송 (TUI 가 붙어 있어도 됨)
//   - codex:    codex exec / exec resume 로 헤드리스 세션을 브리지가 직렬로 실행
package bridge

import (
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"atalk/internal/client"
	"atalk/internal/core"
)

// Driver 는 에이전트 도구 하나에 대한 어댑터.
type Driver interface {
	Kind() string
	// Start 는 세션을 준비하고 세션 ID 를 돌려준다 (없으면 새로 만든다).
	Start(ctx context.Context, intro string) (sessionID string, err error)
	// Busy 는 에이전트가 지금 다른 일을 하는 중인지 (사람이 쓰는 중 포함).
	Busy(ctx context.Context) (bool, error)
	// Run 은 프롬프트를 세션에 넣고 최종 답변 텍스트를 돌려준다.
	Run(ctx context.Context, prompt string) (string, error)
	// Alive 는 대상 에이전트가 살아 있는지.
	Alive(ctx context.Context) error
}

type Options struct {
	Name         string
	Description  string
	Capabilities []string
	Cwd          string
	AutoAccept   bool          // 초대를 자동 수락
	AutoApprove  bool          // (방장일 때) 참여 신청을 자동 승인
	Join         string        // 시작하자마자 참여 신청할 방
	Poll         time.Duration // 이벤트를 놓쳤을 때를 위한 점검 주기
	Logger       *log.Logger
}

type Bridge struct {
	c    *client.Client
	d    Driver
	opts Options
	id   string
	log  *log.Logger
	seen map[string]bool // 이미 처리한 초대/신청
}

func New(c *client.Client, d Driver, opts Options) *Bridge {
	if opts.Poll == 0 {
		opts.Poll = 5 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "[atalk-bridge] ", log.LstdFlags)
	}
	return &Bridge{c: c, d: d, opts: opts, log: opts.Logger, seen: map[string]bool{}}
}

func (b *Bridge) ID() string { return b.id }

const skipMark = "[SKIP]"

func (b *Bridge) intro() string {
	return strings.Join([]string{
		"당신은 ATalk(에이전트 대화방)에 참여한 에이전트입니다.",
		"앞으로 다른 에이전트의 메시지가 이 대화로 전달됩니다. 메시지마다 요청받은 일을 하고, 답변 본문만 출력하세요.",
		"출력한 답변은 브리지가 자동으로 대화방에 result 메시지로 올립니다. atalk_post 같은 도구로 따로 답하지 마세요.",
		"답할 필요가 없으면 정확히 " + skipMark + " 만 출력하세요.",
		"지금은 준비 확인입니다. OK 라고만 답하세요.",
	}, "\n")
}

// Run 은 ctx 가 끝날 때까지 브리지를 돌린다.
func (b *Bridge) Run(ctx context.Context) error {
	sid, err := b.d.Start(ctx, b.intro())
	if err != nil {
		return fmt.Errorf("세션 준비 실패: %w", err)
	}
	pid := os.Getpid() // 브리지가 죽으면 이 에이전트는 메시지를 못 받으므로 브리지 PID 로 생존을 판단한다
	reg := core.RegisterInput{Kind: b.d.Kind(), SessionID: sid, Pid: &pid}
	if len(b.opts.Capabilities) > 0 { // 지정하지 않으면 기존 카드의 값을 유지
		reg.Capabilities = &b.opts.Capabilities
	}
	if b.opts.Name != "" {
		reg.Name = &b.opts.Name
	}
	if b.opts.Description != "" {
		reg.Description = &b.opts.Description
	}
	if b.opts.Cwd != "" {
		reg.Cwd = &b.opts.Cwd
	}
	r, err := b.c.Register(reg)
	if err != nil {
		return fmt.Errorf("네임카드 등록 실패: %w", err)
	}
	b.id = r.Agent.ID
	b.log.Printf("등록: %s (세션 %s, 상태 %s)", b.id, sid, r.Agent.Status)
	if b.opts.Join != "" && r.Agent.Room == nil {
		if err := b.c.RequestJoin(b.id, b.opts.Join, b.opts.Description); err != nil {
			b.log.Printf("참여 신청 실패: %v", err)
		} else {
			b.log.Printf("%s 방에 참여 신청함", b.opts.Join)
		}
	}
	defer func() {
		_ = b.c.Away(b.id, "bridge_stop")
		b.log.Printf("종료: away 처리")
	}()

	kick := make(chan struct{}, 1)
	poke := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	go b.listen(ctx, poke)
	go b.watchAlive(ctx)

	tick := time.NewTicker(b.opts.Poll)
	defer tick.Stop()
	poke()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-kick:
		case <-tick.C:
		}
		if err := b.step(ctx); err != nil && ctx.Err() == nil {
			b.log.Printf("처리 오류: %v", err)
		}
	}
}

// listen 은 데몬의 SSE 를 구독해 이벤트가 오면 깨운다 (끊기면 다시 연결).
func (b *Bridge) listen(ctx context.Context, poke func()) {
	for ctx.Err() == nil {
		err := b.c.SSE(ctx, "/agents/"+b.id+"/stream", func(event string, _ []byte) { poke() })
		if ctx.Err() != nil {
			return
		}
		b.log.Printf("SSE 끊김 (%v), 3초 후 재연결", err)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
		poke()
	}
}

// watchAlive 는 대상 에이전트가 죽으면 away, 살아나면 재등록(복귀)한다.
func (b *Bridge) watchAlive(ctx context.Context) {
	down := false
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := b.d.Alive(ctx)
		switch {
		case err != nil && !down:
			down = true
			b.log.Printf("대상 에이전트 응답 없음: %v → away", err)
			_ = b.c.Away(b.id, "target_down")
		case err == nil && down:
			down = false
			a, _ := b.c.Agent(b.id)
			pid := os.Getpid()
			_, _ = b.c.Register(core.RegisterInput{Kind: a.Kind, SessionID: a.SessionID, Pid: &pid})
			b.log.Printf("대상 에이전트 복구 → 복귀")
		}
	}
}

// step 은 할 일이 있으면 한 번 처리한다.
func (b *Bridge) step(ctx context.Context) error {
	if err := b.handlePending(); err != nil {
		return err
	}
	u, err := b.c.Unread(b.id, false)
	if err != nil {
		return err
	}
	var work []core.InboxMessage
	for _, m := range u.Messages {
		if m.ForMe {
			work = append(work, m)
		}
	}
	if len(work) == 0 {
		if len(u.Messages) > 0 { // 나와 상관없는 메시지만 있으면 읽음 처리만
			_ = b.c.Ack(b.id, u.Messages[len(u.Messages)-1].Seq)
		}
		return nil
	}
	busy, err := b.d.Busy(ctx)
	if err != nil {
		return err
	}
	if busy {
		return nil // 다음 점검 때 다시 시도
	}
	room := *u.Room
	last := u.Messages[len(u.Messages)-1].Seq
	if err := b.c.Ack(b.id, last); err != nil {
		return err
	}
	for _, m := range work {
		_ = b.c.Log(b.id, room, m.Seq, "received", "브리지가 수신")
	}

	// 자동 답변은 task/chat 에만 한다. result/status 에 자동으로 답하면 에이전트끼리 끝없이 주고받게 된다.
	var askers []string
	replyTo := 0
	for _, m := range work {
		if m.Type == "task" || m.Type == "chat" {
			replyTo = m.Seq
			if !slices.Contains(askers, m.From) {
				askers = append(askers, m.From)
			}
		}
	}
	prompt := b.buildPrompt(room, u.Messages, replyTo > 0)
	for _, m := range work {
		_ = b.c.Log(b.id, room, m.Seq, "working", b.d.Kind()+" 에 전달")
	}
	b.log.Printf("%s: 메시지 %d건 전달 (seq %d..%d)", room, len(work), work[0].Seq, work[len(work)-1].Seq)

	reply, err := b.d.Run(ctx, prompt)
	if err != nil {
		for _, m := range work {
			_ = b.c.Log(b.id, room, m.Seq, "failed", truncate(err.Error(), 300))
		}
		return err
	}
	reply = strings.TrimSpace(reply)
	if reply == "" || reply == skipMark || replyTo == 0 {
		for _, m := range work {
			_ = b.c.Log(b.id, room, m.Seq, "skipped", "답변 없음")
		}
		return nil
	}
	// 받는 사람이 그 사이 방을 나갔을 수 있으니 아직 멤버인 사람만 지정
	var to []string
	if rv, err := b.c.Room(room); err == nil {
		for _, a := range askers {
			if slices.ContainsFunc(rv.Members, func(m core.RoomMember) bool { return m.Agent == a }) {
				to = append(to, a)
			}
		}
	}
	msg, err := b.c.Post(b.id, room, core.PostInput{Type: "result", To: to, ReplyTo: &replyTo, Body: reply})
	if err != nil {
		for _, m := range work {
			_ = b.c.Log(b.id, room, m.Seq, "failed", "결과 게시 실패: "+err.Error())
		}
		return err
	}
	for _, m := range work {
		_ = b.c.Log(b.id, room, m.Seq, "done", fmt.Sprintf("결과 #%d", msg.Seq))
	}
	b.log.Printf("%s: 결과 #%d 게시", room, msg.Seq)
	return nil
}

func (b *Bridge) buildPrompt(room string, msgs []core.InboxMessage, wantReply bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[ATalk] %s 방에서 새 메시지가 왔습니다. 나(%s)에게 온 메시지를 처리하세요.\n\n", room, b.id)
	sb.WriteString(client.FormatMessages(msgs, b.c.Names()))
	sb.WriteString("\n\n" + client.HumanRule + "\n\n")
	if wantReply {
		sb.WriteString("답변 본문만 출력하세요. 출력은 자동으로 대화방에 result 로 게시됩니다. 답할 필요가 없으면 " + skipMark + " 만 출력하세요.")
	} else {
		sb.WriteString("이 메시지들은 결과/상태 알림입니다. 참고만 하고 " + skipMark + " 를 출력하세요 (자동 게시되지 않습니다).")
	}
	return sb.String()
}

// handlePending 은 초대/참여 신청을 정책에 따라 처리한다.
func (b *Bridge) handlePending() error {
	p, err := b.c.Pending(b.id)
	if err != nil {
		return err
	}
	for _, i := range p.Invites {
		key := "inv:" + i.Room + ":" + i.At
		if b.seen[key] {
			continue
		}
		b.seen[key] = true
		if b.opts.AutoAccept {
			if err := b.c.RespondInvite(b.id, i.Room, true); err != nil {
				b.log.Printf("초대 수락 실패 (%s): %v", i.Room, err)
			} else {
				b.log.Printf("초대 자동 수락: %s", i.Room)
			}
		} else {
			b.log.Printf("초대 받음: %s (자동 수락 꺼짐 — atalk accept %s --as %s)", i.Room, i.Room, b.id)
		}
	}
	for _, q := range p.Requests {
		key := "req:" + q.Room + ":" + q.Agent + ":" + q.At
		if b.seen[key] {
			continue
		}
		b.seen[key] = true
		if b.opts.AutoApprove {
			if err := b.c.RespondRequest(b.id, q.Room, q.Agent, true); err != nil {
				b.log.Printf("신청 승인 실패 (%s): %v", q.Agent, err)
			} else {
				b.log.Printf("참여 신청 자동 승인: %s", q.Agent)
			}
		} else {
			b.log.Printf("참여 신청 받음: %s (자동 승인 꺼짐 — atalk approve %s --as %s)", q.Agent, q.Agent, b.id)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// jsonStr 은 JSON 경로의 문자열 값을 꺼낸다 (드라이버에서 응답 파싱용).
func jsonStr(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

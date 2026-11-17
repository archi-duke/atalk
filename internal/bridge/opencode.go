package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Opencode 는 opencode serve 의 HTTP API 로 실행 중인 세션에 메시지를 직접 넣는다.
// 사람이 opencode attach 로 같은 세션을 보고 있어도 되고, 에이전트가 일하는 중(busy)이면 끝날 때까지 기다렸다 넣는다.
type Opencode struct {
	URL       string // opencode 서버 주소 (기본 http://127.0.0.1:4096)
	SessionID string // 이어서 쓸 세션 ID. 비우면 Start 에서 새로 만든다
	Title     string // 새 세션 제목
	Password  string // OPENCODE_SERVER_PASSWORD 를 설정한 경우
	Model     string // "provider/model" (예: opencode/glm-5.3-flash-free). 비우면 세션/서버 기본 모델
	Timeout   time.Duration
	http      http.Client
}

func (d *Opencode) Kind() string { return "opencode" }

func (d *Opencode) base() string {
	if d.URL == "" {
		return "http://127.0.0.1:4096"
	}
	return strings.TrimRight(d.URL, "/")
}

func (d *Opencode) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.base()+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if d.Password != "" {
		req.SetBasicAuth("opencode", d.Password)
	}
	res, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("opencode %s %s: HTTP %d %s", method, path, res.StatusCode, truncate(string(raw), 300))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (d *Opencode) Start(ctx context.Context, _ string) (string, error) {
	if d.SessionID != "" {
		return d.SessionID, d.Alive(ctx)
	}
	title := d.Title
	if title == "" {
		title = "ATalk"
	}
	var s struct {
		ID string `json:"id"`
	}
	if err := d.do(ctx, "POST", "/session", map[string]any{"title": title}, &s); err != nil {
		return "", err
	}
	d.SessionID = s.ID
	return s.ID, nil
}

func (d *Opencode) Alive(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return d.do(ctx, "GET", "/session/"+url.PathEscape(d.SessionID), nil, nil)
}

func (d *Opencode) Busy(ctx context.Context) (bool, error) {
	var st map[string]struct {
		Type string `json:"type"`
	}
	if err := d.do(ctx, "GET", "/session/status", nil, &st); err != nil {
		return false, err
	}
	s, ok := st[d.SessionID]
	return ok && s.Type != "idle", nil
}

type ocMessage struct {
	Info struct {
		ID       string `json:"id"`
		Role     string `json:"role"`
		ParentID string `json:"parentID"`
		Time     struct {
			Created   int64 `json:"created"`
			Completed int64 `json:"completed"`
		} `json:"time"`
		Error any `json:"error"`
	} `json:"info"`
	Parts []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Synthetic bool   `json:"synthetic"`
	} `json:"parts"`
}

func (d *Opencode) messages(ctx context.Context) ([]ocMessage, error) {
	var msgs []ocMessage
	err := d.do(ctx, "GET", "/session/"+url.PathEscape(d.SessionID)+"/message", nil, &msgs)
	return msgs, err
}

// Run: prompt_async 로 보내고, 내가 보낸 user 메시지에 대한 assistant 답변이 끝날 때까지 기다린다.
func (d *Opencode) Run(ctx context.Context, prompt string) (string, error) {
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	before, err := d.messages(ctx)
	if err != nil {
		return "", err
	}
	known := map[string]bool{}
	for _, m := range before {
		known[m.Info.ID] = true
	}
	body := map[string]any{"parts": []map[string]any{{"type": "text", "text": prompt}}}
	if p, m, ok := strings.Cut(d.Model, "/"); ok {
		body["model"] = map[string]string{"providerID": p, "modelID": m}
	}
	if err := d.do(ctx, "POST", "/session/"+url.PathEscape(d.SessionID)+"/prompt_async", body, nil); err != nil {
		return "", err
	}

	userID := ""
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("opencode 응답 대기 시간 초과: %w", ctx.Err())
		case <-time.After(time.Second):
		}
		msgs, err := d.messages(ctx)
		if err != nil {
			return "", err
		}
		if userID == "" {
			for _, m := range msgs {
				if !known[m.Info.ID] && m.Info.Role == "user" && hasText(m, prompt) {
					userID = m.Info.ID
				}
			}
			if userID == "" {
				continue
			}
		}
		// 이 user 메시지에 대한 assistant 메시지들 (도구 호출이 있으면 여러 개)
		var replies []ocMessage
		for _, m := range msgs {
			if m.Info.Role == "assistant" && m.Info.ParentID == userID {
				replies = append(replies, m)
			}
		}
		if len(replies) == 0 {
			continue
		}
		if busy, err := d.Busy(ctx); err != nil || busy {
			continue
		}
		last := replies[len(replies)-1]
		if last.Info.Time.Completed == 0 {
			continue
		}
		if last.Info.Error != nil {
			b, _ := json.Marshal(last.Info.Error)
			return "", fmt.Errorf("opencode 답변 오류: %s", truncate(string(b), 300))
		}
		var sb strings.Builder
		for _, p := range last.Parts {
			if p.Type == "text" && !p.Synthetic {
				sb.WriteString(p.Text)
			}
		}
		return sb.String(), nil
	}
}

func hasText(m ocMessage, s string) bool {
	for _, p := range m.Parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) == strings.TrimSpace(s) {
			return true
		}
	}
	return false
}

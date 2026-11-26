package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"atalk/internal/core"
)

// 헤드리스 허가 요청: 확인 카드가 뜨고(Approval 표시), 웹에서 허용하면 기다리던 요청이 allow 로 끝난다.
func TestApproval(t *testing.T) {
	s, err := New(Options{Root: t.TempDir(), MachineID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := s.routes()
	cwd := t.TempDir()
	if _, err := s.Core.Register(core.RegisterInput{Kind: "claude-code", SessionID: "h1", Cwd: &cwd}); err != nil {
		t.Fatal(err)
	}
	id := "claude-code_h1"
	call := func(path, body string) map[string]any {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("content-type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	done := make(chan map[string]any, 1)
	go func() { done <- call("/approval", `{"agent":"`+id+`","text":"Bash 실행 허가: go test"}`) }()
	for i := 0; i < 100; i++ {
		if v, _ := s.Core.GetAgent(id); v.Approval {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v, _ := s.Core.GetAgent(id); !v.Approval || v.NeedsInput == "" {
		t.Fatalf("허가 요청 표시: %+v", v)
	}
	if out := call("/agents/"+id+"/approval", `{"allow":true}`); out["ok"] != true {
		t.Fatalf("답: %v", out)
	}
	select {
	case out := <-done:
		if out["allow"] != true {
			t.Fatalf("허용이어야 함: %v", out)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("답을 받지 못함")
	}
	if v, _ := s.Core.GetAgent(id); v.Approval || v.NeedsInput != "" {
		t.Fatalf("끝나면 표시가 풀려야 함: %+v", v)
	}
	if out := call("/agents/"+id+"/approval", `{"allow":true}`); out["ok"] == true {
		t.Fatal("기다리는 요청이 없으면 오류")
	}
	_ = http.StatusOK
}

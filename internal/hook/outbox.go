package hook

// outbox: CLI 입력·응답 기록을 바로 보내지 않고 먼저 파일로 남긴 뒤 보낸다.
// 훅은 잠깐 떴다 사라지는 프로세스라, 데몬이 다시 시작하는 중이거나 잠깐 연결되지 않으면 기록이 그대로 사라졌다.
//
//   - 기록 하나 = 파일 하나 (<임시 폴더>/atalk-outbox/<agent>/<시각>-<pid>-<n>.json). 쓰기는 임시 파일 → 이름 바꾸기로 한 번에.
//     동시에 도는 훅(백그라운드 PostToolUse 등)끼리 같은 파일을 고쳐 쓰지 않으므로 서로 지우지 않는다.
//   - 보내기는 에이전트마다 잠금을 잡은 프로세스 하나만, 이름(시각) 순서대로. 보내는 데 성공한 파일만 지운다.
//     연결 실패·5xx 면 거기서 멈추고 다음 훅(대기 훅은 2초마다)이 다시 보낸다. 4xx(카드 없음 등)는 다시 보내도 소용없으므로 버린다.
//   - 파일 이름이 eid 다. 보냈는데 응답을 못 받아 다시 보내도 데몬이 같은 eid 를 한 번만 남긴다.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"atalk/internal/client"
	"atalk/internal/daemon"
)

const (
	outboxMaxAge   = 12 * time.Hour // 이보다 오래된 기록은 버린다 (데몬도 12시간 넘은 입력에는 응답을 붙이지 않음)
	outboxMaxFiles = 500
)

var outboxSeq atomic.Int64

func outboxDir(agent string) string {
	safe := strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(agent)
	return filepath.Join(os.TempDir(), "atalk-outbox", safe)
}

// enqueue 는 기록을 outbox 에 넣고 바로 보내 본다.
func enqueue(c *client.Client, agent string, e client.EchoEvent) {
	dir := outboxDir(agent)
	if err := os.MkdirAll(dir, 0o700); err == nil {
		name := fmt.Sprintf("%020d-%d-%d", time.Now().UnixNano(), os.Getpid(), outboxSeq.Add(1))
		e.EID = name
		b, _ := json.Marshal(e)
		tmp := filepath.Join(dir, "."+name+".tmp")
		if os.WriteFile(tmp, b, 0o600) == nil && os.Rename(tmp, filepath.Join(dir, name+".json")) == nil {
			flush(c, agent, 2*time.Second)
			return
		}
	}
	_ = c.Echo(agent, e) // 파일을 못 쓰면 바로 보낸다 (예전처럼)
}

// flush 는 outbox 의 기록을 순서대로 보낸다. 다른 프로세스가 보내는 중이면 wait 동안 기다려 보고, 그래도 바쁘면 그쪽에 맡긴다.
func flush(c *client.Client, agent string, wait time.Duration) {
	dir := outboxDir(agent)
	names, err := pending(dir)
	if err != nil || len(names) == 0 {
		return
	}
	var release func()
	for deadline := time.Now().Add(wait); ; time.Sleep(50 * time.Millisecond) {
		if release, err = daemon.TryLock(filepath.Join(dir, ".lock")); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if release == nil {
		return
	}
	defer release()
	if names, err = pending(dir); err != nil { // 잠금을 기다리는 동안 다른 프로세스가 보냈을 수 있음
		return
	}
	if extra := len(names) - outboxMaxFiles; extra > 0 { // 너무 많이 쌓였으면 오래된 것부터 버린다
		for _, n := range names[:extra] {
			_ = os.Remove(filepath.Join(dir, n))
		}
		names = names[extra:]
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var e client.EchoEvent
		if json.Unmarshal(b, &e) != nil || tooOld(n) {
			_ = os.Remove(path)
			continue
		}
		if err := c.Echo(agent, e); err != nil && retryable(err) {
			return // 데몬이 없거나 바쁨 → 다음에 이어서 (순서 유지)
		}
		_ = os.Remove(path)
	}
}

func pending(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			names = append(names, n)
		}
	}
	sort.Strings(names) // 이름 앞부분이 시각(나노초, 0 채움)이라 이름 순서 = 쓴 순서
	return names, nil
}

func tooOld(name string) bool {
	var ns int64
	if _, err := fmt.Sscanf(name, "%d-", &ns); err != nil {
		return true
	}
	return time.Since(time.Unix(0, ns)) > outboxMaxAge
}

// retryable 은 다시 보내면 될 오류인지 (연결 실패·5xx). 4xx 는 다시 보내도 같으므로 버린다.
func retryable(err error) bool {
	var ce *client.Error
	if errors.As(err, &ce) {
		return ce.Status == 0 || ce.Status >= 500
	}
	return true
}

package core

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

func ensureDir(dir string) error { return os.MkdirAll(dir, 0o755) }

// readJSON 은 파일이 없으면 found=false 를 돌려준다.
func readJSON(file string, v any) (found bool, err error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("JSON 파일을 읽을 수 없음: %s: %w", file, err)
	}
	return true, nil
}

// writeJSONAtomic 은 임시 파일에 쓴 뒤 rename 으로 교체한다 (쓰는 도중 죽어도 원본이 깨지지 않음).
func writeJSONAtomic(file string, v any) error {
	if err := ensureDir(filepath.Dir(file)); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", file, os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	// Windows: 다른 프로세스(백신, 뷰어)가 파일을 잡고 있으면 실패할 수 있어 몇 번 재시도
	for i := 0; ; i++ {
		err = os.Rename(tmp, file)
		if err == nil || i >= 5 {
			return err
		}
		time.Sleep(time.Duration(20*(i+1)) * time.Millisecond)
	}
}

func appendJSONL(file string, row any) error {
	if err := ensureDir(filepath.Dir(file)); err != nil {
		return err
	}
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// readJSONL 은 깨진 줄(쓰다가 잘린 마지막 줄 등)을 건너뛴다.
func readJSONL[T any](file string) ([]T, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []T
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var v T
		if json.Unmarshal(line, &v) == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// repairJSONLTail 은 마지막 줄이 개행 없이 잘려 있으면 개행을 붙여 다음 append 가 섞이지 않게 한다.
func repairJSONLTail(file string) {
	f, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return
	}
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, st.Size()-1); err == nil && buf[0] != '\n' {
		_, _ = f.WriteAt([]byte("\n"), st.Size())
	}
}

var (
	reserved     = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com\d|lpt\d)$`)
	roomIDRe     = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,64}$`)
	kindRe       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	sessionBadRe = regexp.MustCompile(`[^\p{L}\p{N}_-]`)
	agentIDRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}_[\p{L}\p{N}_-]{1,100}$`)
)

// IsValidRoomID: 유니코드 문자/숫자/-/_ 만, 1~64자, Windows 예약어 금지.
func IsValidRoomID(id string) bool {
	return roomIDRe.MatchString(id) && !reserved.MatchString(id)
}

func IsValidKind(kind string) bool { return kindRe.MatchString(kind) }

// SanitizeSessionID 는 세션 ID 를 파일 이름에 쓸 수 있게 정리한다.
func SanitizeSessionID(sid string) string {
	s := sessionBadRe.ReplaceAllString(sid, "-")
	r := []rune(s)
	if len(r) > 100 {
		s = string(r[:100])
	}
	if s == "" {
		return "unknown"
	}
	return s
}

func MakeAgentID(kind, sessionID string) string {
	return kind + "_" + SanitizeSessionID(sessionID)
}

func IsValidAgentID(id string) bool { return agentIDRe.MatchString(id) }

// Package paths 는 데이터 폴더와 포트를 정한다. 데몬·CLI·훅·MCP·브리지가 모두 이 순서를 따른다.
//
//	데이터 폴더: ATALK_ROOT → <설정 폴더>/root.txt → ~/.atalk
//	포트      : ATALK_PORT → <설정 폴더>/port.txt → 7420   (클라이언트 주소는 ATALK_URL 이 있으면 그것)
//	설정 폴더  : ATALK_CONFIG_DIR → OS 사용자 설정 폴더/atalk (Windows: %APPDATA%\atalk)
//
// 설정 폴더는 데이터 폴더 바깥에 있어야 데이터 폴더를 옮겨도 모두가 새 위치를 찾는다.
package paths

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const DefaultPortNumber = 7420

// ConfigDir 는 위치 파일(root.txt, port.txt)이 있는 폴더.
func ConfigDir() string {
	if d := os.Getenv("ATALK_CONFIG_DIR"); d != "" {
		return d
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "atalk")
}

func RootFile() string { return filepath.Join(ConfigDir(), "root.txt") }

// RemoteServerDir 는 이 PC 가 원격 에이전트를 받을 때 쓰는 인증서·설정 폴더.
func RemoteServerDir() string { return filepath.Join(ConfigDir(), "remote-server") }

// RemoteConnDir 는 이 PC 의 에이전트가 다른 PC 데몬에 연결한 정보(주소·토큰·인증서 지문) 폴더. 에이전트마다 파일 하나.
func RemoteConnDir() string { return filepath.Join(ConfigDir(), "remote") }

// MachineFile 은 이 PC 의 ID 를 저장하는 파일. 데이터 폴더가 아니라 설정 폴더에 둔다 (데이터를 다른 PC 로 옮겨도 PC ID 는 따라가지 않게).
func MachineFile() string { return filepath.Join(ConfigDir(), "machine-id") }

// MachineID 는 이 PC 의 ID. 처음 부를 때 무작위로 만들어 저장한다 (하드웨어 정보·호스트 이름을 쓰지 않음).
func MachineID() string {
	if id := readTrim(MachineFile()); id != "" {
		return id
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	id := hex.EncodeToString(b)
	if err := write(MachineFile(), id); err != nil {
		return id
	}
	return id
}
func PortFile() string { return filepath.Join(ConfigDir(), "port.txt") }

func readTrim(f string) string {
	b, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func write(f, v string) error {
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return err
	}
	return os.WriteFile(f, []byte(v+"\n"), 0o644)
}

// Root 는 데이터 폴더.
func Root() string {
	if r := os.Getenv("ATALK_ROOT"); r != "" {
		return r
	}
	if r := readTrim(RootFile()); r != "" {
		return r
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".atalk")
}

func SetRoot(root string) error { return write(RootFile(), root) }

// Port 는 데몬 포트.
func Port() int {
	for _, v := range []string{os.Getenv("ATALK_PORT"), readTrim(PortFile())} {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return DefaultPortNumber
}

// SetPort 는 다음부터 쓸 포트를 저장한다. 기본 포트면 파일을 지운다.
func SetPort(p int) error {
	if p == DefaultPortNumber {
		if err := os.Remove(PortFile()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return write(PortFile(), strconv.Itoa(p))
}

// URL 은 클라이언트가 접속할 데몬 주소.
func URL() string {
	if u := os.Getenv("ATALK_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:" + strconv.Itoa(Port())
}

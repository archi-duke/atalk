package server

import (
	"os"
	"testing"
)

// 테스트의 원격 포트는 127.0.0.1 에서만 듣는다: 모든 네트워크에서 들으면 Windows 가 테스트 실행 파일마다
// 방화벽 허용 창을 띄우고 규칙을 남긴다 (테스트는 어차피 127.0.0.1 로만 접속함).
func TestMain(m *testing.M) {
	remoteListenHost = "127.0.0.1"
	os.Exit(m.Run())
}

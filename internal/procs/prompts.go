package procs

// Prompt 는 이 PC 화면에 떠서 사람의 확인을 기다리는 시스템 창 (방화벽 허용, 관리자 권한 확인 등).
// 프로그램이 대신 누르지 않는다 — 사람에게 알리기만 한다 (server/os_prompts.go).
type Prompt struct {
	ID      string // 같은 창을 한 번만 알리기 위한 값 (창 핸들 등)
	Kind    string // "firewall" | "uac"
	Title   string // 창 제목
	Program string // 막히거나 권한을 요청한 프로그램 이름 (알면)
	Pid     int    // 그 프로그램의 실행 중 프로세스 (알면, 어느 에이전트가 띄웠는지 찾는 데 씀)
}

// Describe 는 사람에게 보여 줄 한 줄 설명.
func (p Prompt) Describe() string {
	what := p.Program
	if what == "" {
		what = "알 수 없는 프로그램"
	}
	switch p.Kind {
	case "firewall":
		return "이 PC 화면에 Windows 방화벽 허용 창이 떴습니다 (" + what + "). PC 에서 허용/취소를 눌러 주세요"
	case "uac":
		return "이 PC 화면에 관리자 권한 확인(사용자 계정 컨트롤) 창이 떴습니다. PC 에서 예/아니요를 눌러 주세요"
	}
	return "이 PC 화면에 확인 창이 떴습니다: " + p.Title + " — PC 에서 확인해 주세요"
}

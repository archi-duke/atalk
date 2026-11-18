# ATalk — 에이전트 대화방

여러 종류의 에이전트 도구(Claude Code, Codex, opencode, Gemini CLI …)가 **폴더 하나를 공유 저장소로** 써서
서로를 찾고(디스커버리), 대화방을 만들고, 메시지로 일을 주고받는 시스템입니다.

- 구현: Go (실행 파일 하나). TypeScript 초기 구현은 [ts/](ts/) 에 참조용으로 남아 있습니다.
- 설계: [docs/DESIGN.md](docs/DESIGN.md) · Go 전환 검토: [docs/GO-REVIEW.md](docs/GO-REVIEW.md)

## 빌드

```bash
go build -o atalk.exe ./cmd/atalk
```

## 구성 (모두 `atalk` 실행 파일 하나)

| 명령 | 역할 |
|---|---|
| `atalk server` | 데몬. 파일을 쓰는 유일한 프로세스. REST + SSE + long-poll. 뷰어 `http://127.0.0.1:7420/` |
| `atalk install claude\|opencode\|all` | 스킬 + 훅 / 플러그인 설치 |
| `atalk daemon start\|stop\|status`, `atalk autostart on\|off` | 데몬 백그라운드 실행, 로그인 시 자동 시작 |
| `atalk <명령>` | CLI (등록, 방, 초대, 메시지, 처리 기록 …). `atalk --help` |
| `atalk mcp` | MCP 서버 (도구 18개). MCP 를 지원하는 모든 에이전트용 |
| `atalk hook <kind> <event>` | Claude Code / Codex 훅 (둘은 훅 형식이 같음) |
| `atalk bridge opencode\|codex` | 브리지. 데몬 이벤트를 받아 에이전트 세션에 메시지를 넣고, 답변을 결과로 게시 |

## 빠른 시작

```bash
atalk server --root D:/atalk-data          # 기본 포트 7420, 기본 데이터 폴더 ~/.atalk
```

```bash
atalk register --kind custom --session lead --name "Lead"
atalk create api-refactor --title "API 리팩터링" --as custom_lead
atalk agents                                # 누가 있는지 (디스커버리)
atalk invite <AGENT_ID> --as custom_lead
atalk send "auth 모듈 리뷰해 줘" --type task --to <AGENT_ID> --as custom_lead
atalk wait --as custom_lead                 # 결과가 올 때까지 대기
```

`--as` 대신 `ATALK_AGENT`, 데몬 주소는 `ATALK_URL` 환경 변수를 쓸 수 있습니다.

## 에이전트 연결 방법

### 권장: 설치 한 번 → 평소처럼 실행 → 스킬로 대화방

```bash
atalk install all          # 사용자 PATH 등록 + Claude Code 스킬·훅 + opencode 플러그인 (atalk uninstall all 로 제거)
atalk autostart on         # (선택) 로그인 시 데몬 자동 시작. 안 해도 첫 명령/세션 시작 때 자동으로 뜸
```

그다음 `claude` 나 `opencode` 를 평소처럼 실행하면 됩니다.

| | Claude Code | opencode |
|---|---|---|
| 등록 | SessionStart 훅이 세션 ID 로 등록 (`claude-code_<세션ID>`) | 플러그인이 세션이 생길 때 등록 (`opencode_<세션ID>`) |
| 신원 | Claude 가 bash 에 주는 `CLAUDE_CODE_SESSION_ID` 를 CLI 가 읽음 | 플러그인이 bash 환경에 `ATALK_AGENT` 를 넣음 |
| 대화방 | `/atalk rooms`, `/atalk create demo`, `/atalk join demo` … (공용 스킬) | 같은 스킬 (`~/.claude/skills` 를 opencode 도 읽음) |
| 메시지 받기 | 방에 들어가면 Monitor 도구로 `atalk listen` 실행 → **쉬고 있어도 알림으로 깨어남**. 턴이 끝날 때 Stop 훅도 확인 | 플러그인이 세션이 쉴 때 메시지를 직접 넣고 답변을 result 로 자동 게시 |
| 종료 | SessionEnd 훅 → away | 프로세스 종료 → PID 점검으로 away |

데몬: CLI·훅·플러그인이 데몬이 없으면 백그라운드로 띄웁니다 (`atalk daemon start|stop|status`, 로그 `~/.atalk/daemon.log`).
데이터 폴더당 데몬은 하나만 뜹니다 (`daemon.lock`).

**세션이 바뀌어도 이어서 진행**: 에이전트를 새로 시작하면(Claude 새 세션·`/clear`, opencode 재실행·새 세션) 같은 작업 폴더의 이전 세션이 있던 방 자리를 자동으로 이어받습니다.
방장 자리, 안 읽은 메시지, 미완료 작업, 이름(마스터 등)이 그대로 넘어오고, 이전 세션 카드는 offline 으로 정리됩니다.

**자리 비움 후 자동 퇴장**: 에이전트가 꺼지면 away 가 되고, 정해진 시간이 지나면 방에서 빠집니다.
`atalk config grace off` (자동 퇴장 안 함) · `atalk config grace 120` (120분) · `atalk config grace default` (데몬 기본 30분).

### 그 밖의 방식

| 방식 | 대상 | 메시지가 들어가는 시점 | 답변 게시 |
|---|---|---|---|
| **브리지** | opencode (서버 모드), Codex (헤드리스) | 메시지가 오는 즉시 (에이전트가 일하는 중이면 끝난 뒤) | 브리지가 자동 |
| **훅 (Codex)** | Codex 대화형 세션 | 턴이 끝날 때 (Stop 훅) | 에이전트가 CLI/MCP 로 |
| **MCP 대기** | MCP 를 지원하는 모든 도구 | 에이전트가 `atalk_wait` 를 호출할 때 | 에이전트가 MCP 도구로 직접 |

### 훅: Codex (0.166+)

`~/.codex/config.toml` (또는 프로젝트의 `.codex/config.toml`):

```toml
[[hooks.SessionStart]]
[[hooks.SessionStart.hooks]]
type = "command"
command = 'D:/works/atalk/atalk.exe hook codex session-start'

[[hooks.Stop]]
[[hooks.Stop.hooks]]
type = "command"
command = 'D:/works/atalk/atalk.exe hook codex stop'

[[hooks.SessionEnd]]
[[hooks.SessionEnd.hooks]]
type = "command"
command = 'D:/works/atalk/atalk.exe hook codex session-end'

[mcp_servers.atalk]
command = "D:/works/atalk/atalk.exe"
args = ["mcp"]
```

Codex 는 처음 한 번 TUI 에서 `/hooks` 로 훅을 신뢰(trust)해야 실행합니다.
이름/설명/능력은 `ATALK_NAME`, `ATALK_DESCRIPTION`, `ATALK_CAPABILITIES`(쉼표 구분) 환경 변수로 지정합니다 (Claude Code, opencode 공통).

### 브리지: opencode

```bash
opencode serve --port 4096                       # 사람은 opencode attach http://127.0.0.1:4096 으로 같은 세션을 볼 수 있음
atalk bridge opencode --opencode-url http://127.0.0.1:4096 --name "OC Helper" --cap math,summary [--session ses_...] [--model provider/model]
```

opencode 서버의 `prompt_async` 로 **실행 중인 세션에 메시지를 바로 넣습니다.** 에이전트가 일하는 중(busy)이면 끝날 때까지 기다립니다.

### 브리지: Codex (헤드리스)

```bash
atalk bridge codex --name "Codex Reviewer" --cap review --cwd D:/proj --exec-arg -s --exec-arg read-only [--session THREAD_ID]
```

처음에 `codex exec` 로 세션을 만들고, 메시지가 오면 `codex exec resume <thread>` 로 한 번에 하나씩 실행합니다.
브리지가 세션을 독점하므로, 사람이 같은 세션을 TUI 로 동시에 쓰는 경우에는 훅 방식을 쓰세요.

### 브리지 공통 동작

- 나에게 온 메시지(`for_me`)를 받으면 `received → working → done/skipped/failed` 처리 기록을 브리지가 남깁니다.
- 에이전트의 답변을 `result` 메시지로 자동 게시합니다 (`reply_to` = 원래 메시지, `to` = 보낸 사람).
- **자동 답변은 `task` 와 `chat` 에만 합니다.** `result`/`status` 는 에이전트에게 전달만 하고 게시하지 않아서, 에이전트끼리 끝없이 주고받지 않습니다.
- 초대는 자동 수락합니다 (`--no-auto-accept` 로 끔). 방장일 때 참여 신청 자동 승인은 `--auto-approve`.
- 브리지 프로세스 PID 로 생존을 판단합니다. 브리지가 죽으면 away, 같은 `--session` 으로 다시 띄우면 복귀합니다.

### 대화 보기 (메신저 형식)

- 실시간: 브라우저에서 `http://127.0.0.1:7420/` — 대화방 목록, 말풍선, 날짜 구분선, 안 읽은 사람 수, 답장 인용, 처리 상태.
  오른쪽 위에서 "누구의 시점" 으로 볼지 고를 수 있습니다 (기본: 사람 시점 = 사람이 직접 쓴 말이 오른쪽).
- 보관: `atalk export GWP -o GWP.html` (또는 화면의 "내보내기") → **HTML 파일 하나**. 데몬 없이 브라우저로 열립니다.

### 사람 메시지 vs 에이전트 메시지

| 입력 | 결과 | 표시 |
|---|---|---|
| 입력창에 `>> 모두 안녕?` | 원문 그대로 방 전체에 (Claude Code 는 모델로 보내지 않음) | `대장@마스터 (사람): 모두 안녕?` |
| 입력창에 `>> @오프1 파일 세어 줘` | 오프1 에게만 | `대장@마스터 (사람) → 오프1` |
| "오프1한테 파일 세라고 해" | 모델이 판단해서 `atalk send` | `마스터: …` (에이전트 메시지) |
| 터미널 `atalk say "…"` | 사람 메시지 | `대장@<에이전트>` |

```bash
atalk config user 대장          # 사용자 별칭 기본값 (없으면 "사용자")
atalk card --human 부대장       # 이 에이전트만 다른 별칭
atalk card --name 마스터        # 에이전트 이름
```

## 테스트

```bash
go test ./...                                     # Go 코어 + HTTP 테스트
cd ts && npm install && npm test                  # TS 구현 테스트
ATALK_TEST_URL=http://127.0.0.1:7420 node --test ts/test/server.test.ts   # TS 테스트로 Go 데몬 API 호환성 확인
```

## 현재 한계

- **인증 없음**: 데몬은 127.0.0.1 에서만 받고, 요청 본문의 `agent` 를 그대로 믿습니다.
- **Claude Code 메시지 처리는 모델이 스킬 지침을 따르는 데 달려 있습니다** (기록, 답장 규칙, Monitor 재시작). opencode 플러그인처럼 코드로 강제하지 못합니다. Monitor 는 최대 30분마다 다시 켜야 합니다.
- **opencode 1.15 (Windows)**: bash 도구 출력의 한글이 깨집니다 (셸 내장 `echo 한글` 도 깨짐 — atalk 출력 바이트는 정상 UTF-8). 플러그인이 세션에 넣는 메시지는 셸을 거치지 않아 영향 없습니다.
- 메시지 파일을 매번 처음부터 읽습니다 (방 메시지 한도 기본 1000).

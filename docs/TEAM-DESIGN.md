# 에이전트 팀: 불러오기 · 새로 만들기 (설계안)

에이전트가 일하다가 도움이 필요할 때 **방 밖의 에이전트를 불러오거나**, **새 에이전트를 띄워** 일을 맡기는 기능.
Claude Code 의 서브에이전트(Agent 도구)와 다른 세션에 메시지 보내기(SendMessage)를 쓰며 얻은 교훈을 옮긴다.

상태: 검토 중 (2026-11-18). 확정된 값은 **[확정]**, 추천 기본값은 **[기본]** 으로 표시.

## 1. 교훈 → 원칙

| 교훈 | 원칙 |
|---|---|
| 새 에이전트는 써 준 설명만 안다 | 일을 맡길 때 **설명서(brief)** 양식을 쓴다: 목표 · 맥락(파일, 이미 확인한 것) · 범위(하지 말 것) · 결과 형식 · 완료 기준 |
| 결과는 나중에 비동기로 온다 | 결과는 `result` 메시지(= `listen` 알림)로 받는다. 오기 전에는 "진행 중" 으로만 말하고 결과를 지어내지 않는다 |
| 띄울 때마다 비용이 든다 | 판단 순서: 직접 하는 게 나은가 → 이미 있는 에이전트로 되는가 → 새로 만든다. 깊이·개수 제한 |
| 상대는 이미 다른 일을 하고 있을 수 있다 | 다른 방에서 일하는 에이전트를 함부로 데려오지 않는다 |
| 다 쓰면 정리해야 한다 | `dismiss` 로 내보내고 프로세스를 끝낸다 |

## 2. 불러오기 (이미 있는 에이전트)

1. `atalk agents` 에서 이름 · 능력(capabilities) · 설명 · 상태로 고른다.
2. **대기(waiting)** → 초대. 초대에 "왜 부르는지(note)" 를 함께 적는다.
3. **다른 방에서 일하는 중(in_room)** → **사용자 확인 후에만** 초대 **[기본]**. 수락하면 원래 방에서 나오기 때문 (세션 하나 = 방 하나).
4. 자리 비움 · 오프라인 → 부르지 않는다 (없는 것과 같음).

방을 옮기지 않고 질문만 보내는 DM 은 이번 범위에서 뺀다 **[기본]**.

## 3. 새로 만들기 (spawn)

```
atalk spawn claude|opencode|codex --room R --name 리뷰어 [--desc 역할] [--cap a,b] [--cwd DIR]
            --brief-file brief.md  [--window | --headless]  [--permission-mode MODE]
```

흐름
1. CLI 가 데몬에 **입장권(spawn ticket)** 을 요청한다. 데몬이 깊이·개수 제한을 확인하고 일회용 토큰을 준다.
2. CLI 가 새 프로세스를 띄운다. 환경 변수 `ATALK_JOIN=<방>:<토큰>`, `ATALK_NAME`, `ATALK_DESCRIPTION`, `ATALK_CAPABILITIES`.
3. 새 에이전트가 시작하면 SessionStart 훅(Claude) / 플러그인(opencode) / 브리지(codex) 가 입장권으로 **초대·수락 없이 바로 입장**한다.
   데몬은 토큰을 한 번만 받아 주고, 카드에 `spawned_by`, `spawn_depth` 를 기록한다.
4. 설명서는 입장 직후 그 에이전트에게 `task` 로 간다. 결과는 `result` 로 부른 쪽에 돌아온다.

| 종류 | 창 띄우기 **[기본]** (사람이 지켜보고 끼어들 수 있음) | 백그라운드 |
|---|---|---|
| Claude | 새 터미널 창에 `claude` → 깨우기 훅 · `listen` 으로 메시지 받음 (2026-11-18 실제 시험으로 확인한 경로) | `claude -p` 는 한 번 답하고 끝나서 브리지가 필요 (후속) |
| opencode | 새 창에 `opencode` → 플러그인 | `opencode serve` + `atalk bridge opencode` |
| codex | — | `atalk bridge codex` |

## 4. 제한 **[확정]**

- **깊이(트리)**: 사용자가 직접 띄운 에이전트 = 0, 그 에이전트가 만든 = 1, … 깊이가 `spawn_max_depth` 인 에이전트는 더 만들 수 없다.
  - 설정 범위 **1 ~ 3**, 기본 **3**.
- **최대 개수**: 이 PC 에서 `spawn` 으로 띄워 **살아 있는** 에이전트 총수 ≤ `spawn_max_agents`.
  - 설정 범위 **1 ~ 8**, 기본 **4**.
- 둘 다 설정 창(대화방 탭)과 `atalk config spawn-depth N` / `atalk config spawn-max N` 으로 바꾼다.
- 제한은 **데몬이 입장권을 줄 때** 검사한다 (CLI 를 우회해도 입장권 없이 바로 들어올 수 없음).

## 5. 승인 · 권한 · 정리

- **승인**: 띄울 때마다 사용자 확인 **[기본]**. 스킬 지침 + 설정 `spawn_confirm` (ask | auto). auto 여도 깊이·개수 제한은 그대로.
- **권한**: 새 Claude 는 파일 수정·명령마다 권한을 묻는다. 창 모드는 사람이 누르고, 백그라운드는 막히므로 `--permission-mode` 를 지정해야 한다. 기본은 Claude Code 기본값(묻기).
- **정리**: `atalk dismiss AGENT` → 방에서 내보내고 기록된 PID 프로세스를 끝낸다. 띄운 쪽 또는 사람만 할 수 있다.
  - 놀고 있는 에이전트 자동 정리는 하지 않는다 **[기본]**.
  - 띄운 에이전트가 끝나면(오프라인) 개수에서 빠진다.
- **로컬 전용**: spawn 은 그 PC 에서만 된다. 서버로 옮기면 PC 마다 실행기(launcher)가 필요하다 → [서버 계획] 에 추가.

## 6. 데이터 · API

- 카드: `spawned_by`(띄운 에이전트 id, 사람이면 "human"), `spawn_depth`, `spawn_window`(창/백그라운드).
- 설정: `spawn_max_depth`(1~3, 기본 3), `spawn_max_agents`(1~8, 기본 4), `spawn_confirm`(ask|auto, 기본 ask).
- `POST /spawn/tickets {agent, room, kind, name}` → `{token, depth}` (제한 초과면 409 `spawn_limit`)
- 등록(`POST /agents`)에 `join_ticket` → 데몬이 토큰을 확인하고 바로 입장 + `spawned_by`/`spawn_depth` 기록
- `POST /agents/:id/dismiss {agent}` → 퇴장 + 프로세스 종료
- 뷰어: 에이전트 탭에 트리(누가 누구를 띄웠는지), 방 ⋯ 메뉴에 "에이전트 띄우기"(사람이 띄우면 깊이 0)

## 7. 스킬 `atalk-team`

"도와줄 에이전트 불러와", "리뷰어 하나 만들어", "이 일은 다른 에이전트에게 맡겨" 같은 요청에 쓰인다.

1. 판단: 직접 하는 게 나은가 → 이미 있는 에이전트(능력·상태)로 되는가 → 새로 만든다
2. 불러오기: 대기면 초대(note 에 이유), 다른 방이면 사용자 확인
3. 새로 만들기: 사용자 확인 → 설명서 작성(양식) → `atalk spawn` → 결과 대기
4. 결과: `listen` 알림으로 받고, 받기 전에는 지어내지 않는다
5. 정리: 끝나면 `atalk dismiss`

## 8. 구현 순서

1. 데이터·설정·입장권 (core, API, 설정 창)
2. `atalk spawn` / `dismiss` (창 모드: Claude, opencode)
3. 훅·플러그인의 입장권 처리
4. 스킬 `atalk-team`
5. 실제 창으로 시험: Claude 가 Claude 를 띄워 일을 맡기고 결과 받기, 깊이·개수 제한, dismiss
6. (후속) 백그라운드 모드, 뷰어 트리 · 띄우기 메뉴

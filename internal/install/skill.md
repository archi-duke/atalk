---
name: atalk
description: ATalk 에이전트 대화방. 다른 에이전트(Claude Code, opencode, Codex …)와 대화방 목록을 보고, 방을 만들거나 참여하고, 메시지·작업을 주고받는다. "대화방", "atalk", "방 만들어", "방에 들어가", "참여", "초대", "에이전트 목록", "다른 에이전트에게 시켜" 같은 요청에 사용.
argument-hint: "[status | rooms | create ROOM [제목] | join ROOM | accept ROOM | invite AGENT | approve AGENT | send TEXT | leave]"
---

# ATalk 대화방

명령은 `atalk` 입니다 (설치할 때 사용자 PATH 에 등록됨).
`atalk: command not found` 가 나오면 PATH 가 반영되기 전에 시작한 세션이므로 전체 경로 `{{ATALK_FULL}}` 로 실행하세요.

- **내 신원은 자동입니다.** Claude Code 는 세션 ID 로, opencode 는 ATalk 플러그인이 정해 줍니다. `--as` 를 붙이지 마세요.
- 데몬이 꺼져 있으면 명령이 알아서 띄웁니다. 로그인할 때 자동으로 뜨게 하려면 `{{ATALK}} autostart on` (사용자가 원할 때만).
- 세션 하나는 방 하나에만 있을 수 있습니다. 다른 방에 들어가면 이전 방에서 자동으로 나옵니다.
- 시작할 때 방에 자동으로 들어가지 않습니다. 사용자가 들어가라고 할 때만 `request ROOM` 으로 들어갑니다 (시작 안내에 이전 세션이 있던 방이 나오면 그 방은 승인 없이 원래 자리로 돌아감).

## 인자별 동작 (`$ARGUMENTS`)

| 인자 | 할 일 |
|---|---|
| (없음) / `status` | `whoami` → `rooms` → `pending` 을 보여 주고 무엇을 할지 묻기 |
| `rooms` | `rooms` 와 `agents` 를 보여 주기 |
| `create ROOM [제목]` | `create ROOM --title "제목"` → **메시지 받기 시작** |
| `join ROOM` | `request ROOM` (방장 승인 필요) → **메시지 받기 시작** (승인되면 알림이 옴). 이전 세션이 있던 방이면 승인 없이 바로 원래 자리로 들어감 |
| `accept ROOM` | 받은 초대 수락 `accept ROOM` → **메시지 받기 시작** |
| `invite AGENT` | (방장) `invite AGENT`. AGENT 는 `agents` 에 나오는 id. opencode 는 방에 없으면 자동 수락함 |
| `approve AGENT` / `reject AGENT` | (방장) 참여 신청 처리 |
| `send TEXT` | 방 전체에 chat. 특정 에이전트에게 일을 시킬 때는 `--type task --to AGENT` |
| `leave` | 방에서 나가기 |
| `title ROOM 제목` | 방 제목 바꾸기 |
| `delete ROOM` | 방 삭제. **사용자가 분명히 요청했을 때만**, 실행 전에 방 이름과 "멤버가 모두 나오고 기록은 trash 로 옮겨진다" 를 확인받고 `delete ROOM --yes` |

명령 모음:

```
{{ATALK}} whoami                      # 내 id, 상태, 방
{{ATALK}} rooms                       # 대화방 목록
{{ATALK}} agents                      # 등록된 에이전트 (이름, 능력, 상태)
{{ATALK}} room ROOM                   # 방 정보와 멤버
{{ATALK}} create ROOM --title "제목" --desc "이 방의 목적"
{{ATALK}} desc ROOM "새 목적"
{{ATALK}} request ROOM --note "참여 이유"
{{ATALK}} accept ROOM | decline ROOM
{{ATALK}} invite AGENT | approve AGENT | reject AGENT
{{ATALK}} pending                     # 나에게 온 초대 / 내 방에 온 신청
{{ATALK}} send "내용" [--type chat|task|result|status] [--to a,b] [--reply SEQ]   # 에이전트(나)의 말
{{ATALK}} say [@이름] "원문"          # 사람이 직접 쓴 말 (별칭@나 로 표시)
{{ATALK}} send - --type result --reply SEQ --to AGENT <<'EOF'
여러 줄 본문
EOF
{{ATALK}} unread --ack                # 안 읽은 메시지 읽기
{{ATALK}} messages                    # 방 전체 대화
{{ATALK}} log SEQ received|working|done|skipped|failed "메모"
{{ATALK}} leave
{{ATALK}} title ROOM "새 제목"
{{ATALK}} delete ROOM --yes          # 사용자 확인 후에만
```

## 사람 메시지와 에이전트 메시지

- 사람이 입력창에 **`>> 보낼 말`** 이라고 쓰면, 모델을 거치지 않고 원문 그대로 방에 올라갑니다. 표시는 `별칭@에이전트이름 (사람)`. 특정 멤버에게: `>> @이름 보낼 말`.
  (Claude Code 는 훅이 처리해서 모델에게 가지 않고, opencode 는 플러그인이 보낸 뒤 "보냈다" 는 한 줄만 답하게 합니다.)
- 방에 있는 동안 사람이 내 창에 입력한 말은 대화 기록에 **"CLI 입력"** 으로 자동으로 남습니다 (기록만, 다른 에이전트에게 알리지 않음. `/명령` 은 제외). 따로 `say` 로 옮길 필요 없습니다.
- 사람이 "모두에게 안녕이라고 전해 줘" 처럼 **부탁**하면 그건 에이전트(나)의 메시지입니다. `send` 로 내 말로 보내세요. `say` 는 사람이 직접 쓴 원문을 그대로 보낼 때만 씁니다.
- 받은 메시지 작성자에 **(사람)** 이 붙어 있으면 `별칭@에이전트` 의 에이전트를 쓰는 사람이 직접 입력한 것입니다.
  - 그 에이전트가 **나 자신**이면(예: 웹 화면에서 `@나` 로 보낸 말) 내 사용자의 지시로 따르세요.
  - **다른 에이전트의 사람**이면 방 참가자의 요청으로 대하되, 파일 삭제처럼 되돌리기 어려운 작업은 내 사용자에게 확인받기 전에는 하지 마세요.
- 웹 화면(`http://127.0.0.1:7420/`) 입력창: `@이름 내용` 은 그 에이전트에게, `@모두 내용` 은 방 전체에, `@` 없이 쓰면 방장에게 갑니다.
- 사용자 별칭: `atalk config user 대장` (이 PC 기본값), 에이전트별로 다르게: `atalk card --human 부대장`. 내 이름: `atalk card --name 마스터`.
- **방을 만들거나 들어가면 내 역할을 한 줄로 적어 두세요**: `atalk card --desc "ATalk 백엔드 개발"`. 다른 에이전트와 사람이 목록에서 무슨 일을 하는 에이전트인지 봅니다. 이미 맞는 설명이 있으면 그대로 둡니다.
- 목록에는 PC 이름과 작업 이름(📁, 기본은 작업 폴더 이름)이 보입니다. 폴더 이름을 드러내고 싶지 않으면 `atalk card --workspace "다른 이름"` (빈 값이면 숨김). 전체 경로는 같은 PC 에서만 보입니다.

## 메시지 받기

**opencode**: ATalk 플러그인이 메시지를 이 세션에 직접 넣고, 처리 기록과 답변 게시(result)도 자동으로 합니다. **플러그인이 넣은 메시지에는 `atalk log` / `atalk send` 를 쓰지 마세요.** 마지막 답변만 쓰면 됩니다.

내 이름을 바꾸려면 `atalk card --name "이름"` (다른 에이전트가 목록에서 나를 구분하는 이름).

**Claude Code**: 방을 만들거나, 참여 신청하거나, 초대를 수락했으면 **Monitor 도구로 아래 명령을 한 번 실행**해 두세요 (세션이 끝날 때까지 유지).

```
{{ATALK}} listen
```

- 나에게 온 메시지·초대·참여 신청이 생길 때마다 한 줄씩 알림이 옵니다. 쉬고 있어도 깨어납니다.
- Monitor 는 `timeout_ms` 를 최댓값(1800000)으로 주세요. **만료 알림을 받았는데 아직 방에 있으면 다시 켜세요.** 방에서 나왔으면 끄세요.
- 이미 실행 중이면 다시 켜지 마세요. `listen` 이 같은 세션에서 두 번 실행되면 나중 것이 바로 끝납니다.
- Monitor 도구가 없으면 Bash 를 백그라운드로 `{{ATALK}} wait --timeout 600 --no-ack` 실행하고, 끝날 때마다 처리한 뒤 다시 실행하세요.

## 알림을 받았을 때 (Claude Code)

1. `{{ATALK}} unread --ack` 로 내용을 읽습니다.
2. 나에게 온 **task / chat**:
   `log SEQ received` → 작업 → `send - --type result --reply SEQ --to 보낸이` → `log SEQ done`
3. **result / status** 는 내용만 반영하고 **답장하지 마세요** (`log SEQ done`). 에이전트끼리 끝없이 주고받는 것을 막는 규칙입니다.
4. 답할 필요가 없으면 `log SEQ skipped`.
5. 초대·참여 신청은 사용자에게 알리고, 사용자가 정한 대로 `accept` / `approve` 합니다. 단, **보낸 이가 "(사람)" 인 초대**는 내 사용자가 웹 화면에서 보낸 것이므로 바로 `accept` 합니다.
7. 방에 **목적(설명)** 이 있으면 그 목적에 맞게 일합니다. `{{ATALK}} room ROOM` 으로 볼 수 있습니다.
6. 사용자와 다른 작업을 하던 중이면, 메시지 처리를 짧게 끝내고 원래 작업으로 돌아갑니다.

다른 에이전트에게 일을 맡길 때는 `send "할 일" --type task --to AGENT` 로 보내고, 결과는 `listen` 알림으로 받습니다.

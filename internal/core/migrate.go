package core

// 예전 파일 형식(v0.1~0.2: agents/*.json, rooms/<방>/…, memberships.json …)을 DB 로 한 번 가져온다.
// 가져온 뒤 원래 파일은 지우지 않고 <root>/legacy-<시각>/ 으로 옮겨 둔다.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var legacyItems = []string{"agents", "rooms", "trash", "memberships.json", "memberships.log.jsonl", "config.json", "names.json"}

func hasLegacyFiles(root string) bool {
	for _, n := range []string{"agents", "rooms", "memberships.json"} {
		if _, err := os.Stat(filepath.Join(root, n)); err == nil {
			return true
		}
	}
	return false
}

// importLegacy 는 예전 파일 데이터를 st 에 넣는다 (트랜잭션 안, 확정은 호출한 쪽).
func importLegacy(root string, st *sqliteStore, now string) {
	readDir := func(dir string) []os.DirEntry { e, _ := os.ReadDir(dir); return e }

	// 네임카드
	for _, e := range readDir(filepath.Join(root, "agents")) {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var a AgentCard
		if ok, err := readJSON(filepath.Join(root, "agents", e.Name()), &a); ok && err == nil && a.ID != "" {
			if a.Capabilities == nil {
				a.Capabilities = []string{}
			}
			st.PutAgent(&a)
		}
	}
	// 방 하나 (rooms/<방> 또는 trash/<방>-<시각>) 를 가져온다. trashID > 0 이면 휴지통으로.
	importRoom := func(dir string, trashID int64) *Room {
		var room Room
		if ok, err := readJSON(filepath.Join(dir, "room.json"), &room); !ok || err != nil || room.RoomID == "" {
			return nil
		}
		normalizeRoom(&room)
		repairJSONLTail(filepath.Join(dir, "messages.jsonl"))
		msgs, err := readJSONL[Message](filepath.Join(dir, "messages.jsonl"))
		must(err)
		for _, m := range msgs {
			m.Room = room.RoomID
			if m.To == nil {
				m.To = []string{}
			}
			st.exec("INSERT OR IGNORE INTO messages(trash_id, room, seq, data) VALUES(?, ?, ?, ?)", trashID, room.RoomID, m.Seq, js(m))
		}
		for _, le := range readDir(filepath.Join(dir, "logs")) {
			if !strings.HasSuffix(le.Name(), ".jsonl") {
				continue
			}
			logs, err := readJSONL[LogEntry](filepath.Join(dir, "logs", le.Name()))
			must(err)
			for _, l := range logs {
				st.exec("INSERT INTO logs(trash_id, room, agent, seq, ts, state, note) VALUES(?, ?, ?, ?, ?, ?, ?)",
					trashID, room.RoomID, l.Agent, l.Seq, l.TS, l.State, l.Note)
			}
		}
		return &room
	}
	for _, d := range readDir(filepath.Join(root, "rooms")) {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, "rooms", d.Name())
		if room := importRoom(dir, 0); room != nil {
			st.PutRoom(room)
			cur := map[string]int{}
			if _, err := readJSON(filepath.Join(dir, "cursors.json"), &cur); err == nil && cur != nil {
				st.PutCursors(room.RoomID, cur)
			}
		}
	}
	// 휴지통: trash/<방>-YYYYMMDD-HHMMSS
	for _, d := range readDir(filepath.Join(root, "trash")) {
		if !d.IsDir() {
			continue
		}
		at := now
		if n := d.Name(); len(n) > 16 {
			if t, err := time.Parse("20060102-150405", n[len(n)-15:]); err == nil {
				at = t.UTC().Format("2006-01-02T15:04:05.000Z")
			}
		}
		var room Room
		if ok, _ := readJSON(filepath.Join(root, "trash", d.Name(), "room.json"), &room); !ok {
			continue
		}
		res := st.exec("INSERT INTO trash(room_id, deleted_at, room) VALUES(?, ?, ?)", room.RoomID, at, js(room))
		id, err := res.LastInsertId()
		must(err)
		importRoom(filepath.Join(root, "trash", d.Name()), id)
	}
	// 멤버십, 이력, 이름, 설정
	ms := map[string]*Membership{}
	if _, err := readJSON(filepath.Join(root, "memberships.json"), &ms); err == nil && ms != nil {
		st.PutMemberships(ms)
	}
	if evs, err := readJSONL[MembershipEvent](filepath.Join(root, "memberships.log.jsonl")); err == nil {
		for _, e := range evs {
			st.AppendMembershipEvent(e)
		}
	}
	names := map[string]string{}
	if _, err := readJSON(filepath.Join(root, "names.json"), &names); err == nil {
		for id, n := range names {
			st.PutName(id, n)
		}
	}
	var cfg Config
	if ok, err := readJSON(filepath.Join(root, "config.json"), &cfg); ok && err == nil {
		st.PutConfig(cfg)
	}
	st.setMeta("legacy_imported", now)
}

// moveLegacy 는 가져온 예전 파일을 <root>/legacy-<시각>/ 으로 옮긴다. 실패해도 데이터는 이미 DB 에 있으므로 무시한다.
func moveLegacy(root string, t time.Time) string {
	dst := filepath.Join(root, "legacy-"+t.Format("20060102-150405"))
	note := "이 폴더는 ATalk 가 SQLite(atalk.db)로 바뀌기 전의 파일 데이터입니다. 내용은 모두 atalk.db 로 옮겨졌습니다.\r\n필요 없으면 지워도 됩니다.\r\n"
	moved := false
	for _, n := range legacyItems {
		src := filepath.Join(root, n)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		_ = os.MkdirAll(dst, 0o755)
		if os.Rename(src, filepath.Join(dst, n)) == nil {
			moved = true
		}
	}
	if moved {
		_ = os.WriteFile(filepath.Join(dst, "README.txt"), []byte(note), 0o644)
		return dst
	}
	return ""
}

var _ = json.Marshal

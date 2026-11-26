package core

// 저장 계층. 코어(core.go)는 상태를 메모리에 들고 있고, 바뀐 것을 Store 로 저장한다.
// 지금 구현은 SQLite (<root>/atalk.db, 순수 Go 드라이버 → 실행 파일 하나 유지).
// 나중에 서버 여러 대가 필요하면 같은 인터페이스로 Postgres 구현을 추가한다.
//
// 트랜잭션: 코어의 공개 메서드 하나(guard)가 트랜잭션 하나다. 처음 쓰기/읽기 때 시작하고 guard 가 끝날 때 확정한다.

import (
	"database/sql"
	"encoding/json"
	"strings"

	_ "modernc.org/sqlite"
)

type Store interface {
	Load() (*snapshot, error)
	Commit() error
	Rollback()
	Close() error

	PutAgent(a *AgentCard)
	DeleteAgent(id string)
	PutRoom(r *Room)
	PutMemberships(ms map[string]*Membership)
	PutCursors(room string, cur map[string]int)
	AppendMembershipEvent(e MembershipEvent)
	PutName(id, name string)
	PutConfig(cfg Config)
	PutRemoteTokens(t map[string]RemoteToken)

	AppendMessage(m Message)
	PruneMessages(room string, upTo int) // 방의 seq <= upTo 메시지와 그 처리 기록을 지운다 (메시지 한도, 선입선출)
	Messages(room string, after, limit int) []Message // limit <= 0 이면 전부
	MessagesBySeq(room string, seqs []int) map[int]Message

	AppendLog(room string, e LogEntry)
	Logs(room, agent string) []LogEntry // agent 가 "" 이면 방 전체 (시간순)
	CopyLogs(room, from, to string)

	TrashRoom(r *Room, at string) int64
	TrashInfo() (count int, bytes int64)
	EmptyTrash(before string) int // before 가 "" 이면 전부, 아니면 deleted_at < before 인 것

	BackupTo(path string) error // 일관된 복사본 (VACUUM INTO)
}

// snapshot 은 시작할 때 읽는 전체 상태.
type snapshot struct {
	agents       []*AgentCard
	rooms        []*Room
	memberships  map[string]*Membership
	cursors      map[string]map[string]int
	lastSeq      map[string]int
	names        map[string]string
	config       Config
	remoteTokens map[string]RemoteToken
	imported     bool // 예전 파일 데이터를 이미 가져왔는가
}

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS agents(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS rooms(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS memberships(agent TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS cursors(room TEXT NOT NULL, agent TEXT NOT NULL, seq INTEGER NOT NULL, PRIMARY KEY(room, agent));
CREATE TABLE IF NOT EXISTS messages(trash_id INTEGER NOT NULL DEFAULT 0, room TEXT NOT NULL, seq INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(trash_id, room, seq));
CREATE TABLE IF NOT EXISTS logs(id INTEGER PRIMARY KEY AUTOINCREMENT, trash_id INTEGER NOT NULL DEFAULT 0, room TEXT NOT NULL, agent TEXT NOT NULL,
  seq INTEGER NOT NULL, ts TEXT NOT NULL, state TEXT NOT NULL, note TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS logs_room_agent ON logs(trash_id, room, agent);
CREATE TABLE IF NOT EXISTS membership_events(id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS names(id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS trash(id INTEGER PRIMARY KEY AUTOINCREMENT, room_id TEXT NOT NULL, deleted_at TEXT NOT NULL, room TEXT NOT NULL);
`

type sqliteStore struct {
	db *sql.DB
	tx *sql.Tx
}

func openSQLite(path string) (*sqliteStore, error) {
	dsn := "file:" + strings.ReplaceAll(path, `\`, `/`) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // 데몬 하나가 직렬로 쓴다 (코어 잠금과 같음)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &sqliteStore{db: db}, nil
}

// x 는 지금 트랜잭션 (없으면 시작).
func (s *sqliteStore) x() *sql.Tx {
	if s.tx == nil {
		tx, err := s.db.Begin()
		must(err)
		s.tx = tx
	}
	return s.tx
}

func (s *sqliteStore) exec(q string, args ...any) sql.Result {
	r, err := s.x().Exec(q, args...)
	must(err)
	return r
}

func (s *sqliteStore) Commit() error {
	if s.tx == nil {
		return nil
	}
	err := s.tx.Commit()
	s.tx = nil
	return err
}

func (s *sqliteStore) Rollback() {
	if s.tx != nil {
		_ = s.tx.Rollback()
		s.tx = nil
	}
}

func (s *sqliteStore) Close() error {
	s.Rollback()
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return s.db.Close()
}

func js(v any) string {
	b, err := json.Marshal(v)
	must(err)
	return string(b)
}

func (s *sqliteStore) Load() (*snapshot, error) {
	snap := &snapshot{memberships: map[string]*Membership{}, cursors: map[string]map[string]int{}, lastSeq: map[string]int{}, names: map[string]string{}}
	tx := s.x()
	each := func(q string, fn func(*sql.Rows) error) {
		rows, err := tx.Query(q)
		must(err)
		defer rows.Close()
		for rows.Next() {
			must(fn(rows))
		}
		must(rows.Err())
	}
	each("SELECT data FROM agents", func(r *sql.Rows) error {
		var d string
		if err := r.Scan(&d); err != nil {
			return err
		}
		var a AgentCard
		if json.Unmarshal([]byte(d), &a) == nil && a.ID != "" {
			if a.Capabilities == nil {
				a.Capabilities = []string{}
			}
			snap.agents = append(snap.agents, &a)
		}
		return nil
	})
	each("SELECT data FROM rooms", func(r *sql.Rows) error {
		var d string
		if err := r.Scan(&d); err != nil {
			return err
		}
		var room Room
		if json.Unmarshal([]byte(d), &room) == nil && room.RoomID != "" {
			normalizeRoom(&room)
			snap.rooms = append(snap.rooms, &room)
		}
		return nil
	})
	each("SELECT agent, data FROM memberships", func(r *sql.Rows) error {
		var a, d string
		if err := r.Scan(&a, &d); err != nil {
			return err
		}
		var m Membership
		if json.Unmarshal([]byte(d), &m) == nil {
			snap.memberships[a] = &m
		}
		return nil
	})
	each("SELECT room, agent, seq FROM cursors", func(r *sql.Rows) error {
		var room, a string
		var seq int
		if err := r.Scan(&room, &a, &seq); err != nil {
			return err
		}
		if snap.cursors[room] == nil {
			snap.cursors[room] = map[string]int{}
		}
		snap.cursors[room][a] = seq
		return nil
	})
	each("SELECT room, MAX(seq) FROM messages WHERE trash_id = 0 GROUP BY room", func(r *sql.Rows) error {
		var room string
		var seq int
		if err := r.Scan(&room, &seq); err != nil {
			return err
		}
		snap.lastSeq[room] = seq
		return nil
	})
	each("SELECT id, name FROM names", func(r *sql.Rows) error {
		var id, n string
		if err := r.Scan(&id, &n); err != nil {
			return err
		}
		snap.names[id] = n
		return nil
	})
	each("SELECT key, value FROM meta", func(r *sql.Rows) error {
		var k, v string
		if err := r.Scan(&k, &v); err != nil {
			return err
		}
		switch k {
		case "config":
			_ = json.Unmarshal([]byte(v), &snap.config)
		case "legacy_imported":
			snap.imported = true
		case "remote_tokens":
			_ = json.Unmarshal([]byte(v), &snap.remoteTokens)
		}
		return nil
	})
	return snap, nil
}

func (s *sqliteStore) setMeta(k, v string) {
	s.exec("INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", k, v)
}

func (s *sqliteStore) PutAgent(a *AgentCard) {
	s.exec("INSERT INTO agents(id, data) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data", a.ID, js(a))
}
func (s *sqliteStore) DeleteAgent(id string) { s.exec("DELETE FROM agents WHERE id = ?", id) }
func (s *sqliteStore) PutRoom(r *Room) {
	s.exec("INSERT INTO rooms(id, data) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET data = excluded.data", r.RoomID, js(r))
}

// PutMemberships 는 멤버십 표 전체를 바꾼다 (행 수가 에이전트 수 정도로 작음).
func (s *sqliteStore) PutMemberships(ms map[string]*Membership) {
	s.exec("DELETE FROM memberships")
	for a, m := range ms {
		s.exec("INSERT INTO memberships(agent, data) VALUES(?, ?)", a, js(m))
	}
}

func (s *sqliteStore) PutCursors(room string, cur map[string]int) {
	s.exec("DELETE FROM cursors WHERE room = ?", room)
	for a, seq := range cur {
		s.exec("INSERT INTO cursors(room, agent, seq) VALUES(?, ?, ?)", room, a, seq)
	}
}

func (s *sqliteStore) AppendMembershipEvent(e MembershipEvent) {
	s.exec("INSERT INTO membership_events(ts, data) VALUES(?, ?)", e.TS, js(e))
}
func (s *sqliteStore) PutName(id, name string) {
	s.exec("INSERT INTO names(id, name) VALUES(?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name", id, name)
}
func (s *sqliteStore) PutConfig(cfg Config)                     { s.setMeta("config", js(cfg)) }
func (s *sqliteStore) PutRemoteTokens(t map[string]RemoteToken) { s.setMeta("remote_tokens", js(t)) }

func (s *sqliteStore) PruneMessages(room string, upTo int) {
	s.exec("DELETE FROM messages WHERE trash_id = 0 AND room = ? AND seq <= ?", room, upTo)
	s.exec("DELETE FROM logs WHERE trash_id = 0 AND room = ? AND seq <= ?", room, upTo)
}

func (s *sqliteStore) AppendMessage(m Message) {
	s.exec("INSERT INTO messages(trash_id, room, seq, data) VALUES(0, ?, ?, ?)", m.Room, m.Seq, js(m))
}

func scanMessages(rows *sql.Rows) []Message {
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var d string
		must(rows.Scan(&d))
		var m Message
		if json.Unmarshal([]byte(d), &m) == nil {
			if m.To == nil {
				m.To = []string{}
			}
			out = append(out, m)
		}
	}
	must(rows.Err())
	return out
}

func (s *sqliteStore) Messages(room string, after, limit int) []Message {
	q := "SELECT data FROM messages WHERE trash_id = 0 AND room = ? AND seq > ? ORDER BY seq"
	args := []any{room, after}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.x().Query(q, args...)
	must(err)
	return scanMessages(rows)
}

func (s *sqliteStore) MessagesBySeq(room string, seqs []int) map[int]Message {
	out := map[int]Message{}
	if len(seqs) == 0 {
		return out
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(seqs)), ",")
	args := []any{room}
	for _, n := range seqs {
		args = append(args, n)
	}
	rows, err := s.x().Query("SELECT data FROM messages WHERE trash_id = 0 AND room = ? AND seq IN ("+ph+")", args...)
	must(err)
	for _, m := range scanMessages(rows) {
		out[m.Seq] = m
	}
	return out
}

func (s *sqliteStore) AppendLog(room string, e LogEntry) {
	s.exec("INSERT INTO logs(trash_id, room, agent, seq, ts, state, note) VALUES(0, ?, ?, ?, ?, ?, ?)", room, e.Agent, e.Seq, e.TS, e.State, e.Note)
}

func (s *sqliteStore) Logs(room, agent string) []LogEntry {
	q := "SELECT agent, seq, ts, state, note FROM logs WHERE trash_id = 0 AND room = ?"
	args := []any{room}
	if agent != "" {
		q += " AND agent = ?"
		args = append(args, agent)
	}
	rows, err := s.x().Query(q+" ORDER BY ts, id", args...)
	must(err)
	defer rows.Close()
	out := []LogEntry{}
	for rows.Next() {
		var e LogEntry
		must(rows.Scan(&e.Agent, &e.Seq, &e.TS, &e.State, &e.Note))
		out = append(out, e)
	}
	must(rows.Err())
	return out
}

func (s *sqliteStore) CopyLogs(room, from, to string) {
	s.exec(`INSERT INTO logs(trash_id, room, agent, seq, ts, state, note)
		SELECT 0, room, ?, seq, ts, state, note FROM logs WHERE trash_id = 0 AND room = ? AND agent = ? ORDER BY id`, to, room, from)
}

// TrashRoom 은 방을 휴지통으로 옮긴다: 방 정보는 trash 행으로, 메시지·처리 기록은 trash_id 를 붙여 남긴다.
// 같은 방 ID 로 새 방을 만들어도 겹치지 않는다.
func (s *sqliteStore) TrashRoom(r *Room, at string) int64 {
	res := s.exec("INSERT INTO trash(room_id, deleted_at, room) VALUES(?, ?, ?)", r.RoomID, at, js(r))
	id, err := res.LastInsertId()
	must(err)
	s.exec("UPDATE messages SET trash_id = ? WHERE trash_id = 0 AND room = ?", id, r.RoomID)
	s.exec("UPDATE logs SET trash_id = ? WHERE trash_id = 0 AND room = ?", id, r.RoomID)
	s.exec("DELETE FROM cursors WHERE room = ?", r.RoomID)
	s.exec("DELETE FROM rooms WHERE id = ?", r.RoomID)
	return id
}

func (s *sqliteStore) TrashInfo() (count int, bytes int64) {
	tx := s.x()
	must(tx.QueryRow("SELECT COUNT(*), COALESCE(SUM(LENGTH(room)), 0) FROM trash").Scan(&count, &bytes))
	var mb int64
	must(tx.QueryRow("SELECT COALESCE(SUM(LENGTH(data)), 0) FROM messages WHERE trash_id > 0").Scan(&mb))
	return count, bytes + mb
}

func (s *sqliteStore) EmptyTrash(before string) int {
	cond, args := "", []any{}
	if before != "" {
		cond, args = " WHERE deleted_at < ?", []any{before}
	}
	sub := "SELECT id FROM trash" + cond
	s.exec("DELETE FROM messages WHERE trash_id IN ("+sub+")", args...)
	s.exec("DELETE FROM logs WHERE trash_id IN ("+sub+")", args...)
	n, err := s.exec("DELETE FROM trash"+cond, args...).RowsAffected()
	must(err)
	return int(n)
}

func (s *sqliteStore) BackupTo(path string) error {
	if s.tx != nil { // VACUUM 은 트랜잭션 밖에서만
		if err := s.Commit(); err != nil {
			return err
		}
	}
	_, err := s.db.Exec("VACUUM INTO ?", path)
	return err
}

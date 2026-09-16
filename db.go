package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps SQLite.
//
// Pool size is small but deliberately greater than one. A single connection
// looks safest for SQLite, but it turns any accidental s.db.sql.* call made
// while a transaction is open into a permanent, server-wide deadlock (the tx
// holds the only connection and the nested call queues behind it forever).
// We hit exactly that three times. Four connections plus WAL + busy_timeout
// keeps writes serialised by SQLite itself while making a regression degrade
// into a stale read instead of an outage.
//
// The rule still applies: never call s.db.sql.* between Begin and Commit.
type DB struct {
	sql *sql.DB
}

func openDB(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(0)"
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(4)
	sdb.SetMaxIdleConns(4)
	sdb.SetConnMaxLifetime(0)
	if err := sdb.Ping(); err != nil {
		return nil, err
	}
	d := &DB{sql: sdb}
	if err := d.migrate(); err != nil {
		return nil, err
	}
	return d, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS users(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  pass_hash TEXT NOT NULL,
  is_admin INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens(
  token TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS devices(
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  platform TEXT NOT NULL DEFAULT '',
  last_seen_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS calendars(
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  color TEXT NOT NULL DEFAULT '#378ADD',
  kind TEXT NOT NULL DEFAULT 'private',
  seq INTEGER NOT NULL DEFAULT 0,
  field_seqs TEXT NOT NULL DEFAULT '{}',
  updated_at INTEGER NOT NULL DEFAULT 0,
  updated_by_device TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0,
  deleted_at INTEGER
);
CREATE TABLE IF NOT EXISTS calendar_members(
  calendar_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'editor',
  PRIMARY KEY(calendar_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_calmembers_user ON calendar_members(user_id);
CREATE TABLE IF NOT EXISTS events(
  id TEXT PRIMARY KEY,
  calendar_id TEXT NOT NULL,
  uid TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  notes TEXT NOT NULL DEFAULT '',
  location TEXT NOT NULL DEFAULT '',
  all_day INTEGER NOT NULL DEFAULT 0,
  dtstart_utc INTEGER,
  duration_sec INTEGER NOT NULL DEFAULT 0,
  tzid TEXT NOT NULL DEFAULT 'Asia/Shanghai',
  rrule TEXT,
  rdate TEXT NOT NULL DEFAULT '[]',
  exdate TEXT NOT NULL DEFAULT '[]',
  is_task INTEGER NOT NULL DEFAULT 0,
  done_at INTEGER,
  seq INTEGER NOT NULL DEFAULT 0,
  field_seqs TEXT NOT NULL DEFAULT '{}',
  updated_at INTEGER NOT NULL DEFAULT 0,
  updated_by_device TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0,
  deleted_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_events_cal ON events(calendar_id);
CREATE TABLE IF NOT EXISTS event_overrides(
  event_id TEXT NOT NULL,
  occurrence_key INTEGER NOT NULL,
  patch TEXT NOT NULL DEFAULT '{}',
  cancelled INTEGER NOT NULL DEFAULT 0,
  seq INTEGER NOT NULL DEFAULT 0,
  field_seqs TEXT NOT NULL DEFAULT '{}',
  updated_at INTEGER NOT NULL DEFAULT 0,
  updated_by_device TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(event_id, occurrence_key)
);
CREATE TABLE IF NOT EXISTS reminders(
  id TEXT PRIMARY KEY,
  event_id TEXT NOT NULL,
  minutes_before INTEGER NOT NULL DEFAULT 0,
  channel TEXT NOT NULL DEFAULT 'notify',
  seq INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  deleted_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_rem_event ON reminders(event_id);
-- 个人提醒表态表：记「这个人就这条日程的提醒表过态」。
-- 单看 reminders 表分不出"明确要 0 条"和"从没表态"，因为两者都查不到行。
CREATE TABLE IF NOT EXISTS reminder_prefs(
  event_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(event_id, user_id)
);
CREATE TABLE IF NOT EXISTS instances(
  id TEXT PRIMARY KEY,
  event_id TEXT NOT NULL,
  occurrence_key INTEGER NOT NULL,
  calendar_id TEXT NOT NULL,
  start_utc INTEGER NOT NULL,
  end_utc INTEGER NOT NULL,
  all_day INTEGER NOT NULL DEFAULT 0,
  title TEXT NOT NULL DEFAULT '',
  notes TEXT NOT NULL DEFAULT '',
  location TEXT NOT NULL DEFAULT '',
  is_task INTEGER NOT NULL DEFAULT 0,
  done_at INTEGER,
  is_override INTEGER NOT NULL DEFAULT 0,
  seq INTEGER NOT NULL DEFAULT 0,
  deleted_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_inst_start ON instances(start_utc);
CREATE INDEX IF NOT EXISTS idx_inst_event ON instances(event_id);
CREATE TABLE IF NOT EXISTS snoozes(
  id TEXT PRIMARY KEY,
  event_id TEXT NOT NULL,
  occurrence_key INTEGER NOT NULL,
  user_id TEXT NOT NULL,
  remind_at_utc INTEGER NOT NULL,
  device_id TEXT NOT NULL DEFAULT '',
  seq INTEGER NOT NULL DEFAULT 0,
  deleted_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_snooze_ev ON snoozes(event_id);
CREATE TABLE IF NOT EXISTS conflicts(
  id TEXT PRIMARY KEY,
  entity TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  field TEXT NOT NULL,
  base_seq INTEGER NOT NULL DEFAULT 0,
  server_value TEXT,
  client_value TEXT,
  client_device TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  resolved_at INTEGER,
  resolution TEXT
);
CREATE TABLE IF NOT EXISTS sync_log(
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  entity TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  op TEXT NOT NULL,
  changed_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_synclog_entity ON sync_log(entity, entity_id);
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS api_keys(
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  key_hash TEXT NOT NULL UNIQUE,
  prefix TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  revoked_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_apikeys_hash ON api_keys(key_hash);
CREATE TABLE IF NOT EXISTS app_settings(
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL,
  updated_at INTEGER NOT NULL DEFAULT 0
);
-- 强提醒（per-event / per-user）：把"这条日程要强提醒"这个**用户意图**存下来，
-- 好让手机和电脑看到同一个设置。
--
-- ① 为什么带 on_off 列，而不是像 reminder_prefs 那样"有行即表过态"：
--    reminder_prefs 的存在性代表"这个人对提醒表过态了"（至于几条，看 reminders）。
--    强提醒**本身就是个布尔值**，on / off 都是有效状态，用列存最直接。
-- ② 为什么没有 seq 列：对端靠 **event 行自己的 seq** 拉到整条日程，再读它的
--    strong（和 reminders 同理）。所以写强提醒必须搭 POST/PUT /api/events 的车，
--    不要另开独立接口 —— 开了就得自己抬 seq，漏了就"改了别的设备永远不知道"。
-- ③ 服务端只存**意图**，不存实现：安卓写系统日历、桌面弹置顶提示栏、手表响铃震动，
--    各端能力不同，这个判断不属于服务端。
CREATE TABLE IF NOT EXISTS strong_reminders(
  event_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  on_off INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(event_id, user_id)
);
`

func (d *DB) migrate() error {
	if _, err := d.sql.Exec(schema); err != nil {
		return err
	}
	// Columns added after the first release. CREATE TABLE IF NOT EXISTS cannot
	// evolve a table that already exists, and this server ships to devices
	// that already hold real data, so additive changes land here.
	d.ensureColumn("devices", "version", "TEXT NOT NULL DEFAULT ''")
	d.ensureColumn("calendars", "sort_order", "INTEGER NOT NULL DEFAULT 0")
	d.ensureColumn("calendars", "hidden", "INTEGER NOT NULL DEFAULT 0")
	// 个人提醒：提醒从"事件的属性"改成"每个用户自己的属性"。
	// 老行的 user_id 补成 ''，语义是"共享默认"—— 只有本人没有专属行时才生效，
	// 所以老客户端（不认这个字段）看到的行为与升级前完全一致。
	d.ensureColumn("reminders", "user_id", "TEXT NOT NULL DEFAULT ''")
	d.repairSharedReminderOwners()
	d.repairTaskFlags()
	return nil
}

// repairSharedReminderOwners 把 user_id 空着的提醒行认领给事件创建者。
//
// 为什么要做：升级前提醒挂在事件上，改提醒是"谁都能改全体"。升级后
// user_id=” 保住了老客户端的行为，但创建者自己去编辑那条日程时，服务端
// 会把他的改动写成专属行 —— 于是同一条日程上会同时存在"共享默认 15 分钟"
// 和"我的 30 分钟"，本人看到的是自己的那份、别人看到旧的那份，很难解释。
// 启动时把无主的行交给创建者，事件从此只有"个人的提醒"这一种语义。
func (d *DB) repairSharedReminderOwners() {
	rows, err := d.sql.Query(`
		SELECT e.id, e.uid FROM events e
		WHERE EXISTS(SELECT 1 FROM reminders r WHERE r.event_id=e.id AND r.user_id='')
		  AND e.uid <> ''`)
	if err != nil {
		log.Printf("migrate: looking for ownerless reminders failed: %v", err)
		return
	}
	type row struct{ eventID, uid string }
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.eventID, &r.uid); err == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	if len(list) == 0 {
		return
	}
	fixed := 0
	for _, r := range list {
		if _, err := d.sql.Exec(
			`UPDATE reminders SET user_id=? WHERE event_id=? AND user_id=''`, r.uid, r.eventID); err != nil {
			log.Printf("migrate: claiming reminders of %s failed: %v", r.eventID, err)
			continue
		}
		if _, err := d.sql.Exec(
			`INSERT INTO reminder_prefs(event_id,user_id,updated_at) VALUES(?,?,?)
			 ON CONFLICT(event_id,user_id) DO NOTHING`, r.eventID, r.uid, time.Now().Unix()); err != nil {
			log.Printf("migrate: marking pref of %s failed: %v", r.eventID, err)
			continue
		}
		fixed++
	}
	log.Printf("migrate: claimed reminders of %d event(s) for their owners", fixed)
}

// repairTaskFlags rebuilds the instances of events whose is_task no longer
// matches the event row.
//
// 0.2.2 fixed a SELECT in recomputeInstances that left is_task out of its
// column list. The bug was silent (the scan targets still had a slot for it),
// so every instance written before 0.2.2 claims is_task=0 no matter what the
// event says — and clients read instances, so a task showed up as an ordinary
// all-day entry with nothing to tick.
//
// Instances are only rebuilt when their event is touched, which would leave
// every existing task broken until somebody happened to edit it. Hence a sweep
// at startup. It goes through recomputeInstances rather than a raw UPDATE
// because the rebuild is what allocates fresh seqs — a bare UPDATE would
// correct the database and leave every device holding the stale copy.
func (d *DB) repairTaskFlags() {
	// Collect the ids first, then open a transaction per event. Querying while
	// a transaction is open is the deadlock described on the DB type.
	rows, err := d.sql.Query(`
		SELECT DISTINCT e.id FROM events e
		JOIN instances i ON i.event_id = e.id
		WHERE i.deleted_at IS NULL AND i.is_task != e.is_task`)
	if err != nil {
		log.Printf("migrate: looking for drifted task flags failed: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return
	}

	fixed := 0
	for _, id := range ids {
		tx, err := d.sql.Begin()
		if err != nil {
			log.Printf("migrate: repair %s: begin failed: %v", id, err)
			continue
		}
		if err := recomputeInstances(tx, id, time.Now()); err != nil {
			_ = tx.Rollback()
			log.Printf("migrate: repair %s failed: %v", id, err)
			continue
		}
		if err := tx.Commit(); err != nil {
			log.Printf("migrate: repair %s commit failed: %v", id, err)
			continue
		}
		fixed++
	}
	log.Printf("migrate: rebuilt %d event(s) whose task flag had drifted", fixed)
}

// ensureColumn adds a column when it is missing, and stays quiet when it is
// already there (which is the normal case on every restart).
func (d *DB) ensureColumn(table, column, decl string) {
	_, err := d.sql.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		log.Printf("migrate: adding %s.%s failed: %v", table, column, err)
	}
}

// ---------- app settings ----------

func (s *Server) setting(key, def string) string {
	var v string
	if err := s.db.sql.QueryRow(`SELECT v FROM app_settings WHERE k=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Server) setSetting(key, value string) error {
	_, err := s.db.sql.Exec(`
		INSERT INTO app_settings(k,v,updated_at) VALUES(?,?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v, updated_at=excluded.updated_at`,
		key, value, time.Now().Unix())
	return err
}

// ---------- seq ----------

func nextSeq(tx *sql.Tx, entity, id, op string) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO sync_log(entity, entity_id, op, changed_at) VALUES(?,?,?,?)`,
		entity, id, op, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) currentSeq() (int64, error) {
	var s sql.NullInt64
	err := d.sql.QueryRow(`SELECT MAX(seq) FROM sync_log`).Scan(&s)
	if err != nil {
		return 0, err
	}
	if !s.Valid {
		return 0, nil
	}
	return s.Int64, nil
}

// ---------- meta ----------

func (d *DB) getMeta(k string) (string, error) {
	var v string
	err := d.sql.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (d *DB) setMeta(k, v string) error {
	_, err := d.sql.Exec(`INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// ---------- field_seqs helpers ----------

func decodeFieldSeqs(s string) jsonMap {
	m := jsonMap{}
	if strings.TrimSpace(s) == "" {
		return m
	}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func encodeFieldSeqs(m jsonMap) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// fieldSeq returns the recorded seq for a single field (0 = never written).
func fieldSeq(m jsonMap, field string) int64 {
	if v, ok := m.int64Of(field); ok {
		return v
	}
	return 0
}

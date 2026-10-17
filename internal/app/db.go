package app

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

// withTx 跑一个事务：回调返回 nil 就提交，返回错误就回滚。
//
// 为什么加这个：仓库里到处是 `tx, err := s.db.sql.Begin()` + `defer tx.Rollback()`
// + `tx.Commit()` 的三件套 —— 漏掉哪一个都会出问题（漏 Rollback 会挂住连接、
// 漏 Commit 会**静默丢写入**）。新代码统一用它，少一个出错的地方。
//
// ⚠️ 回调里**不要**调 `s.db.sql.*` —— 那会绕过事务、用的是另一条连接，
// 见 DB 结构体上那段注释（为了这个死锁过三次）。要查就查 `tx`。
func (s *Server) withTx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.sql.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
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
-- 实时协作票据（2026-10-10）。
-- ⚠️ **这张表已经不用了**：票据后来改成 HMAC 自包含签名（因为协作服务端
--    在**另一台服务器**上，没法读这个库）。保留建表语句只是为了让老库不报错，
--    新库建出来也是空的。见 live_ticket.go 顶部那段说明。
--    留着它的另一个原因：将来如果想加"主动吊销票据"，这张表就是现成的落点。
CREATE TABLE IF NOT EXISTS live_tickets(
  ticket TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
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
-- 收件箱（2026-09-19，跨端方案第九节）。
--
-- **这是唯一一个"以用户为中心"的实体**：按 user_id = 自己 过滤，
-- **不是按日历权限**。所以它的可见性判定和其它实体完全不同 ——
-- 在 loadEntities / preflight 里都是 「user_id == uid」，不碰 calendarRole。
--
-- 两类 kind：
--   'share'   —— 别人把一条行程**只读投递**给我（发件人需对源日历有读权限）
--   'capture' —— 本机（通知/识图/文件）抓到的候选行程**同步上来**，
--                此时 to_user_id 必须等于自己，否则 403
--
-- 为什么提醒要存在这里：候选条目**还不是 event**，
-- 但它要能把"提前多久提醒"这个意图带到确认那一刻，所以先存成 JSON 数组。
-- ⚠️ 空数组 = 不提醒，**不许 omitempty**（红线 18）。
CREATE TABLE IF NOT EXISTS inbox_items(
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  source TEXT NOT NULL DEFAULT '',
  from_user_id TEXT NOT NULL DEFAULT '',
  from_name TEXT NOT NULL DEFAULT '',
  origin_calendar_id TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  notes TEXT NOT NULL DEFAULT '',
  location TEXT NOT NULL DEFAULT '',
  all_day INTEGER NOT NULL DEFAULT 0,
  dtstart_utc INTEGER NOT NULL DEFAULT 0,
  duration_sec INTEGER NOT NULL DEFAULT 0,
  remind_minutes_before TEXT NOT NULL DEFAULT '[]',
  source_excerpt TEXT NOT NULL DEFAULT '',
  confidence TEXT NOT NULL DEFAULT '',
  accepted_event_id TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  decided_at INTEGER NOT NULL DEFAULT 0,
  seq INTEGER NOT NULL DEFAULT 0,
  field_seqs TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_inbox_user ON inbox_items(user_id, status);
-- 邀请（2026-09-19，跨端方案 §9.3）。
--
-- kind 一期就留出来：'invite' 是"加入某张日历"，
-- 'readonly' 是二期的"只读链接"，复用同一张表（那时 calendar_id 可为空）。
--
-- 为什么不把全站邀请码下放给普通成员：「GET /api/invite」 是**全站级注册凭证**，
-- 下放等于"任何成员都能往这个部署里放人进来"。所以邀请必须**绑死日历 + 预授权角色**。
CREATE TABLE IF NOT EXISTS invites(
  id TEXT PRIMARY KEY,
  code TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL DEFAULT 'invite',
  calendar_id TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL,
  created_by TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  max_uses INTEGER NOT NULL DEFAULT 1,
  used_count INTEGER NOT NULL DEFAULT 0,
  revoked_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_invites_code ON invites(code);
-- ===================== 团队（2026-09-21）=====================
--
-- 为什么要有团队：原来的共享是"把日历一个个分享给一个个具体的人"。
-- 一个班级/一个社团要同步一张课表，得把三十个人挨个加一遍 —— 加人是
-- 逐人的、要 canManage 的、而且每加一次都要重挂同步流（红线 19）。
--
-- 团队把这件事变成"日历共享给团队一次"。新同学通过**团队邀请码**自己进来，
-- 不需要任何人再操作一次日历成员。
--
-- ★ 三条设计红线的落点：
--   ① **团队身份绝不等于日历管理权**。团队共享给日历带来的是**内容可见性**，
--      角色上限是 editor（见 acl.go 的 teamRoleCeiling）——
--      **永远不会**通过团队拿到 owner/admin。所以"团队管理员"管的是团队，
--      碰不到别人日历的成员名单。
--   ② **只能看到自己已加入的团队**（见 teams.go 的 handleListTeams 的 WHERE）。
--      全服团队列表会泄漏"这个部署里有哪些组织"，是明确的隐私问题。
--   ③ 共享给团队必须**双方都满足**：我对这张日历 canManage，且**我是这个团队的成员**。
--      否则任何人都能把别人的日历投进任意团队。
CREATE TABLE IF NOT EXISTS teams(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
-- 团队成员。role 只有两档：'admin'（团队管理员）/ 'member'（普通成员）。
-- ⚠️ 这两个角色**只在团队语境里有意义**，不要和日历角色（owner/admin/editor/viewer）
-- 混用 —— 名字撞了但语义不同，混着写迟早出错。
CREATE TABLE IF NOT EXISTS team_members(
  team_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'member',
  joined_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(team_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_team_members_user ON team_members(user_id);
-- 团队邀请码。和日历邀请码（invites 表）分开：
-- 那张表的 role 是日历角色，这张的是团队角色，混用会让"邀请进团队"变成
-- "邀请进某个日历的某个角色"。
CREATE TABLE IF NOT EXISTS team_invites(
  id TEXT PRIMARY KEY,
  code TEXT NOT NULL UNIQUE,
  team_id TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'member',
  created_by TEXT NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  max_uses INTEGER NOT NULL DEFAULT 0,   -- 0 = 不限次数（团队邀请通常要给很多人）
  used_count INTEGER NOT NULL DEFAULT 0,
  revoked_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_team_invites_code ON team_invites(code);
-- 日历 ↔ 团队。一张日历可以共享给**多个**团队；一个团队也能看到多张日历。
CREATE TABLE IF NOT EXISTS calendar_teams(
  calendar_id TEXT NOT NULL,
  team_id TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'viewer',   -- 团队能拿到的角色上限，服务端强制 ≤ editor
  shared_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(calendar_id, team_id)
);
CREATE INDEX IF NOT EXISTS idx_calendar_teams_team ON calendar_teams(team_id);

-- ===========================================================================
-- 违禁词（内容审核）
-- ===========================================================================
--
-- 为什么落库而不是硬编码：词表要能**随时增删**。硬编码意味着"加一个词就得
-- 重新编译 + 重新部署服务端"，而审核这件事的反应速度要求恰恰很高。
-- 代码里只留一份**内置初始词**（见 moderation_seed.go），首次启动灌进来，
-- 之后由管理员在后台里维护 —— 内置的那份**只在词表为空时**用，不会覆盖
-- 管理员的增删（否则重启一次就把人改的冲掉了）。
--
-- word 存**归一化后**的形式（去空白、去常见分隔符、统一大小写/全角半角）。
-- 匹配时把待检文本做同样归一化再查 —— 这样"法 轮 功"、"法*轮*功"这类
-- 靠插字符绕过的写法会被同一套规则挡住。原始写法留在 raw 里，便于后台显示。
--
-- severity 三档（2026-09-23 加第 3 档）：
--   1 = 提醒   —— 只提示用户，放行
--   2 = 拦截   —— 直接不让保存
--   3 = 风险   —— **不拦，转交 AI 复核**（AI 说违规才拦）
--
-- 为什么需要第 3 档：词表匹配只能抓字面量，对三种情况无能为力 ——
-- ① 生僻字/异体字/谐音/拆字（「珐輪功」）；② 一个敏感词都没有但整体违规
-- （拉人进群赌钱）；③ 词本身有正常用法、一刀切会误伤（「笑气」「接码」）。
-- 这类词放进第 3 档，由 AI 理解语义后再定性。见 moderation_ai.go。
CREATE TABLE IF NOT EXISTS banned_words(
  id TEXT PRIMARY KEY,
  word TEXT NOT NULL,                    -- 归一化后的形式，匹配用
  raw TEXT NOT NULL DEFAULT '',          -- 管理员输入的原样，展示用
  category TEXT NOT NULL DEFAULT 'other',-- porn/politics/violence/gang/other
  severity INTEGER NOT NULL DEFAULT 1,   -- 1=提醒 2=拦截 3=风险（转 AI）
  enabled INTEGER NOT NULL DEFAULT 1,
  note TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_banned_words_word ON banned_words(word);
CREATE INDEX IF NOT EXISTS idx_banned_words_cat ON banned_words(category, enabled);

-- ===========================================================================
-- 举报（用户举报违规内容，管理员处理）
-- ===========================================================================
--
-- 为什么要**快照**（target_title / target_excerpt / target_owner）：
-- 举报处理是有延迟的，期间被举报的内容**可能已被作者改掉或删掉**。
-- 如果处理页只按 id 去现查，那时要么查不到（举报变成"无法处理"），
-- 要么看到的是改后的内容（管理员据此判"没问题"，而举报人看到的确实是违规的）。
-- 快照 = 举报当时看到的样子，这才是处理依据。
--
-- target_kind/calendar_id/event_id 只用于**跳转定位**，不作为判据。
CREATE TABLE IF NOT EXISTS reports(
  id TEXT PRIMARY KEY,
  reporter_id TEXT NOT NULL,
  reporter_name TEXT NOT NULL DEFAULT '',
  target_kind TEXT NOT NULL DEFAULT 'event',   -- event/calendar/user/other
  target_id TEXT NOT NULL DEFAULT '',
  target_title TEXT NOT NULL DEFAULT '',       -- 快照
  target_excerpt TEXT NOT NULL DEFAULT '',     -- 快照
  target_owner TEXT NOT NULL DEFAULT '',       -- 被举报内容的作者（快照）
  calendar_id TEXT NOT NULL DEFAULT '',
  event_id TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT 'other',        -- porn/politics/violence/gang/abuse/spam/other
  detail TEXT NOT NULL DEFAULT '',             -- 举报人补充说明
  status TEXT NOT NULL DEFAULT 'pending',      -- pending/accepted/rejected
  handler_id TEXT NOT NULL DEFAULT '',
  handler_name TEXT NOT NULL DEFAULT '',
  handle_note TEXT NOT NULL DEFAULT '',        -- 处理意见，会回给举报人
  action TEXT NOT NULL DEFAULT '',             -- 实际动作：none/content_deleted/...
  created_at INTEGER NOT NULL,
  handled_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_reports_status ON reports(status, created_at);
CREATE INDEX IF NOT EXISTS idx_reports_reporter ON reports(reporter_id);

-- ===========================================================================
-- AI 内容复核的审计记录（2026-09-23）
-- ===========================================================================
--
-- 为什么**连"判合规"的也记**：判合规的那些恰恰是漏放的高发区。
-- 只记"判违规"的话，事后复盘永远只能看到已经拦下来的那些，
-- 而真正的盲区（本该拦却放过去的）一条记录都没有。
--
-- 为什么连「source」「error_text」「model」「elapsed_ms」都存：
-- ① "今天一条都没拦"到底是内容干净还是 AI 压根没调通？看 source 立刻能分辨；
-- ② 模型和提示词都会变，事后要能回答"当时是按什么条件判的"；
-- ③ 耗时不正常地长说明每次都在接近超时，是要调超时还是换模型的依据。
--
-- excerpt 截 300 字：够复盘，又不至于把整张日程表复制一遍。
-- ⚠️ **绝不存 API Key**（任何形态、任何长度）。
CREATE TABLE IF NOT EXISTS moderation_audit(
  id TEXT PRIMARY KEY,
  field TEXT NOT NULL DEFAULT '',        -- 命中的字段名（标题/备注/地点）
  excerpt TEXT NOT NULL DEFAULT '',      -- 送审文本（截断）
  hits TEXT NOT NULL DEFAULT '',         -- 词表命中的原词，用 | 连接
  violation INTEGER NOT NULL DEFAULT 0,
  category TEXT NOT NULL DEFAULT 'other',
  confidence REAL NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',       -- AI 给的理由
  source TEXT NOT NULL DEFAULT '',       -- model / disabled / error
  error_text TEXT NOT NULL DEFAULT '',   -- 失败原因（已脱敏）
  model TEXT NOT NULL DEFAULT '',
  elapsed_ms INTEGER NOT NULL DEFAULT 0,
  user_id TEXT NOT NULL DEFAULT '',
  user_name TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_modaudit_time ON moderation_audit(created_at);
CREATE INDEX IF NOT EXISTS idx_modaudit_viol ON moderation_audit(violation, created_at);

-- ===========================================================================
-- 登录与账号事件日志（2026-09-23）
-- ===========================================================================
--
-- 为什么要落库而不是只写文件日志：文件日志要上服务器 grep，而"谁在什么时候
-- 从哪个 IP 登的"是**日常会问**的问题（设备丢了要查、异常登录要查、
-- 账号被共享要查）。落库才能让管理员在后台直接筛、直接分页。
-- 同时**仍然写一份文件**（data/logs/login-YYYY-MM-DD.jsonl）：
-- 库里的会被保留期清理掉，文件是把证据留长一点的那一份（也是取证时最方便的）。
--
-- 「ip」与「real_ip」两个字段是有意的：
--   - real_ip：从 X-Forwarded-For / X-Real-IP 解析出来的**客户端声称的**来源。
--     反代后面拿到的直连地址永远是 127.0.0.1，光有它等于没记。
--   - ip：本次 TCP 连接的**对端地址**。
-- 两者都留着，才能分辨"确实来自这个 IP"和"它自称来自这个 IP"。
-- ⚠️ **不信任 XFF**：那张头是可以随便伪造的，所以它只进 real_ip 供参考，
--    任何权限判定都不许读它。
--
-- ua / device 分开存：ua 是原始字符串（很长、格式混乱，但不可篡改），
-- device 是从令牌带过来的设备标识（客户端自己报的，用于"这台设备是我吗"）。
--
-- result 用 success/fail 两值而不是布尔：以后加"锁定的账号"这类
-- 第三种结果时不用改表结构（那个字段将来会加）。
CREATE TABLE IF NOT EXISTS login_logs(
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL DEFAULT '',
  user_name TEXT NOT NULL DEFAULT '',
  event TEXT NOT NULL DEFAULT 'login',   -- register/login/logout/login_failed/password_change
  result TEXT NOT NULL DEFAULT 'success',-- success/fail
  reason TEXT NOT NULL DEFAULT '',       -- 失败原因（人话，不含敏感信息）
  ip TEXT NOT NULL DEFAULT '',           -- TCP 对端地址
  real_ip TEXT NOT NULL DEFAULT '',      -- 从 XFF/X-Real-IP 解析（仅供展示）
  ua TEXT NOT NULL DEFAULT '',
  device TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_loginlog_time ON login_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_loginlog_user ON login_logs(user_id, created_at);
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
	// 封禁账号（2026-10-09）。**必须走这里，不能只写进上面的 schema** ——
	// `CREATE TABLE IF NOT EXISTS` 对已存在的表是**静默跳过**的，而这个
	// 服务端要发到"已经有真实数据"的机器上（线上那张 users 表就是老表）。
	// 只写 schema 的症状是"新库有列、老库没有"，一读就报 no such column。
	//
	// 四个字段而不是一个布尔：封禁是**争议裁决**，跟举报处理一样要留痕
	//（谁封的、为什么、什么时候）。解封时把 banned 置 0，理由留着 ——
	// "这个人被封过"本身就是有用的历史。
	d.ensureColumn("users", "banned", "INTEGER NOT NULL DEFAULT 0")
	d.ensureColumn("users", "ban_reason", "TEXT NOT NULL DEFAULT ''")
	d.ensureColumn("users", "banned_at", "INTEGER NOT NULL DEFAULT 0")
	d.ensureColumn("users", "banned_by", "TEXT NOT NULL DEFAULT ''")
	// 封禁到期时刻（2026-10-09 加）。**0 = 永久封禁** —— 用 0 而不是 0 或 NULL 的
	// 双语义：只有一个"没有期限"的表示，判断就永远只有一句
	//（`until > 0 && until <= now`），不会出现"NULL 与 0 到底哪个是永久"的分歧。
	d.ensureColumn("users", "ban_until", "INTEGER NOT NULL DEFAULT 0")
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

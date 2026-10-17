package panel

// 从旧版本迁移业务数据。
//
// 背景：旧版服务端（单独一个 exe）和新版（管理器 + 服务核心）用的是**同一份
// 表结构** —— 两份 db.go 的建表语句逐字相同，新版只在末尾多加了联动相关的
// 两张表。所以迁移在数据层面就是"把库换过去"，新版启动时会自己补上新增的列。
//
// 但"能换"和"敢换"是两件事。这里做的所有事情都是为了后者：
//
//  1. **先看再动**：把上传的库读一遍，报出账号数、日程数、时间范围。
//     看不到这些东西就点"确定"，等于闭着眼睛按下去。
//  2. **两份备份**：当前库复制一份进 backups/，上传的库也留一份。
//     换错了还能回去，而且回得去的是**两份不同的东西**：
//     "换之前的样子"和"换进来的那份"。
//  3. **换完对数**：重新打开新库数一遍，和迁移前报的数字对得上才算完成。
//     对不上不会自动回滚（那会让用户刚看到的数据又消失一次），
//     而是把差异明确写出来。
//
// ⚠️ 这里的读库**只读上传上来的那份临时文件**，不碰正在用的库。
//    换库动作必须停服务 —— 这是唯一能让 WAL / 连接池都不打架的做法。

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// dbStats 是一份数据库的"体检结果"。字段名直接给前端用。
type dbStats struct {
	Size       int64 `json:"size"`
	Users      int64 `json:"users"`
	Calendars  int64 `json:"calendars"`
	Events     int64 `json:"events"`
	Devices    int64 `json:"devices"`
	Reminders  int64 `json:"reminders"`
	InboxItems int64 `json:"inbox_items"`
	// FirstEvent / LastEvent 是这个库里日程覆盖的时间范围（epoch 秒）。
	// 它是"这是不是我要迁的那份库"最直观的证据 —— 数字对不上就别往下走。
	FirstEvent int64 `json:"first_event"`
	LastEvent  int64 `json:"last_event"`

	Tables []string `json:"tables"`
	// Missing 列出缺失的必需表。空 = 结构认得。
	Missing    []string `json:"missing"`
	Compatible bool     `json:"compatible"`
	// SchemaNote 是一句人话的结论，直接显示在界面上。
	SchemaNote string `json:"schema_note"`
}

var requiredTables = []string{
	"users", "calendars", "calendar_members", "events", "devices", "sync_log", "meta",
}

// inspectDB 读一份 SQLite 文件并统计。
//
// ⚠️ 传进来的必须是**临时副本**：这里用读写模式打开，让 SQLite 能重放
// 随文件一起带过来的 WAL。对正在使用的库这么干是不行的。
func inspectDB(path string) (*dbStats, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(3000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}

	out := &dbStats{Size: st.Size()}
	set := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			set[n] = true
			out.Tables = append(out.Tables, n)
		}
	}
	_ = rows.Close()

	for _, t := range requiredTables {
		if !set[t] {
			out.Missing = append(out.Missing, t)
		}
	}
	out.Compatible = len(out.Missing) == 0

	count := func(table, where string) int64 {
		if !set[table] {
			return 0
		}
		q := "SELECT count(*) FROM " + table
		if where != "" {
			q += " WHERE " + where
		}
		var n int64
		if err := db.QueryRow(q).Scan(&n); err != nil {
			return 0
		}
		return n
	}
	out.Users = count("users", "")
	out.Calendars = count("calendars", "deleted_at IS NULL")
	out.Events = count("events", "deleted_at IS NULL")
	out.Devices = count("devices", "")
	out.Reminders = count("reminders", "")
	out.InboxItems = count("inbox_items", "")

	if set["events"] {
		var lo, hi sql.NullInt64
		_ = db.QueryRow(`SELECT min(dtstart_utc), max(dtstart_utc) FROM events WHERE deleted_at IS NULL AND dtstart_utc IS NOT NULL`).Scan(&lo, &hi)
		if lo.Valid {
			out.FirstEvent = lo.Int64
		}
		if hi.Valid {
			out.LastEvent = hi.Int64
		}
	}

	switch {
	case !out.Compatible:
		out.SchemaNote = "这不是本程序的数据库：缺少 " + strings.Join(out.Missing, "、")
	case out.Users == 0:
		out.SchemaNote = "结构对得上，但一个账号都没有 —— 确认一下是不是拿错了文件"
	default:
		out.SchemaNote = "结构对得上，可以迁移"
	}
	return out, nil
}

// copyFile 复制一份文件，失败时删掉半个产物。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// migrateResult 是迁移完成后返回给前端的东西。
type migrateResult struct {
	OK       bool     `json:"ok"`
	Before   *dbStats `json:"before"`
	Source   *dbStats `json:"source"`
	After    *dbStats `json:"after"`
	Backup   string   `json:"backup"`
	Imported string   `json:"imported"`
	Note     string   `json:"note,omitempty"`
	Mismatch []string `json:"mismatch,omitempty"`
}

// migrateFrom 用 src 替换当前库。
//
// src 必须已经在 data 目录里（上传上来的临时文件满足这一点）。
func (p *Panel) migrateFrom(src string) (*migrateResult, error) {
	before, err := p.currentStats()
	if err != nil {
		// 当前库读不出来不阻止迁移：有可能就是"新装的、还没建库"。
		before = nil
	}
	// 迁移源自己的数字。迁移**应该**让这台机器上的数量变化 ——
	// 所以"对不对得上"要拿它比，不能拿"迁移前这台机器"比。
	srcStats, err := inspectDB(src)
	if err != nil {
		return nil, fmt.Errorf("迁移源读不出来：%w", err)
	}

	stamp := time.Now().Format("20060102-150405")
	dbPath := filepath.Join(p.cfg.DataDir, "schedule.db")

	backupDir := filepath.Join(p.cfg.DataDir, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建备份目录失败：%w", err)
	}

	wasRunning, _, _ := p.running()
	if wasRunning {
		if err := p.StopServer(); err != nil {
			return nil, fmt.Errorf("停止服务失败：%w", err)
		}
	}

	// ★ 两份东西都要留：
	//   · pre-migrate  = 换之前这台机器上的样子
	//   · imported     = 这次换进来的那一份
	// 后者看起来多余，但"迁移完之后旧机器就不在了"是常态 ——
	// 没有这一份，出问题时手里只剩一个已经改过 schema 的新库。
	var backupName, importedName string
	if _, err := os.Stat(dbPath); err == nil {
		backupName = filepath.Join(backupDir, "pre-migrate-"+stamp+".db")
		if err := copyFile(dbPath, backupName); err != nil {
			if wasRunning {
				_ = p.StartServer()
			}
			return nil, fmt.Errorf("备份当前数据库失败：%w", err)
		}
	}
	importedName = filepath.Join(backupDir, "imported-"+stamp+".db")
	if err := copyFile(src, importedName); err != nil {
		if wasRunning {
			_ = p.StartServer()
		}
		return nil, fmt.Errorf("留存迁移源失败：%w", err)
	}

	// 换库：先改名再放新的。中途失败就把旧的放回去。
	old := dbPath + ".replaced-" + stamp
	if _, err := os.Stat(dbPath); err == nil {
		if err := os.Rename(dbPath, old); err != nil {
			if wasRunning {
				_ = p.StartServer()
			}
			return nil, fmt.Errorf("替换数据库失败：%w", err)
		}
	}
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	if err := copyFile(src, dbPath); err != nil {
		_ = os.Rename(old, dbPath)
		if wasRunning {
			_ = p.StartServer()
		}
		return nil, fmt.Errorf("写入新数据库失败：%w", err)
	}
	_ = os.Remove(old)

	res := &migrateResult{OK: true, Before: before, Source: srcStats, Backup: backupName, Imported: importedName}

	if wasRunning {
		if err := p.StartServer(); err != nil {
			res.OK = false
			res.Note = "数据已经换过去了，但服务启动失败：" + err.Error() + "。原库在 " + backupName
			return res, nil
		}
	}

	after, err := p.currentStats()
	if err != nil {
		res.Note = "数据已经换过去了，但换完之后读不出来：" + err.Error()
		return res, nil
	}
	res.After = after
	res.Mismatch = diffStats(srcStats, after)

	// ★ 换库之后旧的会话令牌一定失效（tokens 表已经是别人的了）。
	// 不清掉的话，面板仍然认为"你登录着"，但每一个业务请求都返回
	// invalid token —— 界面上看是"什么都是空的、点什么都没反应"。
	p.clearSessions()
	return res, nil
}

// clearSessions 让所有面板会话立即下线。
func (p *Panel) clearSessions() {
	p.mu.Lock()
	p.sessions = map[string]*session{}
	p.mu.Unlock()
}

// diffStats 对比"迁移源"与"迁移后"。
//
// 只比**账号与日程**这类不该变的东西：日历数和设备数在迁移中可能正常变化
// （比如软删的日历被恢复），把它们也拿去报红会制造假警报。
//
// ⚠️ 比的是上传的那份文件，不是"迁移前这台机器"—— 后者本来就应该变。
func diffStats(a, b *dbStats) []string {
	if a == nil || b == nil {
		return nil
	}
	var out []string
	cmp := func(label string, x, y int64) {
		if x != y {
			out = append(out, fmt.Sprintf("%s：迁移前 %d，迁移后 %d", label, x, y))
		}
	}
	cmp("账号", a.Users, b.Users)
	cmp("日程", a.Events, b.Events)
	return out
}

// currentStats 读**正在使用**的库。用只读模式，避免和服核抢写锁。
func (p *Panel) currentStats() (*dbStats, error) {
	dbPath := filepath.Join(p.cfg.DataDir, "schedule.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	// 复制到临时文件再读：不和服务核心抢连接，也不受 WAL 未合并的影响。
	tmp := dbPath + ".inspect-" + time.Now().Format("150405.000")
	if err := copyFile(dbPath, tmp); err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	// WAL 里可能还有没落盘的提交，一起带过去。
	for _, ext := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + ext); err == nil {
			_ = copyFile(dbPath+ext, tmp+ext)
		}
	}
	return inspectDB(tmp)
}

// ImportDatabase 是命令行入口：用一份外部 .db 替换 target 的库。
//
// 与面板那条路走的是同一段代码，区别只是不经过 HTTP。
func ImportDatabase(opt Options, src string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("找不到要导入的数据库：%w", err)
	}
	stats, err := inspectDB(src)
	if err != nil {
		return fmt.Errorf("这个文件读不出来：%w", err)
	}
	if !stats.Compatible {
		return errors.New(stats.SchemaNote)
	}
	if stats.Users == 0 {
		return errors.New("这份库里一个账号都没有，已停止")
	}
	p := New(opt)
	res, err := p.migrateFrom(src)
	if err != nil {
		return err
	}
	// ⚠️ 发布版是 windowsgui 子系统，**没有控制台**：fmt 打印的东西
	//    从资源管理器里双击就消失了。所以同时写一份到日志里 ——
	//    这才是用户事后唯一能查到"到底迁了什么"的地方。
	say := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		fmt.Println(line)
		log.Println(line)
	}
	say("迁移完成。账号 %d，日历 %d，日程 %d", stats.Users, stats.Calendars, stats.Events)
	if res.Backup != "" {
		say("原库备份：%s", res.Backup)
	}
	if res.Imported != "" {
		say("迁移源留档：%s", res.Imported)
	}
	if len(res.Mismatch) > 0 {
		say("⚠️ 迁移前后对不上：")
		for _, m := range res.Mismatch {
			say("  %s", m)
		}
	}
	return nil
}

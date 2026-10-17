// 协作数据层：独立 SQLite（live.db）+ 行级 LWW 冲突判定（2026-10-10）。
//
// ## 为什么是独立库，不是主库
//
// 主库（schedule.db）是**主服务端的**，它有一套自己的迁移机制（`ensureColumn`），
// 而且 SQLite 是**单写者** —— 两个进程同时写会互相锁。
// 所以这里的原则是：
//
//     身份：从主库**只读**（users / tokens / api_keys / live_tickets）
//     文档：写在**自己的** live.db 里
//
// 好处除了避开锁竞争，还有"协作这套东西可以独立演进" —— 改它的表结构
// 不需要动主服务的迁移，也不会让主服务重启。
//
// ## 冲突判定的全部逻辑
//
// 就一个 if：
//
//     收到 edit(line=L, text=T, baseRev=B)
//       if L.rev != B  →  冲突（这行已经被别人改过了）→ **不写**
//       else           →  L.text = T; L.rev += 1
//
// ⚠️ 冲突时**坚决不覆盖**。原因：静默覆盖的后果是
//    "用户以为自己的修改保存了，其实被别人的盖掉了" —— 这是最坏的一种失败，
//    而且**当场看不出来**（他下次打开才发现）。宁可当场报冲突，交给用户决定。
package app

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// 协作笔记的角色。
//
// ⚠️ **刻意和日历那套（`acl.go` 的 RoleOwner/RoleAdmin/RoleEditor/RoleViewer）
//    用不同的标识符**。两边虽然都叫 "owner"，但语义不同：
//    日历的 owner 是"这个日历归谁"，协作的 owner 是"谁把这篇笔记共享出去的"，
//    而且**不可转让**。整合进同一个 Go 包之后，如果两套共用一个名字，
//    将来某个人在日历那边加一个角色，这边会**静默地跟着变**（编译通过、行为不同）。
//
// 值本身仍然是 `owner` / `editor` / `reader` —— 那是**客户端契约**
// （`plugin-mdnote-live` 的 LiveState 直接比字符串），不能改。
const (
	liveRoleOwner  = "owner"
	liveRoleEditor = "editor"
	liveRoleReader = "reader"
)

func roleCanWrite(role string) bool { return role == liveRoleOwner || role == liveRoleEditor }
func roleCanManage(role string) bool { return role == liveRoleOwner }
func roleCanRead(role string) bool   { return role != "" }

// Line 是文档里的一行。
//
// ⚠️ `ID` 是**稳定的**，不等于行号。用行号的话，任何一次插入都会让它后面的行
//    全部"变号"——在同步上等于整篇都变了，客户端要重画整个文档。
type Line struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Rev  int64  `json:"rev"`
	By   string `json:"by"`
	At   int64  `json:"at"`
	Ord  int    `json:"ord"`
}

const liveSchema = `
CREATE TABLE IF NOT EXISTS live_doc(
  doc_id     TEXT PRIMARY KEY,
  owner      TEXT NOT NULL,
  title      TEXT NOT NULL DEFAULT '',
  rev        INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS live_line(
  doc_id  TEXT NOT NULL,
  line_id TEXT NOT NULL,
  ord     INTEGER NOT NULL,
  text    TEXT NOT NULL DEFAULT '',
  rev     INTEGER NOT NULL DEFAULT 0,
  by      TEXT NOT NULL DEFAULT '',
  at      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(doc_id, line_id)
);
CREATE TABLE IF NOT EXISTS live_acl(
  doc_id   TEXT NOT NULL,
  user_id  TEXT NOT NULL,
  role     TEXT NOT NULL,
  added_at INTEGER NOT NULL,
  PRIMARY KEY(doc_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_live_line_ord ON live_line(doc_id, ord);
`

type Store struct{ db *sql.DB }

func openStore(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 协作服务是单进程，写操作都由 hub 串行化了；连接数不用多
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(liveSchema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---------------------------------------------------------------- 文档

// ensureDoc 确保文档存在（第一次共享时创建），owner 只在**创建时**写一次。
//
// ⚠️ 用 `INSERT ... ON CONFLICT DO NOTHING` 而不是先查后插：
//    两个客户端同时第一次进来时，先查后插会有一个撞唯一键报错。
//    而且**不能**在冲突时更新 owner —— 那等于"后来的人能抢走文档"。
func (s *Store) ensureDoc(docID, owner, title string, now int64) error {
	_, err := s.db.Exec(
		`INSERT INTO live_doc(doc_id,owner,title,rev,created_at,updated_at)
		 VALUES(?,?,?,0,?,?)
		 ON CONFLICT(doc_id) DO NOTHING`,
		docID, owner, title, now, now,
	)
	if err != nil {
		return err
	}
	// owner 同时是 ACL 里的一条记录（role=owner），
	// 这样"判权限"只有一条查询路径，不用特判"他是不是 owner"
	_, err = s.db.Exec(
		`INSERT INTO live_acl(doc_id,user_id,role,added_at) VALUES(?,?,?,?)
		 ON CONFLICT(doc_id,user_id) DO NOTHING`,
		docID, owner, liveRoleOwner, now,
	)
	return err
}

type DocMeta struct {
	ID        string `json:"id"`
	Owner     string `json:"owner"`
	Title     string `json:"title"`
	Rev       int64  `json:"rev"`
	UpdatedAt int64  `json:"updatedAt"`
	Lines     int    `json:"lines"`
	MyRole    string `json:"myRole"`
}

// listDocs 列出某用户能看到的文档（owner / editor / reader 三种都算）。
func (s *Store) listDocs(uid string) ([]DocMeta, error) {
	rows, err := s.db.Query(
		`SELECT d.doc_id, d.owner, d.title, d.rev, d.updated_at, a.role,
		        (SELECT COUNT(1) FROM live_line l WHERE l.doc_id=d.doc_id)
		   FROM live_doc d JOIN live_acl a ON a.doc_id=d.doc_id
		  WHERE a.user_id=?
		  ORDER BY d.updated_at DESC`, uid,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DocMeta{}
	for rows.Next() {
		var m DocMeta
		if err := rows.Scan(&m.ID, &m.Owner, &m.Title, &m.Rev, &m.UpdatedAt, &m.MyRole, &m.Lines); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) roleOf(docID, uid string) (string, error) {
	var role string
	err := s.db.QueryRow(`SELECT role FROM live_acl WHERE doc_id=? AND user_id=?`, docID, uid).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil // 不在名单里 —— 不是错误，是"无权限"
	}
	return role, err
}

// loadLines 取文档的全部行（按 ord 排序）。
//
// ⚠️ 全量返回是**有意**的：文档是纯文本，几百行也就几十 KB，
//    而"增量补发"要维护"客户端到底缺哪几段"的状态 —— 那个状态一旦不同步，
//    表现是"有人的屏幕上少一段，而且永远补不回来"。全量刷新不会错。
func (s *Store) loadLines(docID string) ([]Line, error) {
	rows, err := s.db.Query(
		`SELECT line_id,text,rev,by,at,ord FROM live_line WHERE doc_id=? ORDER BY ord`, docID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Line{}
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.ID, &l.Text, &l.Rev, &l.By, &l.At, &l.Ord); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) docTitle(docID string) (string, error) {
	var t string
	err := s.db.QueryRow(`SELECT title FROM live_doc WHERE doc_id=?`, docID).Scan(&t)
	return t, err
}

// createFrom 用一篇笔记的全文创建（或补齐）协作文档。
//
// ⚠️ 幂等：文档已经存在时**不覆盖**已有内容（否则每次"共享"都会把
//    别人正在改的东西冲掉）。只在**行数为 0** 时按传入的全文铺一遍。
func (s *Store) createFrom(docID, owner, title, markdown string, now int64) (int, error) {
	if err := s.ensureDoc(docID, owner, title, now); err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM live_line WHERE doc_id=?`, docID).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return n, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	lines := splitLines(markdown)
	for i, t := range lines {
		_, err := tx.Exec(
			`INSERT INTO live_line(doc_id,line_id,ord,text,rev,by,at) VALUES(?,?,?,?,1,?,?)
			 ON CONFLICT(doc_id,line_id) DO NOTHING`,
			docID, newLineID(), i, t, owner, now,
		)
		if err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`UPDATE live_doc SET title=?, rev=rev+1, updated_at=? WHERE doc_id=?`,
		title, now, docID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(lines), nil
}

// ---------------------------------------------------------------- 编辑（冲突判定的全部逻辑）

// conflictInfo 描述一次"没写进去"的编辑。
type conflictInfo struct {
	LineID string
	Mine   string
	Theirs string
	By     string
	ByRev  int64
}

// applyEdit 改一行。
//
// 返回：
//   ok=true           写入成功，newRev 是这一行的新版本
//   ok=false,cf!=nil  冲突：这一行已经被别人改过（**没有写入**）
//
// ⚠️ 检查与写入必须在**同一个事务**里。分成两步的话，
//    两个并发的 edit 会双双通过检查（都读到旧 rev），然后后写的盖掉先写的 ——
//    那正是这次要防的事情，而它只在"恰好同时"时发生，测试很难稳定复现。
func (s *Store) applyEdit(docID, lineID, text, by string, baseRev int64, now int64) (int64, *conflictInfo, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var curText, curBy string
	var curRev int64
	err = tx.QueryRow(
		`SELECT text,rev,by FROM live_line WHERE doc_id=? AND line_id=?`, docID, lineID,
	).Scan(&curText, &curRev, &curBy)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, fmt.Errorf("这一行不存在（可能已被别人删掉）")
	}
	if err != nil {
		return 0, nil, err
	}

	if curRev != baseRev {
		// ★ 冲突：不写，把对方的原文带回去
		return 0, &conflictInfo{
			LineID: lineID, Mine: text, Theirs: curText, By: curBy, ByRev: curRev,
		}, nil
	}
	// 内容没变就不必加版本（避免"点一下没改也涨版本"）
	if curText == text {
		return curRev, nil, nil
	}

	newRev := curRev + 1
	if _, err := tx.Exec(
		`UPDATE live_line SET text=?,rev=?,by=?,at=? WHERE doc_id=? AND line_id=?`,
		text, newRev, by, now, docID, lineID,
	); err != nil {
		return 0, nil, err
	}
	if _, err := tx.Exec(
		`UPDATE live_doc SET rev=rev+1, updated_at=? WHERE doc_id=?`, now, docID,
	); err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return newRev, nil, nil
}

// insertLine 在某行后面插一行。返回新行的 ord。
func (s *Store) insertLine(docID, afterLineID, text, by string, now int64) (Line, error) {
	var ord int
	if afterLineID == "" {
		// 插到最前面
		err := s.db.QueryRow(`SELECT COALESCE(MIN(ord),0)-1 FROM live_line WHERE doc_id=?`, docID).Scan(&ord)
		if err != nil {
			return Line{}, err
		}
	} else {
		var afterOrd int
		if err := s.db.QueryRow(`SELECT ord FROM live_line WHERE doc_id=? AND line_id=?`,
			docID, afterLineID).Scan(&afterOrd); err != nil {
			return Line{}, fmt.Errorf("找不到要插到它后面的那一行")
		}
		// 把后面的行整体往后挪一格（ord 是整数序号，挪一下最简单）
		if _, err := s.db.Exec(
			`UPDATE live_line SET ord=ord+1 WHERE doc_id=? AND ord>?`, docID, afterOrd,
		); err != nil {
			return Line{}, err
		}
		ord = afterOrd + 1
	}

	l := Line{ID: newLineID(), Text: text, Rev: 1, By: by, At: now, Ord: ord}
	if _, err := s.db.Exec(
		`INSERT INTO live_line(doc_id,line_id,ord,text,rev,by,at) VALUES(?,?,?,?,?,?,?)`,
		docID, l.ID, l.Ord, l.Text, l.Rev, l.By, l.At,
	); err != nil {
		return Line{}, err
	}
	_, _ = s.db.Exec(`UPDATE live_doc SET rev=rev+1, updated_at=? WHERE doc_id=?`, now, docID)
	return l, nil
}

// deleteLine 删一行。
//
// ⚠️ 带 baseRev 检查（和 applyEdit 一样）—— 否则"我删的时候别人正在改"
//    会变成静默丢失：他以为改上了，其实那行已经没了。
func (s *Store) deleteLine(docID, lineID, by string, baseRev int64, now int64) (bool, *conflictInfo, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var curText, curBy string
	var curRev int64
	err = tx.QueryRow(`SELECT text,rev,by FROM live_line WHERE doc_id=? AND line_id=?`,
		docID, lineID).Scan(&curText, &curRev, &curBy)
	if errors.Is(err, sql.ErrNoRows) {
		// 已经没了 —— 视作成功（幂等），免得客户端卡在"删不掉"
		return true, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if curRev != baseRev {
		return false, &conflictInfo{LineID: lineID, Mine: "", Theirs: curText, By: curBy, ByRev: curRev}, nil
	}
	if _, err := tx.Exec(`DELETE FROM live_line WHERE doc_id=? AND line_id=?`, docID, lineID); err != nil {
		return false, nil, err
	}
	if _, err := tx.Exec(`UPDATE live_doc SET rev=rev+1, updated_at=? WHERE doc_id=?`, now, docID); err != nil {
		return false, nil, err
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	return true, nil, nil
}

// ---------------------------------------------------------------- 权限

type LiveMember struct {
	UserID string `json:"id"`
	Role   string `json:"role"`
}

func (s *Store) listMembers(docID string) ([]LiveMember, error) {
	rows, err := s.db.Query(`SELECT user_id,role FROM live_acl WHERE doc_id=? ORDER BY added_at`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LiveMember{}
	for rows.Next() {
		var m LiveMember
		if err := rows.Scan(&m.UserID, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// setRole 改某个成员的角色。
//
// ⚠️ 三条护栏（都是"不可逆或会把自己锁在门外"的情况）：
//   ① 不能改 owner 的角色 —— 那是"把别人的文档收走"，本期不做移交；
//   ② 不能把**最后一个**能写的人降级 —— 降完就没人能改了（文档变只读）；
//   ③ 角色必须是三个合法值之一。
func (s *Store) setRole(docID, uid, role string) error {
	if role != liveRoleOwner && role != liveRoleEditor && role != liveRoleReader {
		return fmt.Errorf("角色只能是 owner / editor / reader")
	}
	if role == liveRoleOwner {
		return fmt.Errorf("所有权不能转让（本期不支持）")
	}
	var owner string
	if err := s.db.QueryRow(`SELECT owner FROM live_doc WHERE doc_id=?`, docID).Scan(&owner); err != nil {
		return fmt.Errorf("文档不存在")
	}
	if uid == owner {
		return fmt.Errorf("不能改创建者的权限")
	}

	if role == liveRoleReader {
		// 降级前先数一下还有几个能写的
		var writers int
		if err := s.db.QueryRow(
			`SELECT COUNT(1) FROM live_acl WHERE doc_id=? AND role IN (?,?)`,
			docID, liveRoleOwner, liveRoleEditor,
		).Scan(&writers); err != nil {
			return err
		}
		var cur string
		err := s.db.QueryRow(`SELECT role FROM live_acl WHERE doc_id=? AND user_id=?`, docID, uid).Scan(&cur)
		if err == nil && roleCanWrite(cur) && writers <= 1 {
			return fmt.Errorf("至少得留一个能编辑的人 —— 现在只剩他一个了")
		}
	}

	now := time.Now().UnixMilli()
	_, err := s.db.Exec(
		`INSERT INTO live_acl(doc_id,user_id,role,added_at) VALUES(?,?,?,?)
		 ON CONFLICT(doc_id,user_id) DO UPDATE SET role=excluded.role`,
		docID, uid, role, now,
	)
	return err
}

// removeMember 把某人从名单里去掉（= 无权限）。
func (s *Store) removeMember(docID, uid string) error {
	var owner string
	if err := s.db.QueryRow(`SELECT owner FROM live_doc WHERE doc_id=?`, docID).Scan(&owner); err != nil {
		return fmt.Errorf("文档不存在")
	}
	if uid == owner {
		return fmt.Errorf("不能移除创建者")
	}
	_, err := s.db.Exec(`DELETE FROM live_acl WHERE doc_id=? AND user_id=?`, docID, uid)
	return err
}

// renameDoc 改标题（只有 owner 能改，权限在调用方判）。
func (s *Store) renameDoc(docID, title string, now int64) error {
	_, err := s.db.Exec(`UPDATE live_doc SET title=?, updated_at=? WHERE doc_id=?`, title, now, docID)
	return err
}

// dropDoc 删除整个协作文档（只有 owner 能删）。**不可逆**。
func (s *Store) dropDoc(docID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM live_line WHERE doc_id=?`,
		`DELETE FROM live_acl WHERE doc_id=?`,
		`DELETE FROM live_doc WHERE doc_id=?`,
	} {
		if _, err := tx.Exec(q, docID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- 小工具

// splitLines 按行拆，并且**规范化换行**（\r\n → \n）。
//
// ⚠️ 不规范化的话，Windows 上编辑过的笔记会带 \r，
//    拆出来的每行结尾都多一个不可见字符，两边看到的文本永远不相等 ——
//    表现是"明明没改，版本一直在涨"。
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

// newLineID 生成稳定的行 id。
//
// ⚠️ 不用行号（插入会让后面全体变号），也不复用别人的 id 空间。
func newLineID() string { return "l_" + randHex(10) }

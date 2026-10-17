package app

import (
	"database/sql"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 权限模型（2026-09-15 重做）
//
// 分成两层，互相独立：
//
//   1. **日历成员** —— 这张日历谁能看见、谁能改。
//      角色 owner > admin > editor > viewer（见 roleRank）。
//      `role` 是从 SQL 里 SELECT 出来的真字段名，千万别用 Go 关键字 `range` 之类；
//      历史上这里有过一个 `role` 被搞丢的 SELECT，见 loadMembers 的注释。
//
//   2. **个人提醒** —— 提醒是**每个人自己的**，不是日历的属性。
//      同一条日程，「我」可能提前 15 分钟响，「同学」可能压根不想被吵。
//      之前把提醒挂在 event 上（reminders 表按 event_id 存），
//      于是共享日历以后，"谁改了提醒"变成了对全体的改动 —— 语义是错的。
//      现在按 (event_id, user_id) 存；user_id='' 的行是"共享默认"，只在
//      本人没有自己的行时才用得上（详见 remindersForUser）。
//
// 三条硬规则，越界会静默出错而不是报错：
//
//   - **加成员不改 kind**。以前 handleAddMember 顺手把 kind 设成 'shared'，
//     于是"给同学加权限"等于"把这本日历变成共享日历"，用户没法只授权。
//     kind 只由创建和显式改名决定（setCalendarShared）。
//   - **改 kind 必须补发内容**。private → shared 之后新成员的游标早过了
//     日历里日程的 seq，不补发他只会看到一个空日历（resendCalendarContents）。
//   - **默认不提醒**。空提醒列表 = 不提醒，不是"退回默认提前 15 分钟"。
//     这条以前在三个地方各写了一遍兜底，导致用户没法创建不提醒的日程，
//     连 AI 都没法表达"这个不用提醒我"。
// ---------------------------------------------------------------------------

// ---------- 角色 ----------

const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// roleRank orders roles from most to least privileged. Anything unknown is
// treated as a viewer: an unrecognised role string must never widen access.
func roleRank(role string) int {
	switch role {
	case RoleOwner:
		return 4
	case RoleAdmin:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

func isValidRole(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleEditor, RoleViewer:
		return true
	}
	return false
}

func atLeast(role, want string) bool {
	return roleRank(role) >= roleRank(want)
}

// ---------- 读模型 ----------

// calendarRole resolves the caller's role on a calendar.
//
// The owner is derived from owner_id rather than from calendar_members so a
// missing/half-written member row can never lock an owner out of their own
// calendar.
// teamRoleCeiling 团队共享能给一张日历带来的**最高**角色。
//
// ⚠️⚠️ **这个上限是团队功能的安全底线，不许放开。**
//
// 团队共享给的是"**内容可见性**"（能看到、能改日程），
// **不是**"能管人"。如果团队能给到 admin，那么任何一个团队管理员
// 只要建个团队、把别人的日历收进来，就能拿到那张日历的成员管理权 ——
// 等于绕过了日历自己的成员名单，也绕过了"加成员必须 canManage"这条。
//
// 而且团队是可自建的（任何人能建团队），所以团队侧的角色**必须**比日历侧更严。
const teamRoleCeiling = RoleEditor

// teamRoleForCalendar 算出"我因为团队身份，对这张日历能拿到什么角色"。
//
// 一张日历可能共享给了多个团队，我可能在其中几个里 → 取最高的那个，
// 再压到 teamRoleCeiling。
//
// 归属维度是 `team_members.user_id`（我在不在这几个团队里），
// 不是日历权限 —— 这两个是**正交**的，别混。
func (s *Server) teamRoleForCalendar(calendarID, uid string) (string, bool) {
	if calendarID == "" || uid == "" {
		return "", false
	}
	rows, err := s.db.sql.Query(
		`SELECT ct.role FROM calendar_teams ct
		 JOIN team_members tm ON tm.team_id = ct.team_id
		 WHERE ct.calendar_id=? AND tm.user_id=?`, calendarID, uid)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	best := ""
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			continue
		}
		if r == "" {
			// 空角色一律按最低处理 —— 宁可少给权限（和下面直接成员那条同一个原则）
			r = RoleViewer
		}
		// 先压上限再比大小：如果行里存了个越界的 admin（手工改库/老数据），
		// **不能**让它参与比较，否则一次比较就把上限绕过去了。
		if !atLeast(teamRoleCeiling, r) {
			r = teamRoleCeiling
		}
		if best == "" || atLeast(r, best) {
			best = r
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// calendarRole 返回某人对某张日历的**有效角色**。
//
// 这是**整个权限体系的唯一收口点** —— canRead / canWrite / canManageMembers
// 全都走它，所以团队支持只要改这一个函数，所有调用点自动继承。
//
// 有效角色 = max(直接成员角色, 团队派生角色)
//
// ⚠️ 团队派生角色已经压过上限（≤ editor），所以这里不可能通过团队拿到
// owner/admin。**owner 仍然只认 `calendars.owner_id`。**
func (s *Server) calendarRole(calendarID, uid string) (string, bool) {
	if calendarID == "" || uid == "" {
		return "", false
	}
	var owner string
	err := s.db.sql.QueryRow(`SELECT owner_id FROM calendars WHERE id=?`, calendarID).Scan(&owner)
	if err != nil {
		return "", false
	}
	if owner == uid {
		return RoleOwner, true
	}

	// ① 直接成员
	direct := ""
	var role string
	var deleted sql.NullInt64
	err = s.db.sql.QueryRow(
		`SELECT cm.role, c.deleted_at FROM calendar_members cm
		 JOIN calendars c ON c.id = cm.calendar_id
		 WHERE cm.calendar_id=? AND cm.user_id=?`, calendarID, uid).Scan(&role, &deleted)
	if err == nil {
		direct = role
		if direct == "" {
			// 老库里 role 列可能为空（schema 默认写的是 'editor'，但历史行未必）。
			// 空角色一律降为 viewer —— 宁可少给权限，不要因为一个空字符串把
			// 只读成员变成可写成员。
			direct = RoleViewer
		}
	}

	// ② 团队派生
	team, hasTeam := s.teamRoleForCalendar(calendarID, uid)

	switch {
	case direct == "" && !hasTeam:
		return "", false
	case direct == "":
		return team, true
	case !hasTeam:
		return direct, true
	}
	// 两边都有 → 取高的那个
	if atLeast(team, direct) {
		return team, true
	}
	return direct, true
}

func (s *Server) canRead(calendarID, uid string) bool {
	_, ok := s.calendarRole(calendarID, uid)
	return ok
}

func (s *Server) canWrite(calendarID, uid string) bool {
	role, ok := s.calendarRole(calendarID, uid)
	return ok && atLeast(role, RoleEditor)
}

// canManageMembers: owner or admin may add/remove/retitle other people.
func (s *Server) canManageMembers(calendarID, uid string) bool {
	role, ok := s.calendarRole(calendarID, uid)
	return ok && atLeast(role, RoleAdmin)
}

// ---------- 写模型 ----------

// resendCalendarContents 把一张日历里已有的日程/实例重新挂进同步流。
//
// 为什么需要它（2026-09-15 修的真实 bug）：同步是**增量**的 —— 客户端带着自己的
// 游标来要"比这个更新的变更"。把某人加进一张早就存在的共享日历时，只给日历本身
// 分配新 seq 是不够的：那张日历里的日程 seq 都很旧，早就落在新成员的游标之下了，
// 于是他只会收到"多了一个日历"，打开却是空的（用户报的就是"共享的日历别人看不到"）。
//
// 这里给日历里的每个 event / instance 各补一条 sync_log。**不改行自身的 seq**，
// 免得打乱字段级合并的基线（field_seqs 才是基线）。代价是其它成员也会重收这些行 ——
// 客户端是按 id upsert，重收是幂等的。
func (s *Server) resendCalendarContents(tx *sql.Tx, calendarID string) error {
	for _, spec := range []struct{ entity, table string }{
		{"event", "events"},
		{"instance", "instances"},
	} {
		rows, err := tx.Query(
			`SELECT id FROM `+spec.table+` WHERE calendar_id=? AND deleted_at IS NULL`, calendarID)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			if _, err := nextSeq(tx, spec.entity, id, "share"); err != nil {
				return err
			}
		}
	}
	return nil
}

// resendCalendarMembers 把成员名单挂进同步流。
//
// **这是"别人改了共享权限、我这台看不到"的根因。**
//
// 以前的 handleAddMember/handleRemoveMember 只做了一件事：`UPDATE calendars SET seq=?`。
// 但客户端同步是按 id upsert 一整行 —— 只有**别的字段**也变了、日历这一行被重新下发
// 时，成员名单才会顺带更新。如果那次改动恰好只改了成员（这正是"加同学"的场景），
// 日历行的内容对旧客户端来说没有任何变化，快照里的 members 就一直是旧的。
//
// 修法是在**同一个事务里**把日历行的每个字段 seq 抬到新 seq（不是只抬 kind），
// 让这一行必然被重新下发一次。客户端按 id upsert，重收是幂等的。
func (s *Server) resendCalendarMembers(tx *sql.Tx, calendarID string) error {
	var fsStr string
	if err := tx.QueryRow(`SELECT field_seqs FROM calendars WHERE id=?`, calendarID).Scan(&fsStr); err != nil {
		return err
	}
	seq, err := nextSeq(tx, "calendar", calendarID, "members")
	if err != nil {
		return err
	}
	fs := decodeFieldSeqs(fsStr)
	for _, f := range []string{"name", "color", "kind"} {
		fs[f] = seq
	}
	_, err = tx.Exec(`UPDATE calendars SET seq=?, field_seqs=?, updated_at=? WHERE id=?`,
		seq, encodeFieldSeqs(fs), time.Now().Unix(), calendarID)
	return err
}

// setCalendarShared 是**唯一**允许改 kind 的入口。
//
// private → shared 要补发内容：新成员（已经在这张日历上的那些人）的游标早就
// 过了日程的 seq，不补发就是"日历出现在列表里、点开是空的"。
// 反过来 shared → private 不删成员：降级只影响新成员能不能被加进来，已经分享
// 出去的人不该因为创建者反悔就被静默摘掉权限 —— 要摘就显式移除。
func (s *Server) setCalendarShared(tx *sql.Tx, calendarID, kind string) error {
	var cur string
	if err := tx.QueryRow(`SELECT kind FROM calendars WHERE id=?`, calendarID).Scan(&cur); err != nil {
		return err
	}
	if cur == kind {
		return nil
	}
	var fsStr string
	_ = tx.QueryRow(`SELECT field_seqs FROM calendars WHERE id=?`, calendarID).Scan(&fsStr)
	seq, err := nextSeq(tx, "calendar", calendarID, "upsert")
	if err != nil {
		return err
	}
	fs := decodeFieldSeqs(fsStr)
	for _, f := range []string{"name", "color", "kind"} {
		fs[f] = seq
	}
	if _, err := tx.Exec(`UPDATE calendars SET kind=?, seq=?, field_seqs=?, updated_at=? WHERE id=?`,
		kind, seq, encodeFieldSeqs(fs), time.Now().Unix(), calendarID); err != nil {
		return err
	}
	if kind == "shared" || kind == "course" {
		return s.resendCalendarContents(tx, calendarID)
	}
	return nil
}

// upsertMember writes one membership row and nudges the sync feed.
func (s *Server) upsertMember(tx *sql.Tx, calendarID, userID, role string) error {
	if !isValidRole(role) {
		role = RoleEditor
	}
	if _, err := tx.Exec(`INSERT INTO calendar_members(calendar_id,user_id,role) VALUES(?,?,?)
		ON CONFLICT(calendar_id,user_id) DO UPDATE SET role=excluded.role`,
		calendarID, userID, role); err != nil {
		return err
	}
	return s.resendCalendarMembers(tx, calendarID)
}

func (s *Server) dropMember(tx *sql.Tx, calendarID, userID string) error {
	if _, err := tx.Exec(`DELETE FROM calendar_members WHERE calendar_id=? AND user_id=?`,
		calendarID, userID); err != nil {
		return err
	}
	return s.resendCalendarMembers(tx, calendarID)
}

// ---------- 个人提醒 ----------

func clampMinutes(m int64) int64 {
	if m < 0 {
		return 0
	}
	if m > 10080 { // 一周，超出没有意义
		return 10080
	}
	return m
}

// setPersonalReminders 全量替换"某个人在某条日程上的提醒"。
//
// **空列表是合法输入** —— 它表示"这条我不想被提醒"，会把该用户的专属行清空。
// （若 event 上还有共享默认行，本人专属行清空后会退回共享默认，这是有意的：
// 把默认值改掉是另一件事。）
func (s *Server) setPersonalReminders(tx *sql.Tx, eventID, userID string, minutes []int64) error {
	if _, err := tx.Exec(`DELETE FROM reminders WHERE event_id=? AND user_id=?`, eventID, userID); err != nil {
		return err
	}
	now := time.Now().Unix()
	seen := map[int64]bool{}
	n := 0
	for _, m := range minutes {
		m = clampMinutes(m)
		if seen[m] {
			continue
		}
		seen[m] = true
		if n >= 5 { // 和客户端保持一致：最多 5 条
			break
		}
		n++
		if _, err := tx.Exec(`INSERT INTO reminders(id,event_id,user_id,minutes_before,channel,seq,updated_at)
			VALUES(?,?,?,?,?,0,?)`, newID("r_"), eventID, userID, m, "notify", now); err != nil {
			return err
		}
	}
	return nil
}

// setSharedReminders 直接改写 event 上的共享默认行（user_id=” 的历史行）。
// 只有事件**创建者**走这条路，其它人写的是自己的专属行。
func (s *Server) setSharedReminders(tx *sql.Tx, eventID string, minutes []int64) error {
	if _, err := tx.Exec(`DELETE FROM reminders WHERE event_id=? AND user_id=''`, eventID); err != nil {
		return err
	}
	now := time.Now().Unix()
	seen := map[int64]bool{}
	n := 0
	for _, m := range minutes {
		m = clampMinutes(m)
		if seen[m] {
			continue
		}
		seen[m] = true
		if n >= 5 {
			break
		}
		n++
		if _, err := tx.Exec(`INSERT INTO reminders(id,event_id,user_id,minutes_before,channel,seq,updated_at)
			VALUES(?,?,?,?,?,0,?)`, newID("r_"), eventID, "", m, "notify", now); err != nil {
			return err
		}
	}
	return nil
}

type reminderView struct {
	ID            string `json:"id"`
	EventID       string `json:"event_id"`
	UserID        string `json:"user_id,omitempty"`
	MinutesBefore int64  `json:"minutes_before"`
	Channel       string `json:"channel"`
	Mine          bool   `json:"mine"`
}

// remindersForUser 返回"这个人在这条日程上会响的提醒"。
//
// 规则（按优先级）：
//  1. 本人表过态（reminder_prefs 里有行）—— 以专属行为准，**哪怕是空集**（= 明确不要提醒）；
//  2. 共享默认行（user_id = ”）—— 老数据在 event 上配的提前量，本人没表过态时沿用；
//  3. 都没有 → **空**，即不提醒。
//
// 第 3 条是这次改动的重点。以前它返回"默认提前 15 分钟"，于是
// "创建一个不提醒的日程"根本做不到，AI 也没法表达这件事。
func (s *Server) remindersForUser(eventID, uid string, withSharedFallback bool) []reminderView {
	if eventID == "" || uid == "" {
		return []reminderView{}
	}
	rows, err := s.db.sql.Query(`
		SELECT id,event_id,user_id,minutes_before,channel FROM reminders
		WHERE event_id=? AND (user_id=? OR user_id='')
		ORDER BY minutes_before DESC`, eventID, uid)
	if err != nil {
		return []reminderView{}
	}
	defer rows.Close()

	mine := []reminderView{}
	shared := []reminderView{}
	for rows.Next() {
		var r reminderView
		if err := rows.Scan(&r.ID, &r.EventID, &r.UserID, &r.MinutesBefore, &r.Channel); err != nil {
			continue
		}
		r.Mine = r.UserID == uid
		if r.UserID == "" {
			shared = append(shared, r)
		} else {
			mine = append(mine, r)
		}
	}
	// 「本人专属行存在」本身就是一个信号，但空集无法用行数表达 —— 所以另立
	// reminder_prefs 记"这个人表过态"。没表过态时才退回共享默认。
	if s.hasPersonalPref(eventID, uid) {
		return mine
	}
	if withSharedFallback && len(shared) > 0 {
		return shared
	}
	return []reminderView{}
}

// hasPersonalPref 区分"这个人明确要 0 条提醒"和"这个人没表过态"。
//
// 单靠 reminders 表分不出来：两种情况都是"查不到专属行"。老数据里没有
// reminder_prefs 行，于是它天然退化为旧行为（沿用共享默认）。
func (s *Server) hasPersonalPref(eventID, uid string) bool {
	var n int
	if err := s.db.sql.QueryRow(
		`SELECT COUNT(*) FROM reminder_prefs WHERE event_id=? AND user_id=?`,
		eventID, uid).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// markPersonalPref 记录"本人已经就这条日程的提醒表过态"。
func markPersonalPref(tx *sql.Tx, eventID, uid string) error {
	if uid == "" {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO reminder_prefs(event_id,user_id,updated_at) VALUES(?,?,?)
		ON CONFLICT(event_id,user_id) DO UPDATE SET updated_at=excluded.updated_at`,
		eventID, uid, time.Now().Unix())
	return err
}

// setStrongReminder 设置"某人在某条日程上"的强提醒开关。
//
// 和 setPersonalReminders 一样是**全量替换**语义：不管原来有没有行，
// 写完就是 on_off = on。这里只有一行，所以用 UPSERT（先 DELETE 再 INSERT
// 是为了处理"一组行"，单个布尔值没必要）。
//
// ⚠️ 不在这里抬 seq。调用方是 POST/PUT /api/events，event 行的 seq 会由
// applyFieldMerge 抬起来，对端靠它把整条日程重新拉一遍 —— 这是强提醒能跨端
// 同步的**唯一**保证。所以**不要**把强提醒做成独立接口。
func (s *Server) setStrongReminder(tx *sql.Tx, eventID, uid string, on bool) error {
	if uid == "" || eventID == "" {
		return nil
	}
	v := 0
	if on {
		v = 1
	}
	_, err := tx.Exec(`INSERT INTO strong_reminders(event_id,user_id,on_off,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(event_id,user_id) DO UPDATE SET on_off=excluded.on_off, updated_at=excluded.updated_at`,
		eventID, uid, v, time.Now().Unix())
	return err
}

// strongForUser 返回"这个人对这条日程开没开强提醒"。
//
// **查不到行 = false（默认关闭）**，这是产品的硬要求：只有用户手动点开过才会触发，
// 任何"没设置就默认打开"的兜底都是错的。sql.ErrNoRows 也走这里。
func (s *Server) strongForUser(eventID, uid string) bool {
	if eventID == "" || uid == "" {
		return false
	}
	var v int
	err := s.db.sql.QueryRow(
		`SELECT on_off FROM strong_reminders WHERE event_id=? AND user_id=?`,
		eventID, uid).Scan(&v)
	if err != nil {
		return false
	}
	return v == 1
}

// ---------- HTTP: 成员管理 ----------

type memberReq struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
	// Add 让老客户端（只发一个布尔值）继续可用：true → editor，false → 移除。
	Add *bool `json:"add"`
}

func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	if !s.canManageMembers(id, uid) {
		apiErr(w, 403, "只有日历创建者能管理同学")
		return
	}
	var req memberReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	req.UserID = strings.TrimSpace(req.UserID)
	if req.UserID == "" {
		apiErr(w, 400, "请指定要添加的同学")
		return
	}
	if req.UserID == uid {
		apiErr(w, 400, "你自己已经在名单里了")
		return
	}
	// 目标用户必须真实存在，否则会写出一个永远查不到名字的幽灵成员
	var exists int
	if err := s.db.sql.QueryRow(`SELECT COUNT(*) FROM users WHERE id=?`, req.UserID).Scan(&exists); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if exists == 0 {
		apiErr(w, 404, "没有这个账号")
		return
	}
	role := req.Role
	if !isValidRole(role) {
		role = RoleEditor
	}
	if role == RoleOwner {
		// 转让所有权是另一个动作，不能顺手发生。这里只允许降级为 admin。
		role = RoleAdmin
	}

	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	if err := s.upsertMember(tx, id, req.UserID, role); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	// **注意**：这里刻意不碰 kind。加同学只给权限，不会把日历变成共享日历。
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	c, _ := s.calendarByID(id, uid)
	writeJSON(w, 200, map[string]any{"calendar": c})
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	target := r.PathValue("uid")
	if !s.canManageMembers(id, uid) {
		apiErr(w, 403, "只有日历创建者能管理同学")
		return
	}
	if target == uid {
		apiErr(w, 400, "不能移除自己")
		return
	}
	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	if err := s.dropMember(tx, id, target); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	c, _ := s.calendarByID(id, uid)
	writeJSON(w, 200, map[string]any{"calendar": c})
}

type shareReq struct {
	Shared *bool  `json:"shared"`
	Kind   string `json:"kind"`
}

// handleSetCalendarShared 对应界面上那个「共享」开关。
// 它与成员名单**互不干涉**：关掉共享只是让"以后再加人"需要重新打开，
// 已经分享出去的同学会继续看到这张日历（要收回请单独移除）。
func (s *Server) handleSetCalendarShared(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	role, ok := s.calendarRole(id, uid)
	if !ok {
		apiErr(w, 404, "日历不存在或无权访问")
		return
	}
	if !atLeast(role, RoleAdmin) {
		apiErr(w, 403, "只有日历创建者能改共享状态")
		return
	}
	var req shareReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		if req.Shared == nil {
			apiErr(w, 400, "缺少 shared")
			return
		}
		if *req.Shared {
			kind = "shared"
		} else {
			kind = "private"
		}
	}
	if kind != "shared" && kind != "private" && kind != "course" {
		apiErr(w, 400, "kind 只能是 shared / private / course")
		return
	}

	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	if err := s.setCalendarShared(tx, id, kind); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	c, _ := s.calendarByID(id, uid)
	writeJSON(w, 200, map[string]any{"calendar": c})
}

// handleSetPermissions 是权限页用的**一次性提交**接口。
//
//	PUT /api/calendars/{id}/permissions
//	{ "role": "shared",                       // 可选，等于共享开关；不传就不动
//	  "members": [ {"user_id":"u_x","role":"editor"}, ... ],
//	  "revoke":  ["u_y"] }
//
// 为什么要有"一次性提交"：老的界面是**每点一下复选框就发一个请求**，
// 于是改三个人的权限就有三个请求、三次 `resendCalendarMembers`、三个中间状态
// 会被同步下去；中间任何一个失败，界面和服务端就各信一半（用户看到的就是
// "设置没生效"）。这里把整次编辑收敛成一个事务：全成或全不成。
//
// 语义：
//   - members 里没有列出的现有成员**保持不动**（增量更新，不是全量替换）；
//   - revoke 里的显式移除；
//   - 自己（调用者）不允许被移除，也不允许把自己的角色改低 —— 否则会出现
//     "我把唯一的管理员降级了，谁也改不回来"的死局；
//   - owner（owner_id）永远不进 members 列表，也不可被移除。
func (s *Server) handleSetPermissions(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	role, ok := s.calendarRole(id, uid)
	if !ok {
		apiErr(w, 404, "日历不存在或无权访问")
		return
	}
	if !atLeast(role, RoleAdmin) {
		apiErr(w, 403, "只有日历创建者能改共享权限")
		return
	}

	var req struct {
		Role    string            `json:"role"`
		Kind    string            `json:"kind"`
		Members []memberReq       `json:"members"`
		Revoke  []string          `json:"revoke"`
		Names   map[string]string `json:"-"` // 兼容旧字段，忽略
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}

	// 目标用户先全部校验完再开事务：一次请求里混进了不存在的账号，
	// 应该整批失败，而不是"前三个成了、第四个没成"。
	known := map[string]bool{}
	rows, err := s.db.sql.Query(`SELECT id FROM users`)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	for rows.Next() {
		var x string
		if err := rows.Scan(&x); err == nil {
			known[x] = true
		}
	}
	rows.Close()

	var owner string
	if err := s.db.sql.QueryRow(`SELECT owner_id FROM calendars WHERE id=?`, id).Scan(&owner); err != nil {
		apiErr(w, 404, "日历不存在")
		return
	}

	for _, m := range req.Members {
		if m.UserID == "" || !known[m.UserID] {
			apiErr(w, 404, "名单里有不存在的账号："+m.UserID)
			return
		}
		if m.UserID == owner {
			apiErr(w, 400, "创建者不需要单独授权")
			return
		}
		if m.UserID == uid && m.UserID != owner {
			// 管理员给自己降级 = 把自己锁在门外，允许但要挡住"全部降级"这种自伤。
			if !isValidRole(m.Role) || roleRank(m.Role) < roleRank(RoleAdmin) {
				apiErr(w, 400, "不能降低自己的权限（否则就没人能再改这张日历了）")
				return
			}
		}
	}
	for _, target := range req.Revoke {
		if target == owner {
			apiErr(w, 400, "不能移除创建者")
			return
		}
		if target == uid {
			apiErr(w, 400, "不能移除自己")
			return
		}
	}

	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	changed := false
	if req.Role != "" || req.Kind != "" {
		kind := req.Kind
		if kind == "" {
			kind = req.Role
		}
		if kind != "shared" && kind != "private" && kind != "course" {
			apiErr(w, 400, "共享状态只能是 shared / private / course")
			return
		}
		if err := s.setCalendarShared(tx, id, kind); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		changed = true
	}
	for _, m := range req.Members {
		role := m.Role
		if !isValidRole(role) {
			role = RoleEditor
		}
		if role == RoleOwner {
			role = RoleAdmin // 转让所有权是另一个动作，不能顺手发生
		}
		if err := s.upsertMember(tx, id, m.UserID, role); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		changed = true
	}
	for _, target := range req.Revoke {
		if err := s.dropMember(tx, id, target); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		changed = true
	}
	// 只有"共享状态变了"而成员没变时，上面已经补发过了；这里不再重复补发。
	if !changed {
		if err := s.resendCalendarMembers(tx, id); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	c, _ := s.calendarByID(id, uid)
	writeJSON(w, 200, map[string]any{"calendar": c})
}

// handleLeaveCalendar 让成员自己退出共享（不是被踢）。
// 这跟 handleRemoveMember 是两回事：后者是管理员的操作，会校验"不能移除自己"。
func (s *Server) handleLeaveCalendar(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	role, ok := s.calendarRole(id, uid)
	if !ok {
		apiErr(w, 404, "日历不存在或你不是这张日历的成员")
		return
	}
	if role == RoleOwner {
		apiErr(w, 400, "创建者不能退出自己的日历，请直接删除")
		return
	}
	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	if err := s.dropMember(tx, id, uid); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// 它与成员名单**互不干涉**：关掉共享只是让"以后再加人"需要重新打开，
// 已经分享出去的同学会继续看到这张日历（要收回请单独移除）。

// calendarExists 判断日历行是否**还存在**（不区分是否已软删）。
//
// ★ 2026-09-28 补。用途只有一个：给"日程挂在一张已经没有任何日历行的日历上"
// 这种孤儿数据开一条退路 —— 此时任何基于 calendarRole 的权限判定必然失败
// （它 `SELECT owner_id FROM calendars WHERE id=?` 直接 ErrNoRows），
// 于是那条日程在客户端**看得见、删不掉**（用户报的就是这个）。
//
// ⚠️ 不要拿它当权限判据：它回答的是"这张日历的行还在不在"，
// 不是"我能不能动它"。权限一律走 calendarRole / canWrite。
func (s *Server) calendarExists(id string) bool {
	if id == "" {
		return false
	}
	var one int
	err := s.db.sql.QueryRow(`SELECT 1 FROM calendars WHERE id=?`, id).Scan(&one)
	return err == nil
}

// isAdminUser 是 requireAdmin 的"只要答案不要响应"版本。
//
// requireAdmin 会直接往 w 里写 401/403 并返回 true，用在"路由入口"上；
// 这里给它一个纯查询版本，好在别的判定里当**一个条件**用
// （例如"这条孤儿日程是不是该放行给管理员删"）。
// 查不到账号一律 false —— 默认值只能往收紧的方向偏。
func (s *Server) isAdminUser(uid string) bool {
	if uid == "" {
		return false
	}
	var isAdmin int
	if err := s.db.sql.QueryRow(`SELECT is_admin FROM users WHERE id=?`, uid).Scan(&isAdmin); err != nil {
		return false
	}
	return isAdmin != 0
}

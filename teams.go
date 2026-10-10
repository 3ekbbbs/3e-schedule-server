package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 团队（2026-09-21）
//
// **为什么要有团队**：原来的共享是"把日历一个个分享给一个个具体的人"。
// 一个班级要同步一张课表，得把三十个人挨个加一遍 —— 加人是逐人的、
// 每次都要 canManage、每加一次还要重挂同步流（红线 19）。
//
// 团队把这件事变成"**日历共享给团队一次**"。新同学拿**团队邀请码**自己进来，
// 不需要任何人再动那张日历的成员名单。
//
// ★★ 五条安全约束（每条都有闸门断言，改动前先读这里）
//
//   ① **只看得到自己已加入的团队。**
//      `GET /api/teams` 的 WHERE 里必须有 `team_members.user_id = 我`。
//      全服团队列表会泄漏"这个部署里有哪些组织"，是明确的隐私问题。
//      用户原话：「注意,仅显示自己已添加的团队,而不是全服的团队」
//
//   ② **团队身份 ≠ 日历管理权。** 团队共享带来的角色上限是 editor
//      （`teamRoleCeiling`）。团队**永远**给不出 owner/admin ——
//      否则任何团队管理员建个团队就能把别人日历的成员管理权收走。
//
//   ③ **共享给团队要双方都满足**：我对这张日历 `canManage`，
//      **且我是这个团队的成员**。缺任一条，任何人都能把别人的日历
//      投进任意团队（包括他自己刚建的团队）。
//
//   ④ **"先加入、后共享"**：这是用户明确的要求。所以
//      `POST /api/calendars/{id}/teams` 里要真的判成员资格，
//      而不是只在界面上藏起来。
//
//   ⑤ 团队**不加成员到日历**。团队共享走 `calendar_teams`，
//      `calendar_members` 一行都不动 —— 否则"退团"就得反向清理日历成员，
//      而那个反向清理一旦漏了就变成永久后门。
// ---------------------------------------------------------------------------

const (
	teamRoleAdmin  = "admin"  // 团队管理员：能邀人、能移除人
	teamRoleMember = "member" // 普通成员：只能看

	teamInvitePrefix = "fst_" // 和日历邀请的 fsi_ 区分开
	teamInviteBytes  = 32

	teamMaxNameLen = 40

	// 一个团队的成员上限。不是为了省资源，是为了防止"把邀请码发到公开群里
	// 然后被无限加入"变成一次不可逆的事故。
	teamMaxMembers = 500
)

func newTeamInviteCode() string {
	b := make([]byte, teamInviteBytes)
	if _, err := rand.Read(b); err != nil {
		// 熵源坏了不能"凑一个"出来
		panic("crypto/rand failed: " + err.Error())
	}
	return teamInvitePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// normalizeTeamRole 把团队角色收紧到「admin | member」。
func normalizeTeamRole(role string) (string, bool) {
	switch strings.TrimSpace(role) {
	case teamRoleAdmin:
		return teamRoleAdmin, true
	case teamRoleMember, "":
		return teamRoleMember, true
	default:
		return "", false
	}
}

// ---------------------------------------------------------------- 数据访问

type Team struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	OwnerID   string `json:"owner_id"`
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"`
	// 派生字段：调用者自己的团队角色 + 成员数。界面上直接可用，免得各端各算一套。
	MyRole    string `json:"my_role"`
	MemberCnt int64  `json:"member_count"`
	// 这个团队能看到几张日历（自己共享出去的）
	CalendarCnt int64 `json:"calendar_count"`
}

type TeamMember struct {
	TeamID   string `json:"team_id"`
	UserID   string `json:"user_id"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	JoinedAt int64  `json:"joined_at"`
}

func (s *Server) teamByID(id string) (*Team, bool) {
	var t Team
	err := s.db.sql.QueryRow(
		`SELECT id,name,owner_id,note,created_at FROM teams WHERE id=?`, id).
		Scan(&t.ID, &t.Name, &t.OwnerID, &t.Note, &t.CreatedAt)
	if err != nil {
		return nil, false
	}
	return &t, true
}

// teamMembership 返回我在这个团队里的角色。**这是团队侧唯一的归属判定入口。**
func (s *Server) teamMembership(teamID, uid string) (string, bool) {
	if teamID == "" || uid == "" {
		return "", false
	}
	var role string
	err := s.db.sql.QueryRow(
		`SELECT role FROM team_members WHERE team_id=? AND user_id=?`, teamID, uid).Scan(&role)
	if err != nil {
		return "", false
	}
	if role == "" {
		// 空角色一律按最低处理（和日历角色同一个原则）
		role = teamRoleMember
	}
	return role, true
}

// isTeamAdmin 团队管理员（或团队创建者）。**只用于团队内部治理**，
// 和日历的 canManageMembers 是两回事。
func (s *Server) isTeamAdmin(teamID, uid string) bool {
	role, ok := s.teamMembership(teamID, uid)
	if !ok {
		return false
	}
	if t, ok2 := s.teamByID(teamID); ok2 && t.OwnerID == uid {
		return true
	}
	return role == teamRoleAdmin
}

// fillTeamStats 把派生字段补上。
func (s *Server) fillTeamStats(t *Team, uid string) {
	if role, ok := s.teamMembership(t.ID, uid); ok {
		t.MyRole = role
		if t.OwnerID == uid {
			t.MyRole = teamRoleAdmin
		}
	}
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM team_members WHERE team_id=?`, t.ID).Scan(&t.MemberCnt)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM calendar_teams WHERE team_id=?`, t.ID).Scan(&t.CalendarCnt)
}

// ---------------------------------------------------------------- 团队 CRUD

// handleCreateTeam 建团队。创建者自动成为管理员。
func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var req struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		apiErr(w, 400, "团队名不能为空")
		return
	}
	if len([]rune(name)) > teamMaxNameLen {
		apiErr(w, 400, "团队名太长了")
		return
	}
	id := newID("tm_")
	now := time.Now().Unix()
	if err := s.withTx(func(tx *sql.Tx) error {
		if _, e := tx.Exec(
			`INSERT INTO teams(id,name,owner_id,note,created_at) VALUES(?,?,?,?,?)`,
			id, name, uid, req.Note, now); e != nil {
			return e
		}
		_, e := tx.Exec(
			`INSERT INTO team_members(team_id,user_id,role,joined_at) VALUES(?,?,?,?)`,
			id, uid, teamRoleAdmin, now)
		return e
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	t, _ := s.teamByID(id)
	s.fillTeamStats(t, uid)
	writeJSON(w, 200, map[string]any{"team": t})
}

// handleListTeams **只返回自己已加入的团队**。
//
// ⚠️⚠️ 这个 WHERE 是安全边界，不是过滤器便利。
// 少了 `tm.user_id=?` 就变成全服团队列表 —— 用户明确要求不许这样
// （「仅显示自己已添加的团队,而不是全服的团队」）。
// `scripts/team-gate.cjs` 里有专门的断言盯着这一行。
func (s *Server) handleListTeams(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	rows, err := s.db.sql.Query(
		`SELECT t.id FROM teams t
		 JOIN team_members tm ON tm.team_id = t.id
		 WHERE tm.user_id = ?
		 ORDER BY t.created_at DESC`, uid)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	out := []Team{}
	for _, id := range ids {
		if t, ok := s.teamByID(id); ok {
			s.fillTeamStats(t, uid)
			out = append(out, *t)
		}
	}
	writeJSON(w, 200, map[string]any{"teams": out})
}

// handleTeamDetail 团队详情 + 成员名单。**非成员一律 404**。
//
// 为什么不返回 403：403 会告诉对方"这个团队存在"。
// 团队的存在本身就是隐私（见文件头约束 ①）。
func (s *Server) handleTeamDetail(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	if _, ok := s.teamMembership(id, uid); !ok {
		apiErr(w, 404, "团队不存在")
		return
	}
	t, ok := s.teamByID(id)
	if !ok {
		apiErr(w, 404, "团队不存在")
		return
	}
	s.fillTeamStats(t, uid)

	rows, err := s.db.sql.Query(
		`SELECT tm.team_id,tm.user_id,COALESCE(u.name,''),tm.role,tm.joined_at
		 FROM team_members tm LEFT JOIN users u ON u.id = tm.user_id
		 WHERE tm.team_id=? ORDER BY tm.joined_at`, id)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	members := []TeamMember{}
	for rows.Next() {
		var m TeamMember
		if err := rows.Scan(&m.TeamID, &m.UserID, &m.Name, &m.Role, &m.JoinedAt); err == nil {
			members = append(members, m)
		}
	}
	rows.Close()

	// 这个团队能看到哪些日历（只给名字和角色，不给内容 —— 内容走同步流）
	calRows, _ := s.db.sql.Query(
		`SELECT ct.calendar_id, ct.role, COALESCE(c.name,'')
		 FROM calendar_teams ct LEFT JOIN calendars c ON c.id = ct.calendar_id
		 WHERE ct.team_id=?`, id)
	cals := []map[string]any{}
	if calRows != nil {
		for calRows.Next() {
			var calID, role, name string
			if err := calRows.Scan(&calID, &role, &name); err == nil {
				cals = append(cals, map[string]any{"calendar_id": calID, "role": role, "name": name})
			}
		}
		calRows.Close()
	}

	writeJSON(w, 200, map[string]any{"team": t, "members": members, "calendars": cals})
}

// handleLeaveTeam 退出团队。**创建者不能退**（会把团队变成没人管的孤儿）。
func (s *Server) handleLeaveTeam(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	t, ok := s.teamByID(id)
	if !ok {
		apiErr(w, 404, "团队不存在")
		return
	}
	if t.OwnerID == uid {
		apiErr(w, 409, "你是这个团队的创建者，不能退出（可以把团队交给别人或删掉）")
		return
	}
	if _, isMember := s.teamMembership(id, uid); !isMember {
		apiErr(w, 404, "你不在这个团队里")
		return
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		_, e := tx.Exec(`DELETE FROM team_members WHERE team_id=? AND user_id=?`, id, uid)
		return e
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	// 退团之后他看不到那些日历了 —— 但**不需要**重挂同步流：
	// 同步流是按人过滤的（loadEntities 里做），拉不到就是拉不到。
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleRemoveTeamMember 团队管理员移除成员。
func (s *Server) handleRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	teamID := r.PathValue("id")
	target := r.PathValue("uid")

	if !s.isTeamAdmin(teamID, uid) {
		apiErr(w, 403, "只有团队管理员可以移除成员")
		return
	}
	t, ok := s.teamByID(teamID)
	if !ok {
		apiErr(w, 404, "团队不存在")
		return
	}
	// **创建者不可被移除** —— 否则团队可能被管理员"夺走"
	if target == t.OwnerID {
		apiErr(w, 409, "不能移除团队创建者")
		return
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		_, e := tx.Exec(`DELETE FROM team_members WHERE team_id=? AND user_id=?`, teamID, target)
		return e
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleSetTeamMemberRole 改成员角色（admin ↔ member）。仅团队管理员。
func (s *Server) handleSetTeamMemberRole(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	teamID := r.PathValue("id")
	target := r.PathValue("uid")
	if !s.isTeamAdmin(teamID, uid) {
		apiErr(w, 403, "只有团队管理员可以改角色")
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	role, ok := normalizeTeamRole(req.Role)
	if !ok {
		apiErr(w, 400, "团队角色只能是 admin 或 member")
		return
	}
	if t, ok2 := s.teamByID(teamID); ok2 && t.OwnerID == target && role != teamRoleAdmin {
		apiErr(w, 409, "团队创建者必须保持管理员")
		return
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		_, e := tx.Exec(
			`UPDATE team_members SET role=? WHERE team_id=? AND user_id=?`, role, teamID, target)
		return e
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- 团队邀请

type TeamInvite struct {
	ID        string `json:"id"`
	Code      string `json:"code"`
	TeamID    string `json:"team_id"`
	Role      string `json:"role"`
	CreatedBy string `json:"created_by"`
	ExpiresAt int64  `json:"expires_at"`
	MaxUses   int64  `json:"max_uses"`
	UsedCount int64  `json:"used_count"`
	RevokedAt int64  `json:"revoked_at"`
	CreatedAt int64  `json:"created_at"`
	Expired   bool   `json:"expired"`
	Revoked   bool   `json:"revoked"`
	Exhaust   bool   `json:"exhausted"`
}

func (s *Server) teamInviteByID(id string) (*TeamInvite, bool) {
	var v TeamInvite
	err := s.db.sql.QueryRow(
		`SELECT id,code,team_id,role,created_by,expires_at,max_uses,used_count,revoked_at,created_at
		 FROM team_invites WHERE id=?`, id).
		Scan(&v.ID, &v.Code, &v.TeamID, &v.Role, &v.CreatedBy, &v.ExpiresAt,
			&v.MaxUses, &v.UsedCount, &v.RevokedAt, &v.CreatedAt)
	if err != nil {
		return nil, false
	}
	return &v, true
}

func (s *Server) teamInviteByCode(code string) (*TeamInvite, bool) {
	var v TeamInvite
	err := s.db.sql.QueryRow(
		`SELECT id,code,team_id,role,created_by,expires_at,max_uses,used_count,revoked_at,created_at
		 FROM team_invites WHERE code=?`, code).
		Scan(&v.ID, &v.Code, &v.TeamID, &v.Role, &v.CreatedBy, &v.ExpiresAt,
			&v.MaxUses, &v.UsedCount, &v.RevokedAt, &v.CreatedAt)
	if err != nil {
		return nil, false
	}
	return &v, true
}

func (v *TeamInvite) toJSON() *TeamInvite {
	now := time.Now().Unix()
	v.Expired = v.ExpiresAt > 0 && now > v.ExpiresAt
	v.Revoked = v.RevokedAt != 0
	v.Exhaust = v.MaxUses > 0 && v.UsedCount >= v.MaxUses
	return v
}

// teamInviteUnusableReason 团队邀请能不能用，返回**人能看懂的原因**。
// 注册/加入/查看三处共用同一个判定（分两套判法迟早分叉 —— 日历邀请那边踩过）。
func teamInviteUnusableReason(v *TeamInvite) string {
	now := time.Now().Unix()
	switch {
	case v.RevokedAt != 0:
		return "已被撤销"
	case v.ExpiresAt > 0 && now > v.ExpiresAt:
		return "已过期"
	case v.MaxUses > 0 && v.UsedCount >= v.MaxUses:
		return "已用尽"
	}
	return ""
}

func (s *Server) handleCreateTeamInvite(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	teamID := r.PathValue("id")
	if !s.isTeamAdmin(teamID, uid) {
		apiErr(w, 403, "只有团队管理员可以生成邀请码")
		return
	}
	var req struct {
		Role     string `json:"role"`
		TTLHours int64  `json:"ttl_hours"`
		MaxUses  int64  `json:"max_uses"`
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	role, ok := normalizeTeamRole(req.Role)
	if !ok {
		apiErr(w, 400, "团队角色只能是 admin 或 member")
		return
	}
	ttl := req.TTLHours
	if ttl <= 0 {
		ttl = 7 * 24
	}
	if ttl > 365*24 {
		ttl = 365 * 24
	}
	uses := req.MaxUses
	if uses < 0 {
		uses = 0
	}
	// 团队邀请默认**不限次数**（要给一个班的人）—— 和日历邀请（默认 1 次）故意不同。
	// 传 0 就是不限。
	id := newID("ti_")
	code := newTeamInviteCode()
	now := time.Now().Unix()
	if err := s.withTx(func(tx *sql.Tx) error {
		_, e := tx.Exec(
			`INSERT INTO team_invites(id,code,team_id,role,created_by,expires_at,max_uses,used_count,revoked_at,created_at)
			 VALUES(?,?,?,?,?,?,?,0,0,?)`,
			id, code, teamID, role, uid, now+ttl*3600, uses, now)
		return e
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	inv, _ := s.teamInviteByID(id)
	writeJSON(w, 200, map[string]any{"invite": inv.toJSON()})
}

func (s *Server) handleListTeamInvites(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	teamID := r.PathValue("id")
	if !s.isTeamAdmin(teamID, uid) {
		apiErr(w, 403, "只有团队管理员可以查看邀请码")
		return
	}
	rows, err := s.db.sql.Query(
		`SELECT id FROM team_invites WHERE team_id=? ORDER BY created_at DESC LIMIT 100`, teamID)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	out := []*TeamInvite{}
	for _, id := range ids {
		if v, ok := s.teamInviteByID(id); ok {
			out = append(out, v.toJSON())
		}
	}
	writeJSON(w, 200, map[string]any{"invites": out})
}

func (s *Server) handleRevokeTeamInvite(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	v, ok := s.teamInviteByID(id)
	if !ok {
		apiErr(w, 404, "邀请不存在")
		return
	}
	if v.CreatedBy != uid && !s.isTeamAdmin(v.TeamID, uid) {
		apiErr(w, 403, "只有创建者或团队管理员可以撤销")
		return
	}
	if v.RevokedAt != 0 {
		writeJSON(w, 200, map[string]any{"ok": true, "already": true})
		return
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE team_invites SET revoked_at=? WHERE id=?`, time.Now().Unix(), id)
		return e
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handlePeekTeamInvite 匿名读团队邀请的元信息（落地页用）。
//
// **只返回"能不能用 + 哪个团队 + 谁邀的 + 什么角色 + 为什么不可用"**，
// 绝不返回成员名单或日历内容 —— 这是个不需要登录的端点。
func (s *Server) handlePeekTeamInvite(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	invalid := func(reason string) {
		writeJSON(w, 200, map[string]any{
			"valid": false, "team_name": "", "inviter_name": "", "role": "", "reason": reason,
		})
	}
	if !strings.HasPrefix(code, teamInvitePrefix) {
		invalid("not_found")
		return
	}
	v, ok := s.teamInviteByCode(code)
	if !ok {
		invalid("not_found")
		return
	}
	if reason := teamInviteUnusableReason(v); reason != "" {
		invalid(map[string]string{
			"已被撤销": "revoked", "已过期": "expired", "已用尽": "exhausted",
		}[reason])
		return
	}
	var teamName string
	_ = s.db.sql.QueryRow(`SELECT name FROM teams WHERE id=?`, v.TeamID).Scan(&teamName)
	var inviter string
	_ = s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, v.CreatedBy).Scan(&inviter)
	writeJSON(w, 200, map[string]any{
		"valid": true, "team_name": teamName, "inviter_name": inviter,
		"role": v.Role, "reason": "",
	})
}

// handleAcceptTeamInvite 加入团队。
//
// ★ 幂等：已经是成员就直接成功，**不消耗次数**。
// ★ 占坑用**条件 UPDATE**（`used_count < max_uses`）挡 TOCTOU ——
//
//	两个请求同时用一张一次性团队邀请时，只有一条能命中。
//
// ★ 满员（teamMaxMembers）拒绝 —— 免得公开群的邀请码把团队撑爆。
func (s *Server) handleAcceptTeamInvite(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	code := strings.TrimSpace(r.PathValue("code"))
	v, ok := s.teamInviteByCode(code)
	if !ok {
		apiErr(w, 404, "邀请不存在")
		return
	}
	if reason := teamInviteUnusableReason(v); reason != "" {
		apiErr(w, 410, "邀请"+reason)
		return
	}
	// 幂等
	if _, isMember := s.teamMembership(v.TeamID, uid); isMember {
		writeJSON(w, 200, map[string]any{"ok": true, "already_member": true, "team_id": v.TeamID})
		return
	}
	var n int64
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM team_members WHERE team_id=?`, v.TeamID).Scan(&n)
	if n >= teamMaxMembers {
		apiErr(w, 409, "团队人数已满")
		return
	}

	role, okRole := normalizeTeamRole(v.Role)
	if !okRole {
		role = teamRoleMember // 库里存了非法角色 → 按最低处理，绝不放行
	}

	now := time.Now().Unix()
	if err := s.withTx(func(tx *sql.Tx) error {
		res, e := tx.Exec(
			`UPDATE team_invites SET used_count=used_count+1 WHERE id=? AND revoked_at=0
			   AND (expires_at=0 OR expires_at>?) AND (max_uses=0 OR used_count<max_uses)`,
			v.ID, now)
		if e != nil {
			return e
		}
		if k, e := res.RowsAffected(); e != nil {
			return e
		} else if k == 0 {
			return errTeamInviteGone
		}
		_, e = tx.Exec(
			`INSERT INTO team_members(team_id,user_id,role,joined_at) VALUES(?,?,?,?)
			 ON CONFLICT(team_id,user_id) DO NOTHING`,
			v.TeamID, uid, role, now)
		return e
	}); err != nil {
		if err == errTeamInviteGone {
			apiErr(w, 410, "这张邀请已被用掉")
			return
		}
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "team_id": v.TeamID, "role": role})
}

var errTeamInviteGone = &teamInviteGoneError{}

type teamInviteGoneError struct{}

func (*teamInviteGoneError) Error() string { return "team invite gone" }

// ---------------------------------------------------------------- 日历 ↔ 团队

// handleShareCalendarToTeam 把一张日历共享给一个团队。
//
// ★★ **双方都要满足**（文件头约束 ③④）：
//
//	· 我对这张日历 `canManageMembers`（能管人才谈得上对外共享）
//	· **我是这个团队的成员** —— 这是用户明确要求的"先加入、后共享"。
//	  少了这一条，任何人都能把别人的日历投进他自己刚建的团队。
//
// ★ 角色**强制上限 editor**（teamRoleCeiling），传更高的直接 400，
//
//	**不静默降级** —— 静默降级会让共享者以为自己给了 admin。
func (s *Server) handleShareCalendarToTeam(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	calID := r.PathValue("id")
	if !s.canManageMembers(calID, uid) {
		apiErr(w, 403, "只有日历拥有者或管理员可以共享给团队")
		return
	}
	var req struct {
		TeamID string `json:"team_id"`
		Role   string `json:"role"`
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	teamID := strings.TrimSpace(req.TeamID)
	if teamID == "" {
		apiErr(w, 400, "缺少 team_id")
		return
	}
	// ★ 必须是我自己已加入的团队
	if _, isMember := s.teamMembership(teamID, uid); !isMember {
		apiErr(w, 403, "你不在这个团队里，先加入才能共享")
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = RoleViewer
	}
	// 只认 viewer / editor；更高的一律 400（不静默降级）
	if role != RoleViewer && role != RoleEditor {
		apiErr(w, 400, "团队能拿到的角色只能是 editor 或 viewer（团队不能获得 admin/owner）")
		return
	}
	// 角色已经被上面限定成 viewer|editor 两种，而 teamRoleCeiling 就是 editor ——
	// 所以到这里**不可能**出现越界值。不要再写"再判一次上限"的死代码：
	// 那种代码看起来像多一层保险，实际只是让人以为上面那个判断不充分。

	if err := s.withTx(func(tx *sql.Tx) error {
		if _, e := tx.Exec(
			`INSERT INTO calendar_teams(calendar_id,team_id,role,shared_by,created_at) VALUES(?,?,?,?,?)
			 ON CONFLICT(calendar_id,team_id) DO UPDATE SET role=excluded.role`,
			calID, teamID, role, uid, time.Now().Unix()); e != nil {
			return e
		}
		// ★ 共享给团队 = 一批人**新看到**这张日历的内容，
		// 所以必须重挂同步流，否则团队里的人拉到的是一张空日历（红线 19）。
		// 两步都要：抬日历自己（否则"多了张日历"）+ 抬里面的日程（否则"打开是空的"）。
		if e := s.resendCalendarMembers(tx, calID); e != nil {
			return e
		}
		return s.resendCalendarContents(tx, calID)
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "calendar_id": calID, "team_id": teamID, "role": role})
}

// handleUnshareCalendarFromTeam 取消共享。
//
// 同样要重挂 —— 否则被取消的团队**仍然能在本地看到**那张日历的旧内容
// （它们的 sync 游标已经过了那些 seq，不重挂就永远收不到"日历没了"）。
func (s *Server) handleUnshareCalendarFromTeam(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	calID := r.PathValue("id")
	teamID := r.PathValue("teamId")
	if !s.canManageMembers(calID, uid) {
		apiErr(w, 403, "只有日历拥有者或管理员可以取消共享")
		return
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		if _, e := tx.Exec(
			`DELETE FROM calendar_teams WHERE calendar_id=? AND team_id=?`, calID, teamID); e != nil {
			return e
		}
		if e := s.resendCalendarMembers(tx, calID); e != nil {
			return e
		}
		return s.resendCalendarContents(tx, calID)
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleListCalendarTeams 这张日历共享给了哪些团队（需 canManage）。
func (s *Server) handleListCalendarTeams(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	calID := r.PathValue("id")
	if !s.canManageMembers(calID, uid) {
		apiErr(w, 403, "只有日历拥有者或管理员可以查看共享情况")
		return
	}
	rows, err := s.db.sql.Query(
		`SELECT ct.team_id, ct.role, COALESCE(t.name,'') FROM calendar_teams ct
		 LEFT JOIN teams t ON t.id = ct.team_id WHERE ct.calendar_id=?`, calID)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for rows.Next() {
		var teamID, role, name string
		if err := rows.Scan(&teamID, &role, &name); err == nil {
			out = append(out, map[string]any{"team_id": teamID, "role": role, "name": name})
		}
	}
	rows.Close()
	writeJSON(w, 200, map[string]any{"shares": out})
}

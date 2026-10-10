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
// 邀请入伙（2026-09-19，跨端方案 §9.3）
//
// **为什么不能把全站邀请码下放给普通成员**：
// `GET /api/invite` 那个是全站级注册凭证 —— 谁能拿到，谁就能往这个部署里注册账号。
// 下放给任意成员，等于"任何成员都能往部署里放人"，而"这个部署里有几张日历是私有的"
// 完全挡不住这一步。
//
// 所以邀请必须**绑死目标日历 + 预授权角色**，并且带有效期、可撤销、可看使用情况。
//
// 四条硬约束（方案 §9.3，每条都有闸门断言）：
//   ① 生成者必须对该日历 canManage
//   ② **请求 role 传 admin/owner → 400**。不许静默降级成 editor ——
//      静默降级会让邀请人以为自己给的是 admin（这类"我以为"是最难查的）
//   ③ 服务端强制角色上限 editor
//   ④ 注册时角色**取 token 里的值，绝不取请求里的值**
// ---------------------------------------------------------------------------

const (
	inviteKindInvite   = "invite"
	inviteKindReadonly = "readonly" // 二期只读链接复用同一张表

	inviteCodePrefix = "fsi_"
	// inviteCodeBytes 32 字节 = 256 位随机。
	// 匿名端点要靠它防枚举 —— 256 位下穷举不可行，这是主要的防线。
	inviteCodeBytes = 32

	inviteTTLDefaultHours = 7 * 24
	inviteTTLMaxHours     = 365 * 24
	inviteMaxUsesDefault  = 1
	inviteMaxUsesLimit    = 100
)

func newInviteCode() string {
	b := make([]byte, inviteCodeBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败意味着系统熵源坏了，不能"凑一个"出来
		panic("crypto/rand failed: " + err.Error())
	}
	return inviteCodePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// normalizeInviteRole 把请求里的 role 收紧到「editor | viewer」。
//
// ⚠️ 返回 ok=false 表示**必须报错**，不是"降级处理"。
// 传 admin/owner 时静默降级为 editor 是错的：邀请人以为自己给的是 admin，
// 事后发现不是，而这条路径上没有任何提示。
func normalizeInviteRole(role string) (string, bool) {
	switch strings.TrimSpace(role) {
	case RoleEditor:
		return RoleEditor, true
	case RoleViewer:
		return RoleViewer, true
	default:
		return "", false
	}
}

type inviteRow struct {
	ID         string `json:"id"`
	Code       string `json:"code"`
	Kind       string `json:"kind"`
	CalendarID string `json:"calendar_id"`
	Role       string `json:"role"`
	CreatedBy  string `json:"created_by"`
	ExpiresAt  int64  `json:"expires_at"`
	MaxUses    int64  `json:"max_uses"`
	UsedCount  int64  `json:"used_count"`
	RevokedAt  int64  `json:"revoked_at"`
	CreatedAt  int64  `json:"created_at"`
	// 下面两个是给列表 UI 直接用的派生字段，避免各端各算一套
	Expired bool `json:"expired"`
	Revoked bool `json:"revoked"`
	Exhaust bool `json:"exhausted"`
}

func (r inviteRow) toJSON() inviteRow {
	r.Expired = r.ExpiresAt > 0 && time.Now().Unix() > r.ExpiresAt
	r.Revoked = r.RevokedAt != 0
	r.Exhaust = r.MaxUses > 0 && r.UsedCount >= r.MaxUses
	return r
}

func (s *Server) inviteByID(id string) (*inviteRow, bool) {
	var r inviteRow
	err := s.db.sql.QueryRow(`
		SELECT id,code,kind,calendar_id,role,created_by,expires_at,max_uses,used_count,revoked_at,created_at
		FROM invites WHERE id=?`, id).
		Scan(&r.ID, &r.Code, &r.Kind, &r.CalendarID, &r.Role, &r.CreatedBy,
			&r.ExpiresAt, &r.MaxUses, &r.UsedCount, &r.RevokedAt, &r.CreatedAt)
	if err != nil {
		return nil, false
	}
	return &r, true
}

func (s *Server) inviteByCode(code string) (*inviteRow, bool) {
	var r inviteRow
	err := s.db.sql.QueryRow(`
		SELECT id,code,kind,calendar_id,role,created_by,expires_at,max_uses,used_count,revoked_at,created_at
		FROM invites WHERE code=?`, code).
		Scan(&r.ID, &r.Code, &r.Kind, &r.CalendarID, &r.Role, &r.CreatedBy,
			&r.ExpiresAt, &r.MaxUses, &r.UsedCount, &r.RevokedAt, &r.CreatedAt)
	if err != nil {
		return nil, false
	}
	return &r, true
}

// ---------- 生成 / 列表 / 撤销 ----------

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	calID := r.PathValue("id")
	if !s.canManageMembers(calID, uid) {
		apiErr(w, 403, "只有管理员或拥有者可以邀请")
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
	role, ok := normalizeInviteRole(req.Role)
	if !ok {
		// 明确报错，**不许静默降级**（见文件头硬约束 ②）
		apiErr(w, 400, "角色只能是 editor 或 viewer（邀请不能授予 admin/owner）")
		return
	}
	ttl := req.TTLHours
	if ttl <= 0 {
		ttl = inviteTTLDefaultHours
	}
	if ttl > inviteTTLMaxHours {
		ttl = inviteTTLMaxHours
	}
	uses := req.MaxUses
	if uses <= 0 {
		uses = inviteMaxUsesDefault
	}
	if uses > inviteMaxUsesLimit {
		uses = inviteMaxUsesLimit
	}

	id := newID("iv_")
	code := newInviteCode()
	now := time.Now().Unix()
	expires := now + ttl*3600

	if err := s.withTx(func(tx *sql.Tx) error {
		_, e := tx.Exec(`
			INSERT INTO invites(id,code,kind,calendar_id,role,created_by,expires_at,max_uses,used_count,revoked_at,created_at)
			VALUES(?,?,?,?,?,?,?,?,0,0,?)`,
			id, code, inviteKindInvite, calID, role, uid, expires, uses, now)
		return e
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	row, _ := s.inviteByID(id)
	writeJSON(w, 200, map[string]any{"invite": row.toJSON()})
}

func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	calID := r.PathValue("id")
	if !s.canManageMembers(calID, uid) {
		apiErr(w, 403, "只有管理员或拥有者可以查看邀请")
		return
	}
	rows, err := s.db.sql.Query(
		`SELECT id FROM invites WHERE calendar_id=? AND kind=? ORDER BY created_at DESC LIMIT 100`,
		calID, inviteKindInvite)
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
	out := []inviteRow{}
	for _, id := range ids {
		if row, ok := s.inviteByID(id); ok {
			out = append(out, row.toJSON())
		}
	}
	writeJSON(w, 200, map[string]any{"invites": out})
}

// handleRevokeInvite 撤销。创建者本人或该日历的 owner 都能撤。
func (s *Server) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	row, ok := s.inviteByID(id)
	if !ok {
		apiErr(w, 404, "邀请不存在")
		return
	}
	if row.CreatedBy != uid {
		// 只有创建者和日历 owner 能撤。**admin 不算** ——
		// 撤销别人的邀请是"推翻别人已经做出的授权决定"，比管成员更进一步，
		// 所以这里比 canManageMembers 更严。用 calendarRole 直接判 owner。
		role, ok := s.calendarRole(row.CalendarID, uid)
		if !ok || role != RoleOwner {
			apiErr(w, 403, "只有创建者或日历拥有者可以撤销")
			return
		}
	}
	if row.RevokedAt != 0 {
		writeJSON(w, 200, map[string]any{"ok": true, "already": true})
		return
	}
	if err := s.withTx(func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE invites SET revoked_at=? WHERE id=?`, time.Now().Unix(), id)
		return e
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 匿名查看（落地页用） ----------

// handlePeekInvite 匿名读一张邀请的**元信息**。
//
// 只返回「有效吗 + 是哪张日历 + 谁邀的 + 什么角色 + 什么原因无效」，
// **绝不返回日历内容或成员名单** —— 这是一个不需要登录的端点，
// 它唯一该做的事就是让落地页能把话说明白。
//
// 返回 200 而不是 404（即使 code 不存在）：
// 让"不存在"和"存在但过期"在**状态码层面**无法区分，配合 256 位随机码一起防枚举。
// 区分信息放在 body 的 `reason` 里，那是给**持有正确 code 的人**看的。
func (s *Server) handlePeekInvite(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	invalid := func(reason string) {
		writeJSON(w, 200, map[string]any{
			"valid": false, "kind": "", "calendar_name": "",
			"inviter_name": "", "role": "", "reason": reason,
		})
	}
	if !strings.HasPrefix(code, inviteCodePrefix) {
		invalid("not_found")
		return
	}
	row, ok := s.inviteByCode(code)
	if !ok {
		invalid("not_found")
		return
	}
	now := time.Now().Unix()
	switch {
	case row.RevokedAt != 0:
		invalid("revoked")
		return
	case row.ExpiresAt > 0 && now > row.ExpiresAt:
		invalid("expired")
		return
	case row.MaxUses > 0 && row.UsedCount >= row.MaxUses:
		invalid("exhausted")
		return
	}
	var calName string
	_ = s.db.sql.QueryRow(`SELECT name FROM calendars WHERE id=?`, row.CalendarID).Scan(&calName)
	var inviter string
	_ = s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, row.CreatedBy).Scan(&inviter)
	writeJSON(w, 200, map[string]any{
		"valid":         true,
		"kind":          row.Kind,
		"calendar_name": calName,
		"inviter_name":  inviter,
		"role":          row.Role,
		"reason":        "",
	})
}

// ---------- 接受 ----------

// handleAcceptInvite 已登录用户接受邀请。**幂等**：已是成员就直接成功。
//
// ⚠️ 角色**取 token 里的值**（row.Role），绝不取请求里的值（硬约束 ④）。
// 否则"邀请的是 viewer、接受时自己改成 editor"就是个提权漏洞。
func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	code := strings.TrimSpace(r.PathValue("code"))
	row, ok := s.inviteByCode(code)
	if !ok {
		apiErr(w, 404, "邀请不存在")
		return
	}
	s.acceptInviteRow(w, row, uid)
}

// acceptInviteRow 是"接受邀请"的唯一实现 ——
// REST 那条路和"注册时带 invite_code"那条路都走它，避免两套逻辑分叉。
func (s *Server) acceptInviteRow(w http.ResponseWriter, row *inviteRow, uid string) {
	now := time.Now().Unix()
	// 和注册那条路**共用同一个判定**（inviteUnusableReason）。
	// 分两套判法迟早会分叉：一处放过了、另一处拦住了，而两边的错误文案还不一样。
	if reason := inviteUnusableReason(row); reason != "" {
		apiErr(w, 410, "邀请"+reason)
		return
	}
	if row.CalendarID == "" {
		apiErr(w, 500, "邀请没有绑定日历")
		return
	}
	// 已经是成员 → 幂等成功，不消耗次数
	if _, isMember := s.calendarRole(row.CalendarID, uid); isMember {
		writeJSON(w, 200, map[string]any{
			"ok": true, "already_member": true, "calendar_id": row.CalendarID, "role": row.Role,
		})
		return
	}

	role, okRole := normalizeInviteRole(row.Role)
	if !okRole {
		// 库里存了非法角色（手工改库 / 老数据）→ 按最低权限处理，绝不放行
		role = RoleViewer
	}

	if err := s.withTx(func(tx *sql.Tx) error {
		// 占坑：把 used_count+1 和"加成员"放进同一个事务。
		// ⚠️ 这里用条件更新而不是先读后写 —— 两个请求同时接受同一张一次性邀请时，
		// 只有一条 UPDATE 能命中 `used_count < max_uses`，
		// 另一条影响 0 行 → 报"已用尽"。先读后写会让两张都通过。
		res, e := tx.Exec(
			`UPDATE invites SET used_count=used_count+1 WHERE id=? AND revoked_at=0
			   AND (expires_at=0 OR expires_at>?) AND (max_uses=0 OR used_count<max_uses)`,
			row.ID, now)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			return errInviteGone
		}
		_, e = tx.Exec(
			`INSERT INTO calendar_members(calendar_id,user_id,role) VALUES(?,?,?)
			 ON CONFLICT(calendar_id,user_id) DO UPDATE SET role=excluded.role`,
			row.CalendarID, uid, role)
		if e != nil {
			return e
		}
		// 加了人必须补发成员变更，否则对方拿到一张空日历（红线 19）
		return s.resendCalendarMembers(tx, row.CalendarID)
	}); err != nil {
		if err == errInviteGone {
			apiErr(w, 410, "邀请已过期、被撤销或用尽")
			return
		}
		apiErr(w, 500, err.Error())
		return
	}

	writeJSON(w, 200, map[string]any{
		"ok": true, "calendar_id": row.CalendarID, "role": role,
	})
}

// errInviteGone 邀请在事务里失效（并发下被别人先用掉了）。
var errInviteGone = &inviteGoneError{}

type inviteGoneError struct{}

func (*inviteGoneError) Error() string { return "invite gone" }

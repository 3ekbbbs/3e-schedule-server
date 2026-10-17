package app

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type ctxKey string

const ctxUserID ctxKey = "uid"

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if tok == "" {
			apiErr(w, http.StatusUnauthorized, "missing token")
			return
		}
		var uid string
		err := s.db.sql.QueryRow(`SELECT user_id FROM tokens WHERE token=?`, tok).Scan(&uid)
		if err != nil {
			apiErr(w, http.StatusUnauthorized, "invalid token")
			return
		}
		// ⚠️ 封禁判定放在**中间件**里，不放各个 handler（见 banGuard 的说明）。
		if s.banGuard(w, uid) {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUserID, uid)))
	}
}

// banGuard 统一处理"这个账号已被封禁"。
//
// 返回 true = 已经写了响应，调用方**必须立刻 return**。
//
// ★ 为什么放在中间件里而不是各个 handler 里：
//
//	本项目有**两条**鉴权入口（`auth` / `authOrKey`），下面挂着上百条路由。
//	逐个 handler 加判断，漏掉一个就是一个越权后门 —— 而漏掉的那个一定是
//	最不常用的那条，也就是最难被发现的那条。放在中间件里，
//	"所有需要登录的接口"自动全部覆盖，加新路由也不用记得补。
//
// ★ 为什么不是"封禁时就删掉 token"就完了：
//
//	那是**两件事**。删 token 解决"已经登录的设备立刻失效"，
//	这里解决"凭证其实还能用"（比如 API Key、以及任何我们没想到的凭证）。
//	两个都要有 —— 红线"兜底只拦物理上不可能的"在这里反过来用：
//	封禁是产品判断，所以要在**最靠外那一层**执行，越靠里越容易漏。
func (s *Server) banGuard(w http.ResponseWriter, uid string) bool {
	if strings.TrimSpace(uid) == "" {
		return false
	}
	var banned int64
	var reason string
	var until int64
	err := s.db.sql.QueryRow(`SELECT banned, ban_reason, ban_until FROM users WHERE id=?`, uid).
		Scan(&banned, &reason, &until)
	if err != nil {
		// 查不到行（账号被硬删）时**不在这里报错** —— 各接口自己会给出更合适的
		// 说法（比如 404 / 401）。这里只负责"确认被封禁"这一件事。
		return false
	}
	if !s.banActive(uid, banned, until, time.Now().Unix()) {
		return false
	}
	apiErrDetail(w, http.StatusForbidden, "banned", banMessage(reason, until), nil)
	return true
}

// banActive 判断"这条封禁现在还有效吗"；**已过期就顺手解开**。
//
// ★ 为什么是"懒到期"，而不是后台清扫协程：
//
//	这个服务端**没有任何定时任务**（服务器上那三个计划任务是起进程用的）。
//	加一个 goroutine 就要连带考虑它和 SQLite 单写者的竞争、以及重启后的补偿 ——
//	而"到期的封禁"本来就是**只影响一个人**的小事：第一个人来敲门时顺手翻掉就够，
//	之后所有判断走同一条路（`banned=0`）。
//
// ⚠️ 判据只有一句 `until > 0 && until <= now`：**0 表示永久**，没有第二种"无期限"
//
//	的表示法（不加 NULL 那一路），所以不会出现"NULL 和 0 哪个是永久"的分歧。
func (s *Server) banActive(uid string, banned int64, until, now int64) bool {
	if banned == 0 {
		return false
	}
	if until > 0 && until <= now {
		// 一次性翻掉。条件里带 `banned<>0` 是为了并发下不重复写。
		//
		// ⚠️ 2026-10-17 改（第十轮审计 N3）：错误**不再直接丢掉**。
		//    这个 UPDATE 失败时用户仍会被当成"被封"（但下个请求会重走这条路径重试，
		//    属于自愈）—— 所以原来判成"可接受降级"是对的。
		//    但**连日志都没有**就变成了"查不到"：如果它一直失败（库被锁死、磁盘满），
		//    症状是"这个用户永远登不上"，而日志里一个字都没有。
		//    自愈不等于不该留痕。
		if _, err := s.db.sql.Exec(`UPDATE users SET banned=0, ban_until=0 WHERE id=? AND banned<>0`, uid); err != nil {
			log.Printf("解封写失败（uid=%s）：%v —— 下个请求会重试", uid, err)
		}
		return false
	}
	return true
}

// cnZone 是给"人看的时间"用的固定时区。
//
// ⚠️ 不用 `time.Local`：那是**进程环境的**时区，服务器换一台机器就可能变，
//
//	同一句文案在两台机器上会显示成不同的时间。封禁文案是要给人看的，
//	必须定死（`recur.go` 那边用 LoadLocation 是因为它要算日程展开，
//	这里只是显示）。
var cnZone = time.FixedZone("CST", 8*3600)

// banMessage 拼"给被封的人看的那句话"。
//
// 两件事必须都在里面：**为什么**（理由）和**到什么时候**（期限）。
// 少任何一个，用户能做的只有来问管理员 —— 而那正好是这条消息想省掉的成本。
func banMessage(reason string, until int64) string {
	msg := "这个账号已被管理员封禁，不能再使用。"
	if r := strings.TrimSpace(reason); r != "" {
		msg += "原因：" + r + "。"
	}
	if until > 0 {
		msg += "解封时间：" + time.Unix(until, 0).In(cnZone).Format("2006-01-02 15:04") + "。"
	} else {
		msg += "这是永久封禁。"
	}
	return msg
}

func userIDOf(r *http.Request) string {
	if v, ok := r.Context().Value(ctxUserID).(string); ok {
		return v
	}
	return ""
}

// bearerToken 取请求头里的令牌原文。中间件里已经验过一次，这里是为了
// "改密后只留当前这台"这种需要知道自己拿的是哪个 token 的场景。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// handleBootstrap reports whether the server still needs its first admin, so
// the very first launch can create the admin without an invite code.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	var n int
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	writeJSON(w, 200, map[string]any{"need_admin": n == 0, "server_time": time.Now().Unix()})
}

type registerReq struct {
	InviteCode string `json:"invite_code"`
	Name       string `json:"name"`
	Pass       string `json:"pass"`
	// CalendarInvite 是 2026-09-19 加的**新**字段：一张日历邀请码（`fsi_` 前缀）。
	//
	// ⚠️ 为什么另立字段而不是复用 invite_code：
	// `invite_code` 一直是**全站注册凭证**，它的含义不能改（服务端只做加法）。
	// 两条通道的语义也不同：全站码只给你一个账号，日历邀请码还多给一张日历的成员身份。
	//
	// 但为了兼容"按方案字面实现"的客户端，下面还接受
	// **invite_code 传 fsi_ 前缀** 的写法（见 handleRegister 里的分支）。
	CalendarInvite string `json:"calendar_invite"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Pass) < 4 {
		apiErr(w, 400, "名字不能为空，密码至少 4 位")
		return
	}

	// ---- 先弄清"凭哪张票进来的" ----
	//
	// 三条路：
	//   ① 空库第一个账号 → 免票（建管理员）
	//   ② 全站注册码（invite_code，非 fsi_）→ 只建账号
	//   ③ 日历邀请码（calendar_invite，或 invite_code 传 fsi_）→ 建账号 + 自动加成员
	//
	// ③ 之所以也能注册：邀请本来就比全站码**更窄**（它绑死一张日历 + 一个不超过
	// editor 的角色）。让持邀请的人能注册，不等于"把全站码下放给成员"。
	calendarCode := strings.TrimSpace(req.CalendarInvite)
	if calendarCode == "" && strings.HasPrefix(strings.TrimSpace(req.InviteCode), inviteCodePrefix) {
		calendarCode = strings.TrimSpace(req.InviteCode)
	}
	siteCode := strings.TrimSpace(req.InviteCode)
	if calendarCode != "" {
		siteCode = "" // 走邀请这条路就不再要求全站码
	}

	var n int
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	isAdmin := n == 0

	// ② 全站注册码这条路 —— **行为和 0.4.x 完全一致**（旧客户端只有这条路）
	if !isAdmin && calendarCode == "" {
		if s.cfg.InviteCode == "" || siteCode != s.cfg.InviteCode {
			apiErr(w, 403, "邀请码不正确")
			return
		}
	}

	// ⚠️ 邀请要在**建账号之前**先验一遍完整性，而且真正的"用掉"要和建账号
	// 在**同一个事务**里（见下面的 tx）。
	//
	// 这里踩过一次真 bug：最初只在建账号前确认"邀请存在"，
	// 把"过期/撤销/用尽"留给后面的加成员去判，又规定"加成员失败不让注册失败" ——
	// 结果**一张已用尽的邀请仍能源源不断地注册账号**（返回 200，
	// 只是没加进日历）。而"能往这个部署里注册账号"正是我们拒绝下放给普通成员的权力。
	// 2026-09-19 由 scripts/inbox-check.mjs 的"已用尽的邀请不能再注册"一条抓到。
	var invite *inviteRow
	if !isAdmin && calendarCode != "" {
		row, ok := s.inviteByCode(calendarCode)
		if !ok {
			apiErr(w, 403, "邀请码不正确")
			return
		}
		if reason := inviteUnusableReason(row); reason != "" {
			apiErr(w, 403, "邀请码"+reason)
			return
		}
		invite = row
	}

	id := newID("u_")
	now := time.Now().Unix()
	// 哈希在事务**外面**算：它要几十毫秒 CPU，不该占着事务（Cloudflare 那条线同理）
	pwHash := hashPassword(req.Pass)
	tok := newToken()

	joinedCal := ""
	if err := s.withTx(func(tx *sql.Tx) error {
		// ⚠️ 2026-10-17 加（第四轮审计 L3）：`isAdmin` **必须在事务内重算一次**。
		//
		//    上面那次（事务外）只能用来决定"要不要验全站注册码"，不能拿去写库：
		//    全新部署上两个并发注册（第一次开服，两个人同时点）会各自读到
		//    `COUNT(users)==0`，于是**两个人都被写成管理员**。
		//    概率极低，但后果是两个 ID 都有最高权限、且互相删不掉。
		//    事务内重算的并发窗口被 SQLite 的写锁封住（同一时刻只有一个写事务）。
		var cnt int
		_ = tx.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&cnt)
		admin := cnt == 0
		if _, e := tx.Exec(
			`INSERT INTO users(id,name,pass_hash,is_admin,created_at) VALUES(?,?,?,?,?)`,
			id, req.Name, pwHash, boolToInt(admin), now); e != nil {
			return e
		}
		if _, e := tx.Exec(`INSERT INTO tokens(token,user_id,created_at) VALUES(?,?,?)`, tok, id, now); e != nil {
			return e
		}
		if invite == nil {
			return nil
		}
		// 占坑 + 加成员 + 补发，全在同一个事务里。
		// 占坑用**条件 UPDATE**：两个请求同时用同一张一次性邀请时，
		// 只有一条能命中 `used_count < max_uses`，另一条影响 0 行 →
		// 整条事务回滚（账号也不会留下）。先读后写会让两张都通过（TOCTOU）。
		res, e := tx.Exec(
			`UPDATE invites SET used_count=used_count+1 WHERE id=? AND revoked_at=0
			   AND (expires_at=0 OR expires_at>?) AND (max_uses=0 OR used_count<max_uses)`,
			invite.ID, now)
		if e != nil {
			return e
		}
		if n, e := res.RowsAffected(); e != nil {
			return e
		} else if n == 0 {
			return errInviteGone
		}
		role, okRole := normalizeInviteRole(invite.Role)
		if !okRole {
			role = RoleViewer // 库里存了非法角色 → 按最低权限处理，绝不放行
		}
		if _, e := tx.Exec(
			`INSERT INTO calendar_members(calendar_id,user_id,role) VALUES(?,?,?)
			 ON CONFLICT(calendar_id,user_id) DO UPDATE SET role=excluded.role`,
			invite.CalendarID, id, role); e != nil {
			return e
		}
		// 加了人必须补发成员变更，否则对方拿到一张空日历（红线 19）
		if e := s.resendCalendarMembers(tx, invite.CalendarID); e != nil {
			return e
		}
		joinedCal = invite.CalendarID
		return nil
	}); err != nil {
		if err == errInviteGone {
			// 并发下被别人先用掉了 → 整条回滚，账号也没留下，报清楚
			apiErr(w, 410, "这张邀请已经被用掉了")
			return
		}
		if strings.Contains(err.Error(), "UNIQUE") {
			apiErr(w, 409, "这个名字已经被占用了")
			return
		}
		apiErr(w, 500, err.Error())
		return
	}

	writeJSON(w, 200, map[string]any{
		"token": tok,
		"user":  User{ID: id, Name: req.Name, IsAdmin: isAdmin, CreatedAt: now},
		// 2026-09-19 新增的可选字段：凭日历邀请码注册时，告诉客户端进了哪张日历。
		// 旧客户端忽略即可。
		"joined_calendar_id": joinedCal,
	})

	// 账号事件留痕（见 audit.go）。放在**写响应之后**是有意的：
	// 记日志失败绝不能让"注册成功"这件事变成失败。
	// 这里不需要判 err —— logAccountEvent 内部自己吞掉失败并打警告。
	s.logAccountEvent(r, id, req.Name, logEventRegister, "success", "")
}

// inviteUnusableReason 判一张邀请现在能不能用，返回**人能看懂的原因**
// （可用时返回空串）。注册和接受两处共用，免得两边判得不一样。
func inviteUnusableReason(row *inviteRow) string {
	now := time.Now().Unix()
	switch {
	case row.RevokedAt != 0:
		return "已被撤销"
	case row.ExpiresAt > 0 && now > row.ExpiresAt:
		return "已过期"
	case row.MaxUses > 0 && row.UsedCount >= row.MaxUses:
		return "已用尽"
	case row.Kind != inviteKindInvite:
		return "类型不对"
	case row.CalendarID == "":
		return "没有绑定日历"
	}
	return ""
}

type loginReq struct {
	Name string `json:"name"`
	Pass string `json:"pass"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	name := strings.TrimSpace(req.Name)
	var id, hash string
	var admin int64
	var created int64
	err := s.db.sql.QueryRow(
		`SELECT id,pass_hash,is_admin,created_at FROM users WHERE name=?`,
		name).Scan(&id, &hash, &admin, &created)
	if err != nil || !verifyPassword(req.Pass, hash) {
		// ⚠️ 失败也要记，而且**记用户输入的那个名字**（此时查不到 uid）。
		//
		// "有人在拿哪些名字试"正是暴力破解的特征 —— 只看成功记录的话，
		// 这件事完全不可见。同理 reason 要区分"账号不存在"与"密码不对"，
		// 因为前者是"在猜名字"、后者是"知道名字了在猜密码"，处置方式不同。
		//
		// 但**对外的响应文案仍然是同一句**（下面 apiErr 那句）：
		// 明确区分会变成一个账号枚举接口 —— 攻击者能靠它列出所有有效用户名。
		// 日志里区分、响应里不区分，这两件事必须同时做对。
		reason := "密码不对"
		if err != nil {
			reason = "账号不存在"
		}
		s.logAccountEvent(r, "", name, logEventLoginFailed, "fail", reason)
		apiErr(w, 401, "用户名或密码不对")
		return
	}
	tok := newToken()
	now := time.Now().Unix()
	// 封禁检查放在**密码校验通过之后**，两个理由：
	//   ① 放在之前的话，被封禁的账号名会变成一个"存在性探针"（名字对不对
	//      与封禁无关，密码随便填也能看出被封了）；
	//   ② 校验通过说明确实是本人，那**该告诉他被封了** —— 不告诉的话他会
	//      一直以为是密码错了，反复重置密码。这与"对外不区分账号是否存在"
	//      并不矛盾：那句话保护的是**别人**猜名字，这里保护的是**本人**知情。
	//
	// 到期的封禁走 `banActive`（顺手解开），所以"临时封 3 天"到点后本人直接就能登。
	var banned int64
	var banReason string
	var banUntil int64
	if e := s.db.sql.QueryRow(`SELECT banned, ban_reason, ban_until FROM users WHERE id=?`, id).
		Scan(&banned, &banReason, &banUntil); e == nil &&
		s.banActive(id, banned, banUntil, time.Now().Unix()) {
		s.logAccountEvent(r, id, name, logEventLoginFailed, "fail", "账号已被封禁")
		apiErr(w, http.StatusForbidden, banMessage(banReason, banUntil))
		return
	}
	if _, err := s.db.sql.Exec(`INSERT INTO tokens(token,user_id,created_at) VALUES(?,?,?)`, tok, id, now); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"token": tok,
		"user":  User{ID: id, Name: strings.TrimSpace(req.Name), IsAdmin: admin != 0, CreatedAt: created},
	})
	s.logAccountEvent(r, id, name, logEventLogin, "success", "")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	_, _ = s.db.sql.Exec(`DELETE FROM tokens WHERE token=?`, bearerToken(r))
	writeJSON(w, 200, map[string]any{"ok": true})
	// 登出也记：这是"这台设备什么时候停止活动"的唯一线索。
	// 排查"我的号在别的设备上被用了"时，登出记录和登录记录一样重要。
	s.logAccountEvent(r, uid, s.userNameOf(uid), logEventLogout, "success", "")
}

type passwordReq struct {
	OldPass string `json:"old_pass"`
	NewPass string `json:"new_pass"`
}

/**
 * handleChangePassword 改自己的密码。
 *
 * 两道门槛：①带着有效 token（身份），②**还得报出旧密码**。
 * 只认 token 的话，谁临时拿到你的手机就能把密码改掉、把你所有设备踢下线；
 * 加一道旧密码，改密这件事就必须是本人操作。
 *
 * 改完把**其它设备**的会话作废（当前这台留着，否则用户改完密码自己先被踢出来）。
 * 这正是改密该有的效果：密码是泄露后才来改的，别处的登录必须一起失效。
 */
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var req passwordReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	req.NewPass = strings.TrimSpace(req.NewPass)
	if len(req.NewPass) < 4 {
		apiErr(w, 400, "新密码至少 4 位")
		return
	}
	if req.NewPass == req.OldPass {
		apiErr(w, 400, "新密码不能和旧密码一样")
		return
	}

	var oldHash string
	if err := s.db.sql.QueryRow(`SELECT pass_hash FROM users WHERE id=?`, uid).Scan(&oldHash); err != nil {
		apiErr(w, 404, "找不到这个账户")
		return
	}
	if !verifyPassword(req.OldPass, oldHash) {
		apiErr(w, 403, "旧密码不对")
		return
	}
	if _, err := s.db.sql.Exec(`UPDATE users SET pass_hash=? WHERE id=?`,
		hashPassword(req.NewPass), uid); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	cur := bearerToken(r)
	kicked := int64(0)
	if res, err := s.db.sql.Exec(`DELETE FROM tokens WHERE user_id=? AND token<>?`, uid, cur); err == nil {
		if n, e := res.RowsAffected(); e == nil {
			kicked = n
		}
	}
	log.Printf("[password] 账户 %s 改了密码，作废其它会话 %d 个", uid, kicked)

	// 改密是**最敏感**的账号事件之一：它要么是本人主动改，要么是账号被入侵后
	// 攻击者在锁人。两种情况下"什么时候、从哪台机器改的、踢掉了几个会话"
	// 都是第一手证据，所以必须留痕。
	s.logAccountEvent(r, uid, s.userNameOf(uid), logEventPasswordChange, "success",
		fmt.Sprintf("作废其它会话 %d 个", kicked))

	// ★ 被踢的那些会话**单独记一条**（而不是只在上面那条里捎个数字）。
	//
	// "我发现有人登了我的号"这个场景下，用户真正想看的是：被踢掉的是哪些
	// 设备、从哪个 IP 来的、什么时候。只写"作废 3 个"等于什么都没说 ——
	// 那 3 个恰好就是入侵者的时候，"被踢"这个动作本身就是唯一的线索。
	//
	// ⚠️ 只在真踢掉了会话时记（kicked > 0）。改密码时如果本来就没别的设备
	//    登录，记一条"会话被踢 0 个"只是噪音，会把真正出事那条淹掉。
	if kicked > 0 {
		s.logAccountEvent(r, uid, s.userNameOf(uid), logEventTokenRevoked, "success",
			fmt.Sprintf("改密码时作废其它会话 %d 个", kicked))
	}

	writeJSON(w, 200, map[string]any{"ok": true, "signed_out": kicked})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var u User
	var admin int64
	err := s.db.sql.QueryRow(`SELECT id,name,is_admin,created_at FROM users WHERE id=?`, uid).
		Scan(&u.ID, &u.Name, &admin, &u.CreatedAt)
	if err != nil {
		apiErr(w, 404, "user not found")
		return
	}
	u.IsAdmin = admin != 0
	writeJSON(w, 200, map[string]any{"user": u})
}

// handleGetInvite lets an admin read the code they need to hand out. Without
// this the only way to learn it is reading the server log on the machine.
func (s *Server) handleGetInvite(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var admin int64
	if err := s.db.sql.QueryRow(`SELECT is_admin FROM users WHERE id=?`, uid).Scan(&admin); err != nil || admin == 0 {
		apiErr(w, 403, "只有管理员可以查看邀请码")
		return
	}
	writeJSON(w, 200, map[string]any{"invite_code": s.cfg.InviteCode})
}

// handleUsers lists every account, used by the member picker. The group is
// deliberately smaller than ten people, so exposing the roster is fine.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.sql.Query(`SELECT id,name,is_admin,created_at FROM users ORDER BY created_at`)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var admin int64
		if err := rows.Scan(&u.ID, &u.Name, &admin, &u.CreatedAt); err == nil {
			u.IsAdmin = admin != 0
			out = append(out, u)
		}
	}
	writeJSON(w, 200, map[string]any{"users": out})
}

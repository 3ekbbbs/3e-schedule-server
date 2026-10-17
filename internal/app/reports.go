package app

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ===========================================================================
// 举报（用户侧）
// ===========================================================================
//
// 设计要点：
//
// 1. **举报要存快照**，不能只存 id。理由见 db.go 里 reports 表上方那段 ——
//    核心是"处理时内容可能已经被改掉或删掉"，只存 id 会让处理页要么查不到、
//    要么看到的是改后的样子。
//
// 2. **"不能举报自己"只对"举报某个账号"成立**，日程 / 日历**允许自报**。
//    原文写的是"自己举报自己会制造无效待办"，但判据只能拿到"日历 owner"
//    （events 表没有作者列），用错代理去拦的结果是把举报在单账号 / 家庭部署里
//    **整体废掉**（详细经过见 handleCreateReport 里那段）。
//
// 3. **同一人对同一目标只留一条待处理举报**。重复提交（连点、网络重试）不该
//    变成三条待办。已处理过的可以再报一次 —— 那是"上次处理我不认同"，合理。
//
// 4. **快照内容由服务端读取**，不接受客户端传进来的标题/正文。
//    否则举报人可以随便编造"他写了 XXX"来陷害别人 —— 举报入口一旦能被用来
//    伪造证据，它就成了攻击工具。客户端只能传"我举报谁"和"为什么"。

// reportReq 是举报请求体。
type reportReq struct {
	TargetKind string `json:"target_kind"` // event/calendar/user/other
	TargetID   string `json:"target_id"`
	CalendarID string `json:"calendar_id"`
	EventID    string `json:"event_id"`
	Reason     string `json:"reason"` // porn/politics/violence/gang/abuse/spam/other
	Detail     string `json:"detail"` // 举报人补充说明
}

var reportReasonLabels = map[string]string{
	"porn":     "涉黄",
	"politics": "涉政",
	"violence": "涉暴",
	"gang":     "涉黑违法",
	"abuse":    "辱骂骚扰",
	"spam":     "广告垃圾",
	"other":    "其他",
}

func reportReasonLabel(r string) string {
	if s, ok := reportReasonLabels[r]; ok {
		return s
	}
	return "其他"
}

// Report 是举报单（给管理员列表用）。
type Report struct {
	ID            string `json:"id"`
	ReporterID    string `json:"reporter_id"`
	ReporterName  string `json:"reporter_name"`
	TargetKind    string `json:"target_kind"`
	TargetID      string `json:"target_id"`
	TargetTitle   string `json:"target_title"`
	TargetExcerpt string `json:"target_excerpt"`
	TargetOwner   string `json:"target_owner"`
	// TargetOwnerName 是归属人的**显示名**（服务端查好，界面直接用）。
	//
	// 为什么不改 `target_owner` 的含义：那是 id，别的地方（以及将来的接口）
	// 按 id 用。所以另开一个字段，两个都在。
	// 账号被删掉之后这个名字会空着 —— 界面显示「（已删除账号）」，
	// 比甩一串 `u_28f076…` 有用。
	TargetOwnerName string `json:"target_owner_name"`
	CalendarID    string `json:"calendar_id"`
	EventID       string `json:"event_id"`
	Reason        string `json:"reason"`
	ReasonLabel   string `json:"reason_label"`
	Detail        string `json:"detail"`
	Status        string `json:"status"`
	HandlerID     string `json:"handler_id"`
	HandlerName   string `json:"handler_name"`
	HandleNote    string `json:"handle_note"`
	Action        string `json:"action"`
	CreatedAt     int64  `json:"created_at"`
	HandledAt     int64  `json:"handled_at"`
	// 自动识别结果：提交时服务端跑一遍词库，命中的话给管理员一个参考。
	// **只是参考，不是判据** —— 词库会漏，也会误报，最终要人来判断。
	AutoHits []WordHit `json:"auto_hits,omitempty"`
	// 处理时内容还在不在（让管理员知道"删"这个动作还能不能做）
	ContentExists bool `json:"content_exists"`
}

// handleCreateReport 提交举报。POST /api/reports
func (s *Server) handleCreateReport(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var req reportReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	kind := strings.TrimSpace(req.TargetKind)
	if kind == "" {
		kind = "event"
	}
	if kind != "event" && kind != "calendar" && kind != "user" && kind != "other" {
		apiErr(w, 400, "举报对象类型不合法")
		return
	}
	if strings.TrimSpace(req.Detail) == "" && strings.TrimSpace(req.Reason) == "" {
		apiErr(w, 400, "请选择举报原因或填写说明")
		return
	}
	if len(req.Detail) > 2000 {
		apiErr(w, 400, "补充说明太长了（最多 2000 字）")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "other"
	}
	if _, ok := reportReasonLabels[reason]; !ok {
		reason = "other"
	}

	// ---- 快照：由服务端自己去读被举报的对象 ----
	//
	// 注意这里**只存 id 与正文快照**，不存"作者的名字"。名字是展示用的冗余，
	// 处理时按 target_owner 现查即可 —— 存下来反而会在用户改名后显示旧名。
	var (
		targetID = strings.TrimSpace(req.TargetID)
		calID    = strings.TrimSpace(req.CalendarID)
		eventID  = strings.TrimSpace(req.EventID)
		title    string
		excerpt  string
		ownerID  string
	)

	switch kind {
	case "event":
		if eventID == "" {
			eventID = targetID
		}
		if eventID == "" {
			apiErr(w, 400, "缺少被举报的日程 id")
			return
		}
		// 用"能读到"作为门槛：读不到就是没权限或已删除，都统一报"内容不存在"，
		// 不区分 —— 区分了就等于告诉举报人"这条存在但你看不到"。
		ev, ok := s.eventByID(eventID, uid)
		if !ok {
			apiErr(w, 404, "这条日程不存在，或你已看不到它")
			return
		}
		// ⚠️ **这两行必须真的存在**（2026-10-09 补）。
		//
		// 上面那句注释一直宣称"用'能读到'作为门槛"，但 `s.eventByID` 是**裸查**
		// ——按 id 取一行，既不判 ACL 也不筛 `deleted_at`。于是"门槛"从来没生效过：
		// 举报接口实际上是一条**按 id 读任意日程标题**的通道
		//（快照里的 target_title 被存下来，而 `GET /api/reports/mine` 是回给
		//  举报人自己的 → 换个 id 就能把别人的标题读回来）。
		//
		// 判据用 `canRead(ev.CalendarID, uid)`：与同步/列表**同一套**权限函数，
		// 所以"客户端看得见的日程一定报得动"，不会多拦。
		if ev.DeletedAt != nil {
			apiErr(w, 404, "这条日程不存在，或你已看不到它")
			return
		}
		if !s.canRead(ev.CalendarID, uid) {
			apiErr(w, 404, "这条日程不存在，或你已看不到它")
			return
		}
		calID = ev.CalendarID
		title = ev.Title
		excerpt = buildExcerpt(ev.Title, ev.Notes, ev.Location, 300)
		// ⚠️ **不能用 `ev.UID`**：events.uid 是每行一个随机令牌（newID("uid_")），
		// 和 users.id 完全是两套格式，拿它跟当前登录 uid 比较恒不相等 ——
		// "不能举报自己的内容"会直接失效。
		// 日程没有"作者"列，归属由所在日历的 owner_id 决定，那是真正的 user id。
		ownerID = s.eventOwnerID(calID)

	case "calendar":
		if calID == "" {
			calID = targetID
		}
		if calID == "" {
			apiErr(w, 400, "缺少被举报的日历 id")
			return
		}
		if !s.canRead(calID, uid) {
			apiErr(w, 404, "这个日历不存在，或你已不在其中")
			return
		}
		var name, owner string
		if err := s.db.sql.QueryRow(`SELECT name, owner_id FROM calendars WHERE id=? AND deleted_at IS NULL`,
			calID).Scan(&name, &owner); err != nil {
			apiErr(w, 404, "这个日历不存在")
			return
		}
		title = name
		excerpt = "日历名称：" + name
		ownerID = owner

	case "user":
		if targetID == "" {
			apiErr(w, 400, "缺少被举报的用户 id")
			return
		}
		if targetID == uid {
			apiErr(w, 400, "不用举报自己")
			return
		}
		// 用户举报只记"对象是谁"，不存任何内容快照（用户名不算违规内容），
		// 让它去查"这个人在哪些共有日历里" —— 管理员自己判断。
		var n string
		if err := s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, targetID).Scan(&n); err != nil {
			apiErr(w, 404, "这个用户不存在")
			return
		}
		title = n
		excerpt = "用户账号：" + n
		ownerID = targetID

	default: // other
		title = strings.TrimSpace(req.TargetID)
		excerpt = ""
	}

	// 「不能举报自己」**只在"举报某个账号"这一种情形下成立**（报自己确实没意义）。
	//
	// ⚠️⚠️ 2026-10-09 修：原来这里写的是 `kind != "other"`，也就是
	//    **日程 / 日历只要归属人是自己就一律 400**。它的判据是
	//    `eventOwnerID(calID)` = **日历的 owner_id**，而 events 表**没有作者列**
	//    （`events.uid` 是每行一个随机令牌 `uid_xxx`，实测确认），
	//    所以"这条是不是我写的"根本判不出来 —— 用一个错代理去拦。
	//
	//    后果不是"偶尔拦错"，而是**把功能整体废掉**：
	//    本项目上线以来的实际数据里，主账号可见的日程全部落在自己拥有的日历上
	//    （多成员共用日历反而少见）→ 五端（网页/手机/电脑/小程序）点「举报」
	//    全部返回 400「不用举报自己」→ 用户看到的就是"举报功能全部失效"。
	//    上架审核用第一个账号（拥有全部日历）试也会同样撞上。
	//
	//    至于"制造无效待办、稀释多人举报信号"：下面那段**同一人 + 同一目标 +
	//    仍是 pending 就 UPDATE 而不是 INSERT** 的去重已经挡住了刷屏，
	//    而管理台本来就同时显示"举报人"和"归属"，是不是自报管理员一眼能看见。
	//    → 所以这里只保留"报自己账号"这一条。
	if ownerID != "" && ownerID == uid && kind == "user" {
		apiErr(w, 400, "不用举报自己")
		return
	}

	now := time.Now().Unix()

	// 同一人对同一目标，若已有**待处理**的举报，就更新它而不是新增一条。
	// 这样连点 / 网络重试不会变成多条待办；而"上次处理完不服气再报一次"
	// 仍然可以（因为那时已不是 pending）。
	var dupID string
	if targetID != "" || eventID != "" {
		_ = s.db.sql.QueryRow(`
			SELECT id FROM reports
			WHERE reporter_id=? AND status='pending' AND target_kind=?
			  AND (CASE WHEN target_id<>'' THEN target_id ELSE event_id END)
			    = ?
			LIMIT 1`, uid, kind, firstNonEmpty(targetID, eventID)).Scan(&dupID)
	}
	if dupID != "" {
		if _, err := s.db.sql.Exec(`
			UPDATE reports SET reason=?, detail=?, target_title=?, target_excerpt=?,
			       target_owner=?, calendar_id=?, event_id=?, created_at=?
			WHERE id=?`,
			reason, req.Detail, title, excerpt, ownerID, calID, eventID, now, dupID); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "id": dupID, "updated": true})
		return
	}

	id := newID("rp_")
	if _, err := s.db.sql.Exec(`
		INSERT INTO reports
		  (id, reporter_id, reporter_name, target_kind, target_id, target_title,
		   target_excerpt, target_owner, calendar_id, event_id, reason, detail,
		   status, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'pending',?)`,
		id, uid, s.userNameOf(uid), kind, firstNonEmpty(targetID, eventID), title,
		excerpt, ownerID, calID, eventID, reason, req.Detail, now); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}

// handleListMyReports 查自己提交的举报与处理结果。GET /api/reports/mine
//
// 为什么要有它：举报如果提交完就石沉大海，用户下次就不报了。
// 让他能看到"已受理 / 已驳回 + 处理意见"，举报机制才立得住。
func (s *Server) handleListMyReports(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	rows, err := s.db.sql.Query(`
		SELECT id, target_kind, target_title, reason, detail, status,
		       handle_note, action, created_at, handled_at
		FROM reports WHERE reporter_id=?
		ORDER BY created_at DESC LIMIT 100`, uid)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type mine struct {
		ID          string `json:"id"`
		TargetKind  string `json:"target_kind"`
		TargetTitle string `json:"target_title"`
		Reason      string `json:"reason"`
		ReasonLabel string `json:"reason_label"`
		Detail      string `json:"detail"`
		Status      string `json:"status"`
		HandleNote  string `json:"handle_note"`
		Action      string `json:"action"`
		CreatedAt   int64  `json:"created_at"`
		HandledAt   int64  `json:"handled_at"`
	}
	out := []mine{}
	for rows.Next() {
		var m mine
		if err := rows.Scan(&m.ID, &m.TargetKind, &m.TargetTitle, &m.Reason, &m.Detail,
			&m.Status, &m.HandleNote, &m.Action, &m.CreatedAt, &m.HandledAt); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		m.ReasonLabel = reportReasonLabel(m.Reason)
		out = append(out, m)
	}
	writeJSON(w, 200, map[string]any{"reports": out})
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// buildExcerpt 拼一段快照正文。多段非空内容用 " / " 连接后按 limit 截断。
//
// 截断要在**字符安全**的前提下做：按 rune 截，否则会把一个汉字劈成半个
// 无效 UTF-8，前端显示成乱码方块。
func buildExcerpt(title, notes, location string, limit int) string {
	if limit <= 0 {
		limit = 300
	}
	var segs []string
	for _, p := range []string{title, notes, location} {
		if strings.TrimSpace(p) != "" {
			segs = append(segs, strings.TrimSpace(p))
		}
	}
	s := strings.Join(segs, " / ")
	rs := []rune(s)
	if len(rs) > limit {
		return string(rs[:limit]) + "…"
	}
	return s
}

// userNameOf 查用户名，查不到返回空串（**不报错**）。
//
// 用于"给举报单补一个展示用的名字"。这里不该因为查不到就中断举报流程 ——
// 用户名缺失是可接受的降级，举报提交失败不是。
func (s *Server) userNameOf(uid string) string {
	if strings.TrimSpace(uid) == "" {
		return ""
	}
	var name string
	if err := s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, uid).Scan(&name); err != nil {
		return ""
	}
	return name
}

// eventOwnerID 返回某张日历的归属人（真正的 users.id）。
//
// 为什么需要它：日程表**没有作者列**。历史上 events.uid 被塞的是
// `newID("uid_")` —— 每行一个随机令牌，既不是人也不是任何实体的外键，
// 纯属写进去之后没人读的遗留值。任何"这是谁的日程"的判断都只能从
// 所在日历的 owner_id 推。
//
// 传日历 id 而不是日程 id：调用方（举报快照）本来就已经拿到
// ev.CalendarID，不必再多查一次 events 表。
func (s *Server) eventOwnerID(calendarID string) string {
	if strings.TrimSpace(calendarID) == "" {
		return ""
	}
	var owner string
	if err := s.db.sql.QueryRow(`SELECT owner_id FROM calendars WHERE id=?`, calendarID).Scan(&owner); err != nil {
		return ""
	}
	return owner
}

// ---------------------------------------------------------------------------
// 状态查询（前端用：拿词库版本，决定本地缓存的词表要不要更新）
// ---------------------------------------------------------------------------

// handleModerationMeta 给**所有登录用户**的一份轻量元信息。
// GET /api/moderation/meta
//
// 前端要拿词表做输入预检，就需要词表本体；但把整张表发下来又太重（而且会
// 把"有哪些词"暴露给每个用户 —— 那是绕过指南）。折中：
//   - 发**词表本体**（前端预检必须要有词，否则只能靠服务端往返，体验很差）；
//   - 但**只发启用中的**，且不带 note/created_by 这类内部字段。
//
// 词表本来就不是秘密（用户试一次就知道哪个词被拦），真正要保护的是
// "内部备注"和"谁加的"，那些不下发。
func (s *Server) handleModerationMeta(w http.ResponseWriter, r *http.Request) {
	s.words.mu.RLock()
	list := s.words.list
	s.words.mu.RUnlock()

	type pubWord struct {
		Word     string `json:"word"`
		Raw      string `json:"raw"`
		Category string `json:"category"`
		Severity int    `json:"severity"`
	}
	out := []pubWord{}
	for i := range list {
		if !list[i].Enabled || list[i].Word == "" {
			continue
		}
		out = append(out, pubWord{
			Word:     list[i].Word,
			Raw:      firstNonEmpty(list[i].Raw, list[i].Word),
			Category: list[i].Category,
			Severity: list[i].Severity,
		})
	}
	writeJSON(w, 200, map[string]any{
		"words":      out,
		"categories": wordCategoryLabels,
		"reasons":    reportReasonLabels,
		"count":      len(out),
	})
}

// scanReport 把一行举报读成结构体。
func scanReport(rows interface {
	Scan(dest ...any) error
}) (Report, error) {
	var rp Report
	var status string
	if err := rows.Scan(
		&rp.ID, &rp.ReporterID, &rp.ReporterName, &rp.TargetKind, &rp.TargetID,
		&rp.TargetTitle, &rp.TargetExcerpt, &rp.TargetOwner, &rp.CalendarID,
		&rp.EventID, &rp.Reason, &rp.Detail, &status, &rp.HandlerID,
		&rp.HandlerName, &rp.HandleNote, &rp.Action, &rp.CreatedAt, &rp.HandledAt,
	); err != nil {
		return rp, err
	}
	rp.Status = status
	rp.ReasonLabel = reportReasonLabel(rp.Reason)
	return rp, nil
}

// reportByContentExists 判断被举报的日程是否还在（能不能执行"删除"）。
//
// 独立成函数是因为它要**用管理员身份**判断，而不是举报人或作者的 ——
// 内容可能已经从作者的可见范围里消失（被移出日历），但对管理员来说它还在。
// 用错身份会让"内容还在"和"内容已删"混为一谈，管理员就不敢点删除了。
func (s *Server) reportContentExists(rp Report) bool {
	switch rp.TargetKind {
	case "event":
		id := firstNonEmpty(rp.EventID, rp.TargetID)
		if id == "" {
			return false
		}
		var n int
		if err := s.db.sql.QueryRow(`SELECT COUNT(*) FROM events WHERE id=? AND deleted_at IS NULL`, id).Scan(&n); err != nil {
			return false
		}
		return n > 0
	case "calendar":
		if rp.CalendarID == "" && rp.TargetID == "" {
			return false
		}
		var n int
		if err := s.db.sql.QueryRow(`SELECT COUNT(*) FROM calendars WHERE id=? AND deleted_at IS NULL`,
			firstNonEmpty(rp.CalendarID, rp.TargetID)).Scan(&n); err != nil {
			return false
		}
		return n > 0
	default:
		// 用户/其他：没有"内容可删"这个概念，返回 false 让前端隐藏删除按钮。
		return false
	}
}

// requireAdmin 是管理员接口的统一闸门。
//
// 返回 true 表示"不是管理员，已写响应"，调用方应立即 return。
// 判定复用 users.is_admin（**已存在的字段**，不新增权限体系）。
//
// ⚠️ 这里刻意**不返回 404 伪装成"没有这个接口"**：使用者是本系统的管理员，
// 明确告诉他"你不是管理员"比让他以为接口坏了更有用。但**也不泄漏**
// 任何举报内容 —— 403 出去之前一个字节的业务数据都不读。
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	uid := userIDOf(r)
	if uid == "" {
		apiErr(w, 401, "请先登录")
		return true
	}
	var isAdmin int
	if err := s.db.sql.QueryRow(`SELECT is_admin FROM users WHERE id=?`, uid).Scan(&isAdmin); err != nil {
		if err == sql.ErrNoRows {
			apiErr(w, 401, "账号不存在")
			return true
		}
		apiErr(w, 500, err.Error())
		return true
	}
	if isAdmin == 0 {
		apiErrDetail(w, http.StatusForbidden, "not_admin", "这个页面只有管理员能打开", nil)
		return true
	}
	return false
}

// handleIsAdmin 让前端在渲染导航前问一句"我是不是管理员"。
// GET /api/admin/whoami —— **不需要管理员权限**，任何人都能问，只是答案不同。
//
// 为什么单独一个接口：管理员入口不该出现在普通用户的界面上（哪怕点了会被拒）。
// 前端必须能在**渲染之前**知道该不该画那个入口，所以需要一个"能提前问"的接口。
func (s *Server) handleIsAdmin(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var isAdmin int
	_ = s.db.sql.QueryRow(`SELECT is_admin FROM users WHERE id=?`, uid).Scan(&isAdmin)
	writeJSON(w, 200, map[string]any{
		"is_admin": isAdmin != 0,
		"user_id":  uid,
	})
}

// atoiDefault 是 strconv.Atoi 的"失败就用默认值"版本，给分页参数用。
func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}

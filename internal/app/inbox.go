package app

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 收件箱（2026-09-19，跨端方案第九节）
//
// 这是**唯一一个"以用户为中心"的实体**：可见性判定是 `user_id == 自己`，
// **不是**日历权限。所以本文件里**不出现任何 calendarRole / canRead / canWrite** ——
// 出现即错。收件箱的 origin_calendar_id 可能指向发件人的日历，
// 用日历权限去滤只会把事情搞反。
//
// 三条硬约束（方案 §9.2）：
//   ① `kind:'share'`   → 发件人必须对源日历有**读**权限，服务端校验
//   ② `kind:'capture'` → `to_user_id` 必须等于自己，否则 403
//      （这是"本机抓取同步上来"的通道，不是"给别人发东西"的通道）
//   ③ **`null` 一律等于"没传"**（契约第 10 条），不做清空语义
//
// 权力边界（架构原则 1 的推论）：
//   服务端**不提供**"从收件箱造事件"的接口。
//   "加入我的日历"是客户端调既有 POST /api/events 建成功之后，
//   再把 event_id 一起提交过来改状态。否则就多出第二条写事件的路径。
// ---------------------------------------------------------------------------

const (
	inboxKindShare   = "share"
	inboxKindCapture = "capture"
	// inboxKindSystem 是**服务端自己发**的系统通知（如"你的举报已处理"）。
	//
	// ⚠️ 这个 kind **只能由服务端写入**，客户端的同步推送上永远拿不到它 ——
	// pushInboxItem 里那段归一会把不认识的 kind 落成 capture（那是给老客户端
	// 的兜底）。所以它不会因为客户端不认识而丢失：它压根不由客户端产生。
	//
	// 为什么不复用 share：share 的语义是"有人要共享给我，我要不要接受"，
	// 带 pending/accepted 决策流程。系统通知没有"接不接受"这回事，
	// 混进去会让收件箱里出现一堆需要"处理"但其实只需读的通知。
	// 展示层按 kind 区分即可（读到 system 就只显示文案、不显示接受/忽略按钮）。
	inboxKindSystem = "system"

	inboxStatusPending   = "pending"
	inboxStatusAccepted  = "accepted"
	inboxStatusDismissed = "dismissed"
)

// inboxMaxBatch 一次批量决策的上限。
// 存在的意义不是"省事"，而是防止一次请求带十万个 id 把事务撑爆。
const inboxMaxBatch = 200

// inboxListLimit 列表默认与上限。
const (
	inboxListDefault = 200
	inboxListMax     = 500
)

// inboxByID 读一行（不过滤归属 —— 归属判定由调用方做，见 loadEntities / preflight）。
func (s *Server) inboxByID(id string) (*InboxItem, bool) {
	var it InboxItem
	var allDay int64
	var fs string
	err := s.db.sql.QueryRow(`
		SELECT id,user_id,kind,status,source,from_user_id,from_name,origin_calendar_id,
		       title,notes,location,all_day,dtstart_utc,duration_sec,remind_minutes_before,
		       source_excerpt,confidence,accepted_event_id,created_at,decided_at,seq,field_seqs
		FROM inbox_items WHERE id=?`, id).
		Scan(&it.ID, &it.UserID, &it.Kind, &it.Status, &it.Source, &it.FromUserID, &it.FromName,
			&it.OriginCalendarID, &it.Title, &it.Notes, &it.Location, &allDay, &it.DTStartUTC,
			&it.DurationSec, &it.RemindMinutesBefore, &it.SourceExcerpt, &it.Confidence,
			&it.AcceptedEventID, &it.CreatedAt, &it.DecidedAt, &it.Seq, &fs)
	if err != nil {
		return nil, false
	}
	it.AllDay = allDay != 0
	it.FieldSeqs = decodeFieldSeqs(fs)
	return &it, true
}

func (s *Server) inboxItemsFor(uid, status string, limit int) ([]InboxItem, error) {
	if limit <= 0 || limit > inboxListMax {
		limit = inboxListMax
	}
	q := `SELECT id FROM inbox_items WHERE user_id=?`
	args := []any{uid}
	if status != "" && status != "all" {
		q += ` AND status=?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	out := []InboxItem{}
	for _, id := range ids {
		if it, ok := s.inboxByID(id); ok {
			out = append(out, *it)
		}
	}
	return out, nil
}

// pushInboxItem 走同步通道创建/更新一条收件箱条目。
//
// 与 REST 那条路的区别：**这里没有字段级合并**。
// 收件箱是 per-user 私有行，不存在"两台设备同时改同一条"的场景，
// 但**必须占 sync_log 的 seq**（否则别的设备永远拉不到）。
//
// 权限已在 preflight 里判过（归属 + capture 通道），这里只负责落库。
func (s *Server) pushInboxItem(tx *sql.Tx, uid string, op pushOp, pre opPre) error {
	now := time.Now().Unix()
	if op.Op == "delete" {
		if pre.notFound {
			return nil
		}
		seq, err := nextSeq(tx, "inbox", op.ID, "delete")
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM inbox_items WHERE id=? AND user_id=?`, op.ID, uid)
		_ = seq
		return err
	}

	if pre.notFound {
		// 新行：user_id 强制成自己（preflight 已经挡掉了填别人的情况）
		seq, err := nextSeq(tx, "inbox", op.ID, "upsert")
		if err != nil {
			return err
		}
		kind := op.Data.stringOf("kind")
		if kind != inboxKindShare && kind != inboxKindCapture {
			kind = inboxKindCapture
		}
		status := op.Data.stringOf("status")
		if status == "" {
			status = inboxStatusPending
		}
		_, err = tx.Exec(`
			INSERT INTO inbox_items(
				id,user_id,kind,status,source,from_user_id,from_name,origin_calendar_id,
				title,notes,location,all_day,dtstart_utc,duration_sec,remind_minutes_before,
				source_excerpt,confidence,accepted_event_id,created_at,decided_at,seq,field_seqs)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'{}')`,
			op.ID, uid, kind, status,
			op.Data.stringOf("source"),
			op.Data.stringOf("from_user_id"),
			op.Data.stringOf("from_name"),
			op.Data.stringOf("origin_calendar_id"),
			op.Data.stringOf("title"),
			op.Data.stringOf("notes"),
			op.Data.stringOf("location"),
			boolToInt(op.Data.boolOf("all_day")),
			mustInt64(op.Data, "dtstart_utc"),
			mustInt64(op.Data, "duration_sec"),
			normalizeRemindJSON(op.Data),
			op.Data.stringOf("source_excerpt"),
			op.Data.stringOf("confidence"),
			"", now, 0, seq)
		return err
	}

	// 已有行：只允许改状态相关字段 + 正文，**不允许改 user_id**
	it, ok := s.inboxByID(op.ID)
	if !ok {
		return nil
	}
	seq, err := nextSeq(tx, "inbox", op.ID, "upsert")
	if err != nil {
		return err
	}
	status := it.Status
	if st := op.Data.stringOf("status"); st == inboxStatusPending || st == inboxStatusAccepted || st == inboxStatusDismissed {
		status = st
	}
	_, err = tx.Exec(`
		UPDATE inbox_items SET status=?, title=?, notes=?, location=?, all_day=?,
			dtstart_utc=?, duration_sec=?, remind_minutes_before=?, accepted_event_id=?, seq=?
		WHERE id=? AND user_id=?`,
		status,
		pickStr(op.Data, "title", it.Title),
		pickStr(op.Data, "notes", it.Notes),
		pickStr(op.Data, "location", it.Location),
		boolToInt(pickBool(op.Data, "all_day", it.AllDay)),
		mustInt64(op.Data, "dtstart_utc"),
		mustInt64(op.Data, "duration_sec"),
		normalizeRemindJSON(op.Data),
		pickStr(op.Data, "accepted_event_id", it.AcceptedEventID),
		seq, op.ID, uid)
	return err
}

func mustInt64(m jsonMap, k string) int64 {
	v, _ := m.int64Of(k)
	return v
}

func pickStr(m jsonMap, k, fallback string) string {
	if _, ok := m[k]; !ok {
		return fallback
	}
	return m.stringOf(k)
}

func pickBool(m jsonMap, k string, fallback bool) bool {
	if _, ok := m[k]; !ok {
		return fallback
	}
	return m.boolOf(k)
}

// normalizeRemindJSON 把提醒列表统一成**JSON 数组文本**。
//
// ⚠️ 这里有一个必须守住的语义：**空数组 = 不提醒**，它和"没传"是两回事。
// 所以：
//
//	· 字段缺失        → "[]"（新行）／保持原值（改行，由调用方兜）
//	· 显式 []         → "[]"
//
// 两者落库都是 "[]"，但**不能因为没有元素就退化成别的值** ——
// 历史上"任何一层的默认 15 分钟兜底"就是这么长出来的（红线 18）。
func normalizeRemindJSON(m jsonMap) string {
	v, ok := m["remind_minutes_before"]
	if !ok || v == nil {
		// 也接受 []int64 形态（Go 侧调用方可能直接给切片）
		if list, ok := m["remind_minutes"]; ok {
			b, err := json.Marshal(list)
			if err == nil {
				return string(b)
			}
		}
		return "[]"
	}
	switch t := v.(type) {
	case []any:
		out := []int64{}
		for _, x := range t {
			if f, ok := x.(float64); ok {
				out = append(out, int64(f))
			}
		}
		b, err := json.Marshal(out)
		if err != nil {
			return "[]"
		}
		return string(b)
	case string:
		// 已经是 JSON 文本就原样存（客户端可能直接透传）
		var chk []int64
		if json.Unmarshal([]byte(t), &chk) == nil {
			return t
		}
		return "[]"
	}
	return "[]"
}

// ---------------------------------------------------------------- REST handlers

type inboxCreateReq struct {
	Kind                string  `json:"kind"`
	Source              string  `json:"source"`
	ToUserID            string  `json:"to_user_id"`
	FromUserID          string  `json:"from_user_id"`
	OriginCalendarID    string  `json:"origin_calendar_id"`
	Title               string  `json:"title"`
	Notes               string  `json:"notes"`
	Location            string  `json:"location"`
	AllDay              bool    `json:"all_day"`
	DTStartUTC          int64   `json:"dtstart_utc"`
	DurationSec         int64   `json:"duration_sec"`
	RemindMinutesBefore []int64 `json:"remind_minutes_before"`
	SourceExcerpt       string  `json:"source_excerpt"`
	Confidence          string  `json:"confidence"`
}

// handleCreateInbox 创建/投递一条收件箱条目。
//
// 两条通道的权限完全不同，别混：
//
//	· share   → 发件人必须对 origin_calendar_id 有**读**权限（我只投我自己的东西）
//	· capture → to_user_id 必须等于自己（本机抓取同步上来，不是"给别人发"）
func (s *Server) handleCreateInbox(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var req inboxCreateReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind != inboxKindShare && kind != inboxKindCapture {
		apiErr(w, 400, "kind 必须是 share 或 capture")
		return
	}
	if strings.TrimSpace(req.Title) == "" && strings.TrimSpace(req.SourceExcerpt) == "" {
		apiErr(w, 400, "标题和原文摘要不能都为空")
		return
	}
	// 违禁词闸门：收件箱是**第二条进入日历的路径**（AI 提案先落这里，
	// 用户点"加入我的日历"时才真正建日程）。不在这里拦，就等于
	// "通过 AI 抓取可以绕过日程接口的审核"。
	if s.blockedByWords(w, r, map[string]string{
		"标题": req.Title,
		"备注": req.Notes,
		"地点": req.Location,
	}) {
		return
	}

	toUser := strings.TrimSpace(req.ToUserID)
	switch kind {
	case inboxKindShare:
		if toUser == "" {
			apiErr(w, 400, "share 必须给 to_user_id")
			return
		}
		// ★ 发件人必须能看见源日历。
		// 这里用 canRead 是**对的** —— 判的是"我有没有资格把这条东西发出去"，
		// 不是"收件人能不能看"（收件人看到的是快照，不依赖源日历权限）。
		if req.OriginCalendarID != "" && !s.canRead(req.OriginCalendarID, uid) {
			apiErr(w, 403, "你对源日历没有读权限")
			return
		}
		// 收件人必须真实存在，否则收件箱里会出现永远没人能看到的幽灵条目
		var n int
		_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM users WHERE id=?`, toUser).Scan(&n)
		if n == 0 {
			apiErr(w, 404, "收件人不存在")
			return
		}
	case inboxKindCapture:
		// ★ 本机抓取只能投给自己。这条挡住"用 capture 通道给别人塞东西"。
		if toUser != "" && toUser != uid {
			apiErr(w, 403, "capture 只能投给自己")
			return
		}
		toUser = uid
	}

	// from_user_id 由服务端定，不信请求里的值（否则可以伪造发件人）
	fromUser := uid
	fromName := s.userNameOf(uid)

	id := newID("ib_")
	now := time.Now().Unix()
	var seq int64
	err := s.withTx(func(tx *sql.Tx) error {
		var e error
		seq, e = nextSeq(tx, "inbox", id, "upsert")
		if e != nil {
			return e
		}
		b, _ := json.Marshal(dedupeInt64(req.RemindMinutesBefore))
		_, e = tx.Exec(`
			INSERT INTO inbox_items(
				id,user_id,kind,status,source,from_user_id,from_name,origin_calendar_id,
				title,notes,location,all_day,dtstart_utc,duration_sec,remind_minutes_before,
				source_excerpt,confidence,accepted_event_id,created_at,decided_at,seq,field_seqs)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'',?,0,?,'{}')`,
			id, toUser, kind, inboxStatusPending, nonEmpty(req.Source, kind),
			fromUser, fromName, req.OriginCalendarID,
			req.Title, req.Notes, req.Location, boolToInt(req.AllDay),
			req.DTStartUTC, req.DurationSec, string(b),
			req.SourceExcerpt, req.Confidence, now, seq)
		return e
	})
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	it, _ := s.inboxByID(id)
	writeJSON(w, 200, map[string]any{"item": it, "seq": seq})
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// dedupeInt64 去重并保持稳定顺序 —— 提醒列表里重复的"提前 30 分钟"没有意义。
func dedupeInt64(in []int64) []int64 {
	if len(in) == 0 {
		return []int64{}
	}
	seen := map[int64]bool{}
	out := []int64{}
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

// handleListInbox 只返回自己的行。
//
// **过期不改 status**（方案 §9.2）：读操作不许写库，也免得引入定时任务。
// 过期由各端展示层判（`dtstart_utc + duration_sec < now - 24h` 且 pending），
// 并且**过期条目不占未读数、不进提醒链**。
func (s *Server) handleListInbox(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	status := r.URL.Query().Get("status")
	if status == "" {
		status = inboxStatusPending
	}
	limit := inboxListDefault
	if v, ok := atoi64(r.URL.Query().Get("limit")); ok && v > 0 {
		limit = int(v)
	}
	items, err := s.inboxItemsFor(uid, status, limit)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "count": len(items)})
}

type inboxDecisionReq struct {
	Action  string `json:"action"`
	EventID string `json:"event_id"`
}

// handleInboxDecision 只改状态。
//
// **服务端不造事件** —— 客户端先用既有 POST /api/events 建成功，
// 再把 event_id 一起提交过来。否则就有第二条写事件的路径。
//
// 幂等：同一 action 重复提交返回 200 不变更；pending → 终态单向
// （已 accepted 的条目再提交 dismissed 会被拒，防止误撤回已落库的行程）。
func (s *Server) handleInboxDecision(w http.ResponseWriter, r *http.Request) {
	s.inboxDecide(w, r, false)
}

// handleInboxBatch 批量决策：**一次事务，全有或全无**（红线 4）。
//
// 不要"每点一下发一个请求"——那样用户看到的是"改了一半"。
func (s *Server) handleInboxBatch(w http.ResponseWriter, r *http.Request) {
	s.inboxDecide(w, r, true)
}

func (s *Server) inboxDecide(w http.ResponseWriter, r *http.Request, batch bool) {
	uid := userIDOf(r)

	var ids []string
	var action, eventID string

	if batch {
		var req struct {
			IDs     []string `json:"ids"`
			Action  string   `json:"action"`
			EventID string   `json:"event_id"`
		}
		if err := readJSON(r, &req); err != nil {
			apiErr(w, 400, "bad json")
			return
		}
		ids, action, eventID = req.IDs, req.Action, req.EventID
		if len(ids) == 0 {
			apiErr(w, 400, "ids 不能为空")
			return
		}
		if len(ids) > inboxMaxBatch {
			apiErr(w, 400, "一次最多处理 "+itoa(int64(inboxMaxBatch))+" 条")
			return
		}
	} else {
		id := r.PathValue("id")
		if id == "" {
			apiErr(w, 400, "缺少 id")
			return
		}
		var req inboxDecisionReq
		if err := readJSON(r, &req); err != nil {
			apiErr(w, 400, "bad json")
			return
		}
		ids, action, eventID = []string{id}, req.Action, req.EventID
	}

	if action != inboxStatusAccepted && action != inboxStatusDismissed {
		apiErr(w, 400, "action 必须是 accepted 或 dismissed")
		return
	}

	// 幂等判定返回给调用方：哪些真变了、哪些本来就是这个状态
	changed := []string{}
	unchanged := []string{}

	err := s.withTx(func(tx *sql.Tx) error {
		for _, id := range ids {
			it, ok := s.inboxByID(id)
			if !ok || it.UserID != uid {
				// 归属不对当"没发生过"处理：既不报错也不改别人数据。
				// 报 404 会把"这条存在但不归你"这个事实漏出去。
				unchanged = append(unchanged, id)
				continue
			}
			if it.Status == action {
				unchanged = append(unchanged, id) // 幂等：已经是这个状态
				continue
			}
			if it.Status != inboxStatusPending {
				// pending → 终态是单向的；已决的条目不许再翻。
				unchanged = append(unchanged, id)
				continue
			}
			now := time.Now().Unix()
			seq, e := nextSeq(tx, "inbox", id, "decide")
			if e != nil {
				return e
			}
			evID := it.AcceptedEventID
			if action == inboxStatusAccepted && strings.TrimSpace(eventID) != "" {
				evID = strings.TrimSpace(eventID)
			}
			if _, e := tx.Exec(
				`UPDATE inbox_items SET status=?, accepted_event_id=?, decided_at=?, seq=? WHERE id=? AND user_id=?`,
				action, evID, now, seq, id, uid); e != nil {
				return e
			}
			changed = append(changed, id)
		}
		return nil
	})
	if err != nil {
		// 批量事务：任何一条失败 → 整体回滚，一条都不会改
		apiErr(w, 500, err.Error())
		return
	}

	seq, _ := s.db.currentSeq()
	writeJSON(w, 200, map[string]any{
		"ok": true, "changed": changed, "unchanged": unchanged, "seq": seq,
	})
}

// handleDeleteInbox 撤回/丢弃一条**未决**的收件箱条目。
//
// 两种情况都允许，但**都只能在 pending 时**：
//
//	· 收件人自己丢弃（还没决定，不想看到它了）
//	· 发件人撤回（分享出去又反悔了，别让对方看到）
//
// 已 accepted 的不许删 —— 那已经在收件人日历里成了 event，
// 删掉收件箱条目会让人以为"行程也撤销了"，但它没有。
func (s *Server) handleDeleteInbox(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	it, ok := s.inboxByID(id)
	if !ok {
		apiErr(w, 404, "条目不存在")
		return
	}
	if it.UserID != uid && it.FromUserID != uid {
		apiErr(w, 403, "无权删除这条")
		return
	}
	if it.Status != inboxStatusPending {
		apiErr(w, 409, "已处理的条目不能撤回（它已经在你或对方的日历里了）")
		return
	}
	err := s.withTx(func(tx *sql.Tx) error {
		if _, e := nextSeq(tx, "inbox", id, "delete"); e != nil {
			return e
		}
		_, e := tx.Exec(`DELETE FROM inbox_items WHERE id=? AND status=?`, id, inboxStatusPending)
		return e
	})
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// userNameOf 取用户名，取不到给空串（收件箱里 from_name 只是省一次查询的冗余）。
//
// ⚠️ 本函数定义在 reports.go（那边带了 uid 为空的短路）。这里不重复定义 ——
// Go 同一个包内重名函数会直接编译失败。

// pushSystemInbox 由**服务端**给某个用户塞一条系统通知。
//
// 用于"举报处理结果"这类系统主动告知。它走的是与用户收件箱完全相同的
// 表与同步通道 —— 这样四端不需要任何改动就能看到（收件箱各端早已实现）。
//
// from_user_id 留空：系统通知没有发件人。前端据此显示成"系统"而非某人。
// 返回值只用于日志，调用方不该因为通知失败而回滚业务操作 ——
// "举报处理成功了但通知没发出去"和"举报处理失败"是两件严重程度完全不同的事。
func (s *Server) pushSystemInbox(toUser, title, body string) {
	if strings.TrimSpace(toUser) == "" {
		return
	}
	id := newID("ib_")
	now := time.Now().Unix()
	err := s.withTx(func(tx *sql.Tx) error {
		seq, e := nextSeq(tx, "inbox", id, "upsert")
		if e != nil {
			return e
		}
		// status 直接给 dismissed：系统通知是**已读即归档**的，不需要用户
		// 接受或忽略（没有可决策的东西）。给 pending 会制造一条永远悬着的
		// 待办、并占用未读角标，用户只能手动清掉，纯属噪音。
		//
		// ⚠️ 实参必须与上面 22 个列**逐个对齐**（见下方注释的列序）。
		// 少给一个不会编译报错 —— database/sql 是运行期校验，会返回
		// "sql: expected 22 arguments, got 10" 并在日志里静默失败：
		// 举报处理结果永远进不了收件箱，而接口仍然返回 200。
		_, e = tx.Exec(`
			INSERT INTO inbox_items(
				id,user_id,kind,status,source,from_user_id,from_name,origin_calendar_id,
				title,notes,location,all_day,dtstart_utc,duration_sec,remind_minutes_before,
				source_excerpt,confidence,accepted_event_id,created_at,decided_at,seq,field_seqs)
			VALUES(?,?,?,'dismissed','system','','',?,
			       ?,?,'',0,0,0,'[]',?,'',?,?,?,?, '{}')`,
			id, toUser, inboxKindSystem, "", // id, user_id, kind, origin_calendar_id
			title, body, // title, notes
			body,     // source_excerpt
			"",       // accepted_event_id
			now, now, // created_at, decided_at
			seq) // seq
		return e
	})
	if err != nil {
		log.Printf("inbox: pushing system notice to %s failed: %v", toUser, err)
	}
}

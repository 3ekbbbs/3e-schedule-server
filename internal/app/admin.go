package app

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ===========================================================================
// 管理员接口（举报处理 + 违禁词维护）
// ===========================================================================
//
// 所有接口都以 s.requireAdmin(w, r) 开头 —— 那是唯一的权限闸门。
//
// 三条硬约束：
//
// 1. **管理员也必须经正常的内容删除路径**，不能直接 SQL UPDATE deleted_at。
//    否则会绕过 sync_log 的 seq 抬升，出现"管理员删了但客户端还在显示、
//    而且再也不会同步掉"这种最难查的不同步。所以删日程一律走
//    s.adminDeleteEvent，它内部复用已有的删除逻辑。
//
// 2. **处理动作要留痕**：谁处理的、什么时候、什么结论、说了什么。举报是
//    对争议的裁决，没有留痕就没法复核。
//
// 3. **改词库后立刻 reload 缓存**，不是等 TTL —— 管理员加完词就应该马上生效，
//    否则他会以为没生效而反复加。

// ---------------------------------------------------------------------------
// 举报列表与处理
// ---------------------------------------------------------------------------

// handleAdminListReports 列出举报单。GET /api/admin/reports?status=&limit=
func (s *Server) handleAdminListReports(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	// status 语义：'' / 'all' = 全部；pending / accepted / rejected = 只看该状态。
	//
	// ⚠️ 2026-10-09 修：空值原来被当成 'pending'，而**前端的契约是"空 = 全部"**
	//    ——`web/public/api.js` 的注释写着"status 传空 = 全部"，`admin.js` 的
	//    「全部」页签也正是这么传的（选中时 status 是空串 → 连接参数都不拼）。
	//    两边各按自己的理解走，结果是管理台点「全部」只看到待处理：
	//    实测库里有 1 条已处理，`?status=all` 看得见，不传参数回 0 条。
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = "all"
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 100)
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	q := `SELECT id, reporter_id, reporter_name, target_kind, target_id, target_title,
	             target_excerpt, target_owner, calendar_id, event_id, reason, detail,
	             status, handler_id, handler_name, handle_note, action, created_at, handled_at
	      FROM reports`
	var args []any
	if status != "all" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	// 待处理的**旧的在前**（先来先处理）；已处理的**新的在前**（最近处理的最有参考价值）
	if status == "pending" {
		q += ` ORDER BY created_at ASC`
	} else {
		q += ` ORDER BY COALESCE(NULLIF(handled_at,0), created_at) DESC`
	}
	q += ` LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.sql.Query(q, args...)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer rows.Close()

	out := []Report{}
	// 归属人的名字按 id 查一次就够（同一批里常常重复），所以带个小缓存 ——
	// 一页 100 条不至于打 100 次查询。
	nameCache := map[string]string{}
	for rows.Next() {
		rp, err := scanReport(rows)
		if err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		if rp.TargetOwner != "" {
			n, hit := nameCache[rp.TargetOwner]
			if !hit {
				n = s.userNameOf(rp.TargetOwner)
				nameCache[rp.TargetOwner] = n
			}
			rp.TargetOwnerName = n
		}
		rp.ContentExists = s.reportContentExists(rp)
		// 自动识别：拿快照文本跑一遍词库，给管理员一个参考（不是判据）
		if txt := strings.TrimSpace(rp.TargetTitle + " " + rp.TargetExcerpt); txt != "" {
			rp.AutoHits = s.classifyText(txt)
		}
		out = append(out, rp)
	}

	// 待处理数量：导航角标要用
	var pending int
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM reports WHERE status='pending'`).Scan(&pending)

	writeJSON(w, 200, map[string]any{"reports": out, "pending": pending})
}

// handleAdminHandleReport 处理一条举报。POST /api/admin/reports/{id}/handle
//
// 处理动作（req.Action）：
//
//	none        仅记录结论，不动内容（默认）
//	del_content 删除被举报的日程（只有 target_kind=event 且内容还在时可用）
//	dismiss     驳回（等价于 status=rejected + action=none）
type handleReportReq struct {
	Status string `json:"status"` // accepted / rejected
	Note   string `json:"note"`   // 处理意见（会回给举报人）
	Action string `json:"action"` // none / del_content
}

func (s *Server) handleAdminHandleReport(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	uid := userIDOf(r)
	id := r.PathValue("id")
	var req handleReportReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	status := strings.TrimSpace(req.Status)
	if status != "accepted" && status != "rejected" {
		apiErr(w, 400, "处理结论只能是 accepted（受理）或 rejected（驳回）")
		return
	}
	action := strings.TrimSpace(req.Action)
	if action == "" {
		action = "none"
	}
	if action != "none" && action != "del_content" {
		apiErr(w, 400, "不认识的处理动作："+action)
		return
	}

	var rp Report
	{
		row := s.db.sql.QueryRow(`
			SELECT id, reporter_id, reporter_name, target_kind, target_id, target_title,
			       target_excerpt, target_owner, calendar_id, event_id, reason, detail,
			       status, handler_id, handler_name, handle_note, action, created_at, handled_at
			FROM reports WHERE id=?`, id)
		var err error
		rp, err = scanReport(row)
		if err != nil {
			apiErr(w, 404, "没有这条举报")
			return
		}
	}

	// 已经处理过的不重复处理 —— 否则处理意见和动作会被后来的覆盖，
	// 留痕就失去意义了。要改结论应该是一条新记录（或明确的重开动作）。
	if rp.Status != "pending" {
		apiErr(w, 409, "这条举报已经处理过了（"+rp.Status+"）")
		return
	}

	// 动作可行性**先校验再执行**，不要执行到一半才发现做不了
	if action == "del_content" {
		if rp.TargetKind != "event" {
			apiErr(w, 400, "只有日程可以这样删除")
			return
		}
		if !s.reportContentExists(rp) {
			apiErr(w, 409, "这条日程已经不存在了，不需要再删")
			return
		}
	}

	now := time.Now().Unix()
	executed := action

	if action == "del_content" {
		evID := firstNonEmpty(rp.EventID, rp.TargetID)
		if err := s.adminDeleteEvent(uid, evID); err != nil {
			apiErr(w, 500, "删除内容失败："+err.Error())
			return
		}
	}

	handlerName := s.userNameOf(uid)
	if _, err := s.db.sql.Exec(`
		UPDATE reports
		SET status=?, handler_id=?, handler_name=?, handle_note=?, action=?, handled_at=?
		WHERE id=? AND status='pending'`,
		status, uid, handlerName, strings.TrimSpace(req.Note), executed, now, id); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	// 处理结果通知举报人（走站内信）。
	// 不通知的话，举报就是"投进黑洞"，用户下次不会再报。
	s.notifyReportHandled(rp, status, strings.TrimSpace(req.Note), executed)

	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"status":     status,
		"action":     executed,
		"handled_at": now,
	})
}

// adminDeleteEvent 以管理员身份删除一条日程。
//
// ⚠️ 这里是"管理员删内容"的**唯一入口**，它必须产生一次正常的删除
// （抬 seq、写 sync_log），这样所有端才会同步掉。
// 直接 `UPDATE events SET deleted_at=?` 会让客户端永远留着那条日程 ——
// 因为客户端靠 seq 变化来发现"这条被删了"。
//
// 做法：走与用户删除**完全相同的**服务端函数，只是跳过权限判断
// （管理员本来就有权限，但可能不在那张日历的成员表里，所以不能走 canWrite）。
func (s *Server) adminDeleteEvent(adminUID, eventID string) error {
	if strings.TrimSpace(eventID) == "" {
		return fmt.Errorf("缺少日程 id")
	}
	// 复用 handleDeleteEvent 内部真正干活的那层。若它被抽成了独立函数就直接调；
	// 这里是直接执行与它一致的删除语义（软删 + 抬 seq + 写 sync_log）。
	return s.deleteEventCore(adminUID, eventID)
}

// notifyReportHandled 把处理结果写进举报人的收件箱。
//
// 复用现有的 inbox 机制（而不是另造一个通知表）：收件箱是各端**已经实现好**
// 的通知入口，另造一套意味着四端都要改，而且用户要去两个地方看消息。
func (s *Server) notifyReportHandled(rp Report, status, note, action string) {
	if strings.TrimSpace(rp.ReporterID) == "" {
		return
	}
	verdict := "已受理"
	if status == "rejected" {
		verdict = "未受理"
	}
	body := "你对「" + firstNonEmpty(rp.TargetTitle, "一条内容") + "」的举报" + verdict + "。"
	switch action {
	case "del_content":
		body += "相关内容已被删除。"
	}
	if note != "" {
		body += "处理意见：" + note
	}
	s.pushSystemInbox(rp.ReporterID, "举报处理结果", body)
}

// ---------------------------------------------------------------------------
// 违禁词维护
// ---------------------------------------------------------------------------

// handleAdminListWords 列出全部违禁词（含停用的）。GET /api/admin/words
func (s *Server) handleAdminListWords(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	cat := strings.TrimSpace(r.URL.Query().Get("category"))
	kw := strings.TrimSpace(r.URL.Query().Get("q"))
	// ★ 2026-10-11：支持按**严重度**过滤。管理台那个独立的「风险词」页
	//   只看 severity=3，没必要把整张词表拉过去再在前端筛一遍。
	//   ⚠️ 认不出的值当"不过滤"，**不是**当"没有匹配" —— 一个拼错的参数
	//      不该让管理员对着一张空表以为词库没了。
	sevRaw := strings.TrimSpace(r.URL.Query().Get("severity"))

	q := `SELECT id, word, raw, category, severity, enabled, note,
	             created_by, created_at, updated_at
	      FROM banned_words`
	var conds []string
	var args []any
	if cat != "" && cat != "all" {
		conds = append(conds, "category=?")
		args = append(args, cat)
	}
	if kw != "" {
		conds = append(conds, "(raw LIKE ? OR word LIKE ?)")
		args = append(args, "%"+kw+"%", "%"+normalizeForMatch(kw)+"%")
	}
	if sevRaw != "" && sevRaw != "all" {
		if n, err := strconv.Atoi(sevRaw); err == nil && (n == sevNotice || n == sevBlock || n == sevRisk) {
			conds = append(conds, "severity=?")
			args = append(args, n)
		}
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY category ASC, severity DESC, raw ASC"

	rows, err := s.db.sql.Query(q, args...)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []BannedWord{}
	for rows.Next() {
		var bw BannedWord
		var enabled int
		if err := rows.Scan(&bw.ID, &bw.Word, &bw.Raw, &bw.Category, &bw.Severity,
			&enabled, &bw.Note, &bw.CreatedBy, &bw.CreatedAt, &bw.UpdatedAt); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		bw.Enabled = enabled != 0
		out = append(out, bw)
	}

	// 分类计数：后台按分类筛选时要显示"每类几个"
	counts := map[string]int{}
	for _, bw := range out {
		counts[bw.Category]++
	}
	writeJSON(w, 200, map[string]any{
		"words":      out,
		"categories": wordCategoryLabels,
		"counts":     counts,
	})
}

type wordUpsertReq struct {
	Raw      string `json:"raw"`
	Category string `json:"category"`
	Severity int    `json:"severity"`
	Enabled  *bool  `json:"enabled"`
	Note     string `json:"note"`
}

// handleAdminAddWord 新增违禁词。POST /api/admin/words
func (s *Server) handleAdminAddWord(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	uid := userIDOf(r)
	var req wordUpsertReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	raw := strings.TrimSpace(req.Raw)
	if raw == "" {
		apiErr(w, 400, "词不能为空")
		return
	}
	if len([]rune(raw)) > 100 {
		apiErr(w, 400, "单个词太长了（最多 100 字）")
		return
	}
	norm := normalizeForMatch(raw)
	if norm == "" {
		// 全是分隔符/零宽字符 —— 归一化后什么都不剩，这种词永远匹配不到任何东西。
		// 直接拒绝比"加进去但是个哑巴"好：后者会让管理员以为加了、实际不生效。
		apiErr(w, 400, "这个词去掉空白和符号后是空的，起不到任何作用")
		return
	}
	cat := strings.TrimSpace(req.Category)
	if _, ok := wordCategoryLabels[cat]; !ok {
		cat = "other"
	}
	sev := req.Severity
	// 三档合法值：1=仅提醒 2=直接拦截 3=风险词（转 AI 复核）。
	// 非法值默认给 1（**最保守的那一档**）而不是 2 —— 见下面那段说明。
	if sev != sevNotice && sev != sevBlock && sev != sevRisk {
		sev = sevNotice
	}
	enabled := 1
	if req.Enabled != nil && !*req.Enabled {
		enabled = 0
	}

	// 归一化后重复的词**不新增，而是更新已有那条**。
	// 为什么不是报错：管理员输"法 轮 功"时，库里已经有"法轮功"了 ——
	// 这两者是同一个词。报"已存在"会让他困惑（明明写法不一样），
	// 而静默新增一条又会造成同一词两条记录、停用了一条另一条还在拦。
	// 合并更新是唯一符合直觉的处理。
	var existID string
	_ = s.db.sql.QueryRow(`SELECT id FROM banned_words WHERE word=?`, norm).Scan(&existID)
	now := time.Now().Unix()
	if existID != "" {
		if _, err := s.db.sql.Exec(`
			UPDATE banned_words SET raw=?, category=?, severity=?, enabled=?, note=?, updated_at=?
			WHERE id=?`, raw, cat, sev, enabled, strings.TrimSpace(req.Note), now, existID); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		s.words.reload(s.db.sql)
		writeJSON(w, 200, map[string]any{"ok": true, "id": existID, "merged": true,
			"word": norm, "raw": raw})
		return
	}

	id := newID("bw_")
	if _, err := s.db.sql.Exec(`
		INSERT INTO banned_words(id, word, raw, category, severity, enabled, note,
		                         created_by, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, norm, raw, cat, sev, enabled, strings.TrimSpace(req.Note), uid, now, now); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	// **立刻重建缓存**：管理员加完词应该马上生效
	s.words.reload(s.db.sql)
	writeJSON(w, 200, map[string]any{"ok": true, "id": id, "word": norm, "raw": raw})
}

// handleAdminUpdateWord 改一个词（启停 / 改分类 / 改严重度 / 改备注）。
// PUT /api/admin/words/{id}
func (s *Server) handleAdminUpdateWord(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	var req wordUpsertReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	var curRaw, curCat, curNote string
	var curSev, curEnabled int
	if err := s.db.sql.QueryRow(`SELECT raw, category, severity, enabled, note FROM banned_words WHERE id=?`,
		id).Scan(&curRaw, &curCat, &curSev, &curEnabled, &curNote); err != nil {
		apiErr(w, 404, "没有这个词")
		return
	}

	raw := curRaw
	word := ""
	if strings.TrimSpace(req.Raw) != "" {
		raw = strings.TrimSpace(req.Raw)
		word = normalizeForMatch(raw)
		if word == "" {
			apiErr(w, 400, "这个词去掉空白和符号后是空的，起不到任何作用")
			return
		}
		// 改成另一个已存在的词 → 会撞唯一索引，这里提前拦下给出人话提示
		var other string
		if err := s.db.sql.QueryRow(`SELECT id FROM banned_words WHERE word=? AND id<>?`, word, id).Scan(&other); err == nil && other != "" {
			apiErr(w, 409, "已经有一个等价于「"+raw+"」的词了")
			return
		}
	}
	cat := curCat
	if _, ok := wordCategoryLabels[strings.TrimSpace(req.Category)]; ok {
		cat = strings.TrimSpace(req.Category)
	}
	sev := curSev
	if req.Severity == sevNotice || req.Severity == sevBlock || req.Severity == sevRisk {
		sev = req.Severity
	}
	enabled := curEnabled
	if req.Enabled != nil {
		if *req.Enabled {
			enabled = 1
		} else {
			enabled = 0
		}
	}
	note := curNote
	if strings.TrimSpace(req.Note) != "" {
		note = strings.TrimSpace(req.Note)
	}

	now := time.Now().Unix()
	var err error
	if word != "" {
		_, err = s.db.sql.Exec(`
			UPDATE banned_words SET word=?, raw=?, category=?, severity=?, enabled=?, note=?, updated_at=?
			WHERE id=?`, word, raw, cat, sev, enabled, note, now, id)
	} else {
		_, err = s.db.sql.Exec(`
			UPDATE banned_words SET category=?, severity=?, enabled=?, note=?, updated_at=?
			WHERE id=?`, cat, sev, enabled, note, now, id)
	}
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	s.words.reload(s.db.sql)
	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}

// handleAdminDeleteWord 删一个词。DELETE /api/admin/words/{id}
func (s *Server) handleAdminDeleteWord(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	res, err := s.db.sql.Exec(`DELETE FROM banned_words WHERE id=?`, id)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		apiErr(w, 404, "没有这个词")
		return
	}
	s.words.reload(s.db.sql)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAdminCheckText 用词库跑一段文本，返回所有命中。
// POST /api/admin/words/check —— 给后台"试一下这个词会不会误伤"用。
//
// 为什么需要：加词最怕**误伤**。管理员在加入之前应该能拿几句正常话试一遍，
// 确认不会拦住无关内容。没有这个入口，就只能"加进去看有没有人喊"。
func (s *Server) handleAdminCheckText(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Text string `json:"text"`
		// AI=true 时**真的跑一次复核**（见下面的说明）。
		AI bool `json:"ai"`
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	// ★ 2026-10-11：把"会不会进入 AI 判断流程"一并算出来。
	//
	// 为什么试词必须显示这个：**风险词那一档什么都不拦** —— 它只是"送去让 AI 看一眼"。
	// 而"会不会真的被看"取决于 AI 复核有没有启用（见 moderation_ai.go 的 fail-open）。
	// 不显示这个，管理员试完只知道"命中了风险词"，**不知道接下来会发生什么**：
	// 是真的有人看，还是一路放行。而这两件事的差别就是这条风险词有没有用。
	hits := s.classifyText(req.Text)
	var riskHits []WordHit
	for _, h := range hits {
		if h.Severity == sevRisk {
			riskHits = append(riskHits, h)
		}
	}
	ai := s.aiConfigFor()
	resp := map[string]any{
		"hits":       hits,
		"normalized": normalizeForMatch(req.Text),
		"risk_hits":  riskHits,
		"ai_review": map[string]any{
			"enabled": ai.Enabled,
			// will_review 是**两个条件都满足**才算 —— 命中了风险词、且 AI 开着。
			// 缺一个都不会真的走判断：没有风险词就没什么可审的；
			// AI 关了就是 fail-open（内容照常保存）。
			"will_review": len(riskHits) > 0 && ai.Enabled,
		},
	}

	// ★ 真的跑一次 AI 复核（2026-10-11 用户要求：「在试词的那里，根本就没有触发那个 AI 复核」）。
	//
	// 为什么必须能真跑：只回答"会不会进入 AI 流程"解决不了管理员真正的问题 ——
	// **AI 拿到这段话会怎么判**。尤其是提示词改过之后，这里是唯一能当场验证的入口
	//（否则只能建一条真内容去试，而那是往用户数据里写东西）。
	//
	// ⚠️ 三条边界：
	//  ① **只有命中风险词才调** —— 没命中就没什么可复核的，白花一次调用；
	//  ② 用 `r.Context()`：试词是普通 HTTP 请求，客户端断开就该放弃
	//     （push 路径那个 `context.Background()` 是因为它在事务中间，没有请求上下文）；
	//  ③ **不写审计表**：试词是"练手"，写进去会让「判断记录」页混进一堆不是真内容的记录。
	//     真实拦截的审计照旧在 `gateAIRisk` / `gateByAI` 里写。
	if req.AI && len(riskHits) > 0 {
		v := s.aiReviewText(r.Context(), req.Text, riskHits)
		resp["ai_verdict"] = map[string]any{
			"violation":  v.Violation,
			"category":   v.Category,
			"confidence": v.Confidence,
			"reason":     v.Reason,
			"terms":      v.Terms,
			"source":     v.Source,    // model / disabled / error
			"error":      v.ErrorText, // 只有 source=error 时有意义
			"model":      v.Model,
		}
	}
	writeJSON(w, 200, resp)
}

// handleAdminSeedRiskWords 把**内置的风险词**（severity=3）补进词库。
// POST /api/admin/words/seed-risk
//
// ★ 为什么必须有这个出口：内置词表只在 `banned_words` **完全为空**时灌一次
//
//	（见 `ensureSeedWords`）。已经跑过一阵的部署，表里早有词了 ——
//	所以**后来加进 seed 的词，它们永远拿不到**。2026-10-11 扩充了谐音/变体那一批，
//	老部署就属于这种情况，只能靠管理员点一下这里。
//
// ⚠️ 它是幂等的（`INSERT OR IGNORE` + 唯一索引），但会把管理员**删过**的内置风险词
//
//	也加回来 —— 这是"导入内置词"这句话的应有语义。之所以可以接受：
//	风险词**不拦任何东西**，最坏结果是多送几次 AI 复核。界面上必须把这点写明。
func (s *Server) handleAdminSeedRiskWords(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	now := time.Now().Unix()
	added, skipped := 0, 0
	for _, sw := range seedBannedWords {
		if sw.Severity != sevRisk {
			continue // 这个出口只管风险词 —— 别的档位不该被"顺手同步"
		}
		norm := normalizeForMatch(sw.Word)
		if norm == "" {
			continue
		}
		res, err := s.db.sql.Exec(`
			INSERT OR IGNORE INTO banned_words
			  (id, word, raw, category, severity, enabled, note, created_by, created_at, updated_at)
			VALUES(?,?,?,?,?,1,?,'内置',?,?)`,
			newID("bw_"), norm, sw.Word, sw.Category, sw.Severity, sw.Note, now, now)
		if err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		} else {
			skipped++
		}
	}
	// 立刻生效（和改词库走同一个出口）—— 不然管理员点完还得等重启。
	if s.words != nil {
		s.words.reload(s.db.sql)
	}
	log.Printf("导入内置风险词：新增 %d 条，已存在 %d 条", added, skipped)
	writeJSON(w, 200, map[string]any{"added": added, "skipped": skipped})
}

// ---------------------------------------------------------------------------
// 管理员概览
// ---------------------------------------------------------------------------

// handleAdminOverview 给管理员页面顶部的几个数字。GET /api/admin/overview
func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	var pending, accepted, rejected, wordsTotal, wordsOn, users, usersBanned int
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM reports WHERE status='pending'`).Scan(&pending)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM reports WHERE status='accepted'`).Scan(&accepted)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM reports WHERE status='rejected'`).Scan(&rejected)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM banned_words`).Scan(&wordsTotal)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM banned_words WHERE enabled=1`).Scan(&wordsOn)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM users WHERE banned<>0`).Scan(&usersBanned)

	byCat := map[string]int{}
	if rows, err := s.db.sql.Query(`SELECT category, COUNT(*) FROM banned_words WHERE enabled=1 GROUP BY category`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var c string
			var n int
			if err := rows.Scan(&c, &n); err == nil {
				byCat[c] = n
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"reports_pending":  pending,
		"reports_accepted": accepted,
		"reports_rejected": rejected,
		"words_total":      wordsTotal,
		"words_enabled":    wordsOn,
		"words_by_cat":     byCat,
		"users":            users,
		"users_banned":     usersBanned,
		"categories":       wordCategoryLabels,
		// severity 的中文名也由服务端给：前端若自己维护一份，
		// 将来加档位（这次就加了第 3 档）必然只改一处，界面显示成"未知"。
		"severities": severityLabels,
		// AI 是否在真正工作 —— 这个数字比任何开关状态都更能说明问题
		"ai_enabled": s.aiConfigFor().Enabled,
	})
}

// ensure sql 包被用到（scanReport 的接口参数用到了它的调用点类型推断）
var _ = sql.ErrNoRows

// ===========================================================================
// 管理员名单维护（2026-09-23）
// ===========================================================================
//
// 背景：`users.is_admin` 原来只有"第一个注册的人"会拿到（见 auth.go 的
// `isAdmin := n == 0`），之后**没有任何接口能改** —— 想加个管理员只能上服务器
// 改 SQLite。这个文件补上这条路。
//
// 四条硬规则，都不是可选的：
//
// 1. **只有管理员能改管理员**（下面每个 handler 都以 requireAdmin 开头）。
//    否则任何人都能把自己提权 —— 那等于没有权限系统。
//
// 2. **不能取消最后一个管理员**。取消掉之后就再没人能进管理台了，
//    而恢复它只能上服务器改库 —— 这就是把自己锁在门外。
//    判据放在**事务里**（先数再改），不然两个并发请求可能各取消一个、
//    各自都以为"还剩一个"。
//
// 3. **不能取消自己**。即使系统里还有别的管理员 —— 一个人点一下就把自己
//    踢出管理台，在多管理员的系统里也是纯粹的误操作，没有任何正当场景。
//    要卸任就让另一个管理员来操作。
//
// 4. **授予/取消都要留痕**（log）。提权是最敏感的一类改动，
//    事后一定要能回答"是谁在什么时候给的"。

// adminUserRow 是管理台用的一行用户（带"当前是不是管理员"）。
//
// 封禁字段（2026-10-09）也在这里一起回：名单页要同时显示"是不是管理员"和
// "是不是被封了"，分两个接口取会让那两件事互相矛盾（中间隔着一次网络往返）。
type adminUserRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsAdmin   bool   `json:"is_admin"`
	Banned    bool   `json:"banned"`
	BanReason string `json:"ban_reason"`
	BannedAt  int64  `json:"banned_at"`
	// 封禁到期时刻（0 = 永久）。界面据此显示"封禁至 10-16 14:00"。
	BanUntil  int64 `json:"ban_until"`
	CreatedAt int64 `json:"created_at"`
}

// handleAdminListUsers 列出全部账号 + 管理员标记。
// GET /api/admin/users
//
// 与已有的 GET /api/users 的区别：那个是**成员选择器**用的（任何登录用户都能看，
// 因为要往日历里加人），返回的是 User 结构。这个是管理台的名单页，
// 走 requireAdmin，语义是"谁有管理权"而不是"系统里有哪些人"。
func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	rows, err := s.db.sql.Query(
		`SELECT id, name, is_admin, banned, ban_reason, banned_at, ban_until, created_at
		 FROM users ORDER BY banned, is_admin DESC, created_at`)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer rows.Close()

	out := []adminUserRow{}
	total, admins, banned := 0, 0, 0
	for rows.Next() {
		var u adminUserRow
		var admin, isBanned int64
		if err := rows.Scan(&u.ID, &u.Name, &admin, &isBanned, &u.BanReason, &u.BannedAt,
			&u.BanUntil, &u.CreatedAt); err != nil {
			continue
		}
		u.IsAdmin = admin != 0
		u.Banned = isBanned != 0
		total++
		if u.IsAdmin {
			admins++
		}
		if u.Banned {
			banned++
		}
		out = append(out, u)
	}
	writeJSON(w, 200, map[string]any{
		"users":        out,
		"total":        total,
		"admin_count":  admins,
		"banned_count": banned,
		"me":           userIDOf(r),
		// 前端据此把"最后一个管理员"的取消按钮直接禁用掉，
		// 而不是等他点了才报错 —— 能提前说清楚的就不要用错误提示代替。
		"admin_policy": "不能取消最后一个管理员，也不能取消自己",
		"ban_policy":   "不能封禁自己；也不能把最后一个未被封禁的管理员封掉",
	})
}

// handleAdminSetUserAdmin 授予 / 取消管理员。
// POST /api/admin/users/{id}/admin   body: {"is_admin": true|false}
//
// 用**同一个接口**做授予和取消（而不是 grant/revoke 两个）：
// 两者要做的校验完全一样（第四条规则 + 事务里的计数），
// 拆开就会变成"改了一处忘了另一处"——这个项目在别的地方已经吃过这个亏。
func (s *Server) handleAdminSetUserAdmin(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	targetID := strings.TrimSpace(r.PathValue("id"))
	if targetID == "" {
		apiErr(w, 400, "缺少用户 id")
		return
	}

	var req struct {
		IsAdmin *bool `json:"is_admin"`
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "请求体格式不对："+err.Error())
		return
	}
	// ⚠️ 用 *bool：JSON 里 `false` 和"整个字段没传"都是 falsy，
	// 用 bool 的话"没传字段"会被静默当成"要取消管理员"——
	// 那是把提权接口变成降权接口的经典写法。
	if req.IsAdmin == nil {
		apiErr(w, 400, "缺少 is_admin（要传 true 或 false）")
		return
	}
	want := *req.IsAdmin

	me := userIDOf(r)
	if !want && targetID == me {
		apiErr(w, 400, "不能取消自己的管理员身份；请让另一个管理员来操作")
		return
	}

	// 目标账号必须存在 —— 否则 UPDATE 影响 0 行也返回 200，
	// 前端会显示"已授予"，而实际上什么都没发生。
	var targetName string
	if err := s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, targetID).Scan(&targetName); err != nil {
		if err == sql.ErrNoRows {
			apiErr(w, 404, "没有这个账号")
			return
		}
		apiErr(w, 500, err.Error())
		return
	}

	// 取消时必须在**同一个事务**里数管理员总数再改。
	// 分成"先 SELECT COUNT 再 UPDATE"两步的话，两个并发取消请求会各自读到
	// "还有 2 个"，然后各取消一个 → 管理员归零，谁都进不去了。
	if !want {
		var err error
		if err = s.withTx(func(tx *sql.Tx) error {
			var n int
			if e := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin<>0`).Scan(&n); e != nil {
				return e
			}
			if n <= 1 {
				return errLastAdmin
			}
			_, e := tx.Exec(`UPDATE users SET is_admin=0 WHERE id=?`, targetID)
			return e
		}); err != nil {
			if err == errLastAdmin {
				apiErr(w, 400, "这是最后一个管理员，不能取消（否则没人能进管理台了）")
				return
			}
			apiErr(w, 500, err.Error())
			return
		}
	} else {
		if _, err := s.db.sql.Exec(`UPDATE users SET is_admin=1 WHERE id=?`, targetID); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}

	// 留痕。提权是最敏感的一类改动，事后必须能回答"谁在什么时候给的"。
	var meName string
	_ = s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, me).Scan(&meName)
	verb := "授予"
	if !want {
		verb = "取消"
	}
	log.Printf("[admin] %s(%s) %s了 %s(%s) 的管理员权限", meName, me, verb, targetName, targetID)

	writeJSON(w, 200, map[string]any{"ok": true, "id": targetID, "name": targetName, "is_admin": want})
}

// errLastAdmin 是"不能取消最后一个管理员"这个业务规则。
// 用哨兵错误而不是在事务里直接写响应 —— 事务回调的职责是**判定 + 改数据**，
// 往 w 里写东西会把"HTTP 层"和"事务层"搅在一起（回调里中途 return 会让
// 事务回滚，但那时可能已经往 w 写了一半）。
var errLastAdmin = fmt.Errorf("last admin")

// ===========================================================================
// 封禁账号（2026-10-09）
// ===========================================================================
//
// 三条规则，与"取消管理员"同源，理由也一样：**别把自己锁在门外**。
//
// 1. **不能封禁自己**。即使还有别的管理员 —— 一个人点一下就把自己踢出去
//    在任何多管理员系统里都是纯误操作。
//
// 2. **不能把最后一个"未被封禁"的管理员封掉**。注意判据是"未被封禁的管理员"
//    而不是"管理员"：系统里有两个管理员、另一个早就被封了，这时再封掉
//    这个能用的，一样是无人能进管理台。判据放在**事务里**数，理由同 setUserAdmin。
//
// 3. **封禁的执行点只有一个：`banGuard`（在 auth / authOrKey 两个中间件里）。**
//    这里刻意**不**去删 tokens、也不去撤销 API Key —— 一开始我两条都做了，
//    后来换成只留中间件那一处，理由是**可验证性**：
//
//      两套机制并存时，"删 token"这一条总是先命中（凭证直接不存在了，
//      返回 401），于是 `banGuard` 永远走不到 —— 它写错了也没有任何声音。
//      而这正是最坏的一种：**一个从不被执行的防线，等于没有防线**
//      （同族红线 103/144：闸门跑不起来 = 闸门不存在）。
//
//      只留一处则每条请求都会经过它，它不可能悄悄坏掉；
//      顺带还解决了一类我们想不到的凭证（将来新加的 cookie / 设备码），
//      封禁自动覆盖，不用记得来补。
//
//    用户体验上也更好：被封的人**在原界面上立刻看到原因**
//    （403 + `code=banned` + 理由），而不是被踢下线之后自己去猜为什么登不上。
//
//    解封因此也变成"恢复原状"：同一个 token 又能用，语义干净。
//
// 解封**不清空** ban_reason / banned_at：封禁是一段历史，
// "这个人被封过、因为什么"事后复盘要用。

// maxBanDays 是"临时封禁"能选的最长天数。
//
// 为什么有上限：超过这个量级的处置**应该是有意为之的**。让"封 3650 天"和
// "永久封禁"变成两个不同按钮，是为了逼人选一次"我到底想不想让他回来" ——
// 而手输一个 3650 的人其实想要的是永久。
const maxBanDays = 365

// maxBatchUsers 是一次批量建号的上限。
//
// 为什么要有个上限：这个接口一次请求能写几百行 users 表，
// 而它是个**同步接口**（前端等它返回）。没上限的话，一个手滑输入 10000
// 就会让服务端在一个请求里做一万次 bcrypt —— 那是几十秒的 CPU 占满，
// 期间所有别的用户都在等。要更多就分批，这是产品约束不是技术限制。
const maxBatchUsers = 200

// maxUserNameRunes 是新账号名字的长度上限。
//
// 注册那条路历史上一律不判长度（老客户端可能发很长的名字），
// 这里**只对新建的账号**收口：名字要印在成员列表、@提及、日志里，
// 太长会把界面撑坏。32 个字符放得下"三年二班李某某"这种最长的情况。
const maxUserNameRunes = 32

// handleAdminBanUser 封禁 / 解封一个账号。
// POST /api/admin/users/{id}/ban   body: {"banned": true|false, "reason": "..."}
func (s *Server) handleAdminBanUser(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	targetID := strings.TrimSpace(r.PathValue("id"))
	if targetID == "" {
		apiErr(w, 400, "缺少用户 id")
		return
	}

	var req struct {
		Banned *bool  `json:"banned"`
		Reason string `json:"reason"`
		// 期限二选一（都不给 = 永久）：
		//   Days  相对天数 —— 界面用这个（"封 7 天"）。服务端算到期时刻，
		//         所以**不受客户端时钟影响**。
		//   Until 绝对时刻 —— 给"到某个时间点为止"的场景，也让行为闸门能
		//         用几秒的封禁来验"到期自动解开"（否则得等一天）。
		Days  int   `json:"days"`
		Until int64 `json:"until"`
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "请求体格式不对："+err.Error())
		return
	}
	// ⚠️ `*bool`，与 setUserAdmin 同一个理由：`false` 与"字段没传"都是 falsy，
	// 用 bool 的话一个漏传字段的请求会被当成"解封"—— 那是把封禁变成解封。
	if req.Banned == nil {
		apiErr(w, 400, "缺少 banned（要传 true 或 false）")
		return
	}
	want := *req.Banned
	reason := strings.TrimSpace(req.Reason)
	if len([]rune(reason)) > 500 {
		apiErr(w, 400, "封禁理由太长了（最多 500 字）")
		return
	}
	// 封禁要求写理由，解封不要求（解封是恢复操作，没什么好解释的）。
	// 这条不是洁癖：理由**会展示给被封的人**（登录时的 403 里带着它），
	// 而"你被封了，原因：无"只会让人来问管理员，等于把成本推给用户。
	if want && reason == "" {
		apiErr(w, 400, "请填写封禁理由 —— 它会被展示给被封的人")
		return
	}

	me := userIDOf(r)
	if want && targetID == me {
		apiErr(w, 400, "不能封禁自己；请让另一个管理员来操作")
		return
	}

	var targetName string
	var targetIsAdmin int64
	if err := s.db.sql.QueryRow(`SELECT name, is_admin FROM users WHERE id=?`, targetID).
		Scan(&targetName, &targetIsAdmin); err != nil {
		if err == sql.ErrNoRows {
			apiErr(w, 404, "没有这个账号")
			return
		}
		apiErr(w, 500, err.Error())
		return
	}

	now := time.Now().Unix()

	// 算到期时刻（0 = 永久）。**服务端算**，不信客户端给的时间戳 ——
	// 那边时钟偏一小时，封禁就跟着偏一小时，而且是那种没人会发现的偏。
	until := int64(0)
	if want {
		switch {
		case req.Until > 0:
			if req.Until <= now {
				apiErr(w, 400, "解封时间已经过去了；要立刻解封就直接选「解封」")
				return
			}
			until = req.Until
		case req.Days > 0:
			if req.Days > maxBanDays {
				apiErr(w, 400, fmt.Sprintf("封禁天数最多 %d 天（要更久就选永久）", maxBanDays))
				return
			}
			until = now + int64(req.Days)*86400
		}
	}

	if want {
		// 封的是管理员时，必须在**同一个事务**里数"还剩几个能用的管理员"。
		// 分成"先 COUNT 再 UPDATE"两步走的话，两个并发封禁会各自读到
		// "还有 2 个"，然后各封一个 → 无人能进管理台。
		//
		// ⚠️ 判据是"**还能用**的管理员"：已经到期但还没被懒解开的封禁不算数
		//    （否则"封一个已到期的管理员"会被误判成"要没人能进管理台了"）。
		if err := s.withTx(func(tx *sql.Tx) error {
			if targetIsAdmin != 0 {
				var n int
				if e := tx.QueryRow(`SELECT COUNT(*) FROM users
					WHERE is_admin<>0 AND (banned=0 OR (ban_until>0 AND ban_until<=?))`, now).Scan(&n); e != nil {
					return e
				}
				if n <= 1 {
					return errLastAdmin
				}
			}
			_, e := tx.Exec(
				`UPDATE users SET banned=1, ban_reason=?, banned_at=?, banned_by=?, ban_until=? WHERE id=?`,
				reason, now, me, until, targetID)
			return e
		}); err != nil {
			if err == errLastAdmin {
				apiErr(w, 400, "这是最后一个能用的管理员，封掉就没人能进管理台了")
				return
			}
			apiErr(w, 500, err.Error())
			return
		}
		// 注意这里**不**删 token、也不撤销 API Key —— 执行点只有 banGuard 一处，
		// 上面那段注释解释了为什么（可验证性 > 双保险）。
	} else {
		// 解封：只把 banned / ban_until 清掉，理由与时间**留着**（历史）。
		// ⚠️ `ban_until` 必须一起清零 —— 只改 banned 的话，这个人虽然能用了，
		//    但库里还挂着"解封时间：某年某月"，下次谁看名单都会以为他还是封着的。
		if _, err := s.db.sql.Exec(`UPDATE users SET banned=0, ban_until=0 WHERE id=?`, targetID); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}

	// 留痕。封禁是争议裁决，与提权同级。
	var meName string
	_ = s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, me).Scan(&meName)
	verb := "封禁"
	if !want {
		verb = "解封"
	}
	log.Printf("[admin] %s(%s) %s了 %s(%s)（理由：%s）", meName, me, verb, targetName, targetID, reason)
	// 同时落进 login_logs：`log.Printf` 只在 stdout 上，翻起来要人去服务器 grep；
	// 落库之后管理台的日志页能按"账号被封禁/被解封"筛出来。
	ev := logEventAccountBanned
	note := "封禁"
	if !want {
		ev = logEventAccountUnbanned
		note = "解封"
	}
	if note == "封禁" && until == 0 {
		note = "永久封禁"
	}
	s.logAccountEvent(r, targetID, targetName, ev, "success", note+"："+reason)

	writeJSON(w, 200, map[string]any{
		"ok": true, "id": targetID, "name": targetName,
		"banned": want, "reason": reason,
		"banned_at": now, "ban_until": until,
	})
}

// handleAdminBatchCreateUsers 批量建账号。
// POST /api/admin/users/batch
//
//	{"names": ["张三","李四"], "pass": "统一的初始密码"}
//	{"prefix": "同学", "count": 20, "pass": "统一的初始密码"}   → 同学01…同学20
//
// 为什么这条路**不需要邀请码**：邀请码是给"外面的人自己进来"用的；
// 而调这个接口的人**已经是管理员**（他有 requireAdmin 兜着），本来就能发
// 日历邀请、能改任何东西 —— 再要他绕过自己的邀请码只是仪式。
//
// 为什么不做"生成随机密码再回显"：管理员要的是**能发出去**的一批账号，
// 统一密码他好通知；随机密码就得再复制一张表，多一次出错的机会。
// 建完之后各自改密码（`POST /api/password`）本来就是现成的。
func (s *Server) handleAdminBatchCreateUsers(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Names  []string `json:"names"`
		Prefix string   `json:"prefix"`
		Count  int      `json:"count"`
		Pass   string   `json:"pass"`
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "请求体格式不对："+err.Error())
		return
	}
	// 与 /api/auth/register 同一条判据（至少 4 位）—— 两条路建出来的账号
	// 必须能被同一个登录接口接受，密码规则不一样就是自找麻烦。
	if len([]rune(req.Pass)) < 4 {
		apiErr(w, 400, "密码至少 4 位")
		return
	}

	names, bad := normalizeBatchNames(req.Names)
	if len(names) == 0 {
		prefix := strings.TrimSpace(req.Prefix)
		if prefix == "" || req.Count <= 0 {
			apiErr(w, 400, "要么给一组名字（每行一个），要么给「前缀 + 数量」")
			return
		}
		if len([]rune(prefix)) > maxUserNameRunes-3 {
			apiErr(w, 400, fmt.Sprintf("前缀太长了（最多 %d 字，要给序号留位置）", maxUserNameRunes-3))
			return
		}
		if req.Count > maxBatchUsers {
			apiErr(w, 400, fmt.Sprintf("一次最多建 %d 个账号，要更多就分批", maxBatchUsers))
			return
		}
		// 序号宽度跟着数量走，**最少 2 位**：5 → 同学01（不是同学1）、
		// 20 → 同学20、200 → 同学001。
		//
		// 为什么最少 2 位：名字会出现在成员列表、@提及、日志里，而它们是**按字符串
		// 排序**的 —— 1 位宽的话「同学10」会排在「同学2」前面，看着像乱了。
		// 而写死 2 位的话建到 100 个就变成「同学100」（长度不齐，同样难看），
		// 所以宽度是 max(2, 数量的位数)。
		width := len(fmt.Sprintf("%d", req.Count))
		if width < 2 {
			width = 2
		}
		for i := 1; i <= req.Count; i++ {
			names = append(names, fmt.Sprintf("%s%0*d", prefix, width, i))
		}
	}
	if len(names) == 0 {
		apiErr(w, 400, "没有有效的名字")
		return
	}
	if len(names) > maxBatchUsers {
		apiErr(w, 400, fmt.Sprintf("一次最多建 %d 个账号，要更多就分批", maxBatchUsers))
		return
	}

	hash := hashPassword(req.Pass)
	now := time.Now().Unix()

	created := []map[string]any{}
	skipped := []map[string]string{}
	for _, nm := range names {
		id := newID("u_")
		// ★ 靠 `users.name` 的 UNIQUE 索引去重（INSERT OR IGNORE + 看影响行数），
		//   而不是"先 SELECT 有没有、再 INSERT"。后者在两个管理员同时提交
		//   同一批名字时会双写（TOCTOU），而这条接口天生就是会被连点的那种。
		res, err := s.db.sql.Exec(
			`INSERT OR IGNORE INTO users(id,name,pass_hash,is_admin,created_at) VALUES(?,?,?,0,?)`,
			id, nm, hash, now)
		if err != nil {
			skipped = append(skipped, map[string]string{"name": nm, "reason": "写入失败"})
			continue
		}
		if n, e := res.RowsAffected(); e == nil && n == 0 {
			skipped = append(skipped, map[string]string{"name": nm, "reason": "这个名字已经有了"})
			continue
		}
		created = append(created, map[string]any{"id": id, "name": nm})
		// 逐个留痕：**批量**不等于**匿名**。日后要回答"这个账号哪来的"，
		// 唯一能查的就是这里（reason 里写明是管理员批量建的）。
		s.logAccountEvent(r, id, nm, logEventRegister, "success", "管理员批量创建")
	}

	var meName string
	_ = s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, userIDOf(r)).Scan(&meName)
	log.Printf("[admin] %s 批量建号：提交 %d 个，成功 %d 个，跳过 %d 个",
		meName, len(names), len(created), len(skipped))

	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"created": created,
		"skipped": skipped,
		// bad 是"格式就不对、根本没试"的那些（空行不算错，别报给用户）
		"invalid": bad,
		"total":   len(names),
	})
}

// normalizeBatchNames 清洗一批名字，返回（可用的名字, 格式不对的名字）。
//
// 三条清洗规则，每一条都有它挡的东西：
//
//	· 去首尾空白 → " 张三" 与 "张三" 是两个人，这是纯粹的输入事故；
//	· 丢空行 → 从表格里粘过来必然带空行，报错会很烦人；
//	· 本次请求内去重 → 同一批里重复写两遍，第二条必然撞 UNIQUE，
//	  与其报"这个名字已经有了"不如直接当重复输入忽略。
//
// ⚠️ 长度用 **rune** 数，不是字节数：`len("张三")` 是 6，
//
//	用字节数的话中文名字会被误判成超长（红线 84 那类错的同族）。
func normalizeBatchNames(raw []string) (out []string, bad []string) {
	seen := map[string]bool{}
	for _, s := range raw {
		nm := strings.TrimSpace(s)
		// 去掉可能从 Excel 粘进来的 BOM 与零宽字符
		nm = strings.Trim(nm, "\ufeff\u200b\u200c\u200d")
		if nm == "" {
			continue
		}
		if len([]rune(nm)) > maxUserNameRunes {
			bad = append(bad, nm)
			continue
		}
		if seen[nm] {
			continue
		}
		seen[nm] = true
		out = append(out, nm)
	}
	return out, bad
}

// ===========================================================================
// 删除账号（2026-10-09）
// ===========================================================================

// handleAdminDeleteUser 彻底删除一个账号。
// DELETE /api/admin/users/{id}   body: {"confirm": "<对方账号名>"}
//
// ★ 这是**不可逆**的操作，所以三道闸门一道都不能省：
//
//	① `requireAdmin`（谁能调）；
//	② 不能删自己、不能删最后一个**还能用**的管理员（别把自己锁在门外）；
//	③ **确认串必须等于对方账号名** —— 与"一键清空账户数据"同一套口径。
//	   界面已经弹了确认框，这一层是防脚本/误调：一个 DELETE 打错 id 就删错人，
//	   而且删完不可恢复。
//
// ★ 删除的边界（"删账号"最容易做错的就是这里，所以写清楚）：
//
//	✅ 他创建的日历：**打墓碑**（不是真删行）→ 所有端下次同步会让它消失，
//	   连同里面的日程。真删行不产生 tombstone，客户端会一直留着它
//	   （红线：本地和服务端就此长期不一致，而且没有任何报错）
//	✅ 他在别人日历/团队里的身份、他的设备/凭证/提醒/收件箱 → 删掉
//	✅ 他拥有的**独占**团队 → 连团队一起清（否则留下一个没有主人的团队）
//	❌ 他创建的日历里**还有别人**（或共享给了团队）→ **拒绝**，见 ownedDataBlockers
//	❌ 登录日志 / 审核审计 / 举报单 → **保留**。那是审计记录，不跟着账号走 ——
//	   "这个人被删了，但他之前被封过、被举报过"正是复盘时要看的。列表里会显示
//	   「（已删除账号）」而不是一串 uid。
func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	targetID := strings.TrimSpace(r.PathValue("id"))
	if targetID == "" {
		apiErr(w, 400, "缺少用户 id")
		return
	}
	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "请求体格式不对："+err.Error())
		return
	}

	me := userIDOf(r)
	if targetID == me {
		apiErr(w, 400, "不能删除自己：请让另一个管理员来操作")
		return
	}

	var targetName string
	var targetIsAdmin int64
	if err := s.db.sql.QueryRow(`SELECT name, is_admin FROM users WHERE id=?`, targetID).
		Scan(&targetName, &targetIsAdmin); err != nil {
		if err == sql.ErrNoRows {
			apiErr(w, 404, "没有这个账号")
			return
		}
		apiErr(w, 500, err.Error())
		return
	}

	// 确认串：**刻意不做 trim**（"我确实输了名字"和"我随手打了个回车"要能分开）
	if req.Confirm == "" || req.Confirm != targetName {
		apiErr(w, 400, "确认失败：请原样输入要删除的账号名「"+targetName+"」")
		return
	}

	now := time.Now().Unix()
	if targetIsAdmin != 0 {
		// 判据与封禁同一句：**还能用**的管理员（到期的封禁不算数）。
		var n int
		if e := s.db.sql.QueryRow(`SELECT COUNT(*) FROM users
			WHERE is_admin<>0 AND (banned=0 OR (ban_until>0 AND ban_until<=?))`, now).Scan(&n); e != nil {
			apiErr(w, 500, e.Error())
			return
		}
		if n <= 1 {
			apiErr(w, 400, "这是最后一个能用的管理员，删掉就没人能进管理台了")
			return
		}
	}

	// 他名下的共享数据：**先拒绝**，别替他做决定（会连带删掉别人的日程）
	if blockers := s.ownedDataBlockers(targetID); len(blockers) > 0 {
		apiErr(w, 409, "先处理这些再删："+strings.Join(blockers, "；")+
			"。否则会连带删掉别人的日程，而那不可逆。")
		return
	}

	// ① 他创建的日历打墓碑 + 退出别人的日历（与"一键清空"共用一份实现）
	res, err := s.wipeUserDataInTx(targetID)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	// ② 账号行本身，以及"只属于他"的东西
	var removed int64
	if err := s.withTx(func(tx *sql.Tx) error {
		// 他一个人独占的团队：连团队一起清（上面已经保证团队里没有别人）
		for _, q := range []string{
			`DELETE FROM team_invites WHERE team_id IN (SELECT id FROM teams WHERE owner_id=?)`,
			`DELETE FROM team_members WHERE team_id IN (SELECT id FROM teams WHERE owner_id=?)`,
			`DELETE FROM calendar_teams WHERE team_id IN (SELECT id FROM teams WHERE owner_id=?)`,
			`DELETE FROM teams WHERE owner_id=?`,
			// 他在别人日历/团队里的身份
			`DELETE FROM calendar_members WHERE user_id=?`,
			`DELETE FROM team_members WHERE user_id=?`,
			// 凭证与设备
			`DELETE FROM tokens WHERE user_id=?`,
			`DELETE FROM api_keys WHERE user_id=?`,
			`DELETE FROM devices WHERE user_id=?`,
			// 个人化的提醒 / 收件箱
			`DELETE FROM snoozes WHERE user_id=?`,
			`DELETE FROM reminder_prefs WHERE user_id=?`,
			`DELETE FROM strong_reminders WHERE user_id=?`,
			`DELETE FROM inbox_items WHERE user_id=?`,
		} {
			if _, e := tx.Exec(q, targetID); e != nil {
				return e
			}
		}
		out, e := tx.Exec(`DELETE FROM users WHERE id=?`, targetID)
		if e != nil {
			return e
		}
		if n, e := out.RowsAffected(); e == nil {
			removed = n
		}
		// 让所有端重拉一次：成员表、日历可见性都变了
		return bumpGlobalRepairCursor(tx, time.Now().Unix())
	}); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if removed == 0 {
		// 事务里没删到行 = 有人在并发下先删掉了。上面查过存在，走到这里说明
		// 是竞态 —— 报出来，别假装成功。
		apiErr(w, 409, "这个账号刚刚已经不在了（可能被另一个管理员删掉了）")
		return
	}

	var meName string
	_ = s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, me).Scan(&meName)
	log.Printf("[admin] %s(%s) 删除了账号 %s(%s)：日历 %d 张、日程 %d 条、退出日历 %d 张",
		meName, me, targetName, targetID, res.Calendars, res.Events, res.Memberships)
	// user_id 传空：那一行已经没了（外键语义上不存在的人）。日志页按 user_name 显示，
	// 所以照样看得懂"谁在什么时候被删了"。
	s.logAccountEvent(r, "", targetName, logEventAccountDeleted, "success",
		fmt.Sprintf("由 %s 删除", meName))

	writeJSON(w, 200, map[string]any{
		"ok": true, "id": targetID, "name": targetName,
		"calendars": res.Calendars, "events": res.Events, "memberships": res.Memberships,
	})
}

// handleAdminDeleteReport 删掉一条举报记录。
// DELETE /api/admin/reports/{id}
//
// ⚠️ **只允许删已经处理过的**（accepted / rejected）。
// 待处理的必须走"受理 / 驳回" —— 允许直接删待处理的话，就等于给了一条
// "不看、不判、悄悄抹掉"的路，而举报机制的全部价值恰恰在于**有裁决、有留痕**。
//
// ★ 为什么该能删：举报单里会有测试单、误报单、以及"当事人私下和解了"的单子。
//
//	不能删的话它们永远躺在列表里，把真正要看的东西稀释掉 —— 这跟最初那条
//	"不能举报自己"的设计想防的是同一件事（无效条目稀释真信号）。
func (s *Server) handleAdminDeleteReport(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		apiErr(w, 400, "缺少举报单 id")
		return
	}

	var status, title, kind string
	if err := s.db.sql.QueryRow(`SELECT status, target_title, target_kind FROM reports WHERE id=?`, id).
		Scan(&status, &title, &kind); err != nil {
		if err == sql.ErrNoRows {
			apiErr(w, 404, "没有这条举报")
			return
		}
		apiErr(w, 500, err.Error())
		return
	}
	if status == "pending" {
		apiErr(w, 409, "这条举报还没处理。待处理的要「受理」或「驳回」—— 直接删掉等于不判不留痕。")
		return
	}

	if _, err := s.db.sql.Exec(`DELETE FROM reports WHERE id=?`, id); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	// 删除是不可逆的，必须留痕（写在 stdout，与其余管理动作一致 ——
	// 举报单本身已经被删掉，没有别的地方能记这件事了）。
	var meName string
	_ = s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, userIDOf(r)).Scan(&meName)
	log.Printf("[admin] %s 删掉了举报单 %s（%s / %s / %s）",
		meName, id, kind, status, title)

	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}

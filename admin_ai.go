package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ===========================================================================
// 管理员接口：AI 复核配置 / 登录日志 / 内容审核审计（2026-09-23）
// ===========================================================================
//
// 全部以 s.requireAdmin(w, r) 开头 —— 与 admin.go 的其余接口一致。
//
// ---------------------------------------------------------------------------
// ★ 本文件最要紧的一条规则：**密钥永不外泄**
// ---------------------------------------------------------------------------
//
// AI 配置里有一个 API Key。它可能出现在四个地方，每一处都要挡住：
//
//   ① 读接口的响应 —— 只回 aiMaskKey() 的结果（头 6 位 + 尾 4 位），
//      且明确回一个 has_key 布尔值，让前端知道"配过了"但拿不到原文。
//   ② 错误信息 —— 任何报错都不许带密钥，连长度都不带。
//   ③ 日志（含审计表）—— 见 moderation_ai.go 的注释。
//   ④ **写接口的响应** —— 有人会习惯性地把请求体回显，那是第四条泄漏路径。
//
// 还有一条容易漏的：**写接口收到空 key 时是"不改"，不是"清空"**。
// 前端只显示掩码，用户不动那个框就会提交空串；若当成清空，
// 每次改个模型都会顺手把密钥删掉 —— 而表现是"保存成功了但 AI 不工作"。

// ---------------------------------------------------------------------------
// AI 复核配置
// ---------------------------------------------------------------------------

// GET /api/admin/ai
func (s *Server) handleAdminGetAI(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	cfg := s.aiConfigFor()
	rawKey := strings.TrimSpace(s.setting(settingAIKey, ""))

	writeJSON(w, 200, map[string]any{
		"enabled":         cfg.Enabled,
		"base_url":        cfg.BaseURL,
		"model":           cfg.Model,
		"timeout_ms":      cfg.Timeout.Milliseconds(),
		"has_key":         rawKey != "",
		"key_masked":      aiMaskKey(rawKey), // ⚠️ 只有掩码，永远不回原文
		"default_base":    aiDefaultBase,
		"default_model":   aiDefaultModel,
		"max_timeout_ms":  aiMaxTimeout,
		"severity_labels": severityLabels,
	})
}

// PUT /api/admin/ai
//
// 请求体字段全部可选（patch 语义）：
//
//	{"enabled":true,"base_url":"...","model":"...","api_key":"sk-...","timeout_ms":8000}
//
// ⚠️ api_key 的三种取值，语义**必须**分清：
//   - 字段不传（nil）→ 不改
//   - 传空串 ""     → 不改（前端只显示掩码，用户没动那个框）
//   - 传 "__clear__" → 清空（需要明确的意图才能清）
//
// 为什么不把空串当清空：前端的密钥框是 password 类型且只回填掩码，
// 用户改模型时那个框是空的 —— 提交空串就等于"顺手删了密钥"。
// 这类 bug 的表现是"保存成功但没有 AI"，非常难查。
const aiKeyClearSentinel = "__clear__"

func (s *Server) handleAdminSetAI(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Enabled   *bool   `json:"enabled"`
		BaseURL   *string `json:"base_url"`
		Model     *string `json:"model"`
		APIKey    *string `json:"api_key"`
		TimeoutMS *int    `json:"timeout_ms"`
	}
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}

	// 先全部校验完再落库（与权限接口同一个原则：全有或全无）。
	// 逐个校验边写的话，中途失败会留下"改了一半"的配置 ——
	// 而配置改一半的表现往往比不改更糟（比如切了模型但没切地址）。
	if req.BaseURL != nil {
		b := strings.TrimRight(strings.TrimSpace(*req.BaseURL), "/")
		if !strings.HasPrefix(b, "http://") && !strings.HasPrefix(b, "https://") {
			apiErr(w, 400, "接口地址要以 http:// 或 https:// 开头")
			return
		}
		// 常见误填：把完整的 /chat/completions 抄进来。
		// 我们自己在后面拼这个路径，抄进来会变成 .../chat/completions/chat/completions。
		// 与其让用户对着 404 猜，不如直接说清。
		if strings.Contains(b, "/chat/completions") {
			apiErr(w, 400, "接口地址只要填到主域名（如 https://api.deepseek.com），不要带 /chat/completions")
			return
		}
	}
	if req.TimeoutMS != nil {
		if *req.TimeoutMS <= 0 || *req.TimeoutMS > aiMaxTimeout {
			apiErr(w, 400, "超时要在 1 到 "+strconv.Itoa(aiMaxTimeout)+" 毫秒之间")
			return
		}
	}

	if req.BaseURL != nil {
		if err := s.setSetting(settingAIBase, strings.TrimRight(strings.TrimSpace(*req.BaseURL), "/")); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	if req.Model != nil {
		m := strings.TrimSpace(*req.Model)
		if m == "" {
			m = aiDefaultModel
		}
		if err := s.setSetting(settingAIModel, m); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	if req.TimeoutMS != nil {
		if err := s.setSetting(settingAITimeout, strconv.Itoa(*req.TimeoutMS)); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	// 密钥：见上面那段三种取值的说明
	if req.APIKey != nil {
		k := strings.TrimSpace(*req.APIKey)
		switch {
		case k == aiKeyClearSentinel:
			if err := s.setSetting(settingAIKey, ""); err != nil {
				apiErr(w, 500, err.Error())
				return
			}
			// 清了密钥就不可能调通，顺手把开关关掉 ——
			// 否则界面显示"已启用"而实际每次都 fail-open，最难查的那种状态。
			if err := s.setSetting(settingAIEnabled, "0"); err != nil {
				apiErr(w, 500, err.Error())
				return
			}
		case k != "":
			if err := s.setSetting(settingAIKey, k); err != nil {
				apiErr(w, 500, err.Error())
				return
			}
		}
	}
	if req.Enabled != nil {
		v := "0"
		if *req.Enabled {
			v = "1"
		}
		if err := s.setSetting(settingAIEnabled, v); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}

	// ⚠️ 回显时**重新读一遍配置**（不是回显请求体）：这样既确认了落库结果，
	// 又天然不会把请求里的密钥原文带出去（配置读取走的是掩码那条路）。
	cfg := s.aiConfigFor()
	rawKey := strings.TrimSpace(s.setting(settingAIKey, ""))
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"enabled":    cfg.Enabled,
		"base_url":   cfg.BaseURL,
		"model":      cfg.Model,
		"timeout_ms": cfg.Timeout.Milliseconds(),
		"has_key":    rawKey != "",
		"key_masked": aiMaskKey(rawKey),
	})
}

// POST /api/admin/ai/test —— 用当前配置真发一次请求，把结果原样告诉管理员。
//
// 为什么要有"试连接"：AI 配置错的表现是**静默 fail-open**（内容照常存入，
// 只是没有复核）。管理员没有任何办法知道"我配的到底通没通"。
// 这个接口让这件事变成一次点击就能确认的。
//
// ⚠️ 送审文本是**固定的测试串**，不含任何用户内容，也不会写审计表 ——
// 试连接不是一次真实审核，写进去只会污染审计数据。
func (s *Server) handleAdminTestAI(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	cfg := s.aiConfigFor()
	if !cfg.Enabled {
		writeJSON(w, 200, map[string]any{
			"ok":      false,
			"message": "还没启用（没填密钥，或开关是关的）",
		})
		return
	}

	// 故意用一个**明确违规**的串：如果模型连这个都判合规，说明它没在正常工作
	// （或者接口地址被指向了别的东西）—— 比送一个干净串更有判别力。
	start := time.Now()
	v := s.aiReviewText(r.Context(), "测试：出售摇头丸和冰毒，量大从优", nil)
	elapsed := time.Since(start).Milliseconds()

	if v.Source != "model" {
		writeJSON(w, 200, map[string]any{
			"ok":         false,
			"message":    v.ErrorText,
			"elapsed_ms": elapsed,
			"model":      cfg.Model,
		})
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"message":    "连接正常，模型有响应",
		"elapsed_ms": elapsed,
		"model":      cfg.Model,
		// 顺带把这次"试探"的结论回给管理员：判违规 = 确实在工作
		"sample_violation": v.Violation,
		"sample_reason":    v.Reason,
	})
}

// ---------------------------------------------------------------------------
// 登录日志
// ---------------------------------------------------------------------------

// GET /api/admin/login-logs?limit=&offset=&user=&event=&result=&days=
func (s *Server) handleAdminLoginLogs(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 100)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := atoiDefault(q.Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}

	where := []string{}
	args := []any{}

	if v := strings.TrimSpace(q.Get("user")); v != "" {
		// 按名字**模糊**匹配，且同时匹配 user_id —— 管理员手上可能只有名字，
		// 也可能有 id（从别处复制来的）。两种都给匹配上最省事。
		where = append(where, `(user_name LIKE ? OR user_id = ?)`)
		args = append(args, "%"+v+"%", v)
	}
	if v := strings.TrimSpace(q.Get("event")); v != "" && v != "all" {
		// ⚠️ 只接受已知事件类型。不校验的话，前端拼错一个值会得到
		// "空结果"而不是"筛错了"，看上去像"这段时间没有登录"。
		if _, ok := loginEventLabels[v]; ok {
			where = append(where, `event = ?`)
			args = append(args, v)
		}
	}
	if v := strings.TrimSpace(q.Get("result")); v != "" && v != "all" {
		if v == "success" || v == "fail" {
			where = append(where, `result = ?`)
			args = append(args, v)
		}
	}
	if days := atoiDefault(q.Get("days"), 0); days > 0 {
		where = append(where, `created_at >= ?`)
		args = append(args, time.Now().AddDate(0, 0, -days).Unix())
	}

	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	// 总数与页面分开查：前端要显示"共 N 条 / 第 x 页"，
	// 而 LIMIT 之后的长度不是总数。
	var total int
	if err := s.db.sql.QueryRow(`SELECT COUNT(*) FROM login_logs`+clause, args...).Scan(&total); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	rows, err := s.db.sql.Query(
		`SELECT id, user_id, user_name, event, result, reason, ip, real_ip, ua, device, created_at
		   FROM login_logs`+clause+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer rows.Close()

	out := []LoginLog{}
	for rows.Next() {
		var l LoginLog
		if err := rows.Scan(&l.ID, &l.UserID, &l.UserName, &l.Event, &l.Result,
			&l.Reason, &l.IP, &l.RealIP, &l.UA, &l.Device, &l.CreatedAt); err != nil {
			continue
		}
		l.EventText = firstNonEmpty(loginEventLabels[l.Event], l.Event)
		out = append(out, l)
	}

	// 顺带回一个"每个账号最后一次成功登录"，这是这个页面最常被看的一栏
	// （"谁最近登过 / 谁的号多久没动了"）。单独查而不是让前端自己算：
	// 前端只有当前这一页的数据，算出来的"最后登录"是错的。
	type lastLogin struct {
		UserName  string `json:"user_name"`
		UserID    string `json:"user_id"`
		IP        string `json:"ip"`
		RealIP    string `json:"real_ip"`
		CreatedAt int64  `json:"created_at"`
		Days      int    `json:"days_ago"`
	}
	last := []lastLogin{}
	if lrows, err := s.db.sql.Query(`
		SELECT user_id, user_name, ip, real_ip, MAX(created_at) AS t
		  FROM login_logs WHERE event='login' AND result='success' AND user_id<>''
		 GROUP BY user_id ORDER BY t DESC`); err == nil {
		defer lrows.Close()
		now := time.Now().Unix()
		for lrows.Next() {
			var l lastLogin
			if err := lrows.Scan(&l.UserID, &l.UserName, &l.IP, &l.RealIP, &l.CreatedAt); err == nil {
				l.Days = int((now - l.CreatedAt) / 86400)
				last = append(last, l)
			}
		}
	}

	writeJSON(w, 200, map[string]any{
		"logs":       out,
		"total":      total,
		"limit":      limit,
		"offset":     offset,
		"events":     loginEventLabels,
		"last_login": last,
		"keep_days":  atoiDefault(s.setting("login_log_keep_days", ""), 180),
	})
}

// ---------------------------------------------------------------------------
// 内容审核审计（AI 判断记录）
// ---------------------------------------------------------------------------

// GET /api/admin/moderation-audit?limit=&offset=&violation=&source=
func (s *Server) handleAdminModerationAudit(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 100)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := atoiDefault(q.Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}

	where := []string{}
	args := []any{}
	if v := strings.TrimSpace(q.Get("violation")); v == "1" || v == "0" {
		where = append(where, `violation = ?`)
		args = append(args, v)
	}
	if v := strings.TrimSpace(q.Get("source")); v == "model" || v == "error" || v == "disabled" {
		where = append(where, `source = ?`)
		args = append(args, v)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.sql.QueryRow(`SELECT COUNT(*) FROM moderation_audit`+clause, args...).Scan(&total); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	rows, err := s.db.sql.Query(
		`SELECT id, field, excerpt, hits, violation, category, confidence, reason,
		        source, error_text, model, elapsed_ms, user_id, user_name, created_at
		   FROM moderation_audit`+clause+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer rows.Close()

	out := []moderationAuditRow{}
	for rows.Next() {
		var a moderationAuditRow
		var viol int
		var hits string
		if err := rows.Scan(&a.ID, &a.Field, &a.Excerpt, &hits, &viol, &a.Category,
			&a.Confidence, &a.Reason, &a.Source, &a.ErrorText, &a.Model, &a.Elapsed,
			&a.UserID, &a.UserName, &a.CreatedAt); err != nil {
			continue
		}
		a.Violation = viol != 0
		if hits != "" {
			a.Hits = strings.Split(hits, "|")
		} else {
			a.Hits = []string{}
		}
		out = append(out, a)
	}

	// 汇总几个数，让管理员一眼看出"AI 到底在不在工作"。
	// ⚠️ 这比"看列表"重要得多：一个配置错误会让 source='error' 占满全部记录，
	// 而列表很长时，人是不会去数 error 占多少条的。
	var vCount, eCount, dCount int
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM moderation_audit WHERE violation=1`).Scan(&vCount)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM moderation_audit WHERE source='error'`).Scan(&eCount)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM moderation_audit WHERE source='disabled'`).Scan(&dCount)

	cfg := s.aiConfigFor()
	rawKey := strings.TrimSpace(s.setting(settingAIKey, ""))

	writeJSON(w, 200, map[string]any{
		"audits":         out,
		"total":          total,
		"limit":          limit,
		"offset":         offset,
		"stat_violation": vCount,
		"stat_error":     eCount,
		"stat_disabled":  dCount,
		"ai": map[string]any{
			"enabled":    cfg.Enabled,
			"model":      cfg.Model,
			"has_key":    rawKey != "",
			"key_masked": aiMaskKey(rawKey),
		},
	})
}

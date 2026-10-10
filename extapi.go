package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Open API for machines.
//
// The point of this file is to let an AI (or a script, or a shortcut on a
// phone) read and edit the schedule without a browser session. Two decisions
// keep it small:
//
//   1. API keys resolve to the very same user id that a session token does, so
//      every existing handler is reused verbatim. There is no parallel
//      "external" implementation that can drift out of sync.
//   2. Request bodies accept human time strings (`"2026-09-15 14:30"`) as well
//      as raw epoch seconds, and `date=YYYY-MM-DD` in queries. A language model
//      writing "next Tuesday at 3pm" should not have to do epoch arithmetic,
//      and neither should a human with curl.
// ---------------------------------------------------------------------------

const apiKeyPrefix = "fsk_"

func hashAPIKey(k string) string {
	sum := sha256.Sum256([]byte(k))
	return hex.EncodeToString(sum[:])
}

func newAPIKey() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is not something we can paper over
		panic("crypto/rand: " + err.Error())
	}
	return apiKeyPrefix + hex.EncodeToString(b)
}

// userForKey resolves an API key to its owner, refreshing last_used_at.
func (s *Server) userForKey(key string) string {
	h := hashAPIKey(key)
	var id, uid string
	err := s.db.sql.QueryRow(
		`SELECT id, user_id FROM api_keys WHERE key_hash=? AND revoked_at IS NULL`, h).Scan(&id, &uid)
	if err != nil {
		return ""
	}
	_, _ = s.db.sql.Exec(`UPDATE api_keys SET last_used_at=? WHERE id=?`, time.Now().Unix(), id)
	return uid
}

// authOrKey accepts either a session token or an API key. Both end up as the
// same context value, which is why the rest of the server does not care.
func (s *Server) authOrKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if tok == "" {
			tok = strings.TrimSpace(r.Header.Get("X-API-Key"))
		}
		if tok == "" {
			tok = strings.TrimSpace(r.URL.Query().Get("key"))
		}
		if tok == "" {
			apiErr(w, http.StatusUnauthorized, "缺少凭证：请带 Authorization: Bearer <token 或 API Key>")
			return
		}
		if strings.HasPrefix(tok, apiKeyPrefix) {
			uid := s.userForKey(tok)
			if uid == "" {
				apiErr(w, http.StatusUnauthorized, "API Key 无效或已被撤销")
				return
			}
			// ⚠️ 外置接口这条路也要判封禁：否则一个被封的账号只要握着 API Key，
			//    照样能读写日历 —— 而这条路的用户（第三方 AI）恰恰最不容易被发现。
			if s.banGuard(w, uid) {
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), ctxUserID, uid)))
			return
		}
		var uid string
		if err := s.db.sql.QueryRow(`SELECT user_id FROM tokens WHERE token=?`, tok).Scan(&uid); err != nil {
			apiErr(w, http.StatusUnauthorized, "凭证无效")
			return
		}
		if s.banGuard(w, uid) {
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUserID, uid)))
	}
}

// ---------- key management (session-authenticated) ----------

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	rows, err := s.db.sql.Query(
		`SELECT id, name, prefix, created_at, last_used_at FROM api_keys
		 WHERE user_id=? AND revoked_at IS NULL ORDER BY created_at DESC`, uid)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, name, prefix string
		var created, used int64
		if err := rows.Scan(&id, &name, &prefix, &created, &used); err == nil {
			out = append(out, map[string]any{
				"id": id, "name": name, "prefix": prefix,
				"created_at": created, "last_used_at": used,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"keys": out})
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var body struct {
		Name string `json:"name"`
	}
	_ = readJSON(r, &body)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "未命名"
	}
	key := newAPIKey()
	id := newID("ak_")
	now := time.Now().Unix()
	_, err := s.db.sql.Exec(
		`INSERT INTO api_keys(id,user_id,name,key_hash,prefix,created_at,last_used_at,revoked_at)
		 VALUES(?,?,?,?,?,?,0,NULL)`,
		id, uid, name, hashAPIKey(key), key[:12], now)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	// The plaintext key is returned exactly once; only its hash is stored.
	writeJSON(w, 200, map[string]any{
		"id": id, "name": name, "key": key, "created_at": now,
		"note": "这是唯一一次显示完整 Key，请立刻保存。服务器只存它的哈希值，丢了只能重新生成。",
	})
}

func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	res, err := s.db.sql.Exec(
		`UPDATE api_keys SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL`,
		time.Now().Unix(), id, uid)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		apiErr(w, 404, "没有这个 Key")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- loose time parsing ----------

// parseLooseTime accepts what a person or a model would actually write.
func parseLooseTime(sv string) (int64, bool) {
	sv = strings.TrimSpace(sv)
	if sv == "" {
		return 0, false
	}
	loc := weatherZone()
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006/01/02 15:04:05",
		"2006/01/02 15:04",
		"2006-01-02 15",
		"2006-01-02",
		"2006/01/02",
	} {
		if t, err := time.ParseInLocation(layout, sv, loc); err == nil {
			return t.Unix(), true
		}
	}
	if n, err := strconv.ParseInt(sv, 10, 64); err == nil {
		return n, true
	}
	return 0, false
}

// looseTimeValue accepts a time as either a human-readable string or an epoch
// number. The JSON layer hands us float64, but middleware that rewrites the
// body in flight inserts int64 — both have to work.
func looseTimeValue(v any) (int64, bool) {
	if s, ok := v.(string); ok {
		return parseLooseTime(s)
	}
	return jsonMap{"t": v}.int64Of("t")
}

func parseLooseDate(sv string) (time.Time, bool) {
	loc := weatherZone()
	sv = strings.TrimSpace(sv)
	if sv == "" {
		return time.Time{}, false
	}
	if sv == "today" || sv == "今天" {
		n := time.Now().In(loc)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc), true
	}
	if sv == "tomorrow" || sv == "明天" {
		n := time.Now().In(loc).AddDate(0, 0, 1)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc), true
	}
	for _, layout := range []string{"2006-01-02", "2006/01/02", "20060102"} {
		if t, err := time.ParseInLocation(layout, sv, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// normalizeExtBody rewrites a machine-written event body into the shape the
// internal handlers expect: an id if none was given, epoch seconds instead of
// ISO strings, and a duration instead of an end time.
func (s *Server) normalizeExtBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			next(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			apiErr(w, 400, "读取请求体失败")
			return
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			// Not an object; let the handler produce its own error.
			r.Body = io.NopCloser(bytes.NewReader(raw))
			next(w, r)
			return
		}
		changed := false

		if s, _ := m["id"].(string); s == "" {
			if r.Method == http.MethodPost {
				m["id"] = newID("e_")
				changed = true
			}
		}

		// "start" / "end" are conveniences; dtstart_utc / duration_sec win.
		if _, ok := m["dtstart_utc"]; !ok {
			if sv, ok := m["start"].(string); ok {
				if ts, ok := parseLooseTime(sv); ok {
					m["dtstart_utc"] = ts
					changed = true
				} else {
					apiErr(w, 400, "看不懂的开始时间："+sv+"（示例：2026-09-15 14:30）")
					return
				}
			}
		}
		if _, ok := m["duration_sec"]; !ok {
			if endRaw, present := m["end"]; present && endRaw != nil {
				end, ok := looseTimeValue(endRaw)
				if !ok {
					apiErr(w, 400, "看不懂的结束时间（示例：\"2026-09-15 15:30\" 或 epoch 秒）")
					return
				}
				start, ok := jsonMap(m).int64Of("dtstart_utc")
				if !ok {
					// Deriving the duration needs an anchor. Failing loudly beats
					// the alternative: storing a zero-length event and losing the
					// end time without telling anyone.
					apiErr(w, 400, "给了 end 但没有可用的 start：请同时给出 start，或改用 duration_minutes")
					return
				}
				d := end - start
				if d <= 0 {
					apiErr(w, 400, "结束时间不晚于开始时间")
					return
				}
				m["duration_sec"] = d
				changed = true
			} else if mins, ok := jsonMap(m).int64Of("duration_minutes"); ok && mins > 0 {
				m["duration_sec"] = mins * 60
				changed = true
			}
		}

		// Relative reminders are much easier to write than absolute ones.
		if _, ok := m["reminders"]; !ok {
			if am, ok := m["remind_minutes_before"]; ok {
				switch v := am.(type) {
				case float64:
					m["reminders"] = []any{map[string]any{"minutes_before": int64(v)}}
					changed = true
				case []any:
					list := []any{}
					for _, x := range v {
						if f, ok := x.(float64); ok {
							list = append(list, map[string]any{"minutes_before": int64(f)})
						}
					}
					if len(list) > 0 {
						m["reminders"] = list
						changed = true
					}
				}
			}
		}

		if !changed {
			r.Body = io.NopCloser(bytes.NewReader(raw))
			next(w, r)
			return
		}
		out, err := json.Marshal(m)
		if err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(out))
		r.ContentLength = int64(len(out))
		next(w, r)
	}
}

// normalizeExtCalendarBody 把外置 API 里的"人话"日历入参翻成服务端字段。
//
// 为什么需要（2026-09-25）：外置 API 在这之前**只能读日历**，日程却能增删改 ——
// 第三方 AI 想"把课程表改名"或者"新建一个课程表日历"就完全做不到。
// 补上写操作之后，顺带把入参也做成**和自带智能体同一套说法**：
//
//	course: true            → kind: "course"（当课程表用）
//	shared: true            → kind: "shared"
//	members: ["小明","小红"]  → member_ids（按名字查 id）
//	member_ids: ["u_xx"]    → 原样透传
//	name / color            → 原样透传
//
// ⚠️ **不猜**：名字查不到、或者重名，都**明确报错**并把现有的人列出来 ——
// 静默忽略一个名字比报错危险得多（用户以为共享了，其实没有）。
// ⚠️ 特意**不套** normalizeExtBody：那个会给 body 塞一个 `e_` 前缀的 id，
// 对日历是脏数据（日历 id 由服务端 `newID("c_")` 生成）。
func (s *Server) normalizeExtCalendarBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			next(w, r)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			apiErr(w, 400, "读取请求体失败")
			return
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			// 不是对象就让 handler 自己去报错（保持和 normalizeExtBody 一致的行为）
			r.Body = io.NopCloser(bytes.NewReader(raw))
			next(w, r)
			return
		}

		// name / color 必须是字符串；给别的类型直接拒绝，别让 json 解出个数字当名字
		for _, k := range []string{"name", "color", "kind"} {
			if v, ok := m[k]; ok && v != nil {
				if _, isStr := v.(string); !isStr {
					apiErr(w, 400, k+" 必须是字符串")
					return
				}
			}
		}

		// course / shared 两个布尔开关 → kind（kind 是一个字段、三个取值互斥）
		if b, ok := m["course"].(bool); ok {
			delete(m, "course")
			if b {
				m["kind"] = "course"
			} else if _, has := m["kind"]; !has {
				// 取消勾选：kind 只能退回 private（没有"只去掉 course 那一位"的操作）
				m["kind"] = "private"
			}
		} else if _, has := m["course"]; has {
			apiErr(w, 400, "course 必须是 true / false")
			return
		}
		if b, ok := m["shared"].(bool); ok {
			delete(m, "shared")
			if b {
				m["kind"] = "shared"
			}
		} else if _, has := m["shared"]; has {
			apiErr(w, 400, "shared 必须是 true / false")
			return
		}

		// ⚠️ 先把**客户端给的** member_ids 校验掉，再做 members → member_ids 的转换。
		//    反过来的话会把自己刚生成的值再校验一遍 ——
		//    `members` 转出来的是 Go 的 `[]string`，而 JSON 解出来的数组是 `[]any`，
		//    于是 `v.([]any)` 判定失败，自己把自己 400 掉（第一版就是这么错的）。
		if v, ok := m["member_ids"]; ok && v != nil {
			if _, isArr := v.([]any); !isArr {
				apiErr(w, 400, "member_ids 必须是数组")
				return
			}
		}

		// members（名字数组）→ member_ids
		if v, ok := m["members"]; ok && v != nil {
			arr, isArr := v.([]any)
			if !isArr {
				apiErr(w, 400, "members 必须是数组（账号名字）")
				return
			}
			lookup, err := s.usersByNames(arr)
			if err != nil {
				apiErr(w, 400, err.Error())
				return
			}
			m["member_ids"] = lookup
			delete(m, "members")
		}

		out, err := json.Marshal(m)
		if err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(out))
		r.ContentLength = int64(len(out))
		next(w, r)
	}
}

// usersByNames 把账号名字数组翻成 id 数组。
//
// 查不到或重名都**直接报错**并把现有的账号列出来 —— 让模型/脚本能自己纠正，
// 而不是把一条查不到的名字悄悄丢掉（那会变成"用户以为共享了，其实没有"）。
func (s *Server) usersByNames(names []any) ([]string, error) {
	rows, err := s.db.sql.Query(`SELECT id, name FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string][]string{}
	all := []string{}
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) != nil {
			continue
		}
		byName[name] = append(byName[name], id)
		all = append(all, name)
	}
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range names {
		// 只认字符串。给数字/对象就当空名字跳过（不猜它想指谁）——
		// 真要传 id 请用 member_ids。
		q, isStr := raw.(string)
		if !isStr {
			continue
		}
		q = trimSpace(q)
		if q == "" {
			continue
		}
		hits := byName[q]
		if len(hits) == 0 {
			return nil, fmt.Errorf("找不到账号「%s」。现在有：%s", q, strings.Join(all, "、"))
		}
		if len(hits) > 1 {
			return nil, fmt.Errorf("有 %d 个账号都叫「%s」，没法确定是哪个 —— 请改用 member_ids", len(hits), q)
		}
		if !seen[hits[0]] {
			seen[hits[0]] = true
			out = append(out, hits[0])
		}
	}
	return out, nil
}

// normalizeExtQuery turns `date=` / `days=` into the from/to the instances
// handler wants, so callers never have to compute epoch seconds.
func normalizeExtQuery(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("from") == "" || q.Get("to") == "" {
			loc := weatherZone()
			start, ok := parseLooseDate(q.Get("date"))
			if !ok {
				n := time.Now().In(loc)
				start = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
			}
			days, _ := strconv.Atoi(q.Get("days"))
			if days <= 0 {
				days = 1
			}
			if days > 400 {
				days = 400
			}
			q.Set("from", strconv.FormatInt(start.Unix(), 10))
			q.Set("to", strconv.FormatInt(start.AddDate(0, 0, days).Unix()-1, 10))
			r.URL.RawQuery = q.Encode()
		}
		next(w, r)
	}
}

// handleExtWhoami tells a caller what it is allowed to do.
func (s *Server) handleExtWhoami(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var name string
	var admin int64
	_ = s.db.sql.QueryRow(`SELECT name, is_admin FROM users WHERE id=?`, uid).Scan(&name, &admin)
	cals, _ := s.calendarsFor(uid)
	type calBrief struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Color    string `json:"color"`
		CanWrite bool   `json:"can_write"`
	}
	briefs := []calBrief{}
	for _, c := range cals {
		briefs = append(briefs, calBrief{c.ID, c.Name, c.Kind, c.Color, s.canWrite(c.ID, uid)})
	}
	writeJSON(w, 200, map[string]any{
		"user":      map[string]any{"id": uid, "name": name, "is_admin": admin != 0},
		"calendars": briefs,
		"timezone":  "Asia/Shanghai",
		"now":       time.Now().In(weatherZone()).Format("2006-01-02 15:04:05"),
		"now_utc":   time.Now().Unix(),
		"hint":      "创建日程时 calendar_id 必填，可从上面的 calendars 里挑一个 can_write=true 的。",
	})
}

// handleExtAgenda returns one day as a compact, human-readable block. This is
// the call an assistant should make first: it answers "what is on today"
// including the weather, in a single round trip.
func (s *Server) handleExtAgenda(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	loc := weatherZone()
	day, ok := parseLooseDate(r.URL.Query().Get("date"))
	if !ok {
		n := time.Now().In(loc)
		day = time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 1
	}
	from := day.Unix()
	to := day.AddDate(0, 0, days).Unix() - 1

	rows, err := s.db.sql.Query(`
		SELECT i.id FROM instances i
		JOIN calendars c ON c.id = i.calendar_id
		WHERE i.deleted_at IS NULL AND i.start_utc >= ? AND i.start_utc <= ?
		  AND c.deleted_at IS NULL
		  AND (c.owner_id = ? OR EXISTS(SELECT 1 FROM calendar_members m WHERE m.calendar_id=c.id AND m.user_id=?))
		ORDER BY i.start_utc`, from, to, uid, uid)
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

	type item struct {
		Date     string `json:"date"`
		Time     string `json:"time"`
		Title    string `json:"title"`
		Calendar string `json:"calendar"`
		Color    string `json:"color"`
		Location string `json:"location,omitempty"`
		Notes    string `json:"notes,omitempty"`
		AllDay   bool   `json:"all_day"`
		IsTask   bool   `json:"is_task"`
		Done     bool   `json:"done"`
		EventID  string `json:"event_id"`
	}
	items := []item{}
	var text strings.Builder
	for _, id := range ids {
		inst, ok := s.instanceByID(id)
		if !ok {
			continue
		}
		calName, calColor := "", ""
		if c, ok := s.calendarByID(inst.CalendarID, uid); ok {
			calName, calColor = c.Name, c.Color
		}
		st := time.Unix(inst.StartUTC, 0).In(loc)
		en := time.Unix(inst.EndUTC, 0).In(loc)
		timestr := st.Format("15:04") + "-" + en.Format("15:04")
		if inst.AllDay {
			timestr = "全天"
		}
		it := item{
			Date: st.Format("2006-01-02"), Time: timestr, Title: inst.Title,
			Calendar: calName, Color: calColor, Location: inst.Location,
			Notes: inst.Notes, AllDay: inst.AllDay, IsTask: inst.IsTask,
			Done: inst.DoneAt != nil, EventID: inst.EventID,
		}
		items = append(items, it)
		line := fmt.Sprintf("%s %s %s", st.Format("01-02"), timestr, inst.Title)
		if calName != "" {
			line += "  [" + calName + "]"
		}
		if inst.Location != "" {
			line += "  @" + inst.Location
		}
		if it.Done {
			line += "  (已完成)"
		}
		text.WriteString(line + "\n")
	}

	wx := s.Weather(24, 3)
	type dayBrief struct {
		Date  string  `json:"date"`
		Label string  `json:"label"`
		TMin  float64 `json:"tmin"`
		TMax  float64 `json:"tmax"`
		Rain  float64 `json:"precip_sum"`
	}
	wxDays := []dayBrief{}
	for _, d := range wx.Days {
		wxDays = append(wxDays, dayBrief{d.Date, d.Label, d.TMin, d.TMax, d.PrecipSum})
	}

	weekday := []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}[day.Weekday()]
	header := fmt.Sprintf("%s %s，共 %d 项\n", day.Format("2006-01-02"), weekday, len(items))
	body := header + text.String()
	if len(wx.Days) > 0 {
		d := wx.Days[0]
		body += fmt.Sprintf("\n天气：%s %.1f~%.1f°C 降水 %.1fmm（%s）\n",
			d.Label, d.TMin, d.TMax, d.PrecipSum, wx.Source)
	}

	writeJSON(w, 200, map[string]any{
		"date":    day.Format("2006-01-02"),
		"weekday": weekday,
		"count":   len(items),
		"items":   items,
		"weather": map[string]any{"source": wx.Source, "days": wxDays, "hours": wx.Hours},
		"text":    body,
	})
}

// ---------- documentation ----------

func (s *Server) handleExtOpenAPI(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, openAPISpec())
}

func (s *Server) handleExtGuide(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = io.WriteString(w, guideMarkdown)
}

func openAPISpec() map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "3e 日程服务 · 机器接口",
			"version":     version,
			"description": "供 AI / 脚本读写日程。鉴权用 Authorization: Bearer <API Key>。时间字段既接受 epoch 秒，也接受 \"2026-09-15 14:30\" 这种字符串。",
		},
		"servers": []map[string]any{{"url": "/api/ext"}},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"},
			},
			"schemas": map[string]any{
				"Event": obj(map[string]any{
					"id":                    str("事件 id，创建时可不填，服务器会生成"),
					"calendar_id":           str("必填，目标日历 id"),
					"title":                 str("标题"),
					"notes":                 str("备注"),
					"location":              str("地点"),
					"all_day":               map[string]any{"type": "boolean"},
					"dtstart_utc":           num("开始时间（epoch 秒）"),
					"start":                 str("开始时间字符串，如 2026-09-15 14:30"),
					"end":                   str("结束时间字符串，和 start 配对使用"),
					"duration_sec":          num("时长（秒）"),
					"duration_minutes":      num("时长（分钟）"),
					"rrule":                 str("RFC5545 循环规则，如 FREQ=WEEKLY;BYDAY=MO,WE;COUNT=8"),
					"is_task":               map[string]any{"type": "boolean", "description": "作为待办（可勾选完成）"},
					"done_at":               num("完成时间戳"),
					"reminders":             map[string]any{"type": "array", "items": obj(map[string]any{"minutes_before": num("提前多少分钟")})},
					"remind_minutes_before": map[string]any{"description": "reminders 的简写，可以是数字或数字数组"},
				}, "calendar_id"),
				// ★ 2026-09-25 加：日历本身也能写（以前只有读）。
				//   `course` / `shared` 是两个**布尔简写**，服务端会翻成 `kind` ——
				//   和自带智能体用同一套说法（`kind` 一个字段三取值互斥，很容易写错）。
				"Calendar": obj(map[string]any{
					"name":       str("日历名称"),
					"color":      str("颜色 #RRGGBB"),
					"kind":       str("private（只有自己可见）/ shared（共享）/ course（当课程表用，会出现在「课表」页）"),
					"course":     map[string]any{"type": "boolean", "description": "kind=course 的简写"},
					"shared":     map[string]any{"type": "boolean", "description": "kind=shared 的简写"},
					"members":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "要一起共享的账号名字（查不到或重名会**明确报错**，不静默忽略）"},
					"member_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "直接给账号 id（从 /users 拿）"},
				}, "name"),
			},
		},
		"security": []map[string]any{{"bearerAuth": []string{}}},
		"paths": map[string]any{
			"/whoami": map[string]any{"get": map[string]any{
				"summary": "确认身份与可写日历", "operationId": "whoami",
				"responses": map[string]any{"200": map[string]any{"description": "OK"}}}},
			"/calendars": map[string]any{
				"get": map[string]any{
					"summary": "列出所有日历（看 can_write 判断哪些能改）", "operationId": "listCalendars",
					"responses": map[string]any{"200": map[string]any{"description": "OK"}}},
				"post": map[string]any{
					"summary": "新建日历", "operationId": "createCalendar",
					"requestBody": map[string]any{"required": true, "content": map[string]any{
						"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Calendar"}}}},
					"responses": map[string]any{"200": map[string]any{"description": "OK"}}}},
			"/calendars/{id}": map[string]any{
				"put": map[string]any{
					"summary": "改日历（改名 / 改色 / 当课程表用；只传要改的字段）", "operationId": "updateCalendar",
					"parameters": []map[string]any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}},
					"requestBody": map[string]any{"required": true, "content": map[string]any{
						"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Calendar"}}}},
					"responses": map[string]any{"200": map[string]any{"description": "OK"}}},
				"delete": map[string]any{
					"summary": "删除日历（只有创建者能删）", "operationId": "deleteCalendar",
					"parameters": []map[string]any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}},
					"responses":  map[string]any{"200": map[string]any{"description": "OK"}}}},
			"/users": map[string]any{"get": map[string]any{
				"summary": "列出账号（建/分享日历时用来拿 member_ids）", "operationId": "listUsers",
				"responses": map[string]any{"200": map[string]any{"description": "OK"}}}},
			"/agenda": map[string]any{"get": map[string]any{
				"summary": "某天的日程 + 天气（推荐先调这个）", "operationId": "agenda",
				"parameters": []map[string]any{
					map[string]any{"name": "date", "in": "query", "schema": map[string]any{"type": "string"}, "description": "YYYY-MM-DD、today、tomorrow，默认今天"},
					map[string]any{"name": "days", "in": "query", "schema": map[string]any{"type": "integer"}, "description": "往后几天，默认 1"},
				},
				"responses": map[string]any{"200": map[string]any{"description": "OK"}}}},
			"/events": map[string]any{
				"get": map[string]any{
					"summary": "按时间区间列出日程（展开循环后的实例）", "operationId": "listEvents",
					"parameters": []map[string]any{
						map[string]any{"name": "date", "in": "query", "schema": map[string]any{"type": "string"}},
						map[string]any{"name": "days", "in": "query", "schema": map[string]any{"type": "integer"}},
						map[string]any{"name": "from", "in": "query", "schema": map[string]any{"type": "integer"}, "description": "epoch 秒，与 to 配对时可代替 date/days"},
						map[string]any{"name": "to", "in": "query", "schema": map[string]any{"type": "integer"}},
					},
					"responses": map[string]any{"200": map[string]any{"description": "OK"}}},
				"post": map[string]any{
					"summary": "新建日程", "operationId": "createEvent",
					"requestBody": map[string]any{"required": true, "content": map[string]any{
						"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Event"}}}},
					"responses": map[string]any{"200": map[string]any{"description": "OK"}}}},
			"/events/{id}": map[string]any{
				"put": map[string]any{
					"summary": "修改日程（只传要改的字段）", "operationId": "updateEvent",
					"parameters": []map[string]any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}},
					"responses":  map[string]any{"200": map[string]any{"description": "OK"}}},
				"delete": map[string]any{
					"summary": "删除日程（整个系列）", "operationId": "deleteEvent",
					"parameters": []map[string]any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}},
					"responses":  map[string]any{"200": map[string]any{"description": "OK"}}}},
			"/events/{id}/occurrence": map[string]any{
				"put": map[string]any{
					"summary": "只改循环日程的某一次", "operationId": "updateOccurrence",
					"parameters": []map[string]any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}},
					"responses":  map[string]any{"200": map[string]any{"description": "OK"}}},
				"delete": map[string]any{
					"summary": "只删除循环日程的某一次", "operationId": "deleteOccurrence",
					"parameters": []map[string]any{
						map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}},
						map[string]any{"name": "occurrence_key", "in": "query", "schema": map[string]any{"type": "integer"}, "description": "该次开始的 epoch 秒"},
					},
					"responses": map[string]any{"200": map[string]any{"description": "OK"}}}},
			"/weather": map[string]any{"get": map[string]any{
				"summary": "逐小时天气预报（地点由 -place / -lat / -lon 配置）", "operationId": "weather",
				"parameters": []map[string]any{
					map[string]any{"name": "hours", "in": "query", "schema": map[string]any{"type": "integer"}},
					map[string]any{"name": "days", "in": "query", "schema": map[string]any{"type": "integer"}},
				},
				"responses": map[string]any{"200": map[string]any{"description": "OK"}}}},
		},
	}
}

var guideMarkdown = `# 家庭日程 · 机器接口

给 AI 或脚本用的日程读写接口。人用的界面是另外两套客户端，这里是纯净的 HTTP。

## 鉴权

在客户端「设置 → 机器接口」里生成一个 API Key（形如 ` + "`fsk_...`" + `），然后：

` + "```" + `bash
curl -H "Authorization: Bearer fsk_xxxxxxxx" https://203.0.113.10:8443/api/ext/whoami
` + "```" + `

自签证书场景记得带上 CA：` + "`curl --cacert ca.crt ...`" + `（Windows 自带 curl 是 Schannel 版，
` + "`--cacert`" + ` 不生效，请用 ` + "`openssl s_client`" + ` 或 Node/Python 验证）。

## 时间怎么写

所有时间字段都接受三种写法，随便挑：

| 写法 | 例子 |
|---|---|
| 人类字符串 | ` + "`\"2026-09-15 14:30\"`" + ` |
| ISO 8601 | ` + "`\"2026-09-15T14:30:00+08:00\"`" + ` |
| epoch 秒 | ` + "`1789536600`" + ` |

查询串里可以用 ` + "`date=2026-09-15`" + `、` + "`date=today`" + `、` + "`date=tomorrow`" + `、
` + "`date=明天`" + `，配合 ` + "`days=N`" + `。

## 先问「今天有什么」

一次调用同时拿到日程和天气，最省 token：

` + "```" + `bash
curl -H "Authorization: Bearer $KEY" \
  "https://HOST:8443/api/ext/agenda?date=today"
` + "```" + `

响应里的 ` + "`text`" + ` 字段已经是排好版的纯文本，直接读那个就行：

` + "```" + `
2026-09-13 星期日，共 3 项
09-13 09:00-10:00 牙医  [个人]  @医院
09-13 14:30-15:30 周会  [工作]
09-13 全天 交水电费  [个人]  (已完成)

天气：小雨 25.1~31.4°C 降水 12.3mm（met.no）
` + "```" + `

## 新建日程

` + "```" + `bash
curl -X POST -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{
        "calendar_id": "c_xxxx",
        "title": "牙医复诊",
        "start": "2026-09-20 14:30",
        "duration_minutes": 60,
        "location": "市口腔医院",
        "remind_minutes_before": [1440, 30]
      }' \
  https://HOST:8443/api/ext/events
` + "```" + `

- ` + "`id`" + ` 可以不填，服务器会生成
- ` + "`end`" + ` 和 ` + "`duration_minutes`" + ` 二选一即可（默认 1 小时）
- ` + "`remind_minutes_before`" + ` 是 ` + "`reminders`" + ` 的简写，数字或数组都行

## 循环日程

` + "`rrule`" + ` 用的是标准 RFC 5545：

| 需求 | rrule |
|---|---|
| 每天 | ` + "`FREQ=DAILY`" + ` |
| 每周一三五 | ` + "`FREQ=WEEKLY;BYDAY=MO,WE,FR`" + ` |
| 每月 15 号 | ` + "`FREQ=MONTHLY;BYMONTHDAY=15`" + ` |
| 每年 10 月 1 日 | ` + "`FREQ=YEARLY;BYMONTH=10;BYMONTHDAY=1`" + ` |
| 每两周一次，共 8 次 | ` + "`FREQ=WEEKLY;INTERVAL=2;COUNT=8`" + ` |
| 到 2026 年底为止 | ` + "`FREQ=DAILY;UNTIL=20261231T235959Z`" + ` |

## 改 / 删

` + "```" + `bash
# 只改标题，其它字段不动
curl -X PUT -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"title": "改过的标题"}' https://HOST:8443/api/ext/events/e_xxxx

# 删整个系列
curl -X DELETE -H "Authorization: Bearer $KEY" https://HOST:8443/api/ext/events/e_xxxx

# 只改循环日程的某一次（occurrence_key 是该次开始的 epoch 秒，从 /agenda 里拿）
curl -X PUT -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"occurrence_key": 1789536600, "patch": {"start": "2026-09-20 16:00"}}' \
  https://HOST:8443/api/ext/events/e_xxxx/occurrence
` + "```" + `

## 日历本身也能改

日程能增删改，日历也一样（` + "`GET /api/ext/calendars`" + ` 里 ` + "`can_write=true`" + ` 的才能改）：

` + "```" + `bash
# 新建一个"当课程表用"的日历
curl -X POST -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"name": "大二上课程表", "color": "#378ADD", "course": true}' \
  https://HOST:8443/api/ext/calendars

# 改名字 / 换个颜色（只传要改的字段，没传的不会动）
curl -X PUT -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"name": "大二下课程表", "color": "#E8833A"}' \
  https://HOST:8443/api/ext/calendars/c_xxxx

# 取消"当课程表用"（注意：kind 是一个字段、三个取值互斥，
# 取消会退回 private，不会保留原来的共享状态）
curl -X PUT -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"course": false}' https://HOST:8443/api/ext/calendars/c_xxxx

# 删掉（只有创建者能删；里面的日程会一起删）
curl -X DELETE -H "Authorization: Bearer $KEY" https://HOST:8443/api/ext/calendars/c_xxxx
` + "```" + `

- ` + "`course` / `shared`" + ` 是两个**布尔简写**，服务端会翻成 ` + "`kind`" + ` ——
  比直接写 ` + "`kind`" + ` 不容易错（` + "`kind`" + ` 一个字段三个取值互斥）。
- 要一起共享就给 ` + "`members: [\"小明\"]`" + `（账号名字）或 ` + "`member_ids`" + `（从 ` + "`GET /api/ext/users`" + ` 拿）。
  **名字查不到或重名会明确报错**，不会静默丢掉一个名字 —— 免得你以为共享了其实没有。

## 其它

- ` + "`GET /api/ext/calendars`" + ` 列出日历（` + "`can_write=true`" + ` 的才能写）
- ` + "`GET /api/ext/users`" + ` 列出账号（建 / 分享日历时拿 ` + "`member_ids`" + `）
- ` + "`GET /api/ext/weather`" + ` 逐小时天气预报（` + "`?hours=48&days=7`" + `）
- ` + "`GET /api/ext/openapi.json`" + ` 机器可读的接口描述，喂给模型最省事

## 冲突

改动走的是和客户端同一套字段级合并：只改不同字段会自动合并；两台设备改了同一条的同一个字段
才会产生冲突，冲突会出现在客户端的「冲突中心」里等人处理，接口调用本身不会因此失败。
`

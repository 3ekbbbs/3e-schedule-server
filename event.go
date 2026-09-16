package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

type reminderReq struct {
	ID            string `json:"id"`
	MinutesBefore int64  `json:"minutes_before"`
	Channel       string `json:"channel"`
}

type eventReq struct {
	ID          string         `json:"id"`
	CalendarID  string         `json:"calendar_id"`
	Title       *string        `json:"title"`
	Notes       *string        `json:"notes"`
	Location    *string        `json:"location"`
	AllDay      *bool          `json:"all_day"`
	DTStartUTC  *int64         `json:"dtstart_utc"`
	DurationSec *int64         `json:"duration_sec"`
	TZID        *string        `json:"tzid"`
	RRule       *string        `json:"rrule"`
	RDate       []int64        `json:"rdate"`
	ExDate      []int64        `json:"exdate"`
	IsTask      *bool          `json:"is_task"`
	DoneAt      *int64         `json:"done_at"`
	Reminders   *[]reminderReq `json:"reminders"`
	// 强提醒开关。**必须是 *bool**：用 bool 的话"没传"和"传了 false"无法区分，
	// 老客户端每次保存整条日程都会把用户的强提醒**静默关掉**。
	// 字段不存在 = 不动现有设置。
	Strong   *bool   `json:"strong"`
	BaseSeqs jsonMap `json:"field_base_seqs"`
	DeviceID string  `json:"device_id"`
}

func (req *eventReq) incomingMap() jsonMap {
	m := jsonMap{}
	if req.Title != nil {
		m["title"] = *req.Title
	}
	if req.Notes != nil {
		m["notes"] = *req.Notes
	}
	if req.Location != nil {
		m["location"] = *req.Location
	}
	if req.AllDay != nil {
		m["all_day"] = *req.AllDay
	}
	if req.DTStartUTC != nil {
		m["dtstart_utc"] = *req.DTStartUTC
	}
	if req.DurationSec != nil {
		m["duration_sec"] = *req.DurationSec
	}
	if req.TZID != nil {
		m["tzid"] = *req.TZID
	}
	if req.RRule != nil {
		m["rrule"] = *req.RRule
	}
	if req.RDate != nil {
		m["rdate"] = req.RDate
	}
	if req.ExDate != nil {
		m["exdate"] = req.ExDate
	}
	if req.IsTask != nil {
		m["is_task"] = *req.IsTask
	}
	if req.DoneAt != nil {
		m["done_at"] = *req.DoneAt
	}
	// ⚠️ reminders 和 strong **刻意不在这里**。
	//
	// 这个 map 是"参与字段级合并的**共享**字段"。提醒和强提醒都是**按人**的
	// （同一张共享日历里，你能要我不要），放进来的话：
	//   ① 会被当成共享字段写进 events 表；
	//   ② 还会进冲突判定 —— 两个人各自设自己的提醒会互相报冲突，纯属噪音。
	// 它们由调用方在 applyFieldMerge 之后单独按 uid 落库。
	return m
}

func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var req eventReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	if req.CalendarID == "" {
		apiErr(w, 400, "缺少 calendar_id")
		return
	}
	if !s.canWrite(req.CalendarID, uid) {
		apiErr(w, 403, "这个日历你没有编辑权限")
		return
	}
	if req.DTStartUTC == nil || *req.DTStartUTC == 0 {
		apiErr(w, 400, "缺少开始时间")
		return
	}

	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	id := newID("e_")
	now := time.Now().Unix()
	title := ""
	if req.Title != nil {
		title = *req.Title
	}
	tzid := "Asia/Shanghai"
	if req.TZID != nil && *req.TZID != "" {
		tzid = *req.TZID
	}
	rr := sql.NullString{}
	if req.RRule != nil && *req.RRule != "" {
		rr = sql.NullString{String: *req.RRule, Valid: true}
	}
	dur := int64(0)
	if req.DurationSec != nil {
		dur = *req.DurationSec
	}
	var doneAt any
	if req.DoneAt != nil {
		doneAt = *req.DoneAt
	}

	fs := jsonMap{}
	seq, err := nextSeq(tx, "event", id, "create")
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	m := req.incomingMap()
	for f := range m {
		fs[f] = seq
	}

	if _, err := tx.Exec(`
		INSERT INTO events(id,calendar_id,uid,title,notes,location,all_day,dtstart_utc,duration_sec,
			tzid,rrule,rdate,exdate,is_task,done_at,seq,field_seqs,updated_at,updated_by_device,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, req.CalendarID, newID("uid_"),
		title, strPtrVal(req.Notes), strPtrVal(req.Location),
		boolToInt(req.AllDay != nil && *req.AllDay), *req.DTStartUTC, dur,
		tzid, rr, jsonList(req.RDate), jsonList(req.ExDate),
		boolToInt(req.IsTask != nil && *req.IsTask), doneAt,
		seq, encodeFieldSeqs(fs), now, req.DeviceID, now); err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	// 提醒是**按人**的：创建者写自己的那一份。
	// 空列表是合法输入 —— "这条不用提醒我"，不再被兜底成默认 15 分钟。
	// （这不是"共享默认"：别人打开这条日程时看到的是他们自己的提醒，
	//  没有专属行就是空 = 不提醒。想给别人一个初始值请在界面上显式配置。）
	if req.Reminders != nil {
		if err := s.setPersonalReminders(tx, id, uid, reminderMinutes(*req.Reminders)); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		if err := markPersonalPref(tx, id, uid); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	// 强提醒：也是**按人**的（你可以开、我不开）。字段不存在时不动现有设置
	// —— 老客户端不传这个字段，行为必须和升级前完全一致。
	if req.Strong != nil {
		if err := s.setStrongReminder(tx, id, uid, *req.Strong); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	if err := recomputeInstances(tx, id, time.Now()); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	ev, _ := s.eventByID(id, uid)
	writeJSON(w, 200, map[string]any{"event": ev})
}

// handleGetEvent 读单条日程。
//
// 存在的理由：提醒是**按人**的，所以"我在这条日程上会不会被提醒"只能由
// 带着我的 token 的请求回答 —— 拿不到就只能靠同步流里的那份（可能已经过期）。
// 客户端在日程详情/提醒设置里用它。
func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	ev, ok := s.eventByID(id, uid)
	if !ok {
		apiErr(w, 404, "事件不存在")
		return
	}
	if _, ok := s.calendarRole(ev.CalendarID, uid); !ok {
		apiErr(w, 403, "无权访问这个日程")
		return
	}
	writeJSON(w, 200, map[string]any{"event": ev})
}

func (s *Server) handleUpdateEvent(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	ev, ok := s.eventByID(id, uid)
	if !ok {
		apiErr(w, 404, "事件不存在")
		return
	}
	if !s.canWrite(ev.CalendarID, uid) {
		apiErr(w, 403, "这个日历你没有编辑权限")
		return
	}
	var req eventReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}

	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	res, err := applyFieldMerge(tx, "event", id, req.incomingMap(), req.BaseSeqs, req.DeviceID)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	// 改提醒 = 改**我自己**的提醒。别人在这条日程上的提前量不受影响。
	if req.Reminders != nil {
		if err := s.setPersonalReminders(tx, id, uid, reminderMinutes(*req.Reminders)); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		if err := markPersonalPref(tx, id, uid); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	// 强提醒同样按人。**这个入口不能漏** —— 只加 POST 的话会表现成
	// "新建时能设、之后改不了"（用户点开关保存后没反应，且不报错）。
	if req.Strong != nil {
		if err := s.setStrongReminder(tx, id, uid, *req.Strong); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	if err := recomputeInstances(tx, id, time.Now()); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	ev2, _ := s.eventByID(id, uid)
	writeJSON(w, 200, map[string]any{"event": ev2, "conflicts": res.Conflicts})
}

func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	ev, ok := s.eventByID(id, uid)
	if !ok {
		apiErr(w, 404, "事件不存在")
		return
	}
	if !s.canWrite(ev.CalendarID, uid) {
		apiErr(w, 403, "这个日历你没有编辑权限")
		return
	}
	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	seq, _ := nextSeq(tx, "event", id, "delete")
	if _, err := tx.Exec(`UPDATE events SET deleted_at=?, seq=?, updated_at=? WHERE id=?`, now, seq, now, id); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := recomputeInstances(tx, id, time.Now()); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type occurrenceReq struct {
	OccurrenceKey int64          `json:"occurrence_key"`
	Patch         map[string]any `json:"patch"`
	Cancelled     bool           `json:"cancelled"`
	DeviceID      string         `json:"device_id"`
}

// handleUpsertOccurrence implements "edit just this one", stored in
// event_overrides keyed by the ORIGINAL occurrence time so a later RRULE
// change does not silently re-point the override at a different instance.
func (s *Server) handleUpsertOccurrence(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	ev, ok := s.eventByID(id, uid)
	if !ok {
		apiErr(w, 404, "事件不存在")
		return
	}
	if !s.canWrite(ev.CalendarID, uid) {
		apiErr(w, 403, "这个日历你没有编辑权限")
		return
	}
	var req occurrenceReq
	if err := readJSON(r, &req); err != nil || req.OccurrenceKey == 0 {
		apiErr(w, 400, "缺少 occurrence_key")
		return
	}

	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	patchBytes, _ := json.Marshal(req.Patch)
	seq, _ := nextSeq(tx, "event", id, "occurrence")
	if _, err := tx.Exec(`
		INSERT INTO event_overrides(event_id,occurrence_key,patch,cancelled,seq,field_seqs,updated_at,updated_by_device)
		VALUES(?,?,?,?,?,'{}',?,?)
		ON CONFLICT(event_id,occurrence_key) DO UPDATE SET
			patch=excluded.patch, cancelled=excluded.cancelled, seq=excluded.seq,
			updated_at=excluded.updated_at, updated_by_device=excluded.updated_by_device`,
		id, req.OccurrenceKey, string(patchBytes), boolToInt(req.Cancelled), seq, now, req.DeviceID); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := recomputeInstances(tx, id, time.Now()); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	ev2, _ := s.eventByID(id, uid)
	writeJSON(w, 200, map[string]any{"event": ev2})
}

func (s *Server) handleDeleteOccurrence(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	ev, ok := s.eventByID(id, uid)
	if !ok {
		apiErr(w, 404, "事件不存在")
		return
	}
	if !s.canWrite(ev.CalendarID, uid) {
		apiErr(w, 403, "这个日历你没有编辑权限")
		return
	}
	key, ok2 := atoi64(r.URL.Query().Get("key"))
	if !ok2 {
		apiErr(w, 400, "缺少 key")
		return
	}

	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	seq, _ := nextSeq(tx, "event", id, "occurrence")
	if _, err := tx.Exec(`
		INSERT INTO event_overrides(event_id,occurrence_key,patch,cancelled,seq,updated_at)
		VALUES(?,?,'{}',1,?,?)
		ON CONFLICT(event_id,occurrence_key) DO UPDATE SET cancelled=1, seq=excluded.seq, updated_at=excluded.updated_at`,
		id, key, seq, now); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := recomputeInstances(tx, id, time.Now()); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type snoozeReq struct {
	OccurrenceKey int64  `json:"occurrence_key"`
	Minutes       int64  `json:"minutes"`
	DeviceID      string `json:"device_id"`
}

// handleSnooze writes the snooze server-side on purpose: if it stayed local,
// snoozing on the phone would still let the desktop ring.
func (s *Server) handleSnooze(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	var req snoozeReq
	if err := readJSON(r, &req); err != nil || req.Minutes <= 0 {
		apiErr(w, 400, "bad json")
		return
	}
	ev, ok := s.eventByID(id, uid)
	if !ok {
		apiErr(w, 404, "事件不存在")
		return
	}
	if _, ok := s.calendarRole(ev.CalendarID, uid); !ok {
		apiErr(w, 403, "无权访问")
		return
	}
	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	sid := newID("s_")
	seq, _ := nextSeq(tx, "snooze", sid, "upsert")
	if _, err := tx.Exec(`
		INSERT INTO snoozes(id,event_id,occurrence_key,user_id,remind_at_utc,device_id,seq,deleted_at)
		VALUES(?,?,?,?,?,?,?,NULL)`,
		sid, id, req.OccurrenceKey, uid, now+req.Minutes*60, req.DeviceID, seq); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "remind_at_utc": now + req.Minutes*60})
}

func (s *Server) handleClearSnooze(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()
	rows, _ := tx.Query(`SELECT id FROM snoozes WHERE event_id=? AND user_id=? AND deleted_at IS NULL`, id, uid)
	ids := []string{}
	for rows != nil && rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err == nil {
			ids = append(ids, sid)
		}
	}
	if rows != nil {
		rows.Close()
	}
	for _, sid := range ids {
		seq, _ := nextSeq(tx, "snooze", sid, "delete")
		_, _ = tx.Exec(`UPDATE snoozes SET deleted_at=?, seq=? WHERE id=?`, time.Now().Unix(), seq, sid)
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// eventByID loads an event for a specific viewer.
//
// `uid` 只影响**提醒列表**：提醒是按人的，同一条日程不同的人拿到不同的份。
// 事件本身的字段对所有能看到它的人是一样的，所以传空 uid 也能拿到主体
// （此时提醒为空），内部调用点（权限预检等）不需要改。
func (s *Server) eventByID(id, viewerID string) (*Event, bool) {
	var e Event
	var fs, rdateS, exdateS string
	var rr, evuid sql.NullString
	var dtstart, doneAt, deleted sql.NullInt64
	var allDay, isTask int64
	err := s.db.sql.QueryRow(`
		SELECT id,calendar_id,uid,title,notes,location,all_day,dtstart_utc,duration_sec,tzid,
		       rrule,rdate,exdate,is_task,done_at,seq,field_seqs,updated_at,created_at,deleted_at
		FROM events WHERE id=?`, id).Scan(
		&e.ID, &e.CalendarID, &evuid, &e.Title, &e.Notes, &e.Location, &allDay, &dtstart, &e.DurationSec, &e.TZID,
		&rr, &rdateS, &exdateS, &isTask, &doneAt, &e.Seq, &fs, &e.UpdatedAt, &e.CreatedAt, &deleted)
	if err != nil {
		return nil, false
	}
	e.UID = evuid.String
	if dtstart.Valid {
		e.DTStartUTC = dtstart.Int64
	}
	e.AllDay = allDay != 0
	e.IsTask = isTask != 0
	if rr.Valid {
		e.RRule = rr.String
	}
	if doneAt.Valid {
		e.DoneAt = &doneAt.Int64
	}
	if deleted.Valid {
		e.DeletedAt = &deleted.Int64
	}
	_ = json.Unmarshal([]byte(rdateS), &e.RDate)
	_ = json.Unmarshal([]byte(exdateS), &e.ExDate)
	e.FieldSeqs = decodeFieldSeqs(fs)
	// 提醒是**按人**的：返回调用者自己会响的那一份。
	// 空列表是正常结果 —— 表示"这条不提醒我"。
	e.Reminders = remindersForWire(s.remindersForUser(id, viewerID, false))
	// 强提醒同理，也按调用者过滤。
	// ⚠️ 必须填在这里：增量同步走的也是 eventByID（loadEntities 传的正是
	// 调用者的 uid），所以填在这一处，GET 和同步流就都是按人的了。
	e.Strong = s.strongForUser(id, viewerID)
	return &e, true
}

// remindersOf 返回事件上所有提醒行（含共享默认行），管理用途。
func (s *Server) remindersOf(eventID string) []Reminder {
	rows, err := s.db.sql.Query(`SELECT id,event_id,minutes_before,channel,seq FROM reminders
		WHERE event_id=? AND deleted_at IS NULL ORDER BY minutes_before DESC`, eventID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []Reminder{}
	for rows.Next() {
		var r Reminder
		if err := rows.Scan(&r.ID, &r.EventID, &r.MinutesBefore, &r.Channel, &r.Seq); err == nil {
			out = append(out, r)
		}
	}
	return out
}

func remindersForWire(views []reminderView) []Reminder {
	out := make([]Reminder, 0, len(views))
	for _, v := range views {
		out = append(out, Reminder{
			ID: v.ID, EventID: v.EventID,
			MinutesBefore: v.MinutesBefore, Channel: v.Channel,
		})
	}
	return out
}

// reminderMinutes 把请求体里的提醒列表压成分钟数。
// **长度为 0 是有效输入**，表示"不要提醒"，与"字段没传"（nil 指针）不是一回事。
func reminderMinutes(list []reminderReq) []int64 {
	out := make([]int64, 0, len(list))
	for _, r := range list {
		out = append(out, r.MinutesBefore)
	}
	return out
}

func strPtrVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func jsonList(v []int64) string {
	if v == nil {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

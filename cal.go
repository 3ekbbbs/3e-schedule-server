package main

import (
	"database/sql"
	"net/http"
	"time"
)

// loadMembers returns a calendar's member list.
//
// `role` 是 SQL 里的真列名，不是 Go 关键字 —— 但 SELECT 列表里少写一个字段
// 会让后面的 Scan 静默错位（值照样塞进去、类型不对就跳过），历史上出过一次。
// 所以列名和 Scan 目标严格按同一顺序排，别插队。
func (s *Server) loadMembers(calendarID string) []Member {
	rows, err := s.db.sql.Query(`
		SELECT cm.user_id, COALESCE(u.name,''), cm.role
		FROM calendar_members cm LEFT JOIN users u ON u.id = cm.user_id
		WHERE cm.calendar_id=? ORDER BY cm.role, u.name`, calendarID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Name, &m.Role); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) calendarByID(id, uid string) (*Calendar, bool) {
	var c Calendar
	var deleted sql.NullInt64
	var fs string
	var ownerName sql.NullString
	err := s.db.sql.QueryRow(`
		SELECT c.id,c.owner_id,c.name,c.color,c.kind,c.seq,c.field_seqs,c.updated_at,c.created_at,c.deleted_at,
		       COALESCE(u.name,'')
		FROM calendars c LEFT JOIN users u ON u.id=c.owner_id WHERE c.id=?`, id).
		Scan(&c.ID, &c.OwnerID, &c.Name, &c.Color, &c.Kind, &c.Seq, &fs, &c.UpdatedAt, &c.CreatedAt, &deleted, &ownerName)
	if err != nil {
		return nil, false
	}
	if deleted.Valid {
		c.DeletedAt = &deleted.Int64
	}
	c.FieldSeqs = decodeFieldSeqs(fs)
	c.OwnerName = ownerName.String
	role, ok := s.calendarRole(id, uid)
	if !ok {
		return nil, false
	}
	c.Role = role
	c.Members = s.loadMembers(id)
	return &c, true
}

// handleListCalendars returns every calendar the caller can see.
// calendarsFor returns every calendar the user can see: the ones they own plus
// the ones they are a member of.
func (s *Server) calendarsFor(uid string) ([]Calendar, error) {
	rows, err := s.db.sql.Query(`
		SELECT c.id FROM calendars c
		WHERE c.owner_id = ?
		   OR EXISTS(SELECT 1 FROM calendar_members m WHERE m.calendar_id=c.id AND m.user_id=?)
		ORDER BY c.created_at`, uid, uid)
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

	out := []Calendar{}
	for _, id := range ids {
		if c, ok := s.calendarByID(id, uid); ok {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (s *Server) handleListCalendars(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	out, err := s.calendarsFor(uid)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"calendars": out})
}

type calendarReq struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Color     string   `json:"color"`
	Kind      string   `json:"kind"`
	MemberIDs []string `json:"member_ids"`
	BaseSeqs  jsonMap  `json:"field_base_seqs"`
	DeviceID  string   `json:"device_id"`
}

func (s *Server) handleCreateCalendar(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var req calendarReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	req.Name = trimSpace(req.Name)
	if req.Name == "" {
		apiErr(w, 400, "日历名称不能为空")
		return
	}
	if req.Color == "" {
		req.Color = "#378ADD"
	}
	kind := req.Kind
	// "course" marks a timetable calendar: the same events, rendered as a
	// periods x weekdays grid instead of an agenda.
	if kind != "shared" && kind != "course" {
		kind = "private"
	}

	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	id := newID("c_")
	now := time.Now().Unix()
	fs := jsonMap{}
	seq, err := nextSeq(tx, "calendar", id, "create")
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	for _, f := range []string{"name", "color", "kind"} {
		fs[f] = seq
	}
	if _, err := tx.Exec(`
		INSERT INTO calendars(id,owner_id,name,color,kind,seq,field_seqs,updated_at,updated_by_device,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, uid, req.Name, req.Color, kind, seq, encodeFieldSeqs(fs), now, req.DeviceID, now); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	// the owner is always a member so the permission query has one code path
	if _, err := tx.Exec(`INSERT INTO calendar_members(calendar_id,user_id,role) VALUES(?,?,?)`,
		id, uid, "owner"); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	for _, mid := range req.MemberIDs {
		if mid == "" || mid == uid {
			continue
		}
		_, _ = tx.Exec(`INSERT INTO calendar_members(calendar_id,user_id,role) VALUES(?,?,?)`,
			id, mid, "editor")
	}
	// 新成员要能看到这张日历**里面已有的东西**，否则他只会看到一个空日历
	// （建日历那一刻通常还没有日程，这一步是为"先有日程、后来才共享"准备的）
	if err := s.resendCalendarContents(tx, id); err != nil {
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

func (s *Server) handleUpdateCalendar(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	if _, ok := s.calendarRole(id, uid); !ok {
		apiErr(w, 404, "日历不存在或无权访问")
		return
	}
	if !s.canWrite(id, uid) {
		apiErr(w, 403, "这个日历你没有编辑权限")
		return
	}
	var req calendarReq
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

	incoming := jsonMap{}
	if req.Name != "" {
		incoming["name"] = req.Name
	}
	if req.Color != "" {
		incoming["color"] = req.Color
	}
	if req.Kind != "" {
		incoming["kind"] = req.Kind
	}

	res, err := applyFieldMerge(tx, "calendar", id, incoming, req.BaseSeqs, req.DeviceID)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	c, _ := s.calendarByID(id, uid)
	writeJSON(w, 200, map[string]any{"calendar": c, "conflicts": res.Conflicts})
}

func (s *Server) handleDeleteCalendar(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	role, ok := s.calendarRole(id, uid)
	if !ok {
		apiErr(w, 404, "日历不存在")
		return
	}
	if role != "owner" {
		apiErr(w, 403, "只有日历创建者能删除")
		return
	}
	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	seq, _ := nextSeq(tx, "calendar", id, "delete")
	if _, err := tx.Exec(`UPDATE calendars SET deleted_at=?, seq=?, updated_at=? WHERE id=?`, now, seq, now, id); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	// cascade the tombstone to events and their instances
	rows, _ := tx.Query(`SELECT id FROM events WHERE calendar_id=? AND deleted_at IS NULL`, id)
	evIDs := []string{}
	for rows != nil && rows.Next() {
		var eid string
		if err := rows.Scan(&eid); err == nil {
			evIDs = append(evIDs, eid)
		}
	}
	if rows != nil {
		rows.Close()
	}
	for _, eid := range evIDs {
		eseq, _ := nextSeq(tx, "event", eid, "delete")
		_, _ = tx.Exec(`UPDATE events SET deleted_at=?, seq=?, updated_at=? WHERE id=?`, now, eseq, now, eid)
		if err := recomputeInstances(tx, eid, time.Now()); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	for j > i && (s[j-1] == ' ' || s[j-1] == '\t' || s[j-1] == '\n' || s[j-1] == '\r') {
		j--
	}
	return s[i:j]
}

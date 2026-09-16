package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// ---------- field-level merge ----------

type mergeResult struct {
	Applied   []string
	Conflicts []Conflict
}

// applyFieldMerge writes only the fields whose server-side version is not
// newer than what the client claimed to have seen. Fields that did move ahead
// become conflict rows instead of silently overwriting.
//
// This is what lets "A renamed the event, B moved it" merge without bothering
// anyone, while a genuine same-field clash still surfaces in the conflict
// centre.
func applyFieldMerge(tx *sql.Tx, entity, id string, incoming jsonMap, baseSeqs jsonMap, deviceID string) (mergeResult, error) {
	res := mergeResult{}
	if len(incoming) == 0 {
		return res, nil
	}
	kinds := kindMapFor(entity)
	table := tableFor(entity)

	cols := make([]string, 0, len(kinds))
	for f := range kinds {
		cols = append(cols, f)
	}
	sort.Strings(cols)

	sel := "field_seqs"
	for _, c := range cols {
		sel += ", " + c
	}
	var fsStr string
	vals := make([]any, len(cols))
	dest := make([]any, 0, len(cols)+1)
	dest = append(dest, &fsStr)
	for i := range cols {
		dest = append(dest, &vals[i])
	}
	if err := tx.QueryRow("SELECT "+sel+" FROM "+table+" WHERE id=?", id).Scan(dest...); err != nil {
		return res, err
	}
	fs := decodeFieldSeqs(fsStr)
	idxOf := map[string]int{}
	for i, c := range cols {
		idxOf[c] = i
	}

	// nil baseSeqs means "this write is authoritative" — used by conflict
	// resolution, where the user has already decided. Without this the
	// keep_mine / keep_both paths would just raise the same conflict again.
	force := baseSeqs == nil

	accFields := []string{}
	accVals := []any{}
	now := time.Now().Unix()

	keys := make([]string, 0, len(incoming))
	for f := range incoming {
		keys = append(keys, f)
	}
	sort.Strings(keys) // deterministic ordering keeps the tests stable

	for _, f := range keys {
		kind, ok := kinds[f]
		if !ok {
			continue
		}
		cur := fieldSeq(fs, f)
		base := fieldSeq(baseSeqs, f)
		if !force && cur > base {
			cid := newID("cf_")
			sv := encodeScalar(vals[idxOf[f]])
			cv := encodeScalar(incoming[f])
			if _, err := tx.Exec(`INSERT INTO conflicts(id,entity,entity_id,field,base_seq,server_value,client_value,client_device,created_at)
				VALUES(?,?,?,?,?,?,?,?,?)`, cid, entity, id, f, cur, sv, cv, deviceID, now); err != nil {
				return res, err
			}
			res.Conflicts = append(res.Conflicts, Conflict{
				ID: cid, Entity: entity, EntityID: id, Field: f, BaseSeq: cur,
				ServerValue: sv, ClientValue: cv, ClientDevice: deviceID, CreatedAt: now,
			})
			continue
		}
		v, err := coerce(kind, incoming[f])
		if err != nil {
			continue
		}
		accFields = append(accFields, f)
		accVals = append(accVals, v)
	}

	if len(accFields) == 0 {
		return res, nil
	}

	seq, err := nextSeq(tx, entity, id, "upsert")
	if err != nil {
		return res, err
	}
	for _, f := range accFields {
		fs[f] = seq
		res.Applied = append(res.Applied, f)
	}

	set := ""
	args := []any{}
	for i, f := range accFields {
		if i > 0 {
			set += ", "
		}
		set += f + "=?"
		args = append(args, accVals[i])
	}
	set += ", seq=?, field_seqs=?, updated_at=?, updated_by_device=?"
	args = append(args, seq, encodeFieldSeqs(fs), now, deviceID, id)

	if _, err := tx.Exec("UPDATE "+table+" SET "+set+" WHERE id=?", args...); err != nil {
		return res, err
	}
	return res, nil
}

func coerce(kind string, v any) (any, error) {
	switch kind {
	case kindText:
		if v == nil {
			return "", nil
		}
		if s, ok := v.(string); ok {
			return s, nil
		}
		b, err := json.Marshal(v)
		return string(b), err
	case kindInt:
		switch t := v.(type) {
		case nil:
			return nil, nil
		case int64:
			return t, nil
		case int:
			return int64(t), nil
		case float64:
			return int64(t), nil
		case bool:
			return boolToInt(t), nil
		case string:
			if t == "" {
				return nil, nil
			}
			return strconv.ParseInt(t, 10, 64)
		}
		return nil, nil
	case kindBool:
		switch t := v.(type) {
		case bool:
			return boolToInt(t), nil
		case float64:
			if t != 0 {
				return int64(1), nil
			}
			return int64(0), nil
		case int64:
			return boolToInt(t != 0), nil
		case string:
			return boolToInt(t == "true" || t == "1"), nil
		}
		return int64(0), nil
	case kindJSON:
		if v == nil {
			return "[]", nil
		}
		if s, ok := v.(string); ok {
			return s, nil
		}
		b, err := json.Marshal(v)
		return string(b), err
	}
	return v, nil
}

// encodeScalar renders a field value for a conflict row.
//
// Always JSON, never a bare string: conflict values must round-trip through
// json.Unmarshal on the way back in (that is how keep_mine and keep_both
// re-apply the losing side). Storing the raw text made every string field
// unmarshal to nil and silently write an empty value.
func encodeScalar(v any) *string {
	if v == nil {
		return nil
	}
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	s := string(out)
	return &s
}

// ---------- change feed ----------

type changeRef struct {
	entity string
	id     string
	seq    int64
}

func (s *Server) collectChanges(since int64, limit int) ([]changeRef, int64, bool, error) {
	rows, err := s.db.sql.Query(`
		SELECT entity, entity_id, MAX(seq) AS mx
		FROM sync_log WHERE seq > ?
		GROUP BY entity, entity_id
		ORDER BY mx ASC
		LIMIT ?`, since, limit+1)
	if err != nil {
		return nil, since, false, err
	}
	defer rows.Close()
	out := []changeRef{}
	for rows.Next() {
		var c changeRef
		if err := rows.Scan(&c.entity, &c.id, &c.seq); err == nil {
			out = append(out, c)
		}
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	last := since
	for _, c := range out {
		if c.seq > last {
			last = c.seq
		}
	}
	if !hasMore {
		if max, err := s.db.currentSeq(); err == nil && max > last {
			last = max
		}
	}
	return out, last, hasMore, nil
}

type syncPayload struct {
	Calendars []Calendar `json:"calendars"`
	Events    []Event    `json:"events"`
	Instances []Instance `json:"instances"`
	Snoozes   []Snooze   `json:"snoozes"`
	Conflicts []Conflict `json:"conflicts"`
	Users     []User     `json:"users,omitempty"`
}

func (s *Server) handleSyncChanges(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	since, _ := atoi64(r.URL.Query().Get("since"))
	limit := 500
	if v, ok := atoi64(r.URL.Query().Get("limit")); ok && v > 0 && v < 3000 {
		limit = int(v)
	}

	refs, last, hasMore, err := s.collectChanges(since, limit)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	payload, err := s.loadEntities(uid, refs)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"calendars": payload.Calendars,
		"events":    payload.Events,
		"instances": payload.Instances,
		"snoozes":   payload.Snoozes,
		"conflicts": payload.Conflicts,
		"last_seq":  last,
		"has_more":  hasMore,
	})
}

func (s *Server) handleSyncSnapshot(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	refs := []changeRef{}
	rows, err := s.db.sql.Query(`SELECT entity, entity_id, MAX(seq) FROM sync_log GROUP BY entity, entity_id`)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	for rows.Next() {
		var c changeRef
		if err := rows.Scan(&c.entity, &c.id, &c.seq); err == nil {
			refs = append(refs, c)
		}
	}
	rows.Close()

	payload, err := s.loadEntities(uid, refs)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	users, _ := s.allUsers()
	last, _ := s.db.currentSeq()
	writeJSON(w, 200, map[string]any{
		"calendars": payload.Calendars,
		"events":    payload.Events,
		"instances": payload.Instances,
		"snoozes":   payload.Snoozes,
		"conflicts": payload.Conflicts,
		"users":     users,
		"last_seq":  last,
	})
}

func (s *Server) allUsers() ([]User, error) {
	rows, err := s.db.sql.Query(`SELECT id,name,is_admin,created_at FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
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
	return out, nil
}

// loadEntities hydrates the change references, dropping anything the caller
// may not see. The cursor still advances past those, otherwise one member
// leaving a calendar would wedge everyone else's sync forever.
func (s *Server) loadEntities(uid string, refs []changeRef) (*syncPayload, error) {
	p := &syncPayload{Calendars: []Calendar{}, Events: []Event{}, Instances: []Instance{}, Snoozes: []Snooze{}, Conflicts: []Conflict{}}
	seenCal := map[string]bool{}
	seenEv := map[string]bool{}

	for _, ref := range refs {
		switch ref.entity {
		case "calendar":
			if seenCal[ref.id] {
				continue
			}
			seenCal[ref.id] = true
			if c, ok := s.calendarByID(ref.id, uid); ok {
				// 成员名单变了也要让客户端知道 —— 新接口是 REST 拉的，
				// 但同步流里带着 members 就不会出现"权限改了、另一台还显示旧名单"。
				p.Calendars = append(p.Calendars, *c)
			}
		case "event":
			if seenEv[ref.id] {
				continue
			}
			seenEv[ref.id] = true
			ev, ok := s.eventByID(ref.id, uid)
			if !ok {
				continue
			}
			if _, ok := s.calendarRole(ev.CalendarID, uid); !ok {
				continue
			}
			p.Events = append(p.Events, *ev)
		case "instance":
			inst, ok := s.instanceByID(ref.id)
			if !ok {
				continue
			}
			if _, ok := s.calendarRole(inst.CalendarID, uid); !ok {
				continue
			}
			p.Instances = append(p.Instances, *inst)
		case "snooze":
			sn, ok := s.snoozeByID(ref.id)
			if !ok {
				continue
			}
			if sn.UserID != uid {
				continue
			}
			p.Snoozes = append(p.Snoozes, *sn)
		}
	}
	cf, err := s.conflictsFor(uid, false)
	if err == nil {
		p.Conflicts = cf
	}
	return p, nil
}

func (s *Server) instanceByID(id string) (*Instance, bool) {
	var in Instance
	var deleted sql.NullInt64
	var doneAt sql.NullInt64
	var allDay, isTask, isOvr int64
	err := s.db.sql.QueryRow(`
		SELECT id,event_id,occurrence_key,calendar_id,start_utc,end_utc,all_day,title,notes,
		       location,is_task,done_at,is_override,seq,deleted_at
		FROM instances WHERE id=?`, id).Scan(
		&in.ID, &in.EventID, &in.OccurrenceKey, &in.CalendarID, &in.StartUTC, &in.EndUTC,
		&allDay, &in.Title, &in.Notes, &in.Location, &isTask, &doneAt, &isOvr, &in.Seq, &deleted)
	if err != nil {
		return nil, false
	}
	in.AllDay = allDay != 0
	in.IsTask = isTask != 0
	in.IsOverride = isOvr != 0
	if doneAt.Valid {
		in.DoneAt = &doneAt.Int64
	}
	if deleted.Valid {
		in.DeletedAt = &deleted.Int64
	}
	return &in, true
}

func (s *Server) snoozeByID(id string) (*Snooze, bool) {
	var sn Snooze
	var deleted sql.NullInt64
	err := s.db.sql.QueryRow(`SELECT id,event_id,occurrence_key,user_id,remind_at_utc,device_id,seq,deleted_at
		FROM snoozes WHERE id=?`, id).Scan(&sn.ID, &sn.EventID, &sn.OccurrenceKey, &sn.UserID,
		&sn.RemindAtUTC, &sn.DeviceID, &sn.Seq, &deleted)
	if err != nil {
		return nil, false
	}
	if deleted.Valid {
		sn.DeletedAt = &deleted.Int64
	}
	return &sn, true
}

// ---------- push ----------

type pushOp struct {
	Entity   string  `json:"entity"`
	Op       string  `json:"op"`
	ID       string  `json:"id"`
	Data     jsonMap `json:"data"`
	BaseSeqs jsonMap `json:"field_base_seqs"`
	DeviceID string  `json:"device_id"`
}

type pushReq struct {
	DeviceID string   `json:"device_id"`
	Ops      []pushOp `json:"ops"`
}

// opPre holds everything a push handler needs BEFORE a transaction is opened.
//
// This exists because the pool is deliberately capped at one connection. Any
// s.db.sql.* call made while a tx is open would queue behind that tx on the
// same single connection and deadlock the whole server, so every permission
// and existence check is resolved up front with no tx in flight.
type opPre struct {
	notFound bool
	denied   bool
	calID    string
	role     string
}

func (s *Server) preflight(uid string, op pushOp) opPre {
	switch op.Entity {
	case "calendar":
		role, ok := s.calendarRole(op.ID, uid)
		if !ok {
			var n int
			_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM calendars WHERE id=?`, op.ID).Scan(&n)
			if n == 0 && op.Op != "delete" {
				return opPre{notFound: true}
			}
			return opPre{denied: true}
		}
		if op.Op == "delete" && role != "owner" && role != "admin" {
			return opPre{denied: true}
		}
		if op.Op != "delete" && !s.canWrite(op.ID, uid) {
			return opPre{denied: true}
		}
		return opPre{calID: op.ID, role: role}

	case "event":
		ev, ok := s.eventByID(op.ID, "")
		if !ok {
			calID := op.Data.stringOf("calendar_id")
			if calID == "" || !s.canWrite(calID, uid) {
				return opPre{denied: true}
			}
			return opPre{notFound: true, calID: calID}
		}
		if !s.canWrite(ev.CalendarID, uid) {
			return opPre{denied: true}
		}
		return opPre{calID: ev.CalendarID}

	case "snooze":
		if op.Op == "delete" {
			return opPre{}
		}
		ev, ok := s.eventByID(op.Data.stringOf("event_id"), "")
		if !ok {
			return opPre{denied: true}
		}
		if _, ok := s.calendarRole(ev.CalendarID, uid); !ok {
			return opPre{denied: true}
		}
		return opPre{calID: ev.CalendarID}
	}
	return opPre{denied: true}
}

func (s *Server) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var req pushReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	applied := []map[string]any{}
	conflicts := []Conflict{}
	errs := []string{}

	// One transaction per op rather than one per batch: the preflight for op N
	// must observe the commit of op N-1 (a batch legitimately creates an
	// entity and then patches it).
	for _, op := range req.Ops {
		dev := op.DeviceID
		if dev == "" {
			dev = req.DeviceID
		}
		pre := s.preflight(uid, op)
		if pre.denied {
			errs = append(errs, op.Entity+" "+op.ID+": forbidden")
			continue
		}

		tx, err := s.db.sql.Begin()
		if err != nil {
			errs = append(errs, op.Entity+":"+err.Error())
			continue
		}

		var res mergeResult
		switch op.Entity {
		case "calendar":
			res, err = s.pushCalendar(tx, uid, op, dev, pre)
		case "event":
			res, err = s.pushEvent(tx, uid, op, dev, pre)
		case "snooze":
			err = s.pushSnooze(tx, uid, op, dev)
			if err == nil {
				applied = append(applied, map[string]any{"entity": "snooze", "id": op.ID})
			}
		default:
			err = errForbidden
			errs = append(errs, "unknown entity "+op.Entity)
		}

		if err != nil {
			_ = tx.Rollback()
			errs = append(errs, op.Entity+" "+op.ID+": "+err.Error())
			continue
		}
		if err := tx.Commit(); err != nil {
			errs = append(errs, op.Entity+" "+op.ID+": commit "+err.Error())
			continue
		}
		conflicts = append(conflicts, res.Conflicts...)
		if len(res.Applied) > 0 {
			applied = append(applied, map[string]any{"entity": op.Entity, "id": op.ID, "fields": res.Applied})
		}
	}

	seq, _ := s.db.currentSeq()
	writeJSON(w, 200, map[string]any{
		"applied": applied, "conflicts": conflicts, "errors": errs, "seq": seq,
	})
}

func (s *Server) pushCalendar(tx *sql.Tx, uid string, op pushOp, dev string, pre opPre) (mergeResult, error) {
	if op.Op == "delete" {
		now := time.Now().Unix()
		seq, err := nextSeq(tx, "calendar", op.ID, "delete")
		if err != nil {
			return mergeResult{}, err
		}
		_, err = tx.Exec(`UPDATE calendars SET deleted_at=?, seq=?, updated_at=? WHERE id=?`, now, seq, now, op.ID)
		return mergeResult{}, err
	}

	if pre.notFound {
		now := time.Now().Unix()
		kind := "private"
		if v, ok := op.Data["kind"]; ok {
			if str, _ := v.(string); str == "shared" || str == "course" {
				kind = str
			}
		}
		seq, err := nextSeq(tx, "calendar", op.ID, "create")
		if err != nil {
			return mergeResult{}, err
		}
		fs := jsonMap{}
		for f := range op.Data {
			fs[f] = seq
		}
		if _, err := tx.Exec(`
			INSERT INTO calendars(id,owner_id,name,color,kind,seq,field_seqs,updated_at,updated_by_device,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			op.ID, uid, op.Data.stringOf("name"), orDefault(op.Data.stringOf("color"), "#378ADD"),
			kind, seq, encodeFieldSeqs(fs), now, dev, now); err != nil {
			return mergeResult{}, err
		}
		_, err = tx.Exec(`INSERT INTO calendar_members(calendar_id,user_id,role) VALUES(?,?,?)`, op.ID, uid, "owner")
		return mergeResult{}, err
	}
	return applyFieldMerge(tx, "calendar", op.ID, op.Data, op.BaseSeqs, dev)
}

func (s *Server) pushEvent(tx *sql.Tx, uid string, op pushOp, dev string, pre opPre) (mergeResult, error) {
	if op.Op == "delete" {
		now := time.Now().Unix()
		seq, err := nextSeq(tx, "event", op.ID, "delete")
		if err != nil {
			return mergeResult{}, err
		}
		if _, err := tx.Exec(`UPDATE events SET deleted_at=?, seq=?, updated_at=? WHERE id=?`, now, seq, now, op.ID); err != nil {
			return mergeResult{}, err
		}
		return mergeResult{}, recomputeInstances(tx, op.ID, time.Now())
	}

	if pre.notFound {
		now := time.Now().Unix()
		seq, err := nextSeq(tx, "event", op.ID, "create")
		if err != nil {
			return mergeResult{}, err
		}
		fs := jsonMap{}
		for f := range op.Data {
			fs[f] = seq
		}
		var dtstart any
		if v, ok := op.Data.int64Of("dtstart_utc"); ok {
			dtstart = v
		}
		var rr any
		if str := op.Data.stringOf("rrule"); str != "" {
			rr = str
		}
		var doneAt any
		if v, ok := op.Data["done_at"]; ok && v != nil {
			doneAt = v
		}
		if _, err := tx.Exec(`
			INSERT INTO events(id,calendar_id,uid,title,notes,location,all_day,dtstart_utc,duration_sec,
				tzid,rrule,rdate,exdate,is_task,done_at,seq,field_seqs,updated_at,updated_by_device,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			op.ID, pre.calID, newID("uid_"),
			op.Data.stringOf("title"), op.Data.stringOf("notes"), op.Data.stringOf("location"),
			boolToInt(op.Data.boolOf("all_day")), dtstart,
			numOr0(op.Data, "duration_sec"), orDefault(op.Data.stringOf("tzid"), "Asia/Shanghai"),
			rr, jsonOrEmpty(op.Data["rdate"]), jsonOrEmpty(op.Data["exdate"]),
			boolToInt(op.Data.boolOf("is_task")), doneAt,
			seq, encodeFieldSeqs(fs), now, dev, now); err != nil {
			return mergeResult{}, err
		}
		return mergeResult{}, recomputeInstances(tx, op.ID, time.Now())
	}

	res, err := applyFieldMerge(tx, "event", op.ID, op.Data, op.BaseSeqs, dev)
	if err != nil {
		return res, err
	}
	if err := recomputeInstances(tx, op.ID, time.Now()); err != nil {
		return res, err
	}
	return res, nil
}

func (s *Server) pushSnooze(tx *sql.Tx, uid string, op pushOp, dev string) error {
	if op.Op == "delete" {
		seq, err := nextSeq(tx, "snooze", op.ID, "delete")
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE snoozes SET deleted_at=?, seq=? WHERE id=?`, time.Now().Unix(), seq, op.ID)
		return err
	}
	seq, err := nextSeq(tx, "snooze", op.ID, "upsert")
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		INSERT INTO snoozes(id,event_id,occurrence_key,user_id,remind_at_utc,device_id,seq,deleted_at)
		VALUES(?,?,?,?,?,?,?,NULL)
		ON CONFLICT(id) DO UPDATE SET remind_at_utc=excluded.remind_at_utc, seq=excluded.seq, deleted_at=NULL`,
		op.ID, op.Data.stringOf("event_id"), numOr0(op.Data, "occurrence_key"),
		uid, numOr0(op.Data, "remind_at_utc"), dev, seq)
	return err
}

// ---------- conflicts ----------

func (s *Server) conflictsFor(uid string, includeResolved bool) ([]Conflict, error) {
	q := `SELECT id,entity,entity_id,field,base_seq,server_value,client_value,client_device,created_at,resolved_at,resolution
	      FROM conflicts`
	if !includeResolved {
		q += ` WHERE resolved_at IS NULL`
	}
	q += ` ORDER BY created_at DESC LIMIT 500`
	rows, err := s.db.sql.Query(q)
	if err != nil {
		return nil, err
	}
	raw := []Conflict{}
	for rows.Next() {
		var c Conflict
		var sv, cv, res sql.NullString
		var ra sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Entity, &c.EntityID, &c.Field, &c.BaseSeq, &sv, &cv,
			&c.ClientDevice, &c.CreatedAt, &ra, &res); err != nil {
			continue
		}
		if sv.Valid {
			c.ServerValue = &sv.String
		}
		if cv.Valid {
			c.ClientValue = &cv.String
		}
		if ra.Valid {
			c.ResolvedAt = &ra.Int64
		}
		if res.Valid {
			c.Resolution = &res.String
		}
		raw = append(raw, c)
	}
	// Close before the permission lookups below: the pool holds a single
	// connection, so a nested query while this iterator is open would deadlock.
	rows.Close()

	out := []Conflict{}
	for _, c := range raw {
		switch c.Entity {
		case "event":
			ev, ok := s.eventByID(c.EntityID, "")
			if !ok {
				continue
			}
			if _, ok := s.calendarRole(ev.CalendarID, uid); !ok {
				continue
			}
		case "calendar":
			if _, ok := s.calendarRole(c.EntityID, uid); !ok {
				continue
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Server) handleListConflicts(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	all := r.URL.Query().Get("all") == "1"
	list, err := s.conflictsFor(uid, all)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"conflicts": list})
}

type resolveReq struct {
	Resolution string `json:"resolution"`
}

// handleResolveConflict is where "keep both" lives. Blindly picking a winner
// loses data in the common family case of two people booking the same slot,
// so duplicating the record is a first-class option rather than an afterthought.
func (s *Server) handleResolveConflict(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	id := r.PathValue("id")
	var req resolveReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	if req.Resolution != "keep_mine" && req.Resolution != "keep_server" && req.Resolution != "keep_both" {
		apiErr(w, 400, "resolution 必须是 keep_mine / keep_server / keep_both")
		return
	}

	var c Conflict
	var sv, cv sql.NullString
	err := s.db.sql.QueryRow(`SELECT id,entity,entity_id,field,base_seq,server_value,client_value,client_device,created_at
		FROM conflicts WHERE id=?`, id).Scan(&c.ID, &c.Entity, &c.EntityID, &c.Field, &c.BaseSeq, &sv, &cv, &c.ClientDevice, &c.CreatedAt)
	if err != nil {
		apiErr(w, 404, "冲突不存在")
		return
	}
	if sv.Valid {
		c.ServerValue = &sv.String
	}
	if cv.Valid {
		c.ClientValue = &cv.String
	}

	// Verify the caller may touch the record this conflict is about, and
	// preload it — the pool has a single connection, so nothing may be fetched
	// once the transaction below is open.
	var srcEvent *Event
	switch c.Entity {
	case "event":
		ev, ok := s.eventByID(c.EntityID, "")
		if !ok {
			apiErr(w, 404, "冲突不存在")
			return
		}
		if _, ok := s.calendarRole(ev.CalendarID, uid); !ok {
			apiErr(w, 403, "无权访问")
			return
		}
		srcEvent = ev
	case "calendar":
		if _, ok := s.calendarRole(c.EntityID, uid); !ok {
			apiErr(w, 403, "无权访问")
			return
		}
	}

	tx, err := s.db.sql.Begin()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	switch req.Resolution {
	case "keep_mine":
		var raw any
		_ = json.Unmarshal([]byte(derefStr(c.ClientValue)), &raw)
		if _, err := applyFieldMerge(tx, c.Entity, c.EntityID, jsonMap{c.Field: raw}, nil, "conflict"); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		if c.Entity == "event" {
			_ = recomputeInstances(tx, c.EntityID, time.Now())
		}
	case "keep_both":
		if c.Entity == "event" {
			if err := s.duplicateEvent(tx, c, srcEvent); err != nil {
				apiErr(w, 500, err.Error())
				return
			}
		} else {
			var raw any
			_ = json.Unmarshal([]byte(derefStr(c.ClientValue)), &raw)
			_, _ = applyFieldMerge(tx, c.Entity, c.EntityID, jsonMap{c.Field: raw}, nil, "conflict")
		}
	case "keep_server":
		// nothing to write; the server value already stands
	}

	res := req.Resolution
	if _, err := tx.Exec(`UPDATE conflicts SET resolved_at=?, resolution=? WHERE id=?`, now, res, id); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// duplicateEvent clones an event carrying the client's version of the
// disputed field, so "keep both" yields two real entries. The source event is
// passed in already loaded: looking it up here would query the pool while the
// transaction holds its only connection.
func (s *Server) duplicateEvent(tx *sql.Tx, c Conflict, src *Event) error {
	if src == nil {
		return nil
	}
	newEvent := newID("e_")
	now := time.Now().Unix()
	seq, err := nextSeq(tx, "event", newEvent, "create")
	if err != nil {
		return err
	}
	title := src.Title + "（副本）"
	fs := jsonMap{}
	for f := range eventFieldKinds {
		fs[f] = seq
	}
	if _, err := tx.Exec(`
		INSERT INTO events(id,calendar_id,uid,title,notes,location,all_day,dtstart_utc,duration_sec,
			tzid,rrule,rdate,exdate,is_task,done_at,seq,field_seqs,updated_at,updated_by_device,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		newEvent, src.CalendarID, newID("uid_"), title, src.Notes, src.Location,
		boolToInt(src.AllDay), src.DTStartUTC, src.DurationSec, src.TZID,
		nullableStr(src.RRule), jsonList(src.RDate), jsonList(src.ExDate),
		boolToInt(src.IsTask), src.DoneAt, seq, encodeFieldSeqs(fs), now, "conflict", now); err != nil {
		return err
	}
	// apply the client's version of the disputed field onto the clone
	var raw any
	_ = json.Unmarshal([]byte(derefStr(c.ClientValue)), &raw)
	if _, err := applyFieldMerge(tx, "event", newEvent, jsonMap{c.Field: raw}, nil, "conflict"); err != nil {
		return err
	}
	// carry the reminders over too
	for _, rem := range src.Reminders {
		_, _ = tx.Exec(`INSERT INTO reminders(id,event_id,minutes_before,channel,seq,updated_at) VALUES(?,?,?,?,0,?)`,
			newID("r_"), newEvent, rem.MinutesBefore, rem.Channel, now)
	}
	return recomputeInstances(tx, newEvent, time.Now())
}

func derefStr(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func numOr0(m jsonMap, k string) int64 {
	if v, ok := m.int64Of(k); ok {
		return v
	}
	return 0
}

func jsonOrEmpty(v any) string {
	if v == nil {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ---------- direct instance query (month view beyond the sync window) ----------

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	from, ok1 := atoi64(r.URL.Query().Get("from"))
	to, ok2 := atoi64(r.URL.Query().Get("to"))
	if !ok1 || !ok2 || to <= from {
		apiErr(w, 400, "需要 from / to 时间戳")
		return
	}
	if to-from > 400*86400 {
		apiErr(w, 400, "查询区间过大")
		return
	}
	rows, err := s.db.sql.Query(`
		SELECT DISTINCT i.id FROM instances i
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
	out := []Instance{}
	for _, id := range ids {
		if in, ok := s.instanceByID(id); ok {
			out = append(out, *in)
		}
	}
	writeJSON(w, 200, map[string]any{"instances": out})
}

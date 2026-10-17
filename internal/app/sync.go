package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
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
			// ⚠️ 2026-10-17 改（第十轮审计 N2）：**不能静默 `continue`**。
			//
			//    原来这个字段没写进去，却**没有任何记录**：没有冲突行、没有错误回报，
			//    而 op 照样计入 `applied` —— 客户端以为"写成功了"。
			//    用户看到的是"改了但没生效"，而服务端连一行日志都没有。
			//    这正是本项目反复声明的头号反模式（"不许静默当成功"）。
			//
			//    可达性：官方 Kotlin 客户端的类型纪律下走不到这里（Long 直传），
			//    只有构造/损坏的客户端能触发 —— 但"静默"本身就是问题：
			//    它是"出事了查不出来"的写法，不该留在代码里。
			return res, errors.New("字段「" + f + "」的值格式不对（" + err.Error() + "）")
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
	// Inbox 收件箱（2026-09-19 新增，per-user）。
	//
	// ⚠️ **向后兼容的关键**：这是**新增的 JSON 键**，不是改已有键。
	// 服务端会先于客户端上线，届时一定有旧客户端在跑 ——
	// 旧客户端只读它认识的键，多出来一个 `inbox` 不影响它。
	// **不要**把 inbox 混进 events/instances，那才真会让旧客户端解析出错误数据。
	// （各端仍需确认 `parsePayload` 遇到未知 entity 是"忽略"而非"整包丢弃"。）
	Inbox []InboxItem `json:"inbox"`
	Users []User      `json:"users,omitempty"`
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

	// ★ 2026-09-28：把 `maintenance`（整库修复）这个信号**显式下发**。
	//
	// 原来只是往 sync_log 里写一条 entity='maintenance' 的记录，
	// 指望客户端"看到不认识的 entity 就整份重拉" —— 但 `loadEntities` 的
	// switch 根本不认识它，于是这条记录**在响应里什么都不产生**，
	// 客户端只看到 last_seq 往前走了一点点，**永远不知道库被修过**。
	//
	// 这就是用户报的「那个不存在日历的日程没有删干净，或者说没有正常同步」：
	// 服务端当时确实修好了，但没有任何一个客户端收到"请重拉"的通知。
	//
	// 现在明确给出 `"maintenance": true`，客户端收到就**丢弃本地游标、
	// 下一轮做一次全量快照**（快照是"以服务端为准"的替换，能洗掉本地残渣）。
	maintenance := false
	filtered := make([]changeRef, 0, len(refs))
	for _, ref := range refs {
		if ref.entity == "maintenance" {
			maintenance = true
			continue
		}
		filtered = append(filtered, ref)
	}
	refs = filtered

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
		"inbox":     payload.Inbox,
		"last_seq":  last,
		"has_more":  hasMore,
		// 客户端据此决定"这轮之后要整份重拉"
		"maintenance": maintenance,
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
		"inbox":     payload.Inbox,
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

// tombstoneEvent / tombstoneInstance 造一条"只带身份、不带内容"的墓碑。
//
// ★ 2026-09-28 新增，解决"换日历之后，原日历的人本地永远清不掉"。
//
// 背景：客户端的同步是**按 seq 增量 upsert**，从不自己判断删除 —— 它只认
// 服务端下发的 deleted_at。"日程从日历 1 挪到日历 2"对**留在日历 1 的人**
// 来说，等价于"这条我没资格看了"；而 loadEntities 原来的写法是直接
// `continue`，一个字节都不下发 → 他们本地那条**永远不会消失**，界面上
// 留下一个点不开、也删不掉的幽灵条目。
//
// ⚠️ 客户端的 pruneOrphans() 救不了这个场景（本以为它可以）：本地残留行的
// `calendar_id` 仍是**日历 1 —— 一个完全合法、仍在集合里的日历**，
// "不在我的日历集合里"这个清理条件根本不成立。只有服务端主动说
// "这条没了"才行。
//
// ⚠️ 墓碑里**只许有 id / calendar_id / seq / deleted_at + 一个空 reminders**，
// 绝不能捎带 title、notes、location 这些内容字段：它是发给"已经无权看这条
// 日程的人"的，带上内容就等于顺着同步流把私有日程原文泄露出去。
//
// 客户端那边是 CONFLICT_REPLACE 写入，deleted_at 非空 → 所有查询
// （都带 `deleted_at IS NULL`）都看不到它，正好达到"删掉"的效果。
func tombstoneEvent(ev Event) Event {
	deleted := ev.UpdatedAt
	if deleted == 0 {
		deleted = time.Now().Unix()
	}
	return Event{
		ID:         ev.ID,
		CalendarID: ev.CalendarID,
		// 空数组而不是 nil：reminders 没有 omitempty，nil 会序列化成
		// `"reminders": null`，客户端解析 null 的路径没必要去趟。
		Reminders: []Reminder{},
		DeletedAt: &deleted,
		Seq:       ev.Seq,
	}
}

func tombstoneInstance(in Instance) Instance {
	deleted := time.Now().Unix()
	return Instance{
		ID:            in.ID,
		EventID:       in.EventID,
		OccurrenceKey: in.OccurrenceKey,
		CalendarID:    in.CalendarID,
		DeletedAt:     &deleted,
		Seq:           in.Seq,
	}
}

// loadEntities hydrates the change references, dropping anything the caller
// may not see. The cursor still advances past those, otherwise one member
// leaving a calendar would wedge everyone else's sync forever.
//
// ⚠️ 但"丢弃"只对**从来看不见**的东西成立。对"本来能看、现在看不见"的
// （换日历 / 被移出成员 / 日历被删），必须下发墓碑而不是 continue ——
// 原因见 tombstoneEvent 上面那段。
func (s *Server) loadEntities(uid string, refs []changeRef) (*syncPayload, error) {
	p := &syncPayload{Calendars: []Calendar{}, Events: []Event{}, Instances: []Instance{}, Snoozes: []Snooze{}, Conflicts: []Conflict{}, Inbox: []InboxItem{}}
	seenCal := map[string]bool{}
	seenEv := map[string]bool{}
	seenInbox := map[string]bool{}

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
				// ★ 从"能看"变成"不能看"（最典型的就是换日历）→ 下发墓碑。
				// 这里不能用 continue，否则客户端那份永远删不掉。
				p.Events = append(p.Events, tombstoneEvent(*ev))
				continue
			}
			p.Events = append(p.Events, *ev)
		case "instance":
			inst, ok := s.instanceByID(ref.id)
			if !ok {
				continue
			}
			if _, ok := s.calendarRole(inst.CalendarID, uid); !ok {
				p.Instances = append(p.Instances, tombstoneInstance(*inst))
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
		case "inbox":
			// ★ 越权过滤就靠这一句。
			//
			// 收件箱是**唯一不按日历权限**判定的实体：可见性 = 这行归不归我。
			// 所以这里**绝不能**用 canRead/calendarRole —— 那套是按日历算的，
			// 而一条 share 收件箱的 origin_calendar_id 可能指向一张我根本没权限的日历
			// （发件人的日历）。用日历权限去滤，结果是"要么看不到该看的、
			// 要么把发件人日历的内容漏给不该看的人"。
			//
			// 闸门 scripts/inbox-gate.cjs 里有专门的越权用例：
			// A 的 token 拉不到 B 的任何一行。
			if seenInbox[ref.id] {
				continue
			}
			seenInbox[ref.id] = true
			it, ok := s.inboxByID(ref.id)
			if !ok {
				continue
			}
			if it.UserID != uid {
				continue
			}
			p.Inbox = append(p.Inbox, *it)
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
	// banned 表示"内容含违禁词，被拦下"。与 denied 分开是因为**错误文案不同**：
	// denied = "没权限"，banned = "内容违规"。混成一个错误码的话，用户看到
	// "forbidden" 会以为是自己没权限，去翻权限设置，永远找不到真正的原因。
	// 另外 banned 要能带上"中了哪个词"，所以 extra 一起传。
	banned    bool
	bannedHit *WordHit
	bannedFld string
	// aiRisk 表示"命中的是风险词（severity=3），需要 AI 复核才能定性"。
	//
	// ⚠️ 它**不是**一个"已拒绝"标记 —— preflight 只负责标出来，
	// AI 调用由 handleSyncPush 在真正要决断时做一次（理由见 preflight 的注释）。
	// 单独一个字段而不是复用 banned：两者的处理时机、错误码、文案都不一样，
	// 混在一起就会出现"到底调没调过 AI"这种要靠读代码才说得清的问题。
	aiRisk  bool
	riskHit *WordHit
	riskFld string
}

// pushOpFields 取出**本次要写入**的文本字段（字段名 → 内容）。
//
// ★ 抽成独立函数是有意的：词表匹配（逐字段）与 AI 送审（拼成一段）
// 必须看**同一份字段清单**。以前这段 switch 内联在 moderatePushOp 里，
// 加 AI 那一路时如果各抄一份，将来新增实体/字段必然只改一处 ——
// 而漏掉的那一路是"静默不检查"，没有任何报错。
//
// 语义是 patch：只有 op.Data 里出现的字段才算本次写入。
// 日历名同理（不传 name 表示不改名）。
func pushOpFields(op pushOp) map[string]string {
	fields := map[string]string{}
	switch op.Entity {
	case "event":
		if v, ok := op.Data["title"]; ok {
			fields["标题"] = anyToString(v)
		}
		if v, ok := op.Data["notes"]; ok {
			fields["备注"] = anyToString(v)
		}
		if v, ok := op.Data["location"]; ok {
			fields["地点"] = anyToString(v)
		}
	case "calendar":
		if v, ok := op.Data["name"]; ok {
			fields["日历名称"] = anyToString(v)
		}
	case "inbox":
		// 收件箱同样要查：它是"AI 提案 → 加入日历"的中转，也是第二条进内容的路径
		if v, ok := op.Data["title"]; ok {
			fields["标题"] = anyToString(v)
		}
		if v, ok := op.Data["notes"]; ok {
			fields["备注"] = anyToString(v)
		}
	}
	return fields
}

// moderatePushOp 检查一次同步推送的内容是否含违禁词。
//
// ★ 为什么必须在**同步通道**上拦：客户端（尤其安卓/手表）日常改日程走的是
// POST /api/sync/push，而不是 REST 的 /api/events。只在 REST 那几处加闸门，
// 等于**主路径根本没审核**，那些客户端可以随便写违规内容。
//
// 这里的看点是"这次要写入哪些字段"：只有 op.Data 里出现的字段才检查
// （patch 语义 = 没出现的字段不动）。日历名同理。
//
// ⚠️ 返回的 WordHit 可能是 sevBlock 也可能是 sevRisk —— 这是**调用方**
// （preflight）按 severity 分流的事，本函数不替它决定。
func (s *Server) moderatePushOp(op pushOp) (*WordHit, string) {
	if s.words == nil || op.Op == "delete" {
		return nil, ""
	}
	fields := pushOpFields(op)
	if len(fields) == 0 {
		return nil, ""
	}
	return s.moderateText(fields)
}

// pushOpText 把一条 pushOp 里**本次要写入**的文本字段拼成送审文本。
//
// 与 moderatePushOp 的区别：那个是"逐字段单独查词表"（词表匹配是以字段为
// 单位的），这个是"合起来给 AI 看"（AI 需要上下文才能判语义）。
// 两件事用同一份字段清单是对的 —— 抄一份新的必然有一天漏了某个字段。
func pushOpText(op pushOp) string {
	fields := pushOpFields(op)
	if len(fields) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, k := range sortedFieldKeys(fields) {
		v := strings.TrimSpace(fields[k])
		if v == "" {
			continue
		}
		sb.WriteString(k + "：" + v + "\n")
	}
	return sb.String()
}

// gateAIRisk 对一条命中了风险词的 pushOp 做 AI 复核。
//
// 返回 (给人看的理由, 是否拦下)。**语义与 moderation.go 的 gateByAI 对齐**：
// AI 判违规才拦，其余所有情况（判合规 / 没启用 / 超时 / 出错）一律放行。
//
// ⚠️ 这里同样要写审计 —— 而且是**判合规的更要写**：
// 风险词这一档的意义就是"我不确定，交给 AI"，那 AI 放过去的那些
// 正是最该被人工复查的。
//
// ⚠️ 2026-10-17 加 `within`（第四轮审计 L1）：**同一批 push 里相同内容只送审一次**。
// 一次 push 最多 500 条 op，每条命中风险词的都走一次 AI 复核（默认超时 8s、
// 上限 30s）—— 而"每条 op 各带一个相同的风险词"是完全构得出来的（比如批量改备注）。
// 缓存由调用方（`handleSyncPush`）持有，**生命周期就是这一次请求**：
// 不放全局是因为那要配 TTL、要防内存增长，而这批 op 本来就是一起进来的，
// 请求内去重已经把"单次 500 次调用"压成"不同文本数那么多次"。
//
// ⚠️ 有意的取舍：**审计照写**（缓存命中也写一行），
// 否则"这批里第 2~500 条也命中了"这件事在审计里就看不见了。
func (s *Server) gateAIRisk(uid string, op pushOp, pre opPre, within map[string]*aiVerdict) (string, bool) {
	text := pushOpText(op)
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	hits := []WordHit{}
	if pre.riskHit != nil {
		hits = append(hits, *pre.riskHit)
	}

	// 同步通道没有 http.Request 的 context（这里是事务中间），
	// 用 Background + aiReviewText 内部的超时控制。这与 REST 路径的
	// 超时语义一致：都是 aiConfig.Timeout 说了算。
	var verdict *aiVerdict
	key := ""
	if within != nil {
		key = text
		verdict = within[key]
	}
	if verdict == nil {
		verdict = s.aiReviewText(context.Background(), text, hits)
		if within != nil {
			within[key] = verdict
		}
	}
	s.logModerationAudit(uid, s.userNameOf(uid), pre.riskFld, text, hits, verdict)

	if !verdict.violated() {
		return "", false
	}
	w := ""
	if pre.riskHit != nil {
		w = pre.riskHit.Raw
	}
	reason := "「" + pre.riskFld + "」被判定为违规内容（" +
		wordCategoryLabel(verdict.Category) + "）"
	if w != "" {
		reason = "「" + pre.riskFld + "」含风险词「" + w + "」，且" +
			"被判定为违规内容（" + wordCategoryLabel(verdict.Category) + "）"
	}
	if verdict.Reason != "" {
		reason += "。理由：" + verdict.Reason
	}
	return reason, true
}

// anyToString 把 jsonMap 里的值按字符串处理。非字符串（nil/数字/布尔）一律给空串 ——
// 内容检查只关心文本字段，别的类型不该被当成文本去匹配词库。
func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return ""
	}
}

// preflight 检查一次推送该不该被允许。
//
// ⚠️ 违禁词也在这里拦（见下面每个 case 开头的判定）。放在 preflight 而不是
// 各个 pushXxx 里的理由：**preflight 是所有实体的必经之路**，新加实体时
// 只要它还在 preflight 里判权限，审核就自动跟着生效 —— 少一个"忘了加"的坑。
//
// ⚠️ 三档 severity 在这里的处理（2026-09-23 加第 3 档后）：
//   - severity 2 直接拦（banned）
//   - severity 3 风险词 → 转 AI 复核（aiRisk），**判定不在这里做**
//   - severity 1 放行
//
// 为什么把 AI 调用留给**调用方**做，而不是就地在这里调：
// preflight 是**纯函数语义**（只看数据、不改状态、不做 IO），
// 它会在一次 push 里被反复调用（"先探一遍、再逐条推"就是两次）。
// 在这里发起网络请求会让同一条内容被送审两次 —— 多花钱、还让审计表里
// 出现成对的重复记录。所以它只**标出"这条需要 AI 看"**，
// 由 handleSyncPush 在真正要拦的那一刻调一次（见那边的 gateAIRisk）。
func (s *Server) preflight(uid string, op pushOp) opPre {
	// 违禁词对所有"写"操作生效。放在 switch 之前，覆盖全部实体。
	if hit, fld := s.moderatePushOp(op); hit != nil {
		switch {
		case hit.Severity >= sevBlock:
			return opPre{banned: true, bannedHit: hit, bannedFld: fld}
		case hit.Severity >= sevRisk:
			return opPre{aiRisk: true, riskHit: hit, riskFld: fld}
		}
	}
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

	case "inbox":
		// 收件箱**不看日历权限**，只看归属（见 loadEntities 里那段注释）。
		//
		// 规则：
		//   · 已有行 → 必须归我，否则拒绝
		//   · 新行   → user_id 要么留空（服务端填成我），要么就是我；填别人一律拒绝
		//
		// 这一条同时挡住了两种越权：
		// ① 直接改/删别人的收件箱条目；
		// ② 用 capture 通道往别人收件箱里塞东西
		//    （契约里 capture 的语义是"本机抓取同步上来"，不是"给别人发"）。
		if it, ok := s.inboxByID(op.ID); ok {
			if it.UserID != uid {
				return opPre{denied: true}
			}
			return opPre{}
		}
		if op.Op == "delete" {
			// 行不存在，删它没有意义，也不算越权
			return opPre{notFound: true}
		}
		if owner := op.Data.stringOf("user_id"); owner != "" && owner != uid {
			return opPre{denied: true}
		}
		return opPre{notFound: true}
	}
	return opPre{denied: true}
}

// push 限流的状态与判据（2026-10-17 加，第四轮审计 L1）。
//
// 每用户维护"最近一分钟内的推送时间戳"。判据见 handleSyncPush 里那段说明。
var (
	pushMu     sync.Mutex
	pushStamps = map[string][]int64{}
)

const (
	pushWindowSec = 60
	pushLimit     = 60
)

func (s *Server) pushAllowed(uid string) bool {
	now := time.Now().Unix()
	pushMu.Lock()
	defer pushMu.Unlock()
	// 窗口内的留下，过期的丢掉
	list := pushStamps[uid]
	keep := list[:0]
	for _, t := range list {
		if now-t < pushWindowSec {
			keep = append(keep, t)
		}
	}
	if len(keep) >= pushLimit {
		pushStamps[uid] = keep
		return false
	}
	pushStamps[uid] = append(keep, now)
	return true
}

func (s *Server) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var req pushReq
	if err := readJSON(r, &req); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	// ⚠️ 2026-10-16 加（第三轮审计 H2）：一次 push 的 op 条数原来**没有上限**。
	//
	//    4MB 请求体按最小 op 算能塞约 4 万条，而每条 op 是**独立事务串行执行**的；
	//    真正贵的是每条 op 的 title 各带一个 sev=3 风险词就会触发一次 AI 复核
	//    （默认超时 8s、上限 30s）—— 一次请求可以放大成 4 万次 AI 调用，
	//    或者把请求拖上几小时。而 push 路由本身没有任何限流。
	//
	//    取 500：正常客户端一次 push 推的是"本机攼下的离线操作"，几十到几百条。
	const maxPushOps = 500
	if len(req.Ops) > maxPushOps {
		apiErr(w, 400, "一次最多推 "+itoa(int64(maxPushOps))+" 条操作，这次是 "+
			itoa(int64(len(req.Ops)))+" 条 —— 客户端分几批推")
		return
	}
	// ⚠️ 2026-10-17 加（第四轮审计 L1）：**每用户每分钟的 push 次数上限**。
	//
	//    上一轮做的"同批去重"只解决了**一次请求内**的放大（同一批 500 条里
	//    同样的文本只送审一次）。**跨请求没有限制** —— 一个写坏的客户端
	//    （或攻击者）可以连续发 500 条一批、每秒一批，每条带一个不同的 risk 词，
	//    就是每分钟几万次 AI 复核。
	//
	//    阈值取得很宽（60 次/分钟）：正常客户端是"攒一批离线操作推一次"，
	//    一分钟 60 次意味着**每秒都在推**，那已经不正常了，不是误伤。
	//    ⚠️ 内存计数、重启清零 —— 对"限流"这个目的够用（它挡的是持续滥用，
	//       不是精确计费）。单实例部署，不需要跨实例一致。
	if !s.pushAllowed(uid) {
		apiErr(w, 429, "推得太频繁了，稍等一下再试")
		return
	}
	applied := []map[string]any{}
	conflicts := []Conflict{}
	errs := []string{}
	// 本批 push 的 AI 送审去重表（第四轮审计 L1）—— 见 gateAIRisk 的注释。
	// 生命周期就是这一次请求。
	aiSeen := map[string]*aiVerdict{}

	// One transaction per op rather than one per batch: the preflight for op N
	// must observe the commit of op N-1 (a batch legitimately creates an
	// entity and then patches it).
	for _, op := range req.Ops {
		// ⚠️ 2026-10-16 加（第三轮审计 M1）：push 路径**必须**带 field_base_seqs。
		//
		//    applyFieldMerge 里的 `force := baseSeqs == nil` 是给**冲突解决**用的
		//    （用户在界面上选完了，"这次写入是权威的"）。push 传 nil 会走同一条分支——
		//    于是"省略这个键"＝无条件覆盖服务端上更新的值，而且**不产生冲突记录**。
		//    它与合法的 force 路径没有任何区分。
		//
		//    REST 那边确实有故意不传的（改日历名，见 AppVm 的注释），所以这道检查
		//    只加在 push 上：push 的调用方（安卓 SyncEngine / 电脑端 sync.js）恒发这个键。
		if op.BaseSeqs == nil {
			errs = append(errs, op.Entity+" "+op.ID+": missing field_base_seqs")
			continue
		}
		dev := op.DeviceID
		if dev == "" {
			dev = req.DeviceID
		}
		pre := s.preflight(uid, op)
		if pre.denied {
			errs = append(errs, op.Entity+" "+op.ID+": forbidden")
			continue
		}
		if pre.banned {
			// 违禁词：给一条**能读懂**的错误，并说清是哪个字段、哪个词。
			// 与 forbidden 分开报，否则用户会去查权限设置（见 opPre.banned 的注释）。
			//
			// ⚠️ 这里只是**拒绝这一条 op**，不是拒绝整个批次 ——
			// 一个批次里可能同时有"改个时间"和"改标题"，前者合法，不该被后者连坐。
			w := ""
			label := ""
			if pre.bannedHit != nil {
				w = pre.bannedHit.Raw
				label = wordCategoryLabel(pre.bannedHit.Category)
			}
			errs = append(errs, op.Entity+" "+op.ID+": banned_word:「"+
				pre.bannedFld+"」含违规词「"+w+"」（"+label+"）")
			continue
		}

		// ---- 风险词（severity=3）：这里才真正调 AI ----
		//
		// 为什么在这一层调而不是在 preflight 里：preflight 是纯函数语义，
		// 会被反复求值（探一遍 + 推一遍），在那里发起网络请求等于对同一条
		// 内容送审两次 —— 多花钱，还会在审计表里留下成对的重复记录。
		// 这里已经是"决定这条 op 走不走"的唯一位置，一次一条，干净。
		//
		// ⚠️ 与 REST 路径（moderation.go 的 gateByAI）语义必须一致：
		// AI 判违规 → 拦这一条 op；AI 不可用/超时 → fail-open 放行。
		if pre.aiRisk {
			if reason, blocked := s.gateAIRisk(uid, op, pre, aiSeen); blocked {
				errs = append(errs, op.Entity+" "+op.ID+": banned_word:"+reason)
				continue
			}
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
		case "inbox":
			// 收件箱不做字段级合并（per-user 私有行，没有并发改同一条的场景），
			// 但**必须占 sync_log 的 seq**，否则别的设备永远拉不到它。
			err = s.pushInboxItem(tx, uid, op, pre)
			if err == nil {
				applied = append(applied, map[string]any{"entity": "inbox", "id": op.ID})
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
		// ⚠️ 2026-10-16 补（第三轮审计 M3）：REST 创建有 saneStartUTC 闸门，
		//    push 这条创建路径没有 —— 构造一个 op 就能把窗口外的"幽灵日程"落库。
		//    最典型的是把**毫秒时间戳当成秒**写进来（1.7e12 远超 2100 年），
		//    也就是 event.go 那段注释描述过的 AI 事故形态；落库之后客户端的
		//    闹钟和列表会错乱。两条创建路径必须同一套判据。
		durIn := numOr0(op.Data, "duration_sec")
		if v, ok := op.Data.int64Of("dtstart_utc"); ok {
			if why := saneStartUTC(v, durIn); why != "" {
				return mergeResult{}, errors.New(why)
			}
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
			rr, jsonArrayOrEmpty(op.Data["rdate"]), jsonArrayOrEmpty(op.Data["exdate"]),
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
	var carry conflictCarry
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
		// ⚠️ 提醒 / 表态 / 强提醒要在这里（事务外）单独读齐。
		//    ev.Reminders / ev.Strong 是**按 viewerID 过滤**的，这里传的是空串，
		//    两个都会直接返回空集 —— 拿它们去复制等于没复制。详见 conflictCarry 的注释。
		carry = s.carryForCopy(c.EntityID)
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
			if err := s.duplicateEvent(tx, c, srcEvent, carry); err != nil {
				apiErr(w, 500, err.Error())
				return
			}
		} else {
			var raw any
			_ = json.Unmarshal([]byte(derefStr(c.ClientValue)), &raw)
			// ⚠️ 这里原来是 `_, _ = applyFieldMerge(…)`：合并失败一声不吭，
			//    而下面照旧把这条冲突标成 resolved 并回 200 ——「保留两边」点了等于没点。
			if _, err := applyFieldMerge(tx, c.Entity, c.EntityID, jsonMap{c.Field: raw}, nil, "conflict"); err != nil {
				apiErr(w, 500, err.Error())
				return
			}
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
//
// 提醒 / 表态 / 强提醒同理，也在事务外读好、随 carry 传进来 ——
// ⚠️ 别改用 `src.Reminders`：那个字段是按 viewerID 过滤的，而这里的调用点传的是空串，
//    它**恒为空集**（详见 conflictCarry 的注释）。
func (s *Server) duplicateEvent(tx *sql.Tx, c Conflict, src *Event, carry conflictCarry) error {
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
	// carry the reminders over too —— 三张按人的表都要**连 user_id 一起搬**，
	// 少任何一张，副本上的提醒对人都不会生效（见 conflictCarry 的注释）。
	for _, rem := range carry.Reminders {
		if _, err := tx.Exec(`INSERT INTO reminders(id,event_id,user_id,minutes_before,channel,seq,updated_at)
			VALUES(?,?,?,?,?,0,?)`,
			newID("r_"), newEvent, rem.UserID, rem.MinutesBefore, rem.Channel, now); err != nil {
			return err
		}
	}
	for _, uid := range carry.Prefs {
		if _, err := tx.Exec(`INSERT INTO reminder_prefs(event_id,user_id,updated_at) VALUES(?,?,?)
			ON CONFLICT(event_id,user_id) DO NOTHING`, newEvent, uid, now); err != nil {
			return err
		}
	}
	for _, sc := range carry.Strong {
		if _, err := tx.Exec(`INSERT INTO strong_reminders(event_id,user_id,on_off,updated_at) VALUES(?,?,?,?)
			ON CONFLICT(event_id,user_id) DO NOTHING`, newEvent, sc.UserID, boolToInt(sc.OnOff), now); err != nil {
			return err
		}
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

// jsonArrayOrEmpty 把客户端给的 rdate / exdate 归一成 JSON 数组文本。
//
// ⚠️ 2026-10-16 改（第三轮审计 L10）。原来只判 `nil` 就直接 marshal，于是客户端
//    传一个**字符串**（`"not-json"`）会被 marshal 成 `"\"not-json\""` 落进列里 ——
//    而 `recomputeInstances` 解析它必然失败、然后**静默清空**这个字段。
//    表现是"我明明设过例外日期，怎么没了"，而服务端连一行日志都没有。
//
//    客户端写错不该污染数据列 —— 现在**不是数组就当成空数组**。
//    这是这两列唯一的写入路径（grep 过），所以改在这里就够了。
func jsonArrayOrEmpty(v any) string {
	if v == nil {
		return "[]"
	}
	if _, ok := v.([]any); !ok {
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
	// 单次查询的实例条数上限。
	//
	// ⚠️ 2026-10-16 加（第三轮审计 M2）。原来只有"跨度 ≤400 天"这一条限制，
	//    **条数**没有上限：跨几个月的日历 + 一条高频重复日程就能让一次
	//    `GET /api/instances` 把几十万条（每条带 title/notes）序列化进响应，
	//    客户端 `readText()` 全量读进内存建 JSON DOM → **客户端 OOM**。
	//    （H1 给单条日程的展开加了 2000 条上限，但"很多条日程"叠加起来仍然会大，
	//     所以这里需要独立的一道。）
	//
	//    超了就**明确报错**而不是静默截断 —— 截断会让用户看到"少了几天"，
	//    而那比报错更难查（他会以为是数据丢了）。
	const maxInstanceRows = 20000
	rows, err := s.db.sql.Query(`
		SELECT DISTINCT i.id FROM instances i
		JOIN calendars c ON c.id = i.calendar_id
		WHERE i.deleted_at IS NULL AND i.start_utc >= ? AND i.start_utc <= ?
		  AND c.deleted_at IS NULL
		  AND (c.owner_id = ? OR EXISTS(SELECT 1 FROM calendar_members m WHERE m.calendar_id=c.id AND m.user_id=?))
		ORDER BY i.start_utc
		LIMIT ?`, from, to, uid, uid, maxInstanceRows+1)
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
	if len(ids) > maxInstanceRows {
		apiErr(w, 400, "这个时间范围里的日程太多了（超过 "+itoa(int64(maxInstanceRows))+
			" 条），把范围缩小一点再查（比如按月看）")
		return
	}
	out := []Instance{}
	for _, id := range ids {
		if in, ok := s.instanceByID(id); ok {
			out = append(out, *in)
		}
	}
	writeJSON(w, 200, map[string]any{"instances": out})
}

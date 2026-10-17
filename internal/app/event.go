package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// pendingEventFields 算出"这次 PUT 之后，这条日程的这几个字段会是什么"。
//
// 做法：以库里现有的值为底，用本次请求里**非 nil** 的字段覆盖。
// 只有参与了才会被检查 —— 没传的字段保持原值，如果原值合规就永远不会被拦，
// 这正是我们想要的（改时间不该因为旧标题而被拦）。
//
// ⚠️ 字段清单与 applyFieldMerge 的合并范围必须一致。这里直接复用
// req.incomingMap() 的键，而不是另抄一份 —— 抄一份的下场是"以后新增了可改
// 字段，拦截范围没跟着扩"，那是一个静默的绕过口子。
func (s *Server) pendingEventFields(ev *Event, req *eventReq) map[string]string {
	// 先把库里的值铺上，再用请求覆盖
	cur := map[string]string{
		"title":    ev.Title,
		"notes":    ev.Notes,
		"location": ev.Location,
	}
	in := req.incomingMap()
	out := map[string]string{}
	for field, label := range map[string]string{
		"title": "标题", "notes": "备注", "location": "地点",
	} {
		if _, touched := in[field]; !touched {
			continue // 本次没改这一栏 → 不检查（避免拿旧内容误拦）
		}
		// 改了这一栏：取本次要给的值；给的是空串就是"要清空"，清空当然合规
		v := cur[field]
		if sv, ok := in[field].(string); ok {
			v = sv
		}
		if strings.TrimSpace(v) != "" {
			out[label] = v
		}
	}
	return out
}

func (req *eventReq) incomingMap() jsonMap {
	m := jsonMap{}
	// ★ 换日历。空串是"没传"，不是"挪到不存在的日历" —— 一条日程**必须**
	// 属于某张日历（calendar_id 在外键意义上是必填的），所以只接受非空值。
	//
	// 这里同时也让 POST /api/events 的 field_seqs 带上 calendar_id：
	// 新建时它就是权威基线，后续第一次移动才不会莫名其妙报冲突。
	if req.CalendarID != "" {
		m["calendar_id"] = req.CalendarID
	}
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

// ---------------------------------------------------------------------------
// 时间合理性闸门（2026-10-08）
// ---------------------------------------------------------------------------
//
// 用户报的原话：「AI 写的那个日期有问题……好像写到了 1970 年。我要你不仅提升
// AI 的能力，并且要做一下我们代码的健壮性，就是**通过程序兜底**来避免这种事」。
//
// 根因在客户端（把 epoch **秒**又除了 1000），那边已经修了。但服务端作为
// **最后一道**必须自己也能拦住 —— 五端都在往这里写，任何一端的单位算错，
// 落下来就是一条谁也看不见的脏数据：
//
//	dtstart 落在实例时间窗口（今天-7 天 ~ 今天+90 天）之外
//	  → 日程列表里**看不见**（界面读的是 instances）
//	  → 但查重读的是 events **全表** → "已经有一条同名日程了"
//	  → 又因为查/删都按时间窗口找 → 拿不到它的 id，**删不掉**
//
// 所以这里**只做物理上不可能的判断**（区间外的年份、end 早于 start、
// 时长超过 366 天），不掺任何产品规则 —— 补录去年的课、记一个生日
// 都是用户的正当意愿，那些归客户端界面管。
//
// ⚠️ 数值必须和安卓端 `data/EventTime.kt` 里的常量**保持一致**，改一起改
//
//	（那边有同样一段说明，两边互相指着）。
const (
	// 2000-01-01 00:00:00 UTC
	eventMinStartUTC = 946684800
	// 2100-01-01 00:00:00 UTC
	eventMaxStartUTC = 4102444800
	// 单条日程最长 366 天。它不是产品限制，而是**单位错的照妖镜**：
	// dtstart 是毫秒而 duration 是秒（或反过来）时，算出来的时长会离谱地大。
	eventMaxDurationSec = 366 * 86400
)

// saneStartUTC 判断一对「开始时间 + 时长」是否合理到可以落库。
// 返回空串 = 通过，否则是给客户端看的原因。
func saneStartUTC(startUTC int64, durationSec int64) string {
	if startUTC < eventMinStartUTC || startUTC >= eventMaxStartUTC {
		return fmt.Sprintf(
			"开始时间不在合理范围（2000~2100 年），收到的是 %d —— 多半是把时间戳当成了日期",
			startUTC)
	}
	if durationSec < 0 {
		return "日程时长不能是负数"
	}
	if durationSec > eventMaxDurationSec {
		return fmt.Sprintf("单条日程的时长超过了 366 天（%d 天）", durationSec/86400)
	}
	return ""
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
	// ★ 时间闸门：和「缺少开始时间」同属**参数校验**，所以放在违禁词闸门之前
	//   （违禁词那一步要读内容，更"贵"，也没必要为一条坏时间跑一遍）。
	durIn := int64(0)
	if req.DurationSec != nil {
		durIn = *req.DurationSec
	}
	if why := saneStartUTC(*req.DTStartUTC, durIn); why != "" {
		apiErr(w, 400, why)
		return
	}
	// 违禁词闸门：放在权限与参数校验**之后**、写库**之前**。
	// 顺序有意为之 —— 没权限的人不该通过"报错文案"探到这部日历的内容规则。
	if s.blockedByWords(w, r, map[string]string{
		"标题": strOrEmpty(req.Title),
		"备注": strOrEmpty(req.Notes),
		"地点": strOrEmpty(req.Location),
	}) {
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
	// 时长复用上面闸门里已经算好的那份，别算两遍（两处一旦写岔就是隐患）
	dur := durIn
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

	// ---- 换日历：目标日历**也要**有写权限 ----
	//
	// 上面那句 `canWrite(ev.CalendarID)` 只回答"你能不能动这条日程"，
	// 回答不了"你能不能往那张日历里塞东西"。少了这一句，就是把日程
	// **塞进自己没有写权限的日历**（只读的共享日历、别人的私人日历）——
	// 一旦落地，那条日程就变成"看得见、改不动、删不掉"，比移动失败更糟。
	//
	// 目标日历不存在时 canWrite 一律 false，所以不需要额外判存在性。
	// 目标 = 原日历时不校验（那是"原地保存"，上面已经放行过）。
	if req.CalendarID != "" && req.CalendarID != ev.CalendarID {
		if !s.canWrite(req.CalendarID, uid) {
			apiErr(w, 403, "目标日历你没有编辑权限")
			return
		}
	}

	// ★ 时间闸门（2026-10-08）。校验的是**合并之后**的最终值，不是只看本次传入 ——
	//   PUT 是局部合并（只传 title 就只改 title），只看传入值会漏掉
	//   "库里本来就是坏的，这次又被原样写回去"这种情况。
	//
	// ⚠️ 因此这条也意味着：**一条时间已经坏掉的日程不能再通过 PUT 改任何字段**
	//    （改标题也会被拒）。这是**有意**的 —— 它的正确归宿是**删掉**，
	//    客户端的「设置 → 数据维护 → 清理时间异常的日程」就是干这个的，
	//    报错文案里也直接指了那条路，免得用户对着一条改不动的日程干瞪眼。
	effStart := ev.DTStartUTC
	if req.DTStartUTC != nil {
		effStart = *req.DTStartUTC
	}
	effDur := ev.DurationSec
	if req.DurationSec != nil {
		effDur = *req.DurationSec
	}
	if why := saneStartUTC(effStart, effDur); why != "" {
		apiErr(w, 400, why+"。如果这条日程的时间本来就是坏的，"+
			"请在「设置 → 数据维护 → 清理时间异常的日程」里删掉它，不要再改它。")
		return
	}

	// 违禁词闸门：检查**合并后的最终值**，而不是只看本次传入的字段。
	//
	// ⚠️ 这一步不能省。PUT 是局部合并（只传 title 就只改 title），如果只检查
	// 传入的键，绕过路径是：先建一条标题干净的日程，再单独 PUT 改 title ——
	// 一样落地。反过来只检查传入值还会**误伤**：用户只想改时间、没传 title，
	// 若把库里已有的内容当"新写入"查，本来合规的老内容也会被拦。
	//
	// 所以：以库里现有值为底，叠加本次传入的非 nil 字段，检查这个结果集。
	if s.blockedByWords(w, r, s.pendingEventFields(ev, &req)) {
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
		// ★ 2026-09-28 放宽：「日程挂在**已经不存在的日历**上」要能删掉。
		//
		// 用户报的原话：「那个不存在日历的日程没有删干净……网页端和手机端
		// 还是有残留，而且这两端好像没有对应的方法删除」—— 后半句就是这里。
		//
		// 根因：日程的 `calendar_id` 指向一张**连行都没有**的日历时，
		// `canWrite` 内部的 `calendarRole` 直接 ErrNoRows → 恒 false
		// → 这条日程在界面上**看得见、删不掉**，只能靠管理员接口清库。
		//
		// 例外**只给创建者本人**（`ev.UID`）和管理员：
		//   · 不是自己建的日程，即使它的日历没了也不许动（否则等于开了
		//     "猜一个 event_id 就能删别人日程"的口子）；
		//   · 日历**行还在（哪怕已软删）**时不走这条路 —— 那种情况
		//     `calendarRole` 查得到 owner，权限判定本来就是正常的。
		orphanOwned := ev.UID != "" && ev.UID == uid && !s.calendarExists(ev.CalendarID)
		if !orphanOwned && !s.isAdminUser(uid) {
			apiErr(w, 403, "这个日历你没有编辑权限")
			return
		}
	}
	// ⚠️ 上面这套放宽**只对"删"成立**。"改"（handleUpdateEvent）仍然要求
	// `canWrite(ev.CalendarID)` —— 所以孤儿日程是「删得掉、但改不动」，
	// 别在这里读成"改也放开了"。
	//
	// 为什么不顺手把"改"也放开：孤儿日程的正常归宿是 autoRepair 把它改挂到
	// 创建者的私人日历（repair.go 第 ③ 段），那条路已经兜住了；再在 PUT 上
	// 开一个"是创建者就放行"的口子，等于多一条越权面，收益却接近零。
	if err := s.deleteEventCore(uid, id); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// deleteEventCore 是"删一条日程"的**唯一实现**，用户删除与管理员删除都走它。
//
// 为什么要抽出来（而不是让管理员那边自己写一段 SQL）：
// 删除不只是"把 deleted_at 填上" —— 它还要 **抬 event 的 seq** 并
// **重算 instances**，客户端才会同步掉这条（客户端靠 seq 变化发现删除）。
// 管理员那边若图省事只 UPDATE deleted_at，结果是"管理员删了、服务端也没了，
// 但所有客户端还显示着，而且再也不会同步掉" —— 这是本系统最难查的一类故障。
// 让两条路径共用同一个函数，这个错误就不可能发生。
//
// 参数 uid 只用于记录是谁删的（审计），**不做权限判断** ——
// 权限由各调用方自己把住（用户删除走 canWrite，管理员删除走 requireAdmin）。
func (s *Server) deleteEventCore(uid, id string) error {
	_ = uid
	tx, err := s.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	seq, err := nextSeq(tx, "event", id, "delete")
	if err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE events SET deleted_at=?, seq=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		now, seq, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 已经被删过了。不是错误（幂等），但也不要继续往下走去重算实例。
		return tx.Commit()
	}
	if err := recomputeInstances(tx, id, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
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

	// 违禁词闸门：放在权限与参数校验**之后**、写库**之前**，与创建/修改/push 三条
	// 路径同一位置、同一理由（没权限的人不该通过报错文案探到内容规则）。
	//
	// ⚠️ 2026-10-16 补：这条 REST 路径原来是**漏的** —— POST/PUT /api/events
	//    和 push 同步路径都挂着词闸 + sev=3 送 AI 复核，唯独「改这一次」没有。
	//    于是它成了整套审核的旁路：先建一条干净日程，再往 patch 的
	//    标题/备注/地点里写违禁内容，词表和 AI 全都不生效，
	//    内容直接落库并同步到所有端。手机端「日程详情 → 改这一次」就是这条路。
	patchText := func(key string) string {
		if v, ok := req.Patch[key].(string); ok {
			return v
		}
		return ""
	}
	if s.blockedByWords(w, r, map[string]string{
		"标题": patchText("title"),
		"备注": patchText("notes"),
		"地点": patchText("location"),
	}) {
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
	// ⚠️ nextSeq 的错误不许吞（2026-10-16 修）：吞掉 → seq=0 → 而这个分支是
	// `ON CONFLICT ... DO UPDATE SET seq=excluded.seq`，会把**原先那个有效的 seq
	// 一起改写成 0**；`sync_log` 里也没有这一条 → 别的设备永远看不到这次「改单次」。
	seq, err := nextSeq(tx, "event", id, "occurrence")
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
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
	// ⚠️ nextSeq 的错误不许吞（2026-10-16 修）：seq=0 → `sync_log` 里没有这条 →
	// **别的设备永远看不到这次「删单次」**，那一格日程会一直留在它们的日历上。
	seq, err := nextSeq(tx, "event", id, "occurrence")
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
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
	// ⚠️ nextSeq 的错误不许吞（2026-10-16 修）：seq=0 → 别的设备的「稍后提醒」
	// 列表拿不到这条 snooze，会照原时间响铃（或者干脆不漏）。
	seq, err := nextSeq(tx, "snooze", sid, "upsert")
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
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
	rows, err := tx.Query(`SELECT id FROM snoozes WHERE event_id=? AND user_id=? AND deleted_at IS NULL`, id, uid)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	ids := []string{}
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err == nil {
			ids = append(ids, sid)
		}
	}
	rows.Close()
	for _, sid := range ids {
		// ⚠️ 同上面那条：吞掉 seq 的错误 = 这次「取消稍后提醒」谁也看不到，
		// 别的设备会按旧时间继续响。
		seq, err := nextSeq(tx, "snooze", sid, "delete")
		if err != nil {
			apiErr(w, 500, err.Error())
			return
		}
		if _, err := tx.Exec(`UPDATE snoozes SET deleted_at=?, seq=? WHERE id=?`, time.Now().Unix(), seq, sid); err != nil {
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

// conflictCarry 是"冲突解决选『两个都保留』"时，要从原事件整份搬到副本上的三样东西。
//
// ## 为什么必须单独读，不能用 ev.Reminders
//
// `eventByID` 里的 `.Reminders` / `.Strong` 是**按调用者过滤**的
// （`remindersForUser(id, viewerID, …)` / `strongForUser(id, viewerID)`），
// 而冲突解决那条路传的 viewerID 是**空串**（`eventByID(c.EntityID, "")`）——
// 两个函数开头都是 `if eventID == "" || uid == "" { return 空 }`，于是拿到的**永远是空集**。
//
// 2026-10-16 之前 `duplicateEvent` 就是拿那个空集去复制的：
//
//	for _, rem := range src.Reminders { … }   // src.Reminders 恒为 []，一次都没进过循环
//
// 所以「两个都保留」出来的副本上**一条提醒、一条强提醒都没有**，
// 那条写着 "carry the reminders over too" 的循环是条死代码。
//
// ## 为什么三张表都要、而且都要带 user_id
//
// reminders / reminder_prefs / strong_reminders **都是按人存的**：
//   · reminders        —— 不写 user_id 就成了"共享默认行"（user_id=''），归属丢失；
//   · reminder_prefs   —— 少了它，`remindersForUser` 判定"这个人没表过态"，
//                         即使提醒行搬过去了，下发时照样返回空集（见 acl.go:451 那段注释）；
//   · strong_reminders —— 主键就是 (event_id, user_id)，换了 event_id 必须整行插一份。
//
// 范本见 db.go:605（那个"认领 ownerless 提醒"的迁移）：改 reminders.user_id 的同时，
// 它一定会补一行 reminder_prefs —— 两件事是一件。
//
// ⚠️ 必须在**事务外**调用：连接池只有一条连接，事务开着时再查会死锁
//    （和 handleConflict 里预加载 srcEvent 是同一个理由）。
// ⚠️ 三个查询之间**必须手动 Close**，不能用 defer —— 单连接池下前一个迭代器没关，
//    后一个查询就会卡在那条唯一连接上。
type conflictCarry struct {
	Reminders []reminderCopy
	Prefs     []string
	Strong    []strongCopy
}

type reminderCopy struct {
	UserID        string
	MinutesBefore int64
	Channel       string
}

type strongCopy struct {
	UserID string
	OnOff  bool
}

// carryForCopy 把原事件上"整份搬走"要用的东西一次读齐。
func (s *Server) carryForCopy(eventID string) conflictCarry {
	out := conflictCarry{Reminders: []reminderCopy{}, Prefs: []string{}, Strong: []strongCopy{}}

	if rows, err := s.db.sql.Query(`SELECT user_id,minutes_before,channel FROM reminders
		WHERE event_id=? AND deleted_at IS NULL ORDER BY minutes_before DESC`, eventID); err == nil {
		for rows.Next() {
			var r reminderCopy
			if rows.Scan(&r.UserID, &r.MinutesBefore, &r.Channel) == nil {
				out.Reminders = append(out.Reminders, r)
			}
		}
		rows.Close()
	}

	if rows, err := s.db.sql.Query(
		`SELECT user_id FROM reminder_prefs WHERE event_id=?`, eventID); err == nil {
		for rows.Next() {
			var uid string
			if rows.Scan(&uid) == nil {
				out.Prefs = append(out.Prefs, uid)
			}
		}
		rows.Close()
	}

	if rows, err := s.db.sql.Query(
		`SELECT user_id,on_off FROM strong_reminders WHERE event_id=?`, eventID); err == nil {
		for rows.Next() {
			var sc strongCopy
			var onOff int64
			if rows.Scan(&sc.UserID, &onOff) == nil {
				sc.OnOff = onOff != 0
				out.Strong = append(out.Strong, sc)
			}
		}
		rows.Close()
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

package app

import (
	"net/http"
	"time"
)

// ---------------------------------------------------------------------------
// 账号详情（2026-10-18）
//
// 列表页只能回答"有哪些人"。真出事的时候要问的是"这个人的东西在哪儿"：
// 他建了几个日历、里面有多少条日程、最近改了什么、从哪台设备登过。
//
// 这个接口只读，不改任何东西。它存在的意义是让管理员不用连数据库就能
// 把一个人的数据范围看清楚 —— 比如"删掉这个账号会连带删掉多少条日程"。
// ---------------------------------------------------------------------------

type userCalendarRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	OwnerID   string `json:"owner_id"`
	Mine      bool   `json:"mine"` // true = 他是所有者，false = 被共享进来
	Events    int    `json:"events"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Server) handleAdminUserDetail(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) {
		return
	}
	uid := r.PathValue("id")
	if uid == "" {
		apiErr(w, 400, "缺少账号 id")
		return
	}

	var name string
	var isAdmin, banned int
	var createdAt, bannedAt, banUntil int64
	var banReason, bannedBy string
	err := s.db.sql.QueryRow(`
		SELECT name, is_admin, created_at,
		       COALESCE(banned,0), COALESCE(ban_reason,''), COALESCE(banned_at,0),
		       COALESCE(banned_by,''), COALESCE(ban_until,0)
		FROM users WHERE id=?`, uid).
		Scan(&name, &isAdmin, &createdAt, &banned, &banReason, &bannedAt, &bannedBy, &banUntil)
	if err != nil {
		apiErr(w, 404, "没有这个账号")
		return
	}

	// 日历：自己拥有的 + 别人共享给他的。两者要分开标，因为"删掉这个账号"
	// 会删掉前者，后者只是退出共享。
	calRows, err := s.db.sql.Query(`
		SELECT c.id, c.name, c.kind, c.owner_id, c.created_at,
		       (SELECT COUNT(1) FROM events e
		         WHERE e.calendar_id=c.id AND e.deleted_at IS NULL) AS n
		FROM calendars c
		WHERE c.deleted_at IS NULL AND (
		  c.owner_id=? OR EXISTS(
		    SELECT 1 FROM calendar_members m WHERE m.calendar_id=c.id AND m.user_id=?)
		)
		ORDER BY c.owner_id=? DESC, c.created_at`, uid, uid, uid)
	cals := []userCalendarRow{}
	if err == nil {
		defer calRows.Close()
		for calRows.Next() {
			var c userCalendarRow
			var owner string
			if err := calRows.Scan(&c.ID, &c.Name, &c.Kind, &owner, &c.CreatedAt, &c.Events); err == nil {
				c.OwnerID = owner
				c.Mine = owner == uid
				cals = append(cals, c)
			}
		}
	}

	owned, shared, totalEvents := 0, 0, 0
	for _, c := range cals {
		if c.Mine {
			owned++
			totalEvents += c.Events
		} else {
			shared++
		}
	}

	// 最近日程：按开始时间倒序，只看没删的。
	evRows, err := s.db.sql.Query(`
		SELECT e.id, e.title, COALESCE(e.dtstart_utc,0), e.is_task,
		       COALESCE(e.done_at,0), c.name
		FROM events e JOIN calendars c ON c.id=e.calendar_id
		WHERE e.deleted_at IS NULL AND c.deleted_at IS NULL AND (
		  c.owner_id=? OR EXISTS(
		    SELECT 1 FROM calendar_members m WHERE m.calendar_id=c.id AND m.user_id=?))
		ORDER BY COALESCE(e.dtstart_utc, e.created_at) DESC
		LIMIT 20`, uid, uid)
	events := []map[string]any{}
	if err == nil {
		defer evRows.Close()
		for evRows.Next() {
			var id, title, calName string
			var start, doneAt int64
			var isTask int
			if err := evRows.Scan(&id, &title, &start, &isTask, &doneAt, &calName); err == nil {
				events = append(events, map[string]any{
					"id": id, "title": title, "start_at": start,
					"is_task": isTask == 1, "done": doneAt > 0, "calendar": calName,
				})
			}
		}
	}

	// 设备
	devRows, err := s.db.sql.Query(`
		SELECT id, name, platform, version, last_seen_at
		FROM devices WHERE user_id=? ORDER BY last_seen_at DESC`, uid)
	devices := []map[string]any{}
	if err == nil {
		defer devRows.Close()
		for devRows.Next() {
			var id, dn, plat, ver string
			var seen int64
			if err := devRows.Scan(&id, &dn, &plat, &ver, &seen); err == nil {
				devices = append(devices, map[string]any{
					"id": id, "name": dn, "platform": plat,
					"version": ver, "last_seen_at": seen,
				})
			}
		}
	}

	// 最近登录（有这张表就用，没有就空着 —— 老库里可能没建）。
	logins := []map[string]any{}
	lgRows, err := s.db.sql.Query(`
		SELECT event, result, COALESCE(ip,''), COALESCE(ua,''), created_at
		FROM login_logs WHERE user_id=? ORDER BY created_at DESC LIMIT 10`, uid)
	if err == nil {
		defer lgRows.Close()
		for lgRows.Next() {
			var ev, res, ip, ua string
			var at int64
			if err := lgRows.Scan(&ev, &res, &ip, &ua, &at); err == nil {
				logins = append(logins, map[string]any{
					"event": ev, "result": res, "ip": ip, "ua": ua, "at": at,
				})
			}
		}
	}

	// 共享给他的日历里，他有多少条别人建的日程 —— 这是"退出共享会失去什么"。
	sharedEvents := 0
	for _, c := range cals {
		if !c.Mine {
			sharedEvents += c.Events
		}
	}

	writeJSON(w, 200, map[string]any{
		"id":         uid,
		"name":       name,
		"is_admin":   isAdmin == 1,
		"created_at": createdAt,
		"banned": map[string]any{
			"active": banned == 1, "reason": banReason, "by": bannedBy,
			"at": bannedAt, "until": banUntil,
		},
		"summary": map[string]any{
			"calendars_owned":  owned,
			"calendars_shared": shared,
			"events_owned":     totalEvents,
			"events_shared":    sharedEvents,
			"devices":          len(devices),
		},
		"calendars":      cals,
		"recent_events":  events,
		"devices_detail": devices,
		"recent_logins":  logins,
		"server_time":    time.Now().Unix(),
	})
}

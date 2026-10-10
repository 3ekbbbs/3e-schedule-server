package main

// 重复日程清理（2026-09-28）。
//
// ## 背景
//
// 用户昨晚用 AI 建日程，AI **把同一节课重复建了好几遍**（每次拿一个新的 id），
// 账号里于是出现一批"同名同时刻"的日程。实测某账号：85 条活日程里
// **30 组重复、多出 47 条**，全是课程表里那几门课的重复副本。
//
// AI 侧的去重（不在提案里重复建）只能防住**以后**；已经写进库的那批必须有
// 地方清掉，否则用户只能一条条手点删除 —— 47 条，不现实。
//
// ## 这个接口做两件事
//
//	POST /api/maintenance/dedupe?dry=1   只报不删（客户端先显示"有多少组重复"）
//	POST /api/maintenance/dedupe         真清理
//
// ## 判据必须保守
//
// 只有「**同一张日历 + 同一标题 + 同一开始时间 + 同一时长**」才算完全重复。
// 为什么不再放宽（这几条都是刻意不做的）：
//
//	· 不按"标题相似"判 —— "高等数学Ⅱx1" 和 "高等数学Ⅱ" 是不是一回事，机器判不了；
//	· 不按"时间接近"判 —— 相邻两节课差 5 分钟是常态；
//	· 不跨日历判 —— 同一节课挂在两张日历上是用户有意的安排。
//
// 换句话说：**这里清的是"同一件事被写了两遍"，不是"看起来像的两件事"**。
// 误删用户数据的代价远高于少清几条，所以判据宁可窄。
//
// ## 权限
//
// 默认只动调用者 `canWrite` 的日历（含团队共享带来的写权限）；
// 管理员可加 `?all=1` 扫全库（用于给别人的账号清）。
// 逐组校验，某一组没权限就**跳过它**（计数进 `skipped_groups`），
// 不让一组没权限把整次清理搞成失败。
//
// ## 修法
//
// 每组**留最早创建的那条**（`created_at` 最小，并列时取 id 最小的 —— 保证
// 幂等：同一个库跑两次结果一样），其余打墓碑 + 抬 seq + 重算实例。
//
// ⚠️ 与项目里所有删除一样：**打墓碑，绝不硬删**。客户端是按 seq 增量 upsert 的，
// 只认 `deleted_at` 非空的行；`DELETE FROM` 只会让"服务端干净了、客户端还留着"。

import (
	"database/sql"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type dedupeGroup struct {
	CalendarID   string   `json:"calendar_id"`
	CalendarName string   `json:"calendar_name"`
	Title        string   `json:"title"`
	StartUTC     int64    `json:"start_utc"`
	DurationSec  int64    `json:"duration_sec"`
	Count        int      `json:"count"`
	Kept         string   `json:"kept"`
	Removed      []string `json:"removed"`
}

type dedupeResult struct {
	OK            bool          `json:"ok"`
	Dry           bool          `json:"dry"`
	Scanned       int           `json:"scanned"`
	Groups        int           `json:"groups"`
	Duplicates    int           `json:"duplicates"`
	Removed       int           `json:"removed"`
	SkippedGroups int           `json:"skipped_groups"`
	Orphans       int           `json:"orphans"`
	Sample        []dedupeGroup `json:"sample"`
}

// dedupeKeyOf 是"完全重复"的判据。改这里等于改判据，想清楚再动。
func dedupeKeyOf(calID, title string, startUTC, durationSec int64) string {
	// 标题两端空白不算差异（AI 有时候会带一个尾空格）。
	// 用 \x00 当分隔符：标题里不可能出现它，避免"标题里带 | 导致串键"。
	return calID + "\x00" + strings.TrimSpace(title) + "\x00" +
		strconv.FormatInt(startUTC, 10) + "\x00" + strconv.FormatInt(durationSec, 10)
}

// scanDuplicates 扫重复组。**只读**，不区分权限（权限在应用阶段逐组判）。
func (s *Server) scanDuplicates() ([]dedupeGroup, int, error) {
	rows, err := s.db.sql.Query(`
		SELECT e.id, e.calendar_id, COALESCE(c.name,''), e.title,
		       COALESCE(e.dtstart_utc,0), e.duration_sec,
		       COALESCE(e.created_at,0)
		FROM events e
		JOIN calendars c ON c.id = e.calendar_id
		WHERE e.deleted_at IS NULL AND c.deleted_at IS NULL
		ORDER BY e.calendar_id, e.title, COALESCE(e.dtstart_utc,0), e.duration_sec,
		         COALESCE(e.created_at,0), e.id`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	type row struct {
		id, calID, calName, title string
		startUTC, duration        int64
		createdAt                 int64
	}
	groups := map[string][]row{}
	order := []string{}
	scanned := 0
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.calID, &r.calName, &r.title,
			&r.startUTC, &r.duration, &r.createdAt); err != nil {
			return nil, 0, err
		}
		scanned++
		k := dedupeKeyOf(r.calID, r.title, r.startUTC, r.duration)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	out := []dedupeGroup{}
	for _, k := range order {
		list := groups[k]
		if len(list) < 2 {
			continue
		}
		// ORDER BY 已经把 created_at, id 排好了 → 第一条就是"该留的那条"。
		g := dedupeGroup{
			CalendarID:   list[0].calID,
			CalendarName: list[0].calName,
			Title:        list[0].title,
			StartUTC:     list[0].startUTC,
			DurationSec:  list[0].duration,
			Count:        len(list),
			Kept:         list[0].id,
			Removed:      []string{},
		}
		for _, r := range list[1:] {
			g.Removed = append(g.Removed, r.id)
		}
		out = append(out, g)
	}
	// 重复条数多的组排前面，客户端看着有用。
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].Removed) != len(out[j].Removed) {
			return len(out[i].Removed) > len(out[j].Removed)
		}
		return out[i].StartUTC < out[j].StartUTC
	})
	return out, scanned, nil
}

// countMyOrphanEvents 数"我建的、但日历已经不存在或已删除"的活日程。
//
// 这是自助口径（只算自己的），所以能给普通用户看 —— 客户端"数据修复"卡片
// 用它在服务端侧对一次账（客户端看到的残留通常比这个多，那部分是**本地镜像**
// 的陈年旧行，得靠客户端自己清理，见各端的 refresh/reconcile）。
func (s *Server) countMyOrphanEvents(uid string) int {
	if uid == "" {
		return 0
	}
	var n int
	_ = s.db.sql.QueryRow(`
		SELECT COUNT(*) FROM events e
		LEFT JOIN calendars c ON c.id = e.calendar_id
		WHERE e.deleted_at IS NULL AND e.uid=?
		  AND (c.id IS NULL OR c.deleted_at IS NOT NULL)`, uid).Scan(&n)
	return n
}

// handleMaintenanceDedupe 见文件顶部说明。
func (s *Server) handleMaintenanceDedupe(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	if uid == "" {
		apiErr(w, 401, "请先登录")
		return
	}
	q := r.URL.Query()
	dry := q.Get("dry") == "1"
	// `all=1` 只对管理员生效；普通用户传了也没用（不报错，按自己范围跑）。
	all := q.Get("all") == "1" && s.isAdminUser(uid)

	groups, scanned, err := s.scanDuplicates()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	res := &dedupeResult{
		OK: true, Dry: dry, Scanned: scanned,
		Sample: []dedupeGroup{}, Orphans: s.countMyOrphanEvents(uid),
	}

	// 逐组过权限，挑出这次真能动的那些。
	todo := []dedupeGroup{}
	for _, g := range groups {
		if !all && !s.canWrite(g.CalendarID, uid) {
			res.SkippedGroups++
			continue
		}
		todo = append(todo, g)
	}
	res.Groups = len(todo)
	for _, g := range todo {
		res.Duplicates += len(g.Removed)
	}
	if len(res.Sample) == 0 {
		for i, g := range todo {
			if i >= 20 {
				break
			}
			res.Sample = append(res.Sample, g)
		}
	}

	if dry || len(todo) == 0 {
		writeJSON(w, 200, res)
		return
	}

	now := time.Now().Unix()
	err = s.withTx(func(tx *sql.Tx) error {
		for _, g := range todo {
			for _, id := range g.Removed {
				ok, e := tombstoneEventTx(tx, id, now)
				if e != nil {
					return e
				}
				if ok {
					res.Removed++
				}
			}
		}
		return nil
	})
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// tombstoneEventTx 给一条日程打墓碑，并重算它的实例。
//
// 与 `deleteEventCore` 完全同一套动作，只是**复用调用者的事务**
// （`deleteEventCore` 自己 Begin，没法嵌在去重的那一个事务里）。
// 幂等：已经删过的返回 false，不算错误。
func tombstoneEventTx(tx *sql.Tx, id string, now int64) (bool, error) {
	seq, err := nextSeq(tx, "event", id, "delete")
	if err != nil {
		return false, err
	}
	res, err := tx.Exec(
		`UPDATE events SET deleted_at=?, seq=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		now, seq, now, id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if err := recomputeInstances(tx, id, time.Now()); err != nil {
		return false, err
	}
	return true, nil
}

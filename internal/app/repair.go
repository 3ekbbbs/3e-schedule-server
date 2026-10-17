package app

import (
	"database/sql"
	"net/http"
	"time"
)

// 数据自检与修复（2026-09-25）。
//
// ## 为什么需要
//
// 用户报的原话：「会出现我这边看得到，但是服务端提示日历不存在的问题，
// 或者说我这边看不到，但是又有对应的日程选到对应的日历」。
//
// 这两句分别对应两类**引用完整性**问题：
//
//  ① 客户端本地镜像里留着一张服务端已经没有（或已软删）的日历 ——
//     界面上看得见，可一旦去动它（改名/加日程）服务端就回 404。
//     根因通常是历史上某次删除没把变更推到客户端，或者客户端处理 tombstone 时漏了。
//  ② 日程/实例挂在一张自己看不到的日历上 ——
//     日程列表里冒出来一条，点开却找不到它属于哪张日历。
//     根因是**软删没有级联干净**：日历被软删了，但它的 events / instances
//     还活着（`handleDeleteCalendar` 现在会级联，但历史上写下的行还在）。
//
// ## 这个文件做什么
//
// 一个**只读扫描**（`/api/maintenance/scan`）+ 一个**修复**（`/api/maintenance/repair`）。
// 两者都是管理员接口，都要显式调用 —— **不会自动改任何东西**。
//
// ## 修复的原则（每条都有理由）
//
//	孤儿成员行 / 孤儿团队共享行  → 直接删（它们指向的日历已经没了，留着只会污染权限查询）
//	日历软删了、日程还活着      → **补上墓碑**（和 handleDeleteCalendar 做的一样）
//	                              ⚠️ 不删日程本身：它可能是需要人工找回的数据，
//	                                 打上墓碑后客户端会正常把它从列表里去掉
//	日程指向**完全不存在的**日历 → 改挂到创建者的私人日历（**保住日程**），
//	                              实在没有归属人时才算孤儿删掉
//	派生的行（instances / overrides / reminders）→ 删；instances 本来就能重算
//	强提醒                       → 删（它跟着事件走）
//
// ⚠️ **每一类修复都必须抬 seq**。这是整件事的关键：客户端的同步是**按 seq 增量拉**的，
// 只改数据库不抬 seq，客户端**永远不会知道**自己手里那份是错的 ——
// 那正是"我这边看得到、服务端说没有"能顽固存在的原因。

// RepairIssue 一条不一致。
type RepairIssue struct {
	Kind   string `json:"kind"`   // 机器可读的分类，见下面 rep* 常量
	ID     string `json:"id"`     // 出问题的行
	Extra  string `json:"extra"`  // 附带的上下文（日历 id / 事件 id 之类）
	Detail string `json:"detail"` // 给人看的一句话
}

// RepairReport 扫描/修复的结果。
type RepairReport struct {
	Issues  []RepairIssue  `json:"issues"`
	Kinds   map[string]int `json:"kinds"`
	Fixed   int            `json:"fixed"`
	Applied bool           `json:"applied"`
}

const (
	repOrphanMember   = "orphan_calendar_member"    // 成员行指向不存在/已删的日历
	repOrphanTeam     = "orphan_calendar_team"      // 团队共享行指向不存在/已删的日历
	repEventNoCal     = "event_without_calendar"    // 日程挂在一张完全不存在的日历上
	repEventDeadCal   = "event_in_deleted_calendar" // 日历软删了、日程还活着
	repEventOrphans   = "orphan_event_child"        // 覆盖/提醒/实例指向不存在的事件
	repStrongOrphan   = "orphan_strong_reminder"    // 强提醒指向不存在的事件
	repInstanceOrphan = "orphan_instance"           // 实例指向不存在的事件
	// ★ 2026-09-27 补：实例指向**已打墓碑**的事件。上一版漏了这类，
	//   于是"日程删了、实例还在"查不出来 —— 正是用户报的那种残留。
	repInstanceOfDeadEvent = "instance_of_deleted_event"
)

func (r *RepairReport) add(kind, id, extra, detail string) {
	r.Issues = append(r.Issues, RepairIssue{Kind: kind, ID: id, Extra: extra, Detail: detail})
	if r.Kinds == nil {
		r.Kinds = map[string]int{}
	}
	r.Kinds[kind]++
}

// scanOrphans 扫描**整个库**的引用完整性问题。
//
// ⚠️ 故意扫全库而不是只扫调用者自己的：这些不一致是"库层面的"，
//
//	按用户过滤反而会漏（比如一张日历的 owner 已经不在了）。
//	所以这个接口**只给管理员**，见 main.go 里的注册。
func (s *Server) scanOrphans() (*RepairReport, error) {
	rep := &RepairReport{Issues: []RepairIssue{}, Kinds: map[string]int{}}
	q := func(sqlText string, args ...any) (*sql.Rows, error) {
		return s.db.sql.Query(sqlText, args...)
	}

	// ① 成员行 → 日历不存在，或日历已软删
	rows, err := q(`SELECT m.calendar_id, m.user_id FROM calendar_members m
		LEFT JOIN calendars c ON c.id = m.calendar_id
		WHERE c.id IS NULL OR c.deleted_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cid, uid string
		if rows.Scan(&cid, &uid) == nil {
			rep.add(repOrphanMember, cid, uid, "日历已不存在或已删除，但成员行还在")
		}
	}
	rows.Close()

	// ② 团队共享行 → 同上
	rows, err = q(`SELECT ct.calendar_id, ct.team_id FROM calendar_teams ct
		LEFT JOIN calendars c ON c.id = ct.calendar_id
		WHERE c.id IS NULL OR c.deleted_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cid, tid string
		if rows.Scan(&cid, &tid) == nil {
			rep.add(repOrphanTeam, cid, tid, "日历已不存在或已删除，但团队共享行还在")
		}
	}
	rows.Close()

	// ③ 日程挂在**完全不存在的**日历上（连软删的行都没有）
	rows, err = q(`SELECT e.id, e.calendar_id FROM events e
		LEFT JOIN calendars c ON c.id = e.calendar_id
		WHERE e.deleted_at IS NULL AND c.id IS NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var eid, cid string
		if rows.Scan(&eid, &cid) == nil {
			rep.add(repEventNoCal, eid, cid, "日程挂在一张不存在的日历上，客户端会看得到、但找不到归属")
		}
	}
	rows.Close()

	// ④ 日历软删了、日程还活着（历史上漏掉的级联）
	rows, err = q(`SELECT e.id, e.calendar_id FROM events e
		JOIN calendars c ON c.id = e.calendar_id
		WHERE e.deleted_at IS NULL AND c.deleted_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var eid, cid string
		if rows.Scan(&eid, &cid) == nil {
			rep.add(repEventDeadCal, eid, cid, "所在日历已删除，但这条日程没有跟着打墓碑")
		}
	}
	rows.Close()

	// ⑤ 覆盖 / 提醒 指向不存在的事件
	for _, t := range []struct{ table, kind string }{
		{"event_overrides", repEventOrphans},
		{"reminders", repEventOrphans},
	} {
		rows, err = q(`SELECT x.event_id FROM ` + t.table + ` x
			LEFT JOIN events e ON e.id = x.event_id WHERE e.id IS NULL`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var eid string
			if rows.Scan(&eid) == nil {
				rep.add(t.kind, eid, t.table, t.table+" 指向一条不存在的日程")
			}
		}
		rows.Close()
	}

	// ⑥ 强提醒 → 事件不存在
	rows, err = q(`SELECT s.event_id FROM strong_reminders s
		LEFT JOIN events e ON e.id = s.event_id WHERE e.id IS NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var eid string
		if rows.Scan(&eid) == nil {
			rep.add(repStrongOrphan, eid, "", "强提醒指向一条不存在的日程")
		}
	}
	rows.Close()

	// ⑦ 实例 → 事件不存在（实例是派生数据，删掉就会重算）
	rows, err = q(`SELECT i.id, i.event_id FROM instances i
		LEFT JOIN events e ON e.id = i.event_id WHERE e.id IS NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var iid, eid string
		if rows.Scan(&iid, &eid) == nil {
			rep.add(repInstanceOrphan, iid, eid, "实例指向一条不存在的日程")
		}
	}
	rows.Close()

	// ⑧ ★ 实例 → 事件**已打墓碑**（2026-09-27 补）
	//
	// 这是上一版的漏洞：⑦ 只查"事件行**不存在**"，而删除日程打的是**墓碑**
	//（`deleted_at` 非空，行还在）—— 于是"日程都删了、实例还在"这种残留
	// **一条都查不出来**。
	//
	// 症状就是用户报的：「我这边看不到日历，但又有对应的日程选到对应的日历」
	// —— 界面上按实例渲染，事件和日历都没了，那几条就成了无主条目。
	//
	// ⚠️ 实例是**派生数据**（`recomputeInstances` 随时能重建），
	//    所以修法是**直接删**，不是打墓碑 —— 打了墓碑反而会和重算出来的打架。
	rows, err = q(`SELECT i.id, i.event_id FROM instances i
		JOIN events e ON e.id = i.event_id
		WHERE i.deleted_at IS NULL AND e.deleted_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var iid, eid string
		if rows.Scan(&iid, &eid) == nil {
			rep.add(repInstanceOfDeadEvent, iid, eid, "日程已删除，但它的实例还活着（界面上会变成无主条目）")
		}
	}
	rows.Close()

	return rep, nil
}

// repairOrphans 扫描 + 就地修。
//
// ⚠️ 整个过程在一个事务里：全成或全不成。修到一半停下比不修更糟 ——
//
//	半修状态会让 seq 和实际数据对不上，客户端拿到的东西更乱。
func (s *Server) repairOrphans() (*RepairReport, error) {
	rep, err := s.scanOrphans()
	if err != nil {
		return nil, err
	}
	if len(rep.Issues) == 0 {
		return rep, nil
	}

	now := time.Now().Unix()
	err = s.withTx(func(tx *sql.Tx) error {
		// 每一类先归拢成"要处理的主键集合"，避免逐条重复处理
		memberKeys := map[string][2]string{}
		teamKeys := map[string][2]string{}
		noCal := map[string]string{}   // eventID → 原 calendarID
		deadCal := map[string]string{} // eventID → calendarID
		childEvents := map[string]bool{}
		strongEvents := map[string]bool{}
		instanceIDs := []string{}

		for _, is := range rep.Issues {
			switch is.Kind {
			case repOrphanMember:
				memberKeys[is.ID+"|"+is.Extra] = [2]string{is.ID, is.Extra}
			case repOrphanTeam:
				teamKeys[is.ID+"|"+is.Extra] = [2]string{is.ID, is.Extra}
			case repEventNoCal:
				noCal[is.ID] = is.Extra
			case repEventDeadCal:
				deadCal[is.ID] = is.Extra
			case repEventOrphans:
				childEvents[is.ID] = true
			case repStrongOrphan:
				strongEvents[is.ID] = true
			case repInstanceOrphan:
				instanceIDs = append(instanceIDs, is.ID)
			case repInstanceOfDeadEvent:
				// 同上：实例是派生数据，直接删（打了墓碑反而和重算出来的打架）
				instanceIDs = append(instanceIDs, is.ID)
			}
		}

		// ---- ①② 孤儿成员 / 孤儿团队共享：直接删 ----
		for _, k := range memberKeys {
			if _, e := tx.Exec(`DELETE FROM calendar_members WHERE calendar_id=? AND user_id=?`, k[0], k[1]); e != nil {
				return e
			}
			rep.Fixed++
		}
		for _, k := range teamKeys {
			if _, e := tx.Exec(`DELETE FROM calendar_teams WHERE calendar_id=? AND team_id=?`, k[0], k[1]); e != nil {
				return e
			}
			rep.Fixed++
		}

		// ---- ④ 日历软删了、日程还活着：补墓碑（和 handleDeleteCalendar 一致）----
		// ⚠️ 不删日程行 —— 万一是要找回的数据。打上墓碑后客户端会正常把它去掉。
		for eid := range deadCal {
			seq, e := nextSeq(tx, "event", eid, "delete")
			if e != nil {
				return e
			}
			if _, e = tx.Exec(`UPDATE events SET deleted_at=?, seq=?, updated_at=? WHERE id=?`, now, seq, now, eid); e != nil {
				return e
			}
			rep.Fixed++
		}

		// ---- ③ 日程挂在不存在的日历上：改挂到创建者的私人日历（保住日程）----
		for eid, badCal := range noCal {
			owner := ""
			// 先从原日历行里找（哪怕它是软删的，owner 信息还在）
			_ = tx.QueryRow(`SELECT owner_id FROM calendars WHERE id=?`, badCal).Scan(&owner)
			if owner == "" {
				// 连日历行都没了 → 退一步用日程自己的 uid（= 创建者）
				_ = tx.QueryRow(`SELECT uid FROM events WHERE id=?`, eid).Scan(&owner)
			}
			target := ""
			if owner != "" {
				// 优先用他名下的私人日历（kind='private' 且没删）
				_ = tx.QueryRow(`SELECT id FROM calendars
					WHERE owner_id=? AND kind='private' AND deleted_at IS NULL
					ORDER BY created_at LIMIT 1`, owner).Scan(&target)
			}
			if target == "" {
				// 实在没有归属人（账号都没了）→ 只能删掉这条孤儿日程
				if _, e := tx.Exec(`DELETE FROM events WHERE id=?`, eid); e != nil {
					return e
				}
				rep.Fixed++
				continue
			}
			// 改挂 + 抬 seq：日历换了，客户端必须重拉这条
			seq, e := nextSeq(tx, "event", eid, "update")
			if e != nil {
				return e
			}
			if _, e = tx.Exec(`UPDATE events SET calendar_id=?, seq=?, updated_at=? WHERE id=?`,
				target, seq, now, eid); e != nil {
				return e
			}
			if e = recomputeInstances(tx, eid, time.Unix(now, 0)); e != nil {
				return e
			}
			rep.Fixed++
		}

		// ---- ⑤⑥ 覆盖 / 提醒 / 强提醒 指向不存在的事件：删 ----
		for eid := range childEvents {
			for _, t := range []string{"event_overrides", "reminders"} {
				if _, e := tx.Exec(`DELETE FROM `+t+` WHERE event_id=?`, eid); e != nil {
					return e
				}
			}
			rep.Fixed++
		}
		for eid := range strongEvents {
			if _, e := tx.Exec(`DELETE FROM strong_reminders WHERE event_id=?`, eid); e != nil {
				return e
			}
			rep.Fixed++
		}

		// ---- ⑦ 孤儿实例：**打墓碑**（不是硬删）----
		//
		// ★★ 2026-09-27 修：这里原来是 `DELETE FROM instances WHERE id=?` —— **错了**。
		//
		//   客户端的同步是"按 seq 增量 upsert"，**从不删除** ——
		//   它只知道"服务端下发了一行 deleted_at 非空的记录"。
		//   硬删不产生 tombstone → 服务端干净了、**客户端那 125 条永远留着** ✓
		//   这正是用户报的「账号里面还是有实例」。
		//
		//   ⚠️ 同一个坑本项目已经踩过（日历、日程都是打墓碑），我在实例上又踩了一次。
		//   ⚠️ 实例确实是派生数据（能重算），但**派生 ≠ 可以不通知客户端**。
		if len(instanceIDs) > 0 {
			for _, iid := range instanceIDs {
				seq, e := nextSeq(tx, "instance", iid, "delete")
				if e != nil {
					return e
				}
				if _, e := tx.Exec(`UPDATE instances SET deleted_at=?, seq=? WHERE id=?`,
					now, seq, iid); e != nil {
					return e
				}
				rep.Fixed++
			}
		}

		// ---- 最后：抬一次"全局游标"，逼客户端做一次完整重拉 ----
		//
		// ★ 这是整件事最关键的一步。上面那些修复**没有**逐条抬 seq
		//   （孤儿行删掉没有"变更"可言，客户端也不需要知道具体删了哪行），
		//   但客户端是**按 seq 增量**同步的 —— 不抬就等于它永远不知道库变了。
		//   抬一个 meta 游标 + 写一条 sync_log，让所有客户端下次同步时
		//   至少把日历和事件列表重拉一遍。
		return bumpGlobalRepairCursor(tx, now)
	})
	if err != nil {
		return nil, err
	}
	rep.Applied = true
	return rep, nil
}

// bumpGlobalRepairCursor 让客户端意识到"库变过"。
//
// 写法：往 sync_log 里写一条特殊实体 `maintenance` 的记录。
// 客户端看到不认识的 entity 会走"整份重拉"的分支（见 SyncEngine 的处理），
// 于是上面那些只有服务端知道的不一致会被自然纠正。
func bumpGlobalRepairCursor(tx *sql.Tx, now int64) error {
	_, err := tx.Exec(`INSERT INTO sync_log(entity, entity_id, op, changed_at) VALUES(?,?,?,?)`,
		"maintenance", "repair", "update", now)
	return err
}

// handleMaintenanceScan 只读扫描。管理员专用。
func (s *Server) handleMaintenanceScan(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(userIDOf(r)) {
		apiErr(w, 403, "只有管理员能跑数据自检")
		return
	}
	rep, err := s.scanOrphans()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"issues": rep.Issues,
		"kinds":  rep.Kinds,
		"total":  len(rep.Issues),
	})
}

// handleMaintenanceRepair 扫描 + 修复。管理员专用，**必须显式调用**。
func (s *Server) handleMaintenanceRepair(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(userIDOf(r)) {
		apiErr(w, 403, "只有管理员能跑数据修复")
		return
	}
	rep, err := s.repairOrphans()
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"issues":  rep.Issues,
		"kinds":   rep.Kinds,
		"total":   len(rep.Issues),
		"fixed":   rep.Fixed,
		"applied": rep.Applied,
	})
}

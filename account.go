package main

import (
	"database/sql"
	"net/http"
	"time"
)

// 一键清空账户数据（2026-09-27）。
//
// ★ 用户要求：「给每一个端都加上一键清空账户数据的内容，点击以后删掉所有日程」。
//
// ## 清掉的是什么（边界要清楚，不然是灾难）
//
//	✅ 我**自己创建**的日历 → 打墓碑，并级联它名下的日程 / 实例
//	✅ 我在**别人**的日历里的成员身份 → 移除（"退出共享"）
//	✅ 日历 / 日程的变更推给所有端（抬 seq）
//
//	❌ **不动**别人创建的日历本身（那是别人的数据）
//	❌ **不动**别人日历里的日程（哪怕是我加的 —— 它属于那张共享日历）
//	❌ **不动**机器接口 Key（那是给脚本用的凭证，清日程不该把它弄废）
//	❌ **不动**账号本身（不注销，只是清数据）
//
// ## 为什么是"打墓碑"而不是真删行
//
// 客户端的同步是**按 seq 增量拉**的：真删行不产生 tombstone，客户端永远不知道
// 那份数据没了（红线：本地会一直留着"服务端已经没有"的日历）。
// 所以一律 `deleted_at = now` + 抬 seq ✓
//
// ## 二次确认
//
// 请求体必须带 `confirm`，且**等于调用者自己的账号名**。
// 这不是为了防"误点"（界面已经弹确认框了），而是防止**脚本/误调**把数据清了 ——
// 一个 POST 打错地址就清库是不可接受的。

type wipeResult struct {
	Calendars   int `json:"calendars"`
	Events      int `json:"events"`
	Memberships int `json:"memberships"`
}

func (s *Server) handleWipeAccount(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	if uid == "" {
		apiErr(w, 401, "未登录")
		return
	}

	var body struct {
		Confirm string `json:"confirm"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体格式不对")
		return
	}

	var myName string
	if err := s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, uid).Scan(&myName); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	// 必须原样写出自己的账号名 —— 少一个空格都不行（刻意不做 trim，
	// 因为"我明明输了名字"和"我随手打了个回车"要能区别开）。
	if body.Confirm == "" || body.Confirm != myName {
		apiErr(w, 400, "确认失败：请在确认框里原样输入你的账号名「"+myName+"」")
		return
	}

	res, err := s.wipeUserDataInTx(uid)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}

	writeJSON(w, 200, map[string]any{
		"ok":          true,
		"calendars":   res.Calendars,
		"events":      res.Events,
		"memberships": res.Memberships,
		"note": "已清空。日历和日程在所有端上都会消失（下次同步生效）。" +
			"别人的日历和里面的日程没有被动。",
	})
}

// wipeUserDataInTx 清掉"某个账号自己创建的数据"（在一个事务里）。
//
// ★ 为什么抽成函数：**两个入口要做同一件事** ——
//
//	· `POST /api/account/wipe`：用户自己一键清空；
//	· `DELETE /api/admin/users/{id}`：管理员删账号（清完还要删账号行本身）。
//	两份实现必然漂，而这类"删数据"的漂移是**不可逆**的（用户的原话是
//	"点击以后删掉所有日程"，少删一处他不会立刻发现，等他发现时已经晚了）。
//
// 返回的计数给界面用（"删掉了 3 张日历、50 条日程"比"成功"有用得多）。
func (s *Server) wipeUserDataInTx(uid string) (wipeResult, error) {
	res := wipeResult{}
	now := time.Now().Unix()

	err := s.withTx(func(tx *sql.Tx) error {
		// ---- ① 我自己创建的日历：打墓碑 + 级联日程 ----
		rows, err := tx.Query(`SELECT id FROM calendars WHERE owner_id=? AND deleted_at IS NULL`, uid)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()

		for _, cid := range ids {
			// 先把这个日历名下的日程收出来（级联用）
			erows, err := tx.Query(`SELECT id FROM events WHERE calendar_id=? AND deleted_at IS NULL`, cid)
			if err != nil {
				return err
			}
			evIDs := []string{}
			for erows.Next() {
				var eid string
				if err := erows.Scan(&eid); err == nil {
					evIDs = append(evIDs, eid)
				}
			}
			erows.Close()

			for _, eid := range evIDs {
				seq, err := nextSeq(tx, "event", eid, "delete")
				if err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE events SET deleted_at=?, seq=?, updated_at=? WHERE id=?`,
					now, seq, now, eid); err != nil {
					return err
				}
				// 实例是派生数据，跟着重算（`recomputeInstances` 对已删事件
				// 会走"不在 desired 里就**打墓碑**"那条路，客户端才收得到）✓
				if err := recomputeInstances(tx, eid, time.Now()); err != nil {
					return err
				}
				// 兜底：重算之后还剩活着的实例，也**打墓碑**（不是硬删 ——
				// 硬删客户端永远不知道，本地会一直留着）。
				// ⚠️ 2026-09-27 第一版这里写的是 DELETE，正是"服务端干净了、
				//    客户端还留着 125 条"的成因。
				rows2, err := tx.Query(`SELECT id FROM instances WHERE event_id=? AND deleted_at IS NULL`, eid)
				if err != nil {
					return err
				}
				left := []string{}
				for rows2.Next() {
					var iid string
					if err := rows2.Scan(&iid); err == nil {
						left = append(left, iid)
					}
				}
				rows2.Close()
				for _, iid := range left {
					iseq, err := nextSeq(tx, "instance", iid, "delete")
					if err != nil {
						return err
					}
					if _, err := tx.Exec(`UPDATE instances SET deleted_at=?, seq=? WHERE id=?`,
						now, iseq, iid); err != nil {
						return err
					}
				}
				res.Events++
			}

			seq, err := nextSeq(tx, "calendar", cid, "delete")
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE calendars SET deleted_at=?, seq=?, updated_at=? WHERE id=?`,
				now, seq, now, cid); err != nil {
				return err
			}
			res.Calendars++
		}

		// ---- ② 我在**别人**日历里的成员身份：移除 ----
		//
		// ⚠️ 只删"日历不是我建的"那些成员行（上面那批已经连日历一起处理了）。
		out, err := tx.Exec(`DELETE FROM calendar_members
			WHERE user_id=? AND calendar_id IN (SELECT id FROM calendars WHERE owner_id <> ?)`, uid, uid)
		if err != nil {
			return err
		}
		if n, err := out.RowsAffected(); err == nil {
			res.Memberships = int(n)
		}

		// ---- ③ 逼所有端重拉一次 ----
		//
		// ★ 关键：客户端的同步是增量拉，光改数据库不抬 seq 客户端不会知道。
		//   写一条 maintenance 记录（和各端既有的"整份重拉"分支对齐）。
		return bumpGlobalRepairCursor(tx, now)
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// ownedDataBlockers 列出"这个账号现在还删不掉"的原因（空切片 = 可以删）。
//
// ★ 为什么是**拒绝**而不是自动处理：他创建的日历里只要还有别人（或其他团队成员
//
//	能看见），删掉这张日历就会**连带删掉别人的日程** —— 那是不可逆的数据损失，
//	而管理员点这一下的时候根本不知道有这回事。
//	所以宁可拒绝并把"怎么处理"说清楚。
//
// 返回的是**直接能给人看**的句子（界面原样显示，不再加工）——
// 错误信息能指出下一步怎么做，比"删除失败"有用得多。
func (s *Server) ownedDataBlockers(uid string) []string {
	var out []string

	// 他建的日历里还有别人（直接成员 / 或有团队能看见）
	calRows, err := s.db.sql.Query(`
		SELECT c.name FROM calendars c
		WHERE c.owner_id=? AND c.deleted_at IS NULL
		  AND ( EXISTS(SELECT 1 FROM calendar_members m WHERE m.calendar_id=c.id AND m.user_id<>?)
		     OR EXISTS(SELECT 1 FROM calendar_teams t WHERE t.calendar_id=c.id) )
		ORDER BY c.created_at`, uid, uid)
	if err == nil {
		for calRows.Next() {
			var n string
			if e := calRows.Scan(&n); e == nil {
				out = append(out, "日历「"+n+"」里还有别人（或共享给了团队）")
			}
		}
		calRows.Close()
	}

	// 他建的团队里还有别人
	teamRows, err := s.db.sql.Query(`
		SELECT t.name FROM teams t
		WHERE t.owner_id=?
		  AND EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=t.id AND m.user_id<>?)`, uid, uid)
	if err == nil {
		for teamRows.Next() {
			var n string
			if e := teamRows.Scan(&n); e == nil {
				out = append(out, "团队「"+n+"」里还有别人")
			}
		}
		teamRows.Close()
	}
	return out
}

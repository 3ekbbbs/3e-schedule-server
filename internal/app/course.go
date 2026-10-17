package app

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Course timetable support.
//
// A timetable is not a new entity: it is an ordinary calendar whose kind is
// "course", rendered as a periods x weekdays grid instead of an agenda. The
// only new state is the period table (which bell times the grid rows are), and
// that lives in app_settings so both clients always agree on the row layout.

type coursePeriod struct {
	Index int    `json:"index"`
	Label string `json:"label"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// A conventional Chinese university timetable: four mornings, four afternoons,
// two evenings.
func defaultCoursePeriods() []coursePeriod {
	raw := [][3]string{
		{"08:00", "08:45", "第1节"},
		{"08:55", "09:40", "第2节"},
		{"10:00", "10:45", "第3节"},
		{"10:55", "11:40", "第4节"},
		{"14:30", "15:15", "第5节"},
		{"15:25", "16:10", "第6节"},
		{"16:30", "17:15", "第7节"},
		{"17:25", "18:10", "第8节"},
		{"19:30", "20:15", "第9节"},
		{"20:25", "21:10", "第10节"},
	}
	out := make([]coursePeriod, 0, len(raw))
	for i, r := range raw {
		out = append(out, coursePeriod{Index: i + 1, Start: r[0], End: r[1], Label: r[2]})
	}
	return out
}

func (s *Server) coursePeriods() []coursePeriod {
	raw := s.setting("course_periods", "")
	if raw == "" {
		return defaultCoursePeriods()
	}
	var list []coursePeriod
	if err := json.Unmarshal([]byte(raw), &list); err != nil || len(list) == 0 {
		return defaultCoursePeriods()
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Index < list[j].Index })
	return list
}

func validClock(v string) bool {
	_, err := time.Parse("15:04", strings.TrimSpace(v))
	return err == nil
}

func (s *Server) handleGetCoursePeriods(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"periods": s.coursePeriods()})
}

func (s *Server) handlePutCoursePeriods(w http.ResponseWriter, r *http.Request) {
	// ⚠️ 2026-10-16 补：节次表存在 app_settings，是**全体客户端共享**的全局配置，
	//    而这条路由原来只包了 s.auth —— 任意登录用户一行请求就能把全站的课表行布局改掉。
	//    对照同项目的 admin_ai.go / skins.go / packs.go：每个管理接口都有这道闸门，
	//    这一处是漏的（不是设计如此）。
	if s.requireAdmin(w, r) {
		return
	}
	var body struct {
		Periods []coursePeriod `json:"periods"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "bad json")
		return
	}
	if len(body.Periods) == 0 || len(body.Periods) > 20 {
		apiErr(w, 400, "节次数需要在 1~20 之间")
		return
	}
	seen := map[int]bool{}
	for i := range body.Periods {
		p := &body.Periods[i]
		if p.Index <= 0 {
			p.Index = i + 1
		}
		if seen[p.Index] {
			apiErr(w, 400, "节次序号重复")
			return
		}
		seen[p.Index] = true
		if !validClock(p.Start) || !validClock(p.End) {
			apiErr(w, 400, "时间格式必须是 HH:MM")
			return
		}
		if p.Start >= p.End {
			apiErr(w, 400, "第 "+itoa(int64(p.Index))+" 节的结束时间要晚于开始时间")
			return
		}
		if strings.TrimSpace(p.Label) == "" {
			p.Label = "第" + itoa(int64(p.Index)) + "节"
		}
	}
	sort.Slice(body.Periods, func(i, j int) bool { return body.Periods[i].Index < body.Periods[j].Index })
	raw, err := json.Marshal(body.Periods)
	if err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	if err := s.setSetting("course_periods", string(raw)); err != nil {
		apiErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "periods": body.Periods})
}

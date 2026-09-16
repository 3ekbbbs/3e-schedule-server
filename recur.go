package main

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
)

// The server is the ONLY place a recurrence rule is expanded. Clients never
// link an RRULE library, which structurally removes the "Go / Kotlin / JS
// libraries disagree on BYSETPOS or leap years" class of bug.
const (
	instanceWindowPastSec   = 7 * 86400
	instanceWindowFutureSec = 90 * 86400
)

func locOf(tzid string) *time.Location {
	if strings.TrimSpace(tzid) == "" {
		tzid = "Asia/Shanghai"
	}
	l, err := time.LoadLocation(tzid)
	if err != nil {
		return time.UTC
	}
	return l
}

// occurrenceKeys expands an event into UTC unix seconds inside [from, to].
// A non-recurring event yields at most one key.
func occurrenceKeys(dtstartUTC int64, tzid, rr string, rdates, exdates []int64, from, to time.Time) ([]int64, error) {
	loc := locOf(tzid)
	rr = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rr), "RRULE:"))

	if rr == "" {
		if dtstartUTC == 0 {
			return nil, nil
		}
		t := time.Unix(dtstartUTC, 0)
		if t.Before(from) || t.After(to) {
			return nil, nil
		}
		return []int64{dtstartUTC}, nil
	}

	start := time.Unix(dtstartUTC, 0).In(loc)
	opt, err := rrule.StrToROptionInLocation(rr, loc)
	if err != nil {
		return nil, err
	}
	opt.Dtstart = start
	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, err
	}

	set := &rrule.Set{}
	set.DTStart(start)
	set.RRule(rule)
	for _, k := range rdates {
		set.RDate(time.Unix(k, 0).In(loc))
	}
	for _, k := range exdates {
		set.ExDate(time.Unix(k, 0).In(loc))
	}

	res := set.Between(from.In(loc), to.In(loc), true)
	out := make([]int64, 0, len(res))
	seen := make(map[int64]bool, len(res))
	for _, t := range res {
		u := t.Unix()
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out, nil
}

// nextOccurrenceAfter returns the first occurrence strictly after t, used to
// tell "the series has ended" apart from "the window just ran out".
func nextOccurrenceAfter(dtstartUTC int64, tzid, rr string, rdates, exdates []int64, after time.Time) (int64, bool, error) {
	loc := locOf(tzid)
	rr = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rr), "RRULE:"))
	if rr == "" {
		if dtstartUTC > after.Unix() {
			return dtstartUTC, true, nil
		}
		return 0, false, nil
	}
	start := time.Unix(dtstartUTC, 0).In(loc)
	opt, err := rrule.StrToROptionInLocation(rr, loc)
	if err != nil {
		return 0, false, err
	}
	opt.Dtstart = start
	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		return 0, false, err
	}
	set := &rrule.Set{}
	set.DTStart(start)
	set.RRule(rule)
	for _, k := range rdates {
		set.RDate(time.Unix(k, 0).In(loc))
	}
	for _, k := range exdates {
		set.ExDate(time.Unix(k, 0).In(loc))
	}
	nxt := set.After(after.In(loc), false)
	if nxt.IsZero() {
		return 0, false, nil
	}
	return nxt.Unix(), true, nil
}

type overridePatch struct {
	Title       *string `json:"title,omitempty"`
	Notes       *string `json:"notes,omitempty"`
	Location    *string `json:"location,omitempty"`
	AllDay      *bool   `json:"all_day,omitempty"`
	DTStartUTC  *int64  `json:"dtstart_utc,omitempty"`
	DurationSec *int64  `json:"duration_sec,omitempty"`
	DoneAt      *int64  `json:"done_at,omitempty"`
}

func (p overridePatch) apply(title, notes, location string, allDay bool, startUTC, dur int64, doneAt *int64) (string, string, string, bool, int64, int64, *int64) {
	if p.Title != nil {
		title = *p.Title
	}
	if p.Notes != nil {
		notes = *p.Notes
	}
	if p.Location != nil {
		location = *p.Location
	}
	if p.AllDay != nil {
		allDay = *p.AllDay
	}
	if p.DTStartUTC != nil {
		startUTC = *p.DTStartUTC
	}
	if p.DurationSec != nil {
		dur = *p.DurationSec
	}
	if p.DoneAt != nil {
		doneAt = p.DoneAt
	}
	return title, notes, location, allDay, startUTC, dur, doneAt
}

// recomputeInstances rebuilds the instance window for one event, allocating a
// new seq only for rows whose content actually changed. Without that diff a
// single 90-day daily event would burn 90 sync_log rows on every keystroke.
func recomputeInstances(tx *sql.Tx, eventID string, now time.Time) error {
	var (
		calID, tzid            string
		title, notes, location string
		allDay, isTask         int64
		dtstart                sql.NullInt64
		dur                    int64
		rr                     sql.NullString
		rdateS, exdateS        string
		deleted                sql.NullInt64
		doneAt                 sql.NullInt64
	)
	// is_task is in this list on purpose, and it used to be missing. The scan
	// targets below always had a slot for it, so leaving it out of the SELECT
	// did not fail — it just left isTask at 0 forever, which meant every
	// instance of a task reached the clients as an ordinary entry with no
	// checkbox. Nothing else writes the instances table, so this single
	// omission disabled the whole 待办 feature on every client.
	err := tx.QueryRow(`
		SELECT calendar_id, tzid, title, notes, location, all_day, is_task, dtstart_utc,
		       duration_sec, rrule, rdate, exdate, deleted_at, done_at
		FROM events WHERE id=?`, eventID).Scan(
		&calID, &tzid, &title, &notes, &location, &allDay, &isTask, &dtstart,
		&dur, &rr, &rdateS, &exdateS, &deleted, &doneAt)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}

	// --- collect the desired instance set -------------------------------
	desired := map[int64]*Instance{}

	gone := deleted.Valid || !dtstart.Valid || dtstart.Int64 == 0
	if !gone {
		from := now.Add(-instanceWindowPastSec * time.Second)
		to := now.Add(instanceWindowFutureSec * time.Second)

		var rdates, exdates []int64
		_ = json.Unmarshal([]byte(rdateS), &rdates)
		_ = json.Unmarshal([]byte(exdateS), &exdates)

		keys, expErr := occurrenceKeys(dtstart.Int64, tzid, rr.String, rdates, exdates, from, to)
		if expErr != nil {
			// A malformed rule must not take the whole sync down: fall back to
			// treating the event as single-shot so the user still sees it.
			keys = []int64{dtstart.Int64}
		}

		// overrides replace or cancel individual occurrences
		ovr := map[int64]overridePatch{}
		cancelled := map[int64]bool{}
		rows, qErr := tx.Query(`SELECT occurrence_key, patch, cancelled FROM event_overrides WHERE event_id=?`, eventID)
		if qErr == nil {
			for rows.Next() {
				var k int64
				var patchS string
				var canc int64
				if err := rows.Scan(&k, &patchS, &canc); err == nil {
					if canc != 0 {
						cancelled[k] = true
					}
					var p overridePatch
					if json.Unmarshal([]byte(patchS), &p) == nil {
						ovr[k] = p
					}
				}
			}
			rows.Close()
		}

		var donePtr *int64
		if doneAt.Valid {
			v := doneAt.Int64
			donePtr = &v
		}

		for _, k := range keys {
			if cancelled[k] {
				continue
			}
			iTitle, iNotes, iLoc, iAllDay, iStart, iDur, iDone :=
				title, notes, location, allDay != 0, k, dur, donePtr
			isOvr := false
			if p, ok := ovr[k]; ok {
				iTitle, iNotes, iLoc, iAllDay, iStart, iDur, iDone =
					p.apply(title, notes, location, allDay != 0, k, dur, donePtr)
				isOvr = true
			}
			if iDur < 0 {
				iDur = 0
			}
			desired[k] = &Instance{
				ID:            eventID + ":" + itoa(k),
				EventID:       eventID,
				OccurrenceKey: k,
				CalendarID:    calID,
				StartUTC:      iStart,
				EndUTC:        iStart + iDur,
				AllDay:        iAllDay,
				Title:         iTitle,
				Notes:         iNotes,
				Location:      iLoc,
				IsTask:        isTask != 0,
				DoneAt:        iDone,
				IsOverride:    isOvr,
			}
		}
	}

	// --- current rows ---------------------------------------------------
	type curRow struct {
		start, end             int64
		allDay, isTask, isOvr  int64
		title, notes, location string
		doneAt                 sql.NullInt64
		deleted                sql.NullInt64
	}
	cur := map[int64]curRow{}
	rows, err := tx.Query(`SELECT occurrence_key, start_utc, end_utc, all_day, title, notes,
		location, is_task, done_at, is_override, deleted_at FROM instances WHERE event_id=?`, eventID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k int64
		var r curRow
		if err := rows.Scan(&k, &r.start, &r.end, &r.allDay, &r.title, &r.notes,
			&r.location, &r.isTask, &r.doneAt, &r.isOvr, &r.deleted); err == nil {
			cur[k] = r
		}
	}
	rows.Close()

	deletedNow := now.Unix()

	// --- upsert changed / new -------------------------------------------
	for k, want := range desired {
		if r, ok := cur[k]; ok && !r.deleted.Valid {
			same := r.start == want.StartUTC && r.end == want.EndUTC &&
				r.allDay == boolToInt(want.AllDay) && r.title == want.Title &&
				r.notes == want.Notes && r.location == want.Location &&
				r.isTask == boolToInt(want.IsTask) && r.isOvr == boolToInt(want.IsOverride) &&
				nullEq(r.doneAt, want.DoneAt)
			if same {
				continue
			}
		}
		seq, err := nextSeq(tx, "instance", want.ID, "upsert")
		if err != nil {
			return err
		}
		want.Seq = seq
		if _, err := tx.Exec(`
			INSERT INTO instances(id,event_id,occurrence_key,calendar_id,start_utc,end_utc,all_day,
				title,notes,location,is_task,done_at,is_override,seq,deleted_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)
			ON CONFLICT(id) DO UPDATE SET
				start_utc=excluded.start_utc, end_utc=excluded.end_utc, all_day=excluded.all_day,
				title=excluded.title, notes=excluded.notes, location=excluded.location,
				is_task=excluded.is_task, done_at=excluded.done_at,
				is_override=excluded.is_override, seq=excluded.seq, deleted_at=NULL`,
			want.ID, want.EventID, want.OccurrenceKey, want.CalendarID, want.StartUTC, want.EndUTC,
			boolToInt(want.AllDay), want.Title, want.Notes, want.Location,
			boolToInt(want.IsTask), want.DoneAt, boolToInt(want.IsOverride), seq); err != nil {
			return err
		}
	}

	// --- tombstone rows that no longer exist ----------------------------
	for k, r := range cur {
		if r.deleted.Valid {
			continue
		}
		if _, ok := desired[k]; ok {
			continue
		}
		seq, err := nextSeq(tx, "instance", idOf(eventID, k), "delete")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE instances SET deleted_at=?, seq=? WHERE event_id=? AND occurrence_key=?`,
			deletedNow, seq, eventID, k); err != nil {
			return err
		}
	}
	return nil
}

func idOf(eventID string, k int64) string { return eventID + ":" + itoa(k) }

func nullEq(a sql.NullInt64, b *int64) bool {
	if !a.Valid {
		return b == nil
	}
	return b != nil && a.Int64 == *b
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

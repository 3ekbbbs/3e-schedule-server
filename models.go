package main

// ---------- wire types (JSON shape shared by Android + Electron) ----------

type User struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsAdmin   bool   `json:"is_admin"`
	CreatedAt int64  `json:"created_at"`
}

type Member struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Role   string `json:"role"`
}

type Calendar struct {
	ID        string   `json:"id"`
	OwnerID   string   `json:"owner_id"`
	Name      string   `json:"name"`
	Color     string   `json:"color"`
	Kind      string   `json:"kind"`
	Role      string   `json:"role"`
	Members   []Member `json:"members,omitempty"`
	Seq       int64    `json:"seq"`
	FieldSeqs jsonMap  `json:"field_seqs,omitempty"`
	UpdatedAt int64    `json:"updated_at"`
	CreatedAt int64    `json:"created_at"`
	DeletedAt *int64   `json:"deleted_at"`
	OwnerName string   `json:"owner_name,omitempty"`
}

type Reminder struct {
	ID            string `json:"id"`
	EventID       string `json:"event_id"`
	MinutesBefore int64  `json:"minutes_before"`
	Channel       string `json:"channel"`
	Seq           int64  `json:"seq"`
	DeletedAt     *int64 `json:"deleted_at"`
}

type Event struct {
	ID          string  `json:"id"`
	CalendarID  string  `json:"calendar_id"`
	UID         string  `json:"uid"`
	Title       string  `json:"title"`
	Notes       string  `json:"notes"`
	Location    string  `json:"location"`
	AllDay      bool    `json:"all_day"`
	DTStartUTC  int64   `json:"dtstart_utc"`
	DurationSec int64   `json:"duration_sec"`
	TZID        string  `json:"tzid"`
	RRule       string  `json:"rrule"`
	RDate       []int64 `json:"rdate"`
	ExDate      []int64 `json:"exdate"`
	IsTask      bool    `json:"is_task"`
	DoneAt      *int64  `json:"done_at"`
	// **不要给 reminders 加 omitempty。**
	// 空列表是有意义的（"这条不提醒我"），一旦被省略，客户端读到的就是
	// undefined —— 分不清"服务端说没有提醒"和"这个字段没下发"，
	// 于是又会退回"默认提前 15 分钟"的老行为。
	Reminders []Reminder `json:"reminders"`
	// 强提醒：**调用者自己**对这条日程开没开。
	// 和 reminders 一样是按调用者过滤过的值，不是共享字段。
	//
	// **同样不能加 omitempty。** false 是有意义的确定状态（明确关闭），
	// 一旦被省略，客户端读到 undefined，就分不清"服务端说关"和"这个字段没下发"
	// —— 与 Reminders 上面那条是同一个道理。
	Strong    bool    `json:"strong"`
	Seq       int64   `json:"seq"`
	FieldSeqs jsonMap `json:"field_seqs,omitempty"`
	UpdatedAt int64   `json:"updated_at"`
	CreatedAt int64   `json:"created_at"`
	DeletedAt *int64  `json:"deleted_at"`
}

type Instance struct {
	ID            string `json:"id"`
	EventID       string `json:"event_id"`
	OccurrenceKey int64  `json:"occurrence_key"`
	CalendarID    string `json:"calendar_id"`
	StartUTC      int64  `json:"start_utc"`
	EndUTC        int64  `json:"end_utc"`
	AllDay        bool   `json:"all_day"`
	Title         string `json:"title"`
	Notes         string `json:"notes"`
	Location      string `json:"location"`
	IsTask        bool   `json:"is_task"`
	DoneAt        *int64 `json:"done_at"`
	IsOverride    bool   `json:"is_override"`
	Seq           int64  `json:"seq"`
	DeletedAt     *int64 `json:"deleted_at"`
}

type Snooze struct {
	ID            string `json:"id"`
	EventID       string `json:"event_id"`
	OccurrenceKey int64  `json:"occurrence_key"`
	UserID        string `json:"user_id"`
	RemindAtUTC   int64  `json:"remind_at_utc"`
	DeviceID      string `json:"device_id"`
	Seq           int64  `json:"seq"`
	DeletedAt     *int64 `json:"deleted_at"`
}

type Conflict struct {
	ID           string  `json:"id"`
	Entity       string  `json:"entity"`
	EntityID     string  `json:"entity_id"`
	Field        string  `json:"field"`
	BaseSeq      int64   `json:"base_seq"`
	ServerValue  *string `json:"server_value"`
	ClientValue  *string `json:"client_value"`
	ClientDevice string  `json:"client_device"`
	CreatedAt    int64   `json:"created_at"`
	ResolvedAt   *int64  `json:"resolved_at"`
	Resolution   *string `json:"resolution"`
}

// ---------- scalars that are legal to field-merge ----------
//
// Field-level merge needs to know how to coerce an incoming JSON value back
// into the SQLite column type, so the whitelist carries a kind, not just a
// boolean. Anything absent here is immutable (id/calendar_id/created_at) or
// derived (seq/updated_at).

const (
	kindText = "text"
	kindInt  = "int"
	kindBool = "bool"
	kindJSON = "json"
)

var eventFieldKinds = map[string]string{
	"title":        kindText,
	"notes":        kindText,
	"location":     kindText,
	"all_day":      kindBool,
	"dtstart_utc":  kindInt,
	"duration_sec": kindInt,
	"tzid":         kindText,
	"rrule":        kindText,
	"rdate":        kindJSON,
	"exdate":       kindJSON,
	"is_task":      kindBool,
	"done_at":      kindInt,
}

var calendarFieldKinds = map[string]string{
	"name":  kindText,
	"color": kindText,
	"kind":  kindText,
}

func kindMapFor(entity string) map[string]string {
	if entity == "calendar" {
		return calendarFieldKinds
	}
	return eventFieldKinds
}

// tableFor maps a sync entity name to its table. `instance` and `snooze` are
// server-managed and never accept a client field merge.
func tableFor(entity string) string {
	switch entity {
	case "calendar":
		return "calendars"
	case "snooze":
		return "snoozes"
	default:
		return "events"
	}
}

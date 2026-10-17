package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// 实时协作票据（2026-10-10，2026-10-15 整合后重写）
//
// ## ★ 2026-10-15：协作服务**已经并进本进程**，这张票仍然要留着
//
// 整合之前，协作是一个**独立部署**的服务（另一台机器、:8445），所以票据必须
// 是"自包含 HMAC 签名"——因为协作端读不到主库。
//
// 整合之后，签票和验票在**同一个进程**里，看起来"直接查库就行了"。**不这么做**，
// 理由有两条，第二条是硬的：
//
//   1. **客户端契约不能变**。手机端的 `plugin-mdnote-live` 只拿得到票据
//      （`HostImpl.currentUser()` 刻意不给 token），改契约等于所有已发布版本同时失效。
//   2. **票据是一条权限边界，不是缓存**。`/api/live/*` 和 `/live` 走的是
//      `withTicket` 而不是 `s.auth`：**插件手里永远没有登录 token**，
//      它只可能拿到一张 5 分钟、绑定具体用户、只对协作有效的票。
//      这条边界是插件安全模型的一部分，跟部署形态无关。
//
// 于是验票那一份实现**只剩一处**（本文件的 `verifyLiveTicket`）——
// 整合前它在 `cmd/live/ticket.go` 里有一份"逐字抄过去"的拷贝，
// 两份必须手工保持同步；现在那个隐患从结构上消失了。
//
// ## 票据长什么样
//
//     票据 = lt_<payload 的 base64url>.<HMAC-SHA256(payload, 共享密钥) 的 base64url>
//     payload = {"u":"用户id","n":"用户名","e":过期毫秒,"i":签发毫秒}
//
// ⚠️ 有效期内**无法单独吊销**，所以有效期必须短（5 分钟）。
//    后来人**不要**为了"少换几次票"把它拉长。
// ============================================================================

// 票据存活时间。⚠️ 不要拉长，理由见上。
const liveTicketTTL = 5 * time.Minute

// 协作服务的地址（存 app_settings，可改）。
//
// ★ 2026-10-15 起默认值是**空**，含义是"和主服务器同源"：
//   客户端换票时拿到 `host=""`，就用它自己配的那台服务器地址去连
//   （同一个端口、同一张证书、同一个 mux）。
//
// ⚠️ 留一个"能指到别处"的口子是有意的：把协作拆出去独立部署仍然是合法需求
//    （比如把长连接放到一台带宽更便宜的机器上）。填了 host/port 就按填的走。
const (
	settingLiveHost   = "live_host"
	settingLivePort   = "live_port"
	settingLiveSecret = "live_secret"

	// 默认 = 同源（空）。整合前这里是 203.0.113.11:8445 —— 那是第二台服务器。
	defaultLiveHost = ""
	defaultLivePort = ""
)

// liveEndpoint 返回协作服务当前该连哪儿。
//
// `sameOrigin=true` 时 host/port 是空的 —— 客户端应当用它自己配置的那台
// 服务器地址去连（同一个端口、同一张证书）。2026-10-15 整合后的默认形态。
func (s *Server) liveEndpoint() (host, port string, sameOrigin bool) {
	host = strings.TrimSpace(s.setting(settingLiveHost, defaultLiveHost))
	port = strings.TrimSpace(s.setting(settingLivePort, defaultLivePort))
	return host, port, host == "" && port == ""
}

// liveSecret 取共享密钥（签票 + 验票用同一个）。
//
// 优先级：app_settings 里配的 → 启动参数 -live-secret → 现场生成一个并落库。
//
// ⚠️ 整合进同进程之后这个密钥**不再需要和第二台机器对齐**，但仍然必须落库：
//    不落库就会每次重启换一个，客户端手里还没过期的票会全部作废
//    （表现是"重连了一下就再也连不上，重启服务端才好"）。
func (s *Server) liveSecret() string {
	if v := strings.TrimSpace(s.setting(settingLiveSecret, "")); v != "" {
		return v
	}
	if v := strings.TrimSpace(s.cfg.LiveSecret); v != "" {
		_ = s.setSetting(settingLiveSecret, v)
		return v
	}
	v := "ls_" + newToken()
	_ = s.setSetting(settingLiveSecret, v)
	return v
}

// liveTicketPayload 是票据里装的东西。
//
// ⚠️ 字段名刻意短（u/n/e/i）：票据会出现在 URL query 里，
//	每个字符都直接变成 URL 长度；短名不影响可读性（这里有注释）。
type liveTicketPayload struct {
	U string `json:"u"` // user id
	N string `json:"n"` // 用户名（协作服务端显示"谁改的"用）
	E int64  `json:"e"` // 过期时间（毫秒）
	I int64  `json:"i"` // 签发时间（排查"这张票什么时候换的"）
}

// signLiveTicket 签发一张票据。
func (s *Server) signLiveTicket(uid, name string, now time.Time) string {
	p := liveTicketPayload{U: uid, N: name, E: now.Add(liveTicketTTL).UnixMilli(), I: now.UnixMilli()}
	body, _ := json.Marshal(p)
	enc := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(s.liveSecret()))
	mac.Write([]byte(enc))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return "lt_" + enc + "." + sig
}

// verifyLiveTicket 是**验签的权威实现**。
//
// ⚠️ 协作服务端那边有一份**逐字抄过去**的拷贝（`server/cmd/live/ticket.go`）。
//	两份必须保持一致 —— 改了签名格式两边都要改，否则表现是
//	"票换出来了但连不上，还报'票据无效'"，看着像密钥配错了。
//	格式刻意写得极简（两段 base64 + 一个点），就是为了让抄写不出错。
func verifyLiveTicket(ticket, secret string, now int64) (liveTicketPayload, bool) {
	var p liveTicketPayload
	ticket = strings.TrimSpace(ticket)
	if !strings.HasPrefix(ticket, "lt_") {
		return p, false
	}
	rest := ticket[3:]
	dot := strings.LastIndex(rest, ".")
	if dot <= 0 || dot == len(rest)-1 {
		return p, false
	}
	body, sig := rest[:dot], rest[dot+1:]

	// 1) **先验签再解 payload** —— 顺序反了的话，未经验证的输入会进 JSON 解析，
	//    那是典型的"把不可信输入当可信"。
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return p, false
	}

	// 2) 解 payload
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return p, false
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, false
	}

	// 3) 判过期 —— 用 **payload 里**的时间，不信任客户端传的任何东西
	if p.E <= now || p.U == "" {
		return p, false
	}
	return p, true
}

// POST /api/live/secret —— 管理员设置共享密钥（**必须和第二台一致**）。
//
// ## 为什么要有这个接口
//
// 密钥的另一种给法是"启动参数 -live-secret"，但那要求**改计划任务的命令行** ——
// 而服务器上的计划任务参数是部署时写死的，改它要动那边的配置。
// 有个运行时接口就简单得多：部署完直接 curl 一下。
//
// ⚠️ 只对管理员开放，而且**只写不回读**（GET 那边只说"配了没有"）。
func (s *Server) handleLiveSetSecret(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var admin int64
	if err := s.db.sql.QueryRow(`SELECT is_admin FROM users WHERE id=?`, uid).Scan(&admin); err != nil || admin == 0 {
		apiErr(w, 403, "只有管理员能改实时协作的密钥")
		return
	}
	var body struct {
		Secret string `json:"secret"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	body.Secret = strings.TrimSpace(body.Secret)
	if body.Secret == "" {
		apiErr(w, 400, "secret 不能为空")
		return
	}
	if err := s.setSetting(settingLiveSecret, body.Secret); err != nil {
		apiErr(w, 500, "保存失败")
		return
	}
	s.cfg.LiveSecret = body.Secret
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"hint": "已保存。2026-10-15 起协作服务与主服务**同进程**，这个密钥只在本进程内用" +
			"（签票 + 验票），不需要再和别的机器对齐。改它会让正在协作的人掉线（票据验不过）。",
	})
}

// GET /api/live/users —— 列出服务器上的用户（名字 + id）。
//
// ## 为什么这个接口在**主服务端**而不是协作服务端
//
// 用户表在主库（`users`），而协作服务端在**另一台机器**上、**不查任何库**
// （它的鉴权是纯验签，见那个服务的说明）。所以"名字 ↔ id"这个映射只有这里做得了。
//
// 协作的界面要"加人"，用户输入的是名字，而接口要的是 id —— 中间这一步就靠它。
//
// ## ⚠️ 用**票据**鉴权，不是登录 token
//
// 调它的是**插件**，而插件拿不到 token。票据本来只给协作服务端用，
// 这里复用一下：**能验签就说明它是我们签发的**，而 payload 里带着 uid/name，
// 身份是可信的。
func (s *Server) handleLiveUsersByTicket(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		tok = strings.TrimSpace(r.Header.Get("X-Live-Ticket"))
	}
	if tok == "" {
		tok = r.URL.Query().Get("ticket")
	}
	p, ok := verifyLiveTicket(tok, s.liveSecret(), time.Now().UnixMilli())
	if !ok {
		apiErr(w, 401, "票据无效或已过期（票据只有 5 分钟）")
		return
	}

	rows, err := s.db.sql.Query(`SELECT id, name, is_admin FROM users ORDER BY created_at`)
	if err != nil {
		apiErr(w, 500, "读用户列表失败")
		return
	}
	defer rows.Close()
	type u struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		IsAdmin bool   `json:"isAdmin"`
		IsMe    bool   `json:"isMe"`
	}
	out := []u{}
	for rows.Next() {
		var x u
		var admin int64
		if err := rows.Scan(&x.ID, &x.Name, &admin); err != nil {
			continue
		}
		x.IsAdmin = admin != 0
		x.IsMe = x.ID == p.U
		out = append(out, x)
	}
	// ⚠️ 这里返回的是**全体成员的名字**。群组本身是刻意做小的（见 handleUsers
	//    那段的注释），所以不额外做可见性裁剪 —— 但要记住这是个人信息。
	writeJSON(w, 200, map[string]any{"ok": true, "users": out, "me": p.U})
}

// GET /api/admin/live —— 看实时协作的配置（管理员）。
//
// 给**网页端管理控制台**用。以前只能改 `app_settings` 里的键（得上服务器），
// 或者重启时带启动参数 —— 用户明确要求把这件事放进管理台。
func (s *Server) handleAdminLiveGet(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var admin int64
	if err := s.db.sql.QueryRow(`SELECT is_admin FROM users WHERE id=?`, uid).Scan(&admin); err != nil || admin == 0 {
		apiErr(w, 403, "只有管理员能看这个")
		return
	}
	host, port, sameOrigin := s.liveEndpoint()
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"host":      host,
		"port":      port,
		"sameOrigin": sameOrigin,
		"secretSet": strings.TrimSpace(s.setting(settingLiveSecret, "")) != "",
		// 默认值也给出来 —— 面板上要能显示"留空就用这个"
		"defaultHost": defaultLiveHost,
		"defaultPort": defaultLivePort,
		"hint": "留空 = 与主服务器同源（推荐）。填了地址/端口就按填的走，" +
			"用于把长连接拆到另一台机器上。",
	})
}

// POST /api/admin/live —— 改实时协作服务的地址/端口（管理员）。
//
// ⚠️ **只改地址端口，不提供改密钥**（密钥要用 `POST /api/live/secret`）——
//    因为改密钥会让**所有正在协作的人立刻掉线**（票据验不过），
//    那不是一件该和"改地址"并排放在同一个保存按钮里的事。
func (s *Server) handleAdminLiveSet(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	var admin int64
	if err := s.db.sql.QueryRow(`SELECT is_admin FROM users WHERE id=?`, uid).Scan(&admin); err != nil || admin == 0 {
		apiErr(w, 403, "只有管理员能改这个")
		return
	}
	var body struct {
		Host string `json:"host"`
		Port string `json:"port"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	// ★ 空 = 同源（2026-10-15 起的推荐形态）。**不要**再把空值填回某个默认 IP：
	//    整合之后那个默认值已经没有对应的部署了，填回去会让所有客户端
	//    去连一台不存在的机器，而报错是"连不上协作服务"。
	host := strings.TrimSpace(body.Host)
	port := strings.TrimSpace(body.Port)
	// 端口必须是纯数字 —— 塞进 URL 的东西要让它在**存之前**就是干净的，
	// 否则客户端换票时会拿到一个拼不出来的地址（而报错在那头，很难查回来）。
	// 允许留空（= 同源，端口跟主服务走）。
	if port != "" {
		for _, c := range port {
			if c < '0' || c > '9' {
				apiErr(w, 400, "端口只能是数字（留空 = 与主服务器同源）")
				return
			}
		}
	}
	if host == "" && port != "" {
		apiErr(w, 400, "只填端口不填地址没有意义：留空两个字段 = 与主服务器同源")
		return
	}
	if err := s.setSetting(settingLiveHost, host); err != nil {
		apiErr(w, 500, "保存失败")
		return
	}
	if err := s.setSetting(settingLivePort, port); err != nil {
		apiErr(w, 500, "保存失败")
		return
	}
	hint := "已保存。客户端下次换票就会用新地址（**不用重启服务**）。"
	if host == "" {
		hint = "已保存：与主服务器同源（客户端用自己配的那台服务器地址连）。**不用重启服务**。"
	}
	writeJSON(w, 200, map[string]any{
		"ok":   true,
		"host": host,
		"port": port,
		"hint": hint,
	})
}

// POST /api/live/ticket —— 换一张协作票据。需要登录（走 s.auth）。
func (s *Server) handleLiveTicket(w http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	if uid == "" {
		apiErr(w, 401, "请先登录")
		return
	}
	var name string
	if err := s.db.sql.QueryRow(`SELECT name FROM users WHERE id=?`, uid).Scan(&name); err != nil || name == "" {
		name = uid
	}

	now := time.Now()
	ticket := s.signLiveTicket(uid, name, now)

	host, port, sameOrigin := s.liveEndpoint()

	hint := "用 wss://<host>:<port>/live?ticket=<ticket> 连接；票据 5 分钟内有效，期间可重连。"
	if sameOrigin {
		// ⚠️ 这一句要写死在这份响应里：客户端**不能**拿 host 去拼 URL，
		//    空串拼出来的是 "wss://:0/live"，报错看起来像"服务器挂了"。
		hint = "host 为空 = 与主服务器同源：用你配置的那台服务器地址（同一个端口）连 wss://<你的服务器>/live?ticket=<ticket>。" +
			"票据 5 分钟内有效，期间可重连。"
	}
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"ticket":     ticket,
		"expiresAt":  now.Add(liveTicketTTL).UnixMilli(),
		"ttlSeconds": int(liveTicketTTL.Seconds()),
		"host":       host,
		"port":       port,
		"sameOrigin": sameOrigin,
		"hint":       hint,
	})
}

// GET /api/live/config —— 看一眼协作服务端的配置（要登录）。
//
// ⚠️ 密钥**不回显内容**，只说"配了没有" —— 它只需要配在第二台上，
//	客户端永远用不到它。
func (s *Server) handleLiveConfig(w http.ResponseWriter, r *http.Request) {
	host, port, sameOrigin := s.liveEndpoint()
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"host":       host,
		"port":       port,
		"sameOrigin": sameOrigin,
		"secretSet":  strings.TrimSpace(s.setting(settingLiveSecret, "")) != "",
		"enabled":    liveReady(),
	})
}

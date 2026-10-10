package main

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
// 实时协作票据（2026-10-10）
//
// ## 这张票是干什么的
//
// 「实时笔记协作」是一个**独立部署**的服务端，跑在**另一台**机器上（默认 :8445）。
// 客户端要用 WebSocket 连它，就得先证明"我是谁"。
//
// 但这里有个硬约束：**插件拿不到登录 token**。
// `HostImpl.currentUser()` 刻意只给 `id/name/isAdmin`，不含 token ——
// 于是插件没有办法直接去连协作服务端。
//
// 所以主服务端签发一张**短期票据**：
//
//     主程序（有 token）──换票──► 主服务端 ──► lt_xxx（5 分钟）
//                                                  │
//                                   插件拿到票据 ──┘
//                                                  │
//                              WebSocket 握手带票据 ──► 协作服务端（验签）
//
// ## ★ 为什么是 **HMAC 自包含**，而不是"存库 + 对端查库"
//
// 第一版设计是"票据写进主库，协作服务端只读主库校验"。那个方案在
// **同一台机器**上成立，但实际部署是**两台服务器**（用户有两台：主服务一台、
// 笔记/协作一台）—— 主库不在协作服务端本地，那个方案直接不成立。
//
// 改成自包含签名之后：
//
//     票据 = lt_<payload 的 base64url>.<HMAC-SHA256(payload, 共享密钥) 的 base64url>
//
// · **无状态**：协作服务端只验签，不查任何库
// · **跨机器**：两边只要共享一个密钥（部署时配一次）
// · **自带身份**：payload 里就有 user_id 和用户名 —— 协作服务端要显示"谁改的"，
//   不用再去查用户表（那又是一次跨机器依赖）
// · **过期**由 payload 里的 `e` 决定，验签时判，不需要清理任务
//
// ⚠️ 代价：**票据在有效期内无法单独吊销**（不像存库那样能删一行）。
//    所以有效期必须短（5 分钟）—— 这是这套方案成立的前提，不是保守。
//    后来人**不要**为了"少换几次票"把它拉长。
// ============================================================================

// 票据存活时间。⚠️ 不要拉长，理由见上。
const liveTicketTTL = 5 * time.Minute

// 协作服务端的地址（存 app_settings，可改）。
//
// ⚠️ 客户端**不写死**这两个值 —— 服务器换 IP / 换端口时，
//
//	让所有客户端跟着改是不可能的。所以由服务端告诉他们。
const (
	settingLiveHost   = "live_host"
	settingLivePort   = "live_port"
	settingLiveSecret = "live_secret"

	// 默认指向**第二台服务器**（协作服务部署在那里）。
	// 第一台是 203.0.113.10（example.com，主服务）。
	defaultLiveHost = "203.0.113.11"
	defaultLivePort = "8445"
)

// liveSecret 取共享密钥（和协作服务端共用的那一个）。
//
// 优先级：app_settings 里配的 → 启动参数 -live-secret → 现场生成一个并落库。
//
// ⚠️ "现场生成"只解决了主服务端这一半 —— 还要把同一个密钥配到第二台上，
//	否则表现是"票换出来了，但协作服务端报'票据无效'"（看着像密钥配错，
//	其实**就是**密钥没配）。
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
		"ok":   true,
		"hint": "已保存。确认第二台协作服务端的启动脚本里是**同一个**密钥，否则换出来的票会被判无效。",
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
	host := strings.TrimSpace(s.setting(settingLiveHost, defaultLiveHost))
	port := strings.TrimSpace(s.setting(settingLivePort, defaultLivePort))
	if host == "" {
		host = defaultLiveHost
	}
	if port == "" {
		port = defaultLivePort
	}
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"host":      host,
		"port":      port,
		"secretSet": strings.TrimSpace(s.setting(settingLiveSecret, "")) != "",
		// 默认值也给出来 —— 面板上要能显示"留空就用这个"
		"defaultHost": defaultLiveHost,
		"defaultPort": defaultLivePort,
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
	host := strings.TrimSpace(body.Host)
	port := strings.TrimSpace(body.Port)
	if host == "" {
		host = defaultLiveHost
	}
	if port == "" {
		port = defaultLivePort
	}
	// 端口必须是纯数字 —— 塞进 URL 的东西要让它在**存之前**就是干净的，
	// 否则客户端换票时会拿到一个拼不出来的地址（而报错在那头，很难查回来）
	for _, c := range port {
		if c < '0' || c > '9' {
			apiErr(w, 400, "端口只能是数字")
			return
		}
	}
	if err := s.setSetting(settingLiveHost, host); err != nil {
		apiErr(w, 500, "保存失败")
		return
	}
	if err := s.setSetting(settingLivePort, port); err != nil {
		apiErr(w, 500, "保存失败")
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":   true,
		"host": host,
		"port": port,
		"hint": "已保存。客户端下次换票就会用新地址（**不用重启服务**）。",
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

	host := strings.TrimSpace(s.setting(settingLiveHost, defaultLiveHost))
	port := strings.TrimSpace(s.setting(settingLivePort, defaultLivePort))
	if host == "" {
		host = defaultLiveHost
	}
	if port == "" {
		port = defaultLivePort
	}

	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"ticket":     ticket,
		"expiresAt":  now.Add(liveTicketTTL).UnixMilli(),
		"ttlSeconds": int(liveTicketTTL.Seconds()),
		"host":       host,
		"port":       port,
		"hint":       "用 wss://<host>:<port>/live?ticket=<ticket> 连接；票据 5 分钟内有效，期间可重连。",
	})
}

// GET /api/live/config —— 看一眼协作服务端的配置（要登录）。
//
// ⚠️ 密钥**不回显内容**，只说"配了没有" —— 它只需要配在第二台上，
//	客户端永远用不到它。
func (s *Server) handleLiveConfig(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimSpace(s.setting(settingLiveHost, defaultLiveHost))
	port := strings.TrimSpace(s.setting(settingLivePort, defaultLivePort))
	if host == "" {
		host = defaultLiveHost
	}
	if port == "" {
		port = defaultLivePort
	}
	writeJSON(w, 200, map[string]any{
		"ok":        true,
		"host":      host,
		"port":      port,
		"secretSet": strings.TrimSpace(s.setting(settingLiveSecret, "")) != "",
	})
}

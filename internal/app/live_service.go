// 实时笔记协作服务端（2026-10-10）。
//
// 独立进程、独立端口（默认 :8445），与主服务端（:8443）同机部署但不是同一个程序。
//
//	主服务端     负责：身份、日程、团队、部署 …… 以及**签发票据**
//	本服务       负责：长连接、房间、行级同步、冲突判定、权限
//
// ## 启动
//
//	schedule-server.exe -addr :8445 -data ../data -livedb ./live.db -certs ../certs
//
// `-data` 指向**主服务的数据目录**（只读用，里面是 schedule.db）；
// `-livedb` 是协作自己的库。
//
// ## 一次会话长什么样
//
//	客户端                 主服务:8443            本服务:8445
//	  │ POST /api/live/ticket  │                      │
//	  │ ───────────────────────►│                      │
//	  │ ◄─── lt_xxx（5 分钟）  │                      │
//	  │                                                │
//	  │ WS /live?ticket=lt_xxx                         │
//	  │ ──────────────────────────────────────────────►│
//	  │                                      校验票据（只读主库）
//	  │ ◄─── welcome{rev, lines[], acl, peers[]}      │
//	  │ ──── edit{line, text, baseRev} ───────────────►│
//	  │ ◄─── apply（广播给房间里的其他人）              │
//	  │ ◄─── conflict（如果有人先改了同一行）           │
package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// 实时协作的运行时（2026-10-15：**从独立进程整合进主服务端**）。
//
// ## 为什么整合
//
// 之前它跑在**第二台服务器**上（:8445），主服务端只负责签票据、告诉客户端去哪台机器。
// 那个形态带来三个持续成本：
//   · 部署面多一个进程、多一张证书、多一份计划任务；
//   · 票据的 HMAC 密钥要在两台机器上手工配一致（配错的表现是"票换出来了但连不上"）；
//   · 而"个人备案应用"这条合规线要求**不依赖第三方服务器**。
//
// 整合之后：同一个进程、同一个端口（:8443）、同一张证书、同一个密钥（`liveSecret()`）。
// 客户端的 `live_host` / `live_port` 留空 = 与主服务器同源，见 `live_ticket.go`。
//
// ## 数据仍然是独立的库
//
// 协作的文档写在 `data/live.db`（自己的 SQLite），**不并进 schedule.db**。
// 理由没变：主库有一套自己的迁移机制，而且 SQLite 是单写者 ——
// 两套表混在一起只会让"改协作的表结构"变成"动主库的迁移"。
// 同进程不改变这一点，只是省掉了跨机器的密钥同步。
var (
	liveAuth  *TicketAuth
	liveStore *Store
	liveHub   *Hub
)

// startLiveService 打开协作库、建房间表。
//
// ⚠️ 失败**不致命**：协作库打不开（磁盘满、文件被占）时，主服务端的日程/团队/
// 插件分发全部照常 —— 只有"实时协作"这一个功能不可用。启动时打一行日志说明，
// 接口那边返回人话错误（`liveReady` 为 false 时）。
func (s *Server) startLiveService() {
	liveAuth = newTicketAuth(s.liveSecret())

	st, err := openStore(filepath.Join(s.cfg.DataDir, "live.db"))
	if err != nil {
		log.Printf("live: 协作库打不开（%v）—— 实时协作不可用，其余功能不受影响", err)
		return
	}
	liveStore = st
	liveHub = newHub()
	log.Printf("live: 实时协作已并入主服务端（库 %s）", filepath.Join(s.cfg.DataDir, "live.db"))
}

// liveReady 说明协作这一块能不能用。
func liveReady() bool { return liveStore != nil && liveHub != nil && liveAuth != nil }

// registerLiveRoutes 把协作的接口挂到主 mux 上。
//
// ⚠️ 路径**刻意保留 `/api/live/...` 这个前缀**（和整合前完全一致）：
//    客户端、插件、管理台都已经在用它，改路径等于让所有已发布的版本同时失效。
func (s *Server) registerLiveRoutes(mux *http.ServeMux) {
	// ---- WebSocket 主入口 ----
	mux.HandleFunc("GET /live", handleWS)

	// ---- REST（都用票据鉴权，因为插件只有票据）----
	mux.HandleFunc("GET /api/live/docs", withTicket(handleListDocs))
	mux.HandleFunc("POST /api/live/share", withTicket(handleShare))
	mux.HandleFunc("POST /api/live/acl", withTicket(handleACL))
	mux.HandleFunc("POST /api/live/rename", withTicket(handleRename))
	mux.HandleFunc("POST /api/live/drop", withTicket(handleDrop))
	mux.HandleFunc("GET /api/live/stats", withTicket(handleStats))
}


// =========================================================================== 鉴权

// liveUser 从请求里取票据对应的用户（写请求头、也接受 query —— 调试方便）。
func liveUser(r *http.Request) (string, string) {
	t := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if t == "" {
		t = strings.TrimSpace(r.Header.Get("X-Live-Ticket"))
	}
	if t == "" {
		t = r.URL.Query().Get("ticket")
	}
	return liveAuth.byTicket(t)
}

// withTicket 是一个"必须有有效票据"的中间件。
func withTicket(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ⚠️ 协作库没打开时必须**在这里**拦住：下面的 handler 会直接解引用
		//    `liveStore`，让它去空指针崩一次，用户看到的是"服务端 500"，
		//    而不是"这个功能没开"。
		if !liveReady() {
			apiErr(w, 503, "实时协作没有启用：服务端的协作库没打开（看启动日志里 live: 那一行）")
			return
		}
		uid, _ := liveUser(r)
		if uid == "" {
			apiErr(w, 401, "票据无效或已过期（票据只有 5 分钟，请重新换一张）")
			return
		}
		next(w, r, uid)
	}
}

// =========================================================================== WebSocket

func handleWS(w http.ResponseWriter, r *http.Request) {
	if !liveReady() {
		apiErr(w, 503, "实时协作没有启用：服务端的协作库没打开")
		return
	}
	uid, usrName := liveUser(r)
	if uid == "" {
		// ⚠️ 这里还**没有**升级，所以可以正常回 HTTP 错误码 ——
		//    客户端能拿到 401 而不是"连接莫名其妙断了"。
		apiErr(w, 401, "票据无效或已过期")
		return
	}
	conn, br, err := wsUpgrade(w, r)
	if err != nil {
		log.Printf("ws 升级失败：%v", err)
		return
	}

	c := &Client{
		conn: conn, br: br, uid: uid, name: usrName, hub: liveHub,
	}
	defer func() {
		liveHub.leave(c)
		wsClose(conn)
	}()

	log.Printf("ws 连上：%s（%s）", c.name, uid)

	for {
		// ⚠️ 读超时 = 心跳检测。客户端每 30 秒 ping 一次，
		//    90 秒没动静就认定它死了 —— 没有这个，"拔网线"的连接会永远留在房间表里，
		//    而 presence 会一直显示一个不在的人。
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, payload, err := wsReadFrame(br, conn)
		if err != nil {
			log.Printf("ws 断开：%s（%v）", c.name, err)
			return
		}
		handleMessage(c, payload)
	}
}

func handleMessage(c *Client, payload []byte) {
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		c.send(map[string]any{"t": "error", "reason": "消息不是合法 JSON"})
		return
	}
	now := time.Now().UnixMilli()
	switch s(m["t"]) {
	case "hello":
		handleHello(c, m)
	case "edit":
		handleEdit(c, m, now)
	case "insert":
		handleInsert(c, m, now)
	case "delete":
		handleDelete(c, m, now)
	case "acl":
		handleACLMessage(c, m, now)
	case "ping":
		c.send(map[string]any{"t": "pong", "time": now})
	default:
		c.send(map[string]any{"t": "error", "reason": "不认识的消息类型 " + s(m["t"])})
	}
}

func handleHello(c *Client, m map[string]any) {
	docID := strings.TrimSpace(s(m["doc"]))
	if docID == "" {
		c.send(map[string]any{"t": "denied", "reason": "没有指定文档"})
		return
	}
	role, err := liveStore.roleOf(docID, c.uid)
	if err != nil {
		c.send(map[string]any{"t": "denied", "reason": "读取权限失败"})
		return
	}
	if !roleCanRead(role) {
		// ⚠️ 不区分"文档不存在"和"你没权限" —— 区分了等于告诉外面
		//    "这个 id 是存在的"，那是信息泄露。
		c.send(map[string]any{"t": "denied", "reason": "这篇笔记没有共享给你（或已取消共享）"})
		return
	}

	// 换房间
	if c.docID != "" && c.docID != docID {
		liveHub.leave(c)
	}
	c.docID = docID
	c.role = role
	liveHub.join(c)

	lines, err := liveStore.loadLines(docID)
	if err != nil {
		c.send(map[string]any{"t": "error", "reason": "读取内容失败"})
		return
	}
	title, _ := liveStore.docTitle(docID)

	var rev int64
	_ = liveStore.db.QueryRow(`SELECT rev FROM live_doc WHERE doc_id=?`, docID).Scan(&rev)

	c.send(map[string]any{
		"t": "welcome", "doc": docID, "title": title, "rev": rev,
		"lines": lines,
		"acl":   map[string]any{"role": role, "canWrite": roleCanWrite(role), "canManage": roleCanManage(role)},
		"me":    c.peer(),
		"peers": liveHub.peersOf(docID),
	})
	// 告诉房间里其他人"我来了"
	liveHub.broadcast(docID, c, map[string]any{"t": "presence", "doc": docID, "peers": liveHub.peersOf(docID)})
}

func handleEdit(c *Client, m map[string]any, now int64) {
	if c.docID == "" {
		c.send(map[string]any{"t": "denied", "reason": "先进房间（发 hello）"})
		return
	}
	if !roleCanWrite(c.role) {
		c.send(map[string]any{"t": "denied", "reason": "你对这篇笔记只有阅读权限"})
		return
	}
	lineID := strings.TrimSpace(s(m["line"]))
	text := s(m["text"])
	baseRev := int64(num(m["baseRev"]))

	newRev, cf, err := liveStore.applyEdit(c.docID, lineID, text, c.name, baseRev, now)
	if err != nil {
		c.send(map[string]any{"t": "error", "reason": err.Error()})
		return
	}
	if cf != nil {
		// ★★ 冲突：没写进去，把"对方写了什么"带回去。
		//    客户端据此弹合并界面（保留我的 / 用他的 / 都保留）。
		//
		//    ⚠️ 这条只发给**发送者**，不广播 —— 其他人看到的本来就是 now 写的那版。
		c.send(map[string]any{
			"t": "conflict", "doc": c.docID, "line": cf.LineID,
			"mine": cf.Mine, "theirs": cf.Theirs,
			"by":     map[string]any{"id": cf.By, "name": cf.By},
			"baseRev": baseRev, "theirsRev": cf.ByRev,
		})
		return
	}
	liveHub.broadcast(c.docID, c, map[string]any{
		"t": "apply", "doc": c.docID, "line": lineID,
		"text": text, "rev": newRev, "by": c.peer(), "at": now,
	})
}

func handleInsert(c *Client, m map[string]any, now int64) {
	if c.docID == "" || !roleCanWrite(c.role) {
		c.send(map[string]any{"t": "denied", "reason": "没有编辑权限"})
		return
	}
	l, err := liveStore.insertLine(c.docID, strings.TrimSpace(s(m["after"])), s(m["text"]), c.name, now)
	if err != nil {
		c.send(map[string]any{"t": "error", "reason": err.Error()})
		return
	}
	liveHub.broadcast(c.docID, c, map[string]any{
		"t": "insert", "doc": c.docID, "after": s(m["after"]), "line": l, "by": c.peer(),
	})
}

func handleDelete(c *Client, m map[string]any, now int64) {
	if c.docID == "" || !roleCanWrite(c.role) {
		c.send(map[string]any{"t": "denied", "reason": "没有编辑权限"})
		return
	}
	lineID := strings.TrimSpace(s(m["line"]))
	ok, cf, err := liveStore.deleteLine(c.docID, lineID, c.name, int64(num(m["baseRev"])), now)
	if err != nil {
		c.send(map[string]any{"t": "error", "reason": err.Error()})
		return
	}
	if !ok && cf != nil {
		c.send(map[string]any{
			"t": "conflict", "doc": c.docID, "line": lineID,
			"mine": "", "theirs": cf.Theirs,
			"by":      map[string]any{"id": cf.By, "name": cf.By},
			"deleted": true,
		})
		return
	}
	liveHub.broadcast(c.docID, c, map[string]any{
		"t": "delete", "doc": c.docID, "line": lineID, "by": c.peer(),
	})
}

// handleACLMessage 是**通过 WebSocket** 改权限（改完立刻把人踢了/放进来）。
func handleACLMessage(c *Client, m map[string]any, now int64) {
	if c.docID == "" || !roleCanManage(c.role) {
		c.send(map[string]any{"t": "denied", "reason": "只有创建者能改权限"})
		return
	}
	target := strings.TrimSpace(s(m["user"]))
	role := strings.TrimSpace(s(m["role"]))
	uid := strings.TrimSpace(target)
	if uid == "" {
		c.send(map[string]any{"t": "error", "reason": "找不到用户「" + target + "」"})
		return
	}
	if role == "" {
		if err := liveStore.removeMember(c.docID, uid); err != nil {
			c.send(map[string]any{"t": "error", "reason": err.Error()})
			return
		}
		liveHub.kickUser(c.docID, uid, "你被移出了这篇笔记的共享名单")
	} else {
		if err := liveStore.setRole(c.docID, uid, role); err != nil {
			c.send(map[string]any{"t": "error", "reason": err.Error()})
			return
		}
		// 降成 reader 要立刻把人的编辑能力收掉（见 hub.kickUser 的注释）
		if !roleCanWrite(role) {
			liveHub.kickUser(c.docID, uid, "你的权限已改为仅可阅读")
		}
	}
	members, _ := liveStore.listMembers(c.docID)
	liveHub.broadcast(c.docID, nil, map[string]any{"t": "members", "doc": c.docID, "members": members})
}

// =========================================================================== REST

func handleListDocs(w http.ResponseWriter, r *http.Request, uid string) {
	_, usrName := liveUser(r)
	docs, err := liveStore.listDocs(uid)
	if err != nil {
		apiErr(w, 500, "读取失败")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "docs": docs, "me": uid, "name": usrName})
}

func handleShare(w http.ResponseWriter, r *http.Request, uid string) {
	var body struct {
		NoteID   string `json:"noteId"`
		Title    string `json:"title"`
		Markdown string `json:"markdown"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	body.NoteID = strings.TrimSpace(body.NoteID)
	if body.NoteID == "" {
		apiErr(w, 400, "缺少 noteId")
		return
	}
	now := time.Now().UnixMilli()

	// ⚠️ 已经存在的文档：**只有 owner 才能再次"共享"**（等于更新标题/补内容）。
	//    否则任何人都能靠调这个接口把别人的文档内容冲掉。
	if role, err := liveStore.roleOf(body.NoteID, uid); err == nil && role != "" {
		if role != liveRoleOwner {
			apiErr(w, 403, "这篇笔记已经共享出去了，且创建者不是你")
			return
		}
	}

	n, err := liveStore.createFrom(body.NoteID, uid, body.Title, body.Markdown, now)
	if err != nil {
		apiErr(w, 500, "共享失败："+err.Error())
		return
	}
	members, _ := liveStore.listMembers(body.NoteID)
	writeJSON(w, 200, map[string]any{
		"ok": true, "doc": body.NoteID, "lines": n, "members": members,
		"hint": "已共享。用 POST /api/live/acl 加人（按用户名或 id）。",
	})
}

func handleACL(w http.ResponseWriter, r *http.Request, uid string) {
	var body struct {
		DocID string `json:"docId"`
		User  string `json:"user"`
		Role  string `json:"role"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	body.DocID = strings.TrimSpace(body.DocID)
	role, err := liveStore.roleOf(body.DocID, uid)
	if err != nil || !roleCanManage(role) {
		apiErr(w, 403, "只有创建者能改权限")
		return
	}
	target := strings.TrimSpace(body.User)
	if target == "" {
		apiErr(w, 404, "找不到用户「"+body.User+"」")
		return
	}
	if strings.TrimSpace(body.Role) == "" {
		if err := liveStore.removeMember(body.DocID, target); err != nil {
			apiErr(w, 400, err.Error())
			return
		}
		liveHub.kickUser(body.DocID, target, "你被移出了这篇笔记的共享名单")
	} else {
		if err := liveStore.setRole(body.DocID, target, strings.TrimSpace(body.Role)); err != nil {
			apiErr(w, 400, err.Error())
			return
		}
		if !roleCanWrite(body.Role) {
			liveHub.kickUser(body.DocID, target, "你的权限已改为仅可阅读")
		}
	}
	members, _ := liveStore.listMembers(body.DocID)
	liveHub.broadcast(body.DocID, nil, map[string]any{"t": "members", "doc": body.DocID, "members": members})
	writeJSON(w, 200, map[string]any{"ok": true, "members": members})
}

func handleRename(w http.ResponseWriter, r *http.Request, uid string) {
	var body struct {
		DocID string `json:"docId"`
		Title string `json:"title"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	role, _ := liveStore.roleOf(body.DocID, uid)
	if !roleCanManage(role) {
		apiErr(w, 403, "只有创建者能改名")
		return
	}
	if err := liveStore.renameDoc(body.DocID, body.Title, time.Now().UnixMilli()); err != nil {
		apiErr(w, 500, "改名失败")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "title": body.Title})
}

// handleDrop 删掉整个协作文档（**不可逆**）。
//
// ⚠️ 护栏：和删工程一样，**要求调用方把文档标题打出来**，而不是传一个 true。
//    布尔值顺手就带上了，起不到"停下来看一眼"的作用（红线 167）。
func handleDrop(w http.ResponseWriter, r *http.Request, uid string) {
	var body struct {
		DocID   string `json:"docId"`
		Confirm string `json:"confirm"`
	}
	if err := readJSON(r, &body); err != nil {
		apiErr(w, 400, "请求体不是合法 JSON")
		return
	}
	role, _ := liveStore.roleOf(body.DocID, uid)
	if !roleCanManage(role) {
		apiErr(w, 403, "只有创建者能删除")
		return
	}
	title, _ := liveStore.docTitle(body.DocID)
	if strings.TrimSpace(body.Confirm) != strings.TrimSpace(title) {
		apiErr(w, 400, fmt.Sprintf(
			"删除不可逆，要确认一次：把 confirm 设成这篇笔记的标题「%s」再调一次。"+
				"（这会删掉协作副本；本机那篇笔记不受影响）", title))
		return
	}
	if err := liveStore.dropDoc(body.DocID); err != nil {
		apiErr(w, 500, "删除失败")
		return
	}
	liveHub.broadcast(body.DocID, nil, map[string]any{"t": "denied", "reason": "这篇笔记已取消共享"})
	liveHub.kickAll(body.DocID, "这篇笔记已取消共享")
	writeJSON(w, 200, map[string]any{"ok": true, "dropped": body.DocID})
}

func handleStats(w http.ResponseWriter, r *http.Request, uid string) {
	writeJSON(w, 200, map[string]any{
		"ok": true, "version": version, "rooms": liveHub.describe(), "conns": liveHub.countClients(),
	})
}

// =========================================================================== 小工具

// randHex 生成 n 个字节的随机十六进制串（用于行 id）。
//
// ⚠️ 用 crypto/rand 而不是 math/rand：行 id 会出现在 URL/日志里，
//    可预测的话别人能猜出"下一篇文档的行 id 长什么样"。
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 极罕见。退回时间戳仍然是唯一的（单进程内）。
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func s(v any) string {
	if v == nil {
		return ""
	}
	if x, ok := v.(string); ok {
		return x
	}
	return fmt.Sprint(v)
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	default:
		return 0
	}
}

// =========================================================================== 票据校验

// TicketAuth 是"验票据"的唯一入口。
//
// ★ 2026-10-15 整合之后它**只做一次转发**：真正验签的是 `live_ticket.go` 里的
// `verifyLiveTicket`。整合前这里有一份"逐字抄过去的拷贝"（`cmd/live/ticket.go`），
// 两份必须手工保持一致 —— 那种结构迟早会因为只改一处而坏掉，
// 而症状是"票换出来了但连不上"，看起来像密钥配错了。
//
// 现在整个仓库里**只有一处**签名格式的定义。这是整合带来的最大收益之一。
type TicketAuth struct{ secret string }

func newTicketAuth(secret string) *TicketAuth { return &TicketAuth{secret: secret} }

// byTicket 验一张票据，返回 (用户id, 用户名)。无效/过期返回 ("","")。
//
// 用户名直接从票据里来 —— 协作端要显示"谁改的"，不必再查一次用户表。
func (a *TicketAuth) byTicket(ticket string) (string, string) {
	p, ok := verifyLiveTicket(ticket, a.secret, time.Now().UnixMilli())
	if !ok {
		return "", ""
	}
	name := p.N
	if name == "" {
		name = p.U
	}
	return p.U, name
}

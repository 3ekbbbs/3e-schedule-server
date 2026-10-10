// 实时笔记协作服务端（2026-10-10）。
//
// 独立进程、独立端口（默认 :8445），与主服务端（:8443）同机部署但不是同一个程序。
//
//	主服务端     负责：身份、日程、团队、部署 …… 以及**签发票据**
//	本服务       负责：长连接、房间、行级同步、冲突判定、权限
//
// ## 启动
//
//	3e-live.exe -addr :8445 -data ../data -livedb ./live.db -certs ../certs
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
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const version = "0.1.0"

// 全局（单进程服务，用包级变量比到处传参清楚）
var (
	auth  *TicketAuth
	storeD *Store
	hubD   *Hub
)

func main() {
	addr := flag.String("addr", ":8445", "监听地址（默认 :8445）")
	httpAddr := flag.String("http", "", "可选的明文 HTTP 监听地址，如 :18081（本机调试用）")
	liveSecret := flag.String("secret", "", "和主服务端 -live-secret 一致的共享密钥（验票据用，必填）")
	liveDB := flag.String("livedb", "./live.db", "协作自己的数据库文件")
	certDir := flag.String("certs", "../certs", "证书目录（server.crt / server.key）")
	flag.Parse()

	if strings.TrimSpace(*liveSecret) == "" {
		log.Fatalf("必须用 -secret 指定共享密钥（要和主服务端的 -live-secret 一致）—— 没有它没法验票据")
	}
	auth = newTicketAuth(*liveSecret)

	var err error
	storeD, err = openStore(*liveDB)
	if err != nil {
		log.Fatalf("打开协作库失败（%s）：%v", *liveDB, err)
	}
	defer func() { _ = storeD.Close() }()

	hubD = newHub()

	mux := http.NewServeMux()

	// ---- 健康检查（部署脚本靠它探活）----
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"ok": true, "version": version, "time": time.Now().Unix(),
			"rooms": len(hubD.describe()), "conns": hubD.countClients(),
		})
	})

	// ---- WebSocket 主入口 ----
	mux.HandleFunc("GET /live", handleWS)

	// ---- REST（都用票据鉴权，因为插件只有票据）----
	mux.HandleFunc("GET /api/live/docs", withTicket(handleListDocs))
	mux.HandleFunc("POST /api/live/share", withTicket(handleShare))
	mux.HandleFunc("POST /api/live/acl", withTicket(handleACL))
	mux.HandleFunc("POST /api/live/rename", withTicket(handleRename))
	mux.HandleFunc("POST /api/live/drop", withTicket(handleDrop))
	mux.HandleFunc("GET /api/live/stats", withTicket(handleStats))

	handler := withCORS(withLogging(mux))

	go func() {
		log.Printf("实时协作服务 v%s 启动：%s（自己的库 %s；**不依赖任何数据库做鉴权**）",
			version, *addr, *liveDB)
		srv := &http.Server{
			Addr:    *addr,
			Handler: handler,
			// ⚠️ 不设 WriteTimeout：WebSocket 是长连接，
			//    WriteTimeout 会在到点后把**正在用的**连接掐掉。
			ReadHeaderTimeout: 10 * time.Second,
		}
		crt := filepath.Join(*certDir, "server.crt")
		key := filepath.Join(*certDir, "server.key")
		if fileExists(crt) && fileExists(key) {
			log.Fatal(srv.ListenAndServeTLS(crt, key))
		}
		log.Printf("⚠️ 没有找到证书（%s），HTTPS 未启动", crt)
	}()

	if *httpAddr != "" {
		log.Printf("明文调试端口 %s", *httpAddr)
		log.Fatal(http.ListenAndServe(*httpAddr, handler))
	}

	select {}
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
	return auth.byTicket(t)
}

// withTicket 是一个"必须有有效票据"的中间件。
func withTicket(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		conn: conn, br: br, uid: uid, name: usrName, hub: hubD,
	}
	defer func() {
		hubD.leave(c)
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
	role, err := storeD.roleOf(docID, c.uid)
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
		hubD.leave(c)
	}
	c.docID = docID
	c.role = role
	hubD.join(c)

	lines, err := storeD.loadLines(docID)
	if err != nil {
		c.send(map[string]any{"t": "error", "reason": "读取内容失败"})
		return
	}
	title, _ := storeD.docTitle(docID)

	var rev int64
	_ = storeD.db.QueryRow(`SELECT rev FROM live_doc WHERE doc_id=?`, docID).Scan(&rev)

	c.send(map[string]any{
		"t": "welcome", "doc": docID, "title": title, "rev": rev,
		"lines": lines,
		"acl":   map[string]any{"role": role, "canWrite": roleCanWrite(role), "canManage": roleCanManage(role)},
		"me":    c.peer(),
		"peers": hubD.peersOf(docID),
	})
	// 告诉房间里其他人"我来了"
	hubD.broadcast(docID, c, map[string]any{"t": "presence", "doc": docID, "peers": hubD.peersOf(docID)})
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

	newRev, cf, err := storeD.applyEdit(c.docID, lineID, text, c.name, baseRev, now)
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
	hubD.broadcast(c.docID, c, map[string]any{
		"t": "apply", "doc": c.docID, "line": lineID,
		"text": text, "rev": newRev, "by": c.peer(), "at": now,
	})
}

func handleInsert(c *Client, m map[string]any, now int64) {
	if c.docID == "" || !roleCanWrite(c.role) {
		c.send(map[string]any{"t": "denied", "reason": "没有编辑权限"})
		return
	}
	l, err := storeD.insertLine(c.docID, strings.TrimSpace(s(m["after"])), s(m["text"]), c.name, now)
	if err != nil {
		c.send(map[string]any{"t": "error", "reason": err.Error()})
		return
	}
	hubD.broadcast(c.docID, c, map[string]any{
		"t": "insert", "doc": c.docID, "after": s(m["after"]), "line": l, "by": c.peer(),
	})
}

func handleDelete(c *Client, m map[string]any, now int64) {
	if c.docID == "" || !roleCanWrite(c.role) {
		c.send(map[string]any{"t": "denied", "reason": "没有编辑权限"})
		return
	}
	lineID := strings.TrimSpace(s(m["line"]))
	ok, cf, err := storeD.deleteLine(c.docID, lineID, c.name, int64(num(m["baseRev"])), now)
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
	hubD.broadcast(c.docID, c, map[string]any{
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
		if err := storeD.removeMember(c.docID, uid); err != nil {
			c.send(map[string]any{"t": "error", "reason": err.Error()})
			return
		}
		hubD.kickUser(c.docID, uid, "你被移出了这篇笔记的共享名单")
	} else {
		if err := storeD.setRole(c.docID, uid, role); err != nil {
			c.send(map[string]any{"t": "error", "reason": err.Error()})
			return
		}
		// 降成 reader 要立刻把人的编辑能力收掉（见 hub.kickUser 的注释）
		if !roleCanWrite(role) {
			hubD.kickUser(c.docID, uid, "你的权限已改为仅可阅读")
		}
	}
	members, _ := storeD.listMembers(c.docID)
	hubD.broadcast(c.docID, nil, map[string]any{"t": "members", "doc": c.docID, "members": members})
}

// =========================================================================== REST

func handleListDocs(w http.ResponseWriter, r *http.Request, uid string) {
	_, usrName := liveUser(r)
	docs, err := storeD.listDocs(uid)
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
	if role, err := storeD.roleOf(body.NoteID, uid); err == nil && role != "" {
		if role != RoleOwner {
			apiErr(w, 403, "这篇笔记已经共享出去了，且创建者不是你")
			return
		}
	}

	n, err := storeD.createFrom(body.NoteID, uid, body.Title, body.Markdown, now)
	if err != nil {
		apiErr(w, 500, "共享失败："+err.Error())
		return
	}
	members, _ := storeD.listMembers(body.NoteID)
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
	role, err := storeD.roleOf(body.DocID, uid)
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
		if err := storeD.removeMember(body.DocID, target); err != nil {
			apiErr(w, 400, err.Error())
			return
		}
		hubD.kickUser(body.DocID, target, "你被移出了这篇笔记的共享名单")
	} else {
		if err := storeD.setRole(body.DocID, target, strings.TrimSpace(body.Role)); err != nil {
			apiErr(w, 400, err.Error())
			return
		}
		if !roleCanWrite(body.Role) {
			hubD.kickUser(body.DocID, target, "你的权限已改为仅可阅读")
		}
	}
	members, _ := storeD.listMembers(body.DocID)
	hubD.broadcast(body.DocID, nil, map[string]any{"t": "members", "doc": body.DocID, "members": members})
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
	role, _ := storeD.roleOf(body.DocID, uid)
	if !roleCanManage(role) {
		apiErr(w, 403, "只有创建者能改名")
		return
	}
	if err := storeD.renameDoc(body.DocID, body.Title, time.Now().UnixMilli()); err != nil {
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
	role, _ := storeD.roleOf(body.DocID, uid)
	if !roleCanManage(role) {
		apiErr(w, 403, "只有创建者能删除")
		return
	}
	title, _ := storeD.docTitle(body.DocID)
	if strings.TrimSpace(body.Confirm) != strings.TrimSpace(title) {
		apiErr(w, 400, fmt.Sprintf(
			"删除不可逆，要确认一次：把 confirm 设成这篇笔记的标题「%s」再调一次。"+
				"（这会删掉协作副本；本机那篇笔记不受影响）", title))
		return
	}
	if err := storeD.dropDoc(body.DocID); err != nil {
		apiErr(w, 500, "删除失败")
		return
	}
	hubD.broadcast(body.DocID, nil, map[string]any{"t": "denied", "reason": "这篇笔记已取消共享"})
	hubD.kickAll(body.DocID, "这篇笔记已取消共享")
	writeJSON(w, 200, map[string]any{"ok": true, "dropped": body.DocID})
}

func handleStats(w http.ResponseWriter, r *http.Request, uid string) {
	writeJSON(w, 200, map[string]any{
		"ok": true, "version": version, "rooms": hubD.describe(), "conns": hubD.countClients(),
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

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	return dec.Decode(v)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type,X-Live-Ticket")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WebSocket 连接是长连接，记"开始"就够了（结束时会由 ws 那边打日志）
		if r.URL.Path == "/live" {
			log.Printf("%s %s（ws）", r.Method, r.URL.Path)
		} else {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

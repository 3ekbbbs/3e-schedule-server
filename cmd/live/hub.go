// 房间与广播（2026-10-10）。
//
// ## 并发模型：一把大锁
//
// 协作服务是**单进程、单机**，房间数 = 共享出去的笔记数（个人应用，个位数到几十），
// 每个房间同时在线的人也就几个。这种量级下：
//
//     一把 `sync.Mutex` 保护"房间表 + 成员表"  →  比分片锁/无锁结构**更不容易写错**
//
// 广播时**先在锁里把接收者列表拷出来、再在锁外写帧** ——
// 写网络是慢操作（对方网络卡住会阻塞），拿着锁写会让**所有人**都卡住。
// 这是这把锁唯一需要注意的地方。
//
// ## 房间 = 文档 id
//
// 进来就按 docId 分房间。一个连接只属于一个房间（换文档要重连）——
// 刻意不支持"一个连接订阅多个文档"：那需要每帧带 docId 路由，
// 而用户的实际操作是"打开一篇看着"，不是"同时盯十篇"。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// Peer 是广播里带的"这个人是谁"。
type Peer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Client 是一条已连上的 WebSocket 连接。
type Client struct {
	conn  net.Conn
	br    *bufio.Reader
	uid   string
	name  string
	docID string
	role  string
	hub   *Hub

	// wmu 保护**写帧**。多个 goroutine 会往同一条连接写
	//（自己的响应 + 房间广播），WebSocket 帧不能交叉，
	// 不串行化的结果是对方收到**半截 JSON**，而且报的是"帧格式错误"（很难查）。
	wmu sync.Mutex
}

func (c *Client) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.sendRaw(string(b))
}

func (c *Client) sendRaw(s string) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// ⚠️ 写超时必须有：对方 TCP 窗口满了（网断了但没发 RST）时，
	//    写会一直阻塞，把这个 goroutine 永远挂住。
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = wsWriteText(c.conn, s)
}

func (c *Client) peer() Peer { return Peer{ID: c.uid, Name: c.name} }

type Room struct {
	docID   string
	clients map[*Client]bool
}

type Hub struct {
	mu    sync.Mutex
	rooms map[string]*Room
}

func newHub() *Hub { return &Hub{rooms: map[string]*Room{}} }

// join 把连接放进房间。
func (h *Hub) join(c *Client) {
	h.mu.Lock()
	r := h.rooms[c.docID]
	if r == nil {
		r = &Room{docID: c.docID, clients: map[*Client]bool{}}
		h.rooms[c.docID] = r
	}
	r.clients[c] = true
	h.mu.Unlock()
	h.broadcastPresence(c.docID)
}

// leave 把连接移出房间（幂等）。
func (h *Hub) leave(c *Client) {
	if c.docID == "" {
		return
	}
	h.mu.Lock()
	r := h.rooms[c.docID]
	if r != nil {
		delete(r.clients, c)
		if len(r.clients) == 0 {
			// 空房间就删掉 —— 不删的话"开过的文档"会一直占着内存
			delete(h.rooms, c.docID)
		}
	}
	h.mu.Unlock()
	h.broadcastPresence(c.docID)
}

// peersOf 取房间内的成员（去重：同一个人可能开了两个设备）。
func (h *Hub) peersOf(docID string) []Peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[docID]
	if r == nil {
		return []Peer{}
	}
	seen := map[string]Peer{}
	for c := range r.clients {
		seen[c.uid] = c.peer()
	}
	out := make([]Peer, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	// 排序是为了让"在线的人"这个列表**稳定** ——
	// map 遍历顺序随机，不排的话界面上的人名会每收到一次就在跳。
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// broadcast 给房间里的所有人发消息（except 可以为 nil = 发给所有人）。
//
// ⚠️ 先在锁里拷出接收者、**在锁外写帧**（见文件头注释）。
func (h *Hub) broadcast(docID string, except *Client, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	r := h.rooms[docID]
	var targets []*Client
	if r != nil {
		targets = make([]*Client, 0, len(r.clients))
		for c := range r.clients {
			if c == except {
				continue
			}
			targets = append(targets, c)
		}
	}
	h.mu.Unlock()

	for _, c := range targets {
		c.sendRaw(string(b))
	}
}

// broadcastPresence 告诉房间里的人"现在有谁在"。
func (h *Hub) broadcastPresence(docID string) {
	peers := h.peersOf(docID)
	h.broadcast(docID, nil, map[string]any{
		"t":     "presence",
		"doc":   docID,
		"peers": peers,
	})
}

// kickUser 把某个用户的**所有**连接从某个房间踢掉（改权限 → 立刻生效）。
//
// ⚠️ 必须踢，不能只"下次判权限时拒绝"：
//    否则被降权的人屏幕上还开着编辑器，他会继续打字，然后每一条都被拒 ——
//    体验是"我说什么它都不理"，比当场断开糟得多。
func (h *Hub) kickUser(docID, uid, reason string) {
	h.mu.Lock()
	r := h.rooms[docID]
	var victims []*Client
	if r != nil {
		for c := range r.clients {
			if c.uid == uid {
				victims = append(victims, c)
			}
		}
	}
	h.mu.Unlock()

	for _, c := range victims {
		c.send(map[string]any{"t": "denied", "reason": reason})
		// ⚠️ 用 wsClose（先发 close 帧再关）而不是裸 Close()：
		//    裸关的话对端看到的是"连接被重置"，会当成网络错误；
		//    发了 close 帧才是**一次正常的关闭**，客户端能干净地走重连逻辑。
		wsClose(c.conn)
	}
}

// kickAll 把房间里的**所有人**踢掉（文档被删掉时用）。
func (h *Hub) kickAll(docID, reason string) {
	h.mu.Lock()
	r := h.rooms[docID]
	var victims []*Client
	if r != nil {
		for c := range r.clients {
			victims = append(victims, c)
		}
	}
	h.mu.Unlock()

	for _, c := range victims {
		c.send(map[string]any{"t": "denied", "reason": reason})
		wsClose(c.conn)
	}
}

// countClients 房间里的连接数（诊断用）。
func (h *Hub) countClients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.rooms {
		n += len(r.clients)
	}
	return n
}

// describe 房间概览（/api/live/stats 用）。
func (h *Hub) describe() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []map[string]any{}
	for id, r := range h.rooms {
		peers := map[string]bool{}
		for c := range r.clients {
			peers[c.uid] = true
		}
		out = append(out, map[string]any{
			"doc": id, "conns": len(r.clients), "people": len(peers),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["doc"]) < fmt.Sprint(out[j]["doc"])
	})
	return out
}

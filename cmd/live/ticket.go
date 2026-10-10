// 票据校验（2026-10-10）。
//
// ## ★ 验签，不查库
//
// 第一版设计是"票据写进主库，本服务只读主库校验"。那个方案假设
// **协作服务和主服务在同一台机器上** —— 但实际部署是**两台服务器**
// （用户有两台：主服务一台、笔记/协作一台），主库不在这边，方案不成立。
//
// 所以票据改成了 **HMAC 自包含签名**：
//
//	lt_<payload 的 base64url>.<HMAC-SHA256(payload, 共享密钥) 的 base64url>
//	payload = {"u":"用户id","n":"用户名","e":过期毫秒,"i":签发毫秒}
//
// 本服务只做三件事：验签 → 解 payload → 判过期。**不碰任何数据库。**
//
// 好处：跨机器、无状态、离线可用（主服务挂了也不影响已经在用的连接）。
// ⚠️ 代价：有效期内**无法单独吊销**某张票。所以票只有 5 分钟 ——
//    这是这套方案成立的前提。后来人不要为了"少换票"把它拉长。
//
// ## ⚠️ 这个文件和主服务端的 `live_ticket.go` 是**一对拷贝**
//
// 签名格式必须完全一致。改了那边就要改这边，否则现象是
// "票换出来了，但本服务说'票据无效'" —— 而且看起来像密钥配错了。
// 格式刻意写得极简（两段 base64 + 一个点），就是为了抄写不出错。
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// TicketAuth 是"验票据"的唯一入口。
//
// ⚠️ 它**不持有任何数据库连接** —— 这是跨机器部署能成立的关键。
//    早先的版本要连主库查 `live_tickets` 表，那要求协作服务和主服务同机，
//    而实际是两台机器。
type TicketAuth struct{ secret string }

func newTicketAuth(secret string) *TicketAuth { return &TicketAuth{secret: secret} }

// byTicket 验一张票据，返回 (用户id, 用户名)。无效/过期返回 ("","")。
//
// 用户名直接从票据里来 —— 协作服务端要显示"谁改的"，
// 而它没有用户表可查（那又是一次跨机器依赖）。
func (a *TicketAuth) byTicket(ticket string) (string, string) {
	p, ok := verifyTicket(ticket, a.secret, time.Now().UnixMilli())
	if !ok {
		return "", ""
	}
	name := p.N
	if name == "" {
		name = p.U
	}
	return p.U, name
}

// ticketPayload 和主服务端 `liveTicketPayload` 字段一一对应。
type ticketPayload struct {
	U string `json:"u"`
	N string `json:"n"`
	E int64  `json:"e"`
	I int64  `json:"i"`
}

// verifyTicket 验签 + 判过期。失败返回 ok=false。
func verifyTicket(ticket, secret string, nowMs int64) (ticketPayload, bool) {
	var p ticketPayload
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

	// 1) 先验签 —— 顺序不能反（未验证的输入不进 JSON 解析）
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

	// 3) 判过期（用 payload 里的时间）
	if p.E <= nowMs || p.U == "" {
		return p, false
	}
	return p, true
}

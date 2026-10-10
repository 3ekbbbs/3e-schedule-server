// WebSocket（RFC6455）的最小实现（2026-10-10）。
//
// ## 为什么要自己写
//
// 项目此前**没有任何 WebSocket**（对 server/web/site 全目录搜过，0 命中），
// 而协作服务端的核心就是长连接。引第三方库（gorilla/websocket）要多一个依赖，
// 而握手 + 帧解析本身只有 200 行 —— 这点代码量不值得换一个长期依赖。
//
// ## 实现范围（刻意只做够用的）
//
// ✅ 握手（Sec-WebSocket-Accept 校验）
// ✅ 文本帧 / 二进制帧 / ping / pong / close
// ✅ 客户端发来的**掩码**（协议强制要求客户端掩码，不解掩码会读到乱码）
// ✅ 分片帧（把 continuation 拼起来）
// ❌ 扩展协商（permessage-deflate 等）—— 不响应，客户端会退回不压缩
// ❌ 服务端主动分片 —— 我们的消息都很小（一行文本）
//
// ⚠️ 一个容易踩的点：**服务端发出的帧不能掩码**。
//    长度字段的 MASK 位必须是 0，否则浏览器/客户端会当作协议错误直接断开。
package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// 协议规定的魔数（RFC6455 §1.3）—— 握手时和客户端的 key 拼起来做 SHA-1。
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// 帧操作码。
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// 单帧最大长度。一行的文本远小于它；设这个上限是为了防止恶意客户端
// 用一个 "payload len = 2^63" 的头把服务端的 buffer 直接撑爆。
const wsMaxFrame = 1 << 20 // 1 MiB

var errWSClosed = errors.New("websocket closed")

// wsUpgrade 把一条 HTTP 连接升级成 WebSocket。
//
// 返回的 conn 已经是裸 TCP 连接（Hijack 之后 HTTP 服务器不再管它），
// 调用方负责 Close；br 是带缓冲的 reader，**必须继续用它读** ——
// 握手请求可能有已经读进缓冲的字节，直接读 conn 会丢数据。
func wsUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.Reader, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, nil, errors.New("不是 websocket 请求")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if key == "" {
		return nil, nil, errors.New("缺少 Sec-WebSocket-Key")
	}
	// 只支持 13，这是所有现代客户端的版本
	if v := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")); v != "" && v != "13" {
		return nil, nil, fmt.Errorf("不支持的 websocket 版本 %s", v)
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("这个 HTTP 服务器不支持连接接管")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}

	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])

	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n")
	b.WriteString("\r\n")

	if _, err := brw.WriteString(b.String()); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := brw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, brw.Reader, nil
}

// wsWriteFrame 发一帧。**不掩码**（服务端侧正确做法）。
func wsWriteFrame(w io.Writer, op byte, payload []byte) error {
	n := len(payload)
	hdr := make([]byte, 0, 10)
	hdr = append(hdr, 0x80|op) // FIN=1
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, 0, 0)
		binary.BigEndian.PutUint16(hdr[len(hdr)-2:], uint16(n))
	default:
		hdr = append(hdr, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(hdr[len(hdr)-8:], uint64(n))
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if n > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func wsWriteText(w io.Writer, s string) error { return wsWriteFrame(w, opText, []byte(s)) }

// wsReadFrame 读一帧（自动解掩码、自动处理分片、自动回 pong）。
//
// 返回值：op 是**逻辑**操作码（分片已合并成 opText/opBinary），payload 是完整载荷。
// 收到 close / 读到 EOF 时返回 errWSClosed。
func wsReadFrame(br *bufio.Reader, out io.Writer) (byte, []byte, error) {
	var (
		fragOp  byte
		fragBuf []byte
	)
	for {
		var h [2]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			return 0, nil, errWSClosed
		}
		fin := h[0]&0x80 != 0
		op := h[0] & 0x0F
		masked := h[1]&0x80 != 0
		n := int(h[1] & 0x7F)

		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(br, ext[:]); err != nil {
				return 0, nil, errWSClosed
			}
			n = int(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(br, ext[:]); err != nil {
				return 0, nil, errWSClosed
			}
			u := binary.BigEndian.Uint64(ext[:])
			if u > wsMaxFrame {
				return 0, nil, errors.New("帧太大")
			}
			n = int(u)
		}
		if n < 0 || n > wsMaxFrame {
			return 0, nil, errors.New("帧长度非法")
		}

		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(br, mask[:]); err != nil {
				return 0, nil, errWSClosed
			}
		}
		payload := make([]byte, n)
		if n > 0 {
			if _, err := io.ReadFull(br, payload); err != nil {
				return 0, nil, errWSClosed
			}
		}
		if masked {
			// 协议要求客户端掩码；不掩码的按错误处理会更安全，
			// 但有些自制客户端会忘，所以这里**宽容地解一下**（掩码全 0 时等价于没掩）
			for i := 0; i < n; i++ {
				payload[i] ^= mask[i&3]
			}
		}

		// 控制帧必须 FIN=1 且不超过 125 字节（协议规定），这里不严格校验长度
		switch op {
		case opPing:
			// 协议要求回 pong，且要带同样的载荷
			_ = wsWriteFrame(out, opPong, payload)
			continue
		case opPong:
			continue
		case opClose:
			return 0, nil, errWSClosed
		case opContinuation:
			if fragOp == 0 {
				return 0, nil, errors.New("收到了没有开头的续帧")
			}
			fragBuf = append(fragBuf, payload...)
			if len(fragBuf) > wsMaxFrame {
				return 0, nil, errors.New("分片总长超限")
			}
			if fin {
				return fragOp, fragBuf, nil
			}
			continue
		case opText, opBinary:
			if !fin {
				fragOp = op
				fragBuf = append(fragBuf[:0], payload...)
				continue
			}
			if op == opText && !utf8.Valid(payload) {
				// 文本帧必须是合法 UTF-8（我们的消息是 JSON，一定是）
				return 0, nil, errors.New("文本帧不是合法 UTF-8")
			}
			return op, payload, nil
		default:
			// 未知操作码：协议上应该断开。这里宽容地忽略，
			// 免得一个将来新增的扩展让整条连接断掉。
			continue
		}
	}
}

// wsClose 发一个 close 帧再关连接。
//
// ⚠️ 顺序不能反：先 Close() 的话，客户端看到的是"连接被重置"，
// 而不是一次礼貌的关闭 —— 表现为客户端报网络错误，而不是正常断开。
func wsClose(conn net.Conn) {
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = wsWriteFrame(conn, opClose, nil)
	_ = conn.Close()
}

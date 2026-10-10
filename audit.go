package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ===========================================================================
// 账号事件日志（登录 / 注册 / 登出 / 改密）2026-09-23
// ===========================================================================
//
// 双写：**SQLite 表 + JSONL 文件**。
//
//   表   —— 管理员在后台直接筛、排序、分页（日常问"谁从哪登的"用这个）
//   文件 —— 保留期更长、格式独立于库（库被清、被迁移，文件还在；
//           取证时 `tail -f` 一条命令就能看，不用起服务）
//
// 为什么不只写文件：文件日志是**只能上服务器看**的东西，而"这台设备是不是
// 我本人登的"是日常会被问到的问题 —— 每次都要 SSH 上去 grep 一遍不现实。
// 为什么不只写库：库有保留期清理，而安全事件的价值恰恰在**事后很久**才体现。
//
// ---------------------------------------------------------------------------
// 三条红线
// ---------------------------------------------------------------------------
//
// 1. **绝不记密码、令牌**。这里只记事件本身（谁、何时、从哪、结果）。
//    任何时候往这个文件里加字段，先问一遍"它泄漏了会不会出事"。
//
// 2. **XFF 只是"自称"**，不是身份。真实对端地址取 `r.RemoteAddr`。
//    两个都记，但**任何权限判定都不许读 real_ip**。
//
// 3. **失败也要记**（登录失败、用户名不存在）。只看成功记录的话，
//    "有人在暴力猜密码"这件事是完全不可见的 —— 而那正是最该被发现的。

// 事件类型。用常量而不是散落的字符串字面量：写错一个字母就会静默产生
// 一类永远不会被筛出来的记录（筛选项里没有它，也就没人发现它存在过）。
const (
	logEventRegister       = "register"
	logEventLogin          = "login"
	logEventLogout         = "logout"
	logEventLoginFailed    = "login_failed"
	logEventPasswordChange = "password_change"
	// logEventTokenRevoked 是改密时"顺带踢掉的其它设备"。
	// 单独记一条而不是只写个数字：被踢的是哪些设备、什么时候、从哪个 IP，
	// 这些在"我发现有人登了我的号"这个场景里正是最想看的。
	logEventTokenRevoked = "session_revoked"
	// 账号级处置（2026-10-09）。封禁/解封/删除都记一条：这三种都是**争议裁决**，
	// 事后必须能回答"谁在什么时候对谁做了什么"，而 `log.Printf` 只写在 stdout 上、
	// 翻起来全靠人去服务器上 grep。落在 login_logs 里，管理台的日志页就能筛出来。
	logEventAccountBanned   = "account_banned"
	logEventAccountUnbanned = "account_unbanned"
	logEventAccountDeleted  = "account_deleted"
)

// loginEventLabels 给后台筛选器用（前端不自己维护一份，避免两边对不上）。
var loginEventLabels = map[string]string{
	logEventRegister:        "注册",
	logEventLogin:           "登录",
	logEventLogout:          "登出",
	logEventLoginFailed:     "登录失败",
	logEventPasswordChange:  "改密码",
	logEventTokenRevoked:    "会话被踢",
	logEventAccountBanned:   "账号被封禁",
	logEventAccountUnbanned: "账号被解封",
	logEventAccountDeleted:  "账号被删除",
}

// ---------------------------------------------------------------------------
// 客户端地址解析
// ---------------------------------------------------------------------------

// clientIP 返回"本次 TCP 连接的对端地址"（去掉端口）。
//
// ⚠️ 这才是**可信**的那个地址 —— 它由内核给出，客户端伪造不了。
// 反代后面它会是 127.0.0.1，这是正常的、也是必须如实记录的。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		// 没有端口（不走 TCP 或格式异常）就原样用
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// realClientIP 解析 X-Forwarded-For / X-Real-IP。
//
// ⚠️ **这个值不可信**，客户端想填什么就填什么。它只用于展示与人工排查，
// 绝不参与任何判定（红线 2）。
//
// XFF 是一个逗号分隔的链，**最左边是客户端自称的最初来源，最右边是最后
// 一个代理**。这里取最左边那个（最接近真实客户端的那个自称值）。
// 取右边会让所有请求都显示成"来自反代地址"，那就完全没有信息量了。
func realClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// 可能有多个头（多个代理各加一个），全部拼起来再切
		for _, part := range strings.Split(xff, ",") {
			// 取链上最左边**合法**的一个，跳过空串与垃圾
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			// 去掉可能带的端口：IPv6 用 [::1]:1234 这种写法
			if h, _, err := net.SplitHostPort(p); err == nil {
				p = h
			}
			if net.ParseIP(strings.Trim(p, "[]")) != nil {
				return strings.Trim(p, "[]")
			}
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if h, _, err := net.SplitHostPort(xr); err == nil {
			xr = h
		}
		if net.ParseIP(strings.Trim(xr, "[]")) != nil {
			return strings.Trim(xr, "[]")
		}
	}
	return ""
}

// clipUA 截断 User-Agent。
//
// 不截的话，一个恶意客户端可以塞 1MB 的 UA 进来，把日志表和文件一起撑爆 ——
// 而且它是**未认证路径**上就能做到的事（登录接口不需要登录）。
func clipUA(ua string) string {
	ua = strings.TrimSpace(ua)
	if r := []rune(ua); len(r) > 300 {
		return string(r[:300]) + "…"
	}
	return ua
}

// ---------------------------------------------------------------------------
// 写入
// ---------------------------------------------------------------------------

// loginLogLogger 负责把 JSONL 文件写到磁盘。
//
// ⚠️ 用一个**独立的 mutex**，并且每行一次 Write（不是攒批）：
// 日志的价值在于"崩了以后还在"，攒批会让最后一批丢掉 —— 而最后那批
// 往往正是出事前后的那几条。
type loginLogLogger struct {
	mu  sync.Mutex
	dir string
}

func newLoginLogLogger(dataDir string) *loginLogLogger {
	dir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("loginlog: 建日志目录失败（只写库）: %v", err)
		return &loginLogLogger{dir: ""}
	}
	return &loginLogLogger{dir: dir}
}

// write 落一行 JSONL。**失败只记警告，不向上报错** ——
// 日志写不进去绝不能让"登录"这件事失败（那会变成一个自锁的故障）。
func (l *loginLogLogger) write(rec map[string]any) {
	if l == nil || l.dir == "" {
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	name := filepath.Join(l.dir,
		"login-"+time.Unix(rec["created_at"].(int64), 0).Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("loginlog: 打开 %s 失败: %v", name, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		log.Printf("loginlog: 写 %s 失败: %v", name, err)
	}
}

// LoginLog 是后台展示用的一行。
type LoginLog struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
	Event     string `json:"event"`
	EventText string `json:"event_text"`
	Result    string `json:"result"`
	Reason    string `json:"reason"`
	IP        string `json:"ip"`
	RealIP    string `json:"real_ip"`
	UA        string `json:"ua"`
	Device    string `json:"device"`
	CreatedAt int64  `json:"created_at"`
}

// logAccountEvent 记一条账号事件。双写（库 + 文件）。
//
// 签名说明：
//   - uid/uname：事件主体。**登录失败时 uid 为空**（连账号都没查到），
//     此时 uname 存**用户输入的那个名字** —— 这一点很重要，
//     "有人在拿哪些名字试"正是暴力破解的特征。
//   - reason：只放人能看懂、且不含敏感信息的原因。
//     ⚠️ 绝不能把"密码不对"写成"密码应该是 xxx"这种 —— 日志会被到处看。
func (s *Server) logAccountEvent(r *http.Request, uid, uname, event, result, reason string) {
	now := time.Now().Unix()
	ip := clientIP(r)
	real := realClientIP(r)
	ua := ""
	device := ""
	if r != nil {
		ua = clipUA(r.Header.Get("User-Agent"))
		device = strings.TrimSpace(r.Header.Get("X-Device"))
		if d := []rune(device); len(d) > 120 {
			device = string(d[:120])
		}
	}

	id := newID("lg_")
	if _, err := s.db.sql.Exec(`
		INSERT INTO login_logs
		  (id, user_id, user_name, event, result, reason, ip, real_ip, ua, device, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, uid, uname, event, result, reason, ip, real, ua, device, now); err != nil {
		log.Printf("loginlog: 写库失败: %v", err)
	}

	if s.loginLog != nil {
		s.loginLog.write(map[string]any{
			"id": id, "user_id": uid, "user_name": uname,
			"event": event, "result": result, "reason": reason,
			"ip": ip, "real_ip": real, "ua": ua, "device": device,
			"created_at": now,
		})
	}
}

// pruneLoginLogs 清掉超过保留期的记录。
//
// 为什么要有：这张表在**每次登录**都会长一行，是典型的只会变大的表。
// 保留期默认 180 天 —— 长到足够回答"半年前谁登过"，
// 短到不至于让表无限膨胀。
//
// ⚠️ 只清库，**不清文件**：文件是留证据的那一份，由运维按需要自己归档。
func (s *Server) pruneLoginLogs() {
	days := atoiDefault(s.setting("login_log_keep_days", ""), 180)
	if days <= 0 {
		days = 180
	}
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	res, err := s.db.sql.Exec(`DELETE FROM login_logs WHERE created_at < ?`, cutoff)
	if err != nil {
		log.Printf("loginlog: 清理失败: %v", err)
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		log.Printf("loginlog: 清理了 %d 条超过 %d 天的记录", n, days)
	}
}

// startLoginLogPruner 起一个每天跑一次的清理协程。
func (s *Server) startLoginLogPruner() {
	go func() {
		// 启动后先跑一次（可能有很久没清过的）
		s.pruneLoginLogs()
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for range t.C {
			s.pruneLoginLogs()
		}
	}()
}

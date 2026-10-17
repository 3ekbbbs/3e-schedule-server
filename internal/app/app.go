package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/3ekbbbs/schedule-server/internal/certgen"
)

// Run 启动完整的服务端，直到收到退出信号或内部关闭请求。
//
// 与旧入口的差别：
//   - 不再解析命令行（由 cmd 负责），配置全部来自 Config；
//   - 监听失败返回错误而不是 log.Fatal，方便管理面板显示原因；
//   - 增加仅回环可达的内部接口（健康检查、备份、关闭），供面板调用；
//   - 支持优雅退出：关闭监听、关库、删除内部地址文件。
func Run(cfg Config) error {
	if cfg.Addr == "" && cfg.HTTPAddr == "" {
		return errors.New("没有启用任何监听地址（HTTPS 与 HTTP 都为空）")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	db, err := openDB(filepath.Join(cfg.DataDir, "schedule.db"))
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer db.sql.Close()

	s := &Server{
		db:       db,
		cfg:      cfg,
		weather:  newWeatherCache(cfg.DataDir),
		words:    &wordEngine{},
		loginLog: newLoginLogLogger(cfg.DataDir),
	}

	// 违禁词：先灌种子再读进内存。顺序颠倒会让空表被读成“没有违禁词”。
	if err := s.ensureSeedWords(); err != nil {
		log.Printf("moderation: 内置词库灌入失败: %v", err)
	}
	if err := s.words.load(db.sql); err != nil {
		log.Printf("moderation: 词库加载失败: %v", err)
	}
	s.startLoginLogPruner()

	// 邀请码：空则复用库里的，库里也没有就生成一个。
	inviteFresh := false
	if cfg.InviteCode == "" {
		code, _ := db.getMeta("invite_code")
		if code == "" {
			code = newToken()[:10]
			_ = db.setMeta("invite_code", code)
			inviteFresh = true
		}
		s.cfg.InviteCode = code
	} else {
		_ = db.setMeta("invite_code", cfg.InviteCode)
		inviteFresh = true
	}

	var users int
	_ = db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)

	// 实时协作的密钥必须落库：否则每次重启都换一个，客户端手里的旧票作废。
	_ = s.liveSecret()
	{
		lh := strings.TrimSpace(s.setting(settingLiveHost, defaultLiveHost))
		lp := strings.TrimSpace(s.setting(settingLivePort, defaultLivePort))
		where := "与主服务同源"
		if lh != "" || lp != "" {
			where = lh + ":" + lp
		}
		log.Printf("live: 实时协作 %s", where)
	}

	mux := http.NewServeMux()
	s.routes(mux)
	s.startLiveService()
	s.registerLiveRoutes(mux)

	// 背词同步：通过环境变量传端口，沿用原有开关约定。
	if cfg.WordbookAddr == "" {
		_ = os.Setenv("WORDBOOK_PORT", "off")
	} else if _, port, err := net.SplitHostPort(cfg.WordbookAddr); err == nil {
		_ = os.Setenv("WORDBOOK_PORT", port)
	} else {
		_ = os.Setenv("WORDBOOK_PORT", strings.TrimPrefix(cfg.WordbookAddr, ":"))
	}
	s.startWordbookSyncServer()

	if cfg.Diag {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		log.Printf("  pprof       : enabled")
	}

	handler := withCORS(withLogging(mux))

	// 证书：HTTPS 与背词同步共用同一张。缺失时按配置自动生成。
	certFile := filepath.Join(cfg.CertDir, "server.crt")
	keyFile := filepath.Join(cfg.CertDir, "server.key")
	if cfg.Addr != "" && cfg.AutoCert {
		if !fileExists(certFile) || !fileExists(keyFile) {
			res, err := certgen.Ensure(certgen.Options{
				Dir:        cfg.CertDir,
				ServerName: cfg.ServerName,
			})
			if err != nil {
				return fmt.Errorf("生成自签证书失败: %w", err)
			}
			if res.Created {
				log.Printf("已生成自签证书：%s（把 ca.crt 导入客户端即可信任）", res.CAFile)
			}
		}
	}

	var (
		servers  []*http.Server
		mu       sync.Mutex
		stopOnce sync.Once
		stopCh   = make(chan struct{})
	)
	stop := func(reason string) {
		stopOnce.Do(func() {
			log.Printf("正在关闭：%s", reason)
			close(stopCh)
		})
	}

	start := func(name, addr string, h http.Handler, tlsCfg *tls.Config, cert, key string) error {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("%s 监听 %s 失败: %w", name, addr, err)
		}
		srv := &http.Server{
			Handler:           h,
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: 15 * time.Second,
		}
		mu.Lock()
		servers = append(servers, srv)
		mu.Unlock()
		if name == "internal" {
			setInternalAddr(ln.Addr().String())
		}
		go func() {
			var err error
			if cert != "" {
				err = srv.ServeTLS(ln, cert, key)
			} else {
				err = srv.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("%s 退出：%v", name, err)
			}
		}()
		log.Printf("  %-12s: %s", name, ln.Addr())
		return nil
	}

	if cfg.Addr != "" {
		if fileExists(certFile) && fileExists(keyFile) {
			tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
			if err := start("HTTPS", cfg.Addr, handler, tlsCfg, certFile, keyFile); err != nil {
				return err
			}
		} else {
			log.Printf("HTTPS 未启动：缺少 %s / %s（可在面板里生成证书）", certFile, keyFile)
		}
	}
	if cfg.HTTPAddr != "" {
		if err := start("HTTP", cfg.HTTPAddr, handler, nil, "", ""); err != nil {
			return err
		}
	}

	// ---- 内部接口：只绑定回环，带随机令牌 ----
	internalToken := newToken()
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("GET /api/internal/health", s.internalGuard(internalToken, s.handleInternalHealth))
	internalMux.HandleFunc("POST /api/internal/backup", s.internalGuard(internalToken, s.handleInternalBackup))
	internalMux.HandleFunc("POST /api/internal/shutdown", s.internalGuard(internalToken, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		go stop("面板请求关闭")
	}))
	if err := start("internal", cfg.InternalAddr, internalMux, nil, "", ""); err != nil {
		return err
	}
	// 实际监听地址由 start 写入 internalListenAddr（配置为 :0 时是随机端口）。
	internalAddr := describeInternal(cfg.InternalAddr)

	addrFile := filepath.Join(cfg.DataDir, "internal.addr")
	if err := os.WriteFile(addrFile, []byte(fmt.Sprintf("{\"addr\":%q,\"token\":%q}\n", internalAddr, internalToken)), 0o600); err != nil {
		log.Printf("内部地址写入失败（面板将无法连接）：%v", err)
	}
	defer os.Remove(addrFile)

	log.Printf("schedule-server %s", version)
	log.Printf("  data dir    : %s", cfg.DataDir)
	log.Printf("  cert dir    : %s", cfg.CertDir)
	if inviteFresh {
		log.Printf("  邀请码      : %s   ← 只在首次生成时显示全文", s.cfg.InviteCode)
	} else {
		log.Printf("  邀请码      : %s（完整值见 %s）", maskInvite(s.cfg.InviteCode),
			filepath.Join(cfg.DataDir, "invite.txt"))
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "invite.txt"),
		[]byte(s.cfg.InviteCode+"\n"), 0o600); err != nil {
		log.Printf("  （邀请码写入 invite.txt 失败：%v）", err)
	}
	if users == 0 {
		log.Printf("  首次运行    : 还没有账号，第一个注册的账号将成为管理员")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		log.Printf("收到信号 %s", sig)
	case <-stopCh:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	mu.Lock()
	snapshot := append([]*http.Server(nil), servers...)
	mu.Unlock()
	for _, srv := range snapshot {
		_ = srv.Shutdown(ctx)
	}
	return nil
}

// describeInternal 把配置里的内部监听地址换算成面板可连接的实际地址。
//
// 默认配置是 127.0.0.1:0（随机端口），但 net.Listen 拿到的端口没在这里保存。
// 因此 start 会把实际地址记录到包级变量，见 internalListenAddr。
func describeInternal(configured string) string {
	if configured == "" || strings.HasSuffix(configured, ":0") {
		if v := internalListenAddr(); v != "" {
			return v
		}
	}
	return configured
}

// internalListenAddr 由 start 更新，保存内部 listener 的真实地址。
var (
	internalMu  sync.Mutex
	internalCur string
)

func setInternalAddr(v string) {
	internalMu.Lock()
	internalCur = v
	internalMu.Unlock()
}

func internalListenAddr() string {
	internalMu.Lock()
	defer internalMu.Unlock()
	return internalCur
}

// handleInternalHealth 返回进程级状态，面板用于判断服务是否就绪。
func (s *Server) handleInternalHealth(w http.ResponseWriter, r *http.Request) {
	var users, calendars, events int
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM calendars WHERE deleted_at IS NULL`).Scan(&calendars)
	_ = s.db.sql.QueryRow(`SELECT COUNT(*) FROM events WHERE deleted_at IS NULL`).Scan(&events)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"version":   version,
		"users":     users,
		"calendars": calendars,
		"events":    events,
		"uptime":    int64(time.Since(startedAt).Seconds()),
		"pid":       os.Getpid(),
	})
}

var startedAt = time.Now()

// handleInternalBackup 用 VACUUM INTO 生成一份一致性快照。
func (s *Server) handleInternalBackup(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(s.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		apiErr(w, http.StatusInternalServerError, "备份目录创建失败: "+err.Error())
		return
	}
	name := "backup-" + time.Now().Format("20060102-150405") + ".db"
	dst := filepath.Join(dir, name)
	q := "VACUUM INTO '" + strings.ReplaceAll(dst, "'", "''") + "'"
	if _, err := s.db.sql.Exec(q); err != nil {
		apiErr(w, http.StatusInternalServerError, "备份失败: "+err.Error())
		return
	}
	st, err := os.Stat(dst)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "备份文件不可读: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"name": name,
		"path": dst,
		"size": st.Size(),
	})
}

// internalGuard 校验回环来源与随机令牌。
//
// 双重条件的理由：内部端口虽然是随机的，但同一台机器上的其它进程也能扫到；
// 令牌文件是 0600，别的用户读不到。
func (s *Server) internalGuard(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			apiErr(w, http.StatusForbidden, "仅允许本机访问")
			return
		}
		if r.Header.Get("X-Internal-Token") != token {
			apiErr(w, http.StatusForbidden, "内部令牌无效")
			return
		}
		next(w, r)
	}
}

// portOf 从 ":8446" 或 "0.0.0.0:8446" 里取端口。
func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	if _, err := strconv.Atoi(strings.TrimPrefix(addr, ":")); err == nil {
		return strings.TrimPrefix(addr, ":")
	}
	return addr
}

package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "time/tzdata" // embed the tz database so the binary is self-contained
)

// 0.8.0：**插件分发** —— 四个新接口（`/api/plugins` 列表 / `download/{id}` /
// `manifest` / `publish`）。插件包的元数据一律由服务端**开包读**
// `assets/threees-plugin.json` 得到，发布接口不接受调用方传 id / 版本 / 能力清单；
// 这样"包里写的"和"服务端记的"在物理上不可能不一致。详见 plugins.go。
//
// 0.7.8：时间合理性闸门 —— 建/改日程时拒绝**物理上不可能**的时间
// （2000~2100 年之外、时长为负、超过 366 天）。防的是客户端单位算错
// （把 epoch 秒又除 1000）写出来的 1970 年日程：它掉出实例时间窗口，
// 于是"列表里看不见、AI 查重却说已存在、还删不掉"。详见 event.go。
//
// 0.7.7：换日历（把日程从一张日历挪到另一张）——
// 字段白名单开放 calendar_id、校验目标日历写权限、实例跟随日程、
// 失去可见性时下发墓碑。详见 event.go / models.go / recur.go / sync.go。
const version = "0.9.2"

var errForbidden = errors.New("forbidden")

type Config struct {
	Addr       string
	HTTPAddr   string
	CertDir    string
	DataDir    string
	InviteCode string

	// Weather. The place is fixed by request (北京市), but the coordinates and
	// the identifying User-Agent stay configurable because met.no rejects
	// anonymous callers and the contact address should be the owner's.
	WeatherPlace string
	WeatherLat   float64
	WeatherLon   float64
	WeatherUA    string

	// LiveSecret 是实时协作票据的 HMAC 共享密钥。
	//
	// ⚠️ 它必须和**第二台服务器**上协作服务端的 `-secret` 一致。
	//    不一致的表现是"票换出来了，但协作服务端说'票据无效'" ——
	//    看着像客户端的问题，其实是两边密钥不同。
	LiveSecret string

	// Diag exposes /api/diag/* and pprof. Off unless -pprof is passed.
	Diag bool

	// COSBase 是对象存储的加速前缀（如 https://dl.example.com/updates）。
	//
	// 配了它之后，`/api/updates` 里的 `download_url` 会换成**对象存储的完整地址**，
	// 客户端更新包走对象存储 + CDN，不再吃这台服务器的带宽。
	// 留空 = 仍然从本机 `/api/updates/download/<platform>` 下（原来的行为）。
	//
	// ⚠️ 键的拼法必须和 `scripts/lib/cos.mjs` 的 `cosKeyFor()` **严格一致**
	//（`<prefix>/<platform>/<文件名>`）—— 两边错一个字符客户端就只会说"下载失败"。
	COSBase string
}

type Server struct {
	db      *DB
	cfg     Config
	weather *weatherCache
	// words 是违禁词引擎（带内存缓存）。写路径（建/改日程等）会高频查它，
	// 所以不能每次去查库 —— 详见 moderation.go 顶部那段说明。
	words *wordEngine
	// loginLog 把账号事件写 JSONL 文件（与库双写）。见 audit.go。
	loginLog *loginLogLogger
}

func main() {
	addr := flag.String("addr", ":8443", "HTTPS listen address (empty to disable)")
	httpAddr := flag.String("http", "", "plain HTTP listen address, e.g. :8080 (empty to disable)")
	dataDir := flag.String("data", "./data", "data directory")
	certDir := flag.String("certs", "./certs", "directory holding server.crt / server.key")
	invite := flag.String("invite", "", "invite code required to register (empty = reuse/generate)")
	liveSecret := flag.String("live-secret", "",
		"实时协作票据的共享密钥（必须和协作服务端的 -secret 一致；"+
			"留空则自动生成并落库，之后看 GET /api/live/config 的 secretSet）")
	liveHost := flag.String("live-host", "",
		"实时协作服务端地址（写进 app_settings，客户端换票时拿；留空用默认值）")
	livePort := flag.String("live-port", "",
		"实时协作服务端端口（同上，留空用默认值）")
	enablePprof := flag.Bool("pprof", false, "expose /debug/pprof (diagnostics only)")
	cosBase := flag.String("cos-base", "",
		"object-storage prefix for update downloads, e.g. https://dl.example.com/updates "+
			"(empty = serve updates from this machine)")
	place := flag.String("place", defaultWeatherPlace, "weather location name shown to clients")
	lat := flag.Float64("lat", defaultWeatherLat, "weather latitude")
	lon := flag.Float64("lon", defaultWeatherLon, "weather longitude")
	ua := flag.String("ua", "3e-schedule-server/"+version+" (personal family calendar; contact: you@example.com)",
		"User-Agent sent to api.met.no — they reject requests without one")
	flag.Parse()

	cfg := Config{
		Addr: *addr, HTTPAddr: *httpAddr, DataDir: *dataDir, CertDir: *certDir, InviteCode: *invite,
		LiveSecret: *liveSecret,
		WeatherPlace: *place, WeatherLat: *lat, WeatherLon: *lon, WeatherUA: *ua,
		Diag:    *enablePprof,
		COSBase: *cosBase,
	}

	db, err := openDB(filepath.Join(cfg.DataDir, "schedule.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	s := &Server{db: db, cfg: cfg, weather: newWeatherCache(cfg.DataDir), words: &wordEngine{},
		loginLog: newLoginLogLogger(cfg.DataDir)}

	// 违禁词：首次启动灌内置词，然后把词表读进内存缓存。
	// 顺序不能颠倒 —— 先 load 会把空表读成"没有违禁词"，之后灌进去的词
	// 要等一次 reload 才生效，表现为"刚装好的服务不拦任何东西"。
	if err := s.ensureSeedWords(); err != nil {
		log.Printf("moderation: seeding banned words failed: %v", err)
	}
	if err := s.words.load(db.sql); err != nil {
		log.Printf("moderation: loading banned words failed: %v", err)
	}
	// 账号事件日志：每天清一次超过保留期的记录（表会随每次登录增长）
	s.startLoginLogPruner()

	// An empty invite code would silently let anyone register, so persist a
	// generated one and print it once.
	if cfg.InviteCode == "" {
		code, _ := db.getMeta("invite_code")
		if code == "" {
			code = newToken()[:10]
			_ = db.setMeta("invite_code", code)
		}
		s.cfg.InviteCode = code
	} else {
		_ = db.setMeta("invite_code", cfg.InviteCode)
	}

	var users int
	_ = db.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users)

	// ── 实时协作的配置（2026-10-10）────────────────────────────────────────
	//
	// 地址/端口只在**命令行传了**的时候才覆盖 —— 不传就保留库里的值、
	// 库里也没有就用默认值。这样"改服务器地址"有两种方式：
	//   · 重启时带 -live-host/-live-port（一次性、明确）
	//   · 直接改 app_settings（不用重启）
	// 两种都必须支持：部署时用前者，线上临时改地址用后者。
	if v := strings.TrimSpace(*liveHost); v != "" {
		_ = s.setSetting(settingLiveHost, v)
	}
	if v := strings.TrimSpace(*livePort); v != "" {
		_ = s.setSetting(settingLivePort, v)
	}
	// ⚠️ 密钥**必须**在这里落库：不然 `liveSecret()` 会现场生成一个，
	//    而那个值第二台服务器不知道 —— 现象是"票换出来了但连不上"。
	//    启动时传了 -live-secret 就用它，两边配一致才通。
	_ = s.liveSecret()
	log.Printf("live: 协作服务指向 %s:%s（secret %s）",
		s.setting(settingLiveHost, defaultLiveHost),
		s.setting(settingLivePort, defaultLivePort),
		map[bool]string{true: "已配置", false: "缺失"}[
			strings.TrimSpace(s.setting(settingLiveSecret, "")) != ""])

	mux := http.NewServeMux()
	s.routes(mux)
	if *enablePprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		log.Printf("  pprof       : enabled")
	}

	log.Printf("3e-schedule-server server %s", version)
	log.Printf("  data dir    : %s", cfg.DataDir)
	log.Printf("  cert dir    : %s", cfg.CertDir)
	log.Printf("  invite code : %s", s.cfg.InviteCode)
	if users == 0 {
		log.Printf("  FIRST RUN   : no accounts yet — the first registration becomes admin")
	}

	handler := withCORS(withLogging(mux))

	if cfg.Addr != "" {
		certFile := filepath.Join(cfg.CertDir, "server.crt")
		keyFile := filepath.Join(cfg.CertDir, "server.key")
		if fileExists(certFile) && fileExists(keyFile) {
			ln, err := net.Listen("tcp", cfg.Addr)
			if err != nil {
				log.Fatalf("listen %s: %v", cfg.Addr, err)
			}
			tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
			srv := &http.Server{
				Handler:           handler,
				TLSConfig:         tlsCfg,
				ReadHeaderTimeout: 15 * time.Second,
			}
			log.Printf("  HTTPS       : %s", cfg.Addr)
			go func() {
				if err := srv.ServeTLS(ln, certFile, keyFile); err != nil {
					log.Fatalf("https serve: %v", err)
				}
			}()
		} else {
			log.Printf("  HTTPS       : skipped (missing %s / %s)", certFile, keyFile)
		}
	}

	if cfg.HTTPAddr != "" {
		log.Printf("  HTTP        : %s", cfg.HTTPAddr)
		srv := &http.Server{Addr: cfg.HTTPAddr, Handler: handler, ReadHeaderTimeout: 15 * time.Second}
		log.Fatal(srv.ListenAndServe())
	}
	select {}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", s.handleHealth)

	// 主页 HTML 卡片的**样式清单**（2026-10-09）—— 见 home_styles.go 文件头。
	//
	// ⚠️ **不挂 `s.auth`**：它里面没有任何用户数据（就是几段写死的 CSS）。
	//    要求登录的话，"用户还没登录时 AI 想画个东西"这条路就断了，
	//    而那条路和"有没有账号"没有关系。
	mux.HandleFunc("GET /api/home/styles", s.handleHomeStyles)

	// ---- 主服务端自身的部署通道（2026-09-21）----
	//
	// ⚠️ 这条**刻意不走 `s.auth`** —— 它只认 `deploy-token.txt` 里的部署令牌。
	// 理由：部署脚本手上没有管理员会话；而且"某个成员的管理员会话能换掉整个
	// 服务端二进制"这件事本身就不该成立。见 deploy.go 的说明。
	mux.HandleFunc("POST /api/deploy/server", s.handleDeployServer)

	// 天气城市：应用内可配（原来只能靠 -lat/-lon 启动参数，改一次要重启）
	mux.HandleFunc("GET /api/weather/location", s.auth(s.handleGetWeatherLocation))
	mux.HandleFunc("PUT /api/weather/location", s.auth(s.handleSetWeatherLocation))

	// ── 实时笔记协作（2026-10-10）──────────────────────────────────────────
	//
	// 主服务端**不参与协作本身**，只负责签发一张短期票据 ——
	// 协作服务是**独立进程**（默认 :8445），长连接的负载特性（常驻连接、
	// 广播、内存里的房间表）不该进主服务，混在一起之后重启策略会互相绑架。
	//
	// 详见 `live_ticket.go` 顶部那段说明（以及 docs/设计-笔记实时协作扩展-2026-10-10.md）。
	mux.HandleFunc("POST /api/live/ticket", s.auth(s.handleLiveTicket))
	mux.HandleFunc("GET /api/live/config", s.auth(s.handleLiveConfig))
	// 管理员设置共享密钥（部署时不用改计划任务参数，直接 curl 这个）
	mux.HandleFunc("POST /api/live/secret", s.auth(s.handleLiveSetSecret))
	// 用户列表（名字 ↔ id）。**认票据**而不是 token —— 调它的是插件，插件没有 token。
	mux.HandleFunc("GET /api/live/users", s.handleLiveUsersByTicket)
	// 网页端管理控制台里改实时协作的地址/端口（管理员）
	mux.HandleFunc("GET /api/admin/live", s.auth(s.handleAdminLiveGet))
	mux.HandleFunc("POST /api/admin/live", s.auth(s.handleAdminLiveSet))
	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)

	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.auth(s.handleLogout))
	// 一键清空账户数据（2026-09-27）。
	//
	// ★ 用户要求：「给每一个端都加上一键清空账户数据的内容，点击以后删掉所有日程」。
	// ⚠️ 需要请求体带 `confirm`（= 自己的账号名）才执行 ——
	//    界面那个确认框挡的是"误点"，这个字段挡的是"脚本打错地址就清库"。
	mux.HandleFunc("POST /api/account/wipe", s.auth(s.handleWipeAccount))

	mux.HandleFunc("GET /api/me", s.auth(s.handleMe))
	mux.HandleFunc("POST /api/password", s.auth(s.handleChangePassword))
	mux.HandleFunc("GET /api/users", s.auth(s.handleUsers))
	mux.HandleFunc("GET /api/invite", s.auth(s.handleGetInvite))

	mux.HandleFunc("GET /api/calendars", s.auth(s.handleListCalendars))
	mux.HandleFunc("POST /api/calendars", s.auth(s.handleCreateCalendar))
	mux.HandleFunc("PUT /api/calendars/{id}", s.auth(s.handleUpdateCalendar))
	mux.HandleFunc("DELETE /api/calendars/{id}", s.auth(s.handleDeleteCalendar))
	// 成员：单个增删（老接口，桌面端/安卓端/AI 都在用）+ 一次性提交（权限页）
	mux.HandleFunc("POST /api/calendars/{id}/members", s.auth(s.handleAddMember))
	mux.HandleFunc("DELETE /api/calendars/{id}/members/{uid}", s.auth(s.handleRemoveMember))
	mux.HandleFunc("PUT /api/calendars/{id}/permissions", s.auth(s.handleSetPermissions))
	mux.HandleFunc("POST /api/calendars/{id}/share", s.auth(s.handleSetCalendarShared))
	mux.HandleFunc("POST /api/calendars/{id}/leave", s.auth(s.handleLeaveCalendar))

	mux.HandleFunc("POST /api/events", s.auth(s.handleCreateEvent))
	mux.HandleFunc("GET /api/events/{id}", s.auth(s.handleGetEvent))
	mux.HandleFunc("PUT /api/events/{id}", s.auth(s.handleUpdateEvent))
	mux.HandleFunc("DELETE /api/events/{id}", s.auth(s.handleDeleteEvent))
	mux.HandleFunc("PUT /api/events/{id}/occurrence", s.auth(s.handleUpsertOccurrence))
	mux.HandleFunc("DELETE /api/events/{id}/occurrence", s.auth(s.handleDeleteOccurrence))
	mux.HandleFunc("POST /api/events/{id}/snooze", s.auth(s.handleSnooze))
	mux.HandleFunc("DELETE /api/events/{id}/snooze", s.auth(s.handleClearSnooze))

	mux.HandleFunc("GET /api/instances", s.auth(s.handleListInstances))

	mux.HandleFunc("GET /api/sync/changes", s.auth(s.handleSyncChanges))
	mux.HandleFunc("GET /api/sync/snapshot", s.auth(s.handleSyncSnapshot))
	mux.HandleFunc("POST /api/sync/push", s.auth(s.handleSyncPush))

	mux.HandleFunc("GET /api/conflicts", s.auth(s.handleListConflicts))
	mux.HandleFunc("POST /api/conflicts/{id}/resolve", s.auth(s.handleResolveConflict))

	// ---- 收件箱（2026-09-19 新增，跨端方案第九节）----
	//
	// 全部需要登录：收件箱是 per-user 私有行，没有匿名入口。
	// 注意这里**没有**"从收件箱造事件"的接口 —— 那是客户端调
	// POST /api/events 建成功之后回写 decision 的（架构原则 1 的推论）。
	mux.HandleFunc("POST /api/inbox", s.auth(s.handleCreateInbox))
	mux.HandleFunc("GET /api/inbox", s.auth(s.handleListInbox))
	mux.HandleFunc("POST /api/inbox/batch", s.auth(s.handleInboxBatch))
	mux.HandleFunc("POST /api/inbox/{id}/decision", s.auth(s.handleInboxDecision))
	mux.HandleFunc("DELETE /api/inbox/{id}", s.auth(s.handleDeleteInbox))

	// ---- 邀请（2026-09-19 新增）----
	mux.HandleFunc("POST /api/calendars/{id}/invites", s.auth(s.handleCreateInvite))
	mux.HandleFunc("GET /api/calendars/{id}/invites", s.auth(s.handleListInvites))
	mux.HandleFunc("DELETE /api/invites/{id}", s.auth(s.handleRevokeInvite))
	// ⚠️ `GET /api/invites/{code}` 是**匿名**的 —— 落地页要用它。
	// 它只回元信息（能不能用 / 哪张日历 / 谁邀的 / 什么角色），
	// 绝不回日历内容或成员名单。
	mux.HandleFunc("GET /api/invites/{code}", s.handlePeekInvite)
	mux.HandleFunc("POST /api/invites/{code}/accept", s.auth(s.handleAcceptInvite))

	// ---- 团队（2026-09-21）----
	//
	// ⚠️ `GET /api/teams` **只返回自己已加入的团队**（见 handler 里的 WHERE）——
	// 全服团队列表会泄漏"这个部署里有哪些组织"。用户明确要求过这一点。
	mux.HandleFunc("POST /api/teams", s.auth(s.handleCreateTeam))
	mux.HandleFunc("GET /api/teams", s.auth(s.handleListTeams))
	mux.HandleFunc("GET /api/teams/{id}", s.auth(s.handleTeamDetail))
	mux.HandleFunc("POST /api/teams/{id}/leave", s.auth(s.handleLeaveTeam))
	mux.HandleFunc("DELETE /api/teams/{id}/members/{uid}", s.auth(s.handleRemoveTeamMember))
	mux.HandleFunc("PUT /api/teams/{id}/members/{uid}", s.auth(s.handleSetTeamMemberRole))
	// 团队邀请（fst_ 前缀，和日历邀请的 fsi_ 区分开）
	mux.HandleFunc("POST /api/teams/{id}/invites", s.auth(s.handleCreateTeamInvite))
	mux.HandleFunc("GET /api/teams/{id}/invites", s.auth(s.handleListTeamInvites))
	mux.HandleFunc("DELETE /api/team-invites/{id}", s.auth(s.handleRevokeTeamInvite))
	// ⚠️ peek 是**匿名**的（落地页要用）—— 只回元信息，不回成员名单
	mux.HandleFunc("GET /api/team-invites/{code}", s.handlePeekTeamInvite)
	mux.HandleFunc("POST /api/team-invites/{code}/accept", s.auth(s.handleAcceptTeamInvite))
	// 日历 ↔ 团队
	mux.HandleFunc("POST /api/calendars/{id}/teams", s.auth(s.handleShareCalendarToTeam))
	mux.HandleFunc("GET /api/calendars/{id}/teams", s.auth(s.handleListCalendarTeams))
	mux.HandleFunc("DELETE /api/calendars/{id}/teams/{teamId}", s.auth(s.handleUnshareCalendarFromTeam))

	// ---- 内容审核与举报（2026-09-23）----
	//
	// 违禁词：**写接口里逐处拦**（不是中间件）。原因见 moderation.go 的
	// blockedByWords 上方那段 —— 中间件只能拿到原始 body，解错字段就静默放过。
	//
	// 举报：任何登录用户都能提交，但只能看**自己**提交的。
	mux.HandleFunc("GET /api/moderation/meta", s.auth(s.handleModerationMeta))
	mux.HandleFunc("POST /api/reports", s.auth(s.handleCreateReport))
	mux.HandleFunc("GET /api/reports/mine", s.auth(s.handleListMyReports))

	// ---- 管理员（举报处理 + 词库维护）----
	//
	// ⚠️ whoami **不需要**管理员权限：前端要在渲染导航之前知道该不该画那个入口，
	// 所以必须有个"能提前问、不报错"的接口。其余全部走 requireAdmin。
	mux.HandleFunc("GET /api/admin/whoami", s.auth(s.handleIsAdmin))
	mux.HandleFunc("GET /api/admin/overview", s.auth(s.handleAdminOverview))
	mux.HandleFunc("GET /api/admin/reports", s.auth(s.handleAdminListReports))
	mux.HandleFunc("POST /api/admin/reports/{id}/handle", s.auth(s.handleAdminHandleReport))
	mux.HandleFunc("GET /api/admin/words", s.auth(s.handleAdminListWords))
	mux.HandleFunc("POST /api/admin/words", s.auth(s.handleAdminAddWord))
	mux.HandleFunc("PUT /api/admin/words/{id}", s.auth(s.handleAdminUpdateWord))
	mux.HandleFunc("DELETE /api/admin/words/{id}", s.auth(s.handleAdminDeleteWord))
	mux.HandleFunc("POST /api/admin/words/check", s.auth(s.handleAdminCheckText))
	// 管理员名单维护（2026-09-23）：授予 / 取消管理员。
	// 授予和取消**共用** POST .../admin 一个端点，body 里用 is_admin 区分 ——
	// 两者的校验完全一样，拆成两个接口就会出现"改了一处忘了另一处"。
	mux.HandleFunc("GET /api/admin/users", s.auth(s.handleAdminListUsers))
	mux.HandleFunc("POST /api/admin/users/{id}/admin", s.auth(s.handleAdminSetUserAdmin))

	// ---- 账号封禁 / 批量建号（2026-10-09）----
	//
	// ⚠️ `batch` 与 `{id}/ban` 段数不同（5 段 vs 6 段），Go 1.22 的 mux 不会
	// 判成冲突；而且 `batch` 是字面量段，比通配更具体，将来加
	// `POST /api/admin/users/{id}` 也不会抢到它。
	//
	// 封禁的判定不在这里、也不在下面的 handler 里 —— 它在 `auth` / `authOrKey`
	// 两个中间件（`banGuard`），这样"所有要登录的接口"一次性全覆盖，
	// 以后加路由不用记得补。
	mux.HandleFunc("POST /api/admin/users/batch", s.auth(s.handleAdminBatchCreateUsers))
	mux.HandleFunc("POST /api/admin/users/{id}/ban", s.auth(s.handleAdminBanUser))
	// 删除账号 / 删除举报单（2026-10-09 二次加强）。
	// ⚠️ 两条都是**不可逆**的，鉴权与确认在 handler 里（requireAdmin + 确认串），
	//    路由这层只负责"必须登录"。
	mux.HandleFunc("DELETE /api/admin/users/{id}", s.auth(s.handleAdminDeleteUser))
	mux.HandleFunc("DELETE /api/admin/reports/{id}", s.auth(s.handleAdminDeleteReport))

	// ---- AI 内容复核配置（2026-09-23）----
	//
	// ⚠️ 这几个接口回给前端的密钥**一律是掩码**（aiMaskKey），原文永不出服务器。
	// 详见 admin_ai.go 顶部那段"密钥永不外泄"。
	//
	// 为什么"试连接"要单独一个接口：AI 配置错的表现是**静默 fail-open**
	// （内容照常存入，只是没有复核），管理员没有任何办法知道配没配对。
	mux.HandleFunc("GET /api/admin/ai", s.auth(s.handleAdminGetAI))
	mux.HandleFunc("PUT /api/admin/ai", s.auth(s.handleAdminSetAI))
	mux.HandleFunc("POST /api/admin/ai/test", s.auth(s.handleAdminTestAI))

	// ---- 登录日志与内容审核审计（2026-09-23）----
	mux.HandleFunc("GET /api/admin/login-logs", s.auth(s.handleAdminLoginLogs))
	mux.HandleFunc("GET /api/admin/moderation-audit", s.auth(s.handleAdminModerationAudit))

	// ---------- API keys for machines ----------
	mux.HandleFunc("GET /api/apikeys", s.auth(s.handleListAPIKeys))
	mux.HandleFunc("POST /api/apikeys", s.auth(s.handleCreateAPIKey))
	mux.HandleFunc("DELETE /api/apikeys/{id}", s.auth(s.handleRevokeAPIKey))

	// ---------- open API (session token or API key) ----------
	//
	// Same handlers as the internal routes on purpose: an API key resolves to a
	// user id, so there is exactly one implementation of every operation.
	ext := s.authOrKey
	mux.HandleFunc("GET /api/ext/whoami", ext(s.handleExtWhoami))
	// 数据自检与修复（2026-09-25）。
	//
	// ★ 用户报的「我这边看得到、服务端提示日历不存在」「我这边看不到、
	//   但又有日程挂在那张日历上」—— 这类**引用完整性**问题只有服务端修得动。
	//
	// ⚠️ 两个都**只给管理员**，而且**都不会自动跑**：扫描是只读的，
	//    修复必须显式调一次。自动改生产数据是绝对不行的。
	mux.HandleFunc("GET /api/maintenance/scan", s.auth(s.handleMaintenanceScan))
	mux.HandleFunc("POST /api/maintenance/repair", s.auth(s.handleMaintenanceRepair))
	// 重复日程清理（2026-09-28）。**本人可用**（只动自己能写的日历），
	// 管理员加 ?all=1 可扫全库。`?dry=1` 只报不删。
	// 详见 dedupe.go 顶部：判据为什么必须保守、为什么只能打墓碑。
	mux.HandleFunc("POST /api/maintenance/dedupe", s.auth(s.handleMaintenanceDedupe))

	mux.HandleFunc("GET /api/ext/calendars", ext(s.handleListCalendars))
	// ★ 日历的写操作（2026-09-25 补）。
	//
	// 用户要求「把改日历的功能也加到 AI 里……所有的操作，包括自带的 AI
	// 以及第三方的 AI API」。在这之前外置 API **只能读日历**（GET），
	// 日程倒是增删改全有 —— 于是第三方 AI 能改日程却改不了日历本身。
	//
	// ⚠️ POST/PUT 走 `normalizeExtCalendarBody`：把"人话"翻成服务端字段
	//   （course/shared 布尔 → kind；members 按名字 → member_ids），
	//   和自带智能体用同一套说法，省得两边各学一套。
	// ⚠️ **不要**套 `normalizeExtBody`（那是给日程的）：它会往 body 里塞一个
	//   `e_` 前缀的 id，对日历是脏数据。
	mux.HandleFunc("POST /api/ext/calendars", ext(s.normalizeExtCalendarBody(s.handleCreateCalendar)))
	mux.HandleFunc("PUT /api/ext/calendars/{id}", ext(s.normalizeExtCalendarBody(s.handleUpdateCalendar)))
	mux.HandleFunc("DELETE /api/ext/calendars/{id}", ext(s.handleDeleteCalendar))
	// 列账号：建/分享日历之前要知道有哪些人（手柄是 session 版的 GET /api/users）
	mux.HandleFunc("GET /api/ext/users", ext(s.handleUsers))
	mux.HandleFunc("GET /api/ext/agenda", ext(s.handleExtAgenda))
	mux.HandleFunc("GET /api/ext/events", ext(normalizeExtQuery(s.handleListInstances)))
	mux.HandleFunc("POST /api/ext/events", ext(s.normalizeExtBody(s.handleCreateEvent)))
	mux.HandleFunc("PUT /api/ext/events/{id}", ext(s.normalizeExtBody(s.handleUpdateEvent)))
	mux.HandleFunc("DELETE /api/ext/events/{id}", ext(s.handleDeleteEvent))
	mux.HandleFunc("PUT /api/ext/events/{id}/occurrence", ext(s.handleUpsertOccurrence))
	mux.HandleFunc("DELETE /api/ext/events/{id}/occurrence", ext(s.handleDeleteOccurrence))
	mux.HandleFunc("GET /api/ext/weather", ext(s.handleWeather))
	mux.HandleFunc("GET /api/ext/openapi.json", ext(s.handleExtOpenAPI))
	mux.HandleFunc("GET /api/ext/guide", ext(s.handleExtGuide))

	// ---------- weather ----------
	mux.HandleFunc("GET /api/weather", ext(s.handleWeather))

	// ---------- hot update ----------
	mux.HandleFunc("GET /api/updates", ext(s.handleUpdateCheck))
	// ★ 公开的产物列表（2026-09-30）：官网的下载按钮靠它拿"最新包在哪"。
	//
	// 为什么不复用 /api/updates/manifest：那个是**管理员**接口（还带发布备注等内部信息），
	// 而官网要的只是"哪个平台、什么版本、多大、下哪儿" —— 这些下载页本来就公开。
	// 让它匿名可读，官网就不需要持有任何令牌，也不会出现"官网显示旧版本"。
	mux.HandleFunc("GET /api/updates/public", s.handleUpdatesPublic)
	mux.HandleFunc("GET /api/updates/download/{platform}", ext(s.handleUpdateDownload))
	mux.HandleFunc("GET /api/updates/manifest", s.auth(s.handleUpdateManifest))
	mux.HandleFunc("POST /api/updates/publish", s.auth(s.handleUpdatePublish))
	mux.HandleFunc("POST /api/device/report", ext(s.handleDeviceReport))
	mux.HandleFunc("GET /api/devices", s.auth(s.handleDevices))

	// ---------- plugin distribution（2026-10-08，批次 5）----------
	//
	// 和上面那组热更新**刻意分开**：热更新是"每个平台一个最新包"，
	// 插件是"每个 id 各自一条线"。合并成一张表会出现"发了个插件包，
	// 结果客户端的更新检查说该升级了"。详见 plugins.go 顶部。
	//
	// ⚠️ list / download 走 `ext`（会话令牌**或**机器 Key）而不是匿名：
	//    商店是登录之后才到得了的地方，没必要开匿名口子（对比 `/api/updates/public`
	//    那条是给官网下载页用的，那是真的需要匿名）。
	// ⚠️ manifest / publish 是管理员接口 —— **发布插件包"只有管理员能发布"** 这条
	//    和"管理员能不能看别人日程"是两回事：前者动的是**所有客户端要用的代码**。
	// ⚠️ 这里**没有**匿名版的插件清单接口 —— 那是刻意的，理由写在 `plugins.go`
	//    文件末尾那段（官网市场页走静态 `plugins.json`，不开跨域口子）。
	mux.HandleFunc("GET /api/plugins", ext(s.handlePluginsList))
	mux.HandleFunc("GET /api/plugins/download/{id}", ext(s.handlePluginDownload))
	mux.HandleFunc("GET /api/plugins/manifest", s.auth(s.handlePluginIndexView))
	mux.HandleFunc("POST /api/plugins/publish", s.auth(s.handlePluginPublish))

	// ---------- course timetable ----------
	mux.HandleFunc("GET /api/course/periods", ext(s.handleGetCoursePeriods))
	mux.HandleFunc("PUT /api/course/periods", s.auth(s.handlePutCoursePeriods))

	if s.cfg.Diag {
		mux.HandleFunc("GET /api/diag/weather", s.auth(s.handleWeatherDiag))
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	seq, _ := s.db.currentSeq()
	c := s.counts()
	// pending_update / staged 是 2026-09-21 加的。
	//
	// 为什么 health 要自述这件事：部署通道**先回响应、随后才替换重启**，
	// 所以"发完了"和"生效了"是两个时刻。不给一个能直接问的地方，
	// 就只能靠反复调 /api/health 看版本号变没变 —— 而"有没有在等着重启"
	// 这件事，只有服务端自己知道（见 web/selfupdate.go 的同一段说明）。
	writeJSON(w, 200, map[string]any{
		"ok":             true,
		"version":        version,
		"seq":            seq,
		"users":          c["users"],
		"events":         c["events"],
		"instances":      c["instances"],
		"pending_update": hasStaged(),
		"staged":         stagedExe(),
		"time":           time.Now().Unix(),
	})
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/api/health" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

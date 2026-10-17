package app

import (
	"log"
	"net/http"
	"os"
	"time"
)

// fileExists 判断路径存在且不是目录。
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

	// 导航标注台：拉某一片的**建筑轮廓**（2026-10-13）—— 见 osm.go 文件头。
	//
	// ⚠️ **不挂 `s.auth`**：标注台（`/nav-studio.html`）本身是个公开页面
	//    （本机工具，不承载用户数据），它没有会话令牌可带 ——
	//    挂了 auth 那个按钮就永远点不通。
	//
	// 不加登录不等于"开放代理"：osm.go 里把 bbox 限在**中国境内 + 6 公里以内**，
	// 并且缓存 10 分钟。理由写在那边。
	mux.HandleFunc("GET /api/nav/osm-buildings", s.handleOsmBuildings)
	// 「二次识别地名」用的腾讯**地点搜索**代理 —— 那个接口没有 CORS 头，
	// 浏览器直连会被拒（实测见 poi.go 顶部）。
	mux.HandleFunc("GET /api/nav/poi", s.handleNavPoi)

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

	// ---------- 邀请落地页（2026-10-15）----------
	//
	// 邀请链接 = `<用户自己配的服务器>/i/<code>`。以前客户端把它拼成
	// `https://<作者域名>:8444/i/<code>` —— 那既写死了作者的域名（合规问题），
	// 又要求网页端在 8444 上跑着。现在主服务端自己出这一页。
	// 详见 landing.go 顶部那段。
	mux.HandleFunc("GET /i/{code}", s.inviteLandingHandler)
	mux.HandleFunc("HEAD /i/{code}", s.inviteLandingHandler)

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
	// 把**内置的风险词**补进词库（2026-10-11）。seed 只在表为空时灌一次，
	// 所以老部署拿不到后来新增的那批，需要这个显式出口。
	mux.HandleFunc("POST /api/admin/words/seed-risk", s.auth(s.handleAdminSeedRiskWords))
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

	// ---------- 插件市场：提交 / 审核 / 公示（2026-10-15）----------
	//
	// 上面那组是"**我们自己的出包脚本**直发"（管理员、立即生效）。
	// 这一组是"**用户上传** → 管理员审核 → 才公示"，两者刻意分成两条路径：
	// 用户上传的东西**不能**走直发那条（那等于把别人的代码直接分发给所有人）。
	// 完整说明见 market.go 顶部。
	//
	// ⚠️ `/api/market/*` 是**匿名可读**的，和上面 `/api/plugins`（要凭证）不同：
	//    市场页是"给人逛的"，没注册的人也该看得见有哪些插件 ——
	//    而它的内容本来就是公开的（名称、描述、体积、下载地址）。
	mux.HandleFunc("GET /api/market/plugins", s.handlePluginMarketList)
	mux.HandleFunc("GET /api/market/download/{platform}/{id}", s.handlePluginMarketDownload)
	mux.HandleFunc("POST /api/plugins/submit", s.auth(s.handlePluginSubmit))
	mux.HandleFunc("GET /api/plugins/my", s.auth(s.handlePluginMySubmissions))
	mux.HandleFunc("GET /api/admin/plugins/pending", s.auth(s.handleAdminPluginPending))
	mux.HandleFunc("POST /api/admin/plugins/review", s.auth(s.handleAdminPluginReview))
	mux.HandleFunc("GET /api/admin/plugins/published", s.auth(s.handleAdminPluginPublished))
	mux.HandleFunc("POST /api/admin/plugins/delist", s.auth(s.handleAdminPluginDelist))

	// ---------- nav pack distribution（2026-10-12）----------
	//
	// 导航数据包（楼栋坐标表 + 可选腾讯 Key）的分发。详见 packs.go 顶部。
	//
	// ★ 授权码就是 packId 本身（用户拍板："一个包对应一个授权码"）——
	//   所以没有第二张表，也没有"码到包"的映射。
	//
	// ⚠️ 下载走 `ext`（会话令牌或机器 Key），**不开匿名口子**：
	//    包里可能带**明文腾讯 Key**，那是一个能刷额度的东西。
	// ⚠️ 上传/列表/查看/删除全部 `s.auth` + `requireAdmin` —— 用户拍板
	//    "只能通过我们管理员在后台进行上传"。
	// ⚠️ 列表（`GET /api/navpacks`）**只给名字和楼栋数，不给 Key、不给地址**：
	//    让"我们做了这个功能"能被看见，而"拿走文件"要凭码。
	mux.HandleFunc("GET /api/navpacks", ext(s.handleNavPacksList))
	mux.HandleFunc("GET /api/navpacks/download/{id}", ext(s.handleNavPackDownload))

	// ★ 2026-10-14：**外观包**（skinpack）—— 和上面那组逐字同构（见 skins.go 的文件头）。
	//   授权码就是包标识；列表与下载要登录（`ext`），上传/删除/预览要管理员。
	//   客户端的对应实现：`web/public/skin-studio.html`（设计台）、
	//   `plugin-appearance/SkinPack.kt`（手机端加载）、`scripts/publish-skinpack.mjs`（发布）。
	mux.HandleFunc("GET /api/skinpacks", ext(s.handleSkinPacksList))
	mux.HandleFunc("GET /api/skinpacks/download/{id}", ext(s.handleSkinPackDownload))
	mux.HandleFunc("GET /api/admin/skinpacks", s.auth(s.handleSkinPackAdminList))
	mux.HandleFunc("POST /api/admin/skinpacks/upload", s.auth(s.handleSkinPackUpload))
	mux.HandleFunc("GET /api/admin/skinpacks/{id}/view", s.auth(s.handleSkinPackAdminView))
	mux.HandleFunc("DELETE /api/admin/skinpacks/{id}", s.auth(s.handleSkinPackDelete))
	mux.HandleFunc("GET /api/admin/navpacks", s.auth(s.handleNavPackAdminList))
	mux.HandleFunc("POST /api/admin/navpacks/upload", s.auth(s.handleNavPackUpload))
	// ⚠️ `{id}/view` 必须注册在 `{id}` 之前吗？Go 1.22 的 mux 按**具体度**匹配，
	//    `/view` 更具体 → 会自动选它，和注册顺序无关。但这里仍然把它写在前面，
	//    为的是读代码的人不用去想那条规则（同 admin.go 里批量建号那段说明）。
	mux.HandleFunc("GET /api/admin/navpacks/{id}/view", s.auth(s.handleNavPackAdminView))
	mux.HandleFunc("DELETE /api/admin/navpacks/{id}", s.auth(s.handleNavPackDelete))

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
		// ⚠️ `X-Live-Ticket` 必须在这里 —— 实时协作的 REST 接口用**票据**鉴权
		//    （插件拿不到登录 token），浏览器预检过不去时表现是
		//    "网页端能看笔记列表，但协作那一块全是网络错误"。
		w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type,X-Live-Ticket,X-Deploy-Token")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

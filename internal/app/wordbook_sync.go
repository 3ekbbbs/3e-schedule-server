package app

import (
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"net"
	"crypto/tls"
	"os"
	"strconv"
	"strings"
	"time"
)

// 「背词」同步（2026-10-10，端口 8446）。
//
// ## 它同步什么、不同步什么（这条比实现重要）
//
// **只同步"每张卡的学习状态"**（FSRS 的 difficulty / stability / due / reps / lapses）。
//
// ⚠️ **绝不同步词库本体**：
//  ① 内置的四六级是随包发的，两端本来就有，传一遍纯属浪费；
//  ② **用户自己导入的词库有版权问题** —— 那是他自己提供的教材数据，
//     传到服务器上性质就变了（从"本机自用"变成"我们替他在服务器上存了一份"）。
//     这条写在企划案 §9/§13，是硬约束，不是性能取舍。
//
// ## 为什么是独立端口、共用进制的第二个 listener
//
// 用户的原话是"再创建一个**线程**，与默认进程独立开"。所以：
//  · **端口独立**（8446）—— 长连接/慢查询不会挤占主服务的连接；
//  · **共用 DB 和鉴权** —— 不重复造 token 校验，也不引入第二份用户表。
// 这比"再发一个二进制"省事得多，而且**故障面没有变大**（同一个进程，
// 挂了是一起挂；真正要隔离的是"资源争抢"，端口分开就够）。
//
// ## 冲突策略
//
// **`updated_at` 后写胜**（同一张卡两端都改时取新的）。第一版**不做复杂合并** ——
// 背词是单人低频操作（一天一次），真需要合并的时候再谈。

// defaultWordbookPort 背词同步的默认端口。
//
// ⚠️ 端口分配：8443 主服务 · 8444 网页 · 8445 实时协作 · **8446 背词同步**。
const defaultWordbookPort = "8446"

// wordbookSync 挂在 Server 上（复用它的 db / 鉴权）。
type wordbookSync struct {
	s *Server
}

// wordbookCard 是要同步的一行 —— 字段名和客户端的 `cards` 表一一对应。
//
// ⚠️ 用短字段名（`d`/`s`/`due`…）是为了省流量：一个重度用户可能有一两万张卡，
// 每次同步都是全量比对。**但不要为了省这点而改成二进制** —— 可读性值这个钱。
type wordbookCard struct {
	WordID   string  `json:"w"`
	Diff     float64 `json:"d"`
	Stab     float64 `json:"s"`
	DueAt    int64   `json:"due"`
	Phase    string  `json:"ph"`
	Reps     int     `json:"r"`
	Lapses   int     `json:"l"`
	LastRev  int64   `json:"lr"`
	Updated  int64   `json:"u"`
}

// wordbookDaily 每日完成量（打卡连续天数、今日进度都要它）。
type wordbookDaily struct {
	Day     string `json:"day"`
	NewDone int    `json:"nd"`
	RevDone int    `json:"rd"`
}

// registerWordbookRoutes 把路由挂到给定的 mux 上。
//
// ⚠️ 单独一个函数而不是塞进 `routes()`：这样 8446 那个 listener 能**只挂这一组**，
// 不用把主服务的全部接口再暴露一遍（那等于多开一个全功能入口）。
func (s *Server) registerWordbookRoutes(mux *http.ServeMux) {
	w := &wordbookSync{s: s}

	// 一次请求拿全量（客户端拿它和自己的比，缺的补、旧的更新）
	mux.HandleFunc("GET /api/wordbook/pull", s.auth(w.handlePull))
	// 一次请求推增量（只推"客户端改过的"）
	mux.HandleFunc("POST /api/wordbook/push", s.auth(w.handlePush))
	// 轻量探活（客户端进来先打一下，判断"同步能不能用"）
	mux.HandleFunc("GET /api/wordbook/health", w.handleHealth)
}

func (w *wordbookSync) handleHealth(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, 200, map[string]any{
		"ok":      true,
		"service": "wordbook-sync",
		"time":    time.Now().Unix(),
		// 告诉客户端"这个服务会不会存你的词库本体" —— 明写成 false，免得将来有人加
		"stores_wordlibs": false,
	})
}

// handlePull 拉全量。
//
// ⚠️ 全量而不是"自上次以来的增量"：卡的数量级是**万**，
//    一次 JSON 也就几百 KB —— 而增量协议要维护 `since` 游标，
//    一旦客户端时钟不对就会**静默漏掉**数据（项目里在 `changes?since=0` 上踩过，红线 138）。
//    **能用笨办法做对的，不要用聪明办法做错的。**
func (w *wordbookSync) handlePull(rw http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	if uid == "" {
		apiErr(rw, 401, "未登录")
		return
	}

	cards, err := w.s.db.wordbookCards(uid)
	if err != nil {
		apiErr(rw, 500, "读不了："+err.Error())
		return
	}
	daily, err := w.s.db.wordbookDaily(uid)
	if err != nil {
		apiErr(rw, 500, "读不了："+err.Error())
		return
	}
	writeJSON(rw, 200, map[string]any{
		"ok":    true,
		"cards": cards,
		"daily": daily,
		"now":   time.Now().Unix(),
	})
}

type wordbookPushReq struct {
	Cards []wordbookCard  `json:"cards"`
	Daily []wordbookDaily `json:"daily"`
}

// handlePush 推增量。
//
// ⚠️ **必须真做"交集"**（后写胜），不能无脑覆盖：
//    用户在手机 A 上学了 20 张，手机 B 上学了另外 30 张，
//    B 先推、A 后推 —— 如果 A 的推送是"整表替换"，B 那 30 张就**没了**
//    （而且是静默的：用户只看到"学习记录少了一截"）。
//    所以这里逐卡比 `updated`，只在新数据更新时才写。
func (w *wordbookSync) handlePush(rw http.ResponseWriter, r *http.Request) {
	uid := userIDOf(r)
	if uid == "" {
		apiErr(rw, 401, "未登录")
		return
	}
	var req wordbookPushReq
	// 与 readJSON 同一个上限（4 MB）。
	// ⚠️ 2026-10-16 改：原来这里直接用 `json.NewDecoder(r.Body).Decode(&req)`，
	//    没有任何大小上限 —— 下面那句「一次推的卡太多了（>50000）」得先把整个 body
	//    读进内存才轮得到它，也就是说那道"上限"防不住超大 body。
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		apiErr(rw, 400, "读请求体失败")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		apiErr(rw, 400, "请求体不是合法 JSON")
		return
	}
	// 单次上限：正常批量是几百到几千张。放一个上限是为了让"客户端写错循环"
	// 这种事在这一层就被拦住，而不是把库写爆。
	if len(req.Cards) > 50000 {
		apiErr(rw, 400, "一次推的卡太多了（>50000）")
		return
	}

	applied, skipped, err := w.s.db.wordbookUpsertCards(uid, req.Cards)
	if err != nil {
		apiErr(rw, 500, "写不进去："+err.Error())
		return
	}
	if err := w.s.db.wordbookUpsertDaily(uid, req.Daily); err != nil {
		apiErr(rw, 500, "写不进去："+err.Error())
		return
	}
	writeJSON(rw, 200, map[string]any{
		"ok":      true,
		"applied": applied, // 真正写进去的（比服务器上的新）
		"skipped": skipped, // 服务器上有更新的版本，跳过
	})
}

// ---------------------------------------------------------------- 表

// wordbookEnsureTables 建表（幂等）。
//
// ⚠️ 和客户端**用同一套字段语义**，但**故意不建外键**：
//    词库本体不上服务器，所以这里没有 `words` 表可引用 —— 只有"哪张卡、什么状态"。
func (d *DB) wordbookEnsureTables() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS wb_cards (
			user_id     TEXT    NOT NULL,
			word_id     TEXT    NOT NULL,
			difficulty  REAL    NOT NULL DEFAULT 0,
			stability   REAL    NOT NULL DEFAULT 0,
			due_at      INTEGER NOT NULL DEFAULT 0,
			phase       TEXT    NOT NULL DEFAULT 'New',
			reps        INTEGER NOT NULL DEFAULT 0,
			lapses      INTEGER NOT NULL DEFAULT 0,
			last_review INTEGER NOT NULL DEFAULT 0,
			updated_at  INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (user_id, word_id)
		)`,
		`CREATE TABLE IF NOT EXISTS wb_daily (
			user_id  TEXT    NOT NULL,
			day      TEXT    NOT NULL,
			new_done INTEGER NOT NULL DEFAULT 0,
			rev_done INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (user_id, day)
		)`,
	}
	for _, s := range stmts {
		if _, err := d.sql.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) wordbookCards(uid string) ([]wordbookCard, error) {
	rows, err := d.sql.Query(
		`SELECT word_id, difficulty, stability, due_at, phase, reps, lapses, last_review, updated_at
		 FROM wb_cards WHERE user_id=?`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]wordbookCard, 0, 256)
	for rows.Next() {
		var c wordbookCard
		if err := rows.Scan(&c.WordID, &c.Diff, &c.Stab, &c.DueAt, &c.Phase,
			&c.Reps, &c.Lapses, &c.LastRev, &c.Updated); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (d *DB) wordbookDaily(uid string) ([]wordbookDaily, error) {
	rows, err := d.sql.Query(
		`SELECT day, new_done, rev_done FROM wb_daily WHERE user_id=? ORDER BY day DESC LIMIT 400`, uid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]wordbookDaily, 0, 64)
	for rows.Next() {
		var x wordbookDaily
		if err := rows.Scan(&x.Day, &x.NewDone, &x.RevDone); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// wordbookUpsertCards 逐卡"后写胜"，返回 (写入数, 跳过数)。
func (d *DB) wordbookUpsertCards(uid string, cards []wordbookCard) (int, int, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// 先查已有卡的 updated_at（一次全查，避免逐卡一条 SELECT）
	existing := map[string]int64{}
	rows, err := tx.Query(`SELECT word_id, updated_at FROM wb_cards WHERE user_id=?`, uid)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var id string
		var up int64
		if err := rows.Scan(&id, &up); err != nil {
			rows.Close()
			return 0, 0, err
		}
		existing[id] = up
	}
	rows.Close()

	stmt, err := tx.Prepare(
		`INSERT INTO wb_cards
		 (user_id, word_id, difficulty, stability, due_at, phase, reps, lapses, last_review, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(user_id, word_id) DO UPDATE SET
		   difficulty=excluded.difficulty, stability=excluded.stability, due_at=excluded.due_at,
		   phase=excluded.phase, reps=excluded.reps, lapses=excluded.lapses,
		   last_review=excluded.last_review, updated_at=excluded.updated_at`)
	if err != nil {
		return 0, 0, err
	}
	defer stmt.Close()

	applied, skipped := 0, 0
	for _, c := range cards {
		if c.WordID == "" {
			continue
		}
		// ★ 后写胜：服务器上那份更新就跳过（这是"两端各学一半"能合起来的关键）
		if up, ok := existing[c.WordID]; ok && up > c.Updated {
			skipped++
			continue
		}
		if _, err := stmt.Exec(uid, c.WordID, c.Diff, c.Stab, c.DueAt, c.Phase,
			c.Reps, c.Lapses, c.LastRev, c.Updated); err != nil {
			return applied, skipped, err
		}
		applied++
	}
	if err := tx.Commit(); err != nil {
		return applied, skipped, err
	}
	return applied, skipped, nil
}

// wordbookUpsertDaily 每日量**取最大值**（不是后写胜）。
//
// ⚠️ 这里和卡不一样：卡是"状态"（新的就是对的），而每日量是**累计计数** ——
//    手机 A 说 12、手机 B 说 8，正确答案是 12 而不是"后写的那个 8"。
//    用 MAX 是最不坏的选择（精确合并要同步每次复习的流水，第一版不值）。
func (d *DB) wordbookUpsertDaily(uid string, daily []wordbookDaily) error {
	if len(daily) == 0 {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(
		`INSERT INTO wb_daily (user_id, day, new_done, rev_done) VALUES (?,?,?,?)
		 ON CONFLICT(user_id, day) DO UPDATE SET
		   new_done=MAX(new_done, excluded.new_done),
		   rev_done=MAX(rev_done, excluded.rev_done)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, x := range daily {
		if x.Day == "" {
			continue
		}
		if _, err := stmt.Exec(uid, x.Day, x.NewDone, x.RevDone); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// startWordbookSyncServer 在**独立端口**上起第二个 listener（用户要的"再开一个"）。
//
// ## 默认端口 8446，但**允许关掉**
//
// 一开始写的是"没设环境变量就不起" —— 后来改成**默认开**：
// 用户要的就是"部署完就能用"，而"要记得去加一个环境变量"是**部署脚本不会提醒你**的那种坑
// （跑完一切正常，就是同步 404）。
//
// 现在：
//
//	`WORDBOOK_PORT=off` / `0` / `-`  → 关掉；
//	`WORDBOOK_PORT=9446`            → 换端口；
//	不设                            → 起在 8446。
//
// ⚠️ 端口分配（现有的）：8443 主服务 · 8444 网页 · 8445 实时协作 · **8446 背词同步**。
func (s *Server) startWordbookSyncServer() {
	port := strings.TrimSpace(os.Getenv("WORDBOOK_PORT"))
	switch port {
	case "off", "0", "-", "no", "false":
		log.Printf("背词同步：按 WORDBOOK_PORT=%s 的要求**未启动**", port)
		return
	case "":
		port = defaultWordbookPort
	}
	if _, err := strconv.Atoi(port); err != nil {
		log.Printf("WORDBOOK_PORT 不是数字（%q），背词同步**未启动**", port)
		return
	}
	if err := s.db.wordbookEnsureTables(); err != nil {
		log.Printf("背词同步：建表失败，未启动：%v", err)
		return
	}

	mux := http.NewServeMux()
	s.registerWordbookRoutes(mux)

	// ⚠️⚠️ **必须和主服务一样走 HTTPS**（2026-10-10 实测踩到）。
	//
	// 第一版这里写的是 `ListenAndServe`（纯 HTTP），结果部署上去**连不上** ——
	// 因为 `example.com` 上没有 8446 的明文监听预期，而客户端按 `https://` 去连，
	// TLS 握手直接失败（curl 一探是空响应，看起来像"服务没起来"，其实是协议不对）。
	//
	// 复用主服务的**同一张证书**（同一个 `CertDir`）：域名没变，
	// 换一张证书反而会让客户端报 `subjectAltNames` 不匹配（项目里踩过，见 `ApiClient.withCandidates`）。
	// ⚠️ 证书检查放在**监听之前**：没有证书就**不启动**，而不是先 listen 再退回明文。
	//
	// 2026-10-16 改。原实现是「listen → 发现没证书 → `srv.ListenAndServe()` 走明文 HTTP」，
	// 而那句日志写的是"仅本机/内网可用" —— 两件事都不对：
	//   ① 监听地址是 `":"+port`，也就是 **0.0.0.0**，不是回环。云服务器上只要 8446 可达，
	//      背词同步的 Bearer 令牌与学习记录就是**明文过公网**；
	//   ② "退回明文"是在**降级安全**的前提下换服务可用，而这条通道本来就要带凭证 ——
	//      宁可不启动（用户看到的是"背词同步没起来"，而不是它悄悄裸奔着）。
	// 线上部署有证书（与主服务共用同一张），所以这条改动不影响正常启动。
	certFile := filepath.Join(s.cfg.CertDir, "server.crt")
	keyFile := filepath.Join(s.cfg.CertDir, "server.key")
	if !fileExists(certFile) || !fileExists(keyFile) {
		log.Printf("背词同步：**没有证书**（%s / %s），**拒绝启动**。"+
			"这条通道要带登录凭证，不能退回明文 HTTP；把证书放好再重启。", certFile, keyFile)
		return
	}

	// 监听地址：默认 `0.0.0.0`，与主服务同样的可达性要求（客户端要连它；
	// 手机端背词插件虽然走 `HostApi.request`，但目标仍是这台服务器的这个端口）。
	// 需要收窄时用 `WORDBOOK_BIND` 显式指定（例如某个内网 IP）。
	bind := strings.TrimSpace(os.Getenv("WORDBOOK_BIND"))
	if bind == "" {
		bind = "0.0.0.0"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bind, port))
	if err != nil {
		log.Printf("背词同步：监听 %s:%s 失败：%v", bind, port, err)
		return
	}

	srv := &http.Server{
		Handler:           mux,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 15 * time.Second,
		// 客户端推全量可能几十万张卡，读写都给宽一点（这是"批量同步"不是"短请求"）
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 120 * time.Second,
	}

	go func() {
		log.Printf("背词同步已启动：https %s:%s（只同步学习状态，**不存词库**）", bind, port)
		if err := srv.ServeTLS(ln, certFile, keyFile); err != nil && err != http.ErrServerClosed {
			log.Printf("背词同步退出：%v", err)
		}
	}()
}

// 让编译器盯着：这块确实用到了 database/sql（表操作都在 DB 上）。
var _ = sql.ErrNoRows

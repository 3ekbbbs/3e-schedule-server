package app

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 导航标注台：**建筑轮廓的来源**（2026-10-13）。
//
// # 一、它为什么走服务端（而不是浏览器直连）
//
// 标注台要从 **OpenStreetMap**（Overpass API）拉某一片的建筑轮廓 ——
// 用户的原话是"最好 3D 建模可以从地图里面直接提取，而不是我挨个重画"。
//
// 浏览器**直连** Overpass 这条路实测**不可靠**：
//
//	· CORS 本身是通的（Overpass 回 `Access-Control-Allow-Origin: *`，允许 POST）
//	· 但实测浏览器导航到 `overpass.kumi.systems` **直接超时**，
//	  而同一台机器的 Node 拉同一个地址是有响应的（429 / 504）
//	· ⚠️ **根因没有查清。** 当时的猜测是"本机 Kaspersky 只拦浏览器"，
//	  但那次测量恰好撞上 Overpass 整体拥堵（见下面熔断器那段）——
//	  而拥堵**足以单独解释**浏览器超时（同一时刻 Node 拉 kumi 也挂了 90 秒）。
//	  所以别拿这条注释当"浏览器被拦"的证据去改配置。
//
// 走服务端的理由**不依赖上面那个根因**，它是形状上的优势：
//
//	· 标注台和服务端**同源**（同一个进程伺服，实测两边 `/api/health` 返回同一份），
//	  所以前端用的是**相对路径** —— 连 CORS 都不用管
//	· 服务端有**两层缓存**，而且能把结果**沉淀下来**（浏览器那边是每次现拉）
//
//	浏览器 → /api/nav/osm-buildings → Overpass → 原样回
//
// ⚠️ 但**它只是兜底**。主路径是 [scripts/osm-bake.mjs] 烘焙出来的静态文件
//    （零网络、离线可用、不受上游拥堵影响）。前端的选择顺序见
//    `nav-studio.js` 的 `nsFetchOsm`：烘焙 → 服务端 → 浏览器直连。
//
// # 二、★ 关于"能不能合法地下载下来缓存" —— 能，而且这是官方希望你做的
//
// 用户问得很对："那些数据我可以合法下载下来，然后进行缓存嘛，就是不每次都调用镜像"
//
// **OpenStreetMap 的数据是 ODbL 协议**，它明确允许：
//
//	· **下载、存储、离线使用**（包括商用）
//	· **缓存**（Overpass 自己的使用条款就写着"别反复查同一片区域，缓存它"）
//	· 做成"Produced Work"（渲染出来的图、我们这种导航包）**只需署名**
//
// 需要遵守的只有两条：
//
//	① **署名**：标注台界面上写了 "© OpenStreetMap contributors" —— **别删**。
//	   导出成 navpack 分发给别人时，那条署名也要跟着走。
//	② 如果**把这份建筑数据本身当成一个数据库再分发**（而不是当成渲染结果），
//	   那一份要按 ODbL 授权出去。我们这里是"用户自己的校园标注"，不涉及。
//
// 所以这个文件里做了**两层缓存**：
//
//	内存（进程内，快） → **磁盘**（`<dataDir>/osm-cache/`，重启也不丢）
//
// 磁盘缓存的 TTL 给到 **30 天** —— 建筑轮廓几个月都不会变，
// 而"每次都去敲欧洲的免费服务器"对谁都不好。
//
// # 三、为什么不加登录
//
// 标注台（`/nav-studio.html`）本身是个**公开页面**（本机工具，不承载用户数据），
// 它没有会话令牌可带。加 `s.auth` 的话那个按钮就永远点不通。
//
// 所以它是公开的，但**不是开放代理** —— 见 [osmAllowedBBox]：
//
//	· bbox 必须落在**中国境内**且**不超过 0.06°**（约 6.6km × 6.3km）
//	· 同一片区域**两层缓存**，磁盘那份 30 天
//
// 最后一条不是"多管闲事"：不圈定范围就等于给全世界开了一个免费的 Overpass 代理，
// 而账是记在我们服务器上的。

const (
	// Overpass 的实例。**并行赛跑**，谁先回来用谁。
	//
// ⚠️ 为什么不按顺序试：Overpass 的公共实例会**整体拥堵**（见下面熔断器那段），
//    实测第一个实例等了 52 秒才超时 —— 按顺序试的话用户要等 50 + 50 + 50 秒。
//
// 所以把**亚洲/俄罗斯的镜像放在前面**（离得近、更可能通），并且**同时发**：
// 哪个先回就用哪个，总耗时 = 最快的那个，而不是最慢的那些之和。
//
// ⚠️ 这个清单里**只放全球数据**的实例。`overpass.osm.ch` 是**瑞士的区域镜像**，
//    拿它查中国校区会在 1 秒内回一个**合法但空的** `{"elements":[]}` ——
//    而"谁先回用谁"会让它次次胜出。（烘焙脚本那边踩过这个坑，见 osm-bake.mjs。）
	osmEndpoints = "https://overpass.osm.jp/api/interpreter|https://overpass.kumi.systems/api/interpreter|https://overpass-api.de/api/interpreter|https://overpass.openstreetmap.ru/api/interpreter"

	// 单次查询允许的最大 bbox 跨度（度）。
	// 0.06° 纬度 ≈ 6.6km，够一个校区了；再大就是"拿它当通用地图下载器"。
	osmMaxSpanDeg = 0.06

	// **磁盘缓存**的有效期。
	//
	// ⚠️ 给到 30 天是刻意的：建筑轮廓几个月都不会变，而"每次刷新都去敲
	//    欧洲的免费服务器"既慢又对别人不礼貌。ODbL 允许缓存，Overpass 也鼓励。
	osmDiskTTL = 30 * 24 * time.Hour

	// 单次上游请求的超时。
	//
	// ⚠️ 给得比较宽（100 秒）：实测一次冷查询在本机要跑 **3 分 46 秒**，
	//    而国内服务器到欧洲更慢。卡太紧的话永远是 502，
	//    而用户看到的是"拉取失败" —— 他会以为这个功能没做。
	//
	// 代价是**第一次**要等 —— 所以标注台那边写了"最多一两分钟"。
	// 拉回来之后：内存命中是瞬时的，磁盘命中也是瞬时的（重启都不丢）。
	osmUpstreamTimeout = 100 * time.Second

	// **连接**阶段的超时（DNS + TCP + TLS）。
	//
	// ⚠️ 这个值单独拎出来是必须的，不是为了快：
	//    一个笼统的 100 秒超时会让"连不上"和"连上了但对方不回"变成**同一句话**
	//    （`context deadline exceeded (Client.Timeout exceeded while awaiting headers)`），
	//    而这两件事的修法完全相反 —— 前者要换镜像/换机房，后者要加大超时。
	//    实测在服务器上第一次跑就是这么被误导的：四个镜像全报同一句，
	//    看起来像"Overpass 都在忙"，而这两件事的修法完全相反。
	//
	// 10 秒足够连上任何一个正常机房（含跨境），连不上就是连不上。
	osmDialTimeout = 10 * time.Second

	// **等响应头**的超时（连接建好之后）。
	//
	// Overpass 是"收到请求才开始算"，一个校区级别的查询在它那边排队 + 跑
	// 可能要几十秒 —— 所以这一段必须宽。
	osmHeaderTimeout = 85 * time.Second
)

// osmMemEntry 内存缓存的一条（**原始 JSON**，不做解析）。
//
// 存原始 JSON 而不是解析后的结构：我们只是**透传**，
// 解析一遍再序列化回去只会多一处可能出错的地方（而且会丢掉我们不认识的字段）。
type osmMemEntry struct {
	body string
	at   time.Time
}

var (
	osmLedMu  sync.Mutex
	osmLedger = map[string]osmMemEntry{}
)

// ---------------------------------------------------------------------------
// ★★ 熔断器（2026-10-13）
//
// ## 为什么必须有它
//
// Overpass 的公共实例**会整体拥堵**。实测：某一段时间四个镜像**同时**挂住，
// 100 秒全部超时 —— 而同一时刻本机拉 `overpass-api.de` 拿到的是 **504**、
// 拉 `kumi.systems` 挂 90 秒。也就是说那不是"某个镜像坏了"，
// 是**上游集体过载**。而十分钟后再试，**6.2 秒**就拿到了真数据。
//
// 原来的行为是：**每一个**没烘焙过的区域，用户都要**干等 100 秒**才看到失败。
// 换个校区再点一次，又是 100 秒。那不是"慢"，是**看起来坏了**。
//
// ## 口径
//
// ```
//   连续 osmFailStreakLimit 次全挂  →  熔断 osmDownCooldown 分钟
//   熔断期间                      →  立刻回 503（毫秒级），并告诉用户"怎么办"
//   任何一次成功                   →  立刻复位
// ```
//
// ⚠️ **不硬编码"上游不通"** —— 上游的状态是随时变的（拥堵几分钟、恢复几秒钟）。
//    熔断器是"自己量出来的"，所以它好了自己就复位。
//
// ⚠️ 503 而不是 502：语义是"暂时不可用，过会儿再来"，
//    而且前端看到 5xx 会继续往下试直连那条路（见 nav-studio.js 的 nsFetchOsm）。
//
// ⚠️ **别把"熔断"当成"这条路不通"的结论。** 它只是"刚才不通，先别堵着用户"。
//    2026-10-13 我自己就在这里栽过一次：看到四个镜像全超时，
//    就写下"这台服务器出网到 Overpass 不通"—— 而十分钟后它 6.2 秒就通了。
//    那个错误的结论差点让"烘焙"从"更快更稳"变成"唯一可行"，
//    也差点让人去查机房出网策略（一条完全错误的方向）。
// ---------------------------------------------------------------------------

var (
	osmDownMu     sync.Mutex
	osmDownUntil  time.Time
	osmFailStreak int
)

const (
	/** 连续几次全挂就熔断。**2 次**是有意的：一次可能是某个镜像抽风，两次就不是偶然了。 */
	osmFailStreakLimit = 2
	/** 熔断多久。⚠️ 给到 10 分钟：够用户去跑一次烘焙脚本，又不至于"恢复了还一直断着"。 */
	osmDownCooldown = 10 * time.Minute
)

// osmIsDown 现在是否处于熔断中。返回剩余冷却时间（0 表示没断）。
func osmIsDown() time.Duration {
	osmDownMu.Lock()
	defer osmDownMu.Unlock()
	if osmDownUntil.IsZero() {
		return 0
	}
	left := time.Until(osmDownUntil)
	if left <= 0 {
		osmDownUntil = time.Time{}
		osmFailStreak = 0
		return 0
	}
	return left
}

// osmMarkFail 记一次"全挂"，够次数就熔断。
func osmMarkFail() {
	osmDownMu.Lock()
	defer osmDownMu.Unlock()
	osmFailStreak += 1
	if osmFailStreak >= osmFailStreakLimit {
		osmDownUntil = time.Now().Add(osmDownCooldown)
		log.Printf("osm: 连续 %d 次全部镜像失败 → 熔断 %s", osmFailStreak, osmDownCooldown)
	}
}

// osmMarkOk 任何一次成功都立刻复位（网络恢复了就该马上恢复）。
func osmMarkOk() {
	osmDownMu.Lock()
	defer osmDownMu.Unlock()
	if osmFailStreak != 0 || !osmDownUntil.IsZero() {
		log.Printf("osm: 上游恢复，熔断复位（此前连续失败 %d 次）", osmFailStreak)
	}
	osmFailStreak = 0
	osmDownUntil = time.Time{}
}

// osmCacheFile 磁盘缓存的文件路径。
//
// ⚠️ 文件名用 bbox 的 **sha1 前 16 位**，不直接用 bbox：
//    bbox 里有逗号和小数点（`21.143800,110.294700,…`），
//    当文件名在 Windows 上是不合法的（逗号可以但点号连续会出问题），
//    而且长度也不好控。哈希一下就干净了。
func osmCacheFile(dataDir, bbox string) string {
	sum := sha1.Sum([]byte(bbox))
	name := hex.EncodeToString(sum[:])[:16] + ".json"
	return filepath.Join(dataDir, "osm-cache", name)
}

// osmDiskRead 读磁盘缓存。命中返回 (body, true)。
//
// ⚠️ 过期**不删文件**（只是不用它）—— 万一上游挂了，旧数据也比"什么都没有"强。
//    删不删由 [osmDiskWrite] 的覆盖来收尾，不需要额外的清理逻辑。
func osmDiskRead(dataDir, bbox string) (string, bool) {
	f := osmCacheFile(dataDir, bbox)
	st, err := os.Stat(f)
	if err != nil || time.Since(st.ModTime()) > osmDiskTTL {
		return "", false
	}
	raw, err := os.ReadFile(f)
	if err != nil || !json.Valid(raw) {
		return "", false
	}
	return string(raw), true
}

// osmDiskWrite 写磁盘缓存。
//
// ⚠️ 写失败**只记日志、不影响这次请求** —— 缓存写不进去（磁盘满 / 权限）
//    不该让用户拿不到刚拉到的数据。
func osmDiskWrite(dataDir, bbox, body string) {
	f := osmCacheFile(dataDir, bbox)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		log.Printf("osm cache mkdir: %v", err)
		return
	}
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		log.Printf("osm cache write: %v", err)
	}
}

// osmAllowedBBox 校验并规范化 bbox。
//
// 参数顺序是 **south,west,north,east**（和 Overpass 一致，不是 GeoJSON 那套）。
//
// 拒绝的情况：
//   - 四个数不是数 / south>=north / west>=east
//   - 不在中国境内（纬度 3~54、经度 73~136）
//   - 跨度超过 [osmMaxSpanDeg]
func osmAllowedBBox(q url.Values) (string, error) {
	get := func(k string) (float64, error) {
		raw := strings.TrimSpace(q.Get(k))
		if raw == "" {
			return 0, fmt.Errorf("缺 %s", k)
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v != v { // v != v 挡 NaN
			return 0, fmt.Errorf("%s 不是数字", k)
		}
		return v, nil
	}
	south, err := get("south")
	if err != nil {
		return "", err
	}
	west, err := get("west")
	if err != nil {
		return "", err
	}
	north, err := get("north")
	if err != nil {
		return "", err
	}
	east, err := get("east")
	if err != nil {
		return "", err
	}

	if south >= north || west >= east {
		return "", fmt.Errorf("范围不合法（south 要小于 north、west 要小于 east）")
	}
	// ⚠️ **境内检查要排在"范围太大"之前**：先报"只支持中国境内"用户才知道是哪儿的问题；
	//    先报"范围太大"的话，一个境外的大范围会得到一句误导性的话（实测巴黎那次就是）。
	if south < 3 || north > 54 || west < 73 || east > 136 {
		return "", fmt.Errorf("只支持中国境内的范围")
	}
	if north-south > osmMaxSpanDeg || east-west > osmMaxSpanDeg {
		return "", fmt.Errorf("范围太大（最多 %.2f 度，约 6 公里）—— 缩小一点再导", osmMaxSpanDeg)
	}
	return fmt.Sprintf("%.6f,%.6f,%.6f,%.6f", south, west, north, east), nil
}

// handleOsmBuildings 拉某一片的建筑轮廓（**公开接口**）。
//
// 返回的是 Overpass 的原始响应（`{elements:[...]}`），前端自己解析。
// 这样上游加字段我们不用跟着改 —— 和 navpack 那条"服务端原样存发"同一个道理。
func (s *Server) handleOsmBuildings(w http.ResponseWriter, r *http.Request) {
	bbox, err := osmAllowedBBox(r.URL.Query())
	if err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	dataDir := s.cfg.DataDir

	// ---- ① 内存缓存
	osmLedMu.Lock()
	if e, ok := osmLedger[bbox]; ok && time.Since(e.at) < osmDiskTTL {
		osmLedMu.Unlock()
		osmRespond(w, e.body, "mem")
		return
	}
	osmLedMu.Unlock()

	// ---- ② 磁盘缓存（**重启也不丢** —— 这是"合法下载下来缓存"的落点）
	if body, ok := osmDiskRead(dataDir, bbox); ok {
		osmLedMu.Lock()
		osmLedger[bbox] = osmMemEntry{body: body, at: time.Now()}
		osmLedMu.Unlock()
		osmRespond(w, body, "disk")
		return
	}

	// ---- ③ 熔断中就直接回，不去等那 100 秒
	//
	// ⚠️ 这一段排在缓存**之后**：熔断只挡"去上游"，不该挡"读缓存"。
	//    顺序反了的话，熔断期间连已经拉回来的数据都取不到 ——
	//    而那是最不该发生的（数据明明就在磁盘上）。
	if left := osmIsDown(); left > 0 {
		apiErr(w, 503, "暂时连不上 OpenStreetMap（刚连续试过 "+
			strconv.Itoa(osmFailStreakLimit)+" 次都不通，冷却 "+
			strconv.Itoa(int(left.Minutes())+1)+" 分钟）—— 这一带还没烘焙过。"+
			"稳妥的办法是在本机跑一次 `node scripts/osm-bake.mjs` 把这片存下来，之后是瞬时的。")
		return
	}

	// ---- ④ 去上游拉
	query := fmt.Sprintf("[out:json][timeout:90];\nway[\"building\"](%s);\nout geom tags;", bbox)

	// 并行赛跑所有镜像：谁先成功用谁
	//
	// ⚠️ 结果必须带**是哪个镜像赢的** —— 出了问题（比如某个镜像开始拒服务）
	//    没有这个信息就只能一个个试，而那要几分钟。
	type result struct {
		ep   string
		body string
		err  error
	}
	eps := strings.Split(osmEndpoints, "|")
	ch := make(chan result, len(eps))
	for _, ep := range eps {
		go func(endpoint string) {
			body, err := osmFetch(endpoint, query)
			ch <- result{ep: endpoint, body: body, err: err}
		}(ep)
	}

	var failures []string
	for i := 0; i < len(eps); i++ {
		r := <-ch
		if r.err == nil {
			// 两层缓存都写上
			osmLedMu.Lock()
			// ⚠️ 上限 200 条：这是个**长期运行的进程**，map 不封顶就是内存泄漏。
			//    超过就整表清掉 —— 磁盘那份还在，重建代价只是读一次文件。
			if len(osmLedger) >= 200 {
				osmLedger = map[string]osmMemEntry{}
			}
			osmLedger[bbox] = osmMemEntry{body: r.body, at: time.Now()}
			osmLedMu.Unlock()
			osmDiskWrite(dataDir, bbox, r.body)
			// ★ 通了就复位熔断 —— 网络恢复了不该还断着
			osmMarkOk()

			log.Printf("osm: %s ← %s (%d 字节)", bbox, r.ep, len(r.body))
			osmRespond(w, r.body, "upstream")
			return
		}
		// ⚠️ **全部失败都要留着**，不能只留最后一个。
		//    只留最后一个的话，错误里出现的永远是"最后返回的那个镜像"，
		//    而"四个都挂了"和"只有那一个挂了"的修法完全不同 ——
		//    实测第一次排查就是被这个坑到了（只看到 kumi.systems，
		//    以为换个镜像就行，其实四个是同一个原因）。
		failures = append(failures, osmShortHost(r.ep)+"："+errText(r.err))
	}

	// ★ 全挂 → 记一笔（够次数就熔断，下一次是毫秒级失败而不是又等 100 秒）
	osmMarkFail()

	apiErr(w, 502, "拉取 OpenStreetMap 失败 —— "+strings.Join(failures, "；")+
		"。Overpass 是免费公共服务器，忙的时候会超时；过一会儿再试。"+
		"如果这一带反复拉不动，在本机跑一次 `node scripts/osm-bake.mjs` 把它烘焙下来更稳妥。")
}

// osmShortHost 把镜像 URL 缩成主机名，好让错误信息一行放得下。
func osmShortHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}

// osmRespond 统一回一份缓存/上游的 JSON。
//
// `X-Osm-Cache` 有三档：`mem` / `disk` / `upstream` —— 排查"到底走没走缓存"时
// 一眼就能看出来（只看耗时是猜的）。
func osmRespond(w http.ResponseWriter, body, from string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Osm-Cache", from)
	_, _ = io.WriteString(w, body)
}

// osmFetch 打一个 Overpass 实例，返回**原始响应体**。
func osmFetch(endpoint, query string) (string, error) {
	form := url.Values{}
	form.Set("data", query)

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// ⚠️ 带一个**能说明来路的** UA：Overpass 会因为"没有意义的 UA"拒服务，
	//    而匿名 UA 也会让运维在排查时找不到是谁在刷。见 weather.go 里同一个教训。
	req.Header.Set("User-Agent", "schedule-server-nav/1.0 (campus building annotation; ODbL data)")

	client := &http.Client{
		Timeout: osmUpstreamTimeout,
		Transport: &http.Transport{
			// 连接阶段单独限时 —— 见 [osmDialTimeout] 那段注释：
			// 目的不是快，是让"连不上"和"对方不回"在日志里长得不一样。
			DialContext:           (&net.Dialer{Timeout: osmDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   osmDialTimeout,
			ResponseHeaderTimeout: osmHeaderTimeout,
			// 复用连接：同一片区域重查（比如换个 bbox 边缘）能省一次握手。
			MaxIdleConns:        8,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	// ⚠️ 读之前先限流：上游理论上会回一个几 MB 的 JSON，
	//    不设上限的话一个恶意 bbox 就能把内存吃满（虽然 bbox 已经限了大小）。
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d：%s", res.StatusCode, strings.TrimSpace(string(body)))
	}
	// 确认它至少是 JSON（Overpass 出错时会回 HTML）
	if !json.Valid(body) {
		return "", fmt.Errorf("上游返回的不是 JSON")
	}
	return string(body), nil
}

// errText 把可能为 nil 的 error 变成一句能显示的话。
func errText(err error) string {
	if err == nil {
		return "未知原因"
	}
	return err.Error()
}

package app

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// 腾讯**地点搜索**的服务端代理（2026-10-13）。
//
// # 为什么必须有这一层
//
// 标注台的"二次识别地名"要用**地图自己的地名**（腾讯 POI），而不是 OSM 那种
// 公开数据。而实测（`tmp/probe-poi.cjs` + curl）：
//
//	curl -D- -H "Origin: https://example.com:8444" \
//	     "https://apis.map.qq.com/ws/place/v1/search?boundary=nearby(21.1493,110.3014,300)&key=…"
//	→ HTTP 200，status=0，返回 72 条真实 POI（"广东海洋大学教学主楼(广学楼)"…）
//	  **但响应里一个 Access-Control-* 都没有**
//
// 于是浏览器里 `fetch` 直接 `Failed to fetch`。标注台是纯前端，
// 所以这条路**只能**由服务端代拿。
//
// # 口径（别把它做成开放代理）
//
//   - 只代理**地点搜索**这一个接口，参数固定成"周边搜索"
//   - 只接受**中国境内**的坐标，半径 30~2000 米
//   - 两层缓存（内存 + 磁盘）：POI 名字几乎不变，一栋楼只该查一次；
//     一次批量识别 150 栋楼 = 150 次上游请求，没缓存就是白烧用户的配额
//   - 轻熔断：连续 3 次全挂 → 冷却 60 秒。上游不通时批量识别不要逐栋干等
//
// ⚠️ **Key 由前端带过来**（用户自己那把），服务端不存、也不写日志 ——
// 那是用户的东西，而日志会落在磁盘上。
//
// ⚠️ 坐标系：进来的 lat/lng 和回去的 location 都是 **GCJ-02**（腾讯的口径），
// 和标注台 / 标注包一致。**不要在这里做 WGS 换算** —— 换一次就整体偏 500 米，
// 而且症状是"名字对、位置不对"，很难查。
const (
	poiUpstream     = "https://apis.map.qq.com/ws/place/v1/search"
	poiDialTimeout  = 4 * time.Second
	poiTotalTimeout = 9 * time.Second
	poiDiskTTL      = 30 * 24 * time.Hour
	poiFailLimit    = 3
	poiDownCooldown = 60 * time.Second
)

type poiEntry struct {
	body string
	at   time.Time
}

var (
	poiMu     sync.Mutex
	poiLedger = map[string]poiEntry{}

	poiFailMu   sync.Mutex
	poiFailN    int
	poiDownTill time.Time
)

// poiParams 校验并归一化参数。
//
// ⚠️ 半径上限 2000 米不是随手定的：再大就变成"一次请求拿半个城市的 POI"，
// 那既是配额问题，也让"哪一栋对应哪个名字"变得没法判（相邻楼的 POI 会互相串）。
func poiParams(q url.Values) (lat, lng float64, rad int, err error) {
	// ⚠️ 用 `strconv` 而不是自己写一个"容错解析" ——
	//    `lat=abc` 这种必须**明确报错**，静默当成 0 会变成
	//    "拿几内亚湾的坐标去查 POI"（然后什么也查不到，还以为校区没数据）。
	lat, err = strconv.ParseFloat(strings.TrimSpace(q.Get("lat")), 64)
	if err != nil || math.IsNaN(lat) || math.IsInf(lat, 0) {
		return 0, 0, 0, fmt.Errorf("lat 不合法")
	}
	lng, err = strconv.ParseFloat(strings.TrimSpace(q.Get("lng")), 64)
	if err != nil || math.IsNaN(lng) || math.IsInf(lng, 0) {
		return 0, 0, 0, fmt.Errorf("lng 不合法")
	}
	if lat < 3 || lat > 54 || lng < 73 || lng > 136 {
		return 0, 0, 0, fmt.Errorf("只支持中国境内的坐标（这是校园标注用的，不打算当公共代理）")
	}
	rad = 150
	if raw := strings.TrimSpace(q.Get("r")); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil {
			return 0, 0, 0, fmt.Errorf("r 不合法")
		}
		rad = n
	}
	if rad < 30 || rad > 2000 {
		return 0, 0, 0, fmt.Errorf("半径要在 30~2000 米之间")
	}
	return lat, lng, rad, nil
}

func poiCacheKey(lat, lng float64, rad int) string {
	// ⚠️ 坐标**取到 5 位小数**（约 1 米）再当键 —— 不取整的话，
	//    用户在地图上微调一下位置就是一个新键，缓存等于没有。
	return fmt.Sprintf("%.5f,%.5f,%d", lat, lng, rad)
}

func poiDiskFile(dataDir, key string) string {
	sum := sha1.Sum([]byte(key))
	return filepath.Join(dataDir, "poi", hex.EncodeToString(sum[:])+".json")
}

func poiDiskRead(dataDir, key string) (string, bool) {
	b, err := os.ReadFile(poiDiskFile(dataDir, key))
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}

func poiDiskWrite(dataDir, key, body string) {
	f := poiDiskFile(dataDir, key)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(f, []byte(body), 0o644)
}

func poiIsDown() time.Duration {
	poiFailMu.Lock()
	defer poiFailMu.Unlock()
	if time.Now().Before(poiDownTill) {
		return time.Until(poiDownTill)
	}
	return 0
}

func poiMarkFail() {
	poiFailMu.Lock()
	defer poiFailMu.Unlock()
	poiFailN++
	if poiFailN >= poiFailLimit {
		poiDownTill = time.Now().Add(poiDownCooldown)
	}
}

func poiMarkOk() {
	poiFailMu.Lock()
	defer poiFailMu.Unlock()
	poiFailN = 0
	poiDownTill = time.Time{}
}

// poiSearchResult 是我们要**回给前端**的那个形状。
//
// ⚠️ 只回我们需要的四个字段，不回腾讯的原始包 ——
// 前端不该知道上游长什么样（哪天换上游就不用动前端），
// 更不该拿到我们没打算公开的东西。
type poiItem struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Distance float64 `json:"distance"`
	Lat      float64 `json:"lat"`
	Lng      float64 `json:"lng"`
}

type poiResp struct {
	Items []poiItem `json:"items"`
	From  string    `json:"from"`
}

// qqPoiEnvelope 是腾讯那边的原始信封（只解我们用的字段）。
type qqPoiEnvelope struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
	Count   int    `json:"count"`
	Data    []struct {
		Title     string  `json:"title"`
		Category  string  `json:"category"`
		Distance  float64 `json:"_distance"`
		Location  struct {
			Lat float64 `json:"lat"`
			Lng float64 `json:"lng"`
		} `json:"location"`
	} `json:"data"`
}

func (s *Server) handleNavPoi(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		apiErr(w, 400, "缺 key —— 这个接口要用你自己的腾讯 Key（和地图那个是同一把）")
		return
	}
	lat, lng, rad, err := poiParams(r.URL.Query())
	if err != nil {
		apiErr(w, 400, err.Error())
		return
	}
	dataDir := s.cfg.DataDir
	cacheKey := poiCacheKey(lat, lng, rad)

	// ---- ① 内存
	poiMu.Lock()
	if e, ok := poiLedger[cacheKey]; ok && time.Since(e.at) < poiDiskTTL {
		poiMu.Unlock()
		poiRespond(w, e.body, "mem")
		return
	}
	poiMu.Unlock()

	// ---- ② 磁盘（重启也不丢）
	if body, ok := poiDiskRead(dataDir, cacheKey); ok {
		poiMu.Lock()
		poiLedger[cacheKey] = poiEntry{body: body, at: time.Now()}
		poiMu.Unlock()
		poiRespond(w, body, "disk")
		return
	}

	// ---- ③ 熔断
	if d := poiIsDown(); d > 0 {
		apiErr(w, 503, fmt.Sprintf(
			"地点搜索连续失败，已暂停 %d 秒（上游不通时不要逐栋干等）。过一会儿再试。",
			int(d.Seconds())+1))
		return
	}

	// ---- ④ 上游
	u := fmt.Sprintf("%s?boundary=nearby(%.6f,%.6f,%d)&page_size=20&key=%s",
		poiUpstream, lat, lng, rad, url.QueryEscape(key))
	body, err := poiFetch(u)
	if err != nil {
		poiMarkFail()
		apiErr(w, 502, "调腾讯地点搜索失败："+err.Error()+
			"。这是腾讯的接口，忙的时候会慢；过一会儿再试。")
		return
	}

	var env qqPoiEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		poiMarkFail()
		apiErr(w, 502, "腾讯地点搜索返回的不是预期结构（可能 Key 没勾 WebServiceAPI）")
		return
	}
	// ⚠️ **status != 0 是"业务失败"**（Key 不对 / 配额用完 / 参数不合法），
	//    这时 HTTP 可能还是 200 —— 只看 HTTP 状态码会把这类错误当成成功。
	if env.Status != 0 {
		if env.Status == 111 || env.Status == 112 || env.Status == 110 {
			// Key / 配额这类问题**不该熔断**：熔断是针对"上游不通"的，
			// 而 Key 不对时等 60 秒再试也没用，应该让用户立刻看到原因。
			apiErr(w, 400, fmt.Sprintf(
				"腾讯地点搜索拒绝了这次请求（status=%d）：%s。检查 Key 有没有勾上「WebServiceAPI」。",
				env.Status, env.Message))
			return
		}
		poiMarkFail()
		apiErr(w, 502, fmt.Sprintf("腾讯地点搜索报错（status=%d）：%s", env.Status, env.Message))
		return
	}

	items := make([]poiItem, 0, len(env.Data))
	for _, d := range env.Data {
		if strings.TrimSpace(d.Title) == "" {
			continue
		}
		items = append(items, poiItem{
			Name:     d.Title,
			Category: d.Category,
			Distance: d.Distance,
			Lat:      d.Location.Lat,
			Lng:      d.Location.Lng,
		})
	}
	out, _ := json.Marshal(poiResp{Items: items, From: "upstream"})

	poiMarkOk()
	poiMu.Lock()
	poiLedger[cacheKey] = poiEntry{body: string(out), at: time.Now()}
	poiMu.Unlock()
	poiDiskWrite(dataDir, cacheKey, string(out))

	poiRespond(w, string(out), "upstream")
}

func poiRespond(w http.ResponseWriter, body, from string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Poi-Cache", from)
	_, _ = io.WriteString(w, body)
}

func poiFetch(u string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "schedule-server-nav/1.0 (campus building annotation)")

	client := &http.Client{
		Timeout: poiTotalTimeout,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: poiDialTimeout}).DialContext,
			TLSHandshakeTimeout:   poiDialTimeout,
			ResponseHeaderTimeout: poiTotalTimeout,
		},
	}
	defer client.CloseIdleConnections()

	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return string(b), nil
}

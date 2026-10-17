package app

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Weather for one fixed place: 北京市.
//
// met.no (the Norwegian Meteorological Institute) is the primary source. Their
// terms require a User-Agent that actually identifies the caller and that you
// honour the Expires header instead of polling on a timer, so both are done
// here: one in-process cache plus a copy on disk so a server restart does not
// immediately re-hit them.
//
// If met.no is unreachable or rate-limits us, the last good answer is served
// (marked stale) and Open-Meteo is tried as a second opinion.

const (
	defaultWeatherPlace = "北京市"
	defaultWeatherLat   = 39.9042
	defaultWeatherLon   = 116.4074

	// A conservative floor. met.no asks for "at most 20 requests per second"
	// but their real expectation is that you cache; 20 minutes is plenty for an
	// hourly forecast and keeps us far away from any limit.
	minWeatherTTL = 20 * time.Minute
	maxWeatherTTL = 2 * time.Hour
)

type weatherHour struct {
	Time     int64   `json:"t"`
	Temp     float64 `json:"temp"`
	Humidity float64 `json:"humidity"`
	Wind     float64 `json:"wind"`
	WindDir  float64 `json:"wind_dir"`
	Cloud    float64 `json:"cloud"`
	Precip   float64 `json:"precip"`
	Symbol   string  `json:"symbol"`
	Label    string  `json:"label"`
	IsDay    bool    `json:"is_day"`
}

type weatherDay struct {
	Date      string  `json:"date"`
	TMin      float64 `json:"tmin"`
	TMax      float64 `json:"tmax"`
	PrecipSum float64 `json:"precip_sum"`
	Symbol    string  `json:"symbol"`
	Label     string  `json:"label"`
}

type weatherDoc struct {
	Place     string        `json:"place"`
	Lat       float64       `json:"lat"`
	Lon       float64       `json:"lon"`
	Source    string        `json:"source"`
	UpdatedAt int64         `json:"updated_at"`
	ExpiresAt int64         `json:"expires_at"`
	Hours     []weatherHour `json:"hours"`
	Days      []weatherDay  `json:"days"`
	Stale     bool          `json:"stale,omitempty"`
	Note      string        `json:"note,omitempty"`
}

type weatherCache struct {
	mu   sync.Mutex
	doc  *weatherDoc
	file string
}

func newWeatherCache(dataDir string) *weatherCache {
	c := &weatherCache{file: filepath.Join(dataDir, "weather-cache.json")}
	// Warm from disk so a restart keeps serving the previous forecast.
	if raw, err := os.ReadFile(c.file); err == nil {
		var d weatherDoc
		if json.Unmarshal(raw, &d) == nil && len(d.Hours) > 0 {
			c.doc = &d
			log.Printf("  weather     : %d cached hours from disk", len(d.Hours))
		}
	}
	return c
}

func (c *weatherCache) save(d *weatherDoc) {
	raw, err := json.Marshal(d)
	if err != nil {
		return
	}
	_ = os.WriteFile(c.file, raw, 0o644)
}

// ---------- symbol translation ----------
//
// met.no hands back ASCII codes such as "heavyrainshowers_day". The clients are
// Chinese, so translate once here rather than in two places.

var weatherLabels = map[string]string{
	"clearsky":                   "晴",
	"fair":                       "晴间多云",
	"partlycloudy":               "多云",
	"cloudy":                     "阴",
	"fog":                        "雾",
	"lightrain":                  "小雨",
	"rain":                       "雨",
	"heavyrain":                  "大雨",
	"lightrainshowers":           "小阵雨",
	"rainshowers":                "阵雨",
	"heavyrainshowers":           "强阵雨",
	"lightsleet":                 "小雨夹雪",
	"sleet":                      "雨夹雪",
	"heavysleet":                 "强雨夹雪",
	"lightsnow":                  "小雪",
	"snow":                       "雪",
	"heavysnow":                  "大雪",
	"lightsnowshowers":           "小阵雪",
	"snowshowers":                "阵雪",
	"heavysnowshowers":           "强阵雪",
	"rainandthunder":             "雨伴雷",
	"heavyrainandthunder":        "大雨伴雷",
	"lightrainandthunder":        "小雨伴雷",
	"rainshowersandthunder":      "阵雨伴雷",
	"heavyrainshowersandthunder": "强阵雨伴雷",
	"lightsleetandthunder":       "小雨夹雪伴雷",
	"sleetandthunder":            "雨夹雪伴雷",
	"heavysleetandthunder":       "强雨夹雪伴雷",
	"lightsnowandthunder":        "小雪伴雷",
	"snowandthunder":             "雪伴雷",
	"heavysnowandthunder":        "大雪伴雷",
}

func symbolLabel(code string) string {
	if code == "" {
		return ""
	}
	base := code
	for _, suffix := range []string{"_day", "_night", "_polartwilight"} {
		base = strings.TrimSuffix(base, suffix)
	}
	if label, ok := weatherLabels[base]; ok {
		return label
	}
	return base
}

// ---------- met.no ----------

type metResponse struct {
	Properties struct {
		Meta struct {
			UpdatedAt string `json:"updated_at"`
		} `json:"meta"`
		Timeseries []struct {
			Time string `json:"time"`
			Data struct {
				Instant struct {
					Details struct {
						AirTemperature        float64 `json:"air_temperature"`
						RelativeHumidity      float64 `json:"relative_humidity"`
						WindSpeed             float64 `json:"wind_speed"`
						WindFromDirection     float64 `json:"wind_from_direction"`
						CloudAreaFraction     float64 `json:"cloud_area_fraction"`
						UltravioletIndexClear float64 `json:"ultraviolet_index_clear_sky"`
					} `json:"details"`
				} `json:"instant"`
				Next1Hours *struct {
					Summary struct {
						SymbolCode string `json:"symbol_code"`
					} `json:"summary"`
					Details struct {
						PrecipitationAmount float64 `json:"precipitation_amount"`
					} `json:"details"`
				} `json:"next_1_hours"`
				Next6Hours *struct {
					Summary struct {
						SymbolCode string `json:"symbol_code"`
					} `json:"summary"`
					Details struct {
						PrecipitationAmount float64 `json:"precipitation_amount"`
					} `json:"details"`
				} `json:"next_6_hours"`
			} `json:"data"`
		} `json:"timeseries"`
	} `json:"properties"`
}

func fetchMetNo(lat, lon float64, userAgent string, client *http.Client) (*weatherDoc, time.Duration, error) {
	u := fmt.Sprintf(
		"https://api.met.no/weatherapi/locationforecast/2.0/compact?lat=%.4f&lon=%.4f",
		lat, lon,
	)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	// met.no rejects requests without a descriptive agent.
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNonAuthoritativeInfo {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, 0, fmt.Errorf("met.no http %d: %s", res.StatusCode, strings.TrimSpace(string(body)))
	}

	var mr metResponse
	if err := json.NewDecoder(res.Body).Decode(&mr); err != nil {
		return nil, 0, fmt.Errorf("met.no decode: %w", err)
	}

	ttl := minWeatherTTL
	if exp := res.Header.Get("Expires"); exp != "" {
		if t, err := http.ParseTime(exp); err == nil {
			if d := time.Until(t); d > minWeatherTTL {
				ttl = d
			}
		}
	}
	if ttl > maxWeatherTTL {
		ttl = maxWeatherTTL
	}

	hours := make([]weatherHour, 0, len(mr.Properties.Timeseries))
	for _, e := range mr.Properties.Timeseries {
		t, err := time.Parse(time.RFC3339, e.Time)
		if err != nil {
			continue
		}
		d := e.Data.Instant.Details
		code := ""
		precip := 0.0
		if e.Data.Next1Hours != nil {
			code = e.Data.Next1Hours.Summary.SymbolCode
			precip = e.Data.Next1Hours.Details.PrecipitationAmount
		} else if e.Data.Next6Hours != nil {
			// The last few days of the series only carry 6-hourly data.
			code = e.Data.Next6Hours.Summary.SymbolCode
			precip = e.Data.Next6Hours.Details.PrecipitationAmount / 6
		}
		hours = append(hours, weatherHour{
			Time:     t.Unix(),
			Temp:     round1(d.AirTemperature),
			Humidity: round1(d.RelativeHumidity),
			Wind:     round1(d.WindSpeed),
			WindDir:  round1(d.WindFromDirection),
			Cloud:    round1(d.CloudAreaFraction),
			Precip:   round1(precip),
			Symbol:   code,
			Label:    symbolLabel(code),
			IsDay:    !strings.HasSuffix(code, "_night"),
		})
	}
	if len(hours) == 0 {
		return nil, 0, fmt.Errorf("met.no returned no timeseries")
	}
	return &weatherDoc{
		Source:    "met.no",
		UpdatedAt: time.Now().Unix(),
		ExpiresAt: time.Now().Add(ttl).Unix(),
		Hours:     hours,
	}, ttl, nil
}

// ---------- Open-Meteo (fallback) ----------

type omResponse struct {
	Hourly struct {
		Time     []string  `json:"time"`
		Temp     []float64 `json:"temperature_2m"`
		Humidity []float64 `json:"relative_humidity_2m"`
		Precip   []float64 `json:"precipitation"`
		Code     []float64 `json:"weather_code"`
		Cloud    []float64 `json:"cloud_cover"`
		Wind     []float64 `json:"wind_speed_10m"`
		WindDir  []float64 `json:"wind_direction_10m"`
		IsDay    []float64 `json:"is_day"`
	} `json:"hourly"`
}

// WMO weather codes, as used by Open-Meteo.
var wmoLabels = map[int]string{
	0: "晴", 1: "晴间多云", 2: "多云", 3: "阴", 45: "雾", 48: "雾凇",
	51: "毛毛雨", 53: "毛毛雨", 55: "毛毛雨",
	56: "冻毛毛雨", 57: "冻毛毛雨",
	61: "小雨", 63: "雨", 65: "大雨",
	66: "冻雨", 67: "冻雨",
	71: "小雪", 73: "雪", 75: "大雪", 77: "米雪",
	80: "小阵雨", 81: "阵雨", 82: "强阵雨",
	85: "小阵雪", 86: "阵雪",
	95: "雷阵雨", 96: "雷阵雨伴冰雹", 99: "强雷阵雨伴冰雹",
}

func fetchOpenMeteo(lat, lon float64, userAgent string, client *http.Client) (*weatherDoc, error) {
	u := fmt.Sprintf("https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f"+
		"&hourly=temperature_2m,relative_humidity_2m,precipitation,weather_code,cloud_cover,"+
		"wind_speed_10m,wind_direction_10m,is_day&forecast_days=7&timezone=UTC",
		lat, lon)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("open-meteo http %d", res.StatusCode)
	}
	var or omResponse
	if err := json.NewDecoder(res.Body).Decode(&or); err != nil {
		return nil, err
	}
	n := len(or.Hourly.Time)
	if n == 0 {
		return nil, fmt.Errorf("open-meteo returned no hours")
	}
	hours := make([]weatherHour, 0, n)
	for i := 0; i < n; i++ {
		t, err := time.Parse("2006-01-02T15:04", or.Hourly.Time[i])
		if err != nil {
			continue
		}
		code := 0
		if i < len(or.Hourly.Code) {
			code = int(or.Hourly.Code[i])
		}
		pick := func(s []float64) float64 {
			if i < len(s) {
				return s[i]
			}
			return 0
		}
		isDay := pick(or.Hourly.IsDay) > 0.5
		hours = append(hours, weatherHour{
			Time:     t.UTC().Unix(),
			Temp:     round1(pick(or.Hourly.Temp)),
			Humidity: round1(pick(or.Hourly.Humidity)),
			Wind:     round1(pick(or.Hourly.Wind)),
			WindDir:  round1(pick(or.Hourly.WindDir)),
			Cloud:    round1(pick(or.Hourly.Cloud)),
			Precip:   round1(pick(or.Hourly.Precip)),
			Symbol:   "wmo" + strconv.Itoa(code),
			Label:    wmoLabel(code),
			IsDay:    isDay,
		})
	}
	return &weatherDoc{
		Source:    "open-meteo",
		UpdatedAt: time.Now().Unix(),
		ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
		Hours:     hours,
	}, nil
}

func wmoLabel(code int) string {
	if l, ok := wmoLabels[code]; ok {
		return l
	}
	return "未知"
}

// ---------- assembly ----------

// clockLocation is the timezone the forecast is grouped by. Zhanjiang has no
// daylight saving, but grouping by the server's local zone would be wrong on a
// server running in UTC.
// weatherCoordinates 返回当前该用哪个坐标取天气。
//
// ## 优先级：应用内配置 > 启动参数
//
// 坐标原来**只能**通过启动参数（`-lat` / `-lon`）指定，改一次要重启服务端。
// 现在允许用户在 App 里直接改城市，所以顺序是：
//
//  1. `app_settings` 里的 `weather_lat` / `weather_lon`（用户在 App 里设的）；
//  2. stored 值缺失或非法 → 回落启动参数（也就是老行为，部署脚本不受影响）。
//
// ⚠️ 为什么要连**范围校验**一起做：经纬度写错（比如把 116.4 填到纬度上）
// 会让 met.no 返回完全不相干地区的天气，而界面上看不出任何异常 ——
// 用户只会觉得"这天气怎么这么不准"。所以超出范围的值得当"没设"处理。
func (s *Server) weatherCoordinates() (float64, float64) {
	lat, lon := s.cfg.WeatherLat, s.cfg.WeatherLon

	if v := strings.TrimSpace(s.setting("weather_lat", "")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= -90 && f <= 90 {
			lat = f
		}
	}
	if v := strings.TrimSpace(s.setting("weather_lon", "")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= -180 && f <= 180 {
			lon = f
		}
	}
	return lat, lon
}

// weatherPlaceName 返回用户给这个坐标起的名字（显示用，可以空）。
func (s *Server) weatherPlaceName() string {
	if v := strings.TrimSpace(s.setting("weather_place", "")); v != "" {
		return v
	}
	return s.cfg.WeatherPlace
}

func weatherZone() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}

func groupDays(hours []weatherHour, loc *time.Location) []weatherDay {
	type acc struct {
		tmin, tmax, precip float64
		symbol             string
		seen               bool
	}
	byDay := map[string]*acc{}
	order := []string{}
	for _, h := range hours {
		key := time.Unix(h.Time, 0).In(loc).Format("2006-01-02")
		a, ok := byDay[key]
		if !ok {
			a = &acc{tmin: h.Temp, tmax: h.Temp}
			byDay[key] = a
			order = append(order, key)
		}
		if !a.seen {
			a.tmin, a.tmax = h.Temp, h.Temp
			a.seen = true
		}
		if h.Temp < a.tmin {
			a.tmin = h.Temp
		}
		if h.Temp > a.tmax {
			a.tmax = h.Temp
		}
		a.precip += h.Precip
		// Prefer the daytime symbol so a "clear" night does not override the
		// day's rain.
		if a.symbol == "" || (h.IsDay && h.Label != "") {
			if h.IsDay || a.symbol == "" {
				a.symbol = h.Symbol
			}
		}
	}
	out := make([]weatherDay, 0, len(order))
	for _, key := range order {
		a := byDay[key]
		out = append(out, weatherDay{
			Date:      key,
			TMin:      round1(a.tmin),
			TMax:      round1(a.tmax),
			PrecipSum: round1(a.precip),
			Symbol:    a.symbol,
			Label:     symbolLabel(a.symbol),
		})
	}
	return out
}

func round1(f float64) float64 {
	return float64(int(f*10+sign(f)*0.5)) / 10
}

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}

// Weather returns the current forecast, refreshing from upstream when the
// cached copy has expired. Never returns an error to the caller: a stale
// forecast beats a broken screen, and the clients can show Stale/Note.
func (s *Server) Weather(hours int, days int) *weatherDoc {
	c := s.weather
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if c.doc != nil && now.Unix() < c.doc.ExpiresAt {
		return trimWeather(c.doc, hours, days, false, "")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	lat, lon := s.weatherCoordinates()
	doc, ttl, err := fetchMetNo(lat, lon, s.cfg.WeatherUA, client)
	if err != nil {
		log.Printf("weather: met.no failed: %v", err)
		if om, omErr := fetchOpenMeteo(s.cfg.WeatherLat, s.cfg.WeatherLon, s.cfg.WeatherUA, client); omErr == nil {
			s.fillWeatherMeta(om)
			c.doc = om
			c.save(om)
			return trimWeather(om, hours, days, false, "met.no 暂不可用，本数据来自 Open-Meteo")
		} else {
			log.Printf("weather: open-meteo failed: %v", omErr)
		}
		if c.doc != nil {
			// Serve the last good copy rather than nothing.
			back := time.Now().Add(minWeatherTTL).Unix()
			c.doc.ExpiresAt = back
			return trimWeather(c.doc, hours, days, true, "上游暂时不可用，显示的是上一次获取的预报")
		}
		return &weatherDoc{
			Place: s.cfg.WeatherPlace, Lat: s.cfg.WeatherLat, Lon: s.cfg.WeatherLon,
			Source: "none", UpdatedAt: now.Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
			Hours: []weatherHour{}, Days: []weatherDay{}, Note: "暂时取不到天气预报：" + err.Error(),
		}
	}
	_ = ttl
	s.fillWeatherMeta(doc)
	c.doc = doc
	c.save(doc)
	return trimWeather(doc, hours, days, false, "")
}

func (s *Server) fillWeatherMeta(d *weatherDoc) {
	d.Place = s.cfg.WeatherPlace
	d.Lat = s.cfg.WeatherLat
	d.Lon = s.cfg.WeatherLon
	loc := weatherZone()
	// Drop hours already in the past so "now" is the first entry.
	cut := time.Now().Add(-30 * time.Minute).Unix()
	kept := d.Hours[:0]
	for _, h := range d.Hours {
		if h.Time >= cut {
			kept = append(kept, h)
		}
	}
	d.Hours = kept
	d.Days = groupDays(d.Hours, loc)
}

func trimWeather(d *weatherDoc, hours, days int, stale bool, note string) *weatherDoc {
	out := *d
	out.Stale = stale
	if note != "" {
		out.Note = note
	}
	if hours <= 0 {
		hours = 48
	}
	if hours > len(out.Hours) {
		hours = len(out.Hours)
	}
	out.Hours = out.Hours[:hours]
	if days <= 0 {
		days = 7
	}
	if days > len(out.Days) {
		days = len(out.Days)
	}
	out.Days = out.Days[:days]
	return &out
}

func (s *Server) handleWeather(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	writeJSON(w, 200, s.Weather(hours, days))
}

// weatherProbe is used by the diagnostics endpoint to prove the upstream call
// works without waiting for the cache to expire.
func (s *Server) weatherProbe() map[string]any {
	client := &http.Client{Timeout: 15 * time.Second}
	lat, lon := s.weatherCoordinates()
	doc, _, err := fetchMetNo(lat, lon, s.cfg.WeatherUA, client)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	first := doc.Hours[0]
	return map[string]any{
		"ok": true, "hours": len(doc.Hours), "source": doc.Source,
		"first_time": time.Unix(first.Time, 0).In(weatherZone()).Format("2006-01-02 15:04"),
		"first_temp": first.Temp, "first_label": first.Label,
	}
}

// handleWeatherDiag deliberately bypasses the cache so a broken upstream is
// visible immediately instead of after the TTL expires.
func (s *Server) handleWeatherDiag(w http.ResponseWriter, r *http.Request) {
	client := &http.Client{Timeout: 15 * time.Second}
	lat, lon := s.weatherCoordinates()
	met, _, metErr := fetchMetNo(lat, lon, s.cfg.WeatherUA, client)
	om, omErr := fetchOpenMeteo(s.cfg.WeatherLat, s.cfg.WeatherLon, s.cfg.WeatherUA, client)
	out := map[string]any{
		"place": s.cfg.WeatherPlace, "lat": s.cfg.WeatherLat, "lon": s.cfg.WeatherLon,
		"user_agent": s.cfg.WeatherUA,
	}
	if metErr != nil {
		out["met_no"] = map[string]any{"ok": false, "error": metErr.Error()}
	} else {
		out["met_no"] = map[string]any{
			"ok": true, "hours": len(met.Hours),
			"first": time.Unix(met.Hours[0].Time, 0).In(weatherZone()).Format("2006-01-02 15:04"),
			"temp":  met.Hours[0].Temp, "label": met.Hours[0].Label,
		}
	}
	if omErr != nil {
		out["open_meteo"] = map[string]any{"ok": false, "error": omErr.Error()}
	} else {
		out["open_meteo"] = map[string]any{
			"ok": true, "hours": len(om.Hours),
			"first": time.Unix(om.Hours[0].Time, 0).In(weatherZone()).Format("2006-01-02 15:04"),
			"temp":  om.Hours[0].Temp, "label": om.Hours[0].Label,
		}
	}
	c := s.weather
	c.mu.Lock()
	if c.doc != nil {
		out["cache"] = map[string]any{
			"source": c.doc.Source, "hours": len(c.doc.Hours),
			"updated_at": c.doc.UpdatedAt, "expires_at": c.doc.ExpiresAt,
			"age_sec": time.Now().Unix() - c.doc.UpdatedAt,
		}
	}
	c.mu.Unlock()
	writeJSON(w, 200, out)
}

var _ = url.Values{}

// ---------------------------------------------------------------------------
// 天气城市设置
// ---------------------------------------------------------------------------
//
// 坐标原来只能靠启动参数（`-lat` / `-lon`）改，也就是说**每换一个城市都要
// 重启服务端、还要去改部署脚本**。对"给家里老人看本地天气"这种用途来说太重了。
//
// 所以开一个应用内可配的入口：写进 `app_settings`，取天气时优先用它
// （见 weatherCoordinates）。启动参数退化成"没设过时的默认值"。
//
// 只让**管理员**改：天气是全家人共用的一个值，谁都能改会变成互相打架。

type weatherLocationReq struct {
	Lat   *float64 `json:"lat"`
	Lon   *float64 `json:"lon"`
	Place *string  `json:"place"`
}

// handleGetWeatherLocation 返回当前生效的坐标，以及它是从哪来的。
func (s *Server) handleGetWeatherLocation(w http.ResponseWriter, r *http.Request) {
	lat, lon := s.weatherCoordinates()
	stored := s.setting("weather_lat", "") != "" && s.setting("weather_lon", "") != ""
	source := "启动参数"
	if stored {
		source = "应用内设置"
	}
	writeJSON(w, 200, map[string]any{
		"lat": lat, "lon": lon,
		"place":  s.weatherPlaceName(),
		"source": source,
	})
}

// handleSetWeatherLocation 改坐标。
//
// 用 `*float64` 接：只改名字（不动坐标）是合理需求，指针能区分"没传"和"传了 0"
// —— 经度 0 和纬度 0 都是合法值（格林尼治、赤道），不能拿 0 当"没传"。
func (s *Server) handleSetWeatherLocation(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(userIDOf(r)) {
		apiErr(w, 403, "只有管理员能改天气城市")
		return
	}

	var req weatherLocationReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, 400, "请求格式不对")
		return
	}

	if req.Lat != nil {
		if *req.Lat < -90 || *req.Lat > 90 {
			apiErr(w, 400, "纬度要在 -90 ~ 90 之间")
			return
		}
		if err := s.setSetting("weather_lat", strconv.FormatFloat(*req.Lat, 'f', 4, 64)); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	if req.Lon != nil {
		if *req.Lon < -180 || *req.Lon > 180 {
			apiErr(w, 400, "经度要在 -180 ~ 180 之间")
			return
		}
		if err := s.setSetting("weather_lon", strconv.FormatFloat(*req.Lon, 'f', 4, 64)); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}
	if req.Place != nil {
		if err := s.setSetting("weather_place", strings.TrimSpace(*req.Place)); err != nil {
			apiErr(w, 500, err.Error())
			return
		}
	}

	lat, lon := s.weatherCoordinates()
	writeJSON(w, 200, map[string]any{
		"lat": lat, "lon": lon, "place": s.weatherPlaceName(), "source": "应用内设置",
	})
}

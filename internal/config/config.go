// Package config 定义服务端与本地管理面板共用的配置。
//
// 配置文件默认放在 exe 同目录的 config.json。相对路径（data / certs）
// 一律相对 **exe 所在目录** 解析，而不是当前工作目录 —— 双击启动时
// 工作目录是资源管理器给的，计划任务里又是另一个，靠 cwd 会到处写数据。
//
// data_dir / cert_dir 留空表示自动：新装直接落到程序目录之外
// （Windows 是 %ProgramData%\3e-schedule-server），老部署原地不动。
// 更新时替换 exe 不会碰数据。
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Config 是所有可持久化的运行参数。
//
// 字段分三组：监听与证书、业务配置、本地面板。面板保存时会校验监听地址，
// 避免写进一个空地址导致下次启动直接失败。
type Config struct {
	// Addr 是主服务的 HTTPS 监听地址，例如 ":8443"。留空表示不启用 HTTPS。
	Addr string `json:"addr"`
	// HTTPAddr 是额外的明文 HTTP 监听地址，例如 ":8080"。
	// 生产环境建议留空，由反向代理终止 TLS。
	HTTPAddr string `json:"http_addr"`
	// WordbookAddr 是背词同步的监听地址，例如 ":8446"。
	// 留空表示关闭这条通道（它需要客户端侧配合，多数自部署用不到）。
	WordbookAddr string `json:"wordbook_addr"`
	// InternalAddr 是仅供本机管理面板调用的明文回环地址。
	// 默认 127.0.0.1:0（系统分配端口），实际端口写入 data/internal.addr。
	InternalAddr string `json:"internal_addr"`

	// CertDir 存放 server.crt / server.key / ca.crt。
	CertDir string `json:"cert_dir"`
	// DataDir 存放 schedule.db、live.db、日志与备份。
	DataDir string `json:"data_dir"`
	// ServerName 是证书要签的域名。留空时证书只包含 localhost 与本机 IP。
	ServerName string `json:"server_name"`
	// AutoCert 为 true 时，证书缺失会自动生成一张自签证书（含本地 CA）。
	AutoCert bool `json:"auto_cert"`

	// InviteCode 是注册邀请码。留空则首次启动生成一个并落库。
	InviteCode string `json:"invite_code"`

	// Weather 是下发给客户端的默认天气地点。
	WeatherPlace string  `json:"weather_place"`
	WeatherLat   float64 `json:"weather_lat"`
	WeatherLon   float64 `json:"weather_lon"`
	WeatherUA    string  `json:"weather_ua"`

	// LiveSecret 是实时协作票据的 HMAC 密钥。留空则自动生成并落库。
	LiveSecret string `json:"live_secret"`

	// Diag 打开 /debug/pprof。排障时临时开启。
	Diag bool `json:"diag"`

	// COSBase 是更新包的对象存储前缀。留空则从本机下载。
	COSBase string `json:"cos_base"`
	// MarketURL 是网页版插件市场地址，由服务端下发给客户端。留空则隐藏入口。
	MarketURL string `json:"market_url"`

	// PanelListen 是本地管理面板的监听地址。
	PanelListen string `json:"panel_listen"`
	// PanelAutoOpen 为 true 时，管理器启动后自动打开默认浏览器。
	PanelAutoOpen bool `json:"panel_auto_open"`
	// AutoRestart 为 true 时，服务进程意外退出会被自动拉起。
	AutoRestart bool `json:"auto_restart"`
}

// Defaults 返回一套可以直接启动的默认配置。
func Defaults() Config {
	return Config{
		Addr:         ":8443",
		HTTPAddr:     "",
		WordbookAddr: "",
		InternalAddr: "127.0.0.1:0",
		CertDir:      "",
		DataDir:      "",
		ServerName:   "",
		AutoCert:     true,
		InviteCode:   "",
		WeatherPlace: "北京市",
		WeatherLat:   39.9042,
		WeatherLon:   116.4074,
		WeatherUA:    "schedule-server (self-hosted)",
		LiveSecret:   "",
		Diag:         false,
		COSBase:      "",
		MarketURL:    "",
		PanelListen:  "127.0.0.1:9800",
		PanelAutoOpen: true,
		AutoRestart:  true,
	}
}

// Load 读取配置文件。文件不存在时返回默认配置且不报错，
// 由调用方决定是否写回（首次启动时写一份，方便用户编辑）。
func Load(path string) (Config, error) {
	c := Defaults()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	// 未知字段不报错：旧版本写下的配置应该还能被新版本读。
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	c.normalize()
	return c, nil
}

// Save 原子写入配置文件（先写临时文件再改名），权限 0600：
// 里面可能有实时协作密钥。
func Save(path string, c Config) error {
	c.normalize()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) normalize() {
	c.Addr = strings.TrimSpace(c.Addr)
	c.HTTPAddr = strings.TrimSpace(c.HTTPAddr)
	c.WordbookAddr = strings.TrimSpace(c.WordbookAddr)
	c.InternalAddr = strings.TrimSpace(c.InternalAddr)
	if c.InternalAddr == "" {
		c.InternalAddr = "127.0.0.1:0"
	}
	// data_dir / cert_dir 留空表示"自动"：解析时落到程序目录之外，
	// 老部署（程序目录里已经有库或证书）仍然原地使用。见 Resolve。
	c.PanelListen = strings.TrimSpace(c.PanelListen)
	if c.PanelListen == "" {
		c.PanelListen = "127.0.0.1:9800"
	}
	if c.WeatherPlace == "" {
		c.WeatherPlace = "北京市"
	}
}

// Resolve 返回一份把目录解析成绝对路径的副本。
// baseDir 应当是 exe 所在目录。
//
// 目录留空时的规则：
//
//	程序目录里已经有 schedule.db / server.crt → 就用程序目录里这份。
//	                                          老部署的数据不能背着用户搬走。
//	都没有                                     → 落到 DataRoot(baseDir)，在程序目录之外。
//
// 这样"替换 exe"不会碰到数据，而老机器升级后照样读得到原来的库。
func (c Config) Resolve(baseDir string) Config {
	dataInProgram := fileExists(filepath.Join(baseDir, "data", "schedule.db"))
	certInProgram := fileExists(filepath.Join(baseDir, "certs", "server.crt"))

	if c.DataDir == "" {
		if dataInProgram {
			c.DataDir = filepath.Join(baseDir, "data")
		} else {
			c.DataDir = filepath.Join(DataRoot(baseDir), "data")
		}
	} else if !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(baseDir, c.DataDir)
	}

	if c.CertDir == "" {
		if dataInProgram || certInProgram {
			c.CertDir = filepath.Join(baseDir, "certs")
		} else {
			c.CertDir = filepath.Join(DataRoot(baseDir), "certs")
		}
	} else if !filepath.IsAbs(c.CertDir) {
		c.CertDir = filepath.Join(baseDir, c.CertDir)
	}

	c.DataDir = filepath.Clean(c.DataDir)
	c.CertDir = filepath.Clean(c.CertDir)
	return c
}

// DataRoot 返回这台机器上放数据的根目录，在程序目录之外。
//
//	Windows  %ProgramData%\3e-schedule-server
//	其他     $XDG_DATA_HOME/3e-schedule-server（没有就用 ~/.local/share/...）
//
// 环境变量 SCHEDULE_SERVER_ROOT 可以覆盖，装在两台机器上、或者测试时
// 同时跑几个实例要靠它区分。
func DataRoot(baseDir string) string {
	if v := strings.TrimSpace(os.Getenv("SCHEDULE_SERVER_ROOT")); v != "" {
		return filepath.Clean(v)
	}
	if runtime.GOOS == "windows" {
		if v := strings.TrimSpace(os.Getenv("ProgramData")); v != "" {
			return filepath.Join(v, "3e-schedule-server")
		}
		if v := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); v != "" {
			return filepath.Join(v, "3e-schedule-server")
		}
	} else {
		if v := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); v != "" {
			return filepath.Join(v, "3e-schedule-server")
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, ".local", "share", "3e-schedule-server")
		}
	}
	// 什么都拿不到：退回程序目录。这是最后手段 —— 至少服务能起来，
	// 面板首页会提示数据还在程序目录里。
	return filepath.Join(baseDir, ".3e-schedule-server")
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// Validate 在保存前做最小校验，返回的错误可以直接展示给用户。
func (c Config) Validate() error {
	if c.Addr == "" && c.HTTPAddr == "" {
		return errors.New("至少要启用一个监听地址：HTTPS 或 HTTP")
	}
	if c.PanelListen == "" {
		return errors.New("管理面板监听地址不能为空")
	}
	if c.WeatherLat < -90 || c.WeatherLat > 90 || c.WeatherLon < -180 || c.WeatherLon > 180 {
		return errors.New("天气坐标超出有效范围")
	}
	return nil
}

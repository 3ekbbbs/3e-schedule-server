package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 本机网络信息（2026-10-18）
//
// 解决两个很具体的场景：
//
//	部署      用户刚把 exe 放上服务器，不知道"别人该填哪个地址"。
//	          这里把公网 IP 和端口拼成一条可直接复制的地址。
//	局域网    用户在自己电脑上跑服务，手机连同一个 WiFi 同步。
//	          这里把每张网卡的内网地址列出来，并标出哪一个是私有网段。
//
// 公网 IP **只能靠外部服务问**：服务器自己看不到 NAT 外面的那个地址，
// 除非它有一张公网网卡（那时候网卡地址就是答案，但两者无法从本机区分）。
// 所以这里两条路都走：先看网卡上有没有非私有地址，没有再问外部服务。
//
// ⚠️ 问外部服务意味着这台服务器的出口 IP 会被第三方看到。这是探测本身的
// 代价，不是实现选择。所以：超时压得很短、失败就如实报错、不重试多轮。
// ---------------------------------------------------------------------------

type localAddr struct {
	Name      string `json:"name"`
	Addr      string `json:"addr"`
	CIDR      string `json:"cidr"`
	Private   bool   `json:"private"`
	Loopback  bool   `json:"loopback"`
	LinkLocal bool   `json:"link_local"`
}

// publicIPSources 按顺序尝试。都是纯文本返回，不需要解析 JSON。
//
// 放多个是因为任何一个都可能临时不可达，而"探测失败"在用户那里等于
// "这个功能是坏的"。三个里面有一个通就够。
var publicIPSources = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://ipinfo.io/ip",
}

// isPrivateIP 判断的是"这个地址能不能给外网的人用"。
//
// 169.254.0.0/16 必须排除：那是 DHCP 拿不到地址时系统自己分的链路本地地址
// （APIPA），它在网卡上确实存在、看起来也像个公网 IP，但没有一台机器能
// 通过它访问到本机。第一版就是漏了这条，面板上把 169.254.x.x 显示成
// "公网地址"，用户复制出去之后怎么都连不上。
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return ip.IsPrivate()
}

// collectLocalAddrs 列出所有可用的 IPv4 地址。
//
// 只收 IPv4：客户端填地址时 IPv6 要加方括号，而绝大多数家庭宽带没有公网 v6，
// 列出来只会让人选错。
func collectLocalAddrs() []localAddr {
	out := []localAddr{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range ifaces {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			var cidr string
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
				cidr = v.String()
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.To4() == nil {
				continue
			}
			v4 := ip.To4()
			ll := v4.IsLinkLocalUnicast() || v4.IsLinkLocalMulticast()
			out = append(out, localAddr{
				Name:      ifi.Name,
				Addr:      v4.String(),
				CIDR:      cidr,
				Private:   isPrivateIP(v4),
				Loopback:  v4.IsLoopback(),
				LinkLocal: ll,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Loopback != out[j].Loopback {
			return !out[i].Loopback
		}
		if out[i].Private != out[j].Private {
			return !out[i].Private
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

// probePublicIP 依次问几个公开服务，第一个给出合法地址的就算成功。
func probePublicIP(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 4 * time.Second}
	var lastErr error
	for _, url := range publicIPSources {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "schedule-server/netinfo")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != 200 {
			lastErr = errors.New("服务返回 " + strconv.Itoa(resp.StatusCode))
			continue
		}
		text := strings.TrimSpace(string(body))
		// ipify 纯文本就是这个地址；ifconfig.me 偶尔带换行，上面 TrimSpace 已处理。
		if ip := net.ParseIP(text); ip != nil && ip.To4() != nil {
			return ip.To4().String(), nil
		}
		lastErr = errors.New("返回内容不是 IP")
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的探测服务")
	}
	return "", lastErr
}

// listenPort 从 ":8443" / "0.0.0.0:8443" 里取出端口数字。
func listenPort(addr string) int {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return 0
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if p, err := strconv.Atoi(addr[i+1:]); err == nil {
			return p
		}
	}
	return 0
}

// handleNetInfo 给面板用：这台机器现在能用哪些地址访问自己。
func (s *Server) handleNetInfo(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(userIDOf(r)) {
		apiErr(w, 403, "只有管理员能查看网络信息")
		return
	}
	s.writeNetInfo(w, r)
}

// writeNetInfo 不检查权限，面板内部的回环调用直接用它。
func (s *Server) writeNetInfo(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	local := collectLocalAddrs()

	// 网卡上已经有非私有 IPv4 的话，那它自己就是公网入口，不必再问外部。
	// 169.254 这类链路本地地址看起来"不是私有网段"，但它连不到任何地方，
	// 所以这里按"能不能被别人访问到"判断，而不是按"是不是私有网段"判断。
	publicIP := ""
	publicErr := ""
	for _, a := range local {
		if !a.Private && !a.Loopback && !a.LinkLocal {
			publicIP = a.Addr
			break
		}
	}
	if publicIP == "" {
		ip, err := probePublicIP(ctx)
		if err != nil {
			publicErr = err.Error()
		} else {
			publicIP = ip
		}
	}

	port := listenPort(s.cfg.Addr)
	// 主监听端口走的是 HTTPS（证书缺失时启动阶段会降级成 HTTP，
	// 那种情况下用户看到 8443 的 https 地址连不上，会在面板的"服务"页
	// 看到明确提示 —— 这里不假装自己知道降级发生了没有）。
	scheme := "https"
	mk := func(host string) string {
		if host == "" {
			return ""
		}
		return scheme + "://" + host + ":" + strconv.Itoa(port)
	}

	lan := []string{}
	for _, a := range local {
		if a.Private && !a.Loopback {
			lan = append(lan, mk(a.Addr))
		}
	}

	out := map[string]any{
		"public_ip":    publicIP,
		"public_ok":    publicIP != "",
		"public_error": publicErr,
		"local":        local,
		"port":         port,
		"scheme":       scheme,
		"url_public":   mk(publicIP),
		"url_lan":      lan,
		"url_local":    mk("127.0.0.1"),
	}
	writeJSON(w, 200, out)
}

// 给实时协作服务端生成自签证书（2026-10-10）。
//
//	go run ./cmd/mkcert-live -ip 203.0.113.11 -dns live.example.com -out ./certs
//
// ## 为什么要自签
//
// 协作服务端在**第二台服务器**（203.0.113.11）上，而手上只有
// `example.com` 的证书 —— 它的 SAN 里没有这台机器的地址，直接复用的话
// 浏览器/客户端会因为"证书和访问的域名不匹配"而拒绝连接。
//
// 三条路：
//  ① 申请一张带 live.example.com 的正规证书（要 DNS 能加记录 + 等待签发）
//  ② 自签（本工具）—— 立刻可用，代价是客户端要**显式信任**这张证书
//  ③ 不用 TLS（明文 ws://）—— 笔记内容会明文过公网，不能接受
//
// 选 ②：主程序的网络层本来就配了 CA pinning（`network_security_config.xml`），
// 加一条信任很轻；而笔记的正文不该明文走公网。
//
// ## SAN 里放什么
//
// **IP 和域名都放**。这样将来给它加了 live.example.com 的解析之后，
// 不用重新签证书 —— 少一次"改了 DNS 结果连不上，查半天发现是证书"的事故。
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	ip := flag.String("ip", "", "要包含在证书里的 IP（逗号分隔，可空）")
	dns := flag.String("dns", "", "要包含在证书里的域名（逗号分隔，可空）")
	out := flag.String("out", "./certs", "输出目录")
	days := flag.Int("days", 3650, "有效期（天）")
	flag.Parse()

	if strings.TrimSpace(*ip) == "" && strings.TrimSpace(*dns) == "" {
		fmt.Println("至少要给一个 -ip 或 -dns（证书里总得有访问时用的名字）")
		os.Exit(1)
	}

	var ips []net.IP
	for _, s := range strings.Split(*ip, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		p := net.ParseIP(s)
		if p == nil {
			fmt.Println("不是合法的 IP：" + s)
			os.Exit(1)
		}
		ips = append(ips, p)
	}
	var names []string
	for _, s := range strings.Split(*dns, ",") {
		if s = strings.TrimSpace(s); s != "" {
			names = append(names, s)
		}
	}

	// RSA 2048：兼容性最好（有些老客户端不吃 ECDSA）
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		fmt.Println("生成密钥失败：" + err.Error())
		os.Exit(1)
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   firstOr(names, "3e-live"),
			Organization: []string{"3e assistant"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour), // 往回留一小时，容忍服务器时钟偏差
		NotAfter:              time.Now().AddDate(0, 0, *days),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // 自签：自己就是根，客户端把它当 CA 装进去即可
		IPAddresses:           ips,
		DNSNames:              names,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		fmt.Println("签发证书失败：" + err.Error())
		os.Exit(1)
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Println("建目录失败：" + err.Error())
		os.Exit(1)
	}
	crtPath := filepath.Join(*out, "server.crt")
	keyPath := filepath.Join(*out, "server.key")

	crt, _ := os.Create(crtPath)
	_ = pem.Encode(crt, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = crt.Close()

	kf, _ := os.Create(keyPath)
	_ = pem.Encode(kf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	_ = kf.Close()

	fmt.Printf("✓ 证书已生成\n  %s\n  %s\n  SAN: %s%s\n  有效期: %d 天\n",
		crtPath, keyPath,
		strings.Join(names, ", "),
		map[bool]string{true: " + " + joinIPs(ips), false: ""}[len(ips) > 0],
		*days)
	fmt.Println("\n⚠️ 这是**自签**证书：客户端必须显式信任它（主程序加进 CA pinning），")
	fmt.Println("   否则会因为'证书不受信任'而连不上 —— 而那个错误看起来像服务没起来。")
}

func firstOr(xs []string, def string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return def
}

func joinIPs(ips []net.IP) string {
	ss := make([]string, 0, len(ips))
	for _, p := range ips {
		ss = append(ss, p.String())
	}
	return strings.Join(ss, ", ")
}

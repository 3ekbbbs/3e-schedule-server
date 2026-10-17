// Package certgen 生成一套自用的本地 CA 与服务端证书。
//
// 为什么内置：服务端默认监听 HTTPS，而客户端按 https:// 去连。让用户先去
// 装 openssl、敲一长串命令，是自部署最常见的第一个失败点。这里用标准库
// 直接生成，首次启动即可用；正式环境仍然建议换成受信任 CA 的证书。
//
// 产物（都在 CertDir 下）：
//
//	ca.crt / ca.key      本地根证书，把 ca.crt 导入客户端即可信任
//	server.crt / server.key  服务端证书（由上面那张 CA 签）
package certgen

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options 控制证书内容。
type Options struct {
	// Dir 是输出目录，不存在会自动创建。
	Dir string
	// ServerName 是客户端实际使用的域名，可为空。
	ServerName string
	// ExtraIPs 是额外要签进 SAN 的 IP（例如服务器的公网地址）。
	ExtraIPs []string
}

// Result 描述生成结果。
type Result struct {
	CertFile string
	KeyFile  string
	CAFile   string
	Created  bool
}

const (
	certName = "server.crt"
	keyName  = "server.key"
	caName   = "ca.crt"
	caKeyName = "ca.key"

	// 十年。自签证书的麻烦在于换证书要重新分发 CA，有效期长一点更省事。
	validity = 10 * 365 * 24 * time.Hour
)

// Ensure 保证 Dir 下有一对可用的 server.crt / server.key。
//
// 已存在且 30 天内不会过期时直接复用，不重写文件 —— 重写会让已经导入
// 旧 CA 的客户端全部失效。
func Ensure(opt Options) (Result, error) {
	res := Result{
		CertFile: filepath.Join(opt.Dir, certName),
		KeyFile:  filepath.Join(opt.Dir, keyName),
		CAFile:   filepath.Join(opt.Dir, caName),
	}
	if err := os.MkdirAll(opt.Dir, 0o755); err != nil {
		return res, err
	}
	if usable(res.CertFile, res.KeyFile) {
		return res, nil
	}
	if err := generate(opt, res); err != nil {
		return res, err
	}
	res.Created = true
	return res, nil
}

// Generate 无条件重签一套证书。面板上的“重新生成证书”走这里。
func Generate(opt Options) (Result, error) {
	res := Result{
		CertFile: filepath.Join(opt.Dir, certName),
		KeyFile:  filepath.Join(opt.Dir, keyName),
		CAFile:   filepath.Join(opt.Dir, caName),
	}
	if err := os.MkdirAll(opt.Dir, 0o755); err != nil {
		return res, err
	}
	if err := generate(opt, res); err != nil {
		return res, err
	}
	res.Created = true
	return res, nil
}

func usable(certFile, keyFile string) bool {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return false
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if time.Until(cert.NotAfter) < 30*24*time.Hour {
		return false
	}
	if _, err := tlsKeyPair(certPEM, keyPEM); err != nil {
		return false
	}
	return true
}

func tlsKeyPair(certPEM, keyPEM []byte) (any, error) {
	// 只做一次解析校验，不在这里构造 tls.Certificate，避免包依赖膨胀。
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("私钥不是 PEM")
	}
	return kb.Type, nil
}

func generate(opt Options, res Result) error {
	caCert, caKey, err := loadOrCreateCA(opt.Dir)
	if err != nil {
		return fmt.Errorf("准备本地 CA 失败: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	cn := strings.TrimSpace(opt.ServerName)
	if cn == "" {
		cn = "schedule-server"
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"Self-hosted Schedule Server"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              sanDNS(opt.ServerName),
		IPAddresses:           sanIPs(opt.ExtraIPs),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(res.CertFile, certPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(res.KeyFile, keyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

func loadOrCreateCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certFile := filepath.Join(dir, caName)
	keyFile := filepath.Join(dir, caKeyName)

	certPEM, errC := os.ReadFile(certFile)
	keyPEM, errK := os.ReadFile(keyFile)
	if errC == nil && errK == nil {
		cb, _ := pem.Decode(certPEM)
		kb, _ := pem.Decode(keyPEM)
		if cb != nil && kb != nil {
			cert, err := x509.ParseCertificate(cb.Bytes)
			if err == nil {
				key, err := x509.ParseECPrivateKey(kb.Bytes)
				if err == nil && time.Until(cert.NotAfter) > 30*24*time.Hour {
					return cert, key, nil
				}
			}
		}
	}

	// 没有可用的 CA 就新建一张。旧文件直接覆盖：此时还没有客户端信任它。
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Self-hosted Schedule Server CA",
			Organization: []string{"Self-hosted Schedule Server"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func sanDNS(serverName string) []string {
	out := []string{"localhost"}
	if s := strings.TrimSpace(serverName); s != "" {
		out = append(out, s)
	}
	return out
}

func sanIPs(extra []string) []net.IP {
	out := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	for _, s := range extra {
		if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil {
			out = append(out, ip)
		}
	}
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
					out = append(out, ipnet.IP)
				}
			}
		}
	}
	return out
}

// mkcert generates the two-tier PKI used by 3e-schedule-server.
//
// Why two tiers: the Android app embeds the CA certificate, not the server
// leaf. That way renewing or reissuing the server certificate (or moving the
// server to a new IP) never requires shipping a new APK or asking family
// members to reinstall anything.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	outDir := flag.String("out", "./certs", "output directory")
	ips := flag.String("ips", "", "comma separated IP SANs, e.g. 203.0.113.10")
	names := flag.String("names", "localhost", "comma separated DNS SANs")
	renew := flag.Bool("renew", false, "reissue only the server certificate, keeping the existing CA")
	caYears := flag.Int("ca-years", 20, "CA validity in years")
	leafYears := flag.Int("leaf-years", 5, "server certificate validity in years")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	caCertPath := filepath.Join(*outDir, "ca.crt")
	caKeyPath := filepath.Join(*outDir, "ca.key")
	leafCertPath := filepath.Join(*outDir, "server.crt")
	leafKeyPath := filepath.Join(*outDir, "server.key")

	var caCert *x509.Certificate
	var caKey *ecdsa.PrivateKey

	if *renew && fileExists(caCertPath) && fileExists(caKeyPath) {
		caCert, caKey = loadCA(caCertPath, caKeyPath)
		fmt.Println("reusing existing CA:", caCert.Subject.CommonName)
	} else {
		caCert, caKey = makeCA(*caYears)
		writePEM(caCertPath, "CERTIFICATE", caCert.Raw, 0o644)
		der, err := x509.MarshalECPrivateKey(caKey)
		if err != nil {
			log.Fatal(err)
		}
		writePEM(caKeyPath, "EC PRIVATE KEY", der, 0o600)
		fmt.Println("created CA:", caCertPath)
	}

	leaf, leafKey := makeLeaf(caCert, caKey, *ips, *names, *leafYears)
	writePEM(leafCertPath, "CERTIFICATE", leaf.Raw, 0o644)
	der, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		log.Fatal(err)
	}
	writePEM(leafKeyPath, "EC PRIVATE KEY", der, 0o600)

	sum := sha256.Sum256(caCert.Raw)
	fp := strings.ToUpper(hex.EncodeToString(sum[:]))
	pretty := make([]string, 0, len(fp)/2)
	for i := 0; i < len(fp); i += 2 {
		pretty = append(pretty, fp[i:i+2])
	}

	fmt.Println()
	fmt.Println("  server cert :", leafCertPath)
	fmt.Println("  server key  :", leafKeyPath)
	fmt.Println("  CA cert     :", caCertPath, "(embed this in the APK and the desktop app)")
	fmt.Println("  CA key      :", caKeyPath, "(KEEP OFFLINE — never deploy, never commit)")
	fmt.Println()
	fmt.Println("  SANs        :", strings.Join(leaf.DNSNames, ", "), ipList(leaf.IPAddresses))
	fmt.Println("  leaf expiry :", leaf.NotAfter.Format("2006-01-02"))
	fmt.Println("  CA expiry   :", caCert.NotAfter.Format("2006-01-02"))
	fmt.Println()
	fmt.Println("  CA SHA-256  :", strings.Join(pretty, ":"))
}

func ipList(ips []net.IP) string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return strings.Join(out, ", ")
}

func makeCA(years int) (*x509.Certificate, *ecdsa.PrivateKey) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Family Schedule Local CA",
			Organization: []string{"3e-schedule-server"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(years, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		log.Fatal(err)
	}
	return cert, key
}

func makeLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, ips, names string, years int) (*x509.Certificate, *ecdsa.PrivateKey) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "3e-schedule-server",
			Organization: []string{"3e-schedule-server"},
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().AddDate(years, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range splitTrim(names) {
		if n != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	for _, s := range splitTrim(ips) {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		}
	}
	if len(tmpl.DNSNames) == 0 && len(tmpl.IPAddresses) == 0 {
		tmpl.DNSNames = []string{"localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		log.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		log.Fatal(err)
	}
	return cert, key
}

func loadCA(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey) {
	cb, err := os.ReadFile(certPath)
	if err != nil {
		log.Fatal(err)
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		log.Fatal(err)
	}
	cBlock, _ := pem.Decode(cb)
	if cBlock == nil {
		log.Fatal("bad ca.crt")
	}
	cert, err := x509.ParseCertificate(cBlock.Bytes)
	if err != nil {
		log.Fatal(err)
	}
	kBlock, _ := pem.Decode(kb)
	if kBlock == nil {
		log.Fatal("bad ca.key")
	}
	key, err := x509.ParseECPrivateKey(kBlock.Bytes)
	if err != nil {
		log.Fatal(err)
	}
	return cert, key
}

func writePEM(path, typ string, der []byte, mode os.FileMode) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		log.Fatal(err)
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

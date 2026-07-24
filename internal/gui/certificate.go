package gui

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	httpsCAFileName      = "hfs-go-ca.crt"
	httpsCAKeyFileName   = "hfs-go-ca-key.pem"
	httpsCertFileName    = "hfs-go-cert.pem"
	httpsCertKeyFileName = "hfs-go-key.pem"
)

// HTTPSCertificateStatus is safe to expose directly through the WebView bridge.
// The CA is deliberately only written to disk; it is never installed into an OS
// or browser trust store automatically.
type HTTPSCertificateStatus struct {
	Available     bool      `json:"available"`
	CAAvailable   bool      `json:"caAvailable"`
	CAPath        string    `json:"caPath"`
	CAKeyPath     string    `json:"caKeyPath"`
	CertPath      string    `json:"certPath"`
	KeyPath       string    `json:"keyPath"`
	ExpiresAt     time.Time `json:"expiresAt,omitempty"`
	CAExpiresAt   time.Time `json:"caExpiresAt,omitempty"`
	Fingerprint   string    `json:"fingerprint,omitempty"`
	CAFingerprint string    `json:"caFingerprint,omitempty"`
	CAReused      bool      `json:"caReused,omitempty"`
	Message       string    `json:"message"`
}

type httpsCertificateFiles struct {
	caCert  string
	caKey   string
	cert    string
	certKey string
}

func httpsCertificateFilePaths(baseDir string) httpsCertificateFiles {
	return httpsCertificateFiles{
		caCert:  filepath.Join(baseDir, httpsCAFileName),
		caKey:   filepath.Join(baseDir, httpsCAKeyFileName),
		cert:    filepath.Join(baseDir, httpsCertFileName),
		certKey: filepath.Join(baseDir, httpsCertKeyFileName),
	}
}

// inspectHTTPSCertificate verifies the CA, leaf certificate, matching private
// keys, validity period, signature chain and ServerAuth usage.
func inspectHTTPSCertificate(baseDir string) HTTPSCertificateStatus {
	files := httpsCertificateFilePaths(baseDir)
	status := HTTPSCertificateStatus{
		CAPath:    files.caCert,
		CAKeyPath: files.caKey,
		CertPath:  files.cert,
		KeyPath:   files.certKey,
	}

	now := time.Now()
	caCert, _, caErr := loadHTTPSCA(files, now)
	if caErr == nil {
		status.CAAvailable = true
		status.CAExpiresAt = caCert.NotAfter
		status.CAFingerprint = certificateFingerprint(caCert.Raw)
	}

	leaf, leafErr := loadHTTPSLeaf(files, caCert, now)
	if leafErr == nil && caErr == nil {
		status.Available = true
		status.ExpiresAt = leaf.NotAfter
		status.Fingerprint = certificateFingerprint(leaf.Raw)
		status.Message = "HTTPS 证书可用"
		return status
	}

	var problems []string
	if caErr != nil {
		problems = append(problems, "CA："+caErr.Error())
	}
	if leafErr != nil {
		problems = append(problems, "服务器证书："+leafErr.Error())
	}
	status.Message = strings.Join(problems, "；")
	if status.Message == "" {
		status.Message = "尚未生成 HTTPS 证书"
	}
	return status
}

func inspectHTTPSCertificateForHost(baseDir, host string) HTTPSCertificateStatus {
	status := inspectHTTPSCertificate(baseDir)
	if !status.Available || strings.TrimSpace(host) == "" {
		return status
	}
	certificate, err := readPEMCertificate(status.CertPath)
	if err == nil {
		host = normalizedCertificateHost(host)
		if zone := strings.IndexByte(host, '%'); zone >= 0 {
			host = host[:zone]
		}
		err = certificate.VerifyHostname(host)
	}
	if err != nil {
		status.Available = false
		status.Message = fmt.Sprintf("HTTPS 证书不包含当前访问地址 %s，请重新生成证书", host)
	}
	return status
}

// generateHTTPSCertificate creates a private local CA on first use and issues
// a new server certificate for hosts. A valid existing CA is always reused.
// Corrupt, incomplete or expired CA files are never overwritten silently.
func generateHTTPSCertificate(baseDir string, hosts []string) (HTTPSCertificateStatus, error) {
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		return HTTPSCertificateStatus{}, errors.New("证书目录不能为空")
	}
	info, err := os.Stat(baseDir)
	if err != nil {
		return HTTPSCertificateStatus{}, fmt.Errorf("访问证书目录失败：%w", err)
	}
	if !info.IsDir() {
		return HTTPSCertificateStatus{}, errors.New("证书目录不是文件夹")
	}

	files := httpsCertificateFilePaths(baseDir)
	now := time.Now()
	caCertExists, err := regularFileExists(files.caCert)
	if err != nil {
		return inspectHTTPSCertificate(baseDir), err
	}
	caKeyExists, err := regularFileExists(files.caKey)
	if err != nil {
		return inspectHTTPSCertificate(baseDir), err
	}

	var (
		caCert     *x509.Certificate
		caKey      *ecdsa.PrivateKey
		caCertPEM  []byte
		caKeyPEM   []byte
		caReused   bool
		writeNewCA bool
	)
	if caCertExists || caKeyExists {
		if !caCertExists || !caKeyExists {
			err = errors.New("本地 CA 文件不完整；为避免丢失，请先备份或移除损坏文件")
			status := inspectHTTPSCertificate(baseDir)
			status.Message = err.Error()
			return status, err
		}
		caCert, caKey, err = loadHTTPSCA(files, now)
		if err != nil {
			err = fmt.Errorf("现有本地 CA 无效，未覆盖原文件：%w", err)
			status := inspectHTTPSCertificate(baseDir)
			status.Message = err.Error()
			return status, err
		}
		caReused = true
	} else {
		caCert, caKey, caCertPEM, caKeyPEM, err = createHTTPSCA(now)
		if err != nil {
			return inspectHTTPSCertificate(baseDir), err
		}
		writeNewCA = true
	}

	ipAddresses, dnsNames, err := httpsCertificateSANs(hosts)
	if err != nil {
		return inspectHTTPSCertificate(baseDir), err
	}
	leafCertPEM, leafKeyPEM, err := createHTTPSLeaf(caCert, caKey, ipAddresses, dnsNames, now)
	if err != nil {
		return inspectHTTPSCertificate(baseDir), err
	}

	writes := make([]atomicCertificateWrite, 0, 4)
	if writeNewCA {
		writes = append(writes,
			atomicCertificateWrite{path: files.caKey, data: caKeyPEM, mode: 0600},
			atomicCertificateWrite{path: files.caCert, data: caCertPEM, mode: 0644},
		)
	}
	writes = append(writes,
		atomicCertificateWrite{path: files.certKey, data: leafKeyPEM, mode: 0600},
		atomicCertificateWrite{path: files.cert, data: leafCertPEM, mode: 0644},
	)
	if err = commitCertificateFiles(baseDir, writes); err != nil {
		return inspectHTTPSCertificate(baseDir), fmt.Errorf("保存 HTTPS 证书失败：%w", err)
	}

	status := inspectHTTPSCertificate(baseDir)
	status.CAReused = caReused
	if !status.Available {
		return status, fmt.Errorf("生成后的 HTTPS 证书校验失败：%s", status.Message)
	}
	if caReused {
		status.Message = "HTTPS 证书已更新，并复用现有本地 CA"
	} else {
		status.Message = "HTTPS 证书和本地 CA 已生成"
	}
	return status, nil
}

func createHTTPSCA(now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("生成 CA 私钥失败：%w", err)
	}
	serial, err := randomCertificateSerial()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("生成 CA 序列号失败：%w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "HFS Go Local CA", Organization: []string{"HFS Go"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		SubjectKeyId:          publicKeyID(&key.PublicKey),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("创建 CA 证书失败：%w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("解析生成的 CA 证书失败：%w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("编码 CA 私钥失败：%w", err)
	}
	return cert, key,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

func createHTTPSLeaf(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, ips []net.IP, dnsNames []string, now time.Time) ([]byte, []byte, error) {
	if !caCert.NotAfter.After(now.Add(time.Minute)) {
		return nil, nil, errors.New("本地 CA 剩余有效期不足，无法签发服务器证书")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("生成服务器私钥失败：%w", err)
	}
	serial, err := randomCertificateSerial()
	if err != nil {
		return nil, nil, fmt.Errorf("生成服务器证书序列号失败：%w", err)
	}
	notAfter := now.Add(397 * 24 * time.Hour)
	if caCert.NotAfter.Before(notAfter) {
		notAfter = caCert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber:   serial,
		Subject:        pkix.Name{CommonName: "HFS Go Local Server", Organization: []string{"HFS Go"}},
		NotBefore:      now.Add(-5 * time.Minute),
		NotAfter:       notAfter,
		KeyUsage:       x509.KeyUsageDigitalSignature,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:    ips,
		DNSNames:       dnsNames,
		SubjectKeyId:   publicKeyID(&key.PublicKey),
		AuthorityKeyId: caCert.SubjectKeyId,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("签发服务器证书失败：%w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("编码服务器私钥失败：%w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

func loadHTTPSCA(files httpsCertificateFiles, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := readPEMCertificate(files.caCert)
	if err != nil {
		return nil, nil, err
	}
	key, err := readECPrivateKey(files.caKey)
	if err != nil {
		return nil, nil, err
	}
	if !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, errors.New("证书不具备 CA 签名权限")
	}
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return nil, nil, errors.New("CA 证书尚未生效或已过期")
	}
	if err = cert.CheckSignatureFrom(cert); err != nil {
		return nil, nil, fmt.Errorf("CA 自签名无效：%w", err)
	}
	publicKey, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !sameECDSAPublicKey(publicKey, &key.PublicKey) {
		return nil, nil, errors.New("CA 证书与私钥不匹配")
	}
	if key.Curve != elliptic.P256() {
		return nil, nil, errors.New("CA 私钥不是 ECDSA P-256")
	}
	return cert, key, nil
}

func loadHTTPSLeaf(files httpsCertificateFiles, caCert *x509.Certificate, now time.Time) (*x509.Certificate, error) {
	if _, err := tls.LoadX509KeyPair(files.cert, files.certKey); err != nil {
		return nil, fmt.Errorf("证书或私钥不可用：%w", err)
	}
	cert, err := readPEMCertificate(files.cert)
	if err != nil {
		return nil, err
	}
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return nil, errors.New("证书尚未生效或已过期")
	}
	serverAuth := false
	for _, usage := range cert.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth {
			serverAuth = true
			break
		}
	}
	if !serverAuth {
		return nil, errors.New("证书缺少 ServerAuth 用途")
	}
	if caCert == nil {
		return nil, errors.New("无法使用本地 CA 验证证书")
	}
	if err = cert.CheckSignatureFrom(caCert); err != nil {
		return nil, fmt.Errorf("本地 CA 签名验证失败：%w", err)
	}
	return cert, nil
}

func httpsCertificateSANs(hosts []string) ([]net.IP, []string, error) {
	ipSet := make(map[string]net.IP)
	dnsSet := map[string]struct{}{"localhost": {}}
	addIP := func(ip net.IP) {
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		} else {
			ip = ip.To16()
		}
		if ip != nil {
			ipSet[ip.String()] = append(net.IP(nil), ip...)
		}
	}
	addIP(net.ParseIP("127.0.0.1"))
	addIP(net.ParseIP("::1"))

	for _, raw := range hosts {
		host := normalizedCertificateHost(raw)
		if host == "" {
			if strings.TrimSpace(raw) == "" {
				continue
			}
			return nil, nil, fmt.Errorf("无效的证书地址：%q", raw)
		}
		if ip := net.ParseIP(strings.TrimSuffix(strings.Split(host, "%")[0], ".")); ip != nil {
			addIP(ip)
			continue
		}
		if !validCertificateDNSName(host) {
			return nil, nil, fmt.Errorf("无效的证书主机名：%q", raw)
		}
		dnsSet[strings.ToLower(strings.TrimSuffix(host, "."))] = struct{}{}
	}
	if hostname, err := os.Hostname(); err == nil {
		hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
		if validCertificateDNSName(hostname) {
			dnsSet[hostname] = struct{}{}
		}
	}

	ipKeys := make([]string, 0, len(ipSet))
	for value := range ipSet {
		ipKeys = append(ipKeys, value)
	}
	sort.Strings(ipKeys)
	ips := make([]net.IP, 0, len(ipKeys))
	for _, value := range ipKeys {
		ips = append(ips, ipSet[value])
	}
	dnsNames := make([]string, 0, len(dnsSet))
	for value := range dnsSet {
		dnsNames = append(dnsNames, value)
	}
	sort.Strings(dnsNames)
	return ips, dnsNames, nil
}

func normalizedCertificateHost(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(value, "[]")
}

func validCertificateDNSName(value string) bool {
	value = strings.TrimSuffix(strings.TrimSpace(value), ".")
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func randomCertificateSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for {
		serial, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, err
		}
		if serial.Sign() > 0 {
			return serial, nil
		}
	}
}

func publicKeyID(key *ecdsa.PublicKey) []byte {
	encoded := elliptic.Marshal(key.Curve, key.X, key.Y)
	sum := sha256.Sum256(encoded)
	return append([]byte(nil), sum[:20]...)
}

func sameECDSAPublicKey(left, right *ecdsa.PublicKey) bool {
	return left != nil && right != nil && left.Curve == right.Curve && left.X.Cmp(right.X) == 0 && left.Y.Cmp(right.Y) == 0
}

func certificateFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	encoded := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(encoded)/2)
	for index := 0; index < len(encoded); index += 2 {
		parts = append(parts, encoded[index:index+2])
	}
	return strings.Join(parts, ":")
}

func readPEMCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("PEM 证书格式无效")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析证书失败：%w", err)
	}
	return cert, nil
}

func readECPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("PEM 私钥格式无效")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析 ECDSA 私钥失败：%w", err)
	}
	return key, nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查 %s 失败：%w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s 不是普通文件", filepath.Base(path))
	}
	return true, nil
}

type atomicCertificateWrite struct {
	path string
	data []byte
	mode os.FileMode
}

type stagedCertificateWrite struct {
	atomicCertificateWrite
	temp      string
	backup    string
	hadTarget bool
	installed bool
}

func commitCertificateFiles(baseDir string, writes []atomicCertificateWrite) error {
	staged := make([]stagedCertificateWrite, 0, len(writes))
	cleanup := func() {
		for _, item := range staged {
			if item.temp != "" {
				_ = os.Remove(item.temp)
			}
			if item.backup != "" {
				_ = os.Remove(item.backup)
			}
		}
	}
	defer cleanup()

	for _, write := range writes {
		temp, err := os.CreateTemp(baseDir, ".hfs-go-certificate-*")
		if err != nil {
			return err
		}
		name := temp.Name()
		item := stagedCertificateWrite{atomicCertificateWrite: write, temp: name}
		if err = temp.Chmod(write.mode); err == nil {
			_, err = temp.Write(write.data)
		}
		if err == nil {
			err = temp.Sync()
		}
		if closeErr := temp.Close(); err == nil {
			err = closeErr
		}
		staged = append(staged, item)
		if err != nil {
			return err
		}
	}

	rollback := func(last int) {
		for index := last; index >= 0; index-- {
			item := &staged[index]
			if item.installed {
				_ = os.Remove(item.path)
			}
			if item.hadTarget && item.backup != "" {
				_ = os.Rename(item.backup, item.path)
				item.backup = ""
			}
		}
	}
	for index := range staged {
		item := &staged[index]
		if exists, err := regularFileExists(item.path); err != nil {
			rollback(index - 1)
			return err
		} else if exists {
			backupFile, createErr := os.CreateTemp(baseDir, ".hfs-go-certificate-backup-*")
			if createErr != nil {
				rollback(index - 1)
				return createErr
			}
			item.backup = backupFile.Name()
			_ = backupFile.Close()
			_ = os.Remove(item.backup)
			if err = os.Rename(item.path, item.backup); err != nil {
				rollback(index - 1)
				return err
			}
			item.hadTarget = true
		}
		if err := os.Rename(item.temp, item.path); err != nil {
			if item.hadTarget {
				_ = os.Rename(item.backup, item.path)
				item.backup = ""
			}
			rollback(index - 1)
			return err
		}
		item.temp = ""
		item.installed = true
		if err := os.Chmod(item.path, item.mode); err != nil {
			rollback(index)
			return err
		}
	}
	for index := range staged {
		if staged[index].backup != "" {
			_ = os.Remove(staged[index].backup)
			staged[index].backup = ""
		}
	}
	if runtime.GOOS != "windows" {
		if dir, err := os.Open(baseDir); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	return nil
}

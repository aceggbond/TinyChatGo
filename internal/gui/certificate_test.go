package gui

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGenerateHTTPSCertificateCreatesValidChainAndSANs(t *testing.T) {
	dir := t.TempDir()
	status, err := generateHTTPSCertificate(dir, []string{
		"192.168.8.24",
		"https://10.20.30.40:9443/path",
		"[2001:db8::24]:9443",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || !status.CAAvailable || status.CAReused {
		t.Fatalf("unexpected generated status: %#v", status)
	}
	if status.CAPath != filepath.Join(dir, httpsCAFileName) ||
		status.CAKeyPath != filepath.Join(dir, httpsCAKeyFileName) ||
		status.CertPath != filepath.Join(dir, httpsCertFileName) ||
		status.KeyPath != filepath.Join(dir, httpsCertKeyFileName) {
		t.Fatalf("unexpected generated paths: %#v", status)
	}
	if status.Fingerprint == "" || status.CAFingerprint == "" || !status.ExpiresAt.After(time.Now()) {
		t.Fatalf("missing certificate metadata: %#v", status)
	}

	ca, err := readPEMCertificate(status.CAPath)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := readPEMCertificate(status.CertPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatalf("generated CA is not usable: %#v", ca)
	}
	if err = ca.CheckSignatureFrom(ca); err != nil {
		t.Fatalf("CA self-signature failed: %v", err)
	}
	if err = leaf.CheckSignatureFrom(ca); err != nil {
		t.Fatalf("leaf signature failed: %v", err)
	}
	if _, err = tls.LoadX509KeyPair(status.CertPath, status.KeyPath); err != nil {
		t.Fatalf("leaf key does not match certificate: %v", err)
	}
	for _, host := range []string{"192.168.8.24", "10.20.30.40", "2001:db8::24", "127.0.0.1", "::1", "localhost"} {
		if err = leaf.VerifyHostname(host); err != nil {
			t.Errorf("SAN does not contain %q: %v", host, err)
		}
	}
	serverAuth := false
	for _, usage := range leaf.ExtKeyUsage {
		serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth
	}
	if !serverAuth {
		t.Fatal("leaf certificate does not have ServerAuth")
	}
	if leaf.NotAfter.After(ca.NotAfter) {
		t.Fatal("leaf expires after its CA")
	}

	caKeyData, err := os.ReadFile(status.CAKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(caKeyData)
	if block == nil {
		t.Fatal("CA key is not PEM")
	}
	caKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := any(caKey).(*ecdsa.PrivateKey); !ok || caKey.Curve.Params().Name != "P-256" {
		t.Fatal("CA key is not ECDSA P-256")
	}

	if runtime.GOOS != "windows" {
		for _, path := range []string{status.CAKeyPath, status.KeyPath} {
			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Mode().Perm() != 0600 {
				t.Errorf("private key %s mode = %o, want 600", filepath.Base(path), info.Mode().Perm())
			}
		}
	}

	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{`"available":true`, `"caPath":`, `"certPath":`, `"keyPath":`, `"expiresAt":`, `"fingerprint":`, `"message":`} {
		if !bytes.Contains(encoded, []byte(marker)) {
			t.Errorf("status JSON missing %s: %s", marker, encoded)
		}
	}
}

func TestGenerateHTTPSCertificateBundleCanBeValidatedFromDatabaseBytes(t *testing.T) {
	bundle, status, err := generateHTTPSCertificateBundle([]string{"127.0.0.1", "192.168.8.24", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || len(bundle.CAKeyPEM) == 0 || len(bundle.KeyPEM) == 0 {
		t.Fatalf("unexpected bundle status: %#v", status)
	}
	for _, host := range []string{"127.0.0.1", "192.168.8.24", "localhost"} {
		if checked := inspectHTTPSCertificateBundle(bundle, host); !checked.Available {
			t.Fatalf("bundle did not validate for %s: %#v", host, checked)
		}
	}
}

func TestGenerateHTTPSCertificateReusesCAAndRenewsLeaf(t *testing.T) {
	dir := t.TempDir()
	first, err := generateHTTPSCertificate(dir, []string{"192.168.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	caCertBefore, err := os.ReadFile(first.CAPath)
	if err != nil {
		t.Fatal(err)
	}
	caKeyBefore, err := os.ReadFile(first.CAKeyPath)
	if err != nil {
		t.Fatal(err)
	}

	second, err := generateHTTPSCertificate(dir, []string{"192.168.1.11"})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Available || !second.CAReused {
		t.Fatalf("expected reused CA status: %#v", second)
	}
	caCertAfter, _ := os.ReadFile(second.CAPath)
	caKeyAfter, _ := os.ReadFile(second.CAKeyPath)
	if !bytes.Equal(caCertBefore, caCertAfter) || !bytes.Equal(caKeyBefore, caKeyAfter) {
		t.Fatal("valid CA files changed while renewing leaf certificate")
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("leaf certificate was not renewed")
	}
	if first.CAFingerprint != second.CAFingerprint {
		t.Fatal("CA fingerprint changed")
	}
	leaf, err := readPEMCertificate(second.CertPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = leaf.VerifyHostname("192.168.1.11"); err != nil {
		t.Fatalf("renewed leaf is missing new SAN: %v", err)
	}
	if err = leaf.VerifyHostname("192.168.1.10"); err == nil {
		t.Fatal("renewed leaf unexpectedly retained stale SAN")
	}
}

func TestHTTPSCertificateDamageFailsSafely(t *testing.T) {
	dir := t.TempDir()
	status, err := generateHTTPSCertificate(dir, []string{"172.16.0.8"})
	if err != nil {
		t.Fatal(err)
	}
	originalKey, err := os.ReadFile(status.CAKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	corruptCA := []byte("not a certificate\n")
	if err = os.WriteFile(status.CAPath, corruptCA, 0644); err != nil {
		t.Fatal(err)
	}

	got := inspectHTTPSCertificate(dir)
	if got.Available || got.CAAvailable || got.Message == "" {
		t.Fatalf("corrupt CA reported as usable: %#v", got)
	}
	if _, err = generateHTTPSCertificate(dir, []string{"172.16.0.9"}); err == nil {
		t.Fatal("expected corrupt existing CA to fail generation")
	}
	caAfter, _ := os.ReadFile(status.CAPath)
	keyAfter, _ := os.ReadFile(status.CAKeyPath)
	if !bytes.Equal(caAfter, corruptCA) || !bytes.Equal(keyAfter, originalKey) {
		t.Fatal("generation overwrote existing CA files after validation failure")
	}
}

func TestInspectHTTPSCertificateRejectsMismatchedLeafKey(t *testing.T) {
	dir := t.TempDir()
	first, err := generateHTTPSCertificate(dir, []string{"10.0.0.5"})
	if err != nil {
		t.Fatal(err)
	}
	otherDir := t.TempDir()
	other, err := generateHTTPSCertificate(otherDir, []string{"10.0.0.6"})
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := os.ReadFile(other.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(first.KeyPath, otherKey, 0600); err != nil {
		t.Fatal(err)
	}

	status := inspectHTTPSCertificate(dir)
	if status.Available || !status.CAAvailable {
		t.Fatalf("mismatched leaf key status is incorrect: %#v", status)
	}
	if !strings.Contains(status.Message, "不匹配") && !strings.Contains(status.Message, "private key") {
		t.Fatalf("mismatched key error is unclear: %s", status.Message)
	}
}

func TestGenerateHTTPSCertificateRejectsInvalidSANWithoutFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := generateHTTPSCertificate(dir, []string{"bad host name"}); err == nil {
		t.Fatal("expected invalid SAN to fail")
	}
	for _, name := range []string{httpsCAFileName, httpsCAKeyFileName, httpsCertFileName, httpsCertKeyFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("invalid SAN left certificate file %s", name)
		}
	}
}

func TestInspectHTTPSCertificateForHostRejectsStaleSAN(t *testing.T) {
	dir := t.TempDir()
	if _, err := generateHTTPSCertificate(dir, []string{"192.168.50.10"}); err != nil {
		t.Fatal(err)
	}
	if status := inspectHTTPSCertificateForHost(dir, "192.168.50.10"); !status.Available {
		t.Fatalf("matching SAN was rejected: %#v", status)
	}
	status := inspectHTTPSCertificateForHost(dir, "192.168.50.11")
	if status.Available || !strings.Contains(status.Message, "重新生成") {
		t.Fatalf("stale SAN status = %#v", status)
	}
}

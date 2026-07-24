package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestStartWithHTTPAndHTTPS(t *testing.T) {
	certFile, keyFile, roots := writeTestCertificate(t)
	s := New(io.Discard)
	s.SetChatEnabled(true)
	addresses, err := s.StartWithHTTPS("127.0.0.1:0", "127.0.0.1:0", certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if addresses.HTTP == "" || addresses.HTTPS == "" {
		t.Fatalf("incomplete addresses: %+v", addresses)
	}

	res, err := http.Get("http://" + addresses.HTTP + "/")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HTTP status = %d", res.StatusCode)
	}
	_ = res.Body.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    roots,
		MinVersion: tls.VersionTLS12,
	}}}
	res, err = client.Get("https://" + addresses.HTTPS + "/")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS status = %d", res.StatusCode)
	}
	if res.TLS == nil || len(res.TLS.PeerCertificates) == 0 {
		t.Fatal("HTTPS response did not include a peer certificate")
	}
	if got := res.TLS.PeerCertificates[0].Subject.CommonName; got != "HFS Go test" {
		t.Fatalf("certificate common name = %q", got)
	}
	_ = res.Body.Close()

	statusResponse, err := client.Get("https://" + addresses.HTTPS + "/__hfs/chat/status")
	if err != nil {
		t.Fatal(err)
	}
	cookies := statusResponse.Cookies()
	_ = statusResponse.Body.Close()
	if len(cookies) == 0 {
		t.Fatal("HTTPS chat status did not create a secure session cookie")
	}
	header := http.Header{"Origin": []string{"https://" + addresses.HTTPS}}
	for _, cookie := range cookies {
		header.Add("Cookie", cookie.Name+"="+cookie.Value)
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 2 * time.Second,
		TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			MinVersion: tls.VersionTLS12,
		},
	}
	connection, handshake, err := dialer.Dial("wss://"+addresses.HTTPS+"/__hfs/chat/ws?tab=0123456789abcdef0123456789abcdef", header)
	if err != nil {
		if handshake != nil {
			t.Fatalf("WSS handshake status %d: %v", handshake.StatusCode, err)
		}
		t.Fatalf("WSS handshake failed: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	var ready struct {
		Type string `json:"type"`
	}
	if err = connection.ReadJSON(&ready); err != nil {
		t.Fatalf("read WSS ready message: %v", err)
	}
	if ready.Type != "ready" {
		t.Fatalf("first WSS message = %#v", ready)
	}
	_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second))
	_ = connection.Close()
	client.CloseIdleConnections()

	if err = s.Stop(); err != nil {
		t.Fatal(err)
	}
	if err = s.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	for protocol, address := range map[string]string{"HTTP": addresses.HTTP, "HTTPS": addresses.HTTPS} {
		connection, dialErr := net.DialTimeout("tcp", address, 150*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			t.Fatalf("%s listener still accepts connections after Stop", protocol)
		}
	}
}

func TestStartWithHTTPSPEMDoesNotRequireCertificateFiles(t *testing.T) {
	certFile, keyFile, _ := writeTestCertificate(t)
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	s := New(io.Discard)
	addresses, err := s.StartWithHTTPSPEM("127.0.0.1:0", "127.0.0.1:0", certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if addresses.HTTP == "" || addresses.HTTPS == "" {
		t.Fatalf("incomplete addresses: %+v", addresses)
	}
}

func TestStartWithHTTPSRollsBackAllListeners(t *testing.T) {
	certFile, keyFile, _ := writeTestCertificate(t)

	t.Run("HTTPS port conflict releases HTTP", func(t *testing.T) {
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer occupied.Close()
		httpAddr := unusedTCPAddress(t)

		s := New(io.Discard)
		if _, err = s.StartWithHTTPS(httpAddr, occupied.Addr().String(), certFile, keyFile); err == nil {
			t.Fatal("expected HTTPS port conflict")
		}
		probe, err := net.Listen("tcp", httpAddr)
		if err != nil {
			t.Fatalf("HTTP listener was not rolled back: %v", err)
		}
		_ = probe.Close()
		if err = s.Stop(); err != nil {
			t.Fatalf("Stop after failed start: %v", err)
		}
	})

	t.Run("HTTP port conflict never binds HTTPS", func(t *testing.T) {
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer occupied.Close()
		httpsAddr := unusedTCPAddress(t)

		s := New(io.Discard)
		if _, err = s.StartWithHTTPS(occupied.Addr().String(), httpsAddr, certFile, keyFile); err == nil {
			t.Fatal("expected HTTP port conflict")
		}
		probe, err := net.Listen("tcp", httpsAddr)
		if err != nil {
			t.Fatalf("HTTPS listener was unexpectedly retained: %v", err)
		}
		_ = probe.Close()
	})

	t.Run("invalid certificate binds nothing", func(t *testing.T) {
		invalidCert := filepath.Join(t.TempDir(), "invalid.pem")
		if err := os.WriteFile(invalidCert, []byte("not a certificate"), 0600); err != nil {
			t.Fatal(err)
		}
		httpAddr := unusedTCPAddress(t)
		s := New(io.Discard)
		if _, err := s.StartWithHTTPS(httpAddr, "127.0.0.1:0", invalidCert, invalidCert); err == nil {
			t.Fatal("expected invalid certificate error")
		}
		probe, err := net.Listen("tcp", httpAddr)
		if err != nil {
			t.Fatalf("certificate validation happened after HTTP bind: %v", err)
		}
		_ = probe.Close()
	})
}

func TestStartWithHTTPSOnlyHTTPAndStartCompatibility(t *testing.T) {
	s := New(io.Discard)
	addresses, err := s.StartWithHTTPS("127.0.0.1:0", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if addresses.HTTP == "" || addresses.HTTPS != "" {
		t.Fatalf("addresses = %+v", addresses)
	}
	repeated, err := s.StartWithHTTPS("127.0.0.1:1", "127.0.0.1:2", "missing", "missing")
	if err != nil || repeated != addresses {
		t.Fatalf("idempotent start = %+v, %v", repeated, err)
	}
	if err = s.Stop(); err != nil {
		t.Fatal(err)
	}

	legacyAddress, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if legacyAddress == "" {
		t.Fatal("Start returned an empty address")
	}
	if err = s.Stop(); err != nil {
		t.Fatal(err)
	}
}

func unusedTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func writeTestCertificate(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "HFS Go test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err = os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("failed to add test certificate to pool")
	}
	return certFile, keyFile, roots
}

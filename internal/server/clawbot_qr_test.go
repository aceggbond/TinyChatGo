package server

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func TestClawBotQRCodeIsServedAsLocalPNG(t *testing.T) {
	manager := newClawBotManager(log.New(bytes.NewBuffer(nil), "", 0))
	manager.bindings["account"] = &ClawBotBinding{
		AccountID:   "account",
		Status:      "waiting",
		QRCode:      "poll-code",
		QRCodeURL:   "https://weixin.qq.com/x/example-qr-payload",
		QRExpiresAt: time.Now().Add(time.Minute),
		UpdatedAt:   time.Now(),
	}

	state := manager.publicState("account")
	if !strings.HasPrefix(state.QRCodeURL, "/__hfs/clawbot/qr-image?v=") {
		t.Fatalf("public QR URL = %q", state.QRCodeURL)
	}
	png, err := manager.qrImage("account")
	if err != nil {
		t.Fatal(err)
	}
	if len(png) < 8 || !bytes.Equal(png[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		t.Fatal("QR image is not a PNG")
	}
}

func TestExpiredClawBotQRCodeRequestsReplacement(t *testing.T) {
	manager := newClawBotManager(log.New(bytes.NewBuffer(nil), "", 0))
	manager.bindings["account"] = &ClawBotBinding{
		AccountID:   "account",
		Status:      "waiting",
		QRCode:      "expired-code",
		QRCodeURL:   "expired-payload",
		QRExpiresAt: time.Now().Add(-time.Second),
	}

	state := manager.publicState("account")
	if state.Status != "unbound" || state.QRCodeURL != "" {
		t.Fatalf("expired public state = %#v", state)
	}
	if _, err := manager.qrImage("account"); err == nil {
		t.Fatal("expired QR image remained available")
	}
}

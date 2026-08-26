package clawbot

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetUpdatesUsesOfficialHeadersAndCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ilink/bot/getupdates" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		if r.Header.Get("AuthorizationType") != "ilink_bot_token" || r.Header.Get("X-WECHAT-UIN") == "" {
			t.Fatalf("missing iLink headers: %#v", r.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["get_updates_buf"] != "cursor" {
			t.Fatalf("cursor = %#v", body["get_updates_buf"])
		}
		_, _ = w.Write([]byte(`{"ret":0,"get_updates_buf":"next","msgs":[]}`))
	}))
	defer server.Close()
	result, err := (&Client{}).GetUpdates(context.Background(), Credentials{BaseURL: server.URL, Token: "secret"}, "cursor")
	if err != nil || result.Buffer != "next" {
		t.Fatalf("GetUpdates = %#v, %v", result, err)
	}
}

func TestAES128ECBMediaRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef")
	for _, plain := range [][]byte{[]byte("a"), []byte("sixteen bytes!!!"), bytes.Repeat([]byte("media"), 100)} {
		ciphertext, err := encryptECB(plain, key)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decryptECB(ciphertext, key)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded, plain) {
			t.Fatalf("roundtrip mismatch: %q", decoded)
		}
	}
}

func TestSendTextIncludesContextToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message Message `json:"msg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Message.ContextToken != "context" || body.Message.ToUserID != "wx-user" || len(body.Message.Items) != 1 {
			t.Fatalf("message = %#v", body.Message)
		}
		if !strings.HasPrefix(body.Message.ClientID, "tinychatgo-") {
			t.Fatalf("client id = %q", body.Message.ClientID)
		}
		_, _ = w.Write([]byte(`{"ret":0}`))
	}))
	defer server.Close()
	if err := (&Client{}).SendText(context.Background(), Credentials{BaseURL: server.URL, Token: "secret"}, "wx-user", "context", "你好"); err != nil {
		t.Fatal(err)
	}
}

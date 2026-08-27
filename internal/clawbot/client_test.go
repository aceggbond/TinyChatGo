package clawbot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
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

func TestDecodeMediaAESKeyAcceptsWeixinFormats(t *testing.T) {
	want := []byte("0123456789abcdef")
	hexKey := hex.EncodeToString(want)
	formats := []string{
		hexKey,
		base64.StdEncoding.EncodeToString(want),
		base64.StdEncoding.EncodeToString([]byte(hexKey)),
	}
	for _, value := range formats {
		got, err := decodeMediaAESKey(value)
		if err != nil {
			t.Fatalf("decode %q: %v", value, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("decode %q = %x, want %x", value, got, want)
		}
	}
}

func TestDownloadMediaAcceptsDirectPlaintextImageURL(t *testing.T) {
	want := []byte("plain image bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(want)
	}))
	defer server.Close()

	got, err := (&Client{}).DownloadMedia(context.Background(), Media{FullURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("direct image = %q, want %q", got, want)
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

func TestSendMediaUsesWeixinOutboundAESKeyEncoding(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getuploadurl":
			var body struct {
				AESKey string `json:"aeskey"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.AESKey) != 32 {
				t.Fatalf("getuploadurl aeskey = %q", body.AESKey)
			}
			_, _ = w.Write([]byte(`{"upload_full_url":"` + server.URL + `/cdn/upload"}`))
		case "/cdn/upload":
			w.Header().Set("X-Encrypted-Param", "download-param")
		case "/ilink/bot/sendmessage":
			var body struct {
				Message Message `json:"msg"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			media := body.Message.Items[0].Image.Media
			decoded, err := base64.StdEncoding.DecodeString(media.AESKey)
			if err != nil {
				t.Fatal(err)
			}
			if len(decoded) != 32 {
				t.Fatalf("decoded outbound aes_key length = %d, want 32 hex bytes", len(decoded))
			}
			if _, err := hex.DecodeString(string(decoded)); err != nil {
				t.Fatalf("outbound aes_key is not base64(hex(key)): %q", decoded)
			}
			if media.EncryptQueryParam != "download-param" || media.EncryptType != 1 {
				t.Fatalf("media = %#v", media)
			}
			_, _ = w.Write([]byte(`{"ret":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	err := (&Client{}).SendMedia(context.Background(), Credentials{BaseURL: server.URL, Token: "secret"}, "wx-user", "context", "image.png", []byte("image bytes"), true)
	if err != nil {
		t.Fatal(err)
	}
}

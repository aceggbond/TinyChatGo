package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tinychatgo/internal/appinfo"
)

func TestClientDownloadToggleControlsPortalAndExecutable(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)

	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if got := recorder.Header().Get("X-TinyChatGo-Version"); got != appinfo.Version {
		t.Fatalf("server version header = %q, want %q", got, appinfo.Version)
	}
	if got := recorder.Header().Get("X-TinyChatGo-Client-Download"); got != "false" {
		t.Fatalf("disabled client-download header = %q", got)
	}
	if !strings.Contains(recorder.Body.String(), `id="client-download"`) ||
		!strings.Contains(recorder.Body.String(), `id="web-download-row" class="settings-row" hidden`) {
		t.Fatal("disabled client download is not rendered hidden")
	}

	downloadRequest := httptest.NewRequest(http.MethodHead, "http://example.test/__hfs/client/download", nil)
	downloadRecorder := httptest.NewRecorder()
	s.ServeHTTP(downloadRecorder, downloadRequest)
	if downloadRecorder.Code != http.StatusNotFound {
		t.Fatalf("disabled download status = %d", downloadRecorder.Code)
	}

	s.SetClientDownloadEnabled(true)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if got := recorder.Header().Get("X-TinyChatGo-Client-Download"); got != "true" {
		t.Fatalf("enabled client-download header = %q", got)
	}
	body := recorder.Body.String()
	if strings.Contains(body, `id="web-download-row" class="settings-row" hidden`) {
		t.Fatal("enabled client download remained hidden")
	}
	for _, marker := range []string{
		`notifyButton.hidden=true`,
		`$('native-settings').hidden=!nativeClient`,
		`clientPlatform.indexOf('mac')`,
		`'下载 macOS ARM 客户端':'下载 Windows 客户端'`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("automatic client platform UI missing %q", marker)
		}
	}

	downloadRecorder = httptest.NewRecorder()
	s.ServeHTTP(downloadRecorder, downloadRequest)
	if downloadRecorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("enabled download status = %d", downloadRecorder.Code)
	}
	wantDownload := "https://github.com/aceggbond/TinyChatGo/releases/download/" + appinfo.Tag + "/TinyChatGo-Client-windows-amd64.exe"
	if got := downloadRecorder.Header().Get("Location"); got != wantDownload {
		t.Fatalf("Windows client redirect = %q, want %q", got, wantDownload)
	}
}

func TestClientDownloadDetectsMacOSAndUsesArm64Release(t *testing.T) {
	for _, test := range []struct {
		name     string
		platform string
		ua       string
		want     string
	}{
		{name: "client hint", platform: `"macOS"`, want: "macos-arm64"},
		{name: "user agent", ua: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5)", want: "macos-arm64"},
		{name: "windows", platform: `"Windows"`, ua: "Mozilla/5.0 (Windows NT 10.0; Win64; x64)", want: "windows-amd64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://example.test/__hfs/client/download", nil)
			request.Header.Set("Sec-CH-UA-Platform", test.platform)
			request.Header.Set("User-Agent", test.ua)
			if got := clientDownloadPlatform(request); got != test.want {
				t.Fatalf("platform = %q, want %q", got, test.want)
			}
		})
	}

	s := New(io.Discard)
	s.SetClientDownloadEnabled(true)
	request := httptest.NewRequest(http.MethodGet, "http://example.test/__hfs/client/download", nil)
	request.Header.Set("Sec-CH-UA-Platform", `"macOS"`)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("macOS download status = %d", recorder.Code)
	}
	want := "https://github.com/aceggbond/TinyChatGo/releases/download/" + appinfo.Tag + "/TinyChatGo-Client-macos-arm64.zip"
	if got := recorder.Header().Get("Location"); got != want {
		t.Fatalf("macOS redirect = %q, want %q", got, want)
	}
}

func TestPortalComposerOffersUnifiedImageFileAndEmojiTools(t *testing.T) {
	s := New(io.Discard)
	s.SetChatEnabled(true)
	s.SetUserListEnabled(true)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://example.test/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("portal status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, marker := range []string{
		`class="compose-toolbar"`,
		`id="emoji-button" class="compose-tool"`,
		`id="chat-image-button" class="compose-tool"`,
		`id="chat-file-button" class="compose-tool"`,
		`id="chat-code-button" class="compose-tool"`,
		`id="chat-dice-button" class="compose-tool"`,
		`id="chat-image-input" class="compose-file-input" type="file"`,
		`id="chat-file-input" class="compose-file-input" type="file"`,
		`queueFiles(imageInput.files)`,
		`queueFiles(fileInput.files)`,
		`class="message-sender"`,
		`.message.other.group-message .message-sender`,
		`.portal-grid.layout-chat-users{grid-template-columns:96px`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("responsive chat composer missing %q", marker)
		}
	}
}

func TestFluentEmojiAssetsAreEmbeddedAndCacheable(t *testing.T) {
	s := New(io.Discard)
	request := httptest.NewRequest(http.MethodGet, "http://example.test/__hfs/fluent-emoji/red_heart.png", nil)
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("emoji asset status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("emoji content type = %q", got)
	}
	if !strings.Contains(recorder.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("emoji cache control = %q", recorder.Header().Get("Cache-Control"))
	}
	if recorder.Body.Len() < 100 {
		t.Fatalf("emoji asset is unexpectedly small: %d bytes", recorder.Body.Len())
	}

	request = httptest.NewRequest(http.MethodGet, "http://example.test/__hfs/fluent-emoji/unknown.png", nil)
	recorder = httptest.NewRecorder()
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown emoji asset status = %d", recorder.Code)
	}
}

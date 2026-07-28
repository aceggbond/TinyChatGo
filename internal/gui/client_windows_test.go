//go:build windows

package gui

import (
	"os"
	"strings"
	"testing"
)

func TestNormalizeDesktopClientURL(t *testing.T) {
	for input, want := range map[string]string{
		"192.168.1.8":                 "http://192.168.1.8",
		"http://192.168.1.8:8080/a?q": "http://192.168.1.8:8080",
		"https://server.local/chat":   "https://server.local",
	} {
		got, err := normalizeDesktopClientURL(input)
		if err != nil || got != want {
			t.Fatalf("normalize %q = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "file:///C:/Windows", "javascript:alert(1)", "http://user:pass@server"} {
		if _, err := normalizeDesktopClientURL(input); err == nil {
			t.Fatalf("unsafe client URL accepted: %q", input)
		}
	}
}

func TestConfigureClientWebView2TLSKeepsExistingArguments(t *testing.T) {
	const name = "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"
	original, existed := os.LookupEnv(name)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, original)
		} else {
			_ = os.Unsetenv(name)
		}
	})
	_ = os.Setenv(name, "--disable-features=Example")
	configureClientWebView2TLS()
	got := os.Getenv(name)
	if !strings.Contains(got, "--disable-features=Example") ||
		!strings.Contains(got, "--ignore-certificate-errors") {
		t.Fatalf("WebView2 arguments = %q", got)
	}
	configureClientWebView2TLS()
	if strings.Count(os.Getenv(name), "--ignore-certificate-errors") != 1 {
		t.Fatalf("ignore-certificate-errors was duplicated: %q", os.Getenv(name))
	}
}

func TestDesktopClientHTMLIncludesDiscoveryTrayAndNotificationControls(t *testing.T) {
	html := renderDesktopClientHTML("data:image/png;base64,test")
	for _, marker := range []string{
		"启动时只发送一次局域网发现广播",
		`id="address"`,
		`id="scan"`,
		`id="autoStart"`,
		`id="notifications"`,
		`id="sound"`,
		"右击托盘图标可重新扫描、调整通知或退出客户端",
		"window.clientConnect",
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("desktop client HTML missing %q", marker)
		}
	}
}

func TestModernServerSettingsExposeClientDownloadToggle(t *testing.T) {
	html := renderModernHTML(nil, nil)
	for _, marker := range []string{
		`id="allow-client-download"`,
		`allowClientDownload:$('allow-client-download').checked`,
		`$('allow-client-download').checked=!!s.allowClientDownload`,
		"自动提供 Windows x64 或 macOS ARM64 客户端",
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("server client-download setting missing %q", marker)
		}
	}
}

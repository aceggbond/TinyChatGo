//go:build windows

package gui

import (
	"encoding/binary"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientTrayAlertIconKeepsLogoShapeWithoutSystemErrorIcon(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			source.SetNRGBA(x, y, color.NRGBA{R: 38, G: 128, B: 246, A: 255})
		}
	}
	icon := buildClientTrayAlertIconDIB(source, clientTrayAlertIconSize)
	wantSize := 40 + clientTrayAlertIconSize*clientTrayAlertIconSize*4 +
		((clientTrayAlertIconSize+31)/32)*4*clientTrayAlertIconSize
	if len(icon) != wantSize {
		t.Fatalf("alert icon size = %d, want %d", len(icon), wantSize)
	}
	width, height := binary.LittleEndian.Uint32(icon[4:8]), binary.LittleEndian.Uint32(icon[8:12])
	if width != 32 || height != 64 {
		t.Fatalf("alert icon dimensions = %dx%d, want 32x64 DIB", width, height)
	}
	blue, green, red, alpha := icon[40], icon[41], icon[42], icon[43]
	if alpha != 255 || red <= green*2 || red <= blue*2 {
		t.Fatalf("alert pixel BGRA = %d,%d,%d,%d, want opaque red-tinted logo", blue, green, red, alpha)
	}
}

func TestClientTrayAlertIconCanBeCreatedFromProjectLogo(t *testing.T) {
	logo, err := os.ReadFile(filepath.Join("..", "..", "logo.png"))
	if err != nil {
		t.Fatal(err)
	}
	icon := createClientTrayAlertIcon(logo)
	if icon == 0 {
		t.Fatal("CreateIconFromResourceEx could not create the red LanChatGo tray icon")
	}
	clientDestroyIcon.Call(icon)
}

func TestTrayCallbackEventSupportsNotifyIconVersion4Packing(t *testing.T) {
	if got := trayCallbackEvent(uintptr(2)<<16 | 0x405); got != 0x405 {
		t.Fatalf("tray callback event = %#x, want balloon click", got)
	}
	if got := trayCallbackEvent(0x205); got != 0x205 {
		t.Fatalf("legacy tray callback event = %#x, want right click", got)
	}
	if got := trayCallbackEvent(uintptr(2)<<16 | wmContextMenu); got != wmContextMenu {
		t.Fatalf("version 4 context event = %#x, want context menu", got)
	}
}

func TestTrayNotificationDataUsesInformationPayload(t *testing.T) {
	base := notifyIconData{Flags: nifMessage | nifIcon | nifTip, Icon: 42}
	got := trayNotificationData(base, "  New\r\nmessage  ", "  hello\r\nworld  ", niifUser|niifNoSound, base.Icon)
	if got.Flags&nifInfo == 0 {
		t.Fatal("notification data is missing NIF_INFO")
	}
	if got.InfoFlags != niifUser|niifNoSound || got.BalloonIcon != 42 {
		t.Fatalf("notification style = %#x, icon = %d", got.InfoFlags, got.BalloonIcon)
	}
	if got.Timeout != 10000 {
		t.Fatalf("notification timeout = %d, want 10000", got.Timeout)
	}
}

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

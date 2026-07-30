//go:build windows

package gui

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
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

func TestClientNotificationAvatarAcceptsImageDataURLs(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	source.SetNRGBA(0, 0, color.NRGBA{R: 20, G: 120, B: 240, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatal(err)
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
	if decoded := decodeClientAvatarImage(dataURL); decoded == nil {
		t.Fatal("PNG sender avatar data URL was rejected")
	}
	icon := createClientAvatarIcon(dataURL)
	if icon == 0 {
		t.Fatal("sender avatar could not be converted to a Windows notification icon")
	}
	clientDestroyIcon.Call(icon)
	if decoded := decodeClientAvatarImage("data:text/plain;base64,SGVsbG8="); decoded != nil {
		t.Fatal("non-image notification avatar was accepted")
	}
}

func TestClientNotificationHasDefaultUserAvatar(t *testing.T) {
	icon := createClientDefaultAvatarIcon()
	if icon == 0 {
		t.Fatal("default sender avatar could not be converted to a Windows notification icon")
	}
	clientDestroyIcon.Call(icon)
}

func TestClientKeyboardCloseDistinguishesAltF4FromTitleBarClose(t *testing.T) {
	if !isClientKeyboardCloseCommand(wmSysCommand, scClose, 0) {
		t.Fatal("Alt+F4 system close was not recognized as an exit request")
	}
	if !isClientKeyboardCloseCommand(wmSysCommand, scClose|0x0003, 0) {
		t.Fatal("system-command flag bits were not masked")
	}
	if isClientKeyboardCloseCommand(wmSysCommand, scClose, uintptr(100|(200<<16))) {
		t.Fatal("title-bar close coordinates were mistaken for Alt+F4")
	}
	if isClientKeyboardCloseCommand(wmClose, scClose, 0) {
		t.Fatal("plain WM_CLOSE was mistaken for a keyboard close command")
	}
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

func TestDesktopClientHTMLUsesBuiltInAddressAndNotificationControls(t *testing.T) {
	html := renderDesktopClientHTML("data:image/png;base64,test")
	for _, marker := range []string{
		"地址在编译时写入",
		`id="server-url"`,
		`id="retry"`,
		`id="autoStart"`,
		`id="notifications"`,
		`id="sound"`,
		"右击托盘图标可调整通知或退出客户端",
		"window.clientRetry",
		"window.clientQuit",
		"e.altKey&&(e.key==='F4'||e.keyCode===115)",
	} {
		if !strings.Contains(html, marker) {
			t.Fatalf("desktop client HTML missing %q", marker)
		}
	}
	for _, removed := range []string{`id="address"`, `id="scan"`, "window.clientConnect", "window.clientScan", "手动输入"} {
		if strings.Contains(html, removed) {
			t.Fatalf("desktop client HTML still exposes removed discovery/input UI %q", removed)
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

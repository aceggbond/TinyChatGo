//go:build windows

package gui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows/registry"

	"hfsgo/internal/appinfo"
	"hfsgo/internal/discovery"
)

const (
	clientInstanceMutexName   = `Local\LanChatGo.Client.SingleInstance.9D6D2D11`
	clientInstanceReadyName   = `Local\LanChatGo.Client.Ready.9D6D2D11`
	clientInstanceMessageName = `LanChatGo.Client.ActivateExisting.9D6D2D11`

	clientTrayShow      = 2201
	clientTraySettings  = 2202
	clientTrayScan      = 2203
	clientTrayNotify    = 2204
	clientTraySound     = 2205
	clientTrayAutoStart = 2206
	clientTrayExit      = 2207
)

type desktopClientSettings struct {
	ServerURL     string `json:"serverUrl,omitempty"`
	AutoStart     bool   `json:"autoStart"`
	Notifications bool   `json:"notifications"`
	Sound         bool   `json:"sound"`
}

type desktopClientServer struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	URL            string `json:"url"`
	ClientDownload bool   `json:"clientDownload"`
}

type desktopClientState struct {
	Version       string                `json:"version"`
	ServerURL     string                `json:"serverUrl"`
	Scanning      bool                  `json:"scanning"`
	Status        string                `json:"status"`
	Servers       []desktopClientServer `json:"servers"`
	AutoStart     bool                  `json:"autoStart"`
	Notifications bool                  `json:"notifications"`
	Sound         bool                  `json:"sound"`
}

type desktopClientController struct {
	mu            sync.RWMutex
	view          webview.WebView
	hwnd          uintptr
	logoDataURL   string
	configPath    string
	settings      desktopClientSettings
	servers       []desktopClientServer
	scanning      bool
	status        string
	trayAdded     bool
	trayFlashing  bool
	trayFlashOn   bool
	trayFlashStop chan struct{}
	exiting       bool
	lastUnread    int
	cleanupOnce   sync.Once
}

type flashWindowInfo struct {
	Size    uint32
	Window  uintptr
	Flags   uint32
	Count   uint32
	Timeout uint32
}

var (
	clientController      *desktopClientController
	clientWindowProc      = syscall.NewCallback(desktopClientWndProc)
	originalClientWndProc uintptr
	flashWindowEx         = user32.NewProc("FlashWindowEx")
)

func RunClient(logo []byte) error {
	instance, primary, err := acquireNamedSingleInstance(
		clientInstanceMutexName,
		clientInstanceReadyName,
		clientInstanceMessageName,
		5*time.Second,
	)
	if err != nil {
		return err
	}
	if !primary {
		return nil
	}
	defer instance.close()
	instanceMessage = instance.message

	dataDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("无法确定客户端配置目录：%w", err)
	}
	dataDir = filepath.Join(dataDir, "LanChatGo")
	if err = os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("无法创建客户端配置目录：%w", err)
	}
	configPath := filepath.Join(dataDir, "client.json")
	settings := loadDesktopClientSettings(configPath)
	settings.AutoStart = desktopClientAutoStartEnabled()
	configureClientWebView2TLS()

	view := webview.NewWithOptions(webview.WebViewOptions{
		Debug:     false,
		DataPath:  filepath.Join(dataDir, "WebView2"),
		AutoFocus: true,
		WindowOptions: webview.WindowOptions{
			Title:  "LanChatGo 客户端",
			Width:  1180,
			Height: 800,
			IconId: modernIconResourceID,
			Center: true,
		},
	})
	if view == nil {
		return errors.New("无法创建客户端界面，请安装或更新 Microsoft Edge WebView2 Runtime")
	}
	view.SetSize(820, 600, webview.HintMin)
	window := uintptr(view.Window())
	controller := &desktopClientController{
		view:        view,
		hwnd:        window,
		logoDataURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(logo),
		configPath:  configPath,
		settings:    settings,
		status:      "正在准备局域网自动发现…",
	}
	clientController = controller

	taskbarCreated := utf16("TaskbarCreated")
	registered, _, _ := registerMessage.Call(uintptr(unsafe.Pointer(&taskbarCreated[0])))
	taskbarCreatedMessage = uint32(registered)
	previous, _, callErr := setWindowLongPtr.Call(window, ^uintptr(3), clientWindowProc)
	if previous == 0 {
		view.Destroy()
		clientController = nil
		return fmt.Errorf("无法连接客户端窗口消息：%v", callErr)
	}
	originalClientWndProc = previous
	controller.ensureTray()

	bindings := map[string]interface{}{
		"clientGetState":      controller.getState,
		"clientConnect":       controller.connect,
		"clientScan":          controller.scan,
		"clientSetOption":     controller.setOption,
		"lanchatNotify":       controller.notify,
		"lanchatUnread":       controller.updateUnread,
		"lanchatOpenSettings": controller.showLauncher,
	}
	for name, binding := range bindings {
		if err = view.Bind(name, binding); err != nil {
			view.Destroy()
			clientController = nil
			return fmt.Errorf("注册客户端操作 %s 失败：%w", name, err)
		}
	}
	view.SetHtml(renderDesktopClientHTML(controller.logoDataURL))
	if err = instance.signalReady(); err != nil {
		view.Destroy()
		clientController = nil
		return err
	}
	if hasArgument("--autostart") {
		showWindow.Call(window, 0)
	}
	controller.scanAsync(true)
	view.Run()
	controller.cleanup()
	clientController = nil
	return nil
}

func hasArgument(want string) bool {
	for _, argument := range os.Args[1:] {
		if strings.EqualFold(strings.TrimSpace(argument), want) {
			return true
		}
	}
	return false
}

func configureClientWebView2TLS() {
	const (
		environmentName = "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"
		ignoreTLSFlag   = "--ignore-certificate-errors"
	)
	arguments := strings.TrimSpace(os.Getenv(environmentName))
	if strings.Contains(arguments, ignoreTLSFlag) {
		return
	}
	if arguments != "" {
		arguments += " "
	}
	_ = os.Setenv(environmentName, arguments+ignoreTLSFlag)
}

func defaultDesktopClientSettings() desktopClientSettings {
	return desktopClientSettings{Notifications: true, Sound: true}
}

func loadDesktopClientSettings(path string) desktopClientSettings {
	settings := defaultDesktopClientSettings()
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, &settings)
	}
	settings.ServerURL, _ = normalizeDesktopClientURL(settings.ServerURL)
	return settings
}

func (c *desktopClientController) saveSettings() error {
	c.mu.RLock()
	settings := c.settings
	c.mu.RUnlock()
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(c.configPath), ".client-settings-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(data)
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	_ = os.Remove(c.configPath)
	return os.Rename(tempPath, c.configPath)
}

func (c *desktopClientController) getState() desktopClientState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return desktopClientState{
		Version:       appinfo.Version,
		ServerURL:     c.settings.ServerURL,
		Scanning:      c.scanning,
		Status:        c.status,
		Servers:       append([]desktopClientServer(nil), c.servers...),
		AutoStart:     c.settings.AutoStart,
		Notifications: c.settings.Notifications,
		Sound:         c.settings.Sound,
	}
}

func (c *desktopClientController) connect(raw string) error {
	address, err := normalizeDesktopClientURL(raw)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.settings.ServerURL = address
	c.status = "正在连接 " + address
	c.mu.Unlock()
	if err = c.saveSettings(); err != nil {
		return fmt.Errorf("保存服务地址失败：%w", err)
	}
	c.view.Dispatch(func() {
		if clientController == c && c.view != nil {
			c.view.Navigate(address)
		}
	})
	return nil
}

func normalizeDesktopClientURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("请输入服务端地址")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", errors.New("服务端地址格式不正确")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("服务端地址只支持 HTTP 或 HTTPS")
	}
	parsed.Path = "/"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (c *desktopClientController) scan() desktopClientState {
	c.scanAsync(false)
	return c.getState()
}

func (c *desktopClientController) scanAsync(autoConnect bool) {
	c.mu.Lock()
	if c.scanning {
		c.mu.Unlock()
		return
	}
	c.scanning = true
	c.status = "正在通过局域网广播发现 LanChatGo 服务…"
	c.mu.Unlock()
	c.refreshLauncher()
	go func() {
		services, err := discovery.ScanCClass(context.Background(), 1600*time.Millisecond)
		found := make([]desktopClientServer, 0, len(services))
		for _, service := range services {
			if address := service.PreferredURL(); address != "" {
				found = append(found, desktopClientServer{
					Name:           service.Name,
					Version:        service.Version,
					URL:            address,
					ClientDownload: service.ClientDownload,
				})
			}
		}
		c.mu.Lock()
		c.servers = found
		c.scanning = false
		saved := c.settings.ServerURL
		switch {
		case err != nil:
			c.status = "自动发现不可用，可手动输入服务地址"
		case len(found) == 0:
			c.status = "未发现局域网服务，可手动输入地址或重新扫描"
		case len(found) == 1:
			c.status = "已发现 1 个服务"
		default:
			c.status = fmt.Sprintf("已发现 %d 个服务", len(found))
		}
		c.mu.Unlock()
		c.refreshLauncher()
		if !autoConnect {
			return
		}
		target := ""
		for _, item := range found {
			if strings.EqualFold(item.URL, saved) {
				target = item.URL
				break
			}
		}
		if target == "" && len(found) > 0 {
			target = found[0].URL
		}
		if target == "" {
			target = saved
		}
		if target != "" {
			_ = c.connect(target)
		}
	}()
}

func (c *desktopClientController) setOption(name string, enabled bool) (desktopClientState, error) {
	c.mu.Lock()
	switch name {
	case "notifications":
		c.settings.Notifications = enabled
	case "sound":
		c.settings.Sound = enabled
	case "autoStart":
		c.mu.Unlock()
		if err := setDesktopClientAutoStart(enabled); err != nil {
			return c.getState(), err
		}
		c.mu.Lock()
		c.settings.AutoStart = enabled
	default:
		c.mu.Unlock()
		return c.getState(), errors.New("未知的客户端设置")
	}
	c.mu.Unlock()
	if name == "notifications" && !enabled {
		c.stopTrayFlash()
	}
	if err := c.saveSettings(); err != nil {
		return c.getState(), err
	}
	return c.getState(), nil
}

func desktopClientAutoStartEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	value, _, err := key.GetStringValue("LanChatGoClient")
	return err == nil && strings.TrimSpace(value) != ""
}

func setDesktopClientAutoStart(enabled bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("无法打开开机启动配置：%w", err)
	}
	defer key.Close()
	if !enabled {
		err = key.DeleteValue("LanChatGoClient")
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := `"` + executable + `" --client --autostart`
	return key.SetStringValue("LanChatGoClient", command)
}

func (c *desktopClientController) notify(title, body, _ string) error {
	c.mu.RLock()
	enabled, sound := c.settings.Notifications, c.settings.Sound
	c.mu.RUnlock()
	if !enabled {
		return nil
	}
	if sound {
		messageBeep.Call(0x40)
	}
	c.flashTaskbar()
	c.startTrayFlash()
	c.showTrayNotification(title, body)
	return nil
}

func (c *desktopClientController) updateUnread(total int, _ string) {
	c.mu.Lock()
	increased := total > c.lastUnread
	c.lastUnread = total
	c.mu.Unlock()
	if increased && total > 0 {
		c.flashTaskbar()
	}
	if total > 0 {
		c.startTrayFlash()
	} else {
		c.stopTrayFlash()
	}
}

func (c *desktopClientController) flashTaskbar() {
	if c.hwnd == 0 {
		return
	}
	info := flashWindowInfo{Size: uint32(unsafe.Sizeof(flashWindowInfo{})), Window: c.hwnd, Flags: 3 | 12, Count: 5}
	flashWindowEx.Call(uintptr(unsafe.Pointer(&info)))
}

func (c *desktopClientController) startTrayFlash() {
	c.ensureTray()
	c.mu.Lock()
	if c.trayFlashing || c.exiting || !c.trayAdded || !c.settings.Notifications {
		c.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	c.trayFlashing = true
	c.trayFlashOn = false
	c.trayFlashStop = stop
	c.mu.Unlock()

	go func() {
		ticker := time.NewTicker(450 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				c.mu.Lock()
				if !c.trayFlashing || c.exiting || !c.trayAdded || c.trayFlashStop != stop {
					c.mu.Unlock()
					return
				}
				c.trayFlashOn = !c.trayFlashOn
				flashOn := c.trayFlashOn
				c.mu.Unlock()
				c.setTrayFlashIcon(flashOn)
			}
		}
	}()
}

func (c *desktopClientController) stopTrayFlash() {
	c.mu.Lock()
	if !c.trayFlashing {
		c.mu.Unlock()
		return
	}
	stop := c.trayFlashStop
	c.trayFlashing = false
	c.trayFlashOn = false
	c.trayFlashStop = nil
	c.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	c.setTrayFlashIcon(false)
}

func (c *desktopClientController) setTrayFlashIcon(flashOn bool) {
	c.mu.RLock()
	added, exiting := c.trayAdded, c.exiting
	c.mu.RUnlock()
	if !added || exiting || c.hwnd == 0 {
		return
	}
	data := c.trayData()
	if flashOn {
		if icon, _, _ := loadIcon.Call(0, 32513); icon != 0 {
			data.Icon = icon
		}
	}
	shellNotify.Call(1, uintptr(unsafe.Pointer(&data)))
}

func (c *desktopClientController) showLauncher() {
	if c == nil || c.view == nil {
		return
	}
	html := renderDesktopClientHTML(c.logoDataURL)
	c.view.Dispatch(func() {
		if clientController == c && c.view != nil {
			c.view.SetHtml(html)
		}
	})
}

func (c *desktopClientController) refreshLauncher() {
	if c == nil || c.view == nil {
		return
	}
	c.view.Dispatch(func() {
		if clientController == c && c.view != nil {
			c.view.Eval("window.clientRefresh && window.clientRefresh()")
		}
	})
}

func (c *desktopClientController) ensureTray() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.trayAdded || c.hwnd == 0 {
		return
	}
	data := c.trayData()
	added, _, _ := shellNotify.Call(0, uintptr(unsafe.Pointer(&data)))
	c.trayAdded = added != 0
}

func (c *desktopClientController) trayData() notifyIconData {
	instance, _, _ := getModule.Call(0)
	icon, _, _ := loadIcon.Call(instance, modernIconResourceID)
	if icon == 0 {
		icon, _, _ = loadIcon.Call(0, 32512)
	}
	data := notifyIconData{Hwnd: c.hwnd, UID: 2, Flags: 1 | 2 | 4, Callback: wmTray, Icon: icon}
	data.Size = uint32(unsafe.Sizeof(data))
	copyUTF16(data.Tip[:], "LanChatGo 客户端 - 双击显示，右击设置或退出")
	return data
}

func (c *desktopClientController) showTrayNotification(title, body string) {
	c.ensureTray()
	c.mu.RLock()
	added := c.trayAdded
	c.mu.RUnlock()
	if !added {
		return
	}
	data := c.trayData()
	data.Flags |= 0x10
	data.InfoFlags = 1 | 0x10
	data.Timeout = 10000
	copyUTF16(data.InfoTitle[:], strings.TrimSpace(title))
	copyUTF16(data.Info[:], strings.TrimSpace(body))
	shellNotify.Call(1, uintptr(unsafe.Pointer(&data)))
}

func (c *desktopClientController) removeTray() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.trayAdded || c.hwnd == 0 {
		return
	}
	data := c.trayData()
	shellNotify.Call(2, uintptr(unsafe.Pointer(&data)))
	c.trayAdded = false
}

func (c *desktopClientController) restore() {
	showWindow.Call(c.hwnd, 9)
	setForeground.Call(c.hwnd)
	setFocus.Call(c.hwnd)
}

func (c *desktopClientController) hide() {
	c.ensureTray()
	showWindow.Call(c.hwnd, 0)
}

func (c *desktopClientController) exit() {
	c.mu.Lock()
	if c.exiting {
		c.mu.Unlock()
		return
	}
	c.exiting = true
	c.mu.Unlock()
	c.stopTrayFlash()
	c.removeTray()
	destroyWindow.Call(c.hwnd)
}

func (c *desktopClientController) cleanup() {
	c.cleanupOnce.Do(func() {
		c.stopTrayFlash()
		c.removeTray()
		_ = c.saveSettings()
	})
}

func (c *desktopClientController) showTrayMenu() {
	var cursor point
	getCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	menu, _, _ := createPopupMenu.Call()
	if menu == 0 {
		return
	}
	defer destroyMenu.Call(menu)
	c.mu.RLock()
	settings := c.settings
	c.mu.RUnlock()
	appendClientMenuItem(menu, clientTrayShow, "显示聊天窗口", false)
	appendClientMenuItem(menu, clientTraySettings, "连接与通知设置", false)
	appendClientMenuItem(menu, clientTrayScan, "重新发现局域网服务", false)
	appendMenu.Call(menu, 0x800, 0, 0)
	appendClientMenuItem(menu, clientTrayNotify, "新消息通知", settings.Notifications)
	appendClientMenuItem(menu, clientTraySound, "通知声音", settings.Sound)
	appendClientMenuItem(menu, clientTrayAutoStart, "开机自动启动", settings.AutoStart)
	appendMenu.Call(menu, 0x800, 0, 0)
	appendClientMenuItem(menu, clientTrayExit, "退出客户端", false)
	setForeground.Call(c.hwnd)
	command, _, _ := trackPopupMenu.Call(menu, 0x100|2, uintptr(cursor.X), uintptr(cursor.Y), 0, c.hwnd, 0)
	postMessage.Call(c.hwnd, 0, 0, 0)
	switch command {
	case clientTrayShow:
		c.restore()
	case clientTraySettings:
		c.restore()
		c.showLauncher()
	case clientTrayScan:
		c.restore()
		c.showLauncher()
		c.scanAsync(false)
	case clientTrayNotify:
		_, _ = c.setOption("notifications", !settings.Notifications)
	case clientTraySound:
		_, _ = c.setOption("sound", !settings.Sound)
	case clientTrayAutoStart:
		_, _ = c.setOption("autoStart", !settings.AutoStart)
	case clientTrayExit:
		c.exit()
	}
}

func appendClientMenuItem(menu, id uintptr, text string, checked bool) {
	flags := uintptr(0)
	if checked {
		flags |= 0x8
	}
	label := utf16(text)
	appendMenu.Call(menu, flags, id, uintptr(unsafe.Pointer(&label[0])))
}

func desktopClientWndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	controller := clientController
	if instanceMessage != 0 && message == instanceMessage {
		if controller != nil {
			controller.restore()
		}
		return 0
	}
	if taskbarCreatedMessage != 0 && message == taskbarCreatedMessage {
		if controller != nil {
			controller.mu.Lock()
			controller.trayAdded = false
			controller.mu.Unlock()
			controller.ensureTray()
		}
		return 0
	}
	switch message {
	case wmSysCommand:
		if wParam&0xfff0 == scClose && controller != nil {
			controller.hide()
			return 0
		}
	case wmClose:
		if controller != nil {
			controller.hide()
			return 0
		}
	case wmTray:
		if controller == nil {
			return 0
		}
		switch lParam {
		case 0x203, 0x405:
			controller.restore()
		case 0x205:
			controller.showTrayMenu()
		}
		return 0
	case wmDestroy:
		if controller != nil {
			controller.cleanup()
		}
	}
	if originalClientWndProc != 0 {
		result, _, _ := callWindowProc.Call(originalClientWndProc, hwnd, uintptr(message), wParam, lParam)
		return result
	}
	result, _, _ := defWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func renderDesktopClientHTML(logoDataURL string) string {
	html := `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>LanChatGo 客户端</title><style>
:root{font-family:Inter,"Segoe UI","Microsoft YaHei UI",sans-serif;color:#1d2737;background:#f3f5f8}*{box-sizing:border-box}body{margin:0;min-height:100vh;background:#f3f5f8}.top{height:66px;display:flex;align-items:center;padding:0 26px;border-bottom:1px solid #dfe4ec;background:#fff}.logo{width:42px;height:42px;border-radius:10px}.brand{margin-left:11px;font-size:19px;font-weight:850}.version{margin-left:7px;padding:2px 7px;border-radius:99px;background:#edf3ff;color:#2f6fed;font-size:10px}.sub{margin-top:2px;color:#758196;font-size:10px}.shell{width:min(920px,calc(100% - 30px));margin:22px auto;display:grid;grid-template-columns:minmax(0,1fr) 310px;gap:14px}.card{border:1px solid #dfe4ec;border-radius:13px;background:#fff}.head{padding:18px;border-bottom:1px solid #e7ebf1}.title{font-size:18px;font-weight:820}.note{margin-top:5px;color:#758196;font-size:11px;line-height:1.6}.body{padding:16px}.status{padding:11px 13px;border-radius:9px;background:#edf3ff;color:#3869b4;font-size:11px}.servers{display:grid;gap:8px;margin-top:12px}.server{width:100%;display:grid;grid-template-columns:42px minmax(0,1fr) auto;align-items:center;gap:10px;padding:10px;border:1px solid #dfe5ee;border-radius:10px;background:#fff;text-align:left;cursor:pointer}.server:hover{border-color:#8db5f5;background:#f7faff}.server-icon{width:42px;height:42px;display:grid;place-items:center;border-radius:9px;background:#2f6fed;color:#fff;font-weight:850}.server-name{font-size:13px;font-weight:780}.server-url{margin-top:4px;color:#758196;font-size:10px}.tag{padding:3px 7px;border-radius:99px;background:#eaf8f2;color:#16835d;font-size:9px}.empty{padding:32px 12px;color:#8994a5;text-align:center;font-size:11px}.manual{display:flex;gap:8px;margin-top:13px}.input{min-width:0;flex:1;height:40px;padding:0 11px;border:1px solid #d8e0ea;border-radius:9px;outline:0}.input:focus{border-color:#75a5f4;box-shadow:0 0 0 3px rgba(47,111,237,.1)}button{height:40px;padding:0 13px;border:1px solid #d8e0ea;border-radius:9px;background:#fff;color:#34435a;cursor:pointer}.primary{border-color:#2f6fed;background:#2f6fed;color:#fff;font-weight:750}.side-title{padding:16px 17px;border-bottom:1px solid #e7ebf1;font-size:14px;font-weight:800}.option{display:flex;align-items:center;gap:12px;min-height:62px;padding:10px 16px;border-bottom:1px solid #edf0f4}.option:last-child{border-bottom:0}.copy{min-width:0;flex:1}.option-name{font-size:12px;font-weight:740}.option-note{margin-top:4px;color:#7a8698;font-size:9px;line-height:1.45}.switch{position:relative;width:42px;height:23px}.switch input{position:absolute;opacity:0}.slider{position:absolute;inset:0;border-radius:99px;background:#cbd3de}.slider:after{content:"";position:absolute;left:3px;top:3px;width:17px;height:17px;border-radius:50%;background:#fff;box-shadow:0 2px 5px rgba(0,0,0,.15);transition:.15s}.switch input:checked+.slider{background:#2f6fed}.switch input:checked+.slider:after{transform:translateX(19px)}.tray-note{margin-top:14px;padding:13px;border-radius:10px;background:#fff8e7;color:#80661e;font-size:10px;line-height:1.6}@media(max-width:720px){.shell{grid-template-columns:1fr}.top{padding:0 15px}}</style></head><body>
<header class="top"><img class="logo" src="{{LOGO}}" alt=""><div><div><span class="brand">LanChatGo 客户端</span><span class="version">v{{VERSION}}</span></div><div class="sub">自动发现局域网服务 · 原生托盘通知</div></div></header>
<main class="shell"><section class="card"><div class="head"><div class="title">连接服务端</div><div class="note">启动时只发送一次局域网发现广播，不扫描每个 IP 或端口。</div></div><div class="body"><div id="status" class="status">正在发现服务…</div><div id="servers" class="servers"></div><form id="manual" class="manual"><input id="address" class="input" placeholder="例如 http://192.168.1.10 或 https://192.168.1.10"><button class="primary">连接</button><button id="scan" type="button">重新扫描</button></form></div></section>
<aside><section class="card"><div class="side-title">客户端设置</div><label class="option"><span class="copy"><span class="option-name">开机自动启动</span><span class="option-note">登录 Windows 后自动连接上次或发现到的服务</span></span><span class="switch"><input id="autoStart" type="checkbox"><span class="slider"></span></span></label><label class="option"><span class="copy"><span class="option-name">新消息通知</span><span class="option-note">使用系统托盘通知，不受浏览器通知权限限制</span></span><span class="switch"><input id="notifications" type="checkbox"><span class="slider"></span></span></label><label class="option"><span class="copy"><span class="option-name">通知声音</span><span class="option-note">收到新消息时播放 Windows 提示音</span></span><span class="switch"><input id="sound" type="checkbox"><span class="slider"></span></span></label></section><div class="tray-note">点击窗口 × 会隐藏到托盘，不会退出。右击托盘图标可重新扫描、调整通知或退出客户端。</div></aside></main>
<script>(function(){'use strict';var state=null;function $(id){return document.getElementById(id)}function esc(v){return String(v||'').replace(/[&<>"']/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]})}function render(){if(!state)return;$('status').textContent=state.status||'等待操作';$('scan').disabled=!!state.scanning;if(document.activeElement!==$('address'))$('address').value=state.serverUrl||'';['autoStart','notifications','sound'].forEach(function(k){$(k).checked=!!state[k]});var rows=(state.servers||[]).map(function(s){return'<button class="server" type="button" data-url="'+esc(s.url)+'"><span class="server-icon">LC</span><span><span class="server-name">'+esc(s.name||'LanChatGo')+' <small>v'+esc(s.version||'未知')+'</small></span><span class="server-url">'+esc(s.url)+'</span></span><span class="tag">连接</span></button>'}).join('');$('servers').innerHTML=rows||'<div class="empty">暂未发现服务，可手动输入地址。</div>';$('servers').querySelectorAll('.server').forEach(function(b){b.onclick=function(){window.clientConnect(b.dataset.url).catch(alert)}})}async function refresh(){try{state=await window.clientGetState();render()}catch(e){}}window.clientRefresh=refresh;$('manual').onsubmit=function(e){e.preventDefault();window.clientConnect($('address').value).catch(function(e){alert(e.message||e)})};$('scan').onclick=function(){window.clientScan().then(function(s){state=s;render()})};['autoStart','notifications','sound'].forEach(function(k){$(k).onchange=function(){window.clientSetOption(k,$(k).checked).then(function(s){state=s;render()}).catch(function(e){alert(e.message||e);refresh()})}});refresh();setInterval(refresh,1000)})();</script></body></html>`
	html = strings.ReplaceAll(html, "{{LOGO}}", logoDataURL)
	html = strings.ReplaceAll(html, "{{VERSION}}", appinfo.Version)
	return html
}

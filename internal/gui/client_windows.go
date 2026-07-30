//go:build windows

package gui

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows/registry"

	"lanchatgo/internal/appinfo"
)

const (
	clientInstanceMutexName   = `Local\LanChatGo.Client.SingleInstance.9D6D2D11`
	clientInstanceReadyName   = `Local\LanChatGo.Client.Ready.9D6D2D11`
	clientInstanceMessageName = `LanChatGo.Client.ActivateExisting.9D6D2D11`

	clientTrayShow      = 2201
	clientTraySettings  = 2202
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

type desktopClientState struct {
	Version       string `json:"version"`
	ServerURL     string `json:"serverUrl"`
	Status        string `json:"status"`
	AutoStart     bool   `json:"autoStart"`
	Notifications bool   `json:"notifications"`
	Sound         bool   `json:"sound"`
}

type desktopClientController struct {
	mu                sync.RWMutex
	view              webview.WebView
	hwnd              uintptr
	logoDataURL       string
	configPath        string
	settings          desktopClientSettings
	status            string
	trayAdded         bool
	trayFlashing      bool
	trayFlashOn       bool
	trayFlashStop     chan struct{}
	trayAlertIcon     uintptr
	trayNotifyUntil   time.Time
	exiting           bool
	lastUnread        int
	notificationRoute string
	promptedUpdates   map[string]struct{}
	updateInProgress  bool
	cleanupOnce       sync.Once
}

type flashWindowInfo struct {
	Size    uint32
	Window  uintptr
	Flags   uint32
	Count   uint32
	Timeout uint32
}

type clientNotifyIconIdentifier struct {
	Size uint32
	Hwnd uintptr
	UID  uint32
	GUID [16]byte
}

type clientRect struct {
	Left, Top, Right, Bottom int32
}

var (
	clientController      *desktopClientController
	clientWindowProc      = syscall.NewCallback(desktopClientWndProc)
	originalClientWndProc uintptr
	flashWindowEx         = user32.NewProc("FlashWindowEx")
	clientWindowFromPoint = user32.NewProc("WindowFromPoint")
	clientNotifyIconRect  = shell32.NewProc("Shell_NotifyIconGetRect")
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
	settings.ServerURL, err = normalizeDesktopClientURL(appinfo.ClientServerURL)
	if err != nil {
		return fmt.Errorf("客户端内置的服务端地址无效：%w", err)
	}
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
		view:            view,
		hwnd:            window,
		logoDataURL:     "data:image/png;base64," + base64.StdEncoding.EncodeToString(logo),
		configPath:      configPath,
		settings:        settings,
		status:          "正在连接内置服务端 " + settings.ServerURL,
		trayAlertIcon:   createClientTrayAlertIcon(logo),
		promptedUpdates: make(map[string]struct{}),
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
		"clientGetState":          controller.getState,
		"clientGetAccessPassword": func() string { return appinfo.ClientAccessPassword },
		"clientRetry":             controller.connectBuiltIn,
		"clientSetOption":         controller.setOption,
		"clientCheckUpdate":       controller.checkUpdateNow,
		"clientQuit":              controller.exit,
		"lanchatNotify":           controller.notify,
		"lanchatUnread":           controller.updateUnread,
		"lanchatOpenSettings":     controller.openPortalSettings,
		"lanchatCopyText":         controller.copyText,
		"lanchatCopyImage":        controller.copyImage,
		"lanchatCopyFile":         controller.copyFile,
		"lanchatOpenExternal":     controller.openExternal,
	}
	for name, binding := range bindings {
		if err = view.Bind(name, binding); err != nil {
			view.Destroy()
			clientController = nil
			return fmt.Errorf("注册客户端操作 %s 失败：%w", name, err)
		}
	}
	view.SetHtml(renderDesktopClientHTML(controller.logoDataURL))
	controller.setNativeWindowVisible(true)
	if err = instance.signalReady(); err != nil {
		view.Destroy()
		clientController = nil
		return err
	}
	if hasArgument("--autostart") {
		showWindow.Call(window, 0)
	}
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = controller.connectBuiltIn()
	}()
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
		Status:        c.status,
		AutoStart:     c.settings.AutoStart,
		Notifications: c.settings.Notifications,
		Sound:         c.settings.Sound,
	}
}

func (c *desktopClientController) connectBuiltIn() error {
	c.mu.Lock()
	address := c.settings.ServerURL
	if address == "" {
		c.mu.Unlock()
		return errors.New("客户端没有内置服务端地址")
	}
	c.status = "正在连接 " + address
	c.mu.Unlock()
	c.view.Dispatch(func() {
		if clientController == c && c.view != nil {
			c.view.Navigate(address)
		}
	})
	go c.checkServerUpdate(address)
	return nil
}

func (c *desktopClientController) copyText(value string) error {
	return setClipboardText(HWND(c.hwnd), value)
}

func (c *desktopClientController) copyImage(dataURL string) error {
	encoded, err := decodeClipboardDataURL(dataURL)
	if err != nil {
		return err
	}
	return setClipboardImage(HWND(c.hwnd), encoded)
}

func (c *desktopClientController) copyFile(rawURL, name string) error {
	c.mu.RLock()
	serverURL := c.settings.ServerURL
	c.mu.RUnlock()
	path, err := downloadClipboardAttachment(rawURL, name, serverURL)
	if err != nil {
		return err
	}
	return setClipboardFiles(HWND(c.hwnd), []string{path})
}

func (c *desktopClientController) openExternal(rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("外部链接地址无效")
	}
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", parsed.String()).Start()
}

func addClientAccessHeader(request *http.Request) {
	if request == nil || appinfo.ClientAccessPassword == "" {
		return
	}
	request.Header.Set("X-LanChatGo-Access-Password", appinfo.ClientAccessPassword)
}

const maxClientUpdateBytes = 256 << 20

func (c *desktopClientController) checkServerUpdate(address string) {
	request, err := http.NewRequest(http.MethodHead, strings.TrimRight(address, "/")+"/", nil)
	if err != nil {
		return
	}
	request.Header.Set("User-Agent", "LanChatGo-Client/"+appinfo.Version)
	addClientAccessHeader(request)
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // #nosec G402 -- LAN servers may use the generated self-signed certificate.
	response, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return
	}
	response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		return
	}
	version := strings.TrimSpace(response.Header.Get("X-LanChatGo-Version"))
	if version == "" || compareVersionNumbers(version, appinfo.Version) <= 0 {
		return
	}
	download := strings.EqualFold(strings.TrimSpace(response.Header.Get("X-LanChatGo-Client-Download")), "true")
	if !download {
		return
	}
	key := strings.TrimRight(address, "/") + "|" + version
	c.mu.Lock()
	if c.updateInProgress {
		c.mu.Unlock()
		return
	}
	if c.promptedUpdates == nil {
		c.promptedUpdates = make(map[string]struct{})
	}
	if _, alreadyPrompted := c.promptedUpdates[key]; alreadyPrompted {
		c.mu.Unlock()
		return
	}
	c.promptedUpdates[key] = struct{}{}
	c.mu.Unlock()

	title := utf16("发现 LanChatGo 客户端更新")
	message := utf16(fmt.Sprintf(
		"服务端版本 %s，高于当前客户端 %s。\r\n\r\n是否现在下载并自动重启客户端？",
		version, appinfo.Version,
	))
	result, _, _ := messageBox.Call(
		c.hwnd,
		uintptr(unsafe.Pointer(&message[0])),
		uintptr(unsafe.Pointer(&title[0])),
		0x44,
	)
	if result != 6 {
		return
	}
	if err := c.downloadAndRestart(address, version); err != nil {
		c.mu.Lock()
		c.updateInProgress = false
		c.status = "客户端自动更新失败"
		c.mu.Unlock()
		c.refreshLauncher()
		title = utf16("客户端自动更新失败")
		message = utf16("无法完成更新：" + err.Error())
		messageBox.Call(c.hwnd, uintptr(unsafe.Pointer(&message[0])), uintptr(unsafe.Pointer(&title[0])), 0x10)
	}
}

func (c *desktopClientController) checkUpdateNow() (string, error) {
	c.mu.RLock()
	address := c.settings.ServerURL
	c.mu.RUnlock()
	if address == "" {
		return "", errors.New("尚未连接服务端")
	}
	request, err := http.NewRequest(http.MethodHead, strings.TrimRight(address, "/")+"/", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "LanChatGo-Client/"+appinfo.Version)
	addClientAccessHeader(request)
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // #nosec G402 -- LAN servers may use generated self-signed certificates.
	response, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return "", fmt.Errorf("无法连接服务端检查更新：%w", err)
	}
	response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("服务端返回状态 %d", response.StatusCode)
	}
	version := strings.TrimSpace(response.Header.Get("X-LanChatGo-Version"))
	if version == "" {
		return "服务端没有提供版本信息", nil
	}
	if compareVersionNumbers(version, appinfo.Version) <= 0 {
		return "当前已是最新版 v" + appinfo.Version, nil
	}
	if !strings.EqualFold(strings.TrimSpace(response.Header.Get("X-LanChatGo-Client-Download")), "true") {
		return "发现 v" + version + "，但服务端未开启客户端下载", nil
	}
	key := strings.TrimRight(address, "/") + "|" + version
	c.mu.Lock()
	delete(c.promptedUpdates, key)
	c.mu.Unlock()
	go c.checkServerUpdate(address)
	return "发现新版 v" + version + "，请在更新确认窗口中选择是否安装", nil
}

func (c *desktopClientController) downloadAndRestart(address, version string) error {
	c.mu.Lock()
	if c.updateInProgress {
		c.mu.Unlock()
		return errors.New("客户端更新正在进行")
	}
	c.updateInProgress = true
	c.status = "正在下载客户端更新 " + version
	c.mu.Unlock()
	c.refreshLauncher()

	requestURL := strings.TrimRight(address, "/") + "/__hfs/client/download"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "LanChatGo-Client/"+appinfo.Version)
	addClientAccessHeader(request)
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // #nosec G402 -- LAN servers may use the generated self-signed certificate.
	response, err := (&http.Client{Transport: transport, Timeout: 3 * time.Minute}).Do(request)
	if err != nil {
		return fmt.Errorf("连接更新下载地址失败：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("服务端返回状态 %d，可能未开启客户端下载", response.StatusCode)
	}
	if response.ContentLength > maxClientUpdateBytes {
		return errors.New("更新文件超过 256 MiB，已停止下载")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(executable), ".lanchatgo-client-update-*.exe")
	if err != nil {
		return fmt.Errorf("创建更新临时文件失败：%w", err)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		_ = temp.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	written, err := io.Copy(temp, io.LimitReader(response.Body, maxClientUpdateBytes+1))
	if err != nil {
		return fmt.Errorf("下载更新文件失败：%w", err)
	}
	if written == 0 || written > maxClientUpdateBytes {
		return errors.New("更新文件大小无效")
	}
	if _, err = temp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var signature [2]byte
	if _, err = io.ReadFull(temp, signature[:]); err != nil || signature != [2]byte{'M', 'Z'} {
		return errors.New("下载的更新文件不是有效的 Windows 程序")
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = scheduleDesktopClientRestart(tempPath, executable); err != nil {
		return fmt.Errorf("启动更新程序失败：%w", err)
	}
	removeTemp = false
	c.exit()
	return nil
}

func scheduleDesktopClientRestart(updatePath, executable string) error {
	script := "$update=$args[0];$target=$args[1];Start-Sleep -Milliseconds 800;Move-Item -LiteralPath $update -Destination $target -Force;Start-Process -FilePath $target -ArgumentList @('--client')"
	return exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script, updatePath, executable).Start()
}

func (c *desktopClientController) setNativeWindowVisible(visible bool) {
	if c == nil || c.view == nil {
		return
	}
	value := "false"
	if visible {
		value = "true"
	}
	c.view.Dispatch(func() {
		if clientController == c && c.view != nil {
			c.view.Eval("window.lanchatSetNativeWindowVisible && window.lanchatSetNativeWindowVisible(" + value + ")")
		}
	})
}

func (c *desktopClientController) isForegroundWindow() bool {
	if c == nil || c.hwnd == 0 {
		return false
	}
	foreground, _, _ := getForeground.Call()
	visible, _, _ := isWindowVisible.Call(c.hwnd)
	minimized, _, _ := isIconic.Call(c.hwnd)
	return foreground == c.hwnd && visible != 0 && minimized == 0
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

func (c *desktopClientController) notify(title, body, route, avatar string, mentioned, private bool) error {
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
	showBalloon := private || mentioned || !c.trayIconVisible()
	if showBalloon {
		c.mu.Lock()
		c.notificationRoute = route
		c.mu.Unlock()
		c.showTrayNotification(title, body, avatar)
	} else {
		c.startTrayFlash()
	}
	return nil
}

func (c *desktopClientController) trayIconVisible() bool {
	if c == nil || c.hwnd == 0 {
		return false
	}
	identifier := clientNotifyIconIdentifier{
		Size: uint32(unsafe.Sizeof(clientNotifyIconIdentifier{})),
		Hwnd: c.hwnd,
		UID:  2,
	}
	var bounds clientRect
	result, _, _ := clientNotifyIconRect.Call(
		uintptr(unsafe.Pointer(&identifier)),
		uintptr(unsafe.Pointer(&bounds)),
	)
	if result != 0 || bounds.Right <= bounds.Left || bounds.Bottom <= bounds.Top {
		return false
	}
	x := bounds.Left + (bounds.Right-bounds.Left)/2
	y := bounds.Top + (bounds.Bottom-bounds.Top)/2
	packed := uintptr(uint64(uint32(x)) | uint64(uint32(y))<<32)
	window, _, _ := clientWindowFromPoint.Call(packed)
	if window == 0 {
		return false
	}
	visible, _, _ := isWindowVisible.Call(window)
	return visible != 0
}

func (c *desktopClientController) updateUnread(total int, _ string) {
	c.mu.Lock()
	increased := total > c.lastUnread
	c.lastUnread = total
	c.mu.Unlock()
	if increased && total > 0 {
		c.flashTaskbar()
	}
	if total > 0 && c.trayIconVisible() {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	added, exiting, alertIcon, notifyUntil := c.trayAdded, c.exiting, c.trayAlertIcon, c.trayNotifyUntil
	if !added || exiting || c.hwnd == 0 || time.Now().Before(notifyUntil) {
		return
	}
	data := c.trayData()
	if flashOn && alertIcon != 0 {
		data.Icon = alertIcon
	}
	shellNotify.Call(nimModify, uintptr(unsafe.Pointer(&data)))
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

func (c *desktopClientController) openPortalSettings() {
	if c == nil || c.view == nil {
		return
	}
	c.view.Dispatch(func() {
		if clientController == c && c.view != nil {
			c.view.Eval("document.getElementById('portal-settings-button') ? document.getElementById('portal-settings-button').click() : void 0")
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
	c.trayAdded = registerNotifyIcon(data)
}

func (c *desktopClientController) trayData() notifyIconData {
	instance, _, _ := getModule.Call(0)
	icon, _, _ := loadIcon.Call(instance, modernIconResourceID)
	if icon == 0 {
		icon, _, _ = loadIcon.Call(0, 32512)
	}
	data := notifyIconData{Hwnd: c.hwnd, UID: 2, Flags: nifMessage | nifIcon | nifTip | nifShowTip, Callback: wmTray, Icon: icon}
	data.Size = uint32(unsafe.Sizeof(data))
	copyUTF16(data.Tip[:], "LanChatGo 客户端 - 双击显示，右击设置或退出")
	return data
}

func (c *desktopClientController) showTrayNotification(title, body, avatar string) {
	c.ensureTray()
	c.mu.Lock()
	added := c.trayAdded
	c.trayNotifyUntil = time.Now().Add(3 * time.Second)
	c.mu.Unlock()
	if !added {
		return
	}
	// Use the LanChatGo icon instead of Windows' generic information icon.
	// Sound is played explicitly by notify so the user's sound preference is
	// respected without the shell adding a second chime.
	title = strings.Join(strings.Fields(title), " ")
	body = strings.Join(strings.Fields(body), " ")
	if title == "" {
		title = "新消息"
	}
	data := c.trayData()
	balloonIcon := createClientAvatarIcon(avatar)
	if balloonIcon == 0 {
		balloonIcon = createClientDefaultAvatarIcon()
	}
	if balloonIcon == 0 {
		balloonIcon = data.Icon
	}
	customIcon := balloonIcon != data.Icon
	if customIcon {
		// Windows 10/11 can ignore hBalloonIcon for legacy tray notifications.
		// Supplying the sender avatar as both icons makes the notification use
		// the sender while the tray icon is restored immediately afterwards.
		data.Icon = balloonIcon
	}
	data = trayNotificationData(data, "LanChatGo · "+title, body, niifUser|niifNoSound|niifLargeIcon, balloonIcon)
	shown := showNotifyIconNotification(data)
	if !shown && c.readdTray() {
		data = c.trayData()
		if customIcon {
			data.Icon = balloonIcon
		}
		data = trayNotificationData(data, "LanChatGo · "+title, body, niifUser|niifNoSound|niifLargeIcon, balloonIcon)
		shown = showNotifyIconNotification(data)
	}
	if customIcon {
		c.restoreTrayApplicationIcon()
		clientDestroyIcon.Call(balloonIcon)
	}
	_ = shown
}

func (c *desktopClientController) restoreTrayApplicationIcon() {
	c.mu.RLock()
	added, exiting := c.trayAdded, c.exiting
	c.mu.RUnlock()
	if !added || exiting {
		return
	}
	data := c.trayData()
	data.Flags = nifIcon
	shellNotify.Call(nimModify, uintptr(unsafe.Pointer(&data)))
}

func (c *desktopClientController) openNotificationRoute() {
	c.mu.Lock()
	route := c.notificationRoute
	c.notificationRoute = ""
	c.mu.Unlock()
	if route == "" || c.view == nil {
		return
	}
	encoded, _ := json.Marshal(route)
	c.view.Dispatch(func() {
		if clientController == c && c.view != nil {
			c.view.Eval("window.lanchatOpenConversation && window.lanchatOpenConversation(" + string(encoded) + ")")
		}
	})
}

func (c *desktopClientController) readdTray() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hwnd == 0 || c.exiting {
		return false
	}
	stale := c.trayData()
	shellNotify.Call(nimDelete, uintptr(unsafe.Pointer(&stale)))
	c.trayAdded = registerNotifyIcon(c.trayData())
	return c.trayAdded
}

func (c *desktopClientController) removeTray() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.trayAdded || c.hwnd == 0 {
		return
	}
	data := c.trayData()
	shellNotify.Call(nimDelete, uintptr(unsafe.Pointer(&data)))
	c.trayAdded = false
}

func (c *desktopClientController) restore() {
	showWindow.Call(c.hwnd, 9)
	setForeground.Call(c.hwnd)
	setFocus.Call(c.hwnd)
	c.setNativeWindowVisible(true)
}

func (c *desktopClientController) hide() {
	c.ensureTray()
	showWindow.Call(c.hwnd, 0)
	c.setNativeWindowVisible(false)
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
	appendClientMenuItem(menu, clientTraySettings, "客户端设置", false)
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
		c.openPortalSettings()
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
	case wmSysKeyDown:
		// Alt+F4 is a deliberate exit shortcut. A title-bar X still follows
		// the normal WM_SYSCOMMAND/SC_CLOSE path below and hides to the tray.
		if wParam == vkF4 && controller != nil {
			controller.exit()
			return 0
		}
	case wmSysCommand:
		if controller != nil && isClientKeyboardCloseCommand(message, wParam, lParam) {
			controller.exit()
			return 0
		}
		if wParam&0xfff0 == scClose && controller != nil {
			controller.hide()
			return 0
		}
	case wmSize:
		if controller != nil {
			visible, _, _ := isWindowVisible.Call(hwnd)
			controller.setNativeWindowVisible(wParam != 1 && visible != 0)
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
		switch trayCallbackEvent(lParam) {
		case 0x203, 0x405:
			controller.restore()
			controller.openNotificationRoute()
		case 0x205, wmContextMenu:
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

func isClientKeyboardCloseCommand(message uint32, wParam, lParam uintptr) bool {
	return message == wmSysCommand && wParam&0xfff0 == scClose && lParam == 0
}

func renderDesktopClientHTML(logoDataURL string) string {
	html := `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>LanChatGo 客户端</title><style>
:root{font-family:Inter,"Segoe UI","Microsoft YaHei UI",sans-serif;color:#1d2737;background:#f3f5f8}*{box-sizing:border-box}body{margin:0;min-height:100vh;background:#f3f5f8}.top{height:66px;display:flex;align-items:center;padding:0 26px;border-bottom:1px solid #dfe4ec;background:#fff}.logo{width:42px;height:42px;border-radius:10px}.brand{margin-left:11px;font-size:19px;font-weight:850}.version{margin-left:7px;padding:2px 7px;border-radius:99px;background:#edf3ff;color:#2f6fed;font-size:10px}.sub{margin-top:2px;color:#758196;font-size:10px}.shell{width:min(920px,calc(100% - 30px));margin:22px auto;display:grid;grid-template-columns:minmax(0,1fr) 310px;gap:14px}.card{border:1px solid #dfe4ec;border-radius:13px;background:#fff}.head{padding:18px;border-bottom:1px solid #e7ebf1}.title{font-size:18px;font-weight:820}.note{margin-top:5px;color:#758196;font-size:11px;line-height:1.6}.body{padding:16px}.status{padding:11px 13px;border-radius:9px;background:#edf3ff;color:#3869b4;font-size:11px}.servers{display:grid;gap:8px;margin-top:12px}.server{width:100%;display:grid;grid-template-columns:42px minmax(0,1fr) auto;align-items:center;gap:10px;padding:10px;border:1px solid #dfe5ee;border-radius:10px;background:#fff;text-align:left;cursor:pointer}.server:hover{border-color:#8db5f5;background:#f7faff}.server-icon{width:42px;height:42px;display:grid;place-items:center;border-radius:9px;background:#2f6fed;color:#fff;font-weight:850}.server-name{font-size:13px;font-weight:780}.server-url{margin-top:4px;color:#758196;font-size:10px}.tag{padding:3px 7px;border-radius:99px;background:#eaf8f2;color:#16835d;font-size:9px}.empty{padding:32px 12px;color:#8994a5;text-align:center;font-size:11px}.manual{display:flex;gap:8px;margin-top:13px}.input{min-width:0;flex:1;height:40px;padding:0 11px;border:1px solid #d8e0ea;border-radius:9px;outline:0}.input:focus{border-color:#75a5f4;box-shadow:0 0 0 3px rgba(47,111,237,.1)}button{height:40px;padding:0 13px;border:1px solid #d8e0ea;border-radius:9px;background:#fff;color:#34435a;cursor:pointer}.primary{border-color:#2f6fed;background:#2f6fed;color:#fff;font-weight:750}.side-title{padding:16px 17px;border-bottom:1px solid #e7ebf1;font-size:14px;font-weight:800}.option{display:flex;align-items:center;gap:12px;min-height:62px;padding:10px 16px;border-bottom:1px solid #edf0f4}.option:last-child{border-bottom:0}.copy{min-width:0;flex:1}.option-name{font-size:12px;font-weight:740}.option-note{margin-top:4px;color:#7a8698;font-size:9px;line-height:1.45}.switch{position:relative;width:42px;height:23px}.switch input{position:absolute;opacity:0}.slider{position:absolute;inset:0;border-radius:99px;background:#cbd3de}.slider:after{content:"";position:absolute;left:3px;top:3px;width:17px;height:17px;border-radius:50%;background:#fff;box-shadow:0 2px 5px rgba(0,0,0,.15);transition:.15s}.switch input:checked+.slider{background:#2f6fed}.switch input:checked+.slider:after{transform:translateX(19px)}.tray-note{margin-top:14px;padding:13px;border-radius:10px;background:#fff8e7;color:#80661e;font-size:10px;line-height:1.6}@media(max-width:720px){.shell{grid-template-columns:1fr}.top{padding:0 15px}}</style></head><body>
<header class="top"><img class="logo" src="{{LOGO}}" alt=""><div><div><span class="brand">LanChatGo 客户端</span><span class="version">v{{VERSION}}</span></div><div class="sub">固定服务端 · 原生托盘通知</div></div></header>
<main class="shell"><section class="card"><div class="head"><div class="title">内置服务端</div><div class="note">客户端地址在编译时写入，启动后会直接连接，不进行局域网探测，也不允许手动更改。</div></div><div class="body"><div id="status" class="status">正在连接内置服务端…</div><div class="server"><span class="server-icon">LC</span><span><span class="server-name">固定连接地址</span><span id="server-url" class="server-url"></span></span><button id="retry" class="primary" type="button">重新连接</button></div></div></section>
<aside><section class="card"><div class="side-title">设置</div><label class="option"><span class="copy"><span class="option-name">开机自动启动</span><span class="option-note">登录 Windows 后自动连接内置服务端</span></span><span class="switch"><input id="autoStart" type="checkbox"><span class="slider"></span></span></label><label class="option"><span class="copy"><span class="option-name">新消息通知</span><span class="option-note">使用系统托盘通知，不受浏览器通知权限限制</span></span><span class="switch"><input id="notifications" type="checkbox"><span class="slider"></span></span></label><label class="option"><span class="copy"><span class="option-name">通知声音</span><span class="option-note">收到新消息时播放 Windows 提示音</span></span><span class="switch"><input id="sound" type="checkbox"><span class="slider"></span></span></label></section><div class="tray-note">点击窗口 × 会隐藏到托盘，不会退出。右击托盘图标可调整通知或退出客户端。</div></aside></main>
<script>(function(){'use strict';var state=null;function $(id){return document.getElementById(id)}function render(){if(!state)return;$('status').textContent=state.status||'等待操作';$('server-url').textContent=state.serverUrl||'构建地址缺失';['autoStart','notifications','sound'].forEach(function(k){$(k).checked=!!state[k]})}async function refresh(){try{state=await window.clientGetState();render()}catch(e){}}window.clientRefresh=refresh;$('retry').onclick=function(){window.clientRetry().catch(function(e){alert(e.message||e)})};['autoStart','notifications','sound'].forEach(function(k){$(k).onchange=function(){window.clientSetOption(k,$(k).checked).then(function(s){state=s;render()}).catch(function(e){alert(e.message||e);refresh()})}});refresh();setInterval(refresh,1000)})();</script></body></html>`
	html = strings.Replace(html, "</body></html>", `<script>(function(){window.addEventListener('keydown',function(e){if(e.altKey&&(e.key==='F4'||e.keyCode===115)){e.preventDefault();if(window.clientQuit)window.clientQuit()}})})();</script></body></html>`, 1)
	html = strings.ReplaceAll(html, "{{LOGO}}", logoDataURL)
	html = strings.ReplaceAll(html, "{{VERSION}}", appinfo.Version)
	return html
}

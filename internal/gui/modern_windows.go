//go:build windows && !client

package gui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview "github.com/jchv/go-webview2"

	"lanchatgo/internal/database"
	"lanchatgo/internal/server"
)

// rsrc stores the icon group at resource ID 1. ID 2 is the individual image
// inside that group and cannot be loaded as a window/tray icon.
const modernIconResourceID = 1

type modernAddress struct {
	Host  string `json:"host"`
	Label string `json:"label"`
}

type modernShare struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	Size      string `json:"size"`
	URL       string `json:"url"`
	Temporary bool   `json:"temporary"`
}

type modernConversation struct {
	ID               string                `json:"id"`
	Name             string                `json:"name"`
	Online           bool                  `json:"online"`
	MessageCount     int                   `json:"messageCount"`
	UserMessageCount int                   `json:"userMessageCount"`
	Client           server.ChatClientInfo `json:"client"`
	LastMessage      string                `json:"lastMessage"`
	LastMessageID    string                `json:"lastMessageId"`
	LastSender       string                `json:"lastSender"`
	LastAt           time.Time             `json:"lastAt"`
}

type modernUser struct {
	IP            string                `json:"ip"`
	Username      string                `json:"username,omitempty"`
	AccountStatus server.AccountStatus  `json:"accountStatus,omitempty"`
	Name          string                `json:"name"`
	Avatar        string                `json:"avatar,omitempty"`
	DisplayName   string                `json:"displayName"`
	SearchKey     string                `json:"searchKey"`
	Online        bool                  `json:"online"`
	Blacklisted   bool                  `json:"blacklisted"`
	FirstSeen     time.Time             `json:"firstSeen"`
	LastSeen      time.Time             `json:"lastSeen"`
	Client        server.ChatClientInfo `json:"client"`
}

type modernState struct {
	Running       bool                   `json:"running"`
	Transitioning bool                   `json:"transitioning"`
	Address       string                 `json:"address"`
	HTTPSAddress  string                 `json:"httpsAddress"`
	Certificate   HTTPSCertificateStatus `json:"certificate"`
	StatusText    string                 `json:"statusText"`
	LastError     string                 `json:"lastError,omitempty"`
	OnlineCount   int                    `json:"onlineCount"`
	Settings      persistedSettings      `json:"settings"`
	Addresses     []modernAddress        `json:"addresses"`
	Shares        []modernShare          `json:"shares"`
	Conversations []modernConversation   `json:"conversations"`
	Groups        []server.ChatGroup     `json:"groups"`
	Users         []modernUser           `json:"users"`
	Logs          string                 `json:"logs"`
	ConfigPath    string                 `json:"configPath"`
}

type modernController struct {
	mu             sync.RWMutex
	view           webview.WebView
	srv            *server.Server
	db             *database.DB
	log            *safeBuffer
	settings       persistedSettings
	settingsPath   string
	sharesPath     string
	databasePath   string
	tempUploadDir  string
	certificateDir string
	addresses      []modernAddress
	activePage     string
	selectedChat   string
	running        bool
	transition     bool
	lastError      string
	exiting        bool
	sharesSaveMu   sync.Mutex
	cleanupOnce    sync.Once
	dropWindow     HWND
}

var (
	modern                *modernController
	modernWindowProc      = syscall.NewCallback(modernWndProc)
	originalModernWndProc uintptr
)

func Run(logo, donation []byte) error {
	instance, primary, err := acquireSingleInstance()
	if err != nil {
		return err
	}
	if !primary {
		return nil
	}
	defer instance.close()
	instanceMessage = instance.message

	executable, err := os.Executable()
	if err != nil {
		return err
	}
	baseDir := filepath.Dir(executable)
	settingsPath := filepath.Join(baseDir, settingsFileName)
	sharesPath := filepath.Join(baseDir, "hfsgo.json")
	databasePath := filepath.Join(baseDir, "lanchatgo.db")
	legacyDatabasePath := filepath.Join(baseDir, "hfs-go.db")
	if _, statErr := os.Stat(databasePath); errors.Is(statErr, os.ErrNotExist) {
		if _, legacyErr := os.Stat(legacyDatabasePath); legacyErr == nil {
			if renameErr := os.Rename(legacyDatabasePath, databasePath); renameErr != nil {
				return fmt.Errorf("迁移旧版数据库失败：%w", renameErr)
			}
		}
	}
	store, err := database.Open(databasePath)
	if err != nil {
		return fmt.Errorf("无法创建或打开持久化数据库：%w", err)
	}
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	settings := defaultPersistedSettings()
	settingsFound, settingsErr := store.LoadSettings(&settings)
	if settingsErr == nil && !settingsFound {
		settings, settingsErr = loadPersistedSettings(settingsPath)
		if settingsErr == nil {
			settingsErr = store.SaveSettings(settings)
		}
	}

	buffer := &safeBuffer{}
	if migrationErr := migrateLegacyHTTPSCertificate(store, baseDir); migrationErr != nil {
		_, _ = fmt.Fprintf(buffer, "%s 旧 HTTPS 证书迁移失败：%v\n", time.Now().Format("2006/01/02 15:04:05"), migrationErr)
	}
	srv := server.New(buffer)
	var persistedShares []server.Share
	sharesFound, sharesErr := store.LoadShares(&persistedShares)
	if sharesErr == nil && sharesFound {
		srv.ReplaceShares(persistedShares)
	} else if sharesErr == nil {
		if loadErr := srv.Load(sharesPath); loadErr != nil {
			sharesErr = loadErr
		} else {
			sharesErr = store.SaveShares(srv.Shares())
		}
	}
	if sharesErr != nil {
		_, _ = fmt.Fprintf(buffer, "%s 分享列表读取失败：%v\n", time.Now().Format("2006/01/02 15:04:05"), sharesErr)
	} else {
		_ = removeLegacyDataFile(sharesPath, baseDir, "hfsgo.json")
	}
	if settingsErr != nil {
		_, _ = fmt.Fprintf(buffer, "%s 配置文件无效，已使用安全默认值：%v\n", time.Now().Format("2006/01/02 15:04:05"), settingsErr)
	}
	settings.AllowUpload = false
	settings.AllowDownload = false
	settings.AllowChat = true
	settings.GroupChat = false
	srv.SetAccess(settings.Password, false, false, false)
	srv.SetHTTPSRedirect(settings.RedirectToHTTPS, settings.AccessHost, settings.HTTPSPort)
	srv.SetChatEnabled(true)
	srv.SetGroupChatEnabled(settings.GroupChat)
	srv.SetUserGroupCreationEnabled(settings.AllowGroupChat)
	srv.SetUserListEnabled(settings.ShowUserList)
	srv.SetPrivateMessagesEnabled(settings.AllowPrivateChat)
	srv.SetClientDownloadEnabled(settings.AllowClientDownload)
	if err = srv.SetPersistence(store); err != nil {
		return fmt.Errorf("读取聊天与用户数据失败：%w", err)
	}
	srv.SetAccountApprovalRequired(settings.RequireAccountApproval)
	srv.SetBrandLogo(logo)
	tempUploadDir := windowsTemporaryUploadDir()
	if err = srv.SetFallbackUploadDir(tempUploadDir); err != nil {
		_, _ = fmt.Fprintf(buffer, "%s 默认上传目录不可用（%s）：%v\n", time.Now().Format("2006/01/02 15:04:05"), tempUploadDir, err)
	}

	addresses, selected := availableAccessAddresses()
	addressOptions := make([]modernAddress, 0, len(addresses))
	selectedHost := ""
	for index, item := range addresses {
		addressOptions = append(addressOptions, modernAddress{Host: item.host, Label: item.label})
		if index == selected {
			selectedHost = item.host
		}
	}
	if !modernHostAvailable(settings.AccessHost, addressOptions) {
		settings.AccessHost = selectedHost
	}
	if settings.AccessHost == "" {
		settings.AccessHost = "127.0.0.1"
	}
	if err = store.SaveSettings(settings); err != nil {
		_, _ = fmt.Fprintf(buffer, "%s 数据库配置写入失败：%v\n", time.Now().Format("2006/01/02 15:04:05"), err)
	} else {
		_ = removeLegacyDataFile(settingsPath, baseDir, settingsFileName)
	}

	view := webview.NewWithOptions(webview.WebViewOptions{
		Debug:     false,
		DataPath:  filepath.Join(os.TempDir(), "lanchatgo-webview2"),
		AutoFocus: true,
		WindowOptions: webview.WindowOptions{
			Title:  "LCGS - LanChatGoServer",
			Width:  1240,
			Height: 820,
			IconId: modernIconResourceID,
			Center: true,
		},
	})
	if view == nil {
		return errors.New("无法创建现代后台界面，请安装或更新 Microsoft Edge WebView2 Runtime")
	}
	view.SetSize(980, 680, webview.HintMin)
	window := uintptr(view.Window())

	controller := &modernController{
		view:           view,
		srv:            srv,
		db:             store,
		log:            buffer,
		settings:       settings,
		settingsPath:   settingsPath,
		sharesPath:     sharesPath,
		databasePath:   databasePath,
		tempUploadDir:  tempUploadDir,
		certificateDir: baseDir,
		addresses:      addressOptions,
		activePage:     "users",
	}
	modern = controller
	srv.SetShareChangeNotifier(func() {
		if saveErr := controller.saveShares(); saveErr != nil {
			_, _ = fmt.Fprintf(buffer, "%s 上传分享列表保存失败：%v\n", time.Now().Format("2006/01/02 15:04:05"), saveErr)
		}
		modernRefresh()
	})

	app = &appState{
		hwnd:             HWND(window),
		srv:              srv,
		log:              buffer,
		config:           databasePath,
		seenChatMessages: make(map[string]struct{}),
		notifyNewVisitor: settings.NotifyNewVisitor,
		notifyNewMessage: settings.NotifyNewMessage,
	}
	buffer.hwnd = HWND(window)
	rememberChatMessages(srv.ChatOverview())

	taskbarCreated := utf16("TaskbarCreated")
	registered, _, _ := registerMessage.Call(uintptr(unsafe.Pointer(&taskbarCreated[0])))
	taskbarCreatedMessage = uint32(registered)
	previous, _, callErr := setWindowLongPtr.Call(window, ^uintptr(3), modernWindowProc)
	if previous == 0 {
		view.Destroy()
		return fmt.Errorf("无法连接现代窗口消息：%v", callErr)
	}
	originalModernWndProc = previous
	dragAccept.Call(window, 1)
	ensureTray()

	srv.SetChatNotifier(func() {
		if modern != nil {
			postMessage.Call(window, wmChat, 0, 0)
		}
	})
	srv.SetVisitorNotifier(queueModernVisitor)

	bindings := map[string]interface{}{
		"hfsGetState":            controller.getState,
		"hfsSaveSettings":        controller.saveSettings,
		"hfsToggleServer":        controller.toggleServer,
		"hfsGenerateCertificate": controller.generateCertificate,
		"hfsCopyText":            controller.copyText,
		"hfsClearLogs":           controller.clearLogs,
		"hfsOpenChatAttachment":  controller.openChatAttachment,
		"hfsListArchives":        controller.listArchives,
		"hfsDeleteArchive":       controller.deleteArchive,
		"hfsGetArchiveThumbnail": controller.archiveThumbnail,
		"hfsRemoveVisitor":       controller.removeVisitor,
		"hfsSetUserName":         controller.setUserName,
		"hfsSetUserBlacklisted":  controller.setUserBlacklisted,
		"hfsSetAccountStatus":    controller.setAccountStatus,
		"hfsRenameGroup":         controller.renameGroup,
		"hfsRemoveGroupMember":   controller.removeGroupMember,
		"hfsDeleteGroup":         controller.deleteGroup,
		"hfsClearChatHistory":    controller.clearChatHistory,
		"hfsClearUsers":          controller.clearUsers,
		"hfsSetActivePage":       controller.setActivePage,
		"hfsSetSelectedChat":     controller.setSelectedChat,
		"hfsCheckUpdate":         controller.checkUpdate,
		"hfsOpenProjectURL":      controller.openProjectURL,
	}
	for name, binding := range bindings {
		if err = view.Bind(name, binding); err != nil {
			view.Destroy()
			return fmt.Errorf("注册界面操作 %s 失败：%w", name, err)
		}
	}

	if err = instance.signalReady(); err != nil {
		view.Destroy()
		return err
	}
	view.SetHtml(renderModernHTML(logo, donation))
	view.Run()
	controller.cleanup()
	modern = nil
	app = nil
	return nil
}

func modernHostAvailable(host string, addresses []modernAddress) bool {
	for _, item := range addresses {
		if item.Host == host {
			return true
		}
	}
	return false
}

func removeLegacyDataFile(filename, baseDir, expectedName string) error {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(baseDir)
	if err != nil {
		return err
	}
	if !strings.EqualFold(filepath.Dir(absolute), root) || !strings.EqualFold(filepath.Base(absolute), expectedName) {
		return errors.New("拒绝删除不安全的旧版数据文件路径")
	}
	err = os.Remove(absolute)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func migrateLegacyHTTPSCertificate(store *database.DB, baseDir string) error {
	if store == nil {
		return nil
	}
	if _, found, err := store.LoadCertificateBundle(); err != nil {
		return err
	} else if found {
		files := httpsCertificateFilePaths(baseDir)
		for _, path := range []string{files.caCert, files.caKey, files.cert, files.certKey} {
			_ = os.Remove(path)
		}
		return nil
	}
	files := httpsCertificateFilePaths(baseDir)
	caCert, err := os.ReadFile(files.caCert)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	caKey, err := os.ReadFile(files.caKey)
	if err != nil {
		return err
	}
	cert, err := os.ReadFile(files.cert)
	if err != nil {
		return err
	}
	key, err := os.ReadFile(files.certKey)
	if err != nil {
		return err
	}
	bundle := database.CertificateBundle{CACertPEM: caCert, CAKeyPEM: caKey, CertPEM: cert, KeyPEM: key}
	if !inspectHTTPSCertificateBundle(bundle, "").Available {
		return errors.New("旧证书文件校验失败")
	}
	if err = store.SaveCertificateBundle(bundle); err != nil {
		return err
	}
	for _, path := range []string{files.caCert, files.caKey, files.cert, files.certKey} {
		_ = os.Remove(path)
	}
	return nil
}

func queueModernVisitor(info server.ChatClientInfo) {
	if app == nil || app.hwnd == 0 {
		return
	}
	app.visitorMu.Lock()
	if len(app.pendingVisitors) >= 32 {
		app.pendingVisitors = append([]server.ChatClientInfo(nil), app.pendingVisitors[len(app.pendingVisitors)-31:]...)
	}
	app.pendingVisitors = append(app.pendingVisitors, info)
	if app.visitorPostPending {
		app.visitorMu.Unlock()
		return
	}
	app.visitorPostPending = true
	window := app.hwnd
	app.visitorMu.Unlock()
	time.AfterFunc(400*time.Millisecond, func() {
		postMessage.Call(uintptr(window), wmVisitor, 0, 0)
	})
}

func modernWndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	if instanceMessage != 0 && message == instanceMessage {
		restoreFromTray()
		return 0
	}
	if taskbarCreatedMessage != 0 && message == taskbarCreatedMessage {
		if app != nil {
			app.trayAdded = false
			ensureTray()
		}
		return 0
	}
	switch message {
	case wmSysCommand:
		if isKeyboardCloseCommand(message, wParam, lParam) {
			if modern != nil {
				modern.cleanup()
			}
			destroyWindow.Call(hwnd)
			return 0
		}
	case wmClose:
		if modern != nil && !modern.isExiting() {
			minimizeToTray()
			return 0
		}
	case wmLog:
		modernRefresh()
		return 0
	case wmChat:
		if app != nil {
			handleChatUpdate()
		}
		modernRefresh()
		return 0
	case wmVisitor:
		if app != nil {
			handleVisitorNotifications()
		}
		modernRefresh()
		return 0
	case wmDropFiles:
		_, _, paths := consumeDroppedPaths(wParam)
		handleModernShareDrop(paths)
		return 0
	case wmTray:
		switch trayCallbackEvent(lParam) {
		case 0x203, 0x405:
			restoreFromTray()
		case 0x205, wmContextMenu:
			showTrayMenu()
		}
		return 0
	case wmDestroy:
		if modern != nil {
			modern.cleanup()
		}
	}
	if originalModernWndProc != 0 {
		result, _, _ := callWindowProc.Call(originalModernWndProc, hwnd, uintptr(message), wParam, lParam)
		return result
	}
	result, _, _ := defWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func (m *modernController) setActivePage(page string) {
	if page != "users" && page != "groups" && page != "archives" && page != "settings" {
		return
	}
	m.mu.Lock()
	m.activePage = page
	m.mu.Unlock()
	if page == "settings" {
		m.setNativeDropZone(0, 0, 0, 0, 1)
	}
}

func (m *modernController) setSelectedChat(id string) {
	m.mu.Lock()
	m.selectedChat = strings.TrimSpace(id)
	m.mu.Unlock()
}

func (m *modernController) dropTarget() (string, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activePage, m.selectedChat
}

// acceptsShareDrop is retained for the file-page behaviour covered by older
// callers; chat drops are routed separately through dropTarget.
func (m *modernController) acceptsShareDrop() bool {
	page, _ := m.dropTarget()
	return page == "files"
}

func (m *modernController) isExiting() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.exiting
}

func (m *modernController) cleanup() {
	m.cleanupOnce.Do(func() {
		m.mu.Lock()
		m.exiting = true
		settings := m.settings
		m.mu.Unlock()
		m.srv.SetChatNotifier(nil)
		m.srv.SetVisitorNotifier(nil)
		m.srv.SetShareChangeNotifier(nil)
		_ = m.srv.Stop()
		_ = m.saveShares()
		_ = m.saveSettingsData(settings)
		removeTray()
	})
}

func (m *modernController) saveShares() error {
	m.sharesSaveMu.Lock()
	defer m.sharesSaveMu.Unlock()
	if m.db != nil {
		return m.db.SaveShares(m.srv.Shares())
	}
	return m.srv.Save(m.sharesPath)
}

func (m *modernController) saveSettingsData(settings persistedSettings) error {
	if m.db != nil {
		return m.db.SaveSettings(settings)
	}
	return savePersistedSettings(m.settingsPath, settings)
}

func modernRefresh() {
	if modern == nil || modern.view == nil {
		return
	}
	modern.view.Dispatch(func() {
		if modern != nil && modern.view != nil {
			modern.view.Eval("window.hfsRefresh && window.hfsRefresh()")
		}
	})
}

func (m *modernController) getState() modernState {
	m.mu.RLock()
	settings := m.settings
	running := m.running
	transition := m.transition
	lastError := m.lastError
	addresses := append([]modernAddress(nil), m.addresses...)
	m.mu.RUnlock()

	address := ""
	httpsAddress := ""
	statusText := "服务未启动"
	if transition {
		statusText = "正在切换服务状态…"
	} else if running {
		address = modernServerAddressForScheme("http", settings.AccessHost, settings.Port)
		if settings.HTTPSPort != "" {
			httpsAddress = modernServerAddressForScheme("https", settings.AccessHost, settings.HTTPSPort)
		}
		statusText = "服务正在运行"
	}
	baseAddress := address
	if httpsAddress != "" {
		baseAddress = httpsAddress
	}
	certificate := m.certificateStatus(settings.AccessHost)
	return modernState{
		Running:       running,
		Transitioning: transition,
		Address:       address,
		HTTPSAddress:  httpsAddress,
		Certificate:   certificate,
		StatusText:    statusText,
		LastError:     lastError,
		OnlineCount:   m.srv.ChatOnlineCount(),
		Settings:      settings,
		Addresses:     addresses,
		Shares:        m.modernShares(baseAddress),
		Conversations: m.modernConversations(),
		Groups:        m.srv.ChatGroups(),
		Users:         m.modernUsers(),
		Logs:          m.persistentLogs(),
		ConfigPath:    firstNonEmpty(m.databasePath, m.settingsPath),
	}
}

func modernServerAddress(host, port string) string {
	return modernServerAddressForScheme("http", host, port)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func modernServerAddressForScheme(scheme, host, port string) string {
	if scheme == "" {
		scheme = "http"
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "80"
	}
	return (&url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port)}).String()
}

func (m *modernController) httpsCertificateDir() string {
	if strings.TrimSpace(m.certificateDir) != "" {
		return m.certificateDir
	}
	if strings.TrimSpace(m.settingsPath) != "" {
		return filepath.Dir(m.settingsPath)
	}
	return ""
}

func (m *modernController) certificateStatus(host string) HTTPSCertificateStatus {
	if m.db != nil {
		bundle, ok, err := m.db.LoadCertificateBundle()
		if err != nil {
			return HTTPSCertificateStatus{Message: "读取数据库中的 HTTPS 证书失败：" + err.Error()}
		}
		if ok {
			return inspectHTTPSCertificateBundle(bundle, host)
		}
		return HTTPSCertificateStatus{Message: "尚未生成 HTTPS 证书"}
	}
	if certificateDir := m.httpsCertificateDir(); certificateDir != "" {
		return inspectHTTPSCertificateForHost(certificateDir, host)
	}
	return HTTPSCertificateStatus{Message: "尚未生成 HTTPS 证书"}
}

func (m *modernController) startWithCertificate(httpListen, httpsListen string, bundle database.CertificateBundle) (server.ListenAddresses, error) {
	return m.srv.StartWithHTTPSPEM(httpListen, httpsListen, bundle.CertPEM, bundle.KeyPEM)
}

func windowsTemporaryUploadDir() string {
	systemRoot := strings.TrimSpace(os.Getenv("SystemRoot"))
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	return filepath.Clean(filepath.Join(systemRoot, "Temp"))
}

func (m *modernController) modernShares(baseAddress string) []modernShare {
	items := m.srv.Shares()
	result := make([]modernShare, 0, len(items))
	for index, item := range items {
		kind := "文件"
		size := "—"
		info, infoErr := os.Stat(item.Path)
		if infoErr == nil {
			if info.IsDir() {
				kind = "文件夹"
			} else {
				size = formatFileSize(info.Size())
			}
		}
		itemURL := ""
		if baseAddress != "" {
			itemURL = strings.TrimRight(baseAddress, "/") + "/" + url.PathEscape(item.Name)
		}
		result = append(result, modernShare{
			Index:     index,
			Name:      item.Name,
			Path:      item.Path,
			Kind:      kind,
			Size:      size,
			URL:       itemURL,
			Temporary: item.ManagedTemporary && pathDirectlyInsideDirectory(item.Path, m.tempUploadDir) && (infoErr != nil || !info.IsDir()),
		})
	}
	return result
}

func formatFileSize(size int64) string {
	const unit = int64(1024)
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	divisor, exponent := unit, 0
	for value := size / unit; value >= unit && exponent < 4; value /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(divisor), "KMGTPE"[exponent])
}

func (m *modernController) modernConversations() []modernConversation {
	items := append(m.srv.ChatOverview(), m.srv.ChatAdministratorOverview()...)
	result := make([]modernConversation, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, exists := seen[item.ID]; exists {
			continue
		}
		seen[item.ID] = struct{}{}
		row := modernConversation{
			ID:           item.ID,
			Name:         item.Name,
			Online:       item.Online,
			MessageCount: len(item.Messages),
			Client:       item.Client,
		}
		for _, message := range item.Messages {
			if message.Sender != "admin" {
				row.UserMessageCount++
			}
		}
		if count := len(item.Messages); count > 0 {
			last := item.Messages[count-1]
			row.LastAt = last.SentAt
			row.LastMessage = chatMessageSummary(last)
			row.LastMessageID = last.ID
			row.LastSender = last.Sender
		}
		result = append(result, row)
	}
	return result
}

func (m *modernController) modernUsers() []modernUser {
	profiles := make(map[string]server.ChatUser)
	for _, user := range m.srv.ChatUsers() {
		profiles[user.IP] = user
	}
	accounts := m.srv.Accounts()
	result := make([]modernUser, 0, len(accounts)+len(profiles))
	seen := make(map[string]struct{}, len(accounts))
	for _, account := range accounts {
		user, hasProfile := profiles[account.ID]
		displayName := account.Username
		name, avatar, blacklisted := "", "", false
		firstSeen, lastSeen := account.CreatedAt, account.LastLoginAt
		client := server.ChatClientInfo{IP: account.LastIP}
		if hasProfile {
			name, avatar, blacklisted = user.Name, user.Avatar, user.Blacklisted
			firstSeen, lastSeen, client = user.FirstSeen, user.LastSeen, user.Client
			if strings.TrimSpace(name) != "" && !strings.EqualFold(name, account.Username) {
				displayName += " (" + name + ")"
			}
		}
		result = append(result, modernUser{
			IP: account.ID, Username: account.Username, AccountStatus: account.Status,
			Name: name, Avatar: avatar, DisplayName: displayName,
			SearchKey: server.ChatUserSearchKey(account.Username+" "+account.LastIP, name),
			Online:    m.srv.ChatIdentityOnline(account.ID), Blacklisted: blacklisted,
			FirstSeen: firstSeen, LastSeen: lastSeen, Client: client,
		})
		seen[account.ID] = struct{}{}
	}
	// Keep legacy IP profiles visible so old history can still be inspected
	// after upgrading, but all new registrations use account identities.
	for id, user := range profiles {
		if _, exists := seen[id]; exists {
			continue
		}
		displayName := firstNonEmpty(user.Name, id)
		result = append(result, modernUser{
			IP: id, Name: user.Name, Avatar: user.Avatar, DisplayName: displayName,
			SearchKey: server.ChatUserSearchKey(id, user.Name),
			Online:    m.srv.ChatIdentityOnline(id), Blacklisted: user.Blacklisted,
			FirstSeen: user.FirstSeen, LastSeen: user.LastSeen, Client: user.Client,
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		pendingI := result[i].AccountStatus == server.AccountStatusPending
		pendingJ := result[j].AccountStatus == server.AccountStatusPending
		if pendingI != pendingJ {
			return pendingI
		}
		if result[i].Online != result[j].Online {
			return result[i].Online
		}
		return result[i].LastSeen.After(result[j].LastSeen)
	})
	return result
}

func (m *modernController) setAccountStatus(id, status string) (modernState, error) {
	if err := m.srv.SetAccountStatus(id, server.AccountStatus(strings.TrimSpace(status))); err != nil {
		return m.getState(), err
	}
	modernRefresh()
	return m.getState(), nil
}

func (m *modernController) persistentLogs() string {
	if m.db == nil {
		return m.log.String()
	}
	records, err := m.db.ListAccessRecords(2000)
	if err != nil {
		return m.log.String() + "\n数据库访问记录读取失败：" + err.Error()
	}
	var builder strings.Builder
	for _, record := range records {
		builder.WriteString(record.At.Local().Format("2006/01/02 15:04:05"))
		if record.Username != "" {
			builder.WriteString("  账号=")
			builder.WriteString(record.Username)
		}
		builder.WriteString("  IP=")
		builder.WriteString(record.IP)
		builder.WriteString("  操作=")
		builder.WriteString(record.Operation)
		if record.Method != "" {
			builder.WriteString("  请求=")
			builder.WriteString(record.Method)
			builder.WriteByte(' ')
			builder.WriteString(record.Path)
		}
		if record.Status != 0 {
			builder.WriteString("  状态=")
			builder.WriteString(strconv.Itoa(record.Status))
		}
		if record.Duration != "" {
			builder.WriteString("  用时=")
			builder.WriteString(record.Duration)
		}
		builder.WriteByte('\n')
	}
	diagnosticLines := make([]string, 0)
	for _, line := range strings.Split(m.log.String(), "\n") {
		if strings.Contains(line, "IP=") && (strings.Contains(line, "操作=") || strings.Contains(line, "请求=")) {
			continue
		}
		if strings.TrimSpace(line) != "" {
			diagnosticLines = append(diagnosticLines, line)
		}
	}
	diagnostics := strings.TrimSpace(strings.Join(diagnosticLines, "\n"))
	if diagnostics != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n—— 程序诊断 ——\n")
		}
		builder.WriteString(diagnostics)
	}
	return builder.String()
}

func (m *modernController) saveSettings(settings persistedSettings) (modernState, error) {
	settings, err := validateModernSettings(settings, m.addresses)
	if err != nil {
		return m.getState(), err
	}

	m.mu.Lock()
	if m.transition {
		m.mu.Unlock()
		return m.getState(), errors.New("服务或证书状态正在切换，请稍候")
	}
	if m.running && (settings.Port != m.settings.Port || settings.HTTPSPort != m.settings.HTTPSPort || settings.AccessHost != m.settings.AccessHost) {
		m.mu.Unlock()
		return m.getState(), errors.New("服务运行时不能更改地址、HTTP 端口或 HTTPS 端口，请先停止服务")
	}
	m.settings = settings
	m.lastError = ""
	m.mu.Unlock()

	m.srv.SetAccess(settings.Password, false, false, false)
	m.srv.SetHTTPSRedirect(settings.RedirectToHTTPS, settings.AccessHost, settings.HTTPSPort)
	m.srv.SetChatEnabled(true)
	m.srv.SetGroupChatEnabled(settings.GroupChat)
	m.srv.SetUserGroupCreationEnabled(settings.AllowGroupChat)
	m.srv.SetUserListEnabled(settings.ShowUserList)
	m.srv.SetPrivateMessagesEnabled(settings.AllowPrivateChat)
	m.srv.SetClientDownloadEnabled(settings.AllowClientDownload)
	m.srv.SetAccountApprovalRequired(settings.RequireAccountApproval)
	if app != nil {
		app.notifyNewVisitor = settings.NotifyNewVisitor
		app.notifyNewMessage = settings.NotifyNewMessage
	}
	if err = m.saveSettingsData(settings); err != nil {
		return m.getState(), fmt.Errorf("保存配置失败：%w", err)
	}
	modernRefresh()
	return m.getState(), nil
}

func validateModernSettings(settings persistedSettings, addresses []modernAddress) (persistedSettings, error) {
	settings.Password = strings.TrimSpace(settings.Password)
	settings.Port = strings.TrimSpace(settings.Port)
	settings.HTTPSPort = strings.TrimSpace(settings.HTTPSPort)
	settings.AllowChat = true
	settings.AllowUpload = false
	settings.AllowDownload = false
	settings.GroupChat = false
	if !settings.ShowUserList {
		settings.AllowPrivateChat = false
	}
	if settings.Port == "" {
		settings.Port = "80"
	}
	if settings.HTTPSPort == "" {
		settings.HTTPSPort = "443"
	}
	httpPort, err := strconv.Atoi(settings.Port)
	if err != nil || httpPort < 1 || httpPort > 65535 {
		return settings, errors.New("HTTP 端口必须是 1 到 65535")
	}
	if settings.HTTPSPort != "" {
		httpsPort, parseErr := strconv.Atoi(settings.HTTPSPort)
		if parseErr != nil || httpsPort < 1 || httpsPort > 65535 {
			return settings, errors.New("HTTPS 端口必须留空或填写 1 到 65535")
		}
		if httpsPort == httpPort {
			return settings, errors.New("HTTP 与 HTTPS 不能使用同一个端口")
		}
	}
	if settings.RedirectToHTTPS && settings.HTTPSPort == "" {
		return settings, errors.New("开启 HTTP 自动跳转前，请先填写 HTTPS 端口并生成证书")
	}
	if !modernHostAvailable(settings.AccessHost, addresses) {
		return settings, errors.New("所选访问地址已不可用，请重新选择网卡地址")
	}
	return settings, nil
}

func (m *modernController) toggleServer(host, portText, httpsPortText string) (modernState, error) {
	m.mu.Lock()
	if m.transition {
		m.mu.Unlock()
		return m.getState(), errors.New("服务状态正在切换，请稍候")
	}
	m.transition = true
	m.lastError = ""
	running := m.running
	settings := m.settings
	m.mu.Unlock()
	modernRefresh()

	var err error
	if running {
		err = m.srv.Stop()
	} else {
		settings.AccessHost = host
		settings.Port = strings.TrimSpace(portText)
		settings.HTTPSPort = strings.TrimSpace(httpsPortText)
		settings, err = validateModernSettings(settings, m.addresses)
		if err == nil {
			m.srv.SetAccess(settings.Password, settings.AllowUpload, settings.AllowDownload, false)
			m.srv.SetHTTPSRedirect(settings.RedirectToHTTPS, settings.AccessHost, settings.HTTPSPort)
			m.srv.SetChatEnabled(true)
			m.srv.SetGroupChatEnabled(settings.GroupChat)
			m.srv.SetUserGroupCreationEnabled(settings.AllowGroupChat)
			m.srv.SetUserListEnabled(settings.ShowUserList)
			m.srv.SetPrivateMessagesEnabled(settings.AllowPrivateChat)
			m.srv.SetAccountApprovalRequired(settings.RequireAccountApproval)
			var certificateBundle database.CertificateBundle
			certFile, keyFile := "", ""
			if settings.HTTPSPort != "" {
				certificate := m.certificateStatus(settings.AccessHost)
				found := false
				if m.db != nil {
					certificateBundle, found, err = m.db.LoadCertificateBundle()
				} else {
					found = certificate.Available
					certFile, keyFile = certificate.CertPath, certificate.KeyPath
				}
				if err == nil && (!found || !certificate.Available) {
					message := strings.TrimSpace(certificate.Message)
					if message == "" {
						message = "HTTPS 证书尚未生成或已失效，请先点击“生成/更新证书”"
					}
					err = errors.New(message)
				}
			}
			if err == nil {
				httpsListen := ""
				if settings.HTTPSPort != "" {
					httpsListen = net.JoinHostPort(settings.AccessHost, settings.HTTPSPort)
				}
				httpListen := net.JoinHostPort(settings.AccessHost, settings.Port)
				if m.db != nil {
					_, err = m.startWithCertificate(httpListen, httpsListen, certificateBundle)
				} else {
					_, err = m.srv.StartWithHTTPS(httpListen, httpsListen, certFile, keyFile)
				}
			}
		}
	}

	m.mu.Lock()
	m.transition = false
	if err == nil {
		m.running = !running
		if !running {
			m.settings = settings
		}
	} else {
		m.lastError = err.Error()
	}
	currentSettings := m.settings
	currentRunning := m.running
	m.mu.Unlock()
	if app != nil {
		app.running = currentRunning
		app.transitioning = false
		app.runningPort = currentSettings.Port
		app.runningAddress = modernServerAddress(currentSettings.AccessHost, currentSettings.Port)
		app.statusAddress = app.runningAddress
	}
	if err == nil {
		_ = m.saveSettingsData(currentSettings)
	}
	modernRefresh()
	if err != nil {
		return m.getState(), err
	}
	return m.getState(), nil
}

func (m *modernController) generateCertificate() (modernState, error) {
	m.mu.Lock()
	if m.running || m.transition {
		m.mu.Unlock()
		return m.getState(), errors.New("请先停止服务，再生成或更新 HTTPS 证书")
	}
	m.transition = true
	settings := m.settings
	addresses := append([]modernAddress(nil), m.addresses...)
	m.lastError = ""
	m.mu.Unlock()
	modernRefresh()

	certificateDir := m.httpsCertificateDir()
	if certificateDir == "" {
		err := errors.New("无法确定证书保存目录")
		m.finishCertificateGeneration(settings, err)
		return m.getState(), err
	}
	hosts := make([]string, 0, len(addresses)+3)
	hosts = append(hosts, "localhost", "127.0.0.1", "::1")
	for _, address := range addresses {
		if strings.TrimSpace(address.Host) != "" {
			hosts = append(hosts, address.Host)
		}
	}
	if m.db != nil {
		bundle, _, generateErr := generateHTTPSCertificateBundle(hosts)
		if generateErr == nil {
			generateErr = m.db.SaveCertificateBundle(bundle)
		}
		if generateErr != nil {
			err := fmt.Errorf("生成 HTTPS 证书失败：%w", generateErr)
			m.finishCertificateGeneration(settings, err)
			return m.getState(), err
		}
		for _, name := range []string{httpsCAFileName, httpsCAKeyFileName, httpsCertFileName, httpsCertKeyFileName} {
			_ = os.Remove(filepath.Join(certificateDir, name))
		}
	} else if _, err := generateHTTPSCertificate(certificateDir, hosts); err != nil {
		err = fmt.Errorf("生成 HTTPS 证书失败：%w", err)
		m.finishCertificateGeneration(settings, err)
		return m.getState(), err
	}
	if settings.HTTPSPort == "" {
		settings.HTTPSPort = suggestedHTTPSPort(settings.Port)
	}
	if err := m.saveSettingsData(settings); err != nil {
		err = fmt.Errorf("证书已生成，但保存 HTTPS 端口失败：%w", err)
		m.finishCertificateGeneration(settings, err)
		return m.getState(), err
	}
	m.finishCertificateGeneration(settings, nil)
	return m.getState(), nil
}

func (m *modernController) finishCertificateGeneration(settings persistedSettings, err error) {
	m.mu.Lock()
	m.transition = false
	if err == nil {
		m.settings = settings
		m.lastError = ""
	} else {
		m.lastError = err.Error()
	}
	m.mu.Unlock()
	modernRefresh()
}

func suggestedHTTPSPort(httpPortText string) string {
	httpPort, err := strconv.Atoi(strings.TrimSpace(httpPortText))
	if err == nil && httpPort == 443 {
		return "8443"
	}
	return "443"
}

func (m *modernController) addFile() (modernState, error) {
	if app == nil || app.hwnd == 0 {
		return m.getState(), errors.New("主窗口尚未准备好")
	}
	paths, err := chooseFilePaths(app.hwnd)
	if err != nil {
		return m.getState(), err
	}
	return m.addPaths(paths)
}

func (m *modernController) addFolder() (modernState, error) {
	if app == nil || app.hwnd == 0 {
		return m.getState(), errors.New("主窗口尚未准备好")
	}
	selectedPath, err := chooseFolderPath(app.hwnd)
	if err != nil {
		return m.getState(), err
	}
	if selectedPath == "" {
		return m.getState(), nil
	}
	return m.addPaths([]string{selectedPath})
}

func (m *modernController) addPaths(paths []string) (modernState, error) {
	if len(paths) == 0 {
		return m.getState(), nil
	}
	added := 0
	errorsFound := make([]string, 0)
	for _, itemPath := range paths {
		itemPath = strings.TrimSpace(itemPath)
		if itemPath == "" {
			continue
		}
		if err := m.srv.Add(itemPath); err != nil {
			errorsFound = append(errorsFound, filepath.Base(itemPath)+"："+err.Error())
			continue
		}
		added++
	}
	if added > 0 {
		if err := m.saveShares(); err != nil {
			return m.getState(), fmt.Errorf("保存分享列表失败：%w", err)
		}
		modernRefresh()
	}
	if len(errorsFound) > 0 {
		return m.getState(), errors.New(strings.Join(errorsFound, "；"))
	}
	return m.getState(), nil
}

func (m *modernController) removeShares(indices []int, deleteTemporary bool) (modernState, error) {
	if len(indices) == 0 {
		return m.getState(), nil
	}
	items := m.srv.Shares()
	removeIndices := make([]int, 0, len(indices))
	failures := make([]string, 0)
	seen := make(map[int]struct{}, len(indices))
	for _, index := range indices {
		if index < 0 || index >= len(items) {
			continue
		}
		if _, duplicate := seen[index]; duplicate {
			continue
		}
		seen[index] = struct{}{}
		item := items[index]
		if deleteTemporary && item.ManagedTemporary && pathDirectlyInsideDirectory(item.Path, m.tempUploadDir) {
			info, err := os.Lstat(item.Path)
			if errors.Is(err, os.ErrNotExist) {
				removeIndices = append(removeIndices, index)
				continue
			}
			if err != nil {
				failures = append(failures, item.Name+"："+err.Error())
				continue
			}
			if info.Mode().IsRegular() {
				if err = os.Remove(item.Path); err != nil {
					failures = append(failures, item.Name+"："+err.Error())
					continue
				}
			}
		}
		removeIndices = append(removeIndices, index)
	}
	m.srv.RemoveMany(removeIndices)
	if err := m.saveShares(); err != nil {
		return m.getState(), err
	}
	modernRefresh()
	if len(failures) > 0 {
		return m.getState(), errors.New("部分临时文件删除失败：" + strings.Join(failures, "；"))
	}
	return m.getState(), nil
}

func pathInsideDirectory(itemPath, directory string) bool {
	if strings.TrimSpace(itemPath) == "" || strings.TrimSpace(directory) == "" {
		return false
	}
	itemAbsolute, err := filepath.Abs(itemPath)
	if err != nil {
		return false
	}
	directoryAbsolute, err := filepath.Abs(directory)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(directoryAbsolute, itemAbsolute)
	if err != nil || relative == "." || relative == ".." {
		return false
	}
	return !strings.HasPrefix(relative, ".."+string(os.PathSeparator)) && !filepath.IsAbs(relative)
}

// pathDirectlyInsideDirectory deliberately excludes nested paths. Browser
// fallback uploads are created directly in the managed Temp directory; this
// prevents a junction or reparse point below Temp from expanding the physical
// deletion boundary.
func pathDirectlyInsideDirectory(itemPath, directory string) bool {
	if !pathInsideDirectory(itemPath, directory) {
		return false
	}
	itemAbsolute, itemErr := filepath.Abs(itemPath)
	directoryAbsolute, directoryErr := filepath.Abs(directory)
	if itemErr != nil || directoryErr != nil {
		return false
	}
	return strings.EqualFold(filepath.Clean(filepath.Dir(itemAbsolute)), filepath.Clean(directoryAbsolute))
}

func (m *modernController) renameShare(index int, name string) (modernState, error) {
	if err := m.srv.Rename(index, name); err != nil {
		return m.getState(), err
	}
	if err := m.saveShares(); err != nil {
		return m.getState(), err
	}
	modernRefresh()
	return m.getState(), nil
}

func (m *modernController) openShare(index int) error {
	m.mu.RLock()
	running := m.running
	settings := m.settings
	m.mu.RUnlock()
	items := m.srv.Shares()
	if index < 0 || index >= len(items) {
		return errors.New("分享项目不存在")
	}
	if !running {
		return errors.New("请先启动服务")
	}
	address := preferredModernServerAddress(settings) + "/" + url.PathEscape(items[index].Name)
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", address).Start()
}

func (m *modernController) revealShare(index int) error {
	items := m.srv.Shares()
	if index < 0 || index >= len(items) {
		return errors.New("分享项目不存在")
	}
	itemPath := items[index].Path
	info, err := os.Stat(itemPath)
	if err != nil {
		return fmt.Errorf("无法打开磁盘位置：%w", err)
	}
	if info.IsDir() {
		return exec.Command("explorer.exe", itemPath).Start()
	}
	return exec.Command("explorer.exe", "/select,"+itemPath).Start()
}

func (m *modernController) openBrowser() error {
	m.mu.RLock()
	running := m.running
	settings := m.settings
	m.mu.RUnlock()
	if !running {
		return errors.New("请先启动服务")
	}
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", preferredModernServerAddress(settings)).Start()
}

func preferredModernServerAddress(settings persistedSettings) string {
	if strings.TrimSpace(settings.HTTPSPort) != "" {
		return modernServerAddressForScheme("https", settings.AccessHost, settings.HTTPSPort)
	}
	return modernServerAddress(settings.AccessHost, settings.Port)
}

func (m *modernController) copyText(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("没有可复制的内容")
	}
	if app == nil || app.hwnd == 0 {
		return errors.New("主窗口尚未准备好")
	}
	return setClipboardText(app.hwnd, value)
}

func (m *modernController) clearLogs() (modernState, error) {
	m.log.Clear()
	if m.db != nil {
		if err := m.db.ClearAccessRecords(); err != nil {
			return m.getState(), err
		}
	}
	modernRefresh()
	return m.getState(), nil
}

func (m *modernController) getConversation(id string) (server.ChatConversation, error) {
	conversation, ok := m.srv.ChatConversationSnapshot(id)
	if !ok {
		return server.ChatConversation{}, errors.New("会话不存在或已被移除")
	}
	return conversation, nil
}

func (m *modernController) sendMessage(id, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("消息不能为空")
	}
	if err := m.srv.SendChatMessage(id, text); err != nil {
		return err
	}
	modernRefresh()
	return nil
}

func (m *modernController) sendImage(id, mimeType, encoded string) error {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if mimeType != "image/png" && mimeType != "image/jpeg" {
		return errors.New("只支持 PNG 或 JPEG 图片")
	}
	if comma := strings.IndexByte(encoded, ','); comma >= 0 {
		encoded = encoded[comma+1:]
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("图片数据无效")
	}
	if err = m.srv.SendChatImage(id, mimeType, data); err != nil {
		return err
	}
	modernRefresh()
	return nil
}

func (m *modernController) sendFile(id, name, mimeType, encoded string) error {
	if comma := strings.IndexByte(encoded, ','); comma >= 0 {
		encoded = encoded[comma+1:]
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("文件数据无效")
	}
	if err = m.srv.SendChatFile(id, name, mimeType, data); err != nil {
		return err
	}
	modernRefresh()
	return nil
}

func (m *modernController) openChatAttachment(messageID string) error {
	if m.db == nil {
		return errors.New("聊天附件数据库尚未初始化")
	}
	attachmentPath, err := m.db.ChatAttachmentPath(strings.TrimSpace(messageID))
	if err != nil {
		return errors.New("聊天附件不存在或已被清理")
	}
	return exec.Command("explorer.exe", "/select,"+attachmentPath).Start()
}

func (m *modernController) listArchives(kind, query, from, to string, page int) (server.ChatArchivePage, error) {
	if m.db == nil {
		return server.ChatArchivePage{}, errors.New("聊天归档数据库尚未初始化")
	}
	parseDate := func(raw string, endOfDay bool) time.Time {
		value, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(raw), time.Local)
		if err != nil {
			return time.Time{}
		}
		if endOfDay {
			value = value.Add(24*time.Hour - time.Nanosecond)
		}
		return value.UTC()
	}
	return m.db.ListChatAttachments(server.ChatArchiveQuery{
		Kind: strings.TrimSpace(kind), Query: strings.TrimSpace(query),
		From: parseDate(from, false), To: parseDate(to, true),
		Page: page, PageSize: 36,
	})
}

func (m *modernController) archiveThumbnail(messageID string) (string, error) {
	if m.db == nil {
		return "", errors.New("聊天归档数据库尚未初始化")
	}
	attachment, err := m.db.OpenChatAttachment(strings.TrimSpace(messageID))
	if err != nil {
		return "", errors.New("图片归档不存在")
	}
	defer attachment.Reader.Close()
	source, _, err := image.Decode(attachment.Reader)
	if err != nil {
		return "", errors.New("图片归档无法读取")
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return "", errors.New("图片尺寸无效")
	}
	maxSize := 320
	targetWidth, targetHeight := maxSize, maxSize
	if width > height {
		targetHeight = height * maxSize / width
	} else if height > width {
		targetWidth = width * maxSize / height
	}
	if targetWidth < 1 {
		targetWidth = 1
	}
	if targetHeight < 1 {
		targetHeight = 1
	}
	target := image.NewNRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		for x := 0; x < targetWidth; x++ {
			sx := bounds.Min.X + x*width/targetWidth
			sy := bounds.Min.Y + y*height/targetHeight
			target.Set(x, y, source.At(sx, sy))
		}
	}
	var output bytes.Buffer
	if err = png.Encode(&output, target); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(output.Bytes()), nil
}

func (m *modernController) deleteArchive(messageID string) error {
	if err := m.srv.DeleteChatMessage(strings.TrimSpace(messageID)); err != nil {
		return err
	}
	modernRefresh()
	return nil
}

func (m *modernController) setUserName(ip, name string) (modernState, error) {
	if err := m.srv.SetChatUserName(ip, name); err != nil {
		return m.getState(), err
	}
	modernRefresh()
	return m.getState(), nil
}

func (m *modernController) setUserBlacklisted(ip string, blacklisted bool) (modernState, error) {
	if err := m.srv.SetChatUserBlacklisted(ip, blacklisted); err != nil {
		return m.getState(), err
	}
	modernRefresh()
	return m.getState(), nil
}

func (m *modernController) renameGroup(groupID, name string) (modernState, error) {
	if err := m.srv.RenameChatGroup(groupID, name); err != nil {
		return m.getState(), err
	}
	modernRefresh()
	return m.getState(), nil
}

func (m *modernController) removeGroupMember(groupID, memberIP string) (modernState, error) {
	if err := m.srv.RemoveChatGroupMember(groupID, memberIP); err != nil {
		return m.getState(), err
	}
	modernRefresh()
	return m.getState(), nil
}

func (m *modernController) deleteGroup(groupID string) (modernState, error) {
	if err := m.srv.DeleteChatGroup(groupID); err != nil {
		return m.getState(), err
	}
	modernRefresh()
	return m.getState(), nil
}

func (m *modernController) clearChatHistory() (modernState, error) {
	if err := m.srv.ClearChatHistory(); err != nil {
		return m.getState(), err
	}
	if app != nil {
		rememberChatMessages(m.srv.ChatOverview())
	}
	modernRefresh()
	return m.getState(), nil
}

func (m *modernController) clearUsers() (modernState, error) {
	if err := m.srv.ClearChatUsers(); err != nil {
		return m.getState(), err
	}
	modernRefresh()
	return m.getState(), nil
}

func (m *modernController) removeVisitor(id string) (modernState, error) {
	if id == server.ChatGroupConversationID {
		return m.getState(), errors.New("系统群会话不能删除")
	}
	for _, account := range m.srv.Accounts() {
		if account.ID == id {
			if err := m.srv.DeleteAccount(id); err != nil {
				return m.getState(), err
			}
			_ = m.srv.RemoveChatVisitor(id)
			modernRefresh()
			return m.getState(), nil
		}
	}
	if !m.srv.RemoveChatVisitor(id) {
		return m.getState(), errors.New("访客不存在或已重新连接")
	}
	modernRefresh()
	return m.getState(), nil
}

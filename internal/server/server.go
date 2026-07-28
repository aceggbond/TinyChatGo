package server

import (
	"archive/tar"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"hfsgo/internal/appinfo"
	"hfsgo/internal/discovery"
)

type Share struct {
	Name             string
	Path             string
	ManagedTemporary bool `json:",omitempty"`
}

const maxVisitorSessions = 4096

var gracefulShutdownTimeout = 3 * time.Second

// ListenAddresses contains the actual TCP addresses used by a running Server.
// HTTPS is empty when the server was started without an HTTPS listener.
type ListenAddresses struct {
	HTTP  string
	HTTPS string
}

type Server struct {
	mu                  sync.RWMutex
	lifecycleMu         sync.Mutex
	handlerMu           sync.Mutex
	handlerWG           sync.WaitGroup
	handlerAccepting    bool
	shares              []Share
	http                *http.Server
	ln                  net.Listener
	https               *http.Server
	tlsLn               net.Listener
	runCancel           context.CancelFunc
	logger              *log.Logger
	password            string
	accessVersion       uint64
	allowUpload         bool
	allowDownload       bool
	allowManage         bool
	allowClientDownload bool
	redirectHTTP        bool
	redirectHTTPHost    string
	fallbackUploadDir   string
	shareChangeNotify   func()
	brandLogo           []byte
	chat                *chatHub
	persistence         Persistence
	visitorMu           sync.Mutex
	visitorSeen         map[string]time.Time
	visitorNotify       func(ChatClientInfo)
}

func New(logWriter io.Writer) *Server {
	logger := log.New(logWriter, "", log.LstdFlags)
	chat := newChatHub()
	result := &Server{
		logger:           logger,
		allowDownload:    true,
		handlerAccepting: true,
		chat:             chat,
		visitorSeen:      make(map[string]time.Time),
	}
	chat.logOperation = func(ip, operation string) {
		at := time.Now().UTC()
		logger.Printf("IP=%s 操作=%s", ip, operation)
		result.mu.RLock()
		persistence := result.persistence
		result.mu.RUnlock()
		if persistence != nil {
			_ = persistence.SaveAccessRecord(AccessRecord{At: at, IP: ip, Operation: operation})
		}
	}
	return result
}

// SetVisitorNotifier installs a callback for the first successful HTML page
// visit from each IP address during the current server run.
func (s *Server) SetVisitorNotifier(notify func(ChatClientInfo)) {
	s.visitorMu.Lock()
	s.visitorNotify = notify
	s.visitorMu.Unlock()
}

func (s *Server) SetAccess(password string, upload, download, manage bool) {
	s.mu.Lock()
	passwordChanged := s.password != password
	if passwordChanged {
		s.accessVersion++
	}
	s.password, s.allowUpload, s.allowDownload, s.allowManage = password, upload, download, manage
	s.mu.Unlock()
	if passwordChanged {
		s.chat.disconnect(websocketClosePolicyViolation, "access credentials changed")
	}
}

// SetClientDownloadEnabled controls whether the browser portal exposes and
// serves a client-mode copy of the currently running Windows executable.
func (s *Server) SetClientDownloadEnabled(enabled bool) {
	s.mu.Lock()
	s.allowClientDownload = enabled
	s.mu.Unlock()
}

func (s *Server) ClientDownloadEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allowClientDownload
}

// SetHTTPSRedirect controls whether requests received by the plain HTTP
// listener are redirected to the configured HTTPS listener. The destination
// host is supplied by the desktop application instead of trusting the incoming
// Host header, which avoids turning the redirect into an open redirect.
func (s *Server) SetHTTPSRedirect(enabled bool, host, port string) {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	target := ""
	if enabled && host != "" && port != "" {
		target = net.JoinHostPort(host, port)
	}
	s.mu.Lock()
	s.redirectHTTP = target != ""
	s.redirectHTTPHost = target
	s.mu.Unlock()
}

// SetFallbackUploadDir configures the directory used for uploads submitted on
// the root page. Each uploaded file is added as an individual Share; the
// directory itself is never shared. Passing an empty path disables root-page
// uploads.
func (s *Server) SetFallbackUploadDir(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		s.mu.Lock()
		s.fallbackUploadDir = ""
		s.mu.Unlock()
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve fallback upload directory: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("open fallback upload directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("fallback upload path is not a directory")
	}
	s.mu.Lock()
	s.fallbackUploadDir = abs
	s.mu.Unlock()
	return nil
}

// SetShareChangeNotifier installs a callback invoked after a browser fallback
// upload adds one or more shares. It lets the desktop host persist the updated
// share list immediately without coupling the HTTP server to a config path.
func (s *Server) SetShareChangeNotifier(notify func()) {
	s.mu.Lock()
	s.shareChangeNotify = notify
	s.mu.Unlock()
}

// SetBrandLogo installs the PNG shown by the browser dashboard. The data is
// copied so callers may release or reuse their source buffer safely.
func (s *Server) SetBrandLogo(png []byte) {
	s.mu.Lock()
	s.brandLogo = append(s.brandLogo[:0], png...)
	s.mu.Unlock()
}

func (s *Server) Shares() []Share {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Share(nil), s.shares...)
}

// ReplaceShares installs a previously persisted share list after validating
// paths and de-duplicating display names.
func (s *Server) ReplaceShares(items []Share) {
	valid := make([]Share, 0, len(items))
	used := make(map[string]struct{})
	for _, item := range items {
		absolute, err := filepath.Abs(strings.TrimSpace(item.Path))
		if err != nil {
			continue
		}
		if _, err = os.Stat(absolute); err != nil {
			continue
		}
		name := strings.TrimSpace(item.Name)
		if name == "" {
			name = filepath.Base(absolute)
		}
		base := name
		for suffix := 2; ; suffix++ {
			key := strings.ToLower(name)
			if _, exists := used[key]; !exists {
				used[key] = struct{}{}
				break
			}
			name = fmt.Sprintf("%s (%d)", base, suffix)
		}
		item.Name = name
		item.Path = absolute
		valid = append(valid, item)
	}
	s.mu.Lock()
	s.shares = valid
	s.mu.Unlock()
}

func (s *Server) Add(realPath string) error {
	return s.addMany([]string{realPath}, false)
}

// AddManagedTemporary adds a file created by the root fallback uploader. The
// marker lets the desktop GUI distinguish application-owned temporary uploads
// from unrelated files a user may have shared from the same directory.
func (s *Server) AddManagedTemporary(realPath string) error {
	return s.addMany([]string{realPath}, true)
}

func (s *Server) addMany(realPaths []string, managedTemporary bool) error {
	prepared := make([]Share, 0, len(realPaths))
	for _, realPath := range realPaths {
		abs, err := filepath.Abs(realPath)
		if err != nil {
			return err
		}
		if _, err = os.Stat(abs); err != nil {
			return err
		}
		prepared = append(prepared, Share{Name: filepath.Base(abs), Path: abs, ManagedTemporary: managedTemporary})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, share := range prepared {
		base := share.Name
		for n := 2; s.nameExists(share.Name); n++ {
			share.Name = fmt.Sprintf("%s (%d)", base, n)
		}
		s.shares = append(s.shares, share)
	}
	return nil
}

func (s *Server) nameExists(name string) bool {
	for _, x := range s.shares {
		if strings.EqualFold(x.Name, name) {
			return true
		}
	}
	return false
}

func (s *Server) Remove(i int) {
	s.RemoveMany([]int{i})
}

// RemoveMany removes the requested share indexes as one atomic update.
// Invalid and duplicate indexes are ignored.
func (s *Server) RemoveMany(indices []int) int {
	if len(indices) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	selected := make(map[int]struct{}, len(indices))
	for _, index := range indices {
		if index >= 0 && index < len(s.shares) {
			selected[index] = struct{}{}
		}
	}
	if len(selected) == 0 {
		return 0
	}
	kept := s.shares[:0]
	for index, share := range s.shares {
		if _, remove := selected[index]; !remove {
			kept = append(kept, share)
		}
	}
	removed := len(s.shares) - len(kept)
	for index := len(kept); index < len(s.shares); index++ {
		s.shares[index] = Share{}
	}
	s.shares = kept
	return removed
}

func (s *Server) Rename(i int, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return errors.New("无效的虚拟名称")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.shares) {
		return errors.New("项目不存在")
	}
	for n, x := range s.shares {
		if n != i && strings.EqualFold(x.Name, name) {
			return errors.New("名称已存在")
		}
	}
	s.shares[i].Name = name
	return nil
}

func (s *Server) Save(filename string) error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.shares, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0600)
}

func (s *Server) Load(filename string) error {
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var items []Share
	if err = json.Unmarshal(data, &items); err != nil {
		return err
	}
	valid := items[:0]
	for _, x := range items {
		if _, e := os.Stat(x.Path); e == nil {
			valid = append(valid, x)
		}
	}
	s.mu.Lock()
	s.shares = valid
	s.mu.Unlock()
	return nil
}

func (s *Server) Start(addr string) (string, error) {
	addresses, err := s.StartWithHTTPS(addr, "", "", "")
	return addresses.HTTP, err
}

// StartWithHTTPS starts the same handler on HTTP and, when httpsAddr is not
// empty, HTTPS. The certificate and both listeners are prepared before either
// serving goroutine starts, so an error never leaves a partial server running.
//
// Repeated calls while the server is running are idempotent and return the
// addresses of the existing listeners.
func (s *Server) StartWithHTTPS(httpAddr, httpsAddr, certFile, keyFile string) (ListenAddresses, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	if s.http != nil {
		return s.listenAddresses(), nil
	}

	var certificate tls.Certificate
	var err error
	if httpsAddr != "" {
		if certFile == "" || keyFile == "" {
			return ListenAddresses{}, errors.New("HTTPS requires both a certificate file and a private key file")
		}
		certificate, err = tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return ListenAddresses{}, fmt.Errorf("load HTTPS certificate: %w", err)
		}
	}
	return s.startWithHTTPSCertificateLocked(httpAddr, httpsAddr, certificate)
}

// StartWithHTTPSPEM starts HTTPS directly from certificate bytes. It allows
// the desktop application to keep all certificate material in lanchatgo.db
// without creating temporary or persistent certificate files.
func (s *Server) StartWithHTTPSPEM(httpAddr, httpsAddr string, certPEM, keyPEM []byte) (ListenAddresses, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.http != nil {
		return s.listenAddresses(), nil
	}
	var certificate tls.Certificate
	var err error
	if httpsAddr != "" {
		if len(certPEM) == 0 || len(keyPEM) == 0 {
			return ListenAddresses{}, errors.New("HTTPS requires both certificate and private key data")
		}
		certificate, err = tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return ListenAddresses{}, fmt.Errorf("load HTTPS certificate data: %w", err)
		}
	}
	return s.startWithHTTPSCertificateLocked(httpAddr, httpsAddr, certificate)
}

// startWithHTTPSCertificateLocked assumes lifecycleMu is held and no listener
// is currently active.
func (s *Server) startWithHTTPSCertificateLocked(httpAddr, httpsAddr string, certificate tls.Certificate) (ListenAddresses, error) {
	httpLn, err := net.Listen("tcp", httpAddr)
	if err != nil {
		return ListenAddresses{}, fmt.Errorf("listen HTTP on %q: %w", httpAddr, err)
	}

	var httpsLn net.Listener
	if httpsAddr != "" {
		rawHTTPSLn, listenErr := net.Listen("tcp", httpsAddr)
		if listenErr != nil {
			_ = httpLn.Close()
			return ListenAddresses{}, fmt.Errorf("listen HTTPS on %q: %w", httpsAddr, listenErr)
		}
		httpsLn = tls.NewListener(rawHTTPSLn, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		})
	}

	s.resetVisitorSessions()
	s.chat.resume()
	runContext, runCancel := context.WithCancel(context.Background())
	s.runCancel = runCancel
	s.ln = httpLn
	baseContext := func(net.Listener) context.Context { return runContext }
	s.http = &http.Server{Handler: s, BaseContext: baseContext, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	if httpsLn != nil {
		s.tlsLn = httpsLn
		s.https = &http.Server{Handler: s, BaseContext: baseContext, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	}
	s.handlerMu.Lock()
	s.handlerAccepting = true
	s.handlerMu.Unlock()

	go s.serve("HTTP", s.http, s.ln)
	if s.https != nil {
		go s.serve("HTTPS", s.https, s.tlsLn)
	}

	addresses := s.listenAddresses()
	httpPort := listenAddressPort(addresses.HTTP)
	httpsPort := listenAddressPort(addresses.HTTPS)
	if err := discovery.StartResponder(runContext, func() discovery.Service {
		s.mu.RLock()
		redirectHTTPS := s.redirectHTTP
		clientDownload := s.allowClientDownload
		s.mu.RUnlock()
		return discovery.Service{
			Name:           appinfo.Name,
			Version:        appinfo.Version,
			HTTPPort:       httpPort,
			HTTPSPort:      httpsPort,
			RedirectHTTPS:  redirectHTTPS,
			ClientDownload: clientDownload,
		}
	}); err != nil {
		s.logger.Printf("局域网客户端自动发现不可用: %v", err)
	}
	s.logger.Printf("HTTP server listening on %s", addresses.HTTP)
	if addresses.HTTPS != "" {
		s.logger.Printf("HTTPS server listening on %s", addresses.HTTPS)
	}
	return addresses, nil
}

func listenAddressPort(address string) int {
	_, portText, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

func (s *Server) Stop() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	httpServer, httpsServer := s.http, s.https
	runCancel := s.runCancel
	// Close the handler gate before canceling connections. Every handler that
	// passed the gate has already incremented handlerWG, and no later request
	// can increment it while Stop is waiting. A request goroutine that was
	// accepted by net/http but has not entered ServeHTTP yet is rejected.
	s.handlerMu.Lock()
	s.handlerAccepting = false
	s.handlerMu.Unlock()
	if runCancel != nil {
		runCancel()
	}

	chatStopped := s.chat.pause(2 * time.Second)
	if httpServer == nil && httpsServer == nil {
		if !chatStopped {
			return errors.New("等待聊天连接关闭超时")
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
	defer cancel()

	type shutdownResult struct {
		protocol string
		server   *http.Server
		err      error
	}
	results := make(chan shutdownResult, 2)
	serverCount := 0
	for protocol, h := range map[string]*http.Server{"HTTP": httpServer, "HTTPS": httpsServer} {
		if h == nil {
			continue
		}
		serverCount++
		go func(protocol string, h *http.Server) {
			results <- shutdownResult{protocol: protocol, server: h, err: h.Shutdown(ctx)}
		}(protocol, h)
	}

	var errs []error
	for i := 0; i < serverCount; i++ {
		result := <-results
		if result.err != nil {
			// Shutdown is graceful and may time out while a client is still
			// uploading. Force-close those connections so a stopped Server never
			// leaves unmanaged handlers running behind a newly started listener.
			if closeErr := result.server.Close(); closeErr != nil && !isClosedListenerError(closeErr) {
				errs = append(errs, fmt.Errorf("force stop %s server after %v: %w", result.protocol, result.err, closeErr))
			} else {
				s.logger.Printf("%s graceful shutdown did not finish (%v); active connections were closed", result.protocol, result.err)
			}
		}
	}
	s.handlerWG.Wait()
	s.http = nil
	s.ln = nil
	s.https = nil
	s.tlsLn = nil
	s.runCancel = nil
	if !chatStopped {
		s.logger.Print("chat shutdown exceeded the graceful timeout; connections were force-closed")
	}
	s.logger.Print("HTTP/HTTPS servers stopped")
	return errors.Join(errs...)
}

func isClosedListenerError(err error) bool {
	return errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection")
}

func (s *Server) listenAddresses() ListenAddresses {
	var addresses ListenAddresses
	if s.ln != nil {
		addresses.HTTP = s.ln.Addr().String()
	}
	if s.tlsLn != nil {
		addresses.HTTPS = s.tlsLn.Addr().String()
	}
	return addresses
}

func (s *Server) serve(protocol string, h *http.Server, ln net.Listener) {
	if err := h.Serve(ln); err != nil &&
		!errors.Is(err, http.ErrServerClosed) &&
		!errors.Is(err, net.ErrClosed) {
		s.logger.Printf("%s server error: %v", protocol, err)
	}
}

func (s *Server) serveClientDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.ClientDownloadEnabled() {
		http.NotFound(w, r)
		return
	}
	platform := clientDownloadPlatform(r)
	executable, err := os.Executable()
	if err != nil {
		http.Error(w, "client executable unavailable", http.StatusServiceUnavailable)
		return
	}
	filename := "LanChatGo-Client-windows-amd64.exe"
	contentType := "application/vnd.microsoft.portable-executable"
	downloadPath := executable
	if platform == "macos-arm64" {
		filename = "LanChatGo-Client-macos-arm64.zip"
		contentType = "application/zip"
		downloadPath = filepath.Join(filepath.Dir(executable), filename)
		if info, statErr := os.Stat(downloadPath); statErr != nil || !info.Mode().IsRegular() {
			releaseURL := "https://github.com/aceggbond/LanChatGo/releases/download/" + appinfo.Tag + "/" + filename
			http.Redirect(w, r, releaseURL, http.StatusTemporaryRedirect)
			return
		}
	}
	file, err := os.Open(downloadPath)
	if err != nil {
		http.Error(w, "client executable unavailable", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "client executable unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filename, info.ModTime(), file)
}

func clientDownloadPlatform(r *http.Request) string {
	if r == nil {
		return "windows-amd64"
	}
	platform := strings.ToLower(strings.Trim(r.Header.Get("Sec-CH-UA-Platform"), `"' `))
	userAgent := strings.ToLower(r.UserAgent())
	if strings.Contains(platform, "mac") ||
		strings.Contains(userAgent, "macintosh") ||
		strings.Contains(userAgent, "mac os x") {
		return "macos-arm64"
	}
	return "windows-amd64"
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.beginHTTPHandler(r.Context()) {
		http.Error(w, "server stopping", http.StatusServiceUnavailable)
		return
	}
	defer s.handlerWG.Done()
	w.Header().Set("X-LanChatGo-Version", appinfo.Version)
	// Keep the old header for clients that still use it to detect the server.
	w.Header().Set("X-HFS-Go-Version", appinfo.Version)
	w.Header().Set("Accept-CH", "Sec-CH-UA, Sec-CH-UA-Platform, Sec-CH-UA-Platform-Version")
	start := time.Now()
	status := 200
	operation := requestOperation(r)
	clientIP, _ := clientAddressFromRequest(r)
	if clientIP == "" {
		clientIP = "未知"
	}
	lw := &statusWriter{ResponseWriter: w, status: &status}
	defer func() {
		duration := time.Since(start).Round(time.Millisecond)
		if r.URL.Path != "/__hfs/chat/status" || status != http.StatusOK {
			s.logger.Printf(
				"IP=%s 操作=%s 请求=%s %q 状态=%d 用时=%s",
				clientIP,
				operation,
				r.Method,
				r.URL.Path,
				status,
				duration,
			)
		}
		s.mu.RLock()
		persistence := s.persistence
		s.mu.RUnlock()
		if persistence != nil {
			_ = persistence.SaveAccessRecord(AccessRecord{
				At:        start.UTC(),
				IP:        clientIP,
				Operation: operation,
				Method:    r.Method,
				Path:      r.URL.Path,
				Status:    status,
				Duration:  duration.String(),
			})
		}
	}()
	if s.ChatUserBlacklisted(clientIP) {
		operation = "黑名单拒绝访问"
		status = http.StatusForbidden
		http.Error(lw, "此 IP 已被管理员加入黑名单", http.StatusForbidden)
		return
	}
	s.mu.RLock()
	password, accessVersion, upload, download, manage, fallbackUploadDir := s.password, s.accessVersion, s.allowUpload, s.allowDownload, s.allowManage, s.fallbackUploadDir
	redirectHTTP, redirectHTTPHost := s.redirectHTTP, s.redirectHTTPHost
	s.mu.RUnlock()
	if redirectHTTP && r.TLS == nil {
		operation = "跳转 HTTPS"
		target := *r.URL
		target.Scheme = "https"
		target.Host = redirectHTTPHost
		target.User = nil
		status = http.StatusTemporaryRedirect
		http.Redirect(lw, r, target.String(), http.StatusTemporaryRedirect)
		return
	}
	if password != "" {
		_, pass, ok := r.BasicAuth()
		if !ok || pass != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="LanChatGo"`)
			http.Error(lw, "需要访问密码", http.StatusUnauthorized)
			return
		}
	}
	if r.URL.Path == "/__hfs/client/download" {
		operation = "下载客户端"
		s.serveClientDownload(lw, r)
		return
	}
	if r.URL.Path == "/__hfs/logo.png" {
		operation = "读取界面资源"
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(lw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.mu.RLock()
		logo := append([]byte(nil), s.brandLogo...)
		s.mu.RUnlock()
		if len(logo) == 0 {
			http.NotFound(lw, r)
			return
		}
		lw.Header().Set("Content-Type", "image/png")
		// The logo is part of the application UI and may change between local
		// rebuilds without a version bump. Do not let an old favicon survive.
		lw.Header().Set("Cache-Control", "no-store")
		lw.Header().Set("X-Content-Type-Options", "nosniff")
		lw.Header().Set("Content-Length", strconv.Itoa(len(logo)))
		if r.Method == http.MethodGet {
			_, _ = lw.Write(logo)
		}
		return
	}
	if r.URL.Path == "/__hfs/chat/status" {
		operation = "查询聊天状态"
		if r.Method != http.MethodGet {
			http.Error(lw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if _, _, err := s.ensureChatSession(lw, r); err != nil {
			http.Error(lw, "无法创建聊天会话", http.StatusInternalServerError)
			return
		}
		lw.Header().Set("Content-Type", "application/json; charset=utf-8")
		lw.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(lw).Encode(map[string]bool{
			"enabled":         s.ChatEnabled(),
			"userListEnabled": s.UserListEnabled(),
			"privateEnabled":  s.PrivateMessagesEnabled(),
			"filesEnabled":    upload || download,
		})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/__hfs/chat/file/") {
		operation = "下载聊天附件"
		s.serveChatAttachment(lw, r, clientIP)
		return
	}
	if r.URL.Path == "/__hfs/chat/archive" {
		operation = "查询聊天归档"
		s.serveChatArchive(lw, r, clientIP)
		return
	}
	if r.URL.Path == "/__hfs/chat/upload" {
		operation = "上传聊天附件"
		if r.Method != http.MethodPost {
			http.Error(lw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !sameWriteOrigin(r) {
			http.Error(lw, "origin not allowed", http.StatusForbidden)
			return
		}
		s.handleChatAttachmentUpload(lw, r, clientIP)
		return
	}
	if r.URL.Path == "/__hfs/chat/ws" {
		operation = "建立聊天连接"
		if r.Method != http.MethodGet {
			http.Error(lw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.ChatEnabled() {
			http.Error(lw, "聊天功能未启用", http.StatusForbidden)
			return
		}
		status = s.handleChatWebSocket(w, r, accessVersion)
		return
	}
	if r.Method == http.MethodPost && !sameWriteOrigin(r) {
		http.Error(lw, "origin not allowed", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost && !upload {
		if !manage {
			http.Error(lw, "写入功能未启用", http.StatusForbidden)
			return
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		http.Error(lw, "method not allowed", 405)
		return
	}
	clean, err := url.PathUnescape(r.URL.EscapedPath())
	if err != nil {
		http.Error(lw, "bad path", 400)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path.Clean("/"+clean), "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		if r.Method == http.MethodPost {
			if !requestIsMultipart(r) {
				http.Error(lw, "首页只接受文件上传请求", http.StatusMethodNotAllowed)
				return
			}
			operation = "上传文件"
			if !upload {
				http.Error(lw, "上传功能未启用", http.StatusForbidden)
				return
			}
			if fallbackUploadDir == "" {
				http.Error(lw, "首页上传目录未配置", http.StatusServiceUnavailable)
				return
			}
			s.handleFallbackUpload(lw, r, fallbackUploadDir)
			return
		}
		s.renderRoot(lw, r)
		return
	}
	s.mu.RLock()
	var sh *Share
	for _, x := range s.shares {
		if x.Name == parts[0] {
			y := x
			sh = &y
			break
		}
	}
	s.mu.RUnlock()
	if sh == nil {
		http.NotFound(lw, r)
		return
	}
	base, _ := filepath.Abs(sh.Path)
	target := base
	if len(parts) > 1 {
		target = filepath.Join(append([]string{base}, parts[1:]...)...)
	}
	resolved, _ := filepath.Abs(target)
	if !pathWithinRoot(resolved, base) {
		http.Error(lw, "forbidden", 403)
		return
	}
	realBase, baseErr := filepath.EvalSymlinks(base)
	realTarget, targetErr := filepath.EvalSymlinks(resolved)
	if baseErr != nil || targetErr != nil || !pathWithinRoot(realTarget, realBase) {
		http.Error(lw, "forbidden", http.StatusForbidden)
		return
	}
	resolved = realTarget
	info, err := os.Stat(resolved)
	if err != nil {
		http.NotFound(lw, r)
		return
	}
	if r.Method == http.MethodPost {
		if !info.IsDir() {
			http.Error(lw, "只能上传到目录", http.StatusBadRequest)
			return
		}
		if requestIsMultipart(r) {
			operation = "上传文件"
			if !upload {
				http.Error(lw, "上传功能未启用", http.StatusForbidden)
				return
			}
			s.handleUpload(lw, r, resolved)
		} else {
			operation = "管理文件"
			if !manage {
				http.Error(lw, "管理功能未启用", http.StatusForbidden)
				return
			}
			s.handleManage(lw, r, resolved)
		}
		return
	}
	if info.IsDir() {
		if r.URL.Query().Get("archive") == "1" {
			operation = "下载文件夹"
			if !download {
				http.Error(lw, "下载功能已关闭", http.StatusForbidden)
				return
			}
			s.serveArchive(r.Context(), lw, resolved, info.Name())
			return
		}
		operation = "浏览目录"
		s.renderDir(lw, r, resolved, info.Name())
		return
	}
	operation = "下载文件"
	if !download {
		http.Error(lw, "下载功能已关闭", http.StatusForbidden)
		return
	}
	disposition := "attachment"
	mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(info.Name())))
	if strings.HasPrefix(mediaType, "image/") && mediaType != "image/svg+xml" || strings.HasPrefix(mediaType, "audio/") || strings.HasPrefix(mediaType, "video/") || mediaType == "application/pdf" || mediaType == "text/plain; charset=utf-8" {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename*=UTF-8''%s", disposition, url.PathEscape(info.Name())))
	http.ServeFile(lw, r, resolved)
}

func (s *Server) beginHTTPHandler(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	s.handlerMu.Lock()
	defer s.handlerMu.Unlock()
	if !s.handlerAccepting || ctx.Err() != nil {
		return false
	}
	s.handlerWG.Add(1)
	return true
}

func sameWriteOrigin(r *http.Request) bool {
	rawOrigin := strings.TrimSpace(r.Header.Get("Origin"))
	if rawOrigin == "" {
		rawOrigin = strings.TrimSpace(r.Header.Get("Referer"))
	}
	if rawOrigin == "" {
		// Preserve compatibility with native clients such as curl. Browsers send
		// Origin or Referer for cross-site POST requests.
		return true
	}
	origin, err := url.Parse(rawOrigin)
	if err != nil || origin.User != nil || origin.Host == "" {
		return false
	}
	expectedScheme := "http"
	if requestIsHTTPS(r) {
		expectedScheme = "https"
	}
	return strings.EqualFold(origin.Scheme, expectedScheme) && strings.EqualFold(origin.Host, r.Host)
}

func pathWithinRoot(target, root string) bool {
	rootAbsolute, rootErr := filepath.Abs(root)
	targetAbsolute, targetErr := filepath.Abs(target)
	if rootErr != nil || targetErr != nil {
		return false
	}
	relative, err := filepath.Rel(rootAbsolute, targetAbsolute)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)))
}

func requestOperation(r *http.Request) string {
	switch {
	case r.URL.Path == "/__hfs/client/download":
		return "下载客户端"
	case r.URL.Path == "/__hfs/chat/ws":
		return "建立聊天连接"
	case r.URL.Path == "/__hfs/chat/status":
		return "查询聊天状态"
	case r.URL.Path == "/__hfs/chat/archive":
		return "查询聊天归档"
	case strings.HasPrefix(r.URL.Path, "/__hfs/chat/file/"):
		return "下载聊天附件"
	case r.URL.Path == "/__hfs/logo.png":
		return "读取界面资源"
	case r.Method == http.MethodPost && requestIsMultipart(r):
		return "上传文件"
	case r.Method == http.MethodPost:
		return "管理文件"
	case r.URL.Path == "/":
		return "访问首页"
	default:
		return "访问文件"
	}
}

func requestIsMultipart(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "multipart/form-data")
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, dir string) {
	paths, status, message := saveUploadedFiles(w, r, dir)
	if status != 0 {
		http.Error(w, message, status)
		return
	}
	writeUploadResponse(w, r, len(paths))
}

func (s *Server) handleFallbackUpload(w http.ResponseWriter, r *http.Request, dir string) {
	paths, status, message := saveUploadedFiles(w, r, dir)
	if status != 0 {
		http.Error(w, message, status)
		return
	}
	if err := s.addMany(paths, true); err != nil {
		removeUploadedFiles(paths)
		http.Error(w, "上传文件已保存，但无法加入分享列表", http.StatusInternalServerError)
		return
	}
	s.mu.RLock()
	notify := s.shareChangeNotify
	s.mu.RUnlock()
	if notify != nil {
		notify()
	}
	writeUploadResponse(w, r, len(paths))
}

// saveUploadedFiles saves the multipart files and returns their final paths.
// A non-zero status is an HTTP status and message suitable for the caller.
func saveUploadedFiles(w http.ResponseWriter, r *http.Request, dir string) ([]string, int, string) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, http.StatusInternalServerError, "上传目录不可写或不可用"
	}
	r.Body = http.MaxBytesReader(w, r.Body, 10<<30)
	if err = r.ParseMultipartForm(32 << 20); err != nil {
		return nil, http.StatusBadRequest, "上传请求无效"
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		return nil, http.StatusBadRequest, "没有选择文件"
	}
	paths := make([]string, 0, len(files))
	fail := func(status int, message string) ([]string, int, string) {
		removeUploadedFiles(paths)
		return nil, status, message
	}
	for _, header := range files {
		name := filepath.Base(filepath.ToSlash(header.Filename))
		if !validLeafName(name) {
			continue
		}
		src, err := header.Open()
		if err != nil {
			return fail(http.StatusInternalServerError, "无法读取上传内容")
		}
		target := uniquePath(dir, name)
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		created := err == nil
		if err == nil {
			_, err = copyWithContext(r.Context(), dst, src)
			if closeErr := dst.Close(); err == nil {
				err = closeErr
			}
		}
		_ = src.Close()
		if err != nil {
			if created {
				_ = os.Remove(target)
			}
			return fail(http.StatusInternalServerError, "上传目录不可写，无法保存文件")
		}
		paths = append(paths, target)
	}
	if len(paths) == 0 {
		return nil, http.StatusBadRequest, "没有可上传的有效文件"
	}
	return paths, 0, ""
}

func removeUploadedFiles(paths []string) {
	for _, uploadedPath := range paths {
		_ = os.Remove(uploadedPath)
	}
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 64<<10)
	var written int64
	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			count, writeErr := destination.Write(buffer[:read])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != read {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func writeUploadResponse(w http.ResponseWriter, r *http.Request, uploaded int) {
	if r.Header.Get("X-HFS-Upload") == "1" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]int{"uploaded": uploaded})
		return
	}
	http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
}

func uniquePath(dir, name string) string {
	target := filepath.Join(dir, name)
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return target
	}
	ext, base := filepath.Ext(name), strings.TrimSuffix(name, filepath.Ext(name))
	for i := 2; ; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
}

func (s *Server) handleManage(w http.ResponseWriter, r *http.Request, dir string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求无效", 400)
		return
	}
	switch r.Form.Get("action") {
	case "mkdir":
		name := filepath.Base(strings.TrimSpace(r.Form.Get("name")))
		if !validLeafName(name) {
			http.Error(w, "目录名称无效", 400)
			return
		}
		if err := os.Mkdir(filepath.Join(dir, name), 0755); err != nil {
			http.Error(w, "创建目录失败", 400)
			return
		}
	case "delete":
		name := filepath.Base(r.Form.Get("name"))
		if !validLeafName(name) {
			http.Error(w, "名称无效", 400)
			return
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			http.Error(w, "删除失败", 500)
			return
		}
	case "rename":
		oldName, newName := filepath.Base(r.Form.Get("name")), strings.TrimSpace(r.Form.Get("new_name"))
		if !validLeafName(oldName) || !validLeafName(newName) {
			http.Error(w, "名称无效", 400)
			return
		}
		oldPath, newPath := filepath.Join(dir, oldName), filepath.Join(dir, newName)
		if _, err := os.Stat(newPath); err == nil {
			http.Error(w, "目标名称已存在", http.StatusConflict)
			return
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			http.Error(w, "重命名失败", 500)
			return
		}
	default:
		http.Error(w, "未知操作", 400)
		return
	}
	http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
}

func validLeafName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}

func (s *Server) serveArchive(ctx context.Context, w http.ResponseWriter, dir, name string) {
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s.tar", url.PathEscape(name)))
	tw := tar.NewWriter(w)
	defer tw.Close()
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return nil
		}
		h.Name = filepath.ToSlash(rel)
		if err = tw.WriteHeader(h); err != nil || info.IsDir() {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		_, err = copyWithContext(ctx, tw, f)
		return err
	})
}

type statusWriter struct {
	http.ResponseWriter
	status *int
}

func (w *statusWriter) WriteHeader(n int) { *w.status = n; w.ResponseWriter.WriteHeader(n) }

type entry struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Size     string `json:"size"`
	Modified string `json:"modified"`
	Dir      bool   `json:"dir"`
}
type pageData struct {
	Title, Parent  string
	Version        string
	Entries        []entry
	Upload         bool
	Manage         bool
	Chat           bool
	Files          bool
	UserList       bool
	PrivateChat    bool
	GroupChat      bool
	ClientDownload bool
	LayoutClass    string
	Query          string
	ArchiveURL     string
	CanUpload      bool
	UploadHint     string
}

func (s *Server) resetVisitorSessions() {
	s.visitorMu.Lock()
	s.visitorSeen = make(map[string]time.Time)
	s.visitorMu.Unlock()
}

func (s *Server) trackPageVisitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		return
	}
	if _, _, err := s.ensureChatSession(w, r); err != nil {
		if err != nil {
			s.logger.Printf("创建访客会话失败: %v", err)
		}
		return
	}
	info := clientInfoFromRequest(r)
	visitorID := info.IP
	if visitorID == "" {
		return
	}
	newUser := s.ObserveChatUser(info)
	s.visitorMu.Lock()
	if _, seen := s.visitorSeen[visitorID]; seen {
		s.visitorSeen[visitorID] = info.ConnectedAt
		s.visitorMu.Unlock()
		return
	}
	if len(s.visitorSeen) >= maxVisitorSessions {
		var oldestID string
		var oldest time.Time
		for id, visitedAt := range s.visitorSeen {
			if oldestID == "" || visitedAt.Before(oldest) {
				oldestID, oldest = id, visitedAt
			}
		}
		delete(s.visitorSeen, oldestID)
	}
	s.visitorSeen[visitorID] = info.ConnectedAt
	notify := s.visitorNotify
	s.visitorMu.Unlock()
	if notify != nil && (newUser || s.Persistence() == nil) {
		notify(info)
	}
}

func (s *Server) renderRoot(w http.ResponseWriter, r *http.Request) {
	items := s.Shares()
	es := make([]entry, 0, len(items))
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	for _, x := range items {
		if query != "" && !strings.Contains(strings.ToLower(x.Name), query) {
			continue
		}
		st, e := os.Stat(x.Path)
		if e == nil {
			es = append(es, entry{Name: x.Name, URL: "/" + url.PathEscape(x.Name), Size: formatSize(st.Size()), Modified: st.ModTime().Format("2006-01-02 15:04"), Dir: st.IsDir()})
		}
	}
	s.mu.RLock()
	upload, download, fallbackUploadDir := s.allowUpload, s.allowDownload, s.fallbackUploadDir
	s.mu.RUnlock()
	hint := ""
	rootUpload := upload && fallbackUploadDir != ""
	if rootUpload {
		hint = "可直接上传文件；每个文件会作为独立分享显示，同名文件不会覆盖。"
	} else if upload {
		hint = "上传已开启：请点击共享文件夹右侧的“上传”，文件不能作为上传目标。"
	}
	s.trackPageVisitor(w, r)
	chatEnabled := s.ChatEnabled()
	userList := chatEnabled && s.UserListEnabled()
	filesEnabled := upload || download
	s.render(w, r, pageData{Title: "LanChatGo - 聊天与文件分享", Entries: es, Upload: rootUpload, Query: r.URL.Query().Get("q"), CanUpload: upload, UploadHint: hint, Chat: chatEnabled, Files: filesEnabled, UserList: userList, PrivateChat: userList && s.PrivateMessagesEnabled(), GroupChat: s.GroupChatEnabled(), ClientDownload: s.ClientDownloadEnabled(), LayoutClass: portalLayoutClass(filesEnabled, userList, chatEnabled)})
}
func (s *Server) renderDir(w http.ResponseWriter, r *http.Request, dir, title string) {
	list, err := os.ReadDir(dir)
	if err != nil {
		http.Error(w, "cannot read directory", 403)
		return
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].IsDir() != list[j].IsDir() {
			return list[i].IsDir()
		}
		return strings.ToLower(list[i].Name()) < strings.ToLower(list[j].Name())
	})
	es := make([]entry, 0, len(list))
	base := strings.TrimSuffix(r.URL.Path, "/")
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	for _, x := range list {
		if query != "" && !strings.Contains(strings.ToLower(x.Name()), query) {
			continue
		}
		st, e := x.Info()
		if e != nil {
			continue
		}
		es = append(es, entry{Name: x.Name(), URL: base + "/" + url.PathEscape(x.Name()), Size: formatSize(st.Size()), Modified: st.ModTime().Format("2006-01-02 15:04"), Dir: x.IsDir()})
	}
	parent := path.Dir(base)
	if parent == "." {
		parent = "/"
	}
	s.mu.RLock()
	upload, download, manage := s.allowUpload, s.allowDownload, s.allowManage
	s.mu.RUnlock()
	s.trackPageVisitor(w, r)
	chatEnabled := s.ChatEnabled()
	userList := chatEnabled && s.UserListEnabled()
	filesEnabled := upload || download
	s.render(w, r, pageData{Title: title, Parent: parent, Entries: es, Upload: upload, CanUpload: upload, Manage: manage, Query: r.URL.Query().Get("q"), ArchiveURL: base + "?archive=1", Chat: chatEnabled, Files: filesEnabled, UserList: userList, PrivateChat: userList && s.PrivateMessagesEnabled(), GroupChat: s.GroupChatEnabled(), ClientDownload: s.ClientDownloadEnabled(), LayoutClass: portalLayoutClass(filesEnabled, userList, chatEnabled)})
}

type fileListResponse struct {
	Path       string  `json:"path"`
	Title      string  `json:"title"`
	Parent     string  `json:"parent,omitempty"`
	Query      string  `json:"query,omitempty"`
	ArchiveURL string  `json:"archiveUrl,omitempty"`
	Upload     bool    `json:"upload"`
	UploadHint string  `json:"uploadHint,omitempty"`
	Entries    []entry `json:"entries"`
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, d pageData) {
	if r.Method == http.MethodGet && r.URL.Query().Get("__hfs_files") == "1" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(fileListResponse{
			Path:       r.URL.Path,
			Title:      d.Title,
			Parent:     d.Parent,
			Query:      d.Query,
			ArchiveURL: d.ArchiveURL,
			Upload:     d.Upload,
			UploadHint: d.UploadHint,
			Entries:    d.Entries,
		})
		return
	}
	d.Version = appinfo.Version
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := portalTemplate.Execute(w, d); err != nil {
		s.logger.Print(err)
	}
}
func formatSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1<<20 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	if n < 1<<30 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}

/*
The legacy page template below is kept in source history for reference. The active
browser UI is portalTemplate in portal.go.
:root{color-scheme:light;--bg:#f4f7fb;--surface:#fff;--line:#e3e9f2;--text:#172033;--muted:#718096;--blue:#2878ff;--blue2:#58a0ff;--soft:#eaf2ff;--green:#19a974;--red:#e45656;--shadow:0 10px 30px rgba(28,47,77,.08);font-family:Inter,"Segoe UI","Microsoft YaHei UI",system-ui,sans-serif}
*{box-sizing:border-box}html,body{height:100%;margin:0}body{background:linear-gradient(145deg,#f7f9fc,#edf3fa);color:var(--text);overflow:hidden}button,input,textarea{font:inherit}a{color:inherit;text-decoration:none}button,.btn{border:1px solid var(--line);border-radius:10px;background:#fff;color:#35435a;padding:9px 14px;font-weight:680;cursor:pointer;transition:.15s}.btn:hover,button:hover:not(:disabled){border-color:#b8d3ff;color:var(--blue);transform:translateY(-1px)}button:disabled{cursor:not-allowed;opacity:.48}.primary,.tools button{border-color:var(--blue);background:linear-gradient(135deg,var(--blue),var(--blue2));color:#fff;box-shadow:0 7px 16px rgba(40,120,255,.18)}
.topbar{height:78px;display:flex;align-items:center;padding:0 clamp(18px,3vw,40px);background:rgba(255,255,255,.96);border-bottom:1px solid var(--line);box-shadow:0 4px 18px rgba(34,53,84,.05)}.brand{display:flex;align-items:center;gap:13px}.brand-logo{width:46px;height:46px;border-radius:14px;object-fit:cover;box-shadow:0 8px 22px rgba(31,190,165,.2)}.brand-name{font-size:21px;font-weight:800}.brand-version{display:inline-flex;margin-left:7px;padding:2px 7px;border-radius:999px;background:var(--soft);color:var(--blue);font-size:10px;vertical-align:middle}.brand-sub{margin-top:2px;color:var(--muted);font-size:12px}.top-chip{margin-left:auto;display:flex;align-items:center;gap:8px;padding:9px 13px;border-radius:12px;background:#eaf9f3;color:#16875f;font-size:13px}.top-chip:before{content:"";width:8px;height:8px;border-radius:50%;background:var(--green);box-shadow:0 0 0 5px rgba(25,169,116,.1)}
.portal-grid{width:min(1540px,calc(100% - 32px));height:calc(100vh - 110px);height:calc(100dvh - 110px);min-height:570px;margin:16px auto;display:grid;grid-template-columns:minmax(0,1fr) minmax(0,1fr);gap:16px}.workspace-card{min-width:0;min-height:0;overflow:hidden;border:1px solid var(--line);border-radius:17px;background:var(--surface);box-shadow:var(--shadow)}.section-head{min-height:76px;display:flex;align-items:center;gap:12px;padding:16px 18px;border-bottom:1px solid var(--line)}.section-icon{width:42px;height:42px;display:grid;place-items:center;border-radius:13px;background:var(--soft);color:var(--blue);font-size:20px;font-weight:800}.section-copy{min-width:0}.section-title{font-size:18px;font-weight:790}.section-subtitle{margin-top:4px;color:var(--muted);font-size:12px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.file-panel{display:flex;flex-direction:column}.tools{display:flex;align-items:center;flex-wrap:wrap;gap:8px;padding:12px 14px;border-bottom:1px solid var(--line);background:#fbfcfe}.tools form{display:flex;min-width:0;gap:7px}.tools form:first-child{flex:1}.tools button,.tools>.btn{flex:0 0 auto;white-space:nowrap}.tools form:first-child button{min-width:64px}.tools input{min-width:0;height:39px;padding:0 11px;border:1px solid #d9e2ed;border-radius:10px;background:#fff;color:var(--text);outline:none}.tools form:first-child input{width:100%}.tools input:focus{border-color:#7fb0ff;box-shadow:0 0 0 3px rgba(40,120,255,.1)}.notice{padding:10px 16px;border-bottom:1px solid #dce9ff;background:#f2f7ff;color:#4873ad;font-size:12px}.upload{padding:12px 14px;border-bottom:1px solid var(--line);background:#f8fbfe}.upload-file{position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0 0 0 0)}.upload-drop{display:flex;align-items:center;justify-content:center;gap:9px;min-height:72px;padding:13px;border:2px dashed #a9c7e7;border-radius:12px;background:#fff;color:#526984;text-align:center;cursor:pointer;transition:.18s}.upload-drop strong{color:var(--blue);font-size:14px}.upload-drop small{color:var(--muted);font-size:11px}.upload-drop.dragging{border-color:var(--blue);background:var(--soft);transform:translateY(-1px);box-shadow:0 7px 18px rgba(40,120,255,.1)}.upload-drop.busy{cursor:wait;opacity:.75}.upload-progress{height:7px;margin-top:10px;border-radius:999px;background:#dce7f0;overflow:hidden}.upload-progress[hidden]{display:none}.upload-progress-bar{width:0;height:100%;border-radius:inherit;background:linear-gradient(90deg,var(--blue),#35c1d7);transition:width .12s linear}.upload-status{display:block;min-height:17px;margin-top:6px;color:#5e7185;font-size:12px}.upload-status.error{color:#b23838}.file-list{min-height:0;flex:1;overflow:auto}.row{display:grid;grid-template-columns:minmax(0,1fr) 78px 128px auto;gap:9px;align-items:center;min-height:57px;padding:10px 15px;border-bottom:1px solid #edf1f6;transition:.14s}.row:hover{background:#f8fbff}.row-name{display:flex;align-items:center;gap:10px;min-width:0;font-weight:680}.file-icon{width:34px;height:34px;display:grid;place-items:center;flex:0 0 auto;border-radius:10px;background:#edf4ff;font-size:17px}.name-text{white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.meta{color:var(--muted);text-align:right;font-size:12px}.row-actions{text-align:right;white-space:nowrap}.row-actions .btn,.row-actions button{padding:6px 9px;border-radius:8px;font-size:12px}.danger{border-color:transparent;background:transparent;color:var(--red);box-shadow:none}.empty-row{display:grid;place-items:center;min-height:180px;color:var(--muted);text-align:center}
.chat-launcher{display:none!important}.chat-panel{position:relative;display:flex;flex-direction:column}.chat-panel.dragging:after{content:"释放图片即可发送";position:absolute;inset:9px;z-index:5;display:flex;align-items:center;justify-content:center;border:2px dashed var(--blue);border-radius:14px;background:rgba(234,242,255,.96);color:var(--blue);font-size:17px;font-weight:750;pointer-events:none}.chat-panel.disabled .chat-notify-row{display:none}.chat-heading-status{margin-left:auto;display:flex;align-items:center;gap:7px;color:var(--muted);font-size:12px}.chat-heading-status:before{content:"";width:8px;height:8px;border-radius:50%;background:#adb7c6}.chat-panel:not(.disabled) .chat-heading-status:before{background:var(--green)}.chat-close{display:none}.chat-notify-row{display:flex;align-items:center;gap:9px;padding:8px 14px;border-bottom:1px solid var(--line);background:#f8fbfe}.chat-notify{flex:0 0 auto;padding:6px 10px;background:#e8f1f9;color:#1769aa;font-size:12px}.chat-notify:disabled{cursor:not-allowed;opacity:.65}.chat-notify-state{min-width:0;color:#66758a;font-size:11px;line-height:1.3}.chat-notify-state.error{color:#a33838}.chat-status{padding:7px 14px;border-bottom:1px solid #f1e3a6;background:#fff8dd;color:#806717;font-size:12px}.chat-status.online{border-color:#d5eddf;background:#e9f8ef;color:#287846}.chat-messages{min-height:0;flex:1;overflow-y:auto;padding:16px;background:linear-gradient(180deg,#f9fbfd,#f3f6fa)}.chat-empty{height:100%;display:grid;place-items:center;margin:0;color:#78869a;text-align:center}.chat-message{display:flex;flex-direction:column;margin:9px 0}.chat-message.mine{align-items:flex-end}.chat-message.admin,.chat-message.other{align-items:flex-start}.chat-message small{margin:0 5px 4px;color:#8290a3;font-size:11px}.chat-bubble{max-width:80%;padding:10px 13px;border:1px solid var(--line);border-radius:5px 15px 15px 15px;background:#fff;box-shadow:0 3px 10px rgba(28,47,77,.05);white-space:pre-wrap;overflow-wrap:anywhere;line-height:1.55}.chat-message.mine .chat-bubble{border-color:transparent;border-radius:15px 5px 15px 15px;background:linear-gradient(135deg,var(--blue),var(--blue2));color:#fff}.chat-message.admin .chat-bubble{border-color:#acd1ee;background:#edf7ff;color:#174c72}.chat-bubble.image{padding:5px;overflow:hidden}.chat-image-link{display:block;padding:0;border:0;border-radius:10px;background:transparent;line-height:0;overflow:hidden;box-shadow:none}.chat-image-link:hover{transform:none}.chat-image{display:block;max-width:min(350px,100%);max-height:360px;object-fit:contain;cursor:zoom-in}.chat-image-invalid{color:#a33838;font-size:13px}.chat-form{display:grid;grid-template-columns:minmax(0,1fr) auto;align-items:end;gap:9px;padding:12px 14px;padding-bottom:max(12px,env(safe-area-inset-bottom));border-top:1px solid var(--line);background:#fff}.chat-compose{min-width:0}.chat-form textarea{width:100%;min-height:68px;max-height:150px;resize:vertical;padding:10px 11px;border:1px solid #d5deea;border-radius:11px;background:#fbfcfe;font:inherit;font-size:14px;line-height:1.5;outline:none}.chat-form textarea:focus{border-color:#7fb0ff;background:#fff;box-shadow:0 0 0 3px rgba(40,120,255,.1)}.chat-compose-tools{display:flex;align-items:center;min-height:21px;padding-top:4px}.chat-compose-note{min-width:0;flex:1;color:var(--muted);font-size:11px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.chat-compose-note.error{color:#b23838}.chat-form>button{min-width:78px;border-color:var(--blue);background:linear-gradient(135deg,var(--blue),var(--blue2));color:#fff}.image-viewer{position:fixed;inset:0;z-index:50;display:none;align-items:center;justify-content:center;background:rgba(7,12,22,.92);backdrop-filter:blur(5px)}.image-viewer.show{display:flex}.image-viewer img{max-width:calc(100vw - 150px);max-height:calc(100vh - 60px);object-fit:contain;border-radius:9px}.image-viewer-close{position:absolute;right:18px;top:18px;width:42px;height:42px;border:0;border-radius:50%;background:rgba(255,255,255,.14);color:#fff;font-size:25px}.image-viewer-nav{position:absolute;right:18px;top:50%;display:flex;flex-direction:column;gap:10px;transform:translateY(-50%)}.image-viewer-nav button{width:52px;height:52px;padding:0;border:0;border-radius:50%;background:rgba(255,255,255,.14);color:#fff;font-size:22px}.image-viewer-counter{position:absolute;left:50%;bottom:16px;transform:translateX(-50%);padding:7px 12px;border-radius:999px;background:rgba(255,255,255,.14);color:#fff;font-size:12px}
@media(max-width:1180px){.row{grid-template-columns:minmax(0,1fr) 72px auto}.row .modified{display:none}.tools form:first-child{flex-basis:100%}}
@media(max-width:900px){html,body{height:auto}body{overflow:auto}.portal-grid{height:auto;min-height:0;grid-template-columns:1fr}.workspace-card{min-height:620px}.top-chip{display:none}.file-panel{max-height:760px}.chat-panel{height:720px}}
@media(max-width:560px){.topbar{height:68px;padding:0 13px}.brand-logo{width:40px;height:40px}.brand-name{font-size:18px}.portal-grid{width:calc(100% - 20px);margin:10px auto;gap:10px}.workspace-card{border-radius:14px}.section-head{min-height:68px;padding:12px}.tools{align-items:stretch}.tools form{width:100%}.tools form:first-child{flex-basis:auto}.tools input{flex:1}.row{grid-template-columns:minmax(0,1fr) auto}.row .size,.row .modified{display:none}.upload-drop{flex-direction:column}.chat-form{grid-template-columns:1fr}.chat-form>button{width:100%}}
</style></head><body>
<header class="topbar"><div class="brand"><img class="brand-logo" src="/__hfs/logo.png" alt="LanChatGo"><div><div class="brand-name">LanChatGo <span class="brand-version">v{{.Version}}</span></div><div class="brand-sub">局域网聊天与文件分享</div></div></div><div class="top-chip">服务已连接</div></header>
<main class="portal-grid">
<section class="workspace-card file-panel" aria-label="共享文件">
<div class="section-head"><div class="section-icon">▤</div><div class="section-copy"><div class="section-title">共享文件</div><div class="section-subtitle">{{.Title}}</div></div></div>
<div class="tools"><form method="get"><input name="q" value="{{.Query}}" placeholder="搜索当前目录"><button>搜索</button></form>{{if .ArchiveURL}}<a class="btn" href="{{.ArchiveURL}}">打包下载</a>{{end}}</div>
{{if .UploadHint}}<div class="notice">{{.UploadHint}}</div>{{end}}
{{if .Upload}}<form id="upload" class="upload" method="post" enctype="multipart/form-data"><input id="upload-files" class="upload-file" name="files" type="file" multiple><label id="upload-drop" class="upload-drop" for="upload-files" tabindex="0"><strong>拖拽文件到这里上传</strong><small>或点击选择多个文件；选择后自动开始，同名文件不会覆盖</small></label><div id="upload-progress" class="upload-progress" role="progressbar" aria-label="上传进度" aria-valuemin="0" aria-valuemax="100" aria-valuenow="0" hidden><div id="upload-progress-bar" class="upload-progress-bar"></div></div><span id="upload-status" class="upload-status" role="status" aria-live="polite"></span></form>{{end}}
<div class="file-list">
{{if .Parent}}<a class="row" href="{{.Parent}}"><span class="row-name"><span class="file-icon">↰</span><span class="name-text">返回上一级</span></span></a>{{end}}
{{range .Entries}}<div class="row"><a class="row-name" href="{{.URL}}"><span class="file-icon">{{if .Dir}}▰{{else}}▱{{end}}</span><span class="name-text">{{.Name}}</span></a><span class="meta size">{{if not .Dir}}{{.Size}}{{end}}</span><span class="meta modified">{{.Modified}}</span><span class="row-actions">{{if and $.CanUpload .Dir}}<a class="btn" href="{{.URL}}#upload">上传</a>{{end}}</span></div>{{else}}<div class="empty-row"><div><div style="font-size:40px;margin-bottom:10px">▱</div>此处暂无文件</div></div>{{end}}
</div></section>
<button id="chat-launcher" class="chat-launcher" type="button" aria-controls="chat-panel" aria-expanded="true" hidden>在线聊天<span id="chat-badge" class="chat-badge"></span></button>
<section id="chat-panel" class="workspace-card chat-panel open {{if not .Chat}}disabled{{end}}" data-enabled="{{if .Chat}}1{{else}}0{{end}}" aria-label="在线聊天">
<div class="section-head"><div class="section-icon">●</div><div class="section-copy"><div class="section-title">在线聊天</div><div id="chat-title" class="section-subtitle">与管理员聊天</div></div><div id="chat-heading-status" class="chat-heading-status">{{if .Chat}}等待连接{{else}}暂未开放{{end}}</div><button id="chat-close" class="chat-close" type="button" aria-label="关闭" hidden>×</button></div>
<div id="chat-notify-row" class="chat-notify-row"><button id="chat-notify" class="chat-notify" type="button" aria-pressed="false">允许订阅提醒</button><span id="chat-notify-state" class="chat-notify-state" role="status" aria-live="polite"></span></div>
<div id="chat-status" class="chat-status">{{if .Chat}}正在连接…{{else}}聊天暂未开启{{end}}</div>
<div id="chat-messages" class="chat-messages" role="log" aria-live="polite"><div class="chat-empty">发送消息后，管理员可在服务器后台回复你。</div></div>
<form id="chat-form" class="chat-form"><div class="chat-compose"><textarea id="chat-text" rows="2" maxlength="32768" placeholder="输入消息，Enter 发送；可粘贴或拖入图片"></textarea><div class="chat-compose-tools"><span id="chat-compose-note" class="chat-compose-note" aria-live="polite">支持长文本，以及粘贴或拖入 PNG/JPEG 图片</span></div></div><button id="chat-submit" type="submit" disabled>发送</button></form>
</section></main><div id="image-viewer" class="image-viewer" role="dialog" aria-modal="true" aria-label="图片预览"><button id="image-viewer-close" class="image-viewer-close" aria-label="关闭">×</button><img id="image-viewer-image" alt="聊天图片预览"><div class="image-viewer-nav"><button id="image-viewer-prev" title="上一张">↑</button><button id="image-viewer-next" title="下一张">↓</button></div><div id="image-viewer-counter" class="image-viewer-counter"></div></div>
<script>(function(){
'use strict';
var uploadForm=document.getElementById('upload'),uploadInput=document.getElementById('upload-files'),uploadDrop=document.getElementById('upload-drop'),uploadProgress=document.getElementById('upload-progress'),uploadProgressBar=document.getElementById('upload-progress-bar'),uploadStatus=document.getElementById('upload-status'),uploadBusy=false;
function uploadSize(bytes){if(bytes<1024)return bytes+' B';if(bytes<1048576)return (bytes/1024).toFixed(1)+' KB';if(bytes<1073741824)return (bytes/1048576).toFixed(1)+' MB';return (bytes/1073741824).toFixed(1)+' GB'}
function setUploadProgress(percent){percent=Math.max(0,Math.min(100,percent||0));uploadProgress.hidden=false;uploadProgressBar.style.width=percent.toFixed(1)+'%';uploadProgress.setAttribute('aria-valuenow',String(Math.round(percent)))}
function uploadFiles(fileList){if(!uploadForm||uploadBusy)return;var files=Array.prototype.slice.call(fileList||[]);if(!files.length)return;var total=files.reduce(function(sum,file){return sum+(file.size||0)},0),payload=new FormData();files.forEach(function(file){payload.append('files',file,file.name)});uploadBusy=true;uploadDrop.classList.add('busy');uploadStatus.classList.remove('error');uploadStatus.textContent='准备上传 '+files.length+' 个文件，共 '+uploadSize(total);setUploadProgress(0);var request=new XMLHttpRequest();request.open('POST',uploadForm.getAttribute('action')||location.href,true);request.setRequestHeader('X-HFS-Upload','1');request.upload.onprogress=function(event){if(!event.lengthComputable)return;var percent=event.total?event.loaded/event.total*100:0;setUploadProgress(percent);uploadStatus.textContent=percent>=100?'上传完成，服务器正在保存…':'正在上传 '+percent.toFixed(0)+'% · '+uploadSize(event.loaded)+' / '+uploadSize(event.total)};request.onload=function(){if(request.status>=200&&request.status<300){setUploadProgress(100);uploadStatus.textContent='上传完成，正在刷新文件列表…';setTimeout(function(){location.reload()},450);return}uploadBusy=false;uploadDrop.classList.remove('busy');uploadStatus.classList.add('error');uploadStatus.textContent=(request.responseText||'上传失败').replace(/\s+/g,' ').trim().slice(0,160)};request.onerror=function(){uploadBusy=false;uploadDrop.classList.remove('busy');uploadStatus.classList.add('error');uploadStatus.textContent='网络连接中断，上传失败'};request.onabort=function(){uploadBusy=false;uploadDrop.classList.remove('busy');uploadStatus.classList.add('error');uploadStatus.textContent='上传已取消'};request.send(payload)}
function initUpload(){if(!uploadForm)return;uploadInput.addEventListener('change',function(){var files=Array.prototype.slice.call(uploadInput.files||[]);uploadInput.value='';uploadFiles(files)});uploadDrop.addEventListener('click',function(event){if(uploadBusy)event.preventDefault()});uploadDrop.addEventListener('keydown',function(event){if(event.key==='Enter'||event.key===' '){event.preventDefault();if(!uploadBusy)uploadInput.click()}});['dragenter','dragover'].forEach(function(name){uploadDrop.addEventListener(name,function(event){event.preventDefault();event.stopPropagation();if(!uploadBusy){uploadDrop.classList.add('dragging');event.dataTransfer.dropEffect='copy'}})});uploadDrop.addEventListener('dragleave',function(event){event.preventDefault();if(!uploadDrop.contains(event.relatedTarget))uploadDrop.classList.remove('dragging')});uploadDrop.addEventListener('drop',function(event){event.preventDefault();event.stopPropagation();uploadDrop.classList.remove('dragging');if(!uploadBusy)uploadFiles(event.dataTransfer.files)});uploadForm.addEventListener('submit',function(event){event.preventDefault();if(!uploadBusy)uploadFiles(uploadInput.files)})}
var launcher=document.getElementById('chat-launcher'),panel=document.getElementById('chat-panel'),closeButton=document.getElementById('chat-close'),chatTitle=document.getElementById('chat-title');
var badge=document.getElementById('chat-badge'),statusBox=document.getElementById('chat-status'),messages=document.getElementById('chat-messages');
var form=document.getElementById('chat-form'),textBox=document.getElementById('chat-text'),submit=document.getElementById('chat-submit');
var headingStatus=document.getElementById('chat-heading-status');
var composeNote=document.getElementById('chat-compose-note'),defaultComposeNote='支持粘贴或拖入 PNG/JPEG 图片';
var notifyRow=document.getElementById('chat-notify-row'),notifyButton=document.getElementById('chat-notify'),notifyState=document.getElementById('chat-notify-state');
var imageViewer=document.getElementById('image-viewer'),imageViewerImage=document.getElementById('image-viewer-image'),imageViewerCounter=document.getElementById('image-viewer-counter'),viewerImages=[],viewerIndex=0;
function openImageViewer(source){viewerImages=Array.prototype.slice.call(messages.querySelectorAll('.chat-image')).map(function(image){return image.src});viewerIndex=Math.max(0,viewerImages.indexOf(source));updateImageViewer();imageViewer.classList.add('show')}
function updateImageViewer(){if(!viewerImages.length){closeImageViewer();return}viewerIndex=(viewerIndex+viewerImages.length)%viewerImages.length;imageViewerImage.src=viewerImages[viewerIndex];imageViewerCounter.textContent=(viewerIndex+1)+' / '+viewerImages.length;document.getElementById('image-viewer-prev').disabled=viewerImages.length<2;document.getElementById('image-viewer-next').disabled=viewerImages.length<2}
function switchImageViewer(offset){if(viewerImages.length){viewerIndex+=offset;updateImageViewer()}}
function closeImageViewer(){imageViewer.classList.remove('show');imageViewerImage.removeAttribute('src');viewerImages=[]}
document.getElementById('image-viewer-prev').addEventListener('click',function(event){event.stopPropagation();switchImageViewer(-1)});document.getElementById('image-viewer-next').addEventListener('click',function(event){event.stopPropagation();switchImageViewer(1)});document.getElementById('image-viewer-close').addEventListener('click',closeImageViewer);imageViewer.addEventListener('click',function(event){if(event.target===imageViewer)closeImageViewer()});document.addEventListener('keydown',function(event){if(!imageViewer.classList.contains('show'))return;if(event.key==='Escape')closeImageViewer();else if(event.key==='ArrowUp'||event.key==='ArrowLeft')switchImageViewer(-1);else if(event.key==='ArrowDown'||event.key==='ArrowRight')switchImageViewer(1)});
var socket=null,reconnectTimer=0,reconnectDelay=1000,unread=0,statusTimer=0,noteTimer=0;
var enabled=panel.dataset.enabled==='1',online=false,imageBusy=false,composing=false,currentClientID='',groupMode=false,seen=Object.create(null);
var audioContext=null,notificationSubscribed=false,notificationRequestPending=false,basePageTitle=document.title,MAX_IMAGE_BYTES=512*1024,MAX_IMAGE_INPUT_BYTES=32*1024*1024,MAX_IMAGE_EDGE=1280;
function stored(area,key){try{return area.getItem(key)||''}catch(e){return ''}}
function store(area,key,value){try{area.setItem(key,value)}catch(e){}}
notificationSubscribed=stored(localStorage,'hfs-chat-notify-enabled')==='1';
	function updateControls(){submit.disabled=!enabled||!online||imageBusy;textBox.disabled=!enabled||imageBusy}
function setStatus(text,isOnline){online=!!isOnline;statusBox.textContent=text;statusBox.classList.toggle('online',online);headingStatus.textContent=online?'已连接':(enabled?'连接中':'暂未开放');updateControls()}
function setComposeNote(text,isError,sticky){clearTimeout(noteTimer);composeNote.textContent=text||defaultComposeNote;composeNote.classList.toggle('error',!!isError);if(text&&!sticky)noteTimer=setTimeout(function(){composeNote.textContent=defaultComposeNote;composeNote.classList.remove('error')},4000)}
function markRead(){unread=0;updateBadge()}
	function setOpen(open){panel.classList.add('open');launcher.setAttribute('aria-expanded','true');if(open&&enabled){markRead();textBox.focus()}}
function updateBadge(){badge.textContent=unread>99?'99+':String(unread);badge.classList.toggle('on',unread>0);document.title=unread?'('+String(unread)+') 新消息 - '+basePageTitle:basePageTitle}
function clearMessages(){while(messages.firstChild)messages.removeChild(messages.firstChild);seen=Object.create(null)}
function timeLabel(value){var date=new Date(value);return isNaN(date.getTime())?'':date.toLocaleTimeString([],{hour:'2-digit',minute:'2-digit'})}
function emptyText(){return groupMode?'当前是系统群，所有在线访客都能看到新消息。':'发送消息后，管理员可在服务器后台回复你。'}
function updateMode(){chatTitle.textContent=groupMode?'系统群':'与管理员聊天';panel.setAttribute('aria-label',groupMode?'系统群':'与管理员聊天')}
function isOwnMessage(message){if(message.sender==='admin')return false;if(currentClientID&&message.clientId)return message.clientId===currentClientID;return !groupMode&&message.sender==='user'}
function messageName(message,mine){if(mine)return '我';if(message.sender==='admin')return '管理员（后台）';var value=typeof message.clientId==='string'?message.clientId.trim():'';value=value||'未知 IP';return groupMode?value+'（IP）':value}
function validImageData(message){var mime=typeof message.mime==='string'?message.mime.toLowerCase():'';var data=typeof message.data==='string'?message.data:'';if(mime!=='image/png'&&mime!=='image/jpeg')return false;if(!data||data.length>Math.ceil(MAX_IMAGE_BYTES*4/3)+4||data.length%4!==0||!/^[A-Za-z0-9+/]+={0,2}$/.test(data))return false;var raw;try{raw=atob(data)}catch(e){return false}if(raw.length>MAX_IMAGE_BYTES)return false;if(mime==='image/jpeg')return raw.length>=3&&raw.charCodeAt(0)===255&&raw.charCodeAt(1)===216&&raw.charCodeAt(2)===255;return raw.length>=8&&raw.charCodeAt(0)===137&&raw.slice(1,4)==='PNG'&&raw.charCodeAt(4)===13&&raw.charCodeAt(5)===10&&raw.charCodeAt(6)===26&&raw.charCodeAt(7)===10}
function appendImage(bubble,message,label){if(!validImageData(message)){bubble.classList.add('chat-image-invalid');bubble.textContent='[图片数据无效或格式不受支持]';return false}bubble.classList.add('image');var source='data:'+message.mime.toLowerCase()+';base64,'+message.data;var link=document.createElement('button');link.type='button';link.className='chat-image-link';link.title='点击放大预览';var image=document.createElement('img');image.className='chat-image';image.src=source;image.alt=label+'发送的图片';image.loading='lazy';image.decoding='async';link.addEventListener('click',function(){openImageViewer(image.src)});link.appendChild(image);bubble.appendChild(link);return true}
function notificationPreview(message){if(message.kind==='image')return '发来一张图片';var value=typeof message.text==='string'?message.text.replace(/\s+/g,' ').trim():'';return value.length>80?value.slice(0,80)+'…':(value||'发来一条消息')}
function emitBeep(){if(!audioContext||audioContext.state!=='running')return;try{var now=audioContext.currentTime,oscillator=audioContext.createOscillator(),gain=audioContext.createGain();oscillator.type='sine';oscillator.frequency.setValueAtTime(880,now);gain.gain.setValueAtTime(0.0001,now);gain.gain.exponentialRampToValueAtTime(0.11,now+0.015);gain.gain.exponentialRampToValueAtTime(0.0001,now+0.16);oscillator.connect(gain);gain.connect(audioContext.destination);oscillator.start(now);oscillator.stop(now+0.17)}catch(e){}}
function playAlert(){if(!audioContext)return;if(audioContext.state==='suspended'){try{var resumed=audioContext.resume();if(resumed&&resumed.then)resumed.then(emitBeep).catch(function(){})}catch(e){};return}emitBeep()}
function prepareAlertsFromGesture(){var AudioCtor=window.AudioContext||window.webkitAudioContext;if(!audioContext&&AudioCtor){try{audioContext=new AudioCtor()}catch(e){}}if(audioContext&&audioContext.state==='suspended'){try{audioContext.resume().catch(function(){})}catch(e){}}}
function setNotificationState(text,isError){notifyState.textContent=text||'';notifyState.classList.toggle('error',!!isError)}
function updateNotificationSubscriptionUI(){var supported='Notification' in window,subscribed=supported&&Notification.permission==='granted'&&notificationSubscribed;notifyRow.hidden=subscribed;notifyButton.classList.remove('on');notifyButton.setAttribute('aria-pressed','false');notifyButton.disabled=false;if(subscribed)return;if(notificationRequestPending){notifyButton.disabled=true;notifyButton.textContent='正在申请…';setNotificationState('请在浏览器弹窗中选择“允许”。',false);return}if(!supported){notifyButton.disabled=true;notifyButton.textContent='提醒不可用';setNotificationState('当前浏览器没有开放系统通知；仍会使用提示音、标题和未读角标。',true);return}if(Notification.permission==='denied'){notifyButton.disabled=true;notifyButton.textContent='提醒已被阻止';setNotificationState('请在地址栏的网站权限中允许通知。局域网 HTTP 是否可用由浏览器决定。',true);return}notifyButton.textContent='允许订阅提醒';setNotificationState(Notification.permission==='granted'?'系统权限已允许，点击开启本页提醒。':'点击后申请浏览器通知权限；局域网 HTTP 将直接交给浏览器尝试。',false)}
function setNotificationSubscription(value){notificationSubscribed=!!value;store(localStorage,'hfs-chat-notify-enabled',notificationSubscribed?'1':'0');updateNotificationSubscriptionUI()}
function requestNotificationSubscription(){prepareAlertsFromGesture();updateNotificationSubscriptionUI();if(notificationRequestPending||!('Notification' in window)||Notification.permission==='denied')return;if(Notification.permission==='granted'){setNotificationSubscription(!notificationSubscribed);return}notificationRequestPending=true;updateNotificationSubscriptionUI();var settled=false;function finish(permission){if(settled)return;settled=true;notificationRequestPending=false;if(permission==='granted'){setNotificationSubscription(true);return}setNotificationSubscription(false);if(permission==='default')setNotificationState('没有授予通知权限；需要时可再次点击申请。',true)}function failed(){if(settled)return;settled=true;notificationRequestPending=false;setNotificationSubscription(false);setNotificationState('浏览器拒绝了系统通知申请；仍会使用提示音、标题和未读角标。',true)}try{var requested=Notification.requestPermission(finish);if(requested&&typeof requested.then==='function')requested.then(finish,failed);else if(typeof requested==='string')finish(requested)}catch(e){failed()}}
function systemNotify(message,label){if(!notificationSubscribed||!('Notification' in window)||Notification.permission!=='granted')return false;try{var notice=new Notification(label+'的新消息',{body:notificationPreview(message),tag:'hfs-chat-'+(message.id||String(Date.now()))});notice.onclick=function(){window.focus();setOpen(true);notice.close()};notice.onerror=function(){playAlert()};return true}catch(e){return false}}
	function notifyIncoming(message,label){var background=document.hidden||!document.hasFocus(),notified=background&&systemNotify(message,label);if(!notified)playAlert();if(background){unread++;updateBadge()}}
function addMessage(message,notify){if(!message||typeof message!=='object')return;if(message.id&&seen[message.id])return;if(message.id)seen[message.id]=true;var mine=isOwnMessage(message),role=mine?'mine':(message.sender==='admin'?'admin':'other'),label=messageName(message,mine);var row=document.createElement('div');row.className='chat-message '+role;var meta=document.createElement('small');meta.textContent=label+(message.sentAt?' · '+timeLabel(message.sentAt):'');var bubble=document.createElement('div');bubble.className='chat-bubble';if(message.kind==='image')appendImage(bubble,message,label);else if(!message.kind||message.kind==='text')bubble.textContent=typeof message.text==='string'?message.text:'';else{bubble.classList.add('chat-image-invalid');bubble.textContent='[不支持的消息类型]'}row.appendChild(meta);row.appendChild(bubble);messages.appendChild(row);messages.scrollTop=messages.scrollHeight;if(notify&&!mine)notifyIncoming(message,label)}
function tabToken(){var token=stored(sessionStorage,'hfs-chat-tab');if(/^[0-9a-f]{32}$/i.test(token))return token;var bytes=new Uint8Array(16);if(window.crypto&&crypto.getRandomValues){crypto.getRandomValues(bytes);token=Array.prototype.map.call(bytes,function(value){return value.toString(16).padStart(2,'0')}).join('')}else{token='';for(var i=0;i<4;i++)token+=Math.floor(Math.random()*0x100000000).toString(16).padStart(8,'0')}store(sessionStorage,'hfs-chat-tab',token);return token}
function socketURL(){var scheme=location.protocol==='https:'?'wss://':'ws://';var query=new URLSearchParams();query.set('tab',tabToken());return scheme+location.host+'/__hfs/chat/ws?'+query.toString()}
function connect(){if(!enabled||socket&&(socket.readyState===WebSocket.CONNECTING||socket.readyState===WebSocket.OPEN||socket.readyState===WebSocket.CLOSING))return;clearTimeout(reconnectTimer);reconnectTimer=0;setStatus('正在连接…',false);var ws;try{ws=new WebSocket(socketURL());socket=ws}catch(e){socket=null;scheduleReconnect();return}
ws.onopen=function(){if(socket===ws)setStatus('正在建立聊天会话…',false)};
ws.onmessage=function(event){if(socket!==ws)return;var data;try{data=JSON.parse(event.data)}catch(e){return}if(data.type==='ready'){reconnectDelay=1000;currentClientID=typeof data.clientId==='string'?data.clientId:'';groupMode=!!data.group;updateMode();clearMessages();var history=Array.isArray(data.history)?data.history:[];history.forEach(function(item){addMessage(item,false)});if(!history.length){var empty=document.createElement('div');empty.className='chat-empty';empty.textContent=emptyText();messages.appendChild(empty)}setStatus(groupMode?'已连接，当前为系统群':'已连接，管理员可实时回复',true)}else if(data.type==='message'){var empty=messages.querySelector('.chat-empty');if(empty)empty.remove();addMessage(data,true)}else if(data.type==='error'){setStatus(data.text||'消息发送失败',ws.readyState===WebSocket.OPEN)}};
ws.onclose=function(event){if(socket!==ws)return;socket=null;if(event.code===4003){applyEnabled(false);clearTimeout(statusTimer);statusTimer=setTimeout(pollStatus,3000);return}if(event.code===4005){try{sessionStorage.removeItem('hfs-chat-tab')}catch(e){}currentClientID='';reconnectDelay=1000;setStatus('检测到重复页面，正在建立独立会话…',false);if(enabled)scheduleReconnect(150);return}setStatus('连接已断开，正在重连…',false);if(enabled)scheduleReconnect(reconnectDelay)};
ws.onerror=function(){if(socket===ws)setStatus('连接异常，正在重试…',false)};}
function scheduleReconnect(delay){clearTimeout(reconnectTimer);var wait=delay||reconnectDelay;reconnectTimer=setTimeout(function(){reconnectTimer=0;connect()},wait);reconnectDelay=Math.min(reconnectDelay*2,30000)}
	function applyEnabled(value){value=!!value;panel.classList.toggle('disabled',!value);if(enabled===value){updateControls();if(value)connect();return}enabled=value;if(!value){markRead();clearTimeout(reconnectTimer);reconnectTimer=0;currentClientID='';groupMode=false;updateMode();if(socket){var old=socket;socket=null;old.onclose=null;old.close(1000,'disabled')}setStatus('聊天暂未开启',false)}else{reconnectDelay=1000;setStatus('正在连接…',false);connect()}updateControls()}
function pollStatus(){fetch('/__hfs/chat/status',{cache:'no-store'}).then(function(response){if(response.status===401)throw new Error('auth');return response.ok?response.json():Promise.reject(new Error('status'))}).then(function(data){applyEnabled(data.enabled)}).catch(function(error){if(error&&error.message==='auth')setStatus('访问凭据已失效，请刷新页面',false)}).then(function(){clearTimeout(statusTimer);var delay=document.hidden?60000:(enabled?30000:3000);statusTimer=setTimeout(pollStatus,delay)})}
function inputImageMime(file){var mime=(file.type||'').toLowerCase();if(mime==='image/jpg')mime='image/jpeg';if(mime==='image/png'||mime==='image/jpeg')return mime;var name=(file.name||'').toLowerCase();if(/\.png$/.test(name))return 'image/png';if(/\.(jpe?g)$/.test(name))return 'image/jpeg';return ''}
function loadImage(file){return new Promise(function(resolve,reject){var source=URL.createObjectURL(file),image=new Image();function release(){URL.revokeObjectURL(source)}image.onload=function(){release();if(image.naturalWidth&&image.naturalHeight)resolve(image);else reject(new Error('无法读取图片尺寸'))};image.onerror=function(){release();reject(new Error('无法读取图片'))};image.src=source})}
function canvasBlob(canvas,quality){return new Promise(function(resolve,reject){function done(blob){if(blob)resolve(blob);else reject(new Error('图片压缩失败'))}if(canvas.toBlob){canvas.toBlob(done,'image/jpeg',quality);return}try{var encoded=canvas.toDataURL('image/jpeg',quality).split(',')[1],raw=atob(encoded),bytes=new Uint8Array(raw.length);for(var i=0;i<raw.length;i++)bytes[i]=raw.charCodeAt(i);done(new Blob([bytes],{type:'image/jpeg'}))}catch(e){reject(new Error('浏览器不支持图片压缩'))}})}
function compressImage(file){var mime=inputImageMime(file);if(!mime)return Promise.reject(new Error('仅支持 PNG 或 JPEG 图片'));if(file.size>MAX_IMAGE_INPUT_BYTES)return Promise.reject(new Error('原始图片不能超过 32 MiB'));return loadImage(file).then(function(image){var scale=Math.min(1,MAX_IMAGE_EDGE/Math.max(image.naturalWidth,image.naturalHeight)),width=Math.max(1,Math.round(image.naturalWidth*scale)),height=Math.max(1,Math.round(image.naturalHeight*scale)),qualities=[0.88,0.76,0.64,0.52,0.42,0.34];function encodeSize(){var canvas=document.createElement('canvas');canvas.width=width;canvas.height=height;var context=canvas.getContext('2d');if(!context)throw new Error('浏览器无法处理图片');context.fillStyle='#fff';context.fillRect(0,0,width,height);context.drawImage(image,0,0,width,height);var qualityIndex=0;function encodeQuality(){return canvasBlob(canvas,qualities[qualityIndex]).then(function(blob){if(blob.size<=MAX_IMAGE_BYTES)return blob;qualityIndex++;if(qualityIndex<qualities.length)return encodeQuality();if(Math.max(width,height)<=320)throw new Error('图片压缩后仍超过 512 KiB');width=Math.max(1,Math.round(width*0.8));height=Math.max(1,Math.round(height*0.8));return encodeSize()})}return encodeQuality()}return encodeSize()})}
function blobBase64(blob){return new Promise(function(resolve,reject){var reader=new FileReader();reader.onload=function(){var value=typeof reader.result==='string'?reader.result:'',comma=value.indexOf(',');if(comma<0){reject(new Error('图片编码失败'));return}resolve(value.slice(comma+1))};reader.onerror=function(){reject(new Error('图片编码失败'))};reader.readAsDataURL(blob)})}
function sendImage(file){if(imageBusy)return Promise.resolve(false);if(!socket||socket.readyState!==WebSocket.OPEN){setComposeNote('聊天尚未连接',true);return Promise.resolve(false)}imageBusy=true;updateControls();setComposeNote('正在压缩图片…',false,true);return compressImage(file).then(blobBase64).then(function(data){if(!socket||socket.readyState!==WebSocket.OPEN)throw new Error('连接已断开，请重新发送');socket.send(JSON.stringify({type:'message',kind:'image',mime:'image/jpeg',data:data}));setComposeNote('图片已发送',false);return true}).then(function(result){imageBusy=false;updateControls();return result},function(error){imageBusy=false;updateControls();setComposeNote(error&&error.message?error.message:'图片发送失败',true);return false})}
function sendImageFiles(fileList){var files=Array.prototype.slice.call(fileList||[]).filter(function(file){return !!inputImageMime(file)});if(!files.length){setComposeNote('拖入的文件中没有 PNG/JPEG 图片',true);return}if(files.length>8){files=files.slice(0,8);setComposeNote('一次最多发送 8 张图片',true)}var sequence=Promise.resolve();files.forEach(function(file){sequence=sequence.then(function(){return sendImage(file)})})}
	launcher.addEventListener('click',function(){prepareAlertsFromGesture();setOpen(true)});closeButton.addEventListener('click',function(){setOpen(true)});
notifyButton.addEventListener('click',requestNotificationSubscription);
form.addEventListener('submit',function(event){event.preventDefault();var value=textBox.value.trim();if(!value||imageBusy)return;if(!socket||socket.readyState!==WebSocket.OPEN){setComposeNote('聊天尚未连接',true);return}socket.send(JSON.stringify({type:'message',kind:'text',text:value}));textBox.value=''});
textBox.addEventListener('compositionstart',function(){composing=true});textBox.addEventListener('compositionend',function(){composing=false});
textBox.addEventListener('keydown',function(event){if(composing||event.isComposing||event.keyCode===229)return;if(event.key==='Enter'&&!event.shiftKey){event.preventDefault();if(form.requestSubmit)form.requestSubmit();else form.dispatchEvent(new Event('submit',{bubbles:true,cancelable:true}))}});
textBox.addEventListener('paste',function(event){var clipboard=event.clipboardData;if(!clipboard)return;var imageFile=null,hasUnsupportedImage=false,items=clipboard.items||[],files=clipboard.files||[];for(var i=0;i<items.length;i++){if(items[i].kind!=='file')continue;var itemFile=items[i].getAsFile();if(!itemFile)continue;if(inputImageMime(itemFile)){imageFile=itemFile;break}if((itemFile.type||'').toLowerCase().indexOf('image/')===0)hasUnsupportedImage=true}for(var j=0;!imageFile&&j<files.length;j++){if(inputImageMime(files[j])){imageFile=files[j];break}if((files[j].type||'').toLowerCase().indexOf('image/')===0)hasUnsupportedImage=true}if(imageFile){event.preventDefault();sendImage(imageFile)}else if(hasUnsupportedImage){event.preventDefault();setComposeNote('剪贴板图片仅支持 PNG 或 JPEG',true)}});
var chatDragDepth=0;panel.addEventListener('dragenter',function(event){if(!event.dataTransfer||!event.dataTransfer.types||Array.prototype.indexOf.call(event.dataTransfer.types,'Files')<0)return;event.preventDefault();chatDragDepth++;panel.classList.add('dragging')});panel.addEventListener('dragover',function(event){if(!panel.classList.contains('dragging'))return;event.preventDefault();event.dataTransfer.dropEffect='copy'});panel.addEventListener('dragleave',function(event){if(!panel.classList.contains('dragging'))return;event.preventDefault();chatDragDepth=Math.max(0,chatDragDepth-1);if(!chatDragDepth)panel.classList.remove('dragging')});panel.addEventListener('drop',function(event){if(!event.dataTransfer)return;event.preventDefault();chatDragDepth=0;panel.classList.remove('dragging');sendImageFiles(event.dataTransfer.files)});
	window.addEventListener('online',function(){reconnectDelay=1000;connect();pollStatus()});window.addEventListener('focus',updateNotificationSubscriptionUI);document.addEventListener('visibilitychange',function(){if(!document.hidden){updateNotificationSubscriptionUI();markRead();clearTimeout(statusTimer);pollStatus()}});initUpload();updateMode();updateBadge();updateControls();updateNotificationSubscriptionUI();pollStatus();
})();</script></body></html>`))
*/

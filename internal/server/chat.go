package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

const (
	chatCookieName                = "hfs_chat_session"
	chatMaxConnections            = 100
	chatMaxConversations          = 100
	chatMaxHistory                = 100
	chatMaxHistoryBytes           = 2 << 20
	chatMaxMessageRunes           = 32768
	chatMaxImageBytes             = 512 << 10
	chatMaxImageDimension         = 4096
	chatMaxImagePixels            = 4 * 1024 * 1024
	chatMaxWireBytes              = chatMaxImageBytes*4/3 + 16<<10
	chatSendQueue                 = 64
	chatWriteWait                 = 5 * time.Second
	chatPongWait                  = 60 * time.Second
	chatPingPeriod                = 25 * time.Second
	chatRateWindow                = 10 * time.Second
	chatRateEvents                = 12
	chatCloseDisabled             = 4003
	chatCloseModeChanged          = 4004
	chatCloseSessionReplaced      = 4005
	websocketClosePolicyViolation = websocket.ClosePolicyViolation
)

const (
	// ChatGroupConversationID is the synthetic conversation selected by the
	// native UI while group chat is enabled.
	ChatGroupConversationID = "__group__"

	ChatMessageKindText  = "text"
	ChatMessageKindImage = "image"
)

// ChatMessage is one message retained in an in-memory conversation.
type ChatMessage struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Sender   string    `json:"sender"`
	ClientID string    `json:"clientId,omitempty"`
	Name     string    `json:"name,omitempty"`
	Text     string    `json:"text,omitempty"`
	Mime     string    `json:"mime,omitempty"`
	Data     []byte    `json:"data,omitempty"`
	SentAt   time.Time `json:"sentAt"`
}

// ChatConversation is a point-in-time, deeply copied view for the native GUI.
type ChatConversation struct {
	ID       string
	Name     string
	Online   bool
	Client   ChatClientInfo
	Messages []ChatMessage
}

// ChatClientInfo is the last observed network and browser information for a
// visitor. The connection port is the remote TCP source port.
type ChatClientInfo struct {
	IP          string
	Port        string
	Browser     string
	OS          string
	UserAgent   string
	ConnectedAt time.Time
}

type chatConversationState struct {
	id           string
	name         string
	client       ChatClientInfo
	messages     []ChatMessage
	historyBytes int
	updated      time.Time
}

type chatHub struct {
	mu            sync.RWMutex
	enabled       bool
	groupEnabled  bool
	accepting     bool
	generation    uint64
	peers         map[string]*chatPeer
	conversations map[string]*chatConversationState
	group         *chatConversationState
	notify        func()
	notifyPending bool
	logOperation  func(ip, operation string)
	active        int
	idle          chan struct{}
}

type chatPeer struct {
	id        string
	ip        string
	client    ChatClientInfo
	conn      *websocket.Conn
	send      chan chatWireMessage
	closeReq  chan chatCloseRequest
	closed    chan struct{}
	closeOnce sync.Once
}

type chatCloseRequest struct {
	code   int
	reason string
}

type chatWireMessage struct {
	Type     string        `json:"type"`
	Group    bool          `json:"group"`
	ClientID string        `json:"clientId,omitempty"`
	Name     string        `json:"name,omitempty"`
	ID       string        `json:"id,omitempty"`
	Kind     string        `json:"kind,omitempty"`
	Sender   string        `json:"sender,omitempty"`
	Text     string        `json:"text,omitempty"`
	Mime     string        `json:"mime,omitempty"`
	Data     []byte        `json:"data,omitempty"`
	SentAt   time.Time     `json:"sentAt,omitempty"`
	History  []ChatMessage `json:"history,omitempty"`
}

type chatClientMessage struct {
	Type string `json:"type"`
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
	Text string `json:"text,omitempty"`
	Mime string `json:"mime,omitempty"`
	Data []byte `json:"data,omitempty"`
}

var chatUpgrader = websocket.Upgrader{
	HandshakeTimeout: time.Second,
	ReadBufferSize:   1024,
	WriteBufferSize:  1024,
	CheckOrigin:      sameChatOrigin,
}

// A compressed image can expand substantially while being decoded. Bounding
// concurrent decodes keeps a burst of valid small PNG/JPEG payloads from
// causing an excessive transient memory spike.
var chatImageDecodeSlots = make(chan struct{}, 4)

func newChatHub() *chatHub {
	idle := make(chan struct{})
	close(idle)
	return &chatHub{
		peers:         make(map[string]*chatPeer),
		conversations: make(map[string]*chatConversationState),
		group: &chatConversationState{
			id:   ChatGroupConversationID,
			name: "多人聊天",
		},
		accepting: true,
		idle:      idle,
	}
}

// SetChatEnabled controls new chat connections. Disabling also removes all
// currently connected visitors without blocking the calling (GUI) thread.
func (s *Server) SetChatEnabled(enabled bool) {
	s.chat.setEnabled(enabled)
}

func (s *Server) ChatEnabled() bool {
	s.chat.mu.RLock()
	defer s.chat.mu.RUnlock()
	return s.chat.enabled
}

// SetGroupChatEnabled selects between private visitor-to-admin conversations
// and one shared room. Switching modes closes current sockets so every client
// receives a fresh, mode-consistent ready snapshot.
func (s *Server) SetGroupChatEnabled(enabled bool) {
	s.chat.setGroupEnabled(enabled)
}

func (s *Server) GroupChatEnabled() bool {
	s.chat.mu.RLock()
	defer s.chat.mu.RUnlock()
	return s.chat.groupEnabled
}

// SetChatNotifier installs a lightweight callback. Notifications are
// coalesced and invoked outside the hub lock so a Win32 UI can safely post a
// message back to its own thread.
func (s *Server) SetChatNotifier(notify func()) {
	s.chat.mu.Lock()
	s.chat.notify = notify
	if notify != nil {
		s.chat.scheduleNotifyLocked()
	}
	s.chat.mu.Unlock()
}

func (s *Server) ChatOnlineCount() int {
	s.chat.mu.RLock()
	defer s.chat.mu.RUnlock()
	return s.chat.onlineIPCountLocked()
}

// ChatSnapshot returns every visible conversation with a deep copy of all
// retained messages, including image bytes.
func (s *Server) ChatSnapshot() []ChatConversation {
	return s.chat.snapshotConversations(true)
}

// ChatOverview returns every visible conversation without copying retained
// image payloads. Message metadata, MIME type, text and timestamps are kept,
// but every returned ChatMessage.Data is nil.
func (s *Server) ChatOverview() []ChatConversation {
	return s.chat.snapshotConversations(false)
}

// ChatConversationSnapshot returns one full, deeply copied conversation.
// Private IDs are unavailable in group mode, and the synthetic group ID is
// unavailable in private mode.
func (s *Server) ChatConversationSnapshot(id string) (ChatConversation, bool) {
	s.chat.mu.RLock()
	item, ok := s.chat.snapshotConversationLocked(id, true)
	s.chat.mu.RUnlock()
	return item, ok
}

type chatConversationSnapshotItem struct {
	conversation ChatConversation
	updated      time.Time
}

func (h *chatHub) snapshotConversations(includeImageData bool) []ChatConversation {
	h.mu.RLock()
	if h.groupEnabled {
		item, _ := h.snapshotConversationLocked(ChatGroupConversationID, includeImageData)
		h.mu.RUnlock()
		return []ChatConversation{item}
	}
	rows := make([]chatConversationSnapshotItem, 0, len(h.conversations))
	for id, conversation := range h.conversations {
		item, _ := h.snapshotConversationLocked(id, includeImageData)
		rows = append(rows, chatConversationSnapshotItem{
			conversation: item,
			updated:      conversation.updated,
		})
	}
	h.mu.RUnlock()
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].conversation.Online != rows[j].conversation.Online {
			return rows[i].conversation.Online
		}
		if !rows[i].updated.Equal(rows[j].updated) {
			return rows[i].updated.After(rows[j].updated)
		}
		return strings.ToLower(rows[i].conversation.Name) < strings.ToLower(rows[j].conversation.Name)
	})
	items := make([]ChatConversation, len(rows))
	for i := range rows {
		items[i] = rows[i].conversation
	}
	return items
}

func (h *chatHub) snapshotConversationLocked(id string, includeImageData bool) (ChatConversation, bool) {
	if h.groupEnabled {
		if id != ChatGroupConversationID {
			return ChatConversation{}, false
		}
		return ChatConversation{
			ID:       ChatGroupConversationID,
			Name:     h.group.name,
			Online:   len(h.peers) > 0,
			Messages: copyChatMessages(h.group.messages, includeImageData),
		}, true
	}
	if id == ChatGroupConversationID {
		return ChatConversation{}, false
	}
	id = normalizeChatIP(id)
	if id == "" {
		return ChatConversation{}, false
	}
	conversation := h.conversations[id]
	if conversation == nil {
		return ChatConversation{}, false
	}
	return ChatConversation{
		ID:       id,
		Name:     conversation.name,
		Online:   h.ipOnlineLocked(id),
		Client:   conversation.client,
		Messages: copyChatMessages(conversation.messages, includeImageData),
	}, true
}

func (h *chatHub) onlineIPCountLocked() int {
	online := make(map[string]struct{}, len(h.peers))
	for _, peer := range h.peers {
		if peer.ip != "" {
			online[peer.ip] = struct{}{}
		}
	}
	return len(online)
}

func (h *chatHub) ipOnlineLocked(ip string) bool {
	for _, peer := range h.peers {
		if peer.ip == ip {
			return true
		}
	}
	return false
}

func (h *chatHub) peersForIPLocked(ip string) []*chatPeer {
	peers := make([]*chatPeer, 0, 1)
	for _, peer := range h.peers {
		if peer.ip == ip {
			peers = append(peers, peer)
		}
	}
	return peers
}

func (h *chatHub) onlineIPsLocked() []string {
	seen := make(map[string]struct{}, len(h.peers))
	ips := make([]string, 0, len(h.peers))
	for _, peer := range h.peers {
		if peer.ip == "" {
			continue
		}
		if _, exists := seen[peer.ip]; exists {
			continue
		}
		seen[peer.ip] = struct{}{}
		ips = append(ips, peer.ip)
	}
	return ips
}

func (h *chatHub) logIPOperation(ip, operation string) {
	if h.logOperation != nil && ip != "" {
		h.logOperation(ip, operation)
	}
}

// RemoveChatVisitor removes one visitor from the native visitor list without
// closing its socket. If that visitor sends another message or reconnects, a
// fresh conversation is created and it appears again.
func (s *Server) RemoveChatVisitor(id string) bool {
	if id == "" || id == ChatGroupConversationID {
		return false
	}
	id = normalizeChatIP(id)
	if id == "" {
		return false
	}
	s.chat.mu.Lock()
	if s.chat.groupEnabled {
		s.chat.mu.Unlock()
		return false
	}
	if _, ok := s.chat.conversations[id]; !ok {
		s.chat.mu.Unlock()
		return false
	}
	delete(s.chat.conversations, id)
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	s.chat.logIPOperation(id, "后台移除聊天记录")
	return true
}

// SendChatMessage queues one administrator reply. It never performs a socket
// write on the caller's thread.
func (s *Server) SendChatMessage(clientID, text string) error {
	cleaned, err := cleanChatText(text)
	if err != nil {
		return err
	}
	message := newAdminChatMessage(ChatMessageKindText)
	message.Text = cleaned
	return s.sendAdminChatMessage(clientID, message)
}

// SendChatImage validates and queues one administrator image. The supplied
// byte slice is copied before the call returns.
func (s *Server) SendChatImage(clientID, mime string, data []byte) error {
	cleanMime, cleanData, err := cleanChatImage(mime, data)
	if err != nil {
		return err
	}
	message := newAdminChatMessage(ChatMessageKindImage)
	message.Mime = cleanMime
	message.Data = cleanData
	return s.sendAdminChatMessage(clientID, message)
}

func newAdminChatMessage(kind string) ChatMessage {
	return ChatMessage{
		ID:       newChatMessageID(),
		Kind:     kind,
		Sender:   "admin",
		ClientID: "admin",
		Name:     "管理员",
		SentAt:   time.Now().UTC(),
	}
}

func (s *Server) sendAdminChatMessage(clientID string, message ChatMessage) error {
	wire := wireFromMessage(message)
	s.chat.mu.Lock()
	if !s.chat.enabled {
		s.chat.mu.Unlock()
		return errors.New("聊天功能未启用")
	}
	if s.chat.groupEnabled {
		targets := s.chat.onlineIPsLocked()
		wire.Group = true
		appendChatMessage(s.chat.group, message)
		slow := s.chat.broadcastLocked(wire)
		s.chat.scheduleNotifyLocked()
		s.chat.mu.Unlock()
		shutdownSlowChatPeers(slow)
		for _, ip := range targets {
			s.chat.logIPOperation(ip, adminChatOperation(message))
		}
		return nil
	}
	clientID = normalizeChatIP(clientID)
	if clientID == "" {
		s.chat.mu.Unlock()
		return errors.New("访客 IP 无效")
	}
	peers := s.chat.peersForIPLocked(clientID)
	conversation := s.chat.conversations[clientID]
	if len(peers) > 0 && conversation == nil {
		s.chat.mu.Unlock()
		return errors.New("访客会话已移除，等待对方再次发送消息或重新连接")
	}
	if len(peers) == 0 || conversation == nil {
		s.chat.mu.Unlock()
		return errors.New("访客已离线")
	}
	delivered := 0
	var slow []*chatPeer
	for _, peer := range peers {
		if peer.enqueue(wire) {
			delivered++
		} else {
			slow = append(slow, peer)
		}
	}
	if delivered == 0 {
		s.chat.mu.Unlock()
		shutdownSlowChatPeers(slow)
		return errors.New("访客连接繁忙，请稍后重试")
	}
	appendChatMessage(conversation, message)
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	shutdownSlowChatPeers(slow)
	s.chat.logIPOperation(clientID, adminChatOperation(message))
	return nil
}

func adminChatOperation(message ChatMessage) string {
	if message.Kind == ChatMessageKindImage {
		return "后台发送聊天图片"
	}
	return "后台发送聊天文本"
}

func (h *chatHub) setGroupEnabled(enabled bool) {
	h.mu.Lock()
	if h.groupEnabled == enabled {
		h.mu.Unlock()
		return
	}
	h.groupEnabled = enabled
	h.generation++
	peers := h.detachAllLocked()
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	for _, peer := range peers {
		peer.shutdown(chatCloseModeChanged, "chat mode changed")
	}
}

func (h *chatHub) setEnabled(enabled bool) {
	h.mu.Lock()
	if h.enabled == enabled {
		h.mu.Unlock()
		return
	}
	h.enabled = enabled
	h.generation++
	var peers []*chatPeer
	if !enabled {
		peers = h.detachAllLocked()
	}
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	for _, peer := range peers {
		peer.shutdown(chatCloseDisabled, "chat disabled")
	}
}

func (h *chatHub) disconnect(code int, reason string) {
	h.mu.Lock()
	h.generation++
	peers := h.detachAllLocked()
	if len(peers) > 0 {
		h.scheduleNotifyLocked()
	}
	h.mu.Unlock()
	for _, peer := range peers {
		peer.shutdown(code, reason)
	}
}

func (h *chatHub) resume() {
	h.mu.Lock()
	h.accepting = true
	h.generation++
	h.mu.Unlock()
}

func (h *chatHub) pause(timeout time.Duration) bool {
	h.mu.Lock()
	h.accepting = false
	h.generation++
	peers := h.detachAllLocked()
	idle := h.idle
	if len(peers) > 0 {
		h.scheduleNotifyLocked()
	}
	h.mu.Unlock()
	for _, peer := range peers {
		peer.shutdown(websocket.CloseGoingAway, "server stopping")
	}
	if timeout <= 0 {
		<-idle
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}

func (h *chatHub) beginHandler() (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.accepting || !h.enabled || h.active >= chatMaxConnections {
		return 0, false
	}
	if h.active == 0 {
		h.idle = make(chan struct{})
	}
	h.active++
	return h.generation, true
}

func (h *chatHub) endHandler() {
	h.mu.Lock()
	h.active--
	if h.active == 0 {
		close(h.idle)
	}
	h.mu.Unlock()
}

func (h *chatHub) detachAllLocked() []*chatPeer {
	peers := make([]*chatPeer, 0, len(h.peers))
	for id, peer := range h.peers {
		peers = append(peers, peer)
		delete(h.peers, id)
	}
	return peers
}

func (h *chatHub) scheduleNotifyLocked() {
	if h.notify == nil || h.notifyPending {
		return
	}
	h.notifyPending = true
	time.AfterFunc(40*time.Millisecond, func() {
		h.mu.Lock()
		h.notifyPending = false
		notify := h.notify
		h.mu.Unlock()
		if notify != nil {
			notify()
		}
	})
}

func (s *Server) ensureChatSession(w http.ResponseWriter, r *http.Request) (string, bool, error) {
	if sessionID := chatSessionID(r); sessionID != "" {
		return sessionID, false, nil
	}
	token, err := newSecureToken(16)
	if err != nil {
		return "", false, err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     chatCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteStrictMode,
	})
	return token, true, nil
}

func chatSessionID(r *http.Request) string {
	cookie, err := r.Cookie(chatCookieName)
	if err != nil || !validChatToken(cookie.Value) {
		return ""
	}
	return strings.ToLower(cookie.Value)
}

func clientInfoFromRequest(r *http.Request) ChatClientInfo {
	host, port := clientAddressFromRequest(r)
	userAgent := cleanClientHeader(r.UserAgent(), 512)
	platform := strings.Trim(cleanClientHeader(r.Header.Get("Sec-CH-UA-Platform"), 64), `"`)
	platformVersion := strings.Trim(cleanClientHeader(r.Header.Get("Sec-CH-UA-Platform-Version"), 64), `"`)
	return ChatClientInfo{
		IP:          host,
		Port:        port,
		Browser:     browserFromUserAgent(userAgent),
		OS:          osFromUserAgent(userAgent, platform, platformVersion),
		UserAgent:   userAgent,
		ConnectedAt: time.Now().UTC(),
	}
}

// clientAddressFromRequest returns a canonical IP address and the remote TCP
// source port. X-Forwarded-For is accepted only from a loopback peer so a
// local reverse proxy can preserve visitor identity without letting a remote
// client choose another visitor's IP.
func clientAddressFromRequest(r *http.Request) (string, string) {
	if r == nil {
		return "", ""
	}
	remote := strings.TrimSpace(r.RemoteAddr)
	host, port, err := net.SplitHostPort(remote)
	if err != nil {
		host = strings.Trim(remote, "[]")
		port = ""
	}
	ip := normalizeChatIP(host)
	parsed := net.ParseIP(ip)
	if parsed != nil && parsed.IsLoopback() {
		forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
		if forwardedIP := normalizeChatIP(forwarded); forwardedIP != "" {
			ip = forwardedIP
		}
	}
	return ip, port
}

func normalizeChatIP(raw string) string {
	raw = strings.TrimSpace(strings.Trim(raw, "[]"))
	if zone := strings.LastIndexByte(raw, '%'); zone >= 0 {
		raw = raw[:zone]
	}
	ip := net.ParseIP(raw)
	if ip == nil {
		return ""
	}
	return ip.String()
}

func cleanClientHeader(value string, maxRunes int) string {
	value = strings.Map(func(r rune) rune {
		if r == 0 || unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes]) + "…"
	}
	return value
}

func browserFromUserAgent(userAgent string) string {
	for _, candidate := range []struct {
		token string
		name  string
	}{
		{"Edg/", "Microsoft Edge"},
		{"OPR/", "Opera"},
		{"SamsungBrowser/", "Samsung Internet"},
		{"CriOS/", "Google Chrome"},
		{"Chrome/", "Google Chrome"},
		{"FxiOS/", "Mozilla Firefox"},
		{"Firefox/", "Mozilla Firefox"},
	} {
		if version := userAgentTokenVersion(userAgent, candidate.token); version != "" {
			return candidate.name + " " + version
		}
	}
	if strings.Contains(userAgent, "Safari/") {
		if version := userAgentTokenVersion(userAgent, "Version/"); version != "" {
			return "Safari " + version
		}
		return "Safari"
	}
	if userAgent == "" {
		return "未知"
	}
	return "其他浏览器"
}

func osFromUserAgent(userAgent, platform, platformVersion string) string {
	if strings.EqualFold(platform, "Windows") {
		if majorText := strings.Split(platformVersion, ".")[0]; majorText != "" {
			if major, err := strconv.Atoi(majorText); err == nil {
				if major >= 13 {
					return "Windows 11"
				}
				if major >= 10 {
					return "Windows 10"
				}
			}
		}
	}
	switch {
	case strings.Contains(userAgent, "CrOS "):
		return "ChromeOS"
	case strings.Contains(userAgent, "Android "):
		version := textBetween(userAgent, "Android ", ";")
		if version != "" {
			return "Android " + version
		}
		return "Android"
	case strings.Contains(userAgent, "iPhone OS "):
		version := strings.ReplaceAll(textBetween(userAgent, "iPhone OS ", " "), "_", ".")
		if version != "" {
			return "iOS " + version
		}
		return "iOS"
	case strings.Contains(userAgent, "iPad; CPU OS "):
		version := strings.ReplaceAll(textBetween(userAgent, "iPad; CPU OS ", " "), "_", ".")
		if version != "" {
			return "iPadOS " + version
		}
		return "iPadOS"
	case strings.Contains(userAgent, "Windows NT 10.0"):
		return "Windows 10/11"
	case strings.Contains(userAgent, "Windows NT 6.3"):
		return "Windows 8.1"
	case strings.Contains(userAgent, "Windows NT 6.1"):
		return "Windows 7"
	case strings.Contains(userAgent, "Mac OS X "):
		version := strings.ReplaceAll(textBetween(userAgent, "Mac OS X ", ")"), "_", ".")
		if version != "" {
			return "macOS " + strings.TrimSuffix(version, ";")
		}
		return "macOS"
	case strings.Contains(userAgent, "Linux"):
		return "Linux"
	case platform != "":
		if platformVersion != "" {
			return platform + " " + platformVersion
		}
		return platform
	default:
		return "未知"
	}
}

func userAgentTokenVersion(userAgent, token string) string {
	index := strings.Index(userAgent, token)
	if index < 0 {
		return ""
	}
	value := userAgent[index+len(token):]
	if end := strings.IndexAny(value, " ;()"); end >= 0 {
		value = value[:end]
	}
	return cleanClientHeader(value, 32)
}

func textBetween(value, prefix, suffix string) string {
	start := strings.Index(value, prefix)
	if start < 0 {
		return ""
	}
	value = value[start+len(prefix):]
	if end := strings.Index(value, suffix); end >= 0 {
		value = value[:end]
	}
	return cleanClientHeader(value, 64)
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return false
	}
	forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(forwarded, "https")
}

func sameChatOrigin(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		return false
	}
	origin, err := url.Parse(raw)
	if err != nil || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || (origin.Scheme != "http" && origin.Scheme != "https") {
		return false
	}
	expectedScheme := "http"
	if requestIsHTTPS(r) {
		expectedScheme = "https"
	}
	return strings.EqualFold(origin.Scheme, expectedScheme) && strings.EqualFold(origin.Host, r.Host)
}

// handleChatWebSocket returns the HTTP status used for access logging. It must
// receive the original ResponseWriter, not statusWriter.
func (s *Server) handleChatWebSocket(w http.ResponseWriter, r *http.Request, accessVersion uint64) int {
	sessionID := chatSessionID(r)
	if sessionID == "" {
		http.Error(w, "missing chat session", http.StatusUnauthorized)
		return http.StatusUnauthorized
	}
	tabID := strings.ToLower(r.URL.Query().Get("tab"))
	if !validChatToken(tabID) {
		http.Error(w, "invalid chat tab", http.StatusBadRequest)
		return http.StatusBadRequest
	}
	connectionID := deriveChatConnectionID(sessionID, tabID)
	client := clientInfoFromRequest(r)
	if client.IP == "" {
		http.Error(w, "invalid client IP", http.StatusBadRequest)
		return http.StatusBadRequest
	}
	if !sameChatOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return http.StatusForbidden
	}
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return http.StatusBadRequest
	}
	s.mu.RLock()
	if s.accessVersion != accessVersion {
		s.mu.RUnlock()
		http.Error(w, "access credentials changed", http.StatusUnauthorized)
		return http.StatusUnauthorized
	}
	generation, accepted := s.chat.beginHandler()
	s.mu.RUnlock()
	if !accepted {
		http.Error(w, "chat unavailable", http.StatusServiceUnavailable)
		return http.StatusServiceUnavailable
	}
	defer s.chat.endHandler()
	conn, err := chatUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return http.StatusBadRequest
	}
	peer := &chatPeer{
		id:       connectionID,
		ip:       client.IP,
		client:   client,
		conn:     conn,
		send:     make(chan chatWireMessage, chatSendQueue),
		closeReq: make(chan chatCloseRequest, 1),
		closed:   make(chan struct{}),
	}
	go peer.writePump()
	if !s.chat.register(peer, generation) {
		peer.shutdown(websocket.CloseTryAgainLater, "chat unavailable")
		<-peer.closed
		return http.StatusSwitchingProtocols
	}
	peer.readPump(s.chat)
	s.chat.unregister(peer)
	peer.shutdown(websocket.CloseNormalClosure, "connection closed")
	<-peer.closed
	return http.StatusSwitchingProtocols
}

// deriveChatConnectionID identifies one browser tab for replacement on
// reconnect. It is deliberately private; the only visitor identity exposed
// by chat is the canonical IP address.
func deriveChatConnectionID(sessionID, tabID string) string {
	sum := sha256.Sum256([]byte(sessionID + ":" + tabID))
	return hex.EncodeToString(sum[:16])
}

func (h *chatHub) register(peer *chatPeer, generation uint64) bool {
	h.mu.Lock()
	if !h.enabled || !h.accepting || h.generation != generation {
		h.mu.Unlock()
		return false
	}
	old := h.peers[peer.id]
	if old == nil && len(h.peers) >= chatMaxConnections {
		h.mu.Unlock()
		return false
	}
	conversation := h.conversations[peer.ip]
	if conversation == nil {
		if len(h.conversations) >= chatMaxConversations && !h.evictOldestOfflineLocked() {
			h.mu.Unlock()
			return false
		}
		conversation = &chatConversationState{id: peer.ip, name: peer.ip, client: peer.client}
		h.conversations[peer.ip] = conversation
	}
	conversation.client = peer.client
	conversation.name = peer.ip
	conversation.updated = time.Now().UTC()
	h.peers[peer.id] = peer
	history := conversation.messages
	if h.groupEnabled {
		history = h.group.messages
	}
	ready := chatWireMessage{
		Type:     "ready",
		Group:    h.groupEnabled,
		ClientID: peer.ip,
		Name:     peer.ip,
		History:  copyChatMessages(history, true),
	}
	if !peer.enqueue(ready) {
		if old != nil {
			h.peers[peer.id] = old
		} else {
			delete(h.peers, peer.id)
		}
		h.mu.Unlock()
		return false
	}
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	if old != nil && old != peer {
		old.shutdown(chatCloseSessionReplaced, "session reconnected")
	}
	h.logIPOperation(peer.ip, "聊天连接上线")
	return true
}

func (h *chatHub) evictOldestOfflineLocked() bool {
	var oldest *chatConversationState
	for _, conversation := range h.conversations {
		if h.ipOnlineLocked(conversation.id) {
			continue
		}
		if oldest == nil || conversation.updated.Before(oldest.updated) {
			oldest = conversation
		}
	}
	if oldest == nil {
		return false
	}
	delete(h.conversations, oldest.id)
	return true
}

func (h *chatHub) unregister(peer *chatPeer) {
	removed := false
	h.mu.Lock()
	if h.peers[peer.id] == peer {
		delete(h.peers, peer.id)
		if conversation := h.conversations[peer.ip]; conversation != nil {
			conversation.updated = time.Now().UTC()
		}
		h.scheduleNotifyLocked()
		removed = true
	}
	h.mu.Unlock()
	if removed {
		h.logIPOperation(peer.ip, "聊天连接离线")
	}
}

func (p *chatPeer) readPump(h *chatHub) {
	p.conn.SetReadLimit(chatMaxWireBytes)
	_ = p.conn.SetReadDeadline(time.Now().Add(chatPongWait))
	p.conn.SetPongHandler(func(string) error {
		return p.conn.SetReadDeadline(time.Now().Add(chatPongWait))
	})
	windowStarted := time.Now()
	events := 0
	for {
		messageType, payload, err := p.conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			p.shutdown(websocket.CloseUnsupportedData, "text messages only")
			return
		}
		now := time.Now()
		if now.Sub(windowStarted) >= chatRateWindow {
			windowStarted, events = now, 0
		}
		events++
		if events > chatRateEvents {
			p.enqueue(chatWireMessage{Type: "error", Text: "发送过于频繁，请稍后再试"})
			p.shutdown(websocket.ClosePolicyViolation, "rate limit exceeded")
			return
		}
		var incoming chatClientMessage
		if err := json.Unmarshal(payload, &incoming); err != nil {
			p.enqueue(chatWireMessage{Type: "error", Text: "消息格式无效"})
			continue
		}
		switch incoming.Type {
		case "message":
			var err error
			switch incoming.Kind {
			case "", ChatMessageKindText:
				err = h.receiveMessage(p, incoming.Text)
			case ChatMessageKindImage:
				err = h.receiveImage(p, incoming.Mime, incoming.Data)
			default:
				err = errors.New("未知的消息类型")
			}
			if err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
			}
		case "image":
			if err := h.receiveImage(p, incoming.Mime, incoming.Data); err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
			}
		case "name":
			// Visitor aliases are intentionally unsupported. A legacy client
			// may still send this frame, so acknowledge the immutable IP
			// identity without changing hub state.
			p.enqueue(chatWireMessage{Type: "name", Name: p.ip})
		default:
			p.enqueue(chatWireMessage{Type: "error", Text: "未知的聊天操作"})
		}
	}
}

func (h *chatHub) receiveMessage(peer *chatPeer, text string) error {
	cleaned, err := cleanChatText(text)
	if err != nil {
		return err
	}
	return h.receivePeerChatMessage(peer, ChatMessage{
		ID:     newChatMessageID(),
		Kind:   ChatMessageKindText,
		Sender: "user",
		Text:   cleaned,
		SentAt: time.Now().UTC(),
	})
}

func (h *chatHub) receiveImage(peer *chatPeer, mime string, data []byte) error {
	cleanMime, cleanData, err := cleanChatImage(mime, data)
	if err != nil {
		return err
	}
	return h.receivePeerChatMessage(peer, ChatMessage{
		ID:     newChatMessageID(),
		Kind:   ChatMessageKindImage,
		Sender: "user",
		Mime:   cleanMime,
		Data:   cleanData,
		SentAt: time.Now().UTC(),
	})
}

func (h *chatHub) receivePeerChatMessage(peer *chatPeer, message ChatMessage) error {
	h.mu.Lock()
	if !h.enabled || h.peers[peer.id] != peer {
		h.mu.Unlock()
		return errors.New("聊天连接已关闭")
	}
	conversation, err := h.ensurePeerConversationLocked(peer)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	message.ClientID = peer.ip
	message.Name = peer.ip
	wire := wireFromMessage(message)
	if h.groupEnabled {
		wire.Group = true
		appendChatMessage(h.group, message)
		slow := h.broadcastLocked(wire)
		h.scheduleNotifyLocked()
		h.mu.Unlock()
		shutdownSlowChatPeers(slow)
		h.logIPOperation(peer.ip, receivedChatOperation(message))
		return nil
	}
	peers := h.peersForIPLocked(peer.ip)
	delivered := 0
	var slow []*chatPeer
	for _, recipient := range peers {
		if recipient.enqueue(wire) {
			delivered++
		} else {
			slow = append(slow, recipient)
		}
	}
	if delivered == 0 {
		h.mu.Unlock()
		shutdownSlowChatPeers(slow)
		return errors.New("连接繁忙，请稍后重试")
	}
	appendChatMessage(conversation, message)
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	h.logIPOperation(peer.ip, receivedChatOperation(message))
	return nil
}

func receivedChatOperation(message ChatMessage) string {
	if message.Kind == ChatMessageKindImage {
		return "接收聊天图片"
	}
	return "接收聊天文本"
}

func (h *chatHub) ensurePeerConversationLocked(peer *chatPeer) (*chatConversationState, error) {
	if conversation := h.conversations[peer.ip]; conversation != nil {
		return conversation, nil
	}
	if len(h.conversations) >= chatMaxConversations && !h.evictOldestOfflineLocked() {
		return nil, errors.New("访客列表已满，请稍后再试")
	}
	conversation := &chatConversationState{
		id:      peer.ip,
		name:    peer.ip,
		client:  peer.client,
		updated: time.Now().UTC(),
	}
	h.conversations[peer.ip] = conversation
	h.scheduleNotifyLocked()
	return conversation, nil
}

func appendChatMessage(conversation *chatConversationState, message ChatMessage) {
	if conversation.historyBytes == 0 && len(conversation.messages) > 0 {
		for i := range conversation.messages {
			conversation.historyBytes += chatMessageHistoryBytes(conversation.messages[i])
		}
	}
	stored := cloneChatMessage(message)
	conversation.messages = append(conversation.messages, stored)
	conversation.historyBytes += chatMessageHistoryBytes(stored)
	for len(conversation.messages) > chatMaxHistory || conversation.historyBytes > chatMaxHistoryBytes {
		conversation.historyBytes -= chatMessageHistoryBytes(conversation.messages[0])
		conversation.messages[0] = ChatMessage{}
		conversation.messages = conversation.messages[1:]
	}
	conversation.updated = message.SentAt
}

func chatMessageHistoryBytes(message ChatMessage) int {
	// Payloads dominate retained memory. The 100-message cap bounds the small
	// fixed/string metadata, so accounting text and image bytes keeps each
	// conversation at approximately two MiB without unexpectedly evicting a
	// fourth maximum-sized image.
	return len(message.Text) + len(message.Data)
}

func cloneChatMessage(message ChatMessage) ChatMessage {
	if message.Data != nil {
		message.Data = append([]byte(nil), message.Data...)
	}
	return message
}

func copyChatMessages(messages []ChatMessage, includeImageData bool) []ChatMessage {
	if len(messages) == 0 {
		return nil
	}
	cloned := make([]ChatMessage, len(messages))
	for i := range messages {
		cloned[i] = messages[i]
		if includeImageData {
			if messages[i].Data != nil {
				cloned[i].Data = append([]byte(nil), messages[i].Data...)
			}
		} else {
			cloned[i].Data = nil
		}
	}
	return cloned
}

func wireFromMessage(message ChatMessage) chatWireMessage {
	kind := message.Kind
	if kind == "" {
		if len(message.Data) > 0 {
			kind = ChatMessageKindImage
		} else {
			kind = ChatMessageKindText
		}
	}
	return chatWireMessage{
		Type:     "message",
		ClientID: message.ClientID,
		Name:     message.Name,
		ID:       message.ID,
		Kind:     kind,
		Sender:   message.Sender,
		Text:     message.Text,
		Mime:     message.Mime,
		Data:     message.Data,
		SentAt:   message.SentAt,
	}
}

func (h *chatHub) broadcastLocked(message chatWireMessage) []*chatPeer {
	var slow []*chatPeer
	for _, peer := range h.peers {
		if !peer.enqueue(message) {
			slow = append(slow, peer)
		}
	}
	return slow
}

func shutdownSlowChatPeers(peers []*chatPeer) {
	for _, peer := range peers {
		peer.shutdown(websocket.CloseTryAgainLater, "client too slow")
	}
}

func (p *chatPeer) enqueue(message chatWireMessage) bool {
	select {
	case <-p.closed:
		return false
	default:
	}
	select {
	case p.send <- message:
		return true
	default:
		return false
	}
}

func (p *chatPeer) shutdown(code int, reason string) {
	p.closeOnce.Do(func() {
		select {
		case p.closeReq <- chatCloseRequest{code: code, reason: reason}:
		case <-p.closed:
		}
		time.AfterFunc(time.Second, func() {
			select {
			case <-p.closed:
			default:
				_ = p.conn.Close()
			}
		})
	})
}

func (p *chatPeer) writePump() {
	ticker := time.NewTicker(chatPingPeriod)
	defer func() {
		ticker.Stop()
		_ = p.conn.Close()
		close(p.closed)
	}()
	for {
		select {
		case request := <-p.closeReq:
			p.writeClose(request)
			return
		default:
		}
		select {
		case request := <-p.closeReq:
			p.writeClose(request)
			return
		case message := <-p.send:
			_ = p.conn.SetWriteDeadline(time.Now().Add(chatWriteWait))
			if err := p.conn.WriteJSON(message); err != nil {
				return
			}
		case <-ticker.C:
			if err := p.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(chatWriteWait)); err != nil {
				return
			}
		}
	}
}

func (p *chatPeer) writeClose(request chatCloseRequest) {
	_ = p.conn.SetWriteDeadline(time.Now().Add(chatWriteWait))
	_ = p.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(request.code, request.reason), time.Now().Add(chatWriteWait))
}

func cleanChatText(raw string) (string, error) {
	raw = strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("消息不能为空")
	}
	if utf8.RuneCountInString(raw) > chatMaxMessageRunes {
		return "", fmt.Errorf("消息不能超过 %d 个字符", chatMaxMessageRunes)
	}
	for _, r := range raw {
		if r == 0 || unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", errors.New("消息包含无效控制字符")
		}
	}
	return raw, nil
}

func cleanChatImage(rawMime string, data []byte) (string, []byte, error) {
	mime := strings.ToLower(strings.TrimSpace(rawMime))
	var expectedFormat string
	switch mime {
	case "image/png":
		expectedFormat = "png"
	case "image/jpeg":
		expectedFormat = "jpeg"
	default:
		return "", nil, errors.New("只支持 PNG 或 JPEG 图片")
	}
	if len(data) == 0 {
		return "", nil, errors.New("图片不能为空")
	}
	if len(data) > chatMaxImageBytes {
		return "", nil, fmt.Errorf("图片不能超过 %d KiB", chatMaxImageBytes>>10)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != expectedFormat {
		return "", nil, errors.New("图片格式无效或与 MIME 类型不匹配")
	}
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > chatMaxImageDimension || config.Height > chatMaxImageDimension ||
		int64(config.Width)*int64(config.Height) > chatMaxImagePixels {
		return "", nil, fmt.Errorf(
			"图片尺寸不能超过 %d×%d，且总像素不能超过 %d",
			chatMaxImageDimension,
			chatMaxImageDimension,
			chatMaxImagePixels,
		)
	}
	select {
	case chatImageDecodeSlots <- struct{}{}:
		defer func() { <-chatImageDecodeSlots }()
	default:
		return "", nil, errors.New("图片处理繁忙，请稍后重试")
	}
	if _, decodedFormat, decodeErr := image.Decode(bytes.NewReader(data)); decodeErr != nil || decodedFormat != expectedFormat {
		return "", nil, errors.New("图片数据无法解码")
	}
	return mime, append([]byte(nil), data...), nil
}

func validChatToken(token string) bool {
	if len(token) != 32 {
		return false
	}
	for _, r := range token {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func newSecureToken(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func newChatMessageID() string {
	id, err := newSecureToken(12)
	if err == nil {
		return id
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

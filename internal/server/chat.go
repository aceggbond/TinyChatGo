package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	chatMaxMessageRunes           = 262144
	chatMaxImageBytes             = 512 << 10
	chatMaxFileBytes              = 32 << 20
	chatMaxUploadImageBytes       = 100 << 20
	chatMaxUploadFileBytes        = int64(1) << 30
	chatMaxImageDimension         = 4096
	chatMaxImagePixels            = 4 * 1024 * 1024
	chatAvatarSize                = 96
	chatAvatarMaxBytes            = 48 << 10
	chatAvatarMaxDataURLBytes     = 72 << 10
	chatMaxWireBytes              = chatMaxFileBytes*4/3 + 32<<10
	chatSendQueue                 = 64
	chatWriteWait                 = 5 * time.Second
	chatPongWait                  = 60 * time.Second
	chatPingPeriod                = 25 * time.Second
	chatRateWindow                = 10 * time.Second
	chatRateEvents                = 30
	chatRateHardEvents            = 300
	chatCloseDisabled             = 4003
	chatCloseModeChanged          = 4004
	chatCloseSessionReplaced      = 4005
	websocketClosePolicyViolation = websocket.ClosePolicyViolation
)

const (
	// ChatGroupConversationID is the synthetic conversation selected by the
	// native UI while group chat is enabled.
	ChatGroupConversationID = "__group__"
	chatUserGroupPrefix     = "group:"
	// ChatAdminConversationID is the browser-side virtual user representing
	// the native administrator. It keeps administrator private messages out of
	// the system-group timeline.
	ChatAdminConversationID = "__admin__"

	ChatMessageKindText  = "text"
	ChatMessageKindImage = "image"
	ChatMessageKindFile  = "file"
)

// ChatMessage is one message retained in an in-memory conversation.
type ChatMessage struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Sender   string `json:"sender"`
	ClientID string `json:"clientId,omitempty"`
	Name     string `json:"name,omitempty"`
	Text     string `json:"text,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Data     []byte `json:"data,omitempty"`
	FileName string `json:"fileName,omitempty"`
	FileSize int64  `json:"fileSize,omitempty"`
	FileURL  string `json:"fileUrl,omitempty"`
	// AttachmentPath is persisted in the database and is never sent to a
	// browser. It is relative to the application's chat_files directory.
	AttachmentPath string    `json:"attachmentPath,omitempty"`
	TargetID       string    `json:"targetId,omitempty"`
	GroupID        string    `json:"groupId,omitempty"`
	Private        bool      `json:"private,omitempty"`
	SentAt         time.Time `json:"sentAt"`
	Receipt        bool      `json:"receipt,omitempty"`
	Read           bool      `json:"read,omitempty"`
	ReadAt         time.Time `json:"readAt,omitempty"`
	Recalled       bool      `json:"recalled,omitempty"`
	RecalledAt     time.Time `json:"recalledAt,omitempty"`
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
	IP          string    `json:"ip"`
	Port        string    `json:"port"`
	Browser     string    `json:"browser"`
	OS          string    `json:"os"`
	UserAgent   string    `json:"userAgent"`
	ConnectedAt time.Time `json:"connectedAt"`
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
	mu              sync.RWMutex
	enabled         bool
	groupEnabled    bool
	accepting       bool
	generation      uint64
	peers           map[string]*chatPeer
	conversations   map[string]*chatConversationState
	group           *chatConversationState
	userGroups      map[string]*chatUserGroupState
	groupCreate     bool
	notify          func()
	notifyPending   bool
	logOperation    func(ip, operation string)
	persistence     Persistence
	users           map[string]*ChatUser
	remarks         map[string]map[string]string
	direct          map[string]*chatConversationState
	userListEnabled bool
	privateEnabled  bool
	active          int
	idle            chan struct{}
}

type chatUserGroupState struct {
	ChatGroup
	messages     []ChatMessage
	historyBytes int
}

type chatPeer struct {
	id         string
	ip         string
	viewTarget string
	client     ChatClientInfo
	conn       *websocket.Conn
	send       chan chatWireMessage
	closeReq   chan chatCloseRequest
	closed     chan struct{}
	closeOnce  sync.Once
}

type chatCloseRequest struct {
	code   int
	reason string
}

type chatWireMessage struct {
	Type               string                   `json:"type"`
	Group              bool                     `json:"group"`
	ClientID           string                   `json:"clientId,omitempty"`
	Name               string                   `json:"name,omitempty"`
	Avatar             string                   `json:"avatar,omitempty"`
	Avatars            map[string]string        `json:"avatars,omitempty"`
	ID                 string                   `json:"id,omitempty"`
	Kind               string                   `json:"kind,omitempty"`
	Sender             string                   `json:"sender,omitempty"`
	Text               string                   `json:"text,omitempty"`
	Mime               string                   `json:"mime,omitempty"`
	Data               []byte                   `json:"data,omitempty"`
	FileName           string                   `json:"fileName,omitempty"`
	FileSize           int64                    `json:"fileSize,omitempty"`
	FileURL            string                   `json:"fileUrl,omitempty"`
	TargetID           string                   `json:"targetId,omitempty"`
	Private            bool                     `json:"private,omitempty"`
	Receipt            bool                     `json:"receipt,omitempty"`
	Read               bool                     `json:"read,omitempty"`
	Recalled           bool                     `json:"recalled,omitempty"`
	RecalledAt         time.Time                `json:"recalledAt,omitempty"`
	Users              []ChatPublicUser         `json:"users,omitempty"`
	Groups             []ChatPublicGroup        `json:"groups,omitempty"`
	GroupHistory       map[string][]ChatMessage `json:"groupHistory,omitempty"`
	GroupCreateEnabled bool                     `json:"groupCreateEnabled"`
	GroupID            string                   `json:"groupId,omitempty"`
	GroupName          string                   `json:"groupName,omitempty"`
	Remarks            map[string]string        `json:"remarks,omitempty"`
	UserListEnabled    bool                     `json:"userListEnabled,omitempty"`
	PrivateEnabled     bool                     `json:"privateEnabled,omitempty"`
	DirectHistory      map[string][]ChatMessage `json:"directHistory,omitempty"`
	SentAt             time.Time                `json:"sentAt,omitempty"`
	IDs                []string                 `json:"ids,omitempty"`
	ReadAt             time.Time                `json:"readAt,omitempty"`
	History            []ChatMessage            `json:"history,omitempty"`
}

type chatClientMessage struct {
	Type     string   `json:"type"`
	Kind     string   `json:"kind,omitempty"`
	Name     string   `json:"name,omitempty"`
	Avatar   string   `json:"avatar,omitempty"`
	Text     string   `json:"text,omitempty"`
	Mime     string   `json:"mime,omitempty"`
	Data     []byte   `json:"data,omitempty"`
	FileName string   `json:"fileName,omitempty"`
	TargetID string   `json:"targetId,omitempty"`
	GroupID  string   `json:"groupId,omitempty"`
	Members  []string `json:"members,omitempty"`
	ID       string   `json:"id,omitempty"`
	IDs      []string `json:"ids,omitempty"`
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

func cleanChatAvatar(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	const prefix = "data:image/jpeg;base64,"
	if len(raw) > chatAvatarMaxDataURLBytes || !strings.HasPrefix(raw, prefix) {
		return "", errors.New("头像必须是经过压缩的 JPEG 图片")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, prefix))
	if err != nil || len(decoded) == 0 {
		return "", errors.New("头像数据无效")
	}
	if len(decoded) > chatAvatarMaxBytes {
		return "", fmt.Errorf("头像压缩后不能超过 %d KiB", chatAvatarMaxBytes>>10)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || format != "jpeg" {
		return "", errors.New("头像必须是有效的 JPEG 图片")
	}
	if config.Width != chatAvatarSize || config.Height != chatAvatarSize {
		return "", fmt.Errorf("头像尺寸必须是 %d×%d", chatAvatarSize, chatAvatarSize)
	}
	if _, _, err = image.Decode(bytes.NewReader(decoded)); err != nil {
		return "", errors.New("头像图片无法解析")
	}
	return prefix + base64.StdEncoding.EncodeToString(decoded), nil
}

func (s *Server) handleChatAttachmentUpload(w http.ResponseWriter, r *http.Request, clientIP string) {
	if !s.ChatEnabled() {
		http.Error(w, "聊天功能未启用", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, chatMaxUploadFileBytes+(2<<20))
	multipartReader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "附件上传格式无效", http.StatusBadRequest)
		return
	}
	var part io.ReadCloser
	var rawName, rawMIME string
	for {
		next, nextErr := multipartReader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			http.Error(w, "读取附件失败", http.StatusBadRequest)
			return
		}
		if next.FileName() == "" {
			_ = next.Close()
			continue
		}
		part, rawName, rawMIME = next, next.FileName(), next.Header.Get("Content-Type")
		break
	}
	if part == nil {
		http.Error(w, "没有选择附件", http.StatusBadRequest)
		return
	}
	defer part.Close()
	name, mimeType, err := cleanChatAttachmentMetadata(rawName, rawMIME)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	buffered := bufio.NewReaderSize(part, 512)
	header, _ := buffered.Peek(512)
	detected := http.DetectContentType(header)
	kind, maxBytes := ChatMessageKindFile, chatMaxUploadFileBytes
	if detected == "image/png" || detected == "image/jpeg" {
		kind, maxBytes, mimeType = ChatMessageKindImage, int64(chatMaxUploadImageBytes), detected
	}
	err = s.chat.receiveHTTPAttachment(clientIP, r.URL.Query().Get("targetId"), name, mimeType, kind, buffered, maxBytes)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "大小") || strings.Contains(err.Error(), "exceeds") {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]bool{"uploaded": true})
}

type chatAttachmentForwardRequest struct {
	MessageID string `json:"messageId"`
	TargetID  string `json:"targetId"`
}

func (s *Server) handleChatAttachmentForward(w http.ResponseWriter, r *http.Request, clientIP string) {
	if !s.ChatEnabled() {
		http.Error(w, "聊天功能未启用", http.StatusForbidden)
		return
	}
	persistence := s.Persistence()
	if persistence == nil {
		http.Error(w, "附件持久化服务不可用", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var request chatAttachmentForwardRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "转发请求格式无效", http.StatusBadRequest)
		return
	}
	request.MessageID = strings.TrimSpace(request.MessageID)
	if request.MessageID == "" {
		http.Error(w, "缺少要转发的附件", http.StatusBadRequest)
		return
	}
	attachment, err := persistence.OpenChatAttachment(request.MessageID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "附件不存在或已被删除", http.StatusNotFound)
		} else {
			http.Error(w, "无法读取要转发的附件", http.StatusInternalServerError)
		}
		return
	}
	defer attachment.Reader.Close()
	if !s.chat.conversationVisibleToIP(attachment.ConversationID, clientIP) {
		http.Error(w, "无权转发此附件", http.StatusForbidden)
		return
	}
	kind, maxBytes := ChatMessageKindFile, int64(chatMaxUploadFileBytes)
	if attachment.MIME == "image/png" || attachment.MIME == "image/jpeg" {
		kind, maxBytes = ChatMessageKindImage, int64(chatMaxUploadImageBytes)
	}
	if attachment.Size <= 0 || attachment.Size > maxBytes {
		http.Error(w, "附件大小超过允许范围", http.StatusRequestEntityTooLarge)
		return
	}
	if err = s.chat.receiveHTTPAttachment(
		clientIP,
		request.TargetID,
		attachment.Name,
		attachment.MIME,
		kind,
		attachment.Reader,
		maxBytes,
	); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]bool{"forwarded": true})
}

func newChatHub() *chatHub {
	idle := make(chan struct{})
	close(idle)
	return &chatHub{
		peers:         make(map[string]*chatPeer),
		conversations: make(map[string]*chatConversationState),
		users:         make(map[string]*ChatUser),
		remarks:       make(map[string]map[string]string),
		direct:        make(map[string]*chatConversationState),
		userGroups:    make(map[string]*chatUserGroupState),
		group: &chatConversationState{
			id:   ChatGroupConversationID,
			name: "系统群",
		},
		accepting: true,
		idle:      idle,
	}
}

// SetUserGroupCreationEnabled controls whether browsers may create private
// multi-user groups. The built-in system group is unaffected.
func (s *Server) SetUserGroupCreationEnabled(enabled bool) {
	s.chat.mu.Lock()
	s.chat.groupCreate = enabled
	slow := s.chat.broadcastGroupListsLocked()
	s.chat.mu.Unlock()
	shutdownSlowChatPeers(slow)
}

func (s *Server) UserGroupCreationEnabled() bool {
	s.chat.mu.RLock()
	defer s.chat.mu.RUnlock()
	return s.chat.groupCreate
}

func (s *Server) ChatGroups() []ChatGroup {
	s.chat.mu.RLock()
	groups := make([]ChatGroup, 0, len(s.chat.userGroups))
	for _, state := range s.chat.userGroups {
		group := state.ChatGroup
		group.Members = append([]string(nil), state.Members...)
		groups = append(groups, group)
	}
	s.chat.mu.RUnlock()
	sort.Slice(groups, func(i, j int) bool { return groups[i].UpdatedAt.After(groups[j].UpdatedAt) })
	return groups
}

// RenameChatGroup lets the desktop administrator update a user-created
// group's display name. The change is persisted and pushed to every member.
func (s *Server) RenameChatGroup(groupID, rawName string) error {
	groupID = strings.TrimSpace(groupID)
	name, err := cleanChatName(rawName)
	if err != nil || name == "" {
		if err == nil {
			err = errors.New("群聊名称不能为空")
		}
		return err
	}
	s.chat.mu.Lock()
	group := s.chat.userGroups[groupID]
	if group == nil {
		s.chat.mu.Unlock()
		return errors.New("群聊不存在")
	}
	group.Name = name
	group.UpdatedAt = time.Now().UTC()
	snapshot := group.ChatGroup
	persistence := s.chat.persistence
	slow := s.chat.broadcastGroupListsLocked()
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	shutdownSlowChatPeers(slow)
	if store, ok := persistence.(ChatGroupPersistence); ok {
		if err = store.SaveChatGroup(snapshot); err != nil {
			return fmt.Errorf("保存群聊名称失败：%w", err)
		}
	}
	s.chat.logIPOperation(snapshot.OwnerIP, "后台修改群聊名称 "+name)
	return nil
}

// RemoveChatGroupMember removes a non-owner member from a user-created group.
// The owner remains protected so a group always has a responsible owner.
func (s *Server) RemoveChatGroupMember(groupID, memberIP string) error {
	groupID = strings.TrimSpace(groupID)
	memberIP = normalizeChatIP(memberIP)
	if groupID == "" || memberIP == "" {
		return errors.New("群聊或成员无效")
	}
	s.chat.mu.Lock()
	group := s.chat.userGroups[groupID]
	if group == nil {
		s.chat.mu.Unlock()
		return errors.New("群聊不存在")
	}
	if normalizeChatIP(group.OwnerIP) == memberIP {
		s.chat.mu.Unlock()
		return errors.New("群主不能被剔除，请改为解散群聊")
	}
	if !containsIP(group.Members, memberIP) {
		s.chat.mu.Unlock()
		return errors.New("该用户不在群聊中")
	}
	group.Members = removeIP(group.Members, memberIP)
	group.UpdatedAt = time.Now().UTC()
	snapshot := group.ChatGroup
	persistence := s.chat.persistence
	slow := s.chat.broadcastGroupListsLocked()
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	shutdownSlowChatPeers(slow)
	if store, ok := persistence.(ChatGroupPersistence); ok {
		if err := store.SaveChatGroup(snapshot); err != nil {
			return fmt.Errorf("保存群聊成员失败：%w", err)
		}
	}
	s.chat.logIPOperation(memberIP, "后台从群聊移除用户 "+snapshot.Name)
	return nil
}

// DeleteChatGroup dissolves a user-created group from the desktop manager.
// Existing attachment records remain available in the administrator archive.
func (s *Server) DeleteChatGroup(groupID string) error {
	groupID = strings.TrimSpace(groupID)
	s.chat.mu.Lock()
	group := s.chat.userGroups[groupID]
	if group == nil {
		s.chat.mu.Unlock()
		return errors.New("群聊不存在")
	}
	snapshot := group.ChatGroup
	delete(s.chat.userGroups, groupID)
	persistence := s.chat.persistence
	slow := s.chat.broadcastGroupListsLocked()
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	shutdownSlowChatPeers(slow)
	if store, ok := persistence.(ChatGroupPersistence); ok {
		if err := store.DeleteChatGroup(groupID); err != nil {
			return fmt.Errorf("删除群聊失败：%w", err)
		}
	}
	s.chat.logIPOperation(snapshot.OwnerIP, "后台解散群聊 "+snapshot.Name)
	return nil
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

// SetGroupChatEnabled is retained for configuration compatibility. The
// LanChatGo desktop host enables the system group and never exposes the
// administrator-chat mode; embedders may still select the legacy mode.
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

// ChatOnlineClients returns one current connection record per canonical IP.
// Unlike ChatOverview, it remains accurate while system-group mode is enabled.
func (s *Server) ChatOnlineClients() []ChatClientInfo {
	s.chat.mu.RLock()
	clients := make(map[string]ChatClientInfo, len(s.chat.peers))
	for _, peer := range s.chat.peers {
		if peer.ip == "" {
			continue
		}
		client := peer.client
		client.IP = peer.ip
		if previous, exists := clients[peer.ip]; !exists || client.ConnectedAt.After(previous.ConnectedAt) {
			clients[peer.ip] = client
		}
	}
	s.chat.mu.RUnlock()
	result := make([]ChatClientInfo, 0, len(clients))
	for _, client := range clients {
		result = append(result, client)
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].IP) < strings.ToLower(result[j].IP)
	})
	return result
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

// ChatAdministratorOverview returns every per-IP administrator conversation
// regardless of whether system-group mode is enabled. Attachment payloads are
// omitted so the native UI can poll this summary without loading large files.
func (s *Server) ChatAdministratorOverview() []ChatConversation {
	return s.chat.snapshotAdministratorConversations(false)
}

// ChatConversationSnapshot returns one full, deeply copied conversation.
// Private IDs are unavailable in group mode, and the synthetic group ID is
// unavailable in private mode.
func (s *Server) ChatConversationSnapshot(id string) (ChatConversation, bool) {
	s.chat.mu.RLock()
	item, ok := s.chat.snapshotConversationLocked(id, true)
	s.chat.mu.RUnlock()
	persistence := s.Persistence()
	if ok && persistence != nil {
		for index := range item.Messages {
			message := &item.Messages[index]
			if message.Kind != ChatMessageKindImage || message.AttachmentPath == "" || len(message.Data) > 0 {
				continue
			}
			attachment, err := persistence.OpenChatAttachment(message.ID)
			if err != nil {
				continue
			}
			data, readErr := io.ReadAll(io.LimitReader(attachment.Reader, int64(chatMaxUploadImageBytes)+1))
			_ = attachment.Reader.Close()
			if readErr == nil && len(data) <= chatMaxUploadImageBytes {
				message.Data = data
			}
		}
	}
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

func (h *chatHub) snapshotAdministratorConversations(includeImageData bool) []ChatConversation {
	h.mu.RLock()
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

func (h *chatHub) publicGroupsLocked(ip string) []ChatPublicGroup {
	groups := make([]ChatPublicGroup, 0, len(h.userGroups))
	for _, group := range h.userGroups {
		if !containsIP(group.Members, ip) {
			continue
		}
		groups = append(groups, ChatPublicGroup{
			ID:          group.ID,
			Name:        group.Name,
			OwnerIP:     group.OwnerIP,
			Members:     append([]string(nil), group.Members...),
			MemberCount: len(group.Members),
			Joined:      true,
			Owner:       normalizeChatIP(group.OwnerIP) == normalizeChatIP(ip),
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Joined != groups[j].Joined {
			return groups[i].Joined
		}
		return strings.ToLower(groups[i].Name) < strings.ToLower(groups[j].Name)
	})
	return groups
}

func (h *chatHub) groupHistoryForPeerLocked(ip string) map[string][]ChatMessage {
	result := make(map[string][]ChatMessage)
	for id, group := range h.userGroups {
		if containsIP(group.Members, ip) {
			result[id] = copyChatMessages(group.messages, true)
		}
	}
	return result
}

func (h *chatHub) broadcastGroupListsLocked() []*chatPeer {
	slow := make([]*chatPeer, 0)
	for _, peer := range h.peers {
		update := chatWireMessage{
			Type:               "groups",
			Groups:             h.publicGroupsLocked(peer.ip),
			GroupHistory:       h.groupHistoryForPeerLocked(peer.ip),
			GroupCreateEnabled: h.groupCreate,
		}
		if !peer.enqueue(update) {
			slow = append(slow, peer)
		}
	}
	return slow
}

func (h *chatHub) snapshotConversationLocked(id string, includeImageData bool) (ChatConversation, bool) {
	if id == ChatGroupConversationID {
		if !h.groupEnabled {
			return ChatConversation{}, false
		}
		return ChatConversation{
			ID:       ChatGroupConversationID,
			Name:     h.group.name,
			Online:   len(h.peers) > 0,
			Messages: copyChatMessages(h.group.messages, includeImageData),
		}, true
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

// RemoveChatVisitor removes one IP's conversation and persisted user record
// without closing its socket. Offline historical users are removable as well.
// A connected IP remains connected and can reappear after new activity.
func (s *Server) RemoveChatVisitor(id string) bool {
	if id == "" || id == ChatGroupConversationID {
		return false
	}
	id = normalizeChatIP(id)
	if id == "" {
		return false
	}
	s.chat.mu.Lock()
	_, conversationExists := s.chat.conversations[id]
	_, userExists := s.chat.users[id]
	if !conversationExists && !userExists {
		s.chat.mu.Unlock()
		return false
	}
	delete(s.chat.conversations, id)
	delete(s.chat.users, id)
	persistence := s.chat.persistence
	slow := s.chat.broadcastUserListLocked()
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	if persistence != nil {
		_ = persistence.DeleteChatConversation(adminConversationID(id))
		_ = persistence.DeleteChatUser(id)
	}
	shutdownSlowChatPeers(slow)
	s.chat.logIPOperation(id, "后台移除 IP 记录")
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
	cleanMime, cleanData, err := cleanLargeChatImage(mime, data)
	if err != nil {
		return err
	}
	message := newAdminChatMessage(ChatMessageKindImage)
	message.Mime = cleanMime
	message.Data = cleanData
	return s.sendAdminChatMessage(clientID, message)
}

// SendChatFile validates and queues one administrator attachment.
func (s *Server) SendChatFile(clientID, name, mime string, data []byte) error {
	cleanName, cleanMime, cleanData, err := cleanChatFile(name, mime, data)
	if err != nil {
		return err
	}
	message := newAdminChatMessage(ChatMessageKindFile)
	message.FileName = cleanName
	message.Mime = cleanMime
	message.Data = cleanData
	message.FileSize = int64(len(cleanData))
	return s.sendAdminChatMessage(clientID, message)
}

// SendChatAttachmentPath streams a native desktop file into the persistent
// chat store without copying it through the WebView bridge as Base64.
func (s *Server) SendChatAttachmentPath(clientID, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return errors.New("只能发送非空普通文件")
	}
	name, mimeType, err := cleanChatAttachmentMetadata(filepath.Base(path), mime.TypeByExtension(filepath.Ext(path)))
	if err != nil {
		return err
	}
	kind, maxBytes := ChatMessageKindFile, chatMaxUploadFileBytes
	if strings.EqualFold(mimeType, "image/png") || strings.EqualFold(mimeType, "image/jpeg") {
		kind, maxBytes = ChatMessageKindImage, int64(chatMaxUploadImageBytes)
	}
	if info.Size() > maxBytes {
		return errors.New("附件超过允许的大小上限")
	}
	message := newAdminChatMessage(kind)
	message.FileName, message.Mime, message.FileSize = name, mimeType, info.Size()
	return s.chat.receiveAdminAttachment(clientID, message, file, maxBytes)
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
	s.chat.mu.Lock()
	if !s.chat.enabled {
		s.chat.mu.Unlock()
		return errors.New("聊天功能未启用")
	}
	if s.chat.groupEnabled {
		rawTarget := strings.TrimSpace(clientID)
		if rawTarget != "" && rawTarget != ChatGroupConversationID {
			clientID = normalizeChatIP(rawTarget)
			if clientID == "" {
				s.chat.mu.Unlock()
				return errors.New("私信目标 IP 无效")
			}
			if !s.chat.userListEnabled || !s.chat.privateEnabled {
				s.chat.mu.Unlock()
				return errors.New("管理员私信功能未启用")
			}
			user := s.chat.users[clientID]
			peers := s.chat.peersForIPLocked(clientID)
			if user == nil || user.Blacklisted || len(peers) == 0 {
				s.chat.mu.Unlock()
				return errors.New("私信用户已离线")
			}
			conversation := s.chat.conversations[clientID]
			if conversation == nil {
				conversation = &chatConversationState{
					id: clientID, name: displayChatUser(*user),
					client: peers[0].client, updated: time.Now().UTC(),
				}
				s.chat.conversations[clientID] = conversation
			}
			message.Private = true
			message.TargetID = clientID
			stored, err := s.chat.persistMessageLocked(adminConversationID(clientID), message)
			if err != nil {
				s.chat.mu.Unlock()
				return fmt.Errorf("保存管理员私信失败：%w", err)
			}
			wire := wireFromMessage(stored)
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
				return errors.New("私信用户连接繁忙，请稍后重试")
			}
			appendChatMessage(conversation, stored)
			s.chat.scheduleNotifyLocked()
			s.chat.mu.Unlock()
			shutdownSlowChatPeers(slow)
			s.chat.logIPOperation(clientID, adminChatOperation(message))
			return nil
		}
		targets := s.chat.onlineIPsLocked()
		stored, err := s.chat.persistMessageLocked(ChatGroupConversationID, message)
		if err != nil {
			s.chat.mu.Unlock()
			return fmt.Errorf("保存聊天记录失败：%w", err)
		}
		wire := wireFromMessage(stored)
		wire.Group = true
		appendChatMessage(s.chat.group, stored)
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
	stored, err := s.chat.persistMessageLocked(adminConversationID(clientID), message)
	if err != nil {
		s.chat.mu.Unlock()
		return fmt.Errorf("保存聊天记录失败：%w", err)
	}
	wire := wireFromMessage(stored)
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
	appendChatMessage(conversation, stored)
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
	if message.Kind == ChatMessageKindFile {
		return "后台发送聊天文件"
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
	clientHints := cleanClientHeader(r.Header.Get("Sec-CH-UA"), 256)
	platform := strings.Trim(cleanClientHeader(r.Header.Get("Sec-CH-UA-Platform"), 64), `"`)
	platformVersion := strings.Trim(cleanClientHeader(r.Header.Get("Sec-CH-UA-Platform-Version"), 64), `"`)
	if userAgent == "" {
		userAgent = clientHints
	}
	if userAgent == "" && platform != "" {
		userAgent = "Client Hints: " + platform
		if platformVersion != "" {
			userAgent += " " + platformVersion
		}
	}
	browser := browserFromUserAgent(userAgent)
	if hinted := browserFromClientHints(clientHints); hinted != "" &&
		(strings.Contains(browser, "未知") || strings.Contains(browser, "其他")) {
		browser = hinted
	}
	return ChatClientInfo{
		IP:          host,
		Port:        port,
		Browser:     browser,
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

// browserFromClientHints handles Chromium's reduced User-Agent mode, where
// the traditional User-Agent may omit the browser brand and version.
func browserFromClientHints(raw string) string {
	for _, candidate := range []struct {
		brand string
		name  string
	}{
		{"Microsoft Edge", "Microsoft Edge"},
		{"Opera", "Opera"},
		{"Google Chrome", "Google Chrome"},
		{"Chromium", "Chromium"},
	} {
		needle := `"` + strings.ToLower(candidate.brand) + `"`
		lower := strings.ToLower(raw)
		index := strings.Index(lower, needle)
		if index < 0 {
			continue
		}
		value := raw[index+len(needle):]
		version := ""
		if marker := strings.Index(value, `;v="`); marker >= 0 {
			value = value[marker+4:]
			if end := strings.IndexByte(value, '"'); end >= 0 {
				version = cleanClientHeader(value[:end], 32)
			}
		}
		if version != "" {
			return candidate.name + " " + version
		}
		return candidate.name
	}
	return ""
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
	user := h.ensureUserLocked(peer.ip, peer.client)
	if user.Blacklisted {
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
		conversation = &chatConversationState{id: peer.ip, name: displayChatUser(*user), client: peer.client}
		h.conversations[peer.ip] = conversation
	}
	conversation.client = peer.client
	conversation.name = displayChatUser(*user)
	conversation.updated = time.Now().UTC()
	h.peers[peer.id] = peer
	history := conversation.messages
	if h.groupEnabled {
		history = h.group.messages
	}
	ready := chatWireMessage{
		Type:               "ready",
		Group:              h.groupEnabled,
		ClientID:           peer.ip,
		Name:               displayChatUser(*user),
		History:            copyChatMessages(history, true),
		Users:              h.publicUsersLocked(peer.ip),
		Avatars:            h.publicAvatarsLocked(peer.ip),
		UserListEnabled:    h.userListEnabled,
		PrivateEnabled:     h.privateEnabled,
		DirectHistory:      h.directHistoryForPeerLocked(peer.ip),
		Remarks:            h.remarksForPeerLocked(peer.ip),
		Groups:             h.publicGroupsLocked(peer.ip),
		GroupHistory:       h.groupHistoryForPeerLocked(peer.ip),
		GroupCreateEnabled: h.groupCreate,
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
	h.persistUserLocked(user)
	slow := h.broadcastUserListLocked()
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
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
	var slow []*chatPeer
	h.mu.Lock()
	if h.peers[peer.id] == peer {
		delete(h.peers, peer.id)
		if conversation := h.conversations[peer.ip]; conversation != nil {
			conversation.updated = time.Now().UTC()
		}
		if user := h.users[peer.ip]; user != nil {
			user.LastSeen = time.Now().UTC()
			user.Client = peer.client
			h.persistUserLocked(user)
		}
		slow = h.broadcastUserListLocked()
		h.scheduleNotifyLocked()
		removed = true
	}
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
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
	events, totalEvents := 0, 0
	rateWarningSent := false
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
			windowStarted, events, totalEvents = now, 0, 0
			rateWarningSent = false
		}
		totalEvents++
		if totalEvents > chatRateHardEvents {
			p.shutdown(websocket.ClosePolicyViolation, "excessive websocket traffic")
			return
		}
		var incoming chatClientMessage
		if err := json.Unmarshal(payload, &incoming); err != nil {
			p.enqueue(chatWireMessage{Type: "error", Text: "消息格式无效"})
			continue
		}
		if chatEventCountsTowardsRate(incoming.Type) {
			events++
			if events > chatRateEvents {
				if !rateWarningSent {
					p.enqueue(chatWireMessage{Type: "error", Text: "发送速度较快，本条消息未发送，请稍后再试"})
					rateWarningSent = true
				}
				continue
			}
		}
		switch incoming.Type {
		case "message":
			var err error
			if incoming.GroupID != "" {
				err = h.receiveGroupTargetedMessage(p, incoming, incoming.GroupID)
			} else if incoming.TargetID != "" {
				err = h.receiveTargetedMessage(p, incoming, incoming.TargetID)
			} else {
				switch incoming.Kind {
				case "", ChatMessageKindText:
					err = h.receiveMessage(p, incoming.Text)
				case ChatMessageKindImage:
					err = h.receiveImage(p, incoming.Mime, incoming.Data)
				case ChatMessageKindFile:
					err = h.receiveFile(p, incoming.FileName, incoming.Mime, incoming.Data, "")
				default:
					err = errors.New("未知的消息类型")
				}
			}
			if err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
			}
		case "createGroup":
			if err := h.createUserGroup(p, incoming.Name, incoming.Members); err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
			}
		case "leaveGroup":
			if err := h.leaveUserGroup(p, incoming.GroupID); err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
			}
		case "dissolveGroup":
			if err := h.dissolveUserGroup(p, incoming.GroupID); err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
			}
		case "image":
			if err := h.receiveImage(p, incoming.Mime, incoming.Data); err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
			}
		case "name":
			// Keep the legacy frame immutable for older clients. New clients
			// use setName, while the canonical identity always remains the IP.
			p.enqueue(chatWireMessage{Type: "name", Name: p.ip})
		case "setName":
			name, err := cleanChatName(incoming.Name)
			if err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
				continue
			}
			h.updatePeerName(p, name)
		case "setAvatar":
			avatar, err := cleanChatAvatar(incoming.Avatar)
			if err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
				continue
			}
			h.updatePeerAvatar(p, avatar)
		case "setRemark":
			name, err := cleanChatName(incoming.Name)
			if err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
				continue
			}
			if err = h.updatePeerRemark(p, incoming.TargetID, name); err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
			}
		case "recall":
			if err := h.recallPeerMessage(p, incoming.ID); err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
			}
		case "view":
			h.updatePeerView(p, incoming.TargetID)
		case "read":
			if err := h.markMessagesRead(p, incoming.TargetID, incoming.IDs); err != nil {
				p.enqueue(chatWireMessage{Type: "error", Text: err.Error()})
			}
		default:
			p.enqueue(chatWireMessage{Type: "error", Text: "未知的聊天操作"})
		}
	}
}

func chatEventCountsTowardsRate(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "view", "read":
		return false
	default:
		return true
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

func (h *chatHub) createUserGroup(peer *chatPeer, rawName string, members []string) error {
	name, err := cleanChatName(rawName)
	if err != nil || strings.TrimSpace(name) == "" {
		if err == nil {
			err = errors.New("群名称不能为空")
		}
		return err
	}
	h.mu.Lock()
	if !h.enabled || h.peers[peer.id] != peer {
		h.mu.Unlock()
		return errors.New("聊天连接已关闭")
	}
	if !h.groupCreate {
		h.mu.Unlock()
		return errors.New("管理员未开启建立群聊")
	}
	if !h.userListEnabled {
		h.mu.Unlock()
		return errors.New("管理员未开启用户列表")
	}
	normalized := normalizeGroupMembers(append(members, peer.ip))
	filtered := normalized[:0]
	for _, member := range normalized {
		user := h.users[member]
		if user == nil || user.Blacklisted {
			continue
		}
		filtered = append(filtered, member)
	}
	normalized = normalizeGroupMembers(filtered)
	if len(normalized) < 2 {
		h.mu.Unlock()
		return errors.New("至少选择一名其他用户")
	}
	now := time.Now().UTC()
	group := ChatGroup{
		ID:        "g-" + newChatMessageID(),
		Name:      name,
		OwnerIP:   peer.ip,
		Members:   normalized,
		CreatedAt: now,
		UpdatedAt: now,
	}
	state := &chatUserGroupState{ChatGroup: group}
	h.userGroups[group.ID] = state
	persistence := h.persistence
	slow := h.broadcastGroupListsLocked()
	for _, recipient := range h.peersForGroupLocked(group.ID) {
		recipient.enqueue(chatWireMessage{Type: "groupCreated", ClientID: peer.ip, GroupID: group.ID, GroupName: group.Name})
	}
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	if store, ok := persistence.(ChatGroupPersistence); ok {
		if err := store.SaveChatGroup(group); err != nil {
			return fmt.Errorf("保存群聊失败：%w", err)
		}
	}
	h.logIPOperation(peer.ip, "创建 "+group.Name)
	return nil
}

func (h *chatHub) leaveUserGroup(peer *chatPeer, groupID string) error {
	groupID = strings.TrimSpace(groupID)
	h.mu.Lock()
	group := h.userGroups[groupID]
	if group == nil || !containsIP(group.Members, peer.ip) {
		h.mu.Unlock()
		return errors.New("群聊不存在或你不在群内")
	}
	if normalizeChatIP(group.OwnerIP) == normalizeChatIP(peer.ip) {
		h.mu.Unlock()
		return errors.New("群主不能退出，请先解散群聊")
	}
	group.Members = removeIP(group.Members, peer.ip)
	group.UpdatedAt = time.Now().UTC()
	if len(group.Members) == 0 {
		delete(h.userGroups, groupID)
	}
	persistence := h.persistence
	slow := h.broadcastGroupListsLocked()
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	if store, ok := persistence.(ChatGroupPersistence); ok {
		var err error
		if len(group.Members) == 0 {
			err = store.DeleteChatGroup(groupID)
		} else {
			err = store.SaveChatGroup(group.ChatGroup)
		}
		if err != nil {
			return fmt.Errorf("保存群聊成员失败：%w", err)
		}
	}
	h.logIPOperation(peer.ip, "退出群聊 "+groupID)
	return nil
}

func (h *chatHub) dissolveUserGroup(peer *chatPeer, groupID string) error {
	groupID = strings.TrimSpace(groupID)
	h.mu.Lock()
	group := h.userGroups[groupID]
	if group == nil {
		h.mu.Unlock()
		return errors.New("群聊不存在")
	}
	if normalizeChatIP(group.OwnerIP) != normalizeChatIP(peer.ip) {
		h.mu.Unlock()
		return errors.New("只有群主可以解散群聊")
	}
	delete(h.userGroups, groupID)
	persistence := h.persistence
	slow := h.broadcastGroupListsLocked()
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	if store, ok := persistence.(ChatGroupPersistence); ok {
		if err := store.DeleteChatGroup(groupID); err != nil {
			return fmt.Errorf("删除群聊失败：%w", err)
		}
	}
	h.logIPOperation(peer.ip, "解散群聊 "+groupID)
	return nil
}

func (h *chatHub) receiveGroupTargetedMessage(peer *chatPeer, incoming chatClientMessage, groupID string) error {
	groupID = strings.TrimSpace(groupID)
	var message ChatMessage
	switch incoming.Kind {
	case "", ChatMessageKindText:
		text, err := cleanChatText(incoming.Text)
		if err != nil {
			return err
		}
		message = ChatMessage{Kind: ChatMessageKindText, Text: text}
	case ChatMessageKindImage:
		mimeType, data, err := cleanChatImage(incoming.Mime, incoming.Data)
		if err != nil {
			return err
		}
		message = ChatMessage{Kind: ChatMessageKindImage, Mime: mimeType, Data: data, FileSize: int64(len(data))}
	case ChatMessageKindFile:
		name, mimeType, data, err := cleanChatFile(incoming.FileName, incoming.Mime, incoming.Data)
		if err != nil {
			return err
		}
		message = ChatMessage{Kind: ChatMessageKindFile, Mime: mimeType, Data: data, FileName: name, FileSize: int64(len(data))}
	default:
		return errors.New("未知的消息类型")
	}
	message.ID = newChatMessageID()
	message.Sender = "user"
	message.ClientID = peer.ip
	message.GroupID = groupID
	message.SentAt = time.Now().UTC()
	return h.receiveUserGroupMessage(peer, groupID, message)
}

func (h *chatHub) receiveUserGroupMessage(peer *chatPeer, groupID string, message ChatMessage) error {
	h.mu.Lock()
	group := h.userGroups[groupID]
	if group == nil || !containsIP(group.Members, peer.ip) {
		h.mu.Unlock()
		return errors.New("群聊不存在或你不在群内")
	}
	user := h.ensureUserLocked(peer.ip, peer.client)
	message.Name = displayChatUser(*user)
	message.ClientID = peer.ip
	message.GroupID = groupID
	stored, err := h.persistMessageLocked(chatUserGroupPrefix+groupID, message)
	if err != nil {
		h.mu.Unlock()
		return fmt.Errorf("保存群聊消息失败：%w", err)
	}
	group.messages = append(group.messages, stored)
	group.historyBytes += chatMessageHistoryBytes(stored)
	for len(group.messages) > chatMaxHistory {
		group.historyBytes -= chatMessageHistoryBytes(group.messages[0])
		group.messages = group.messages[1:]
	}
	group.UpdatedAt = stored.SentAt
	wire := wireFromMessage(stored)
	wire.GroupID = groupID
	var slow []*chatPeer
	delivered := 0
	for _, recipient := range h.peersForGroupLocked(groupID) {
		if recipient.enqueue(wire) {
			delivered++
		} else {
			slow = append(slow, recipient)
		}
	}
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	if delivered == 0 {
		return errors.New("群聊当前没有在线成员")
	}
	h.logIPOperation(peer.ip, "发送群聊消息")
	return nil
}

func (h *chatHub) peersForGroupLocked(groupID string) []*chatPeer {
	group := h.userGroups[groupID]
	if group == nil {
		return nil
	}
	result := make([]*chatPeer, 0, len(group.Members))
	for _, member := range group.Members {
		result = append(result, h.peersForIPLocked(member)...)
	}
	return result
}

func removeIP(members []string, target string) []string {
	result := members[:0]
	for _, member := range members {
		if normalizeChatIP(member) != normalizeChatIP(target) {
			result = append(result, member)
		}
	}
	return result
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

func (h *chatHub) receiveFile(peer *chatPeer, name, mime string, data []byte, targetID string) error {
	name, mime, data, err := cleanChatFile(name, mime, data)
	if err != nil {
		return err
	}
	message := ChatMessage{
		ID:       newChatMessageID(),
		Kind:     ChatMessageKindFile,
		Sender:   "user",
		Mime:     mime,
		Data:     data,
		FileName: name,
		FileSize: int64(len(data)),
		TargetID: targetID,
		SentAt:   time.Now().UTC(),
	}
	if targetID != "" {
		return h.receiveDirectMessage(peer, targetID, message)
	}
	return h.receivePeerChatMessage(peer, message)
}

func (h *chatHub) receiveTargetedMessage(peer *chatPeer, incoming chatClientMessage, targetID string) error {
	var message ChatMessage
	switch incoming.Kind {
	case "", ChatMessageKindText:
		text, err := cleanChatText(incoming.Text)
		if err != nil {
			return err
		}
		message = ChatMessage{Kind: ChatMessageKindText, Text: text}
	case ChatMessageKindImage:
		mime, data, err := cleanChatImage(incoming.Mime, incoming.Data)
		if err != nil {
			return err
		}
		message = ChatMessage{Kind: ChatMessageKindImage, Mime: mime, Data: data, FileSize: int64(len(data))}
	case ChatMessageKindFile:
		name, mime, data, err := cleanChatFile(incoming.FileName, incoming.Mime, incoming.Data)
		if err != nil {
			return err
		}
		message = ChatMessage{Kind: ChatMessageKindFile, Mime: mime, Data: data, FileName: name, FileSize: int64(len(data))}
	default:
		return errors.New("未知的消息类型")
	}
	message.ID = newChatMessageID()
	message.Sender = "user"
	message.TargetID = targetID
	message.Private = true
	message.SentAt = time.Now().UTC()
	return h.receiveDirectMessage(peer, targetID, message)
}

func (h *chatHub) receiveDirectMessage(peer *chatPeer, targetID string, message ChatMessage) error {
	if strings.TrimSpace(targetID) == ChatAdminConversationID {
		return h.receiveAdministratorPrivateMessage(peer, message)
	}
	targetID = normalizeChatIP(targetID)
	if targetID == "" || targetID == peer.ip {
		return errors.New("私信目标无效")
	}
	h.mu.Lock()
	if !h.enabled || h.peers[peer.id] != peer {
		h.mu.Unlock()
		return errors.New("聊天连接已关闭")
	}
	if !h.userListEnabled || !h.privateEnabled {
		h.mu.Unlock()
		return errors.New("管理员尚未开启用户私信")
	}
	targetUser := h.users[targetID]
	if targetUser == nil || targetUser.Blacklisted || !h.ipOnlineLocked(targetID) {
		h.mu.Unlock()
		return errors.New("私信用户已离线")
	}
	sender := h.ensureUserLocked(peer.ip, peer.client)
	if h.userListEnabled && strings.TrimSpace(sender.Name) == "" {
		h.mu.Unlock()
		return errors.New("请先设置名称")
	}
	message.ClientID = peer.ip
	message.Name = displayChatUser(*sender)
	message.TargetID = targetID
	message.Private = true
	conversationID := directConversationID(peer.ip, targetID)
	conversation := h.direct[conversationID]
	if conversation == nil {
		conversation = &chatConversationState{id: conversationID, name: "私信"}
		h.direct[conversationID] = conversation
	}
	stored, err := h.persistMessageLocked(conversationID, message)
	if err != nil {
		h.mu.Unlock()
		return fmt.Errorf("保存私信失败：%w", err)
	}
	appendChatMessage(conversation, stored)
	wire := wireFromMessage(stored)
	recipients := append(h.peersForIPLocked(peer.ip), h.peersForIPLocked(targetID)...)
	delivered := 0
	var slow []*chatPeer
	seen := make(map[*chatPeer]struct{})
	for _, recipient := range recipients {
		if _, duplicate := seen[recipient]; duplicate {
			continue
		}
		seen[recipient] = struct{}{}
		if recipient.enqueue(wire) {
			delivered++
		} else {
			slow = append(slow, recipient)
		}
	}
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	if delivered == 0 {
		return errors.New("私信连接繁忙，请稍后重试")
	}
	h.logIPOperation(peer.ip, "发送用户私信")
	return nil
}

func (h *chatHub) markMessagesRead(peer *chatPeer, rawTargetID string, rawIDs []string) error {
	if len(rawIDs) == 0 {
		return nil
	}
	targetID := normalizeChatIP(rawTargetID)
	if targetID == "" || targetID == peer.ip {
		return nil
	}
	capacity := len(rawIDs)
	if capacity > 100 {
		capacity = 100
	}
	requested := make(map[string]struct{}, capacity)
	for _, rawID := range rawIDs {
		id := strings.TrimSpace(rawID)
		if id == "" || len(id) > 128 {
			continue
		}
		requested[id] = struct{}{}
		if len(requested) == 100 {
			break
		}
	}
	if len(requested) == 0 {
		return nil
	}

	h.mu.Lock()
	if !h.enabled || h.peers[peer.id] != peer {
		h.mu.Unlock()
		return errors.New("聊天连接已关闭")
	}
	if peer.viewTarget != targetID {
		h.mu.Unlock()
		return nil
	}
	matches := make(map[string][]*ChatMessage)
	conversation := h.direct[directConversationID(peer.ip, targetID)]
	if conversation == nil {
		h.mu.Unlock()
		return nil
	}
	for index := range conversation.messages {
		message := &conversation.messages[index]
		if _, wanted := requested[message.ID]; !wanted ||
			!message.Receipt || message.Read ||
			normalizeChatIP(message.ClientID) != targetID ||
			normalizeChatIP(message.TargetID) != peer.ip {
			continue
		}
		matches[message.ID] = append(matches[message.ID], message)
	}
	if len(matches) == 0 {
		h.mu.Unlock()
		return nil
	}
	ids := make([]string, 0, len(matches))
	for id := range matches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	readAt := time.Now().UTC()
	if persistence, ok := h.persistence.(ChatReadPersistence); ok {
		if err := persistence.MarkChatMessagesRead(ids, readAt); err != nil {
			h.mu.Unlock()
			return fmt.Errorf("保存已读状态失败：%w", err)
		}
	}
	for _, id := range ids {
		for _, message := range matches[id] {
			message.Read = true
			message.ReadAt = readAt
		}
	}
	frame := chatWireMessage{
		Type:     "read",
		ClientID: targetID,
		TargetID: peer.ip,
		IDs:      ids,
		ReadAt:   readAt,
	}
	var slow []*chatPeer
	for _, recipient := range h.peers {
		if recipient.ip != targetID {
			continue
		}
		if !recipient.enqueue(frame) {
			slow = append(slow, recipient)
		}
	}
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	return nil
}

func (h *chatHub) updatePeerView(peer *chatPeer, rawTargetID string) {
	targetID := normalizeChatIP(rawTargetID)
	if targetID == peer.ip {
		targetID = ""
	}
	h.mu.Lock()
	if h.peers[peer.id] == peer {
		peer.viewTarget = targetID
	}
	h.mu.Unlock()
}

func (h *chatHub) recallPeerMessage(peer *chatPeer, messageID string) error {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return errors.New("撤回的消息无效")
	}
	now := time.Now().UTC()
	h.mu.Lock()
	if !h.enabled || h.peers[peer.id] != peer {
		h.mu.Unlock()
		return errors.New("聊天连接已关闭")
	}
	var conversationID string
	var conversation *chatConversationState
	var userGroup *chatUserGroupState
	var messageIndex = -1
	if h.groupEnabled {
		for index := range h.group.messages {
			if h.group.messages[index].ID == messageID {
				conversationID, conversation, messageIndex = ChatGroupConversationID, h.group, index
				break
			}
		}
	}
	if messageIndex < 0 {
		for id, candidate := range h.userGroups {
			if !containsIP(candidate.Members, peer.ip) {
				continue
			}
			for index := range candidate.messages {
				if candidate.messages[index].ID == messageID {
					conversationID, userGroup, messageIndex = chatUserGroupPrefix+id, candidate, index
					break
				}
			}
			if messageIndex >= 0 {
				break
			}
		}
	}
	if messageIndex < 0 {
		for id, candidate := range h.direct {
			first, second, ok := parseDirectConversationID(id)
			if !ok || first != peer.ip && second != peer.ip {
				continue
			}
			for index := range candidate.messages {
				if candidate.messages[index].ID == messageID {
					conversationID, conversation, messageIndex = id, candidate, index
					break
				}
			}
			if messageIndex >= 0 {
				break
			}
		}
	}
	if messageIndex < 0 || (conversation == nil && userGroup == nil) {
		h.mu.Unlock()
		return errors.New("消息不存在或已被清理")
	}
	var message ChatMessage
	if conversation != nil {
		message = conversation.messages[messageIndex]
	} else {
		message = userGroup.messages[messageIndex]
	}
	if message.Recalled {
		h.mu.Unlock()
		return errors.New("消息已经撤回")
	}
	if message.Sender == "admin" || normalizeChatIP(message.ClientID) != peer.ip {
		h.mu.Unlock()
		return errors.New("只能撤回自己发送的消息")
	}
	if message.SentAt.IsZero() || now.Sub(message.SentAt) > 2*time.Minute {
		h.mu.Unlock()
		return errors.New("消息发送超过 2 分钟，无法撤回")
	}
	if h.persistence != nil {
		stored, err := h.persistence.RecallChatMessage(messageID, now)
		if err != nil {
			h.mu.Unlock()
			if errors.Is(err, os.ErrNotExist) {
				return errors.New("消息不存在或已被清理")
			}
			return fmt.Errorf("撤回消息失败：%w", err)
		}
		message = stored.Message
	} else {
		message.Recalled = true
		message.RecalledAt = now
		message.Text, message.Mime, message.FileName, message.FileURL, message.AttachmentPath = "", "", "", "", ""
		message.Data = nil
		message.FileSize = 0
	}
	if conversation != nil {
		conversation.historyBytes += chatMessageHistoryBytes(message) - chatMessageHistoryBytes(conversation.messages[messageIndex])
		conversation.messages[messageIndex] = cloneChatMessage(message)
	} else {
		userGroup.historyBytes += chatMessageHistoryBytes(message) - chatMessageHistoryBytes(userGroup.messages[messageIndex])
		userGroup.messages[messageIndex] = cloneChatMessage(message)
	}
	wire := wireFromMessage(message)
	wire.Type = "recall"
	var recipients []*chatPeer
	if conversationID == ChatGroupConversationID {
		for _, recipient := range h.peers {
			recipients = append(recipients, recipient)
		}
	} else if userGroup != nil {
		recipients = append(recipients, h.peersForGroupLocked(userGroup.ID)...)
	} else {
		first, second, _ := parseDirectConversationID(conversationID)
		recipients = append(recipients, h.peersForIPLocked(first)...)
		recipients = append(recipients, h.peersForIPLocked(second)...)
	}
	var slow []*chatPeer
	seen := make(map[*chatPeer]struct{})
	for _, recipient := range recipients {
		if _, exists := seen[recipient]; exists {
			continue
		}
		seen[recipient] = struct{}{}
		if !recipient.enqueue(wire) {
			slow = append(slow, recipient)
		}
	}
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	h.logIPOperation(peer.ip, "撤回聊天消息")
	return nil
}

func (h *chatHub) receiveAdministratorPrivateMessage(peer *chatPeer, message ChatMessage) error {
	h.mu.Lock()
	if !h.enabled || h.peers[peer.id] != peer {
		h.mu.Unlock()
		return errors.New("聊天连接已关闭")
	}
	if !h.userListEnabled || !h.privateEnabled {
		h.mu.Unlock()
		return errors.New("管理员尚未开启私信功能")
	}
	sender := h.ensureUserLocked(peer.ip, peer.client)
	if strings.TrimSpace(sender.Name) == "" {
		h.mu.Unlock()
		return errors.New("请先设置名称")
	}
	conversation := h.conversations[peer.ip]
	if conversation == nil {
		conversation = &chatConversationState{
			id: peer.ip, name: displayChatUser(*sender),
			client: peer.client, updated: time.Now().UTC(),
		}
		h.conversations[peer.ip] = conversation
	}
	message.ClientID = peer.ip
	message.Name = displayChatUser(*sender)
	message.TargetID = ChatAdminConversationID
	message.Private = true
	stored, err := h.persistMessageLocked(adminConversationID(peer.ip), message)
	if err != nil {
		h.mu.Unlock()
		return fmt.Errorf("保存管理员私信失败：%w", err)
	}
	appendChatMessage(conversation, stored)
	wire := wireFromMessage(stored)
	delivered := 0
	var slow []*chatPeer
	for _, recipient := range h.peersForIPLocked(peer.ip) {
		if recipient.enqueue(wire) {
			delivered++
		} else {
			slow = append(slow, recipient)
		}
	}
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	if delivered == 0 {
		return errors.New("管理员私信连接繁忙，请稍后重试")
	}
	h.logIPOperation(peer.ip, "向管理员发送私信")
	return nil
}

func (h *chatHub) updatePeerName(peer *chatPeer, name string) {
	h.mu.Lock()
	if h.peers[peer.id] != peer {
		h.mu.Unlock()
		return
	}
	user := h.ensureUserLocked(peer.ip, peer.client)
	user.Name = name
	copy := *user
	if conversation := h.conversations[peer.ip]; conversation != nil {
		conversation.name = displayChatUser(copy)
	}
	persistence := h.persistence
	label := displayChatUser(copy)
	_ = peer.enqueue(chatWireMessage{Type: "name", ClientID: peer.ip, Name: label})
	slow := h.broadcastUserListLocked()
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	if persistence != nil {
		_ = persistence.SaveChatUser(copy)
	}
	shutdownSlowChatPeers(slow)
}

func (h *chatHub) updatePeerAvatar(peer *chatPeer, avatar string) {
	h.mu.Lock()
	if h.peers[peer.id] != peer {
		h.mu.Unlock()
		return
	}
	user := h.ensureUserLocked(peer.ip, peer.client)
	user.Avatar = avatar
	copy := *user
	persistence := h.persistence
	_ = peer.enqueue(chatWireMessage{
		Type: "avatar", ClientID: peer.ip, Avatar: avatar,
	})
	slow := h.broadcastUserListLocked()
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	if persistence != nil {
		_ = persistence.SaveChatUser(copy)
	}
	shutdownSlowChatPeers(slow)
}

func (h *chatHub) persistMessageLocked(conversationID string, message ChatMessage) (ChatMessage, error) {
	message.Receipt = message.Sender == "user" && strings.HasPrefix(conversationID, directConversationPrefix)
	if h.persistence == nil {
		return message, nil
	}
	stored, err := h.persistence.SaveChatMessage(conversationID, message)
	if err != nil {
		return message, err
	}
	if stored.AttachmentPath != "" {
		stored.FileURL = chatAttachmentURL(stored)
		stored.Data = nil
	}
	return stored, nil
}

func (h *chatHub) persistAttachment(conversationID string, message ChatMessage, reader io.Reader, maxBytes int64) (ChatMessage, error) {
	if h.persistence == nil {
		return message, errors.New("附件持久化服务不可用")
	}
	message.Receipt = message.Sender == "user" && strings.HasPrefix(conversationID, directConversationPrefix)
	stored, err := h.persistence.SaveChatAttachment(conversationID, message, reader, maxBytes)
	if err != nil {
		return message, err
	}
	stored.FileURL = chatAttachmentURL(stored)
	stored.Data = nil
	return stored, nil
}

// receiveHTTPAttachment releases the hub mutex while the body is streamed to
// disk, then verifies the route is still valid before publishing the message.
func (h *chatHub) receiveHTTPAttachment(ip, targetID, name, mimeType, kind string, reader io.Reader, maxBytes int64) error {
	ip = normalizeChatIP(ip)
	targetID = strings.TrimSpace(targetID)
	groupID := ""
	if strings.HasPrefix(targetID, chatUserGroupPrefix) {
		groupID = strings.TrimPrefix(targetID, chatUserGroupPrefix)
		targetID = ""
	}
	adminTarget := targetID == ChatAdminConversationID
	if !adminTarget {
		targetID = normalizeChatIP(targetID)
	}
	h.mu.Lock()
	peers := h.peersForIPLocked(ip)
	if !h.enabled || ip == "" || len(peers) == 0 {
		h.mu.Unlock()
		return errors.New("聊天连接已断开")
	}
	peer := peers[0]
	user := h.ensureUserLocked(ip, peer.client)
	if h.userListEnabled && strings.TrimSpace(user.Name) == "" {
		h.mu.Unlock()
		return errors.New("请先设置名称")
	}
	message := ChatMessage{
		ID: newChatMessageID(), Kind: kind, Sender: "user", ClientID: ip,
		Name: displayChatUser(*user), FileName: name, Mime: mimeType,
		TargetID: targetID, GroupID: groupID, Private: targetID != "", SentAt: time.Now().UTC(),
	}
	conversationID := ""
	if adminTarget {
		if !h.userListEnabled || !h.privateEnabled {
			h.mu.Unlock()
			return errors.New("管理员尚未开启私信功能")
		}
		if h.conversations[ip] == nil {
			h.conversations[ip] = &chatConversationState{
				id: ip, name: displayChatUser(*user),
				client: peer.client, updated: time.Now().UTC(),
			}
		}
		conversationID = adminConversationID(ip)
	} else if targetID != "" {
		if targetID == ip || !h.userListEnabled || !h.privateEnabled {
			h.mu.Unlock()
			return errors.New("私信功能不可用")
		}
		target := h.users[targetID]
		if target == nil || target.Blacklisted || !h.ipOnlineLocked(targetID) {
			h.mu.Unlock()
			return errors.New("私信用户已离线")
		}
		conversationID = directConversationID(ip, targetID)
	} else if groupID != "" {
		group := h.userGroups[groupID]
		if group == nil || !containsIP(group.Members, ip) {
			h.mu.Unlock()
			return errors.New("群聊不存在或你不在群内")
		}
		conversationID = chatUserGroupPrefix + groupID
	} else if h.groupEnabled {
		conversationID = ChatGroupConversationID
	} else {
		if _, err := h.ensurePeerConversationLocked(peer); err != nil {
			h.mu.Unlock()
			return err
		}
		conversationID = adminConversationID(ip)
	}
	h.mu.Unlock()

	stored, err := h.persistAttachment(conversationID, message, reader, maxBytes)
	if err != nil {
		return err
	}
	h.mu.Lock()
	valid := h.enabled && h.ipOnlineLocked(ip)
	if adminTarget {
		valid = valid && h.userListEnabled && h.privateEnabled
	} else if targetID != "" {
		valid = valid && h.userListEnabled && h.privateEnabled && h.ipOnlineLocked(targetID)
	} else if conversationID == ChatGroupConversationID {
		valid = valid && h.groupEnabled
	} else if groupID != "" {
		group := h.userGroups[groupID]
		valid = valid && group != nil && containsIP(group.Members, ip)
	} else {
		valid = valid && !h.groupEnabled
	}
	if !valid {
		h.mu.Unlock()
		_ = h.persistence.DeleteChatMessage(stored.ID)
		return errors.New("上传期间聊天状态已变化，请重试")
	}
	wire := wireFromMessage(stored)
	var slow []*chatPeer
	if adminTarget {
		conversation := h.conversations[ip]
		if conversation == nil {
			h.mu.Unlock()
			_ = h.persistence.DeleteChatMessage(stored.ID)
			return errors.New("管理员会话已移除")
		}
		appendChatMessage(conversation, stored)
		for _, recipient := range h.peersForIPLocked(ip) {
			if !recipient.enqueue(wire) {
				slow = append(slow, recipient)
			}
		}
	} else if targetID != "" {
		conversation := h.direct[conversationID]
		if conversation == nil {
			conversation = &chatConversationState{id: conversationID, name: "私信"}
			h.direct[conversationID] = conversation
		}
		appendChatMessage(conversation, stored)
		seen := make(map[*chatPeer]struct{})
		for _, recipient := range append(h.peersForIPLocked(ip), h.peersForIPLocked(targetID)...) {
			if _, duplicate := seen[recipient]; duplicate {
				continue
			}
			seen[recipient] = struct{}{}
			if !recipient.enqueue(wire) {
				slow = append(slow, recipient)
			}
		}
	} else if conversationID == ChatGroupConversationID {
		appendChatMessage(h.group, stored)
		wire.Group = true
		slow = h.broadcastLocked(wire)
	} else if groupID != "" {
		group := h.userGroups[groupID]
		if group == nil {
			h.mu.Unlock()
			_ = h.persistence.DeleteChatMessage(stored.ID)
			return errors.New("群聊已解散")
		}
		group.messages = append(group.messages, stored)
		group.historyBytes += chatMessageHistoryBytes(stored)
		group.UpdatedAt = stored.SentAt
		wire.GroupID = groupID
		for len(group.messages) > chatMaxHistory {
			group.historyBytes -= chatMessageHistoryBytes(group.messages[0])
			group.messages = group.messages[1:]
		}
		for _, recipient := range h.peersForGroupLocked(groupID) {
			if !recipient.enqueue(wire) {
				slow = append(slow, recipient)
			}
		}
	} else {
		conversation := h.conversations[ip]
		if conversation == nil {
			h.mu.Unlock()
			_ = h.persistence.DeleteChatMessage(stored.ID)
			return errors.New("聊天会话已移除")
		}
		appendChatMessage(conversation, stored)
		for _, recipient := range h.peersForIPLocked(ip) {
			if !recipient.enqueue(wire) {
				slow = append(slow, recipient)
			}
		}
	}
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	h.logIPOperation(ip, receivedChatOperation(stored))
	return nil
}

func (h *chatHub) receiveAdminAttachment(clientID string, message ChatMessage, reader io.Reader, maxBytes int64) error {
	h.mu.Lock()
	if !h.enabled {
		h.mu.Unlock()
		return errors.New("聊天功能未启用")
	}
	conversationID := ChatGroupConversationID
	targets := h.onlineIPsLocked()
	targetedPrivate := false
	if h.groupEnabled && strings.TrimSpace(clientID) != "" && strings.TrimSpace(clientID) != ChatGroupConversationID {
		clientID = normalizeChatIP(clientID)
		if clientID == "" {
			h.mu.Unlock()
			return errors.New("私信目标 IP 无效")
		}
		if !h.userListEnabled || !h.privateEnabled {
			h.mu.Unlock()
			return errors.New("管理员私信功能未启用")
		}
		target := h.users[clientID]
		if target == nil || target.Blacklisted || !h.ipOnlineLocked(clientID) {
			h.mu.Unlock()
			return errors.New("私信用户已离线")
		}
		if h.conversations[clientID] == nil {
			peers := h.peersForIPLocked(clientID)
			if len(peers) == 0 {
				h.mu.Unlock()
				return errors.New("私信用户已离线")
			}
			h.conversations[clientID] = &chatConversationState{
				id: clientID, name: displayChatUser(*target),
				client: peers[0].client, updated: time.Now().UTC(),
			}
		}
		message.Private = true
		message.TargetID = clientID
		conversationID, targets, targetedPrivate = adminConversationID(clientID), []string{clientID}, true
	} else if !h.groupEnabled {
		clientID = normalizeChatIP(clientID)
		if clientID == "" || !h.ipOnlineLocked(clientID) || h.conversations[clientID] == nil {
			h.mu.Unlock()
			return errors.New("访客已离线")
		}
		conversationID, targets = adminConversationID(clientID), []string{clientID}
	}
	h.mu.Unlock()
	stored, err := h.persistAttachment(conversationID, message, reader, maxBytes)
	if err != nil {
		return err
	}
	h.mu.Lock()
	valid := h.enabled
	if targetedPrivate {
		valid = valid && h.groupEnabled && h.userListEnabled && h.privateEnabled && h.ipOnlineLocked(clientID)
	} else if conversationID == ChatGroupConversationID {
		valid = valid && h.groupEnabled
	} else {
		valid = valid && !h.groupEnabled
	}
	if !valid {
		h.mu.Unlock()
		_ = h.persistence.DeleteChatMessage(stored.ID)
		return errors.New("发送期间聊天状态已变化，请重试")
	}
	wire := wireFromMessage(stored)
	var slow []*chatPeer
	if conversationID == ChatGroupConversationID {
		wire.Group = true
		appendChatMessage(h.group, stored)
		slow = h.broadcastLocked(wire)
	} else {
		conversation := h.conversations[clientID]
		if conversation == nil || !h.ipOnlineLocked(clientID) {
			h.mu.Unlock()
			_ = h.persistence.DeleteChatMessage(stored.ID)
			return errors.New("访客已离线")
		}
		appendChatMessage(conversation, stored)
		for _, recipient := range h.peersForIPLocked(clientID) {
			if !recipient.enqueue(wire) {
				slow = append(slow, recipient)
			}
		}
	}
	h.scheduleNotifyLocked()
	h.mu.Unlock()
	shutdownSlowChatPeers(slow)
	for _, ip := range targets {
		h.logIPOperation(ip, adminChatOperation(stored))
	}
	return nil
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
	if user := h.users[peer.ip]; user != nil {
		if h.userListEnabled && strings.TrimSpace(user.Name) == "" {
			h.mu.Unlock()
			return errors.New("请先设置名称")
		}
		message.Name = displayChatUser(*user)
	} else {
		message.Name = peer.ip
	}
	if h.groupEnabled {
		stored, err := h.persistMessageLocked(ChatGroupConversationID, message)
		if err != nil {
			h.mu.Unlock()
			return fmt.Errorf("保存聊天记录失败：%w", err)
		}
		wire := wireFromMessage(stored)
		wire.Group = true
		appendChatMessage(h.group, stored)
		slow := h.broadcastLocked(wire)
		h.scheduleNotifyLocked()
		h.mu.Unlock()
		shutdownSlowChatPeers(slow)
		h.logIPOperation(peer.ip, receivedChatOperation(message))
		return nil
	}
	stored, err := h.persistMessageLocked(adminConversationID(peer.ip), message)
	if err != nil {
		h.mu.Unlock()
		return fmt.Errorf("保存聊天记录失败：%w", err)
	}
	wire := wireFromMessage(stored)
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
	appendChatMessage(conversation, stored)
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
	if message.Kind == ChatMessageKindFile {
		return "接收聊天文件"
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
	if user := h.users[peer.ip]; user != nil {
		conversation.name = displayChatUser(*user)
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
		Type:       "message",
		ClientID:   message.ClientID,
		Name:       message.Name,
		ID:         message.ID,
		Kind:       kind,
		Sender:     message.Sender,
		Text:       message.Text,
		Mime:       message.Mime,
		Data:       message.Data,
		FileName:   message.FileName,
		FileSize:   message.FileSize,
		FileURL:    message.FileURL,
		TargetID:   message.TargetID,
		GroupID:    message.GroupID,
		Private:    message.Private,
		Receipt:    message.Receipt,
		Read:       message.Read,
		ReadAt:     message.ReadAt,
		Recalled:   message.Recalled,
		RecalledAt: message.RecalledAt,
		SentAt:     message.SentAt,
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

func cleanLargeChatImage(rawMime string, data []byte) (string, []byte, error) {
	mimeType := strings.ToLower(strings.TrimSpace(rawMime))
	expected := ""
	switch mimeType {
	case "image/png":
		expected = "png"
	case "image/jpeg":
		expected = "jpeg"
	default:
		return "", nil, errors.New("只支持 PNG 或 JPEG 图片")
	}
	if len(data) == 0 {
		return "", nil, errors.New("图片不能为空")
	}
	if len(data) > chatMaxUploadImageBytes {
		return "", nil, errors.New("图片不能超过 100 MiB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != expected || config.Width <= 0 || config.Height <= 0 {
		return "", nil, errors.New("图片格式无效")
	}
	return mimeType, append([]byte(nil), data...), nil
}

func cleanChatFile(rawName, rawMime string, data []byte) (string, string, []byte, error) {
	name := strings.TrimSpace(rawName)
	name = strings.ReplaceAll(strings.ReplaceAll(name, "\\", "/"), "\x00", "")
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" {
		return "", "", nil, errors.New("文件名无效")
	}
	runes := []rune(name)
	if len(runes) > 120 {
		name = string(runes[:120])
	}
	if len(data) == 0 {
		return "", "", nil, errors.New("文件不能为空")
	}
	if len(data) > chatMaxFileBytes {
		return "", "", nil, fmt.Errorf("聊天文件不能超过 %d MiB", chatMaxFileBytes>>20)
	}
	mime := strings.TrimSpace(rawMime)
	if len(mime) > 128 || strings.ContainsAny(mime, "\r\n\x00") {
		mime = ""
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return name, mime, append([]byte(nil), data...), nil
}

func cleanChatAttachmentMetadata(rawName, rawMime string) (string, string, error) {
	name := strings.TrimSpace(rawName)
	name = strings.ReplaceAll(strings.ReplaceAll(name, "\\", "/"), "\x00", "")
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" {
		return "", "", errors.New("文件名无效")
	}
	runes := []rune(name)
	if len(runes) > 120 {
		name = string(runes[:120])
	}
	mimeType := strings.TrimSpace(rawMime)
	if len(mimeType) > 128 || strings.ContainsAny(mimeType, "\r\n\x00") {
		mimeType = ""
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return name, mimeType, nil
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

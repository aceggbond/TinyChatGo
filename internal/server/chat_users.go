package server

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mozillazg/go-pinyin"
)

const (
	adminConversationPrefix  = "admin:"
	directConversationPrefix = "direct:"
	chatMaxNameRunes         = 32
)

// SetPersistence attaches the application's durable store and restores users
// plus the most recent messages for every conversation.
func (s *Server) SetPersistence(persistence Persistence) error {
	if persistence == nil {
		s.chat.mu.Lock()
		s.chat.persistence = nil
		s.chat.mu.Unlock()
		s.mu.Lock()
		s.persistence = nil
		s.mu.Unlock()
		return nil
	}
	users, messages, err := persistence.LoadChatState()
	if err != nil {
		return err
	}
	s.chat.mu.Lock()
	s.chat.persistence = persistence
	s.chat.users = make(map[string]*ChatUser)
	s.chat.conversations = make(map[string]*chatConversationState)
	s.chat.direct = make(map[string]*chatConversationState)
	s.chat.group.messages = nil
	s.chat.group.historyBytes = 0
	for index := range users {
		user := users[index]
		user.IP = normalizeChatIP(user.IP)
		if user.IP == "" {
			continue
		}
		copy := user
		s.chat.users[user.IP] = &copy
		s.chat.conversations[user.IP] = &chatConversationState{
			id:      user.IP,
			name:    displayChatUser(user),
			client:  user.Client,
			updated: user.LastSeen,
		}
	}
	for _, stored := range messages {
		message := stored.Message
		if message.AttachmentPath != "" {
			message.FileURL = chatAttachmentURL(message)
			message.Data = nil
		}
		var conversation *chatConversationState
		switch {
		case stored.ConversationID == ChatGroupConversationID:
			conversation = s.chat.group
		case strings.HasPrefix(stored.ConversationID, adminConversationPrefix):
			ip := normalizeChatIP(strings.TrimPrefix(stored.ConversationID, adminConversationPrefix))
			if ip == "" {
				continue
			}
			conversation = s.chat.conversations[ip]
			if conversation == nil {
				conversation = &chatConversationState{id: ip, name: ip}
				s.chat.conversations[ip] = conversation
			}
		case strings.HasPrefix(stored.ConversationID, directConversationPrefix):
			conversation = s.chat.direct[stored.ConversationID]
			if conversation == nil {
				conversation = &chatConversationState{id: stored.ConversationID, name: "私信"}
				s.chat.direct[stored.ConversationID] = conversation
			}
		default:
			continue
		}
		appendChatMessage(conversation, message)
	}
	s.chat.mu.Unlock()
	s.mu.Lock()
	s.persistence = persistence
	s.mu.Unlock()
	return nil
}

func (s *Server) Persistence() Persistence {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.persistence
}

func (s *Server) SetUserListEnabled(enabled bool) {
	s.chat.mu.Lock()
	if s.chat.userListEnabled == enabled {
		s.chat.mu.Unlock()
		return
	}
	s.chat.userListEnabled = enabled
	if !enabled {
		s.chat.privateEnabled = false
	}
	slow := s.chat.broadcastUserListLocked()
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	shutdownSlowChatPeers(slow)
}

func (s *Server) UserListEnabled() bool {
	s.chat.mu.RLock()
	defer s.chat.mu.RUnlock()
	return s.chat.userListEnabled
}

func (s *Server) SetPrivateMessagesEnabled(enabled bool) {
	s.chat.mu.Lock()
	enabled = enabled && s.chat.userListEnabled
	if s.chat.privateEnabled == enabled {
		s.chat.mu.Unlock()
		return
	}
	s.chat.privateEnabled = enabled
	slow := s.chat.broadcastUserListLocked()
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	shutdownSlowChatPeers(slow)
}

func (s *Server) PrivateMessagesEnabled() bool {
	s.chat.mu.RLock()
	defer s.chat.mu.RUnlock()
	return s.chat.privateEnabled
}

func (s *Server) ChatUsers() []ChatUser {
	s.chat.mu.RLock()
	users := make([]ChatUser, 0, len(s.chat.users))
	for _, user := range s.chat.users {
		copy := *user
		users = append(users, copy)
	}
	s.chat.mu.RUnlock()
	sort.Slice(users, func(i, j int) bool {
		onlineI, onlineJ := s.chatIPOnline(users[i].IP), s.chatIPOnline(users[j].IP)
		if onlineI != onlineJ {
			return onlineI
		}
		return users[i].LastSeen.After(users[j].LastSeen)
	})
	return users
}

func (s *Server) chatIPOnline(ip string) bool {
	s.chat.mu.RLock()
	defer s.chat.mu.RUnlock()
	return s.chat.ipOnlineLocked(ip)
}

func (s *Server) SetChatUserName(ip, name string) error {
	ip = normalizeChatIP(ip)
	if ip == "" {
		return errors.New("用户 IP 无效")
	}
	name, err := cleanChatName(name)
	if err != nil {
		return err
	}
	s.chat.mu.Lock()
	user := s.chat.ensureUserLocked(ip, ChatClientInfo{IP: ip})
	user.Name = name
	if conversation := s.chat.conversations[ip]; conversation != nil {
		conversation.name = displayChatUser(*user)
	}
	persistence := s.chat.persistence
	copy := *user
	slow := s.chat.broadcastUserListLocked()
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	if persistence != nil {
		if err = persistence.SaveChatUser(copy); err != nil {
			return err
		}
	}
	shutdownSlowChatPeers(slow)
	return nil
}

func (s *Server) SetChatUserBlacklisted(ip string, blacklisted bool) error {
	ip = normalizeChatIP(ip)
	if ip == "" {
		return errors.New("用户 IP 无效")
	}
	s.chat.mu.Lock()
	user := s.chat.ensureUserLocked(ip, ChatClientInfo{IP: ip})
	user.Blacklisted = blacklisted
	copy := *user
	persistence := s.chat.persistence
	var detached []*chatPeer
	if blacklisted {
		for id, peer := range s.chat.peers {
			if peer.ip == ip {
				detached = append(detached, peer)
				delete(s.chat.peers, id)
			}
		}
	}
	slow := s.chat.broadcastUserListLocked()
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	if persistence != nil {
		if err := persistence.SaveChatUser(copy); err != nil {
			return err
		}
	}
	shutdownSlowChatPeers(slow)
	for _, peer := range detached {
		peer.shutdown(websocketClosePolicyViolation, "blacklisted")
	}
	return nil
}

func (s *Server) ChatUserBlacklisted(ip string) bool {
	ip = normalizeChatIP(ip)
	if ip == "" {
		return false
	}
	s.chat.mu.RLock()
	user := s.chat.users[ip]
	blocked := user != nil && user.Blacklisted
	s.chat.mu.RUnlock()
	return blocked
}

// ObserveChatUser records a successful page visit even when chat is disabled.
func (s *Server) ObserveChatUser(client ChatClientInfo) bool {
	client.IP = normalizeChatIP(client.IP)
	if client.IP == "" {
		return false
	}
	s.chat.mu.Lock()
	_, existed := s.chat.users[client.IP]
	user := s.chat.ensureUserLocked(client.IP, client)
	copy := *user
	if conversation := s.chat.conversations[client.IP]; conversation != nil {
		conversation.client = client
		conversation.name = displayChatUser(copy)
	} else {
		s.chat.conversations[client.IP] = &chatConversationState{
			id:      client.IP,
			name:    displayChatUser(copy),
			client:  client,
			updated: copy.LastSeen,
		}
	}
	persistence := s.chat.persistence
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	if persistence != nil {
		_ = persistence.SaveChatUser(copy)
	}
	return !existed
}

func (s *Server) serveChatAttachment(w http.ResponseWriter, r *http.Request, clientIP string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	persistence := s.Persistence()
	if persistence == nil {
		http.NotFound(w, r)
		return
	}
	clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/__hfs/chat/file/"))
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) < 1 || strings.TrimSpace(parts[0]) == "" {
		http.NotFound(w, r)
		return
	}
	messageID, err := url.PathUnescape(parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	attachment, err := persistence.OpenChatAttachment(messageID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "无法读取聊天附件", http.StatusInternalServerError)
		}
		return
	}
	defer attachment.Reader.Close()
	if !chatConversationVisibleToIP(attachment.ConversationID, clientIP) {
		http.Error(w, "无权访问此聊天附件", http.StatusForbidden)
		return
	}
	contentType := strings.TrimSpace(attachment.MIME)
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(attachment.Name))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	disposition := "attachment"
	if strings.HasPrefix(contentType, "image/") {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(attachment.Size, 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename*=UTF-8''%s", disposition, url.PathEscape(attachment.Name)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, attachment.Reader)
}

func chatConversationVisibleToIP(conversationID, ip string) bool {
	ip = normalizeChatIP(ip)
	switch {
	case ip == "":
		return false
	case conversationID == ChatGroupConversationID:
		return true
	case strings.HasPrefix(conversationID, adminConversationPrefix):
		return normalizeChatIP(strings.TrimPrefix(conversationID, adminConversationPrefix)) == ip
	case strings.HasPrefix(conversationID, directConversationPrefix):
		first, second, ok := parseDirectConversationID(conversationID)
		return ok && (first == ip || second == ip)
	default:
		return false
	}
}

func (s *Server) ClearChatHistory() error {
	s.chat.mu.Lock()
	for _, conversation := range s.chat.conversations {
		conversation.messages = nil
		conversation.historyBytes = 0
	}
	s.chat.group.messages = nil
	s.chat.group.historyBytes = 0
	s.chat.direct = make(map[string]*chatConversationState)
	persistence := s.chat.persistence
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	if persistence != nil {
		return persistence.ClearChatMessages()
	}
	return nil
}

func (s *Server) ClearChatUsers() error {
	s.chat.mu.Lock()
	persistence := s.chat.persistence
	now := time.Now().UTC()
	online := make(map[string]ChatClientInfo)
	for _, peer := range s.chat.peers {
		online[peer.ip] = peer.client
	}
	s.chat.users = make(map[string]*ChatUser)
	for ip, conversation := range s.chat.conversations {
		conversation.name = ip
		conversation.client = ChatClientInfo{IP: ip}
	}
	for ip, client := range online {
		user := &ChatUser{IP: ip, FirstSeen: now, LastSeen: now, Client: client}
		s.chat.users[ip] = user
		if conversation := s.chat.conversations[ip]; conversation != nil {
			conversation.name = ip
			conversation.client = client
		}
	}
	slow := s.chat.broadcastUserListLocked()
	s.chat.scheduleNotifyLocked()
	s.chat.mu.Unlock()
	if persistence != nil {
		if err := persistence.ClearChatUsers(); err != nil {
			return err
		}
		for _, user := range s.ChatUsers() {
			if err := persistence.SaveChatUser(user); err != nil {
				return err
			}
		}
	}
	shutdownSlowChatPeers(slow)
	return nil
}

func (h *chatHub) ensureUserLocked(ip string, client ChatClientInfo) *ChatUser {
	now := time.Now().UTC()
	user := h.users[ip]
	if user == nil {
		user = &ChatUser{IP: ip, FirstSeen: now}
		h.users[ip] = user
	}
	user.LastSeen = now
	if client.IP != "" {
		user.Client = client
	}
	if user.Client.IP == "" {
		user.Client.IP = ip
	}
	return user
}

func (h *chatHub) publicUsersLocked(currentIP string) []ChatPublicUser {
	if !h.userListEnabled {
		user := h.users[currentIP]
		if user == nil || user.Blacklisted {
			return nil
		}
		return []ChatPublicUser{{
			IP: currentIP, Name: displayChatUser(*user), Alias: user.Name,
			SearchKey: chatUserSearchKey(currentIP, user.Name), Online: true, Me: true,
		}}
	}
	online := make(map[string]bool)
	for _, peer := range h.peers {
		online[peer.ip] = true
	}
	items := make([]ChatPublicUser, 0, len(online))
	for ip := range online {
		user := h.users[ip]
		if user != nil && user.Blacklisted {
			continue
		}
		name := ip
		if user != nil {
			name = displayChatUser(*user)
		}
		alias := ""
		if user != nil {
			alias = user.Name
		}
		items = append(items, ChatPublicUser{
			IP: ip, Name: name, Alias: alias, SearchKey: chatUserSearchKey(ip, alias),
			Online: true, Me: ip == currentIP,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Me != items[j].Me {
			return items[i].Me
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items
}

func chatUserSearchKey(ip, alias string) string {
	parts := []string{strings.ToLower(ip), strings.ToLower(strings.TrimSpace(alias))}
	syllables := pinyin.LazyPinyin(alias, pinyin.NewArgs())
	if len(syllables) > 0 {
		parts = append(parts, strings.Join(syllables, ""), strings.Join(syllables, " "))
		initials := strings.Builder{}
		for _, syllable := range syllables {
			if syllable != "" {
				initials.WriteByte(strings.ToLower(syllable)[0])
			}
		}
		parts = append(parts, initials.String())
	}
	return strings.Join(parts, " ")
}

// ChatUserSearchKey returns the same IP/name/pinyin search text used by the
// browser user list so the native administrator UI can match users identically.
func ChatUserSearchKey(ip, alias string) string {
	return chatUserSearchKey(ip, alias)
}

func (h *chatHub) broadcastUserListLocked() []*chatPeer {
	if !h.userListEnabled {
		return nil
	}
	var slow []*chatPeer
	for _, peer := range h.peers {
		message := chatWireMessage{
			Type:            "users",
			Users:           h.publicUsersLocked(peer.ip),
			UserListEnabled: h.userListEnabled,
			PrivateEnabled:  h.privateEnabled,
		}
		if !peer.enqueue(message) {
			slow = append(slow, peer)
		}
	}
	return slow
}

func (h *chatHub) directHistoryForPeerLocked(ip string) map[string][]ChatMessage {
	if !h.privateEnabled {
		return nil
	}
	history := make(map[string][]ChatMessage)
	if h.groupEnabled {
		if conversation := h.conversations[ip]; conversation != nil && len(conversation.messages) > 0 {
			history[ChatAdminConversationID] = copyChatMessages(conversation.messages, true)
		}
	}
	for id, conversation := range h.direct {
		first, second, ok := parseDirectConversationID(id)
		if !ok || first != ip && second != ip {
			continue
		}
		other := first
		if other == ip {
			other = second
		}
		history[other] = copyChatMessages(conversation.messages, true)
	}
	if len(history) == 0 {
		return nil
	}
	return history
}

func (h *chatHub) persistUserLocked(user *ChatUser) {
	if h.persistence == nil || user == nil {
		return
	}
	copy := *user
	_ = h.persistence.SaveChatUser(copy)
}

func displayChatUser(user ChatUser) string {
	if strings.TrimSpace(user.Name) == "" {
		return user.IP
	}
	return user.IP + "-" + user.Name
}

func cleanChatName(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if utf8.RuneCountInString(raw) > chatMaxNameRunes {
		return "", fmt.Errorf("名字不能超过 %d 个字符", chatMaxNameRunes)
	}
	for _, value := range raw {
		if value == 0 || unicode.IsControl(value) {
			return "", errors.New("名字包含无效控制字符")
		}
	}
	return raw, nil
}

func adminConversationID(ip string) string {
	return adminConversationPrefix + normalizeChatIP(ip)
}

func directConversationID(first, second string) string {
	first, second = normalizeChatIP(first), normalizeChatIP(second)
	if first == "" || second == "" || first == second {
		return ""
	}
	if first > second {
		first, second = second, first
	}
	return directConversationPrefix + first + "|" + second
}

func parseDirectConversationID(id string) (string, string, bool) {
	if !strings.HasPrefix(id, directConversationPrefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(id, directConversationPrefix), "|")
	if len(parts) != 2 {
		return "", "", false
	}
	first, second := normalizeChatIP(parts[0]), normalizeChatIP(parts[1])
	return first, second, first != "" && second != "" && first != second
}

func chatAttachmentURL(message ChatMessage) string {
	if message.ID == "" {
		return ""
	}
	name := message.FileName
	if name == "" {
		name = "attachment"
	}
	return "/__hfs/chat/file/" + url.PathEscape(message.ID) + "/" + url.PathEscape(name)
}

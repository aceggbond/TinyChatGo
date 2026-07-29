package server

import (
	"encoding/json"
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

func normalizeGroupMembers(members []string) []string {
	seen := make(map[string]struct{}, len(members))
	result := make([]string, 0, len(members))
	for _, member := range members {
		member = normalizeChatIP(member)
		if member == "" {
			continue
		}
		if _, ok := seen[member]; ok {
			continue
		}
		seen[member] = struct{}{}
		result = append(result, member)
	}
	sort.Strings(result)
	return result
}

func containsIP(members []string, ip string) bool {
	ip = normalizeChatIP(ip)
	for _, member := range members {
		if normalizeChatIP(member) == ip {
			return true
		}
	}
	return false
}

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
	remarks, err := persistence.LoadChatRemarks()
	if err != nil {
		return err
	}
	var groups []ChatGroup
	if groupStore, ok := persistence.(ChatGroupPersistence); ok {
		groups, err = groupStore.LoadChatGroups()
		if err != nil {
			return err
		}
	}
	s.chat.mu.Lock()
	s.chat.persistence = persistence
	s.chat.users = make(map[string]*ChatUser)
	s.chat.conversations = make(map[string]*chatConversationState)
	s.chat.direct = make(map[string]*chatConversationState)
	s.chat.userGroups = make(map[string]*chatUserGroupState)
	s.chat.remarks = make(map[string]map[string]string)
	s.chat.group.messages = nil
	s.chat.group.historyBytes = 0
	for _, group := range s.chat.userGroups {
		group.messages = nil
		group.historyBytes = 0
	}
	for _, group := range groups {
		group.ID = strings.TrimSpace(group.ID)
		group.Name = strings.TrimSpace(group.Name)
		group.OwnerIP = normalizeChatIP(group.OwnerIP)
		group.Members = normalizeGroupMembers(group.Members)
		if group.ID == "" || group.Name == "" || group.OwnerIP == "" || !containsIP(group.Members, group.OwnerIP) {
			continue
		}
		s.chat.userGroups[group.ID] = &chatUserGroupState{ChatGroup: group}
	}
	for index := range users {
		user := users[index]
		user.IP = normalizeChatIP(user.IP)
		if user.IP == "" {
			continue
		}
		if avatar, avatarErr := cleanChatAvatar(user.Avatar); avatarErr == nil {
			user.Avatar = avatar
		} else {
			user.Avatar = ""
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
	for _, remark := range remarks {
		owner := normalizeChatIP(remark.OwnerIP)
		target := normalizeChatIP(remark.TargetIP)
		if owner == "" || target == "" || owner == target || strings.TrimSpace(remark.Name) == "" {
			continue
		}
		if s.chat.remarks[owner] == nil {
			s.chat.remarks[owner] = make(map[string]string)
		}
		s.chat.remarks[owner][target] = remark.Name
	}
	for _, stored := range messages {
		message := stored.Message
		if strings.HasPrefix(stored.ConversationID, directConversationPrefix) {
			if message.Receipt && !message.ReadAt.IsZero() {
				message.Read = true
			}
		} else {
			message.Receipt = false
			message.Read = false
			message.ReadAt = time.Time{}
		}
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
		case strings.HasPrefix(stored.ConversationID, chatUserGroupPrefix):
			groupID := strings.TrimPrefix(stored.ConversationID, chatUserGroupPrefix)
			group := s.chat.userGroups[groupID]
			if group == nil {
				continue
			}
			group.messages = append(group.messages, message)
			group.historyBytes += chatMessageHistoryBytes(message)
			continue
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

func (h *chatHub) updatePeerRemark(peer *chatPeer, targetIP, name string) error {
	targetIP = normalizeChatIP(targetIP)
	if targetIP == "" || targetIP == peer.ip {
		return errors.New("备注目标无效")
	}
	h.mu.Lock()
	if !h.enabled || h.peers[peer.id] != peer {
		h.mu.Unlock()
		return errors.New("聊天连接已关闭")
	}
	if h.remarks[peer.ip] == nil {
		h.remarks[peer.ip] = make(map[string]string)
	}
	if name == "" {
		delete(h.remarks[peer.ip], targetIP)
	} else {
		h.remarks[peer.ip][targetIP] = name
	}
	persistence := h.persistence
	remarks := h.remarksForPeerLocked(peer.ip)
	users := h.publicUsersLocked(peer.ip)
	recipients := append([]*chatPeer(nil), h.peersForIPLocked(peer.ip)...)
	userListEnabled, privateEnabled := h.userListEnabled, h.privateEnabled
	h.mu.Unlock()
	var err error
	if persistence != nil {
		if name == "" {
			err = persistence.DeleteChatRemark(peer.ip, targetIP)
		} else {
			err = persistence.SaveChatRemark(ChatRemark{
				OwnerIP: peer.ip, TargetIP: targetIP, Name: name, Updated: time.Now().UTC(),
			})
		}
	}
	if err != nil {
		return fmt.Errorf("保存备注失败：%w", err)
	}
	update := chatWireMessage{
		Type: "remarks", Remarks: remarks, Users: users,
		UserListEnabled: userListEnabled, PrivateEnabled: privateEnabled,
	}
	for _, recipient := range recipients {
		recipient.enqueue(update)
	}
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
	if !s.chat.conversationVisibleToIP(attachment.ConversationID, clientIP) {
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

func (s *Server) serveChatArchive(w http.ResponseWriter, r *http.Request, clientIP string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	persistence := s.Persistence()
	if persistence == nil {
		http.Error(w, "聊天归档尚未初始化", http.StatusServiceUnavailable)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind != "" && kind != ChatMessageKindText && kind != ChatMessageKindImage && kind != ChatMessageKindFile {
		http.Error(w, "归档类型无效", http.StatusBadRequest)
		return
	}
	conversationID := ""
	groupMembers := make(map[string][]string)
	if r.URL.Query().Get("scope") == "all" {
		s.chat.mu.RLock()
		for id, group := range s.chat.userGroups {
			if containsIP(group.Members, clientIP) {
				groupMembers[id] = append([]string(nil), group.Members...)
			}
		}
		s.chat.mu.RUnlock()
	} else {
		groupID := strings.TrimSpace(r.URL.Query().Get("groupId"))
		if groupID != "" {
			s.chat.mu.RLock()
			group := s.chat.userGroups[groupID]
			if group != nil && containsIP(group.Members, clientIP) {
				conversationID = chatUserGroupPrefix + groupID
				groupMembers[groupID] = append([]string(nil), group.Members...)
			}
			s.chat.mu.RUnlock()
			if conversationID == "" {
				http.Error(w, "群聊不存在或你不在群内", http.StatusForbidden)
				return
			}
		}
		target := normalizeChatIP(r.URL.Query().Get("targetId"))
		if conversationID != "" {
			// A group archive route has already been resolved above.
		} else if target == "" {
			conversationID = ChatGroupConversationID
		} else {
			conversationID = directConversationID(clientIP, target)
			if conversationID == "" {
				http.Error(w, "会话目标无效", http.StatusBadRequest)
				return
			}
		}
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
	if kind == ChatMessageKindText {
		history, ok := persistence.(ChatHistoryPersistence)
		if !ok {
			http.Error(w, "聊天记录搜索尚未初始化", http.StatusServiceUnavailable)
			return
		}
		result, err := history.ListChatMessages(ChatHistoryQuery{
			ViewerIP: clientIP, ConversationID: conversationID,
			GroupMembers: groupMembers,
			Query:        r.URL.Query().Get("q"),
			From:         parseDate(r.URL.Query().Get("from"), false),
			To:           parseDate(r.URL.Query().Get("to"), true),
			Page:         page, PageSize: pageSize,
		})
		if err != nil {
			http.Error(w, "读取聊天记录失败", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
		return
	}
	result, err := persistence.ListChatAttachments(ChatArchiveQuery{
		ViewerIP: clientIP, ConversationID: conversationID,
		GroupMembers: groupMembers,
		Query:        r.URL.Query().Get("q"), Kind: kind,
		From: parseDate(r.URL.Query().Get("from"), false),
		To:   parseDate(r.URL.Query().Get("to"), true),
		Page: page, PageSize: pageSize,
	})
	if err != nil {
		http.Error(w, "读取聊天归档失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(result)
}

func (h *chatHub) conversationVisibleToIP(conversationID, ip string) bool {
	if chatConversationVisibleToIP(conversationID, ip) {
		return true
	}
	if !strings.HasPrefix(conversationID, chatUserGroupPrefix) {
		return false
	}
	h.mu.RLock()
	group := h.userGroups[strings.TrimPrefix(conversationID, chatUserGroupPrefix)]
	visible := group != nil && containsIP(group.Members, ip)
	h.mu.RUnlock()
	return visible
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
	case strings.HasPrefix(conversationID, chatUserGroupPrefix):
		// Membership is checked by chatHub.conversationVisibleToIP where the
		// live group registry is available.
		return false
	default:
		return false
	}
}

// ChatConversationVisibleToIP exposes the attachment authorization rule to
// the database archive index without exposing any filesystem paths.
func ChatConversationVisibleToIP(conversationID, ip string) bool {
	return chatConversationVisibleToIP(conversationID, ip)
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

// DeleteChatMessage removes one persisted message and its attachment. It is a
// desktop-management action; browser participants cannot call it.
func (s *Server) DeleteChatMessage(messageID string) error {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return errors.New("消息 ID 无效")
	}
	s.chat.mu.Lock()
	removed := false
	remove := func(conversation *chatConversationState) {
		if conversation == nil {
			return
		}
		for index := range conversation.messages {
			if conversation.messages[index].ID != messageID {
				continue
			}
			conversation.historyBytes -= chatMessageHistoryBytes(conversation.messages[index])
			conversation.messages[index] = ChatMessage{}
			conversation.messages = append(conversation.messages[:index], conversation.messages[index+1:]...)
			removed = true
			return
		}
	}
	remove(s.chat.group)
	for _, group := range s.chat.userGroups {
		for index := range group.messages {
			if group.messages[index].ID != messageID {
				continue
			}
			group.historyBytes -= chatMessageHistoryBytes(group.messages[index])
			group.messages = append(group.messages[:index], group.messages[index+1:]...)
			removed = true
			break
		}
	}
	for _, conversation := range s.chat.conversations {
		remove(conversation)
	}
	for _, conversation := range s.chat.direct {
		remove(conversation)
	}
	persistence := s.chat.persistence
	if removed {
		s.chat.scheduleNotifyLocked()
	}
	s.chat.mu.Unlock()
	if persistence == nil {
		if !removed {
			return os.ErrNotExist
		}
		return nil
	}
	return persistence.DeleteChatMessage(messageID)
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
	s.chat.remarks = make(map[string]map[string]string)
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
		if err := persistence.ClearChatRemarks(); err != nil {
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
			IP: currentIP, Name: displayChatUser(*user), Alias: user.Name, Avatar: user.Avatar,
			Port: user.Client.Port, Browser: user.Client.Browser, OS: user.Client.OS, ClientType: user.Client.ClientType,
			SearchKey: chatUserSearchKey(currentIP, user.Name), Online: true, Me: true,
		}}
	}
	online := make(map[string]bool)
	relevant := make(map[string]bool)
	for _, peer := range h.peers {
		online[peer.ip] = true
		relevant[peer.ip] = true
	}
	relevant[currentIP] = true
	for id := range h.direct {
		first, second, ok := parseDirectConversationID(id)
		if !ok || first != currentIP && second != currentIP {
			continue
		}
		relevant[first] = true
		relevant[second] = true
	}
	for _, group := range h.userGroups {
		if group == nil || !containsIP(group.Members, currentIP) {
			continue
		}
		for _, member := range group.Members {
			relevant[normalizeChatIP(member)] = true
		}
	}
	for ip := range h.remarks[currentIP] {
		relevant[normalizeChatIP(ip)] = true
	}
	items := make([]ChatPublicUser, 0, len(relevant))
	for ip := range relevant {
		user := h.users[ip]
		if user != nil && user.Blacklisted {
			continue
		}
		name := ip
		if user != nil {
			name = displayChatUser(*user)
		}
		alias := ""
		avatar := ""
		if user != nil {
			alias = user.Name
			avatar = user.Avatar
		}
		remark := h.remarks[currentIP][ip]
		client := ChatClientInfo{IP: ip}
		if user != nil {
			client = user.Client
		}
		items = append(items, ChatPublicUser{
			IP: ip, Port: client.Port, Name: name, Alias: alias, Avatar: avatar, Remark: remark,
			SearchKey:  chatUserSearchKey(ip, strings.TrimSpace(alias+" "+remark)),
			Browser:    client.Browser,
			OS:         client.OS,
			ClientType: client.ClientType,
			Online:     online[ip],
			Me:         ip == currentIP,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Me != items[j].Me {
			return items[i].Me
		}
		if items[i].Online != items[j].Online {
			return items[i].Online
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items
}

func (h *chatHub) publicAvatarsLocked(currentIP string) map[string]string {
	currentIP = normalizeChatIP(currentIP)
	relevant := make(map[string]struct{})
	addMessageSenders := func(messages []ChatMessage) {
		for _, message := range messages {
			if ip := normalizeChatIP(message.ClientID); ip != "" {
				relevant[ip] = struct{}{}
			}
		}
	}
	if conversation := h.conversations[currentIP]; conversation != nil {
		addMessageSenders(conversation.messages)
	}
	if h.groupEnabled && h.group != nil {
		addMessageSenders(h.group.messages)
	}
	for id, conversation := range h.direct {
		first, second, ok := parseDirectConversationID(id)
		if !ok || first != currentIP && second != currentIP {
			continue
		}
		relevant[first] = struct{}{}
		relevant[second] = struct{}{}
		addMessageSenders(conversation.messages)
	}
	for _, group := range h.userGroups {
		if group == nil || !containsIP(group.Members, currentIP) {
			continue
		}
		for _, member := range group.Members {
			relevant[normalizeChatIP(member)] = struct{}{}
		}
		addMessageSenders(group.messages)
	}
	items := make(map[string]string)
	for ip := range relevant {
		if ip == "" || ip == currentIP || h.userListEnabled && h.ipOnlineLocked(ip) {
			continue
		}
		user := h.users[ip]
		if user == nil || user.Blacklisted || strings.TrimSpace(user.Avatar) == "" {
			continue
		}
		items[ip] = user.Avatar
	}
	if len(items) == 0 {
		return nil
	}
	return items
}

func (h *chatHub) remarksForPeerLocked(ip string) map[string]string {
	source := h.remarks[normalizeChatIP(ip)]
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for target, name := range source {
		result[target] = name
	}
	return result
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
			Type:               "users",
			Users:              h.publicUsersLocked(peer.ip),
			UserListEnabled:    h.userListEnabled,
			PrivateEnabled:     h.privateEnabled,
			GroupCreateEnabled: h.groupCreate,
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

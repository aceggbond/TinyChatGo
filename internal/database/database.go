package database

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"lanchatgo/internal/server"
)

var (
	bucketApp         = []byte("app")
	bucketUsers       = []byte("users")
	bucketRemarks     = []byte("remarks")
	bucketMessages    = []byte("messages")
	bucketAttachments = []byte("attachments")
	bucketAccess      = []byte("access")

	keySettings = []byte("settings")
	keyShares   = []byte("shares")
	keyHTTPS    = []byte("https-certificate")
	keyGroups   = []byte("chat-groups")
)

const loadedMessagesPerConversation = 100

// DB owns the application's single durable database and chat attachment tree.
type DB struct {
	db             *bolt.DB
	path           string
	attachmentRoot string
}

// CertificateBundle keeps the local CA and current HTTPS leaf certificate in
// the single application database. Saving a new bundle atomically replaces the
// previous one.
type CertificateBundle struct {
	CACertPEM []byte    `json:"caCertPEM"`
	CAKeyPEM  []byte    `json:"caKeyPEM"`
	CertPEM   []byte    `json:"certPEM"`
	KeyPEM    []byte    `json:"keyPEM"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Open creates the database and all required buckets when they do not exist.
func Open(path string) (*DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	raw, err := bolt.Open(absolute, 0600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	store := &DB{
		db:             raw,
		path:           absolute,
		attachmentRoot: filepath.Join(filepath.Dir(absolute), "chat_files"),
	}
	if err = raw.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketApp, bucketUsers, bucketRemarks, bucketMessages, bucketAttachments, bucketAccess} {
			if _, createErr := tx.CreateBucketIfNotExists(name); createErr != nil {
				return createErr
			}
		}
		return nil
	}); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}
	if err = os.MkdirAll(store.attachmentRoot, 0700); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("create chat attachment directory: %w", err)
	}
	return store, nil
}

func (d *DB) Path() string { return d.path }

func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	return d.db.Close()
}

func (d *DB) LoadSettings(value any) (bool, error) {
	return d.loadAppJSON(keySettings, value)
}

func (d *DB) SaveSettings(value any) error {
	return d.saveAppJSON(keySettings, value)
}

func (d *DB) LoadCertificateBundle() (CertificateBundle, bool, error) {
	var bundle CertificateBundle
	ok, err := d.loadAppJSON(keyHTTPS, &bundle)
	if err != nil || !ok {
		return CertificateBundle{}, ok, err
	}
	if len(bundle.CACertPEM) == 0 || len(bundle.CAKeyPEM) == 0 || len(bundle.CertPEM) == 0 || len(bundle.KeyPEM) == 0 {
		return CertificateBundle{}, false, errors.New("HTTPS certificate data is incomplete")
	}
	return bundle, true, nil
}

func (d *DB) SaveCertificateBundle(bundle CertificateBundle) error {
	bundle.UpdatedAt = time.Now().UTC()
	return d.saveAppJSON(keyHTTPS, bundle)
}

func (d *DB) LoadShares(value any) (bool, error) {
	return d.loadAppJSON(keyShares, value)
}

func (d *DB) SaveShares(value any) error {
	return d.saveAppJSON(keyShares, value)
}

func (d *DB) LoadChatGroups() ([]server.ChatGroup, error) {
	var groups []server.ChatGroup
	found, err := d.loadAppJSON(keyGroups, &groups)
	if err != nil || !found {
		return []server.ChatGroup{}, err
	}
	return groups, nil
}

func (d *DB) SaveChatGroup(group server.ChatGroup) error {
	groups, err := d.LoadChatGroups()
	if err != nil {
		return err
	}
	replaced := false
	for index := range groups {
		if groups[index].ID == group.ID {
			groups[index] = group
			replaced = true
			break
		}
	}
	if !replaced {
		groups = append(groups, group)
	}
	return d.saveAppJSON(keyGroups, groups)
}

func (d *DB) DeleteChatGroup(id string) error {
	groups, err := d.LoadChatGroups()
	if err != nil {
		return err
	}
	filtered := groups[:0]
	for _, group := range groups {
		if group.ID != id {
			filtered = append(filtered, group)
		}
	}
	return d.saveAppJSON(keyGroups, filtered)
}

func (d *DB) loadAppJSON(key []byte, value any) (bool, error) {
	found := false
	err := d.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketApp).Get(key)
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, value)
	})
	return found, err
}

func (d *DB) saveAppJSON(key []byte, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketApp).Put(key, raw)
	})
}

func (d *DB) LoadChatState() ([]server.ChatUser, []server.StoredChatMessage, error) {
	users := make([]server.ChatUser, 0)
	byConversation := make(map[string][]server.StoredChatMessage)
	err := d.db.View(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketUsers).ForEach(func(_, value []byte) error {
			var user server.ChatUser
			if err := json.Unmarshal(value, &user); err != nil {
				return err
			}
			users = append(users, user)
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket(bucketMessages).ForEach(func(_, value []byte) error {
			var item server.StoredChatMessage
			if err := json.Unmarshal(value, &item); err != nil {
				return err
			}
			rows := append(byConversation[item.ConversationID], item)
			if len(rows) > loadedMessagesPerConversation {
				copy(rows, rows[len(rows)-loadedMessagesPerConversation:])
				rows = rows[:loadedMessagesPerConversation]
			}
			byConversation[item.ConversationID] = rows
			return nil
		})
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(users, func(i, j int) bool { return users[i].LastSeen.After(users[j].LastSeen) })
	conversations := make([]string, 0, len(byConversation))
	for id := range byConversation {
		conversations = append(conversations, id)
	}
	sort.Strings(conversations)
	messages := make([]server.StoredChatMessage, 0)
	for _, id := range conversations {
		messages = append(messages, byConversation[id]...)
	}
	return users, messages, nil
}

func (d *DB) SaveChatUser(user server.ChatUser) error {
	raw, err := json.Marshal(user)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketUsers).Put([]byte(user.IP), raw)
	})
}

func (d *DB) LoadChatRemarks() ([]server.ChatRemark, error) {
	remarks := make([]server.ChatRemark, 0)
	err := d.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketRemarks).ForEach(func(_, value []byte) error {
			var remark server.ChatRemark
			if err := json.Unmarshal(value, &remark); err != nil {
				return err
			}
			remarks = append(remarks, remark)
			return nil
		})
	})
	return remarks, err
}

func (d *DB) SaveChatRemark(remark server.ChatRemark) error {
	if strings.TrimSpace(remark.OwnerIP) == "" || strings.TrimSpace(remark.TargetIP) == "" {
		return errors.New("chat remark identity is missing")
	}
	remark.Updated = time.Now().UTC()
	raw, err := json.Marshal(remark)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketRemarks).Put(remarkKey(remark.OwnerIP, remark.TargetIP), raw)
	})
}

func (d *DB) DeleteChatRemark(ownerIP, targetIP string) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketRemarks).Delete(remarkKey(ownerIP, targetIP))
	})
}

func (d *DB) ClearChatRemarks() error {
	return d.recreateBucket(bucketRemarks)
}

func (d *DB) DeleteChatUser(ip string) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketUsers).Delete([]byte(ip))
	})
}

func (d *DB) ClearChatUsers() error {
	return d.recreateBucket(bucketUsers)
}

func (d *DB) SaveChatMessage(conversationID string, message server.ChatMessage) (server.ChatMessage, error) {
	if strings.TrimSpace(conversationID) == "" || strings.TrimSpace(message.ID) == "" {
		return message, errors.New("chat message identity is missing")
	}
	stored := message
	if stored.Kind == server.ChatMessageKindImage && strings.TrimSpace(stored.FileName) == "" {
		if strings.EqualFold(stored.Mime, "image/png") {
			stored.FileName = "聊天图片.png"
		} else {
			stored.FileName = "聊天图片.jpg"
		}
	}
	var createdPath string
	if len(stored.Data) > 0 && (stored.Kind == server.ChatMessageKindImage || stored.Kind == server.ChatMessageKindFile) {
		return d.SaveChatAttachment(conversationID, stored, bytes.NewReader(stored.Data), int64(len(stored.Data)))
	}
	return d.saveStoredChatMessage(conversationID, stored, createdPath)
}

// SaveChatAttachment streams a chat attachment to disk without buffering it in
// the database process. The metadata and attachment index are committed only
// after the complete, size-checked file is safely in place.
func (d *DB) SaveChatAttachment(conversationID string, message server.ChatMessage, reader io.Reader, maxBytes int64) (server.ChatMessage, error) {
	if strings.TrimSpace(conversationID) == "" || strings.TrimSpace(message.ID) == "" {
		return message, errors.New("chat message identity is missing")
	}
	if reader == nil || maxBytes <= 0 {
		return message, errors.New("invalid chat attachment stream")
	}
	stored := message
	if stored.Kind == server.ChatMessageKindImage && strings.TrimSpace(stored.FileName) == "" {
		if strings.EqualFold(stored.Mime, "image/png") {
			stored.FileName = "聊天图片.png"
		} else {
			stored.FileName = "聊天图片.jpg"
		}
	}
	relative, size, err := d.writeAttachmentStream(stored, reader, maxBytes)
	if err != nil {
		return stored, err
	}
	stored.AttachmentPath = relative
	stored.FileSize = size
	stored.Data = nil
	createdPath, _ := d.resolveAttachment(relative)
	return d.saveStoredChatMessage(conversationID, stored, createdPath)
}

func (d *DB) saveStoredChatMessage(conversationID string, stored server.ChatMessage, createdPath string) (server.ChatMessage, error) {
	item := server.StoredChatMessage{ConversationID: conversationID, Message: stored}
	raw, err := json.Marshal(item)
	if err != nil {
		if createdPath != "" {
			_ = os.Remove(createdPath)
		}
		return stored, err
	}
	key := messageKey(conversationID, stored)
	err = d.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketMessages).Put(key, raw); err != nil {
			return err
		}
		if stored.AttachmentPath != "" {
			return tx.Bucket(bucketAttachments).Put([]byte(stored.ID), raw)
		}
		return nil
	})
	if err != nil {
		if createdPath != "" {
			_ = os.Remove(createdPath)
		}
		return stored, err
	}
	return stored, nil
}

func (d *DB) DeleteChatMessage(messageID string) error {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil
	}
	var attachmentPath string
	err := d.db.Update(func(tx *bolt.Tx) error {
		attachments := tx.Bucket(bucketAttachments)
		if raw := attachments.Get([]byte(messageID)); raw != nil {
			var item server.StoredChatMessage
			if json.Unmarshal(raw, &item) == nil {
				attachmentPath = item.Message.AttachmentPath
			}
			if err := attachments.Delete([]byte(messageID)); err != nil {
				return err
			}
		}
		messages := tx.Bucket(bucketMessages)
		cursor := messages.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			var item server.StoredChatMessage
			if json.Unmarshal(value, &item) == nil && item.Message.ID == messageID {
				return cursor.Delete()
			}
		}
		return nil
	})
	if err == nil && attachmentPath != "" {
		if absolute, resolveErr := d.resolveAttachment(attachmentPath); resolveErr == nil {
			_ = os.Remove(absolute)
		}
	}
	return err
}

func (d *DB) RecallChatMessage(messageID string, recalledAt time.Time) (server.StoredChatMessage, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return server.StoredChatMessage{}, os.ErrNotExist
	}
	if recalledAt.IsZero() {
		recalledAt = time.Now().UTC()
	}
	var recalled server.StoredChatMessage
	var attachmentPath string
	err := d.db.Update(func(tx *bolt.Tx) error {
		messages := tx.Bucket(bucketMessages)
		var foundKey []byte
		cursor := messages.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			var item server.StoredChatMessage
			if json.Unmarshal(value, &item) != nil || item.Message.ID != messageID {
				continue
			}
			foundKey = append([]byte(nil), key...)
			recalled = item
			break
		}
		if len(foundKey) == 0 {
			return os.ErrNotExist
		}
		attachmentPath = recalled.Message.AttachmentPath
		recalled.Message.Recalled = true
		recalled.Message.RecalledAt = recalledAt
		recalled.Message.Text = ""
		recalled.Message.Mime = ""
		recalled.Message.Data = nil
		recalled.Message.FileName = ""
		recalled.Message.FileSize = 0
		recalled.Message.FileURL = ""
		recalled.Message.AttachmentPath = ""
		raw, err := json.Marshal(recalled)
		if err != nil {
			return err
		}
		if err = messages.Put(foundKey, raw); err != nil {
			return err
		}
		return tx.Bucket(bucketAttachments).Delete([]byte(messageID))
	})
	if err != nil {
		return server.StoredChatMessage{}, err
	}
	if attachmentPath != "" {
		if absolute, resolveErr := d.resolveAttachment(attachmentPath); resolveErr == nil {
			_ = os.Remove(absolute)
		}
	}
	return recalled, nil
}

func (d *DB) MarkChatMessagesRead(messageIDs []string, readAt time.Time) error {
	ids := make(map[string]struct{}, len(messageIDs))
	for _, rawID := range messageIDs {
		if id := strings.TrimSpace(rawID); id != "" {
			ids[id] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if readAt.IsZero() {
		readAt = time.Now().UTC()
	}
	type update struct {
		key        []byte
		messageID  string
		attachment bool
		raw        []byte
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		messages := tx.Bucket(bucketMessages)
		updates := make([]update, 0, len(ids))
		cursor := messages.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			var item server.StoredChatMessage
			if json.Unmarshal(value, &item) != nil {
				continue
			}
			if _, wanted := ids[item.Message.ID]; !wanted ||
				!item.Message.Receipt || item.Message.Read {
				continue
			}
			item.Message.Read = true
			item.Message.ReadAt = readAt
			raw, err := json.Marshal(item)
			if err != nil {
				return err
			}
			updates = append(updates, update{
				key:        append([]byte(nil), key...),
				messageID:  item.Message.ID,
				attachment: item.Message.AttachmentPath != "",
				raw:        raw,
			})
		}
		attachments := tx.Bucket(bucketAttachments)
		for _, item := range updates {
			if err := messages.Put(item.key, item.raw); err != nil {
				return err
			}
			if item.attachment {
				if err := attachments.Put([]byte(item.messageID), item.raw); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (d *DB) DeleteChatConversation(conversationID string) error {
	paths := make([]string, 0)
	err := d.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketMessages)
		cursor := bucket.Cursor()
		prefix := []byte(conversationID + "\x00")
		for key, value := cursor.Seek(prefix); key != nil && strings.HasPrefix(string(key), string(prefix)); key, value = cursor.Next() {
			var item server.StoredChatMessage
			if json.Unmarshal(value, &item) == nil && item.Message.AttachmentPath != "" {
				paths = append(paths, item.Message.AttachmentPath)
				if err := tx.Bucket(bucketAttachments).Delete([]byte(item.Message.ID)); err != nil {
					return err
				}
			}
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, relative := range paths {
		if absolute, resolveErr := d.resolveAttachment(relative); resolveErr == nil {
			_ = os.Remove(absolute)
		}
	}
	return nil
}

func (d *DB) ClearChatMessages() error {
	if err := d.recreateBucket(bucketMessages); err != nil {
		return err
	}
	if err := d.recreateBucket(bucketAttachments); err != nil {
		return err
	}
	if !d.safeAttachmentRoot() {
		return errors.New("refusing to clear an unsafe chat attachment path")
	}
	if err := os.RemoveAll(d.attachmentRoot); err != nil {
		return err
	}
	return os.MkdirAll(d.attachmentRoot, 0700)
}

func (d *DB) OpenChatAttachment(messageID string) (server.ChatAttachment, error) {
	var found server.StoredChatMessage
	err := d.db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket(bucketAttachments).Get([]byte(messageID)); raw != nil {
			return json.Unmarshal(raw, &found)
		}
		// Compatibility with early hfs-go.db files created before the
		// attachment index bucket was introduced.
		return tx.Bucket(bucketMessages).ForEach(func(_, value []byte) error {
			var item server.StoredChatMessage
			if json.Unmarshal(value, &item) != nil || item.Message.ID != messageID {
				return nil
			}
			found = item
			return io.EOF
		})
	})
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return server.ChatAttachment{}, err
	}
	if found.Message.ID == "" || found.Message.AttachmentPath == "" {
		return server.ChatAttachment{}, os.ErrNotExist
	}
	absolute, err := d.resolveAttachment(found.Message.AttachmentPath)
	if err != nil {
		return server.ChatAttachment{}, err
	}
	file, err := os.Open(absolute)
	if err != nil {
		return server.ChatAttachment{}, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return server.ChatAttachment{}, err
	}
	return server.ChatAttachment{
		MessageID:      found.Message.ID,
		ConversationID: found.ConversationID,
		Name:           found.Message.FileName,
		MIME:           found.Message.Mime,
		Size:           info.Size(),
		ModTime:        info.ModTime(),
		Reader:         file,
	}, nil
}

func (d *DB) ListChatAttachments(query server.ChatArchiveQuery) (server.ChatArchivePage, error) {
	page := query.Page
	if page < 1 {
		page = 1
	}
	pageSize := query.PageSize
	if pageSize < 1 {
		pageSize = 24
	}
	if pageSize > 100 {
		pageSize = 100
	}
	needle := strings.ToLower(strings.TrimSpace(query.Query))
	kind := strings.ToLower(strings.TrimSpace(query.Kind))
	conversationID := strings.TrimSpace(query.ConversationID)
	items := make([]server.ChatArchiveItem, 0)
	err := d.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketAttachments).ForEach(func(_, value []byte) error {
			var stored server.StoredChatMessage
			if err := json.Unmarshal(value, &stored); err != nil {
				return err
			}
			message := stored.Message
			if message.Recalled || message.AttachmentPath == "" {
				return nil
			}
			if !chatConversationVisible(stored.ConversationID, query.ViewerIP, query.GroupMembers) {
				return nil
			}
			if conversationID != "" && stored.ConversationID != conversationID {
				return nil
			}
			if kind != "" && strings.ToLower(message.Kind) != kind {
				return nil
			}
			if !query.From.IsZero() && message.SentAt.Before(query.From) {
				return nil
			}
			if !query.To.IsZero() && message.SentAt.After(query.To) {
				return nil
			}
			if needle != "" {
				haystack := strings.ToLower(strings.Join([]string{
					message.FileName, message.Mime, message.ClientID, message.Name,
				}, " "))
				if !strings.Contains(haystack, needle) {
					return nil
				}
			}
			name := strings.TrimSpace(message.FileName)
			if name == "" {
				name = "聊天附件"
			}
			items = append(items, server.ChatArchiveItem{
				MessageID:      message.ID,
				ConversationID: stored.ConversationID,
				Kind:           message.Kind,
				Name:           name,
				MIME:           message.Mime,
				Size:           message.FileSize,
				SentAt:         message.SentAt,
				SenderIP:       message.ClientID,
				SenderName:     message.Name,
				TargetID:       message.TargetID,
				Private:        message.Private,
				FileURL:        "/__hfs/chat/file/" + url.PathEscape(message.ID) + "/" + url.PathEscape(name),
			})
			return nil
		})
	})
	if err != nil {
		return server.ChatArchivePage{}, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].SentAt.Equal(items[j].SentAt) {
			return items[i].MessageID > items[j].MessageID
		}
		return items[i].SentAt.After(items[j].SentAt)
	})
	total := len(items)
	pages := 0
	if total > 0 {
		pages = (total + pageSize - 1) / pageSize
		if page > pages {
			page = pages
		}
	}
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return server.ChatArchivePage{
		Items: append([]server.ChatArchiveItem(nil), items[start:end]...),
		Page:  page, PageSize: pageSize, Total: total, Pages: pages,
	}, nil
}

func (d *DB) ListChatMessages(query server.ChatHistoryQuery) (server.ChatHistoryPage, error) {
	page := query.Page
	if page < 1 {
		page = 1
	}
	pageSize := query.PageSize
	if pageSize < 1 {
		pageSize = 30
	}
	if pageSize > 100 {
		pageSize = 100
	}
	needle := strings.ToLower(strings.TrimSpace(query.Query))
	conversationID := strings.TrimSpace(query.ConversationID)
	items := make([]server.ChatHistoryItem, 0)
	err := d.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMessages).ForEach(func(_, value []byte) error {
			var stored server.StoredChatMessage
			if err := json.Unmarshal(value, &stored); err != nil {
				return err
			}
			message := stored.Message
			kind := strings.ToLower(strings.TrimSpace(message.Kind))
			if message.Recalled || (kind != "" &&
				kind != server.ChatMessageKindText &&
				kind != server.ChatMessageKindCode &&
				kind != server.ChatMessageKindDice) {
				return nil
			}
			if !chatConversationVisible(stored.ConversationID, query.ViewerIP, query.GroupMembers) {
				return nil
			}
			if conversationID != "" && stored.ConversationID != conversationID {
				return nil
			}
			if !query.From.IsZero() && message.SentAt.Before(query.From) {
				return nil
			}
			if !query.To.IsZero() && message.SentAt.After(query.To) {
				return nil
			}
			if needle != "" {
				haystack := strings.ToLower(strings.Join([]string{
					message.Text, message.ClientID, message.Name, message.TargetID,
				}, " "))
				if !strings.Contains(haystack, needle) {
					return nil
				}
			}
			items = append(items, server.ChatHistoryItem{
				MessageID:      message.ID,
				ConversationID: stored.ConversationID,
				Kind:           kind,
				Text:           message.Text,
				SentAt:         message.SentAt,
				SenderIP:       message.ClientID,
				SenderName:     message.Name,
				TargetID:       message.TargetID,
				Private:        message.Private,
			})
			return nil
		})
	})
	if err != nil {
		return server.ChatHistoryPage{}, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].SentAt.Equal(items[j].SentAt) {
			return items[i].MessageID > items[j].MessageID
		}
		return items[i].SentAt.After(items[j].SentAt)
	})
	total := len(items)
	pages := 0
	if total > 0 {
		pages = (total + pageSize - 1) / pageSize
		if page > pages {
			page = pages
		}
	}
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return server.ChatHistoryPage{
		Items: append([]server.ChatHistoryItem(nil), items[start:end]...),
		Page:  page, PageSize: pageSize, Total: total, Pages: pages,
	}, nil
}

func chatConversationVisible(conversationID, viewerIP string, groupMembers map[string][]string) bool {
	if viewerIP == "" || server.ChatConversationVisibleToIP(conversationID, viewerIP) {
		return true
	}
	if !strings.HasPrefix(conversationID, "group:") {
		return false
	}
	groupID := strings.TrimPrefix(conversationID, "group:")
	for _, member := range groupMembers[groupID] {
		if member == viewerIP {
			return true
		}
	}
	return false
}

// ChatAttachmentPath resolves one attachment for trusted desktop-only actions.
func (d *DB) ChatAttachmentPath(messageID string) (string, error) {
	var relative string
	err := d.db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket(bucketAttachments).Get([]byte(messageID)); raw != nil {
			var item server.StoredChatMessage
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			relative = item.Message.AttachmentPath
			return nil
		}
		return tx.Bucket(bucketMessages).ForEach(func(_, value []byte) error {
			var item server.StoredChatMessage
			if json.Unmarshal(value, &item) == nil && item.Message.ID == messageID {
				relative = item.Message.AttachmentPath
				return io.EOF
			}
			return nil
		})
	})
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return "", err
	}
	if relative == "" {
		return "", os.ErrNotExist
	}
	return d.resolveAttachment(relative)
}

func (d *DB) SaveAccessRecord(record server.AccessRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return d.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketAccess)
		sequence, err := bucket.NextSequence()
		if err != nil {
			return err
		}
		record.ID = sequence
		raw, err = json.Marshal(record)
		if err != nil {
			return err
		}
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, sequence)
		return bucket.Put(key, raw)
	})
}

func (d *DB) ListAccessRecords(limit int) ([]server.AccessRecord, error) {
	if limit <= 0 {
		limit = 1000
	}
	records := make([]server.AccessRecord, 0, limit)
	err := d.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(bucketAccess).Cursor()
		for key, value := cursor.Last(); key != nil && len(records) < limit; key, value = cursor.Prev() {
			var record server.AccessRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			records = append(records, record)
		}
		return nil
	})
	for left, right := 0, len(records)-1; left < right; left, right = left+1, right-1 {
		records[left], records[right] = records[right], records[left]
	}
	return records, err
}

func (d *DB) ClearAccessRecords() error {
	return d.recreateBucket(bucketAccess)
}

func (d *DB) recreateBucket(name []byte) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(name); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
			return err
		}
		_, err := tx.CreateBucket(name)
		return err
	})
}

func (d *DB) writeAttachmentStream(message server.ChatMessage, reader io.Reader, maxBytes int64) (string, int64, error) {
	at := message.SentAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	dateDirectory := at.Local().Format("2006-01-02")
	name := safeAttachmentName(message.FileName)
	if name == "" {
		extension := extensionForMIME(message.Mime)
		name = message.ID + extension
	} else {
		name = message.ID + "_" + name
	}
	relative := filepath.Join(dateDirectory, name)
	absolute, err := d.resolveAttachment(relative)
	if err != nil {
		return "", 0, err
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return "", 0, err
	}
	temp, err := os.CreateTemp(filepath.Dir(absolute), ".hfs-chat-*")
	if err != nil {
		return "", 0, err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	var size int64
	if err = temp.Chmod(0600); err == nil {
		size, err = io.Copy(temp, io.LimitReader(reader, maxBytes+1))
		if err == nil && size == 0 {
			err = errors.New("chat attachment is empty")
		} else if err == nil && size > maxBytes {
			err = fmt.Errorf("chat attachment exceeds %d bytes", maxBytes)
		}
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, err
	}
	if err = os.Rename(tempName, absolute); err != nil {
		return "", 0, err
	}
	return filepath.ToSlash(relative), size, nil
}

func (d *DB) resolveAttachment(relative string) (string, error) {
	relative = filepath.FromSlash(strings.TrimSpace(relative))
	if relative == "" || filepath.IsAbs(relative) {
		return "", errors.New("invalid chat attachment path")
	}
	absolute := filepath.Clean(filepath.Join(d.attachmentRoot, relative))
	rel, err := filepath.Rel(d.attachmentRoot, absolute)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("chat attachment escaped its storage directory")
	}
	return absolute, nil
}

func (d *DB) safeAttachmentRoot() bool {
	root := filepath.Clean(d.attachmentRoot)
	parent := filepath.Clean(filepath.Dir(d.path))
	return strings.EqualFold(filepath.Base(root), "chat_files") &&
		strings.EqualFold(filepath.Dir(root), parent) &&
		root != filepath.VolumeName(root)+string(os.PathSeparator)
}

func safeAttachmentName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	runes := []rune(name)
	if len(runes) > 100 {
		name = string(runes[:100])
	}
	name = strings.Trim(name, ". ")
	return name
}

func extensionForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	default:
		return ".bin"
	}
}

func messageKey(conversationID string, message server.ChatMessage) []byte {
	nanos := message.SentAt.UnixNano()
	if nanos < 0 {
		nanos = 0
	}
	return []byte(conversationID + "\x00" + fmt.Sprintf("%020d", nanos) + "\x00" + message.ID)
}

func remarkKey(ownerIP, targetIP string) []byte {
	return []byte(strings.TrimSpace(ownerIP) + "\x00" + strings.TrimSpace(targetIP))
}

// MigrationDone records one-time imports without exposing another sidecar.
func (d *DB) MigrationDone(name string) (bool, error) {
	var done bool
	err := d.db.View(func(tx *bolt.Tx) error {
		done = string(tx.Bucket(bucketApp).Get([]byte("migration:"+name))) == "1"
		return nil
	})
	return done, err
}

func (d *DB) MarkMigrationDone(name string) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketApp).Put([]byte("migration:"+name), []byte(strconv.Itoa(1)))
	})
}

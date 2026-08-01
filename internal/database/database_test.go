package database

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tinychatgo/internal/server"
)

func TestDatabasePersistsApplicationAndChatData(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "hfs-go.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	settings := map[string]any{"allowChat": true, "port": "1122"}
	if err = store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	var restoredSettings map[string]any
	if found, loadErr := store.LoadSettings(&restoredSettings); loadErr != nil || !found {
		t.Fatalf("load settings = found %v, error %v", found, loadErr)
	}
	if restoredSettings["port"] != "1122" {
		t.Fatalf("restored settings = %#v", restoredSettings)
	}

	certificate := CertificateBundle{
		CACertPEM: []byte("ca"), CAKeyPEM: []byte("ca-key"),
		CertPEM: []byte("cert"), KeyPEM: []byte("key"),
	}
	if err = store.SaveCertificateBundle(certificate); err != nil {
		t.Fatal(err)
	}
	restoredCertificate, found, err := store.LoadCertificateBundle()
	if err != nil || !found || string(restoredCertificate.KeyPEM) != "key" {
		t.Fatalf("restored certificate = %#v, found %v, error %v", restoredCertificate, found, err)
	}

	shares := []map[string]any{{"name": "example", "path": `C:\example`}}
	if err = store.SaveShares(shares); err != nil {
		t.Fatal(err)
	}
	var restoredShares []map[string]any
	if found, loadErr := store.LoadShares(&restoredShares); loadErr != nil || !found || len(restoredShares) != 1 {
		t.Fatalf("load shares = %#v, found %v, error %v", restoredShares, found, loadErr)
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	user := server.ChatUser{
		IP:          "192.0.2.10",
		Avatar:      "data:image/jpeg;base64,avatar-data",
		Name:        "王超",
		Blacklisted: true,
		FirstSeen:   now,
		LastSeen:    now,
		Client:      server.ChatClientInfo{IP: "192.0.2.10", Browser: "Edge"},
	}
	if err = store.SaveChatUser(user); err != nil {
		t.Fatal(err)
	}
	text := server.ChatMessage{
		ID:       "text-message",
		Kind:     server.ChatMessageKindText,
		Sender:   "user",
		ClientID: user.IP,
		Text:     "持久化文字",
		SentAt:   now,
	}
	if _, err = store.SaveChatMessage("admin:"+user.IP, text); err != nil {
		t.Fatal(err)
	}
	imageData := []byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10, 1, 2, 3}
	image := server.ChatMessage{
		ID:       "image-message",
		Kind:     server.ChatMessageKindImage,
		Sender:   "user",
		ClientID: user.IP,
		Mime:     "image/png",
		Data:     imageData,
		SentAt:   now,
	}
	storedImage, err := store.SaveChatMessage("admin:"+user.IP, image)
	if err != nil {
		t.Fatal(err)
	}
	if storedImage.AttachmentPath == "" || len(storedImage.Data) != 0 {
		t.Fatalf("stored image metadata = %#v", storedImage)
	}
	streamed := server.ChatMessage{
		ID: "streamed-file", Kind: server.ChatMessageKindFile, Sender: "user",
		ClientID: user.IP, FileName: "large.bin", Mime: "application/octet-stream", SentAt: now.Add(time.Second),
	}
	storedFile, err := store.SaveChatAttachment("admin:"+user.IP, streamed, strings.NewReader("streamed"), 16)
	if err != nil || storedFile.FileSize != 8 || storedFile.AttachmentPath == "" {
		t.Fatalf("streamed attachment = %#v, %v", storedFile, err)
	}
	rejected := streamed
	rejected.ID = "too-large"
	if _, err = store.SaveChatAttachment("admin:"+user.IP, rejected, strings.NewReader("12345"), 4); err == nil {
		t.Fatal("oversized streamed attachment was accepted")
	}
	wantDateDirectory := filepath.Join(root, "chat_files", now.Local().Format("2006-01-02"))
	if _, err = os.Stat(wantDateDirectory); err != nil {
		t.Fatalf("dated chat_files directory missing: %v", err)
	}

	users, messages, err := store.LoadChatState()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Name != "王超" || users[0].Avatar != user.Avatar || !users[0].Blacklisted {
		t.Fatalf("restored users = %#v", users)
	}
	if len(messages) != 3 {
		t.Fatalf("restored messages = %#v", messages)
	}
	attachment, err := store.OpenChatAttachment(image.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotImage, readErr := io.ReadAll(attachment.Reader)
	_ = attachment.Reader.Close()
	if readErr != nil || string(gotImage) != string(imageData) {
		t.Fatalf("attachment = %x, %v", gotImage, readErr)
	}
	history, err := store.ListChatMessages(server.ChatHistoryQuery{
		Query: "持久化", Page: 1, PageSize: 30,
	})
	if err != nil || history.Total != 1 || len(history.Items) != 1 ||
		history.Items[0].MessageID != text.ID || history.Items[0].Text != text.Text {
		t.Fatalf("chat history search = %#v, %v", history, err)
	}

	if err = store.SaveAccessRecord(server.AccessRecord{At: now, IP: user.IP, Operation: "下载文件"}); err != nil {
		t.Fatal(err)
	}
	records, err := store.ListAccessRecords(10)
	if err != nil || len(records) != 1 || records[0].Operation != "下载文件" {
		t.Fatalf("access records = %#v, %v", records, err)
	}
	if err = store.ClearAccessRecords(); err != nil {
		t.Fatal(err)
	}
	if records, err = store.ListAccessRecords(10); err != nil || len(records) != 0 {
		t.Fatalf("cleared access records = %#v, %v", records, err)
	}

	if err = store.ClearChatMessages(); err != nil {
		t.Fatal(err)
	}
	if _, err = store.OpenChatAttachment(image.ID); !os.IsNotExist(err) {
		t.Fatalf("cleared attachment error = %v", err)
	}
	if entries, readDirErr := os.ReadDir(filepath.Join(root, "chat_files")); readDirErr != nil || len(entries) != 0 {
		t.Fatalf("chat_files after clear = %#v, %v", entries, readDirErr)
	}
}

func TestChatAttachmentsDeduplicateBySHA256AndKeepSharedFileUntilLastReference(t *testing.T) {
	root := t.TempDir()
	store, err := Open(filepath.Join(root, "tinychatgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	content := "identical attachment bytes"
	first, err := store.SaveChatAttachment("direct:a|b", server.ChatMessage{
		ID: "dedup-first", Kind: server.ChatMessageKindFile, Sender: "user",
		ClientID: "a", FileName: "first.txt", SentAt: time.Now().UTC(),
	}, strings.NewReader(content), 1024)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveChatAttachment("direct:a|b", server.ChatMessage{
		ID: "dedup-second", Kind: server.ChatMessageKindFile, Sender: "user",
		ClientID: "b", FileName: "renamed.txt", SentAt: time.Now().UTC().Add(48 * time.Hour),
	}, strings.NewReader(content), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if first.AttachmentHash == "" || first.AttachmentHash != second.AttachmentHash ||
		first.AttachmentPath != second.AttachmentPath {
		t.Fatalf("attachments were not deduplicated: first=%#v second=%#v", first, second)
	}
	absolute, err := store.resolveAttachment(first.AttachmentPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteChatMessage(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(absolute); err != nil {
		t.Fatalf("shared attachment was removed too early: %v", err)
	}
	attachment, err := store.OpenChatAttachment(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(attachment.Reader)
	_ = attachment.Reader.Close()
	if readErr != nil || string(got) != content {
		t.Fatalf("deduplicated attachment content = %q, %v", got, readErr)
	}
	if err = store.DeleteChatMessage(second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(absolute); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced attachment still exists: %v", err)
	}
}

func TestDatabasePersistsChatReadReceipt(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tinychatgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := server.ChatMessage{
		ID:       "read-message",
		Kind:     server.ChatMessageKindText,
		Sender:   "user",
		ClientID: "192.0.2.20",
		Text:     "请标记已读",
		Receipt:  true,
		SentAt:   time.Now().UTC(),
	}
	if _, err = store.SaveChatMessage(server.ChatAdminConversationID+":192.0.2.20", message); err != nil {
		t.Fatal(err)
	}
	readAt := time.Now().UTC()
	if err = store.MarkChatMessagesRead([]string{message.ID}, readAt); err != nil {
		t.Fatal(err)
	}
	_, messages, err := store.LoadChatState()
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !messages[0].Message.Read || messages[0].Message.ReadAt.IsZero() {
		t.Fatalf("persisted read receipt = %#v", messages)
	}
}

func TestChatHistorySearchRespectsConversationVisibility(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tinychatgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	directID := "direct:192.0.2.31|192.0.2.32"
	groupID := "group:project-team"
	for index, entry := range []struct {
		conversationID string
		text           string
	}{
		{server.ChatGroupConversationID, "系统群消息"},
		{directID, "私聊消息"},
		{groupID, "自建群消息"},
	} {
		if _, err = store.SaveChatMessage(entry.conversationID, server.ChatMessage{
			ID:       "visibility-" + entry.text,
			Kind:     server.ChatMessageKindText,
			ClientID: "192.0.2.31",
			Text:     entry.text,
			SentAt:   now.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}

	visible, err := store.ListChatMessages(server.ChatHistoryQuery{
		ViewerIP: "192.0.2.31",
		Page:     1,
		PageSize: 30,
		GroupMembers: map[string][]string{
			"project-team": {"192.0.2.31"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if visible.Total != 3 {
		t.Fatalf("member-visible history = %#v", visible)
	}

	outsider, err := store.ListChatMessages(server.ChatHistoryQuery{
		ViewerIP: "192.0.2.99", Page: 1, PageSize: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outsider.Total != 1 || len(outsider.Items) != 1 ||
		outsider.Items[0].ConversationID != server.ChatGroupConversationID {
		t.Fatalf("outsider-visible history = %#v", outsider)
	}
}

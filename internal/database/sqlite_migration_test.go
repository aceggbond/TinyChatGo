package database

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tinychatgo/internal/server"
)

func TestOpenCreatesSQLiteDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tinychatgo.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, sqliteHeader) {
		t.Fatalf("database header = %q, want SQLite", data[:minInt(len(data), len(sqliteHeader))])
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"users", "chat_messages", "files", "clawbot_bindings", "certificates"} {
		var found string
		if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&found); err != nil {
			t.Fatalf("business table %s: %v", table, err)
		}
	}
}

func TestOpenRejectsLegacyDatabaseWithoutMigratingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tinychatgo.db")
	legacy := []byte("legacy-bbolt-database")
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("legacy database was unexpectedly accepted")
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unchanged, legacy) {
		t.Fatal("legacy database was modified")
	}
}

func TestBusinessTablesTrackUsersMessagesFilesAndClawBot(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tinychatgo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Millisecond)
	account := server.Account{ID: "user-1", Username: "alice", PasswordHash: "$argon2id$hash", Status: server.AccountStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveAccount(account); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveChatUser(server.ChatUser{IP: account.ID, Username: account.Username, Name: "Alice", Avatar: "data:image/png;base64,AAAA", FirstSeen: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveClawBotBinding(server.ClawBotBinding{AccountID: account.ID, Status: "bound", BotToken: "secret", BotID: "bot-1", WeixinUserID: "wx-1", UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	message := server.ChatMessage{ID: "message-1", Kind: server.ChatMessageKindFile, Sender: "user", ClientID: account.ID, FileName: "archive.zip", Mime: "application/zip", SentAt: now}
	stored, err := store.SaveChatAttachment("private:user-1", message, bytes.NewReader([]byte("zip-data")), 64)
	if err != nil {
		t.Fatal(err)
	}

	var username, passwordHash, avatar, weixinID string
	var bound int
	if err := store.db.sql.QueryRow("SELECT username,password_hash,avatar_base64,clawbot_bound,weixin_user_id FROM users WHERE id=?", account.ID).Scan(&username, &passwordHash, &avatar, &bound, &weixinID); err != nil {
		t.Fatal(err)
	}
	if username != account.Username || passwordHash != account.PasswordHash || avatar == "" || bound != 1 || weixinID != "wx-1" {
		t.Fatalf("user business row = %q %q avatar=%t bound=%d weixin=%q", username, passwordHash, avatar != "", bound, weixinID)
	}
	var conversation, storagePath, extension string
	if err := store.db.sql.QueryRow(`SELECT m.conversation_id,f.storage_path,f.file_extension
		FROM chat_messages m JOIN files f ON f.message_id=m.id WHERE m.id=?`, message.ID).Scan(&conversation, &storagePath, &extension); err != nil {
		t.Fatal(err)
	}
	if conversation != "private:user-1" || storagePath != stored.AttachmentPath || extension != ".zip" || filepath.Ext(storagePath) != ".zip" {
		t.Fatalf("message/file business rows = conversation %q path %q extension %q", conversation, storagePath, extension)
	}
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

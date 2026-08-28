package database

import (
	"bytes"
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"tinychatgo/internal/server"
)

var (
	errBucketNotFound = errors.New("database: bucket not found")
	errBucketExists   = errors.New("database: bucket already exists")
)

const sqliteSchemaVersion = 2

var sqliteHeader = []byte("SQLite format 3\x00")

type kvDB struct{ sql *sql.DB }

type kvTx struct {
	tx  *sql.Tx
	err error
}

type kvBucket struct {
	tx   *kvTx
	name string
}

type kvPair struct{ key, value []byte }

type kvCursor struct {
	bucket *kvBucket
	items  []kvPair
	index  int
}

func openSQLiteKV(path string) (*kvDB, error) {
	if err := requireFreshSQLiteSchema(path); err != nil {
		return nil, err
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	// One connection gives the old embedded key/value layer's serialized-write
	// semantics and ensures every transaction receives the same PRAGMA settings.
	raw.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		`CREATE TABLE IF NOT EXISTS buckets (
			name TEXT PRIMARY KEY
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS kv (
			bucket TEXT NOT NULL,
			key BLOB NOT NULL,
			value BLOB NOT NULL,
			PRIMARY KEY (bucket, key),
			FOREIGN KEY (bucket) REFERENCES buckets(name) ON DELETE CASCADE
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS bucket_sequences (
			bucket TEXT PRIMARY KEY,
			value INTEGER NOT NULL,
			FOREIGN KEY (bucket) REFERENCES buckets(name) ON DELETE CASCADE
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL DEFAULT '',
			password_hash TEXT NOT NULL DEFAULT '',
			avatar_base64 TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT '',
			approved_at TEXT NOT NULL DEFAULT '',
			last_login_at TEXT NOT NULL DEFAULT '',
			last_ip TEXT NOT NULL DEFAULT '',
			profile_json TEXT NOT NULL DEFAULT '',
			clawbot_bound INTEGER NOT NULL DEFAULT 0,
			clawbot_status TEXT NOT NULL DEFAULT '',
			clawbot_bot_id TEXT NOT NULL DEFAULT '',
			weixin_user_id TEXT NOT NULL DEFAULT '',
			clawbot_json TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS users_username_idx ON users(username) WHERE username <> ''`,
		`CREATE TABLE IF NOT EXISTS chat_messages (
			id TEXT PRIMARY KEY,
			conversation_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			sender TEXT NOT NULL DEFAULT '',
			sender_id TEXT NOT NULL DEFAULT '',
			sender_name TEXT NOT NULL DEFAULT '',
			text_content TEXT NOT NULL DEFAULT '',
			target_id TEXT NOT NULL DEFAULT '',
			group_id TEXT NOT NULL DEFAULT '',
			is_private INTEGER NOT NULL DEFAULT 0,
			sent_at TEXT NOT NULL,
			is_read INTEGER NOT NULL DEFAULT 0,
			read_at TEXT NOT NULL DEFAULT '',
			is_recalled INTEGER NOT NULL DEFAULT 0,
			recalled_at TEXT NOT NULL DEFAULT '',
			message_json TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS chat_messages_conversation_time_idx ON chat_messages(conversation_id,sent_at,id)`,
		`CREATE TABLE IF NOT EXISTS files (
			message_id TEXT PRIMARY KEY,
			conversation_id TEXT NOT NULL,
			content_hash TEXT NOT NULL DEFAULT '',
			storage_path TEXT NOT NULL,
			original_name TEXT NOT NULL DEFAULT '',
			file_extension TEXT NOT NULL DEFAULT '',
			mime_type TEXT NOT NULL DEFAULT '',
			file_size INTEGER NOT NULL DEFAULT 0,
			file_kind TEXT NOT NULL DEFAULT '',
			sent_at TEXT NOT NULL DEFAULT '',
			FOREIGN KEY(message_id) REFERENCES chat_messages(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS files_hash_idx ON files(content_hash)`,
		`CREATE INDEX IF NOT EXISTS files_date_idx ON files(sent_at)`,
		`CREATE TABLE IF NOT EXISTS clawbot_bindings (
			account_id TEXT PRIMARY KEY,
			status TEXT NOT NULL DEFAULT '',
			bot_id TEXT NOT NULL DEFAULT '',
			weixin_user_id TEXT NOT NULL DEFAULT '',
			reply_user_id TEXT NOT NULL DEFAULT '',
			base_url TEXT NOT NULL DEFAULT '',
			forward_enabled INTEGER NOT NULL DEFAULT 0,
			bound_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT '',
			last_message_at TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			binding_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS certificates (
			id TEXT PRIMARY KEY,
			ip TEXT NOT NULL DEFAULT '',
			dns_names TEXT NOT NULL DEFAULT '',
			serial_number TEXT NOT NULL DEFAULT '',
			ca_certificate_pem BLOB NOT NULL,
			ca_private_key_pem BLOB NOT NULL,
			server_certificate_pem BLOB NOT NULL,
			server_private_key_pem BLOB NOT NULL,
			not_before TEXT NOT NULL DEFAULT '',
			not_after TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT ''
		)`,
		fmt.Sprintf("PRAGMA user_version=%d", sqliteSchemaVersion),
	} {
		if _, err := raw.Exec(statement); err != nil {
			raw.Close()
			return nil, fmt.Errorf("initialize sqlite database: %w", err)
		}
	}
	if err := ensureSQLiteColumn(raw, "clawbot_bindings", "reply_user_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		raw.Close()
		return nil, err
	}
	if _, err := raw.Exec("PRAGMA foreign_keys=ON"); err != nil {
		raw.Close()
		return nil, fmt.Errorf("enable sqlite foreign keys: %w", err)
	}
	return &kvDB{sql: raw}, nil
}

func ensureSQLiteColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
}

func requireFreshSQLiteSchema(path string) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect database: %w", err)
	}
	if info, statErr := file.Stat(); statErr == nil && info.Size() == 0 {
		_ = file.Close()
		return nil
	}
	header := make([]byte, len(sqliteHeader))
	_, readErr := io.ReadFull(file, header)
	_ = file.Close()
	if readErr != nil || !bytes.Equal(header, sqliteHeader) {
		return errors.New("旧数据库格式不再支持：请先备份并移走 tinychatgo.db，再启动新版创建全新 SQLite 数据库")
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer check.Close()
	var version int
	if err := check.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read sqlite schema version: %w", err)
	}
	if version != sqliteSchemaVersion {
		return fmt.Errorf("旧 SQLite 结构版本 %d 不再迁移：请先备份并移走 tinychatgo.db，再启动新版", version)
	}
	return nil
}

func (d *kvDB) Close() error { return d.sql.Close() }

func (d *kvDB) View(fn func(*kvTx) error) error {
	return d.withTx(true, fn)
}

func (d *kvDB) Update(fn func(*kvTx) error) error {
	return d.withTx(false, fn)
}

func (d *kvDB) withTx(readOnly bool, fn func(*kvTx) error) error {
	raw, err := d.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return err
	}
	tx := &kvTx{tx: raw}
	if err = fn(tx); err == nil {
		err = tx.err
	}
	if err != nil || readOnly {
		_ = raw.Rollback()
		return err
	}
	return raw.Commit()
}

func (tx *kvTx) Bucket(name []byte) *kvBucket {
	var exists int
	if err := tx.tx.QueryRow("SELECT 1 FROM buckets WHERE name = ?", string(name)).Scan(&exists); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			tx.remember(err)
		}
		return nil
	}
	return &kvBucket{tx: tx, name: string(name)}
}

func (tx *kvTx) CreateBucketIfNotExists(name []byte) (*kvBucket, error) {
	if _, err := tx.tx.Exec("INSERT OR IGNORE INTO buckets(name) VALUES (?)", string(name)); err != nil {
		return nil, err
	}
	return &kvBucket{tx: tx, name: string(name)}, nil
}

func (tx *kvTx) CreateBucket(name []byte) (*kvBucket, error) {
	if _, err := tx.tx.Exec("INSERT INTO buckets(name) VALUES (?)", string(name)); err != nil {
		return nil, fmt.Errorf("%w: %s", errBucketExists, name)
	}
	return &kvBucket{tx: tx, name: string(name)}, nil
}

func (tx *kvTx) DeleteBucket(name []byte) error {
	if err := tx.clearBusinessTable(string(name)); err != nil {
		return err
	}
	result, err := tx.tx.Exec("DELETE FROM buckets WHERE name = ?", string(name))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return errBucketNotFound
	}
	return nil
}

func (tx *kvTx) remember(err error) {
	if tx.err == nil {
		tx.err = err
	}
}

func (b *kvBucket) Put(key, value []byte) error {
	_, err := b.tx.tx.Exec(`INSERT INTO kv(bucket,key,value) VALUES (?,?,?)
		ON CONFLICT(bucket,key) DO UPDATE SET value=excluded.value`, b.name, key, value)
	if err != nil {
		return err
	}
	return b.tx.syncBusinessPut(b.name, key, value)
}

func (b *kvBucket) Get(key []byte) []byte {
	var value []byte
	if err := b.tx.tx.QueryRow("SELECT value FROM kv WHERE bucket = ? AND key = ?", b.name, key).Scan(&value); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			b.tx.remember(err)
		}
		return nil
	}
	return append([]byte(nil), value...)
}

func (b *kvBucket) Delete(key []byte) error {
	var previous []byte
	if err := b.tx.tx.QueryRow("SELECT value FROM kv WHERE bucket = ? AND key = ?", b.name, key).Scan(&previous); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err := b.tx.tx.Exec("DELETE FROM kv WHERE bucket = ? AND key = ?", b.name, key)
	if err != nil {
		return err
	}
	return b.tx.syncBusinessDelete(b.name, key, previous)
}

func (b *kvBucket) ForEach(fn func(k, v []byte) error) error {
	items, err := b.all()
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := fn(item.key, item.value); err != nil {
			return err
		}
	}
	return nil
}

func (b *kvBucket) Cursor() *kvCursor {
	items, err := b.all()
	if err != nil {
		b.tx.remember(err)
	}
	return &kvCursor{bucket: b, items: items, index: -1}
}

func (b *kvBucket) all() ([]kvPair, error) {
	rows, err := b.tx.tx.Query("SELECT key,value FROM kv WHERE bucket = ? ORDER BY key", b.name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []kvPair
	for rows.Next() {
		var item kvPair
		if err := rows.Scan(&item.key, &item.value); err != nil {
			return nil, err
		}
		item.key = append([]byte(nil), item.key...)
		item.value = append([]byte(nil), item.value...)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (b *kvBucket) NextSequence() (uint64, error) {
	if _, err := b.tx.tx.Exec(`INSERT INTO bucket_sequences(bucket,value) VALUES (?,1)
		ON CONFLICT(bucket) DO UPDATE SET value=value+1`, b.name); err != nil {
		return 0, err
	}
	var value uint64
	if err := b.tx.tx.QueryRow("SELECT value FROM bucket_sequences WHERE bucket = ?", b.name).Scan(&value); err != nil {
		return 0, err
	}
	return value, nil
}

func (b *kvBucket) setSequence(value uint64) error {
	_, err := b.tx.tx.Exec(`INSERT INTO bucket_sequences(bucket,value) VALUES (?,?)
		ON CONFLICT(bucket) DO UPDATE SET value=excluded.value`, b.name, value)
	return err
}

func (c *kvCursor) First() ([]byte, []byte) { return c.at(0) }
func (c *kvCursor) Last() ([]byte, []byte)  { return c.at(len(c.items) - 1) }
func (c *kvCursor) Next() ([]byte, []byte)  { return c.at(c.index + 1) }
func (c *kvCursor) Prev() ([]byte, []byte)  { return c.at(c.index - 1) }

func (c *kvCursor) Seek(target []byte) ([]byte, []byte) {
	index := sort.Search(len(c.items), func(i int) bool {
		return bytes.Compare(c.items[i].key, target) >= 0
	})
	return c.at(index)
}

func (c *kvCursor) Delete() error {
	if c.index < 0 || c.index >= len(c.items) {
		return errors.New("database: cursor is not positioned")
	}
	return c.bucket.Delete(c.items[c.index].key)
}

func (c *kvCursor) at(index int) ([]byte, []byte) {
	c.index = index
	if index < 0 || index >= len(c.items) {
		return nil, nil
	}
	return c.items[index].key, c.items[index].value
}

func (tx *kvTx) syncBusinessPut(bucket string, key []byte, value []byte) error {
	switch bucket {
	case string(bucketApp):
		if bytes.Equal(key, keyHTTPS) {
			return tx.syncCertificate(value)
		}
	case string(bucketAccounts):
		var account server.Account
		if err := json.Unmarshal(value, &account); err != nil {
			return err
		}
		_, err := tx.tx.Exec(`INSERT INTO users(id,username,password_hash,status,created_at,updated_at,approved_at,last_login_at,last_ip)
			VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
			username=excluded.username,password_hash=excluded.password_hash,status=excluded.status,
			created_at=excluded.created_at,updated_at=excluded.updated_at,approved_at=excluded.approved_at,
			last_login_at=excluded.last_login_at,last_ip=excluded.last_ip`,
			account.ID, account.Username, account.PasswordHash, account.Status, dbTime(account.CreatedAt), dbTime(account.UpdatedAt),
			dbTime(account.ApprovedAt), dbTime(account.LastLoginAt), account.LastIP)
		return err
	case string(bucketUsers):
		var profile server.ChatUser
		if err := json.Unmarshal(value, &profile); err != nil {
			return err
		}
		_, err := tx.tx.Exec(`INSERT INTO users(id,username,avatar_base64,display_name,profile_json)
			VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET username=CASE WHEN excluded.username<>'' THEN excluded.username ELSE users.username END,
			avatar_base64=excluded.avatar_base64,display_name=excluded.display_name,profile_json=excluded.profile_json`,
			profile.IP, profile.Username, profile.Avatar, profile.Name, string(value))
		return err
	case string(bucketClawBot):
		var binding server.ClawBotBinding
		if err := json.Unmarshal(value, &binding); err != nil {
			return err
		}
		bound := binding.BotToken != "" || binding.WeixinUserID != "" || binding.Status == "bound"
		if _, err := tx.tx.Exec(`INSERT INTO clawbot_bindings(account_id,status,bot_id,weixin_user_id,reply_user_id,base_url,forward_enabled,bound_at,updated_at,last_message_at,last_error,binding_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(account_id) DO UPDATE SET status=excluded.status,bot_id=excluded.bot_id,
			weixin_user_id=excluded.weixin_user_id,reply_user_id=excluded.reply_user_id,base_url=excluded.base_url,forward_enabled=excluded.forward_enabled,
			bound_at=excluded.bound_at,updated_at=excluded.updated_at,last_message_at=excluded.last_message_at,
			last_error=excluded.last_error,binding_json=excluded.binding_json`,
			binding.AccountID, binding.Status, binding.BotID, binding.WeixinUserID, binding.ReplyUserID, binding.BaseURL, binding.ForwardEnabled,
			dbTime(binding.BoundAt), dbTime(binding.UpdatedAt), dbTime(binding.LastMessageAt), binding.LastError, string(value)); err != nil {
			return err
		}
		_, err := tx.tx.Exec(`INSERT INTO users(id,clawbot_bound,clawbot_status,clawbot_bot_id,weixin_user_id,clawbot_json)
			VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET clawbot_bound=excluded.clawbot_bound,
			clawbot_status=excluded.clawbot_status,clawbot_bot_id=excluded.clawbot_bot_id,
			weixin_user_id=excluded.weixin_user_id,clawbot_json=excluded.clawbot_json`,
			binding.AccountID, bound, binding.Status, binding.BotID, binding.WeixinUserID, string(value))
		return err
	case string(bucketMessages):
		var stored server.StoredChatMessage
		if err := json.Unmarshal(value, &stored); err != nil {
			return err
		}
		message := stored.Message
		if _, err := tx.tx.Exec(`INSERT INTO chat_messages(id,conversation_id,kind,sender,sender_id,sender_name,text_content,target_id,group_id,is_private,sent_at,is_read,read_at,is_recalled,recalled_at,message_json)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET conversation_id=excluded.conversation_id,
			kind=excluded.kind,sender=excluded.sender,sender_id=excluded.sender_id,sender_name=excluded.sender_name,
			text_content=excluded.text_content,target_id=excluded.target_id,group_id=excluded.group_id,is_private=excluded.is_private,
			sent_at=excluded.sent_at,is_read=excluded.is_read,read_at=excluded.read_at,is_recalled=excluded.is_recalled,
			recalled_at=excluded.recalled_at,message_json=excluded.message_json`, message.ID, stored.ConversationID, message.Kind,
			message.Sender, message.ClientID, message.Name, message.Text, message.TargetID, message.GroupID, message.Private,
			dbTime(message.SentAt), message.Read, dbTime(message.ReadAt), message.Recalled, dbTime(message.RecalledAt), string(value)); err != nil {
			return err
		}
		if message.AttachmentPath == "" {
			_, err := tx.tx.Exec("DELETE FROM files WHERE message_id=?", message.ID)
			return err
		}
		_, err := tx.tx.Exec(`INSERT INTO files(message_id,conversation_id,content_hash,storage_path,original_name,file_extension,mime_type,file_size,file_kind,sent_at)
			VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(message_id) DO UPDATE SET conversation_id=excluded.conversation_id,
			content_hash=excluded.content_hash,storage_path=excluded.storage_path,original_name=excluded.original_name,
			file_extension=excluded.file_extension,mime_type=excluded.mime_type,file_size=excluded.file_size,
			file_kind=excluded.file_kind,sent_at=excluded.sent_at`, message.ID, stored.ConversationID, message.AttachmentHash,
			message.AttachmentPath, message.FileName, filepath.Ext(message.FileName), message.Mime, message.FileSize, message.Kind, dbTime(message.SentAt))
		return err
	}
	return nil
}

func (tx *kvTx) syncBusinessDelete(bucket string, key, previous []byte) error {
	switch bucket {
	case string(bucketApp):
		if bytes.Equal(key, keyHTTPS) {
			_, err := tx.tx.Exec("DELETE FROM certificates WHERE id='server'")
			return err
		}
	case string(bucketAccounts):
		_, err := tx.tx.Exec("DELETE FROM users WHERE id=?", string(key))
		return err
	case string(bucketUsers):
		_, err := tx.tx.Exec("UPDATE users SET avatar_base64='',display_name='',profile_json='' WHERE id=?", string(key))
		return err
	case string(bucketClawBot):
		if _, err := tx.tx.Exec("DELETE FROM clawbot_bindings WHERE account_id=?", string(key)); err != nil {
			return err
		}
		_, err := tx.tx.Exec("UPDATE users SET clawbot_bound=0,clawbot_status='',clawbot_bot_id='',weixin_user_id='',clawbot_json='' WHERE id=?", string(key))
		return err
	case string(bucketMessages), string(bucketAttachments):
		var stored server.StoredChatMessage
		if json.Unmarshal(previous, &stored) == nil && stored.Message.ID != "" {
			if bucket == string(bucketMessages) {
				_, err := tx.tx.Exec("DELETE FROM chat_messages WHERE id=?", stored.Message.ID)
				return err
			}
			_, err := tx.tx.Exec("DELETE FROM files WHERE message_id=?", stored.Message.ID)
			return err
		}
	}
	return nil
}

func (tx *kvTx) clearBusinessTable(bucket string) error {
	statement := ""
	switch bucket {
	case string(bucketApp):
		statement = "DELETE FROM certificates"
	case string(bucketAccounts):
		statement = "DELETE FROM users"
	case string(bucketUsers):
		statement = "UPDATE users SET avatar_base64='',display_name='',profile_json=''"
	case string(bucketClawBot):
		if _, err := tx.tx.Exec("DELETE FROM clawbot_bindings"); err != nil {
			return err
		}
		statement = "UPDATE users SET clawbot_bound=0,clawbot_status='',clawbot_bot_id='',weixin_user_id='',clawbot_json=''"
	case string(bucketMessages):
		statement = "DELETE FROM chat_messages"
	case string(bucketAttachments):
		statement = "DELETE FROM files"
	}
	if statement == "" {
		return nil
	}
	_, err := tx.tx.Exec(statement)
	return err
}

func (tx *kvTx) syncCertificate(value []byte) error {
	var bundle CertificateBundle
	if err := json.Unmarshal(value, &bundle); err != nil {
		return err
	}
	var ips, dnsNames, serial, notBefore, notAfter string
	if block, _ := pem.Decode(bundle.CertPEM); block != nil {
		if certificate, err := x509.ParseCertificate(block.Bytes); err == nil {
			ipValues := make([]string, 0, len(certificate.IPAddresses))
			for _, address := range certificate.IPAddresses {
				ipValues = append(ipValues, address.String())
			}
			ips = strings.Join(ipValues, ",")
			dnsNames = strings.Join(certificate.DNSNames, ",")
			serial = certificate.SerialNumber.String()
			notBefore = dbTime(certificate.NotBefore)
			notAfter = dbTime(certificate.NotAfter)
		}
	}
	_, err := tx.tx.Exec(`INSERT INTO certificates(id,ip,dns_names,serial_number,ca_certificate_pem,ca_private_key_pem,
		server_certificate_pem,server_private_key_pem,not_before,not_after,updated_at) VALUES('server',?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET ip=excluded.ip,dns_names=excluded.dns_names,serial_number=excluded.serial_number,
		ca_certificate_pem=excluded.ca_certificate_pem,ca_private_key_pem=excluded.ca_private_key_pem,
		server_certificate_pem=excluded.server_certificate_pem,server_private_key_pem=excluded.server_private_key_pem,
		not_before=excluded.not_before,not_after=excluded.not_after,updated_at=excluded.updated_at`, ips, dnsNames, serial,
		bundle.CACertPEM, bundle.CAKeyPEM, bundle.CertPEM, bundle.KeyPEM, notBefore, notAfter, dbTime(bundle.UpdatedAt))
	return err
}

func dbTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func attachmentExtension(name, mimeType string) string {
	extension := strings.ToLower(filepath.Ext(filepath.Base(strings.TrimSpace(name))))
	if len(extension) > 1 && len(extension) <= 20 {
		valid := true
		for _, char := range extension[1:] {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
				valid = false
				break
			}
		}
		if valid {
			return extension
		}
	}
	return extensionForMIME(mimeType)
}

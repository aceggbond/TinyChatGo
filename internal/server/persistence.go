package server

import (
	"io"
	"time"
)

// ChatUser is the persistent chat profile attached to one registered account.
// IP is retained as the serialized legacy field name, but now contains the
// stable account ID. The real network address lives in Client.IP.
type ChatUser struct {
	IP          string         `json:"ip"`
	Username    string         `json:"username,omitempty"`
	Name        string         `json:"name,omitempty"`
	Avatar      string         `json:"avatar,omitempty"`
	Blacklisted bool           `json:"blacklisted,omitempty"`
	FirstSeen   time.Time      `json:"firstSeen"`
	LastSeen    time.Time      `json:"lastSeen"`
	Client      ChatClientInfo `json:"client"`
}

// ChatGroup is a user-created conversation. Members are stable account IDs;
// the owner can rename or dissolve the group.
type ChatGroup struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OwnerIP   string    `json:"ownerIp"`
	Members   []string  `json:"members"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ChatPublicGroup is the browser-safe view of a user-created group.
type ChatPublicGroup struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	OwnerIP     string   `json:"ownerIp"`
	Members     []string `json:"members,omitempty"`
	MemberCount int      `json:"memberCount"`
	Joined      bool     `json:"joined"`
	Owner       bool     `json:"owner"`
}

// ChatPublicUser is the small, non-sensitive user record sent to browsers.
type ChatPublicUser struct {
	IP         string `json:"ip"`
	Username   string `json:"username,omitempty"`
	Address    string `json:"address,omitempty"`
	Port       string `json:"port,omitempty"`
	Name       string `json:"name"`
	Alias      string `json:"alias,omitempty"`
	Avatar     string `json:"avatar,omitempty"`
	Remark     string `json:"remark,omitempty"`
	SearchKey  string `json:"searchKey,omitempty"`
	Browser    string `json:"browser,omitempty"`
	OS         string `json:"os,omitempty"`
	ClientType string `json:"clientType,omitempty"`
	Online     bool   `json:"online"`
	Me         bool   `json:"me,omitempty"`
}

// ChatRemark is a private contact name. Each account owns its own remarks, so
// changing a contact does not rename that person for everybody else.
type ChatRemark struct {
	OwnerIP  string    `json:"ownerIp"`
	TargetIP string    `json:"targetIp"`
	Name     string    `json:"name"`
	Updated  time.Time `json:"updated"`
}

// StoredChatMessage adds the owning conversation ID to a persisted message.
type StoredChatMessage struct {
	ConversationID string      `json:"conversationId"`
	Message        ChatMessage `json:"message"`
}

// AccessRecord is the structured, persistent HTTP/chat activity record.
type AccessRecord struct {
	ID        uint64    `json:"id"`
	At        time.Time `json:"at"`
	IP        string    `json:"ip"`
	AccountID string    `json:"accountId,omitempty"`
	Username  string    `json:"username,omitempty"`
	Operation string    `json:"operation"`
	Method    string    `json:"method,omitempty"`
	Path      string    `json:"path,omitempty"`
	Status    int       `json:"status,omitempty"`
	Duration  string    `json:"duration,omitempty"`
}

// ChatAttachment describes a persisted chat image/file. Path is deliberately
// omitted: callers receive a reader rather than a filesystem location.
type ChatAttachment struct {
	MessageID      string
	ConversationID string
	Name           string
	MIME           string
	Size           int64
	ModTime        time.Time
	Reader         io.ReadCloser
}

// ChatArchiveQuery selects one page of persisted chat attachments.
type ChatArchiveQuery struct {
	ViewerIP       string
	ConversationID string
	GroupMembers   map[string][]string
	Query          string
	Kind           string
	From           time.Time
	To             time.Time
	Page           int
	PageSize       int
}

// ChatArchiveItem is safe attachment metadata used by both the browser
// archive and the desktop management window.
type ChatArchiveItem struct {
	MessageID      string    `json:"messageId"`
	ConversationID string    `json:"conversationId"`
	Kind           string    `json:"kind"`
	Name           string    `json:"name"`
	MIME           string    `json:"mime,omitempty"`
	Size           int64     `json:"size"`
	SentAt         time.Time `json:"sentAt"`
	SenderIP       string    `json:"senderIp"`
	SenderName     string    `json:"senderName,omitempty"`
	TargetID       string    `json:"targetId,omitempty"`
	Private        bool      `json:"private,omitempty"`
	FileURL        string    `json:"fileUrl"`
}

type ChatArchivePage struct {
	Items    []ChatArchiveItem `json:"items"`
	Page     int               `json:"page"`
	PageSize int               `json:"pageSize"`
	Total    int               `json:"total"`
	Pages    int               `json:"pages"`
}

// ChatHistoryQuery selects one page of persisted text messages.
type ChatHistoryQuery struct {
	ViewerIP       string
	ConversationID string
	GroupMembers   map[string][]string
	Query          string
	From           time.Time
	To             time.Time
	Page           int
	PageSize       int
}

type ChatHistoryItem struct {
	MessageID      string    `json:"messageId"`
	ConversationID string    `json:"conversationId"`
	Kind           string    `json:"kind"`
	Text           string    `json:"text"`
	SentAt         time.Time `json:"sentAt"`
	SenderIP       string    `json:"senderIp"`
	SenderName     string    `json:"senderName,omitempty"`
	TargetID       string    `json:"targetId,omitempty"`
	Private        bool      `json:"private,omitempty"`
}

type ChatHistoryPage struct {
	Items    []ChatHistoryItem `json:"items"`
	Page     int               `json:"page"`
	PageSize int               `json:"pageSize"`
	Total    int               `json:"total"`
	Pages    int               `json:"pages"`
}

// Persistence is implemented by the desktop database. Tests and embedders may
// leave it unset; the server then keeps the historical in-memory behaviour.
type Persistence interface {
	LoadChatState() ([]ChatUser, []StoredChatMessage, error)
	LoadChatRemarks() ([]ChatRemark, error)
	SaveChatUser(ChatUser) error
	SaveChatRemark(ChatRemark) error
	DeleteChatRemark(string, string) error
	DeleteChatUser(string) error
	ClearChatUsers() error
	ClearChatRemarks() error

	SaveChatMessage(string, ChatMessage) (ChatMessage, error)
	SaveChatAttachment(string, ChatMessage, io.Reader, int64) (ChatMessage, error)
	RecallChatMessage(string, time.Time) (StoredChatMessage, error)
	DeleteChatMessage(string) error
	DeleteChatConversation(string) error
	ClearChatMessages() error
	OpenChatAttachment(string) (ChatAttachment, error)
	ListChatAttachments(ChatArchiveQuery) (ChatArchivePage, error)

	SaveAccessRecord(AccessRecord) error
	ListAccessRecords(int) ([]AccessRecord, error)
	ClearAccessRecords() error
}

// ChatGroupPersistence is an optional extension implemented by the desktop
// database. Keeping it optional preserves the in-memory server API for tests
// and embedders that do not need durable user-created groups.
type ChatGroupPersistence interface {
	LoadChatGroups() ([]ChatGroup, error)
	SaveChatGroup(ChatGroup) error
	DeleteChatGroup(string) error
}

// ChatReadPersistence is an optional extension for durable read receipts.
// Keeping it separate avoids forcing in-memory embedders to implement it.
type ChatReadPersistence interface {
	MarkChatMessagesRead([]string, time.Time) error
}

// ChatHistoryPersistence is an optional durable text-message search index.
type ChatHistoryPersistence interface {
	ListChatMessages(ChatHistoryQuery) (ChatHistoryPage, error)
}

type AccountStatus string

const (
	AccountStatusPending  AccountStatus = "pending"
	AccountStatusActive   AccountStatus = "active"
	AccountStatusRejected AccountStatus = "rejected"
	AccountStatusDisabled AccountStatus = "disabled"
)

// Account is the durable login identity. PasswordHash is an Argon2id PHC
// string and must never be returned by browser or administrator APIs.
type Account struct {
	ID           string        `json:"id"`
	Username     string        `json:"username"`
	PasswordHash string        `json:"passwordHash"`
	Status       AccountStatus `json:"status"`
	CreatedAt    time.Time     `json:"createdAt"`
	UpdatedAt    time.Time     `json:"updatedAt"`
	ApprovedAt   time.Time     `json:"approvedAt,omitempty"`
	LastLoginAt  time.Time     `json:"lastLoginAt,omitempty"`
	LastIP       string        `json:"lastIp,omitempty"`
}

type AccountSession struct {
	TokenHash string    `json:"tokenHash"`
	AccountID string    `json:"accountId"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	LastSeen  time.Time `json:"lastSeen"`
	LastIP    string    `json:"lastIp,omitempty"`
}

type AccountSummary struct {
	ID          string        `json:"id"`
	Username    string        `json:"username"`
	Status      AccountStatus `json:"status"`
	CreatedAt   time.Time     `json:"createdAt"`
	ApprovedAt  time.Time     `json:"approvedAt,omitempty"`
	LastLoginAt time.Time     `json:"lastLoginAt,omitempty"`
	LastIP      string        `json:"lastIp,omitempty"`
}

// AccountPersistence is an optional durable account/session extension.
// Keeping it separate preserves small in-memory server tests.
type AccountPersistence interface {
	LoadAccounts() ([]Account, error)
	SaveAccount(Account) error
	DeleteAccount(string) error
	LoadAccountSessions() ([]AccountSession, error)
	SaveAccountSession(AccountSession) error
	DeleteAccountSession(string) error
	DeleteAccountSessions(string) error
}

// ClawBotBinding contains the durable state of one TinyChatGo account's
// official Weixin ClawBot connection. BotToken is a credential and must never
// be returned by browser or administrator APIs.
type ClawBotBinding struct {
	AccountID      string           `json:"accountId"`
	Status         string           `json:"status"`
	BotToken       string           `json:"botToken,omitempty"`
	BotID          string           `json:"botId,omitempty"`
	WeixinUserID   string           `json:"weixinUserId,omitempty"`
	BaseURL        string           `json:"baseUrl,omitempty"`
	QRCode         string           `json:"qrCode,omitempty"`
	QRCodeURL      string           `json:"qrCodeUrl,omitempty"`
	QRExpiresAt    time.Time        `json:"qrExpiresAt,omitempty"`
	UpdatesBuffer  string           `json:"updatesBuffer,omitempty"`
	ContextToken   string           `json:"contextToken,omitempty"`
	ForwardEnabled bool             `json:"forwardEnabled"`
	BoundAt        time.Time        `json:"boundAt,omitempty"`
	UpdatedAt      time.Time        `json:"updatedAt"`
	LastMessageAt  time.Time        `json:"lastMessageAt,omitempty"`
	LastError      string           `json:"lastError,omitempty"`
	Messages       []ClawBotMessage `json:"messages,omitempty"`
}

type ClawBotMessage struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Text     string    `json:"text,omitempty"`
	FileName string    `json:"fileName,omitempty"`
	FileURL  string    `json:"fileUrl,omitempty"`
	MIME     string    `json:"mime,omitempty"`
	FileSize int64     `json:"fileSize,omitempty"`
	Mine     bool      `json:"mine"`
	SentAt   time.Time `json:"sentAt"`
}

// ClawBotPersistence is optional so small embedders remain source compatible.
type ClawBotPersistence interface {
	LoadClawBotBindings() ([]ClawBotBinding, error)
	SaveClawBotBinding(ClawBotBinding) error
	DeleteClawBotBinding(string) error
}

package server

import (
	"io"
	"time"
)

// ChatUser is the persistent identity attached to one canonical IP address.
// Name is an optional user/admin supplied alias; IP remains the immutable ID.
type ChatUser struct {
	IP          string         `json:"ip"`
	Name        string         `json:"name,omitempty"`
	Blacklisted bool           `json:"blacklisted,omitempty"`
	FirstSeen   time.Time      `json:"firstSeen"`
	LastSeen    time.Time      `json:"lastSeen"`
	Client      ChatClientInfo `json:"client"`
}

// ChatPublicUser is the small, non-sensitive user record sent to browsers.
type ChatPublicUser struct {
	IP        string `json:"ip"`
	Name      string `json:"name"`
	Alias     string `json:"alias,omitempty"`
	SearchKey string `json:"searchKey,omitempty"`
	Online    bool   `json:"online"`
	Me        bool   `json:"me,omitempty"`
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

// Persistence is implemented by the desktop database. Tests and embedders may
// leave it unset; the server then keeps the historical in-memory behaviour.
type Persistence interface {
	LoadChatState() ([]ChatUser, []StoredChatMessage, error)
	SaveChatUser(ChatUser) error
	DeleteChatUser(string) error
	ClearChatUsers() error

	SaveChatMessage(string, ChatMessage) (ChatMessage, error)
	SaveChatAttachment(string, ChatMessage, io.Reader, int64) (ChatMessage, error)
	DeleteChatMessage(string) error
	DeleteChatConversation(string) error
	ClearChatMessages() error
	OpenChatAttachment(string) (ChatAttachment, error)

	SaveAccessRecord(AccessRecord) error
	ListAccessRecords(int) ([]AccessRecord, error)
	ClearAccessRecords() error
}

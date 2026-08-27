package server

import (
	"testing"
	"time"
)

func TestDirectMessageToOfflineRegisteredUserIsQueuedInHistory(t *testing.T) {
	h := newChatHub()
	h.enabled = true
	h.userListEnabled = true
	h.privateEnabled = true
	senderID := "u_0123456789abcdef0123456789abcdef"
	targetID := "u_fedcba9876543210fedcba9876543210"
	sender := &chatPeer{id: "sender", ip: senderID, send: make(chan chatWireMessage, 4)}
	h.peers[sender.id] = sender
	h.users[senderID] = &ChatUser{IP: senderID, Name: "发送者"}
	h.users[targetID] = &ChatUser{IP: targetID, Name: "离线接收者"}

	message := ChatMessage{ID: "offline-message", Kind: ChatMessageKindText, Sender: "user", Text: "离线消息", SentAt: time.Now().UTC()}
	if err := h.receiveDirectMessage(sender, targetID, message); err != nil {
		t.Fatalf("send to offline user: %v", err)
	}
	frame := <-sender.send
	if frame.ID != message.ID || frame.TargetID != targetID {
		t.Fatalf("sender frame = %#v", frame)
	}
	history := h.directHistoryForPeerLocked(targetID)
	items := history[senderID]
	if len(items) != 1 || items[0].ID != message.ID || items[0].Text != message.Text {
		t.Fatalf("offline history = %#v", history)
	}
}

func TestRegisteredOfflineUsersRemainVisibleAsMessageTargets(t *testing.T) {
	h := newChatHub()
	h.userListEnabled = true
	currentID := "u_0123456789abcdef0123456789abcdef"
	offlineID := "u_fedcba9876543210fedcba9876543210"
	h.users[currentID] = &ChatUser{IP: currentID, Name: "当前用户"}
	h.users[offlineID] = &ChatUser{IP: offlineID, Name: "离线用户"}

	users := h.publicUsersLocked(currentID)
	for _, user := range users {
		if user.IP == offlineID {
			if user.Online {
				t.Fatal("offline user marked online")
			}
			return
		}
	}
	t.Fatal("offline registered user missing from contacts")
}

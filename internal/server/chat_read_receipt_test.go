package server

import (
	"testing"
	"time"
)

func TestChatReadReceiptBroadcastsAndMarksMessage(t *testing.T) {
	h := newChatHub()
	h.enabled = true
	h.userListEnabled = true
	h.privateEnabled = true
	sender := &chatPeer{
		id:   "sender",
		ip:   "10.0.0.10",
		send: make(chan chatWireMessage, 8),
	}
	reader := &chatPeer{
		id:   "reader",
		ip:   "10.0.0.11",
		send: make(chan chatWireMessage, 8),
	}
	observer := &chatPeer{
		id:   "observer",
		ip:   "10.0.0.12",
		send: make(chan chatWireMessage, 2),
	}
	h.peers[sender.id] = sender
	h.peers[reader.id] = reader
	h.peers[observer.id] = observer
	h.users[sender.ip] = &ChatUser{IP: sender.ip, Name: "发送者"}
	h.users[reader.ip] = &ChatUser{IP: reader.ip, Name: "接收者"}

	if err := h.receiveDirectMessage(sender, reader.ip, ChatMessage{
		ID:     "receipt-message",
		Kind:   ChatMessageKindText,
		Sender: "user",
		Text:   "需要已读回执",
		SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("send message: %v", err)
	}
	if first := <-sender.send; first.Type != "message" || !first.Receipt {
		t.Fatalf("sender wire = %#v", first)
	}
	if first := <-reader.send; first.Type != "message" || !first.Receipt {
		t.Fatalf("reader wire = %#v", first)
	}

	if err := h.markMessagesRead(reader, "10.0.0.99", []string{"receipt-message"}); err != nil {
		t.Fatalf("mark message through wrong conversation: %v", err)
	}
	for name, queue := range map[string]chan chatWireMessage{
		"sender": sender.send,
		"reader": reader.send,
	} {
		select {
		case frame := <-queue:
			t.Fatalf("%s received read frame for wrong conversation: %#v", name, frame)
		default:
		}
	}
	conversation := h.direct[directConversationID(sender.ip, reader.ip)]
	if conversation == nil || len(conversation.messages) != 1 || conversation.messages[0].Read {
		t.Fatal("wrong conversation marked the private message read")
	}

	if err := h.markMessagesRead(reader, sender.ip, []string{"receipt-message"}); err != nil {
		t.Fatalf("mark message while system group is active: %v", err)
	}
	select {
	case frame := <-sender.send:
		t.Fatalf("inactive private conversation emitted read frame: %#v", frame)
	default:
	}
	if conversation.messages[0].Read {
		t.Fatal("message was marked read without opening its private conversation")
	}

	h.updatePeerView(reader, sender.ip)
	h.updatePeerView(reader, "")
	if err := h.markMessagesRead(reader, sender.ip, []string{"receipt-message"}); err != nil {
		t.Fatalf("mark message after returning to system group: %v", err)
	}
	select {
	case frame := <-sender.send:
		t.Fatalf("system-group switch emitted stale read frame: %#v", frame)
	default:
	}
	if conversation.messages[0].Read {
		t.Fatal("stale private view marked the message read after switching to system group")
	}

	h.updatePeerView(reader, sender.ip)
	if err := h.markMessagesRead(reader, sender.ip, []string{"receipt-message"}); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	frame := <-sender.send
	if frame.Type != "read" || frame.ClientID != sender.ip || frame.TargetID != reader.ip ||
		len(frame.IDs) != 1 || frame.IDs[0] != "receipt-message" || frame.ReadAt.IsZero() {
		t.Fatalf("sender read frame = %#v", frame)
	}
	select {
	case frame = <-reader.send:
		t.Fatalf("reader unexpectedly received its own read frame: %#v", frame)
	default:
	}
	select {
	case frame := <-observer.send:
		t.Fatalf("unrelated peer received private read frame: %#v", frame)
	default:
	}
	if conversation == nil || len(conversation.messages) != 1 ||
		!conversation.messages[0].Read || conversation.messages[0].ReadAt.IsZero() {
		t.Fatal("message was not marked read in memory")
	}
}

func TestGroupMessagesNeverRequestReadReceipts(t *testing.T) {
	h := newChatHub()
	h.enabled = true
	h.groupEnabled = true
	sender := &chatPeer{id: "sender", ip: "10.0.0.20", send: make(chan chatWireMessage, 4)}
	reader := &chatPeer{id: "reader", ip: "10.0.0.21", send: make(chan chatWireMessage, 4)}
	h.peers[sender.id] = sender
	h.peers[reader.id] = reader

	if err := h.receivePeerChatMessage(sender, ChatMessage{
		ID:     "group-message",
		Kind:   ChatMessageKindText,
		Sender: "user",
		Text:   "系统群不显示已读",
		SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("send group message: %v", err)
	}
	for name, queue := range map[string]chan chatWireMessage{
		"sender": sender.send,
		"reader": reader.send,
	} {
		frame := <-queue
		if frame.Type != "message" || frame.Receipt || frame.Read {
			t.Fatalf("%s group frame = %#v", name, frame)
		}
	}
	if err := h.markMessagesRead(reader, sender.ip, []string{"group-message"}); err != nil {
		t.Fatalf("mark group message: %v", err)
	}
	select {
	case frame := <-sender.send:
		t.Fatalf("group message unexpectedly emitted read frame: %#v", frame)
	default:
	}
}

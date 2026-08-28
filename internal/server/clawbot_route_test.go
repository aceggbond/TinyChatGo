package server

import (
	"testing"

	"tinychatgo/internal/clawbot"
)

func TestUpdateClawBotReplyContextPairsInboundPeerWithFreshContext(t *testing.T) {
	binding := &ClawBotBinding{
		BotID:        "bot-id",
		WeixinUserID: "provisional-user-id",
		ContextToken: "old-context",
	}
	updateClawBotReplyContext(binding, clawbot.Message{
		MessageType:  1,
		FromUserID:   "actual-weixin-user",
		ContextToken: "fresh-context",
	})
	if binding.WeixinUserID != "provisional-user-id" || binding.ReplyUserID != "actual-weixin-user" {
		t.Fatalf("binding route = %#v", binding)
	}
	if binding.ContextToken != "fresh-context" {
		t.Fatalf("context token = %q", binding.ContextToken)
	}
}

func TestUpdateClawBotReplyContextDoesNotTargetBotItself(t *testing.T) {
	binding := &ClawBotBinding{BotID: "bot-id", WeixinUserID: "owner-id"}
	updateClawBotReplyContext(binding, clawbot.Message{MessageType: 1, FromUserID: "bot-id", ContextToken: "context"})
	if binding.WeixinUserID != "owner-id" || binding.ReplyUserID != "" {
		t.Fatalf("reply peer changed to bot ID: %#v", binding)
	}
	if binding.ContextToken != "context" {
		t.Fatalf("context token = %q", binding.ContextToken)
	}
}

func TestUpdateClawBotReplyContextIgnoresSystemAndOutboundEvents(t *testing.T) {
	binding := &ClawBotBinding{BotID: "bot-id", WeixinUserID: "owner-id", ReplyUserID: "reply-owner", ContextToken: "valid-context"}
	for _, messageType := range []int{0, 2, 3} {
		updateClawBotReplyContext(binding, clawbot.Message{
			MessageType:  messageType,
			FromUserID:   "receipt-or-system-id",
			ContextToken: "invalid-context",
		})
	}
	if binding.WeixinUserID != "owner-id" || binding.ReplyUserID != "reply-owner" || binding.ContextToken != "valid-context" {
		t.Fatalf("non-inbound event corrupted reply route: %#v", binding)
	}
}

func TestClawBotSendTargetUsesPeerPairedWithContext(t *testing.T) {
	binding := ClawBotBinding{BotID: "bot-id", WeixinUserID: "qr-user", ReplyUserID: "inbound-user", ContextToken: "fresh-context"}
	if target := clawBotSendTarget(binding); target != "inbound-user" {
		t.Fatalf("send target = %q", target)
	}
	binding.ReplyUserID = ""
	if target := clawBotSendTarget(binding); target != "qr-user" {
		t.Fatalf("QR fallback target = %q", target)
	}
}

package server

import (
	"testing"

	"tinychatgo/internal/clawbot"
)

func TestUpdateClawBotReplyContextKeepsQRConfirmedWeixinPeer(t *testing.T) {
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
	if binding.WeixinUserID != "provisional-user-id" {
		t.Fatalf("reply peer = %q", binding.WeixinUserID)
	}
	if binding.ContextToken != "fresh-context" {
		t.Fatalf("context token = %q", binding.ContextToken)
	}
}

func TestUpdateClawBotReplyContextDoesNotTargetBotItself(t *testing.T) {
	binding := &ClawBotBinding{BotID: "bot-id", WeixinUserID: "owner-id"}
	updateClawBotReplyContext(binding, clawbot.Message{MessageType: 1, FromUserID: "bot-id", ContextToken: "context"})
	if binding.WeixinUserID != "owner-id" {
		t.Fatalf("reply peer changed to bot ID: %q", binding.WeixinUserID)
	}
	if binding.ContextToken != "context" {
		t.Fatalf("context token = %q", binding.ContextToken)
	}
}

func TestUpdateClawBotReplyContextIgnoresSystemAndOutboundEvents(t *testing.T) {
	binding := &ClawBotBinding{BotID: "bot-id", WeixinUserID: "owner-id", ContextToken: "valid-context"}
	for _, messageType := range []int{0, 2, 3} {
		updateClawBotReplyContext(binding, clawbot.Message{
			MessageType:  messageType,
			FromUserID:   "receipt-or-system-id",
			ContextToken: "invalid-context",
		})
	}
	if binding.WeixinUserID != "owner-id" || binding.ContextToken != "valid-context" {
		t.Fatalf("non-inbound event corrupted reply route: %#v", binding)
	}
}

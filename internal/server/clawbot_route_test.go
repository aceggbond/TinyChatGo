package server

import (
	"testing"

	"tinychatgo/internal/clawbot"
)

func TestUpdateClawBotReplyRouteUsesIncomingWeixinPeer(t *testing.T) {
	binding := &ClawBotBinding{
		BotID:        "bot-id",
		WeixinUserID: "provisional-user-id",
		ContextToken: "old-context",
	}
	updateClawBotReplyRoute(binding, clawbot.Message{
		MessageType:  1,
		FromUserID:   "actual-weixin-user",
		ContextToken: "fresh-context",
	})
	if binding.WeixinUserID != "actual-weixin-user" {
		t.Fatalf("reply peer = %q", binding.WeixinUserID)
	}
	if binding.ContextToken != "fresh-context" {
		t.Fatalf("context token = %q", binding.ContextToken)
	}
}

func TestUpdateClawBotReplyRouteDoesNotTargetBotItself(t *testing.T) {
	binding := &ClawBotBinding{BotID: "bot-id", WeixinUserID: "owner-id"}
	updateClawBotReplyRoute(binding, clawbot.Message{MessageType: 1, FromUserID: "bot-id", ContextToken: "context"})
	if binding.WeixinUserID != "owner-id" {
		t.Fatalf("reply peer changed to bot ID: %q", binding.WeixinUserID)
	}
	if binding.ContextToken != "context" {
		t.Fatalf("context token = %q", binding.ContextToken)
	}
}

func TestUpdateClawBotReplyRouteIgnoresSystemAndOutboundEvents(t *testing.T) {
	binding := &ClawBotBinding{BotID: "bot-id", WeixinUserID: "owner-id", ContextToken: "valid-context"}
	for _, messageType := range []int{0, 2, 3} {
		updateClawBotReplyRoute(binding, clawbot.Message{
			MessageType:  messageType,
			FromUserID:   "receipt-or-system-id",
			ContextToken: "invalid-context",
		})
	}
	if binding.WeixinUserID != "owner-id" || binding.ContextToken != "valid-context" {
		t.Fatalf("non-inbound event corrupted reply route: %#v", binding)
	}
}

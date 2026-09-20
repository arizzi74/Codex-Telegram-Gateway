package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestLastMessagesDisplaysBothRolesWithSessionAndPrivatePaging(t *testing.T) {
	stamp := time.Date(2026, 9, 20, 10, 11, 12, 0, time.UTC)
	page := &protocol.HistoryPage{Limit: 2, Conversation: true, Messages: []protocol.HistoryMessage{
		{TurnID: "turn", ItemID: "user", Role: "user", Text: "/status is saved Telegram input", Timestamp: &stamp},
		{TurnID: "turn", ItemID: "answer", Role: "assistant", Text: "SECRET_HISTORY_VALUE is secret", Truncated: true},
	}, Next: &protocol.HistoryCursor{TurnID: "turn", ItemID: "user"}}
	store, row := historyRenderFixture(t, page)
	redactor, err := auth.NewRedactor([]string{"SECRET_HISTORY_VALUE"}, "")
	if err != nil {
		t.Fatal(err)
	}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42, Redactor: redactor})
	parts, keyboard, err := sender.renderDeliveryParts(t.Context(), row)
	if err != nil || len(parts) != 2 || !strings.Contains(parts[0], "auth-fix · 👤 You") || !strings.Contains(parts[1], "auth-fix · 🤖 Codex") || !strings.Contains(parts[0], "/status is saved") || !strings.Contains(parts[1], "shortened") {
		t.Fatalf("conversation parts=%#v err=%v", parts, err)
	}
	if strings.Contains(strings.Join(parts, ""), "SECRET_HISTORY_VALUE") {
		t.Fatal("conversation bypassed redaction")
	}
	if !strings.Contains(parts[0], "Turn time: 2026-09-20 10:11:12 UTC") || !strings.Contains(parts[1], "Turn date/time unavailable") {
		t.Fatalf("missing persisted or unavailable timestamp: %#v", parts)
	}
	if keyboard == nil || keyboard.Rows[0][0].Text != "Older messages" || len(store.callbacks) != 1 {
		t.Fatal("missing conversation paging")
	}
	callback := store.callbacks[0]
	if callback.Action != "history" || callback.UserID != 77 || callback.SessionID != testSessionID || callback.RuntimeID != testRuntimeID || callback.Generation != 7 || callback.History == nil || !callback.History.Messages || callback.History.Before == nil || *callback.History.Before != *page.Next || callback.History.Limit != 2 {
		t.Fatalf("paging lost scope or conversation mode: %#v", callback)
	}
}

func TestLastMessagesDefaultReplyIsOneMessageAndEmptyViewIsExplicit(t *testing.T) {
	for _, empty := range []bool{false, true} {
		page := &protocol.HistoryPage{Limit: 1, Conversation: true}
		if !empty {
			page.Messages = []protocol.HistoryMessage{{TurnID: "turn", ItemID: "answer", Role: "assistant", Text: "Most recent answer"}}
		}
		store, row := historyRenderFixture(t, page)
		parts, keyboard, err := NewSender(store, nil, nil).renderDeliveryParts(t.Context(), row)
		if err != nil || len(parts) != 1 || keyboard != nil {
			t.Fatalf("default last message=%#v %v", parts, err)
		}
		if empty && !strings.Contains(parts[0], "No saved conversation") {
			t.Fatal("empty view did not explain absence")
		}
		if !empty && !strings.Contains(parts[0], "Most recent answer") {
			t.Fatal("last answer missing")
		}
	}
}

func TestHistoryConversationDisplaysNewestFirstAndKeepsHistoryPaging(t *testing.T) {
	stamp := time.Date(2026, 9, 20, 20, 11, 12, 0, time.UTC)
	page := &protocol.HistoryPage{Limit: 2, Conversation: true, NewestFirst: true, Messages: []protocol.HistoryMessage{
		{TurnID: "latest-turn", ItemID: "answer", Role: "assistant", Text: "Most recent Codex answer", Timestamp: &stamp, TimestampSource: "message"},
		{TurnID: "latest-turn", ItemID: "telegram-prompt", Role: "user", Text: "Recent Telegram question"},
	}, Next: &protocol.HistoryCursor{TurnID: "latest-turn", ItemID: "telegram-prompt"}}
	store, row := historyRenderFixture(t, page)
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
	parts, keyboard, err := sender.renderDeliveryParts(t.Context(), row)
	if err != nil || len(parts) != 2 || !strings.Contains(parts[0], "History · auth-fix · 🤖 Codex") || !strings.Contains(parts[0], "Most recent Codex answer") || !strings.Contains(parts[1], "Recent Telegram question") {
		t.Fatalf("newest conversation history=%#v, %v", parts, err)
	}
	if strings.Contains(strings.Join(parts, ""), "omitted") || keyboard == nil || keyboard.Rows[0][0].Text != "Older messages" || len(store.callbacks) != 1 {
		t.Fatalf("unexpected history explanation or paging: %#v, %#v", parts, keyboard)
	}
	if !strings.Contains(parts[0], "Message time: 2026-09-20 20:11:12 UTC") {
		t.Fatalf("verified message timestamp lost its source: %q", parts[0])
	}
	callback := store.callbacks[0]
	if callback.Action != "history" || callback.History == nil || !callback.History.Messages || !callback.History.NewestFirst || callback.History.Limit != 2 || callback.History.Before == nil || *callback.History.Before != *page.Next {
		t.Fatalf("history paging lost newest-first mode: %#v", callback)
	}

	page.Next = &protocol.HistoryCursor{TurnID: "latest-turn", ItemID: "answer"}
	_, row = historyRenderFixture(t, page)
	if _, _, err := sender.renderDeliveryParts(t.Context(), row); err == nil {
		t.Fatal("newest message incorrectly accepted as older-history cursor")
	}
}

func TestLastMessagesRenderRejectsMalformedPages(t *testing.T) {
	for _, scenario := range []string{"role", "duplicate", "overlimit", "mixed", "oversize", "cursor"} {
		t.Run(scenario, func(t *testing.T) {
			page := &protocol.HistoryPage{Limit: 2, Conversation: true, Messages: []protocol.HistoryMessage{{TurnID: "turn", ItemID: "answer", Role: "assistant", Text: "answer"}}}
			switch scenario {
			case "role":
				page.Messages[0].Role = "tool"
			case "duplicate":
				page.Messages = append(page.Messages, page.Messages[0])
			case "overlimit":
				page.Limit = 0
			case "mixed":
				page.Prompts = []protocol.HistoryPrompt{{TurnID: "turn", ItemID: "user", Text: "input"}}
			case "oversize":
				page.Messages[0].Text = strings.Repeat("x", 16001)
			case "cursor":
				page.Next = &protocol.HistoryCursor{TurnID: "turn", ItemID: "wrong"}
			}
			store, row := historyRenderFixture(t, page)
			if _, _, err := NewSender(store, nil, nil).renderDeliveryParts(t.Context(), row); err == nil {
				t.Fatal("accepted invalid conversation page")
			}
		})
	}
}

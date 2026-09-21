package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type historyRenderStore struct {
	*renderStoreFake
	requester int64
	commandID uuid.UUID
	chunks    []registry.DeliveryChunk
	routes    []string
}

func (s *historyRenderStore) HistoryRequester(_ context.Context, id uuid.UUID) (int64, error) {
	if id != s.commandID {
		return 0, registry.ErrTelegramTarget
	}
	return s.requester, nil
}
func (s *historyRenderStore) DeliveryChunks(context.Context, string) ([]registry.DeliveryChunk, error) {
	return s.chunks, nil
}
func (s *historyRenderStore) PrepareDeliveryChunks(_ context.Context, _ string, messages []json.RawMessage) ([]registry.DeliveryChunk, error) {
	for i, message := range messages {
		s.chunks = append(s.chunks, registry.DeliveryChunk{Index: i, Payload: message})
	}
	return s.chunks, nil
}
func (s *historyRenderStore) MarkDeliveryChunkSent(_ context.Context, _ string, index int, _ int64, sessionID, turnID, approvalID string, _ ...string) error {
	if turnID != "" || approvalID != "" {
		return errors.New("history was routed as a live turn or approval")
	}
	s.chunks[index].Sent = true
	s.routes = append(s.routes, sessionID)
	return nil
}

func historyRenderFixture(t *testing.T, page *protocol.HistoryPage) (*historyRenderStore, registry.Delivery) {
	t.Helper()
	store := &historyRenderStore{renderStoreFake: renderFixture(), requester: 77, commandID: uuid.New()}
	return store, eventRow(t, "command_completed", protocol.Result{CommandID: store.commandID.String(), History: page}, testSessionID.String())
}

func TestHistoryDisplaysSeparateUserPromptsAndRequesterScopedPaging(t *testing.T) {
	stamp := time.Date(2026, 9, 20, 14, 5, 6, 0, time.FixedZone("CEST", 2*60*60))
	page := &protocol.HistoryPage{
		Limit: 2, NewestFirst: true,
		Prompts: []protocol.HistoryPrompt{
			{TurnID: "turn-new", ItemID: "item-new", Text: "Token: SECRET_HISTORY_VALUE", Truncated: true, Timestamp: &stamp},
			{TurnID: "turn-old", ItemID: "item-old", Text: "/status is saved text"},
		},
		Next: &protocol.HistoryCursor{TurnID: "turn-old", ItemID: "item-old"},
	}
	store, row := historyRenderFixture(t, page)
	redactor, err := auth.NewRedactor([]string{"SECRET_HISTORY_VALUE"}, "")
	if err != nil {
		t.Fatal(err)
	}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42, Redactor: redactor})
	parts, keyboard, err := sender.renderDeliveryParts(t.Context(), row)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || !strings.Contains(parts[0], "auth-fix") || !strings.Contains(parts[0], "newest last") ||
		parts[1] != "👤 You · Codex\nTurn date/time unavailable\n\n/status is saved text" ||
		!strings.HasPrefix(parts[2], "👤 You · Codex\nTurn time: 2026-09-20 12:05:06 UTC\n\n") || !strings.Contains(parts[2], "shortened") {
		t.Fatalf("unexpected history messages: %#v", parts)
	}
	if strings.Contains(strings.Join(parts, ""), "SECRET_HISTORY_VALUE") {
		t.Fatal("history bypassed the configured redactor")
	}
	if keyboard == nil || len(store.callbacks) != 1 {
		t.Fatal("missing older-history control")
	}
	callback := store.callbacks[0]
	if callback.Action != "history" || callback.UserID != 77 || callback.SessionID != testSessionID || callback.RuntimeID != testRuntimeID ||
		callback.ChatID != row.ChatID || callback.TopicID != row.TopicID || callback.Generation != 7 || callback.History == nil ||
		callback.History.Before == nil || *callback.History.Before != *page.Next || callback.History.Limit != 2 {
		t.Fatalf("pagination lost frozen request context: %#v", callback)
	}
	if strings.Contains(keyboard.Rows[0][0].Data, page.Next.ItemID) || !strings.HasPrefix(keyboard.Rows[0][0].Data, "cb:") {
		t.Fatal("pagination sent raw cursor data instead of an opaque token")
	}
	if session, turn, approval := deliveryRoute(row); session != testSessionID.String() || turn != "" || approval != "" {
		t.Fatal("history acquired live turn routing")
	}
}

func TestHistoryRepeatsOriginalTimestampAcrossLongMessageChunks(t *testing.T) {
	stamp := time.Date(2026, 9, 20, 10, 11, 12, 0, time.UTC)
	text := strings.Repeat("Long saved prompt. ", 900)
	for _, source := range []string{"prompt", "turn", "message"} {
		conversation := source != "prompt"
		page := &protocol.HistoryPage{Limit: 1, Conversation: conversation}
		if conversation {
			page.Messages = []protocol.HistoryMessage{{TurnID: "turn", ItemID: "item", Role: "assistant", Text: text[:15000], Timestamp: &stamp}}
			if source == "message" {
				page.Messages[0].TimestampSource = source
			}
		} else {
			page.Prompts = []protocol.HistoryPrompt{{TurnID: "turn", ItemID: "item", Text: text, Timestamp: &stamp}}
		}
		store, row := historyRenderFixture(t, page)
		parts, _, err := NewSender(store, nil, nil).renderDeliveryParts(t.Context(), row)
		if err != nil || len(parts) < 3 {
			t.Fatalf("long history parts=%d, %v", len(parts), err)
		}
		if !conversation {
			parts = parts[1:]
		}
		prefix := "Turn time: "
		if source == "message" {
			prefix = "Message time: "
		}
		for _, part := range parts {
			if !strings.Contains(part, prefix+"2026-09-20 10:11:12 UTC") || telegramTextLength(part) > 4000 {
				t.Fatalf("history chunk lost timestamp or exceeds Telegram budget: %q", part[:100])
			}
		}
	}
}

func TestHistoryEmptyPageAndInvalidPage(t *testing.T) {
	store, row := historyRenderFixture(t, &protocol.HistoryPage{Limit: 10})
	sender := NewSender(store, nil, nil)
	parts, keyboard, err := sender.renderDeliveryParts(t.Context(), row)
	if err != nil || len(parts) != 1 || !strings.Contains(parts[0], "No saved Codex prompts") || keyboard != nil {
		t.Fatalf("empty page: %#v %v %v", parts, keyboard, err)
	}
	_, row = historyRenderFixture(t, &protocol.HistoryPage{Limit: 0})
	if _, _, err := sender.renderDeliveryParts(t.Context(), row); err == nil {
		t.Fatal("invalid page accepted")
	}
}

func TestHistoryLegacyWorkerPageKeepsOriginalOrder(t *testing.T) {
	page := &protocol.HistoryPage{Limit: 2, Prompts: []protocol.HistoryPrompt{
		{TurnID: "turn-1", ItemID: "old", Text: "Earlier input"},
		{TurnID: "turn-2", ItemID: "new", Text: "Latest input"},
	}}
	store, row := historyRenderFixture(t, page)
	parts, _, err := NewSender(store, nil, nil).renderDeliveryParts(t.Context(), row)
	if err != nil || len(parts) != 3 || !strings.HasSuffix(parts[1], "Earlier input") || !strings.HasSuffix(parts[2], "Latest input") {
		t.Fatalf("legacy worker page not rendered in original order: %#v, %v", parts, err)
	}
}

type historyDeliveryAPI struct {
	TelegramAPI
	calls    int
	messages []SendMessage
}

func (a *historyDeliveryAPI) Send(_ context.Context, message SendMessage) (int64, error) {
	a.calls++
	if a.calls == 3 {
		return 0, &TelegramError{Code: 429}
	}
	a.messages = append(a.messages, message)
	return int64(a.calls), nil
}

func TestHistoryDeliveryResumesAfterPartialPageWithoutRepeatingSentPrompts(t *testing.T) {
	store, row := historyRenderFixture(t, &protocol.HistoryPage{Limit: 2, Prompts: []protocol.HistoryPrompt{
		{TurnID: "turn-1", ItemID: "item-1", Text: "First saved prompt"},
		{TurnID: "turn-2", ItemID: "item-2", Text: "Second saved prompt"},
	}})
	api := &historyDeliveryAPI{}
	sender := NewSender(store, api, nil)
	if err := sender.sendDelivery(t.Context(), row); err == nil {
		t.Fatal("expected simulated Telegram rate limit")
	}
	if err := sender.sendDelivery(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if len(api.messages) != 3 || len(store.routes) != 3 || api.calls != 4 {
		t.Fatalf("sent history was repeated: calls=%d messages=%d routes=%d", api.calls, len(api.messages), len(store.routes))
	}
	for i, message := range api.messages {
		if message.ChatID != row.ChatID || message.TopicID != row.TopicID || store.routes[i] != testSessionID.String() {
			t.Fatal("history delivery changed destination")
		}
	}
}

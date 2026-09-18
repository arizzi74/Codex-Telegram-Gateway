package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/registry"
)

type checkpointStore struct {
	DeliveryStore
	chunks []registry.DeliveryChunk
	row    registry.Delivery
	delay  time.Duration
	marked []int
}

type sessionRepairStore struct {
	*checkpointStore
	repairs       int
	repairAttempt int
	repairError   error
}

func (s *sessionRepairStore) ReplaceUnsentSessionDeliveryChunks(_ context.Context, _ string, attempt int, messages []json.RawMessage) ([]registry.DeliveryChunk, error) {
	s.repairs++
	s.repairAttempt = attempt
	if s.repairError != nil {
		return nil, s.repairError
	}
	var retained []registry.DeliveryChunk
	nextIndex := 0
	for _, chunk := range s.chunks {
		if chunk.Sent {
			retained = append(retained, chunk)
			nextIndex = max(nextIndex, chunk.Index+1)
		}
	}
	for index, message := range messages {
		retained = append(retained, registry.DeliveryChunk{Index: nextIndex + index, Payload: message})
	}
	s.chunks = retained
	return s.chunks, nil
}

func oversizedSessionMessage(t *testing.T) json.RawMessage {
	t.Helper()
	message := SendMessage{ChatID: 99, Text: "old unpaginated sessions", Keyboard: &TelegramKeyboard{Rows: [][]TelegramButton{{{Text: strings.Repeat("x", 5000), Data: "old-token"}}}}}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSenderRepairsLegacySessionKeyboardWithoutReplayingSentChunks(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "entirely unsent", true: "partially sent"}[partial], func(t *testing.T) {
			row := uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String()})
			row.Attempt = 17
			store := &sessionRepairStore{checkpointStore: &checkpointStore{DeliveryStore: renderFixture(), row: row}}
			nextIndex := 0
			if partial {
				store.chunks = append(store.chunks, registry.DeliveryChunk{Index: 0, Sent: true, Payload: json.RawMessage(`{"chat_id":99,"text":"accepted historical text"}`)})
				nextIndex = 1
			}
			store.chunks = append(store.chunks, registry.DeliveryChunk{Index: nextIndex, Payload: oversizedSessionMessage(t)})
			api := &deliveryAPI{}
			if err := NewSender(store, api, nil, SenderOptions{BotID: "bot", OwnerID: 42}).flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if store.repairs != 1 || store.repairAttempt != row.Attempt || len(api.messages) != 1 || len(store.marked) != 1 || store.marked[0] != nextIndex {
				t.Fatalf("bad repaired delivery: repairs=%d attempt=%d messages=%d marked=%v", store.repairs, store.repairAttempt, len(api.messages), store.marked)
			}
			if strings.Contains(api.messages[0].Text, "old unpaginated") || !strings.Contains(api.messages[0].Text, "auth-fix") {
				t.Fatalf("wrong replacement text: %q", api.messages[0].Text)
			}
			if oversizedSessionDelivery(row, store.chunks) {
				t.Fatal("replacement keyboard is still oversized")
			}
			if partial && (!store.chunks[0].Sent || string(store.chunks[0].Payload) != `{"chat_id":99,"text":"accepted historical text"}`) {
				t.Fatal("sent checkpoint changed")
			}
		})
	}
}

func TestSenderDoesNotSendOversizedKeyboardWhenRepairFails(t *testing.T) {
	row := uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String()})
	failure := errors.New("repair lease changed")
	store := &sessionRepairStore{
		checkpointStore: &checkpointStore{DeliveryStore: renderFixture(), row: row, chunks: []registry.DeliveryChunk{{Payload: oversizedSessionMessage(t)}}},
		repairError:     failure,
	}
	api := &deliveryAPI{}
	if err := NewSender(store, api, nil, SenderOptions{BotID: "bot", OwnerID: 42}).flush(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("repair error = %v", err)
	}
	if len(api.messages) != 0 || len(store.marked) != 0 {
		t.Fatal("failed repair sent or checkpointed a message")
	}
}

func TestSenderDoesNotResetNewerClaimAfterSessionRepairIsFenced(t *testing.T) {
	row := uiRow(t, registry.AcceptResult{View: "sessions", RuntimeID: testRuntimeID.String()})
	store := &sessionRepairStore{
		checkpointStore: &checkpointStore{DeliveryStore: renderFixture(), row: row, chunks: []registry.DeliveryChunk{{Payload: oversizedSessionMessage(t)}}},
		repairError:     registry.ErrDeliveryLeaseChanged,
	}
	api := &deliveryAPI{}
	if err := NewSender(store, api, nil, SenderOptions{BotID: "bot", OwnerID: 42}).flush(context.Background()); !errors.Is(err, registry.ErrDeliveryLeaseChanged) {
		t.Fatalf("repair error = %v", err)
	}
	if len(api.messages) != 0 || store.delay != 0 {
		t.Fatal("fenced sender sent a message or changed retry state of newer claim")
	}
}

func TestOversizedSessionDeliveryOnlyRepairsPendingSessionPickers(t *testing.T) {
	row := registry.Delivery{Kind: "ui_response", Payload: json.RawMessage(`{"view":"sessions"}`)}
	large := registry.DeliveryChunk{Payload: oversizedSessionMessage(t)}
	if !oversizedSessionDelivery(row, []registry.DeliveryChunk{large}) {
		t.Fatal("oversized cached picker was not detected")
	}
	for _, test := range []struct {
		name  string
		row   registry.Delivery
		chunk registry.DeliveryChunk
	}{
		{"sent chunk", row, registry.DeliveryChunk{Sent: true, Payload: large.Payload}},
		{"other event", registry.Delivery{Kind: "final_message", Payload: row.Payload}, large},
		{"other UI", registry.Delivery{Kind: "ui_response", Payload: json.RawMessage(`{"view":"help"}`)}, large},
		{"error UI", registry.Delivery{Kind: "ui_response", Payload: json.RawMessage(`{"view":"sessions","error_code":"unavailable"}`)}, large},
		{"small keyboard", row, registry.DeliveryChunk{Payload: json.RawMessage(`{"text":"frozen","reply_markup":{"inline_keyboard":[[{"text":"Select","callback_data":"opaque"}]]}}`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if oversizedSessionDelivery(test.row, []registry.DeliveryChunk{test.chunk}) {
				t.Fatal("unrelated checkpoint was selected for repair")
			}
		})
	}
	message := SendMessage{Text: "many short buttons", Keyboard: &TelegramKeyboard{}}
	for i := 0; i < 2*sessionPageSize+4; i++ {
		message.Keyboard.Rows = append(message.Keyboard.Rows, []TelegramButton{{Text: "x", Data: "x"}})
	}
	raw, _ := json.Marshal(message)
	if !oversizedSessionDelivery(row, []registry.DeliveryChunk{{Payload: raw}}) {
		t.Fatal("legacy picker with too many buttons was not detected")
	}
}

func (s *checkpointStore) ClaimDeliveries(context.Context, int) ([]registry.Delivery, error) {
	return []registry.Delivery{s.row}, nil
}
func (s *checkpointStore) DeliveryChunks(context.Context, string) ([]registry.DeliveryChunk, error) {
	return s.chunks, nil
}
func (s *checkpointStore) ExtendDelivery(context.Context, string) error { return nil }
func (s *checkpointStore) RetryDelivery(_ context.Context, _ string, delay time.Duration, _ string) error {
	s.delay = delay
	return nil
}
func (s *checkpointStore) MarkDeliveryChunkSent(_ context.Context, _ string, index int, _ int64, _, _, _ string, _ ...string) error {
	s.marked = append(s.marked, index)
	return nil
}

type deliveryAPI struct {
	TelegramAPI
	messages []SendMessage
	failure  error
}

func (a *deliveryAPI) Send(_ context.Context, message SendMessage) (int64, error) {
	a.messages = append(a.messages, message)
	return 99, a.failure
}

func TestSenderResumesFrozenMessagesAndHonorsRateLimit(t *testing.T) {
	store := &checkpointStore{row: registry.Delivery{ID: "delivery", Attempt: 5}, chunks: []registry.DeliveryChunk{
		{Index: 0, Sent: true, Payload: json.RawMessage(`{"chat_id":20,"text":"already sent"}`)},
		{Index: 1, Payload: json.RawMessage(`{"chat_id":20,"text":"frozen remaining"}`)},
	}}
	api := &deliveryAPI{failure: &TelegramError{Code: 429, RetryAfter: 45 * time.Second}}
	sender := NewSender(store, api, nil)
	if err := sender.flush(context.Background()); err == nil {
		t.Fatal("rate limit ignored")
	}
	if store.delay != 45*time.Second || len(api.messages) != 1 || api.messages[0].Text != "frozen remaining" || len(store.marked) != 0 {
		t.Fatalf("bad retry %s %#v %v", store.delay, api.messages, store.marked)
	}
	api.failure = nil
	if err := sender.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.marked) != 1 || store.marked[0] != 1 || len(api.messages) != 2 || api.messages[1].Text != "frozen remaining" {
		t.Fatal("sender replayed a checkpointed message or changed content")
	}
}

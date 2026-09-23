package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
)

type pickerCleanupStore struct {
	checkpointStore
	markedIDs []int64
	markError error
	routes    [][3]string
}

func (s *pickerCleanupStore) PrepareDeliveryChunks(_ context.Context, _ string, messages []json.RawMessage) ([]registry.DeliveryChunk, error) {
	for index, payload := range messages {
		s.chunks = append(s.chunks, registry.DeliveryChunk{Index: index, Payload: payload})
	}
	return s.chunks, nil
}

func (s *pickerCleanupStore) MarkDeliveryChunkSent(_ context.Context, _ string, index int, messageID int64, session, turn, approval string, _ ...string) error {
	if s.markError != nil {
		return s.markError
	}
	s.marked = append(s.marked, index)
	s.markedIDs = append(s.markedIDs, messageID)
	s.routes = append(s.routes, [3]string{session, turn, approval})
	s.chunks[index].Sent = true
	return nil
}

type pickerCleanupAPI struct {
	TelegramAPI
	deleted [][2]int64
	err     error
}

func (a *pickerCleanupAPI) DeleteMessage(_ context.Context, chat, message int64) error {
	a.deleted = append(a.deleted, [2]int64{chat, message})
	return a.err
}

// All non-delete methods deliberately remain nil: cleanup must never send or
// edit a visible message, nor acknowledge callbacks through this API path.
func pickerCleanupFixture() (*pickerCleanupStore, *pickerCleanupAPI) {
	return &pickerCleanupStore{checkpointStore: checkpointStore{row: registry.Delivery{
		ID: "cleanup", BotID: "bot", ChatID: 99, TopicID: 4, Kind: "picker_cleanup", Attempt: 1,
		Payload: json.RawMessage(`{"message_id":123,"origin_delivery_id":"original-picker"}`),
	}}}, &pickerCleanupAPI{}
}

func TestPickerCleanupDeletesOriginalAndCheckpointsWithoutReplyRoute(t *testing.T) {
	store, api := pickerCleanupFixture()
	if err := NewSender(store, api, nil).flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != [2]int64{99, 123} || len(store.markedIDs) != 1 || store.markedIDs[0] != 123 {
		t.Fatalf("wrong deletion or checkpoint: deleted=%v marked=%v", api.deleted, store.markedIDs)
	}
	if len(store.routes) != 1 || store.routes[0] != [3]string{} {
		t.Fatalf("cleanup acquired a session reply route: %v", store.routes)
	}
	if len(store.chunks) != 1 || !store.chunks[0].Sent {
		t.Fatalf("cleanup was not checkpointed: %v", store.chunks)
	}
	var checkpoint SendMessage
	if err := json.Unmarshal(store.chunks[0].Payload, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Text != "" || checkpoint.Keyboard != nil || len(checkpoint.Entities) != 0 {
		t.Fatalf("cleanup froze visible content: %+v", checkpoint)
	}
	if err := NewSender(store, api, nil).flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || len(store.markedIDs) != 1 {
		t.Fatal("restarted sender replayed checkpointed cleanup")
	}
}

func TestPickerCleanupSettlesOnlyDefiniteUnavailableMessages(t *testing.T) {
	for _, description := range []string{
		"Bad Request: message to delete not found",
		"Bad Request: message can't be deleted",
	} {
		t.Run(description, func(t *testing.T) {
			store, api := pickerCleanupFixture()
			api.err = &TelegramError{Code: 400, Description: description}
			if err := NewSender(store, api, nil).flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(store.markedIDs) != 1 || store.delay != 0 {
				t.Fatal("permanently unavailable picker would block the next selection")
			}
		})
	}
}

func TestPickerCleanupRetriesTransientAndUnrecognizedErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		wait time.Duration
	}{
		{"network", errors.New("network unavailable"), time.Second},
		{"rate limit", &TelegramError{Code: 429, RetryAfter: 37 * time.Second}, 37 * time.Second},
		{"other bad request", &TelegramError{Code: 400, Description: "Bad Request: chat not found"}, time.Second},
		{"server", &TelegramError{Code: 500, Description: "Internal server error"}, time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, api := pickerCleanupFixture()
			api.err = test.err
			if err := NewSender(store, api, nil).flush(t.Context()); !errors.Is(err, test.err) {
				t.Fatalf("cleanup error = %v; want %v", err, test.err)
			}
			if len(store.markedIDs) != 0 || store.delay != test.wait || len(api.deleted) != 1 {
				t.Fatalf("wrong retry: marked=%v delay=%v deleted=%v", store.markedIDs, store.delay, api.deleted)
			}
			api.err = nil
			if err := NewSender(store, api, nil).flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(store.markedIDs) != 1 || len(api.deleted) != 2 || api.deleted[1] != api.deleted[0] {
				t.Fatal("cleanup retry changed its original target")
			}
		})
	}
}

func TestPickerCleanupRecoversWhenDeleteSucceedsBeforeCheckpoint(t *testing.T) {
	store, api := pickerCleanupFixture()
	store.markError = errors.New("database unavailable")
	if err := NewSender(store, api, nil).flush(t.Context()); !errors.Is(err, store.markError) {
		t.Fatalf("checkpoint error = %v", err)
	}
	store.markError = nil
	api.err = &TelegramError{Code: 400, Description: "Bad Request: message to delete not found"}
	if err := NewSender(store, api, nil).flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(store.markedIDs) != 1 || len(api.deleted) != 2 || store.markedIDs[0] != 123 {
		t.Fatal("successful delete could not recover its interrupted checkpoint")
	}
}

func TestPickerCleanupRejectsMissingAPIAndMalformedTargets(t *testing.T) {
	for _, payload := range []string{`{`, `{}`, `{"message_id":0}`, `{"message_id":-2}`, `{"message_id":"123"}`} {
		t.Run(payload, func(t *testing.T) {
			store, api := pickerCleanupFixture()
			store.row.Payload = json.RawMessage(payload)
			if err := NewSender(store, api, nil).flush(t.Context()); err == nil {
				t.Fatal("invalid cleanup target accepted")
			}
			if len(api.deleted) != 0 || len(store.markedIDs) != 0 {
				t.Fatal("invalid cleanup reached Telegram or was checkpointed")
			}
		})
	}
	store, _ := pickerCleanupFixture()
	api := &deliveryAPI{}
	if err := NewSender(store, api, nil).flush(t.Context()); err == nil {
		t.Fatal("cleanup without DeleteMessage API accepted")
	}
	if len(api.messages) != 0 || len(store.markedIDs) != 0 {
		t.Fatal("missing DeleteMessage API sent or checkpointed a message")
	}
}

func TestWebhookForwardsPickerCallbackMessageIdentity(t *testing.T) {
	store := &webhookStore{}
	cfg := config.GatewayConfig{AllowedUserIDs: []int64{7}, Secrets: config.BotSecrets{BotName: "bot"}}
	handler := NewWebhook(store, cfg, "secret", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"update_id":20,"callback_query":{"id":"callback-id","from":{"id":7},"message":{"message_id":123,"message_thread_id":4,"chat":{"id":99},"reply_to_message":{"message_id":88}},"data":"cb:opaque-token"}}`
	r := httptest.NewRequest("POST", "/tgw/api/v1/telegram/webhook", strings.NewReader(body))
	r.Header.Set("X-Telegram-Bot-Api-Secret-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 || len(store.calls) != 1 {
		t.Fatalf("callback delivery failed: code=%d calls=%d", w.Code, len(store.calls))
	}
	in := store.calls[0]
	if in.CallbackMessageID != 123 || in.CallbackToken != "opaque-token" || in.ChatID != 99 || in.TopicID != 4 || in.UserID != 7 || in.ReplyToMessageID != 88 {
		t.Fatalf("callback origin was lost or confused with reply identity: %+v", in)
	}
}

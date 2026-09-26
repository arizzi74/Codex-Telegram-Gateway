package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/registry"
)

type inputReplyCleanupStore struct {
	*pickerCleanupStore
	skipped            bool
	skipError          error
	authorizationCalls int
}

func (s *inputReplyCleanupStore) ClaimDeliveries(ctx context.Context, limit int) ([]registry.Delivery, error) {
	if s.skipped {
		return nil, nil
	}
	return s.pickerCleanupStore.ClaimDeliveries(ctx, limit)
}

func (s *inputReplyCleanupStore) SkipDelivery(context.Context, string) error {
	if s.skipError != nil {
		return s.skipError
	}
	s.skipped = true
	return nil
}

func (s *inputReplyCleanupStore) TelegramChatAllowed(context.Context, string, int64) (bool, error) {
	s.authorizationCalls++
	return false, nil
}

func inputReplyCleanupFixture() (*inputReplyCleanupStore, *pickerCleanupAPI) {
	store, api := pickerCleanupFixture()
	store.row.Kind = "input_reply_cleanup"
	store.row.Payload = json.RawMessage(`{"message_id":123}`)
	return &inputReplyCleanupStore{pickerCleanupStore: store}, api
}

func TestInputReplyCleanupDeletesHelperWithoutPostingOrSessionRoute(t *testing.T) {
	store, api := inputReplyCleanupFixture()
	// Even irrelevant route fields cannot attach this operation to a session.
	store.row.Payload = json.RawMessage(`{"message_id":123,"session_id":"unrelated-session","turn_id":"unrelated-turn","approval_id":"unrelated-approval"}`)
	sender := NewSender(store, api, nil)
	if err := sender.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != [2]int64{99, 123} || len(store.markedIDs) != 1 || store.markedIDs[0] != 123 {
		t.Fatalf("wrong helper deletion/checkpoint: deleted=%v marked=%v", api.deleted, store.markedIDs)
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
	if checkpoint.Text != "" || checkpoint.Keyboard != nil || len(checkpoint.Entities) > 0 {
		t.Fatalf("cleanup froze visible content: %+v", checkpoint)
	}
	if err := NewSender(store, api, nil).flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(api.deleted) != 1 || len(store.markedIDs) != 1 {
		t.Fatal("restarted sender replayed successful helper cleanup")
	}
}

func TestInputReplyCleanupCancelsUndeletableMessagesWithoutClaimingDeletion(t *testing.T) {
	for _, description := range []string{"Bad Request: message can't be deleted", "Bad Request: message cannot be deleted", "  BAD REQUEST: MESSAGE CAN'T BE DELETED  "} {
		t.Run(description, func(t *testing.T) {
			store, api := inputReplyCleanupFixture()
			api.err = &TelegramError{Code: 400, Description: description}
			var logs bytes.Buffer
			sender := NewSender(store, api, slog.New(slog.NewTextHandler(&logs, nil)))
			if err := sender.flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !store.skipped || len(store.markedIDs) != 0 || len(store.routes) != 0 || store.delay != 0 || store.chunks[0].Sent {
				t.Fatalf("unavailable helper claimed as deleted or retried: skipped=%v marked=%v delay=%v", store.skipped, store.markedIDs, store.delay)
			}
			if !strings.Contains(logs.String(), "cleanup cancelled and message may remain") || !strings.Contains(logs.String(), "level=WARN") {
				t.Fatalf("missing clear deletion limitation warning: %q", logs.String())
			}
			if err := sender.flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(api.deleted) != 1 {
				t.Fatal("permanent Telegram deletion refusal looped")
			}
		})
	}
}

func TestInputReplyCleanupRetriesTransientErrorsAndHonorsRateLimit(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		wait time.Duration
	}{
		{"network", errors.New("network unavailable"), time.Second},
		{"rate limit", &TelegramError{Code: 429, RetryAfter: 37 * time.Second}, 37 * time.Second},
		{"server", &TelegramError{Code: 500, Description: "Internal server error"}, time.Second},
		{"unknown bad request", &TelegramError{Code: 400, Description: "Bad Request: chat not found"}, time.Second},
		{"retryable deletion refusal", &TelegramError{Code: 400, Description: "Bad Request: message can't be deleted", RetryAfter: 10 * time.Second}, 10 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, api := inputReplyCleanupFixture()
			api.err = test.err
			if err := NewSender(store, api, nil).flush(t.Context()); !errors.Is(err, test.err) {
				t.Fatalf("cleanup error = %v; want %v", err, test.err)
			}
			if store.skipped || len(store.markedIDs) != 0 || store.delay != test.wait || len(api.deleted) != 1 {
				t.Fatalf("wrong retry: skipped=%v marked=%v delay=%v deleted=%v", store.skipped, store.markedIDs, store.delay, api.deleted)
			}
			api.err = nil
			if err := NewSender(store, api, nil).flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(store.markedIDs) != 1 || len(api.deleted) != 2 || api.deleted[1] != api.deleted[0] {
				t.Fatal("helper cleanup retry changed target or lost its checkpoint")
			}
		})
	}
}

func TestInputReplyCleanupRecoversDeletedMessageAfterCheckpointFailure(t *testing.T) {
	store, api := inputReplyCleanupFixture()
	store.markError = errors.New("database unavailable")
	if err := NewSender(store, api, nil).flush(t.Context()); !errors.Is(err, store.markError) {
		t.Fatalf("checkpoint error = %v", err)
	}
	store.markError = nil
	api.err = &TelegramError{Code: 400, Description: "Bad Request: message to delete not found"}
	if err := NewSender(store, api, nil).flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.skipped || len(store.markedIDs) != 1 || len(api.deleted) != 2 || store.markedIDs[0] != 123 {
		t.Fatal("already-deleted helper did not recover its interrupted checkpoint")
	}
}

func TestInputReplyCleanupDoesNotLoseFailedCancellation(t *testing.T) {
	store, api := inputReplyCleanupFixture()
	api.err = &TelegramError{Code: 400, Description: "Bad Request: message can't be deleted"}
	store.skipError = errors.New("database unavailable")
	if err := NewSender(store, api, nil).flush(t.Context()); !errors.Is(err, store.skipError) {
		t.Fatalf("cancellation error = %v", err)
	}
	if store.skipped || len(store.markedIDs) != 0 || store.delay != time.Second {
		t.Fatal("failed cancellation was lost or asserted successful deletion")
	}
}

func TestInputReplyCleanupRejectsInvalidTargetAndMissingAPI(t *testing.T) {
	for _, payload := range []string{`{`, `{}`, `null`, `{"message_id":0}`, `{"message_id":-2}`, `{"message_id":"123"}`} {
		t.Run(payload, func(t *testing.T) {
			store, api := inputReplyCleanupFixture()
			store.row.Payload = json.RawMessage(payload)
			if err := NewSender(store, api, nil).flush(t.Context()); err == nil {
				t.Fatal("invalid helper cleanup target accepted")
			}
			if len(api.deleted) != 0 || len(store.markedIDs) != 0 || store.skipped {
				t.Fatal("invalid target reached Telegram or completed cleanup")
			}
		})
	}
	store, _ := inputReplyCleanupFixture()
	api := &deliveryAPI{}
	if err := NewSender(store, api, nil).flush(t.Context()); err == nil {
		t.Fatal("cleanup without DeleteMessage API accepted")
	}
	if len(api.messages) != 0 || len(store.markedIDs) != 0 {
		t.Fatal("missing delete API sent or checkpointed a visible message")
	}
	store, deletionAPI := inputReplyCleanupFixture()
	store.row.ChatID = 0
	if err := NewSender(store, deletionAPI, nil).flush(t.Context()); err == nil || len(deletionAPI.deleted) != 0 {
		t.Fatal("zero chat identity reached Telegram")
	}
}

func TestInputReplyCleanupKeepsOriginalDestinationAfterPostingAuthorizationRevoked(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		for _, allowlist := range [][]int64{nil, {42}} {
			store, api := inputReplyCleanupFixture()
			if frozen {
				// A stale visible-looking chunk cannot become a new send or
				// change this operation's original deletion destination.
				store.chunks = []registry.DeliveryChunk{{Index: 0, Payload: json.RawMessage(`{"chat_id":555,"text":"must never be sent"}`)}}
			}
			if err := NewSender(store, api, nil, SenderOptions{AllowedChatIDs: allowlist}).flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if store.authorizationCalls != 0 || store.skipped || len(api.deleted) != 1 || api.deleted[0] != [2]int64{99, 123} || len(store.markedIDs) != 1 {
				t.Fatalf("revoked-chat helper cleanup suppressed or redirected: checks=%d skipped=%v deleted=%v", store.authorizationCalls, store.skipped, api.deleted)
			}
		}
	}
}

package gateway

import (
	"context"
	"encoding/json"
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

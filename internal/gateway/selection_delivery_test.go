package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type selectionBarrierStore struct {
	*progressReplacementStoreFake
	row             registry.Delivery
	checks, deferAt int
	retryDelay      time.Duration
}

func (s *selectionBarrierStore) ClaimDeliveries(context.Context, int) ([]registry.Delivery, error) {
	return []registry.Delivery{s.row}, nil
}

func (s *selectionBarrierStore) SuppressTelegramDelivery(context.Context, string) (bool, error) {
	s.checks++
	if s.deferAt > 0 && s.checks >= s.deferAt {
		return false, registry.ErrTelegramSelectionPending
	}
	return false, nil
}

func (s *selectionBarrierStore) RetryDelivery(_ context.Context, _ string, delay time.Duration, _ string) error {
	s.retryDelay = delay
	return nil
}

func TestSenderDefersLeasedProgressUntilConnectionConfirmationWithoutCancelling(t *testing.T) {
	for _, kind := range []string{"agent_progress_message", "tool_progress_message"} {
		for _, deferAt := range []int{1, 2, 3} {
			base, api := progressReplacementFixture()
			row := eventRow(t, kind, protocol.Result{TurnID: "turn-a", Text: "Latest progress"}, testSessionID.String())
			store := &selectionBarrierStore{progressReplacementStoreFake: base, row: row, deferAt: deferAt}
			if err := NewSender(store, api, nil).flush(t.Context()); !errors.Is(err, registry.ErrTelegramSelectionPending) {
				t.Fatalf("%s check %d: error = %v", kind, deferAt, err)
			}
			if len(api.messages) != 0 || len(api.edits) != 0 || len(store.marked) != 0 || len(store.skipped) != 0 || store.retryDelay <= 0 {
				t.Fatalf("%s check %d: waiting progress sent/cancelled instead of retried", kind, deferAt)
			}
			// A fresh sender resumes the same prepared delivery after the durable
			// connection confirmation succeeds; no progress is lost.
			store.deferAt = 0
			if err := NewSender(store, api, nil).flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(api.messages) != 1 || len(store.marked) != 1 || len(store.skipped) != 0 {
				t.Fatalf("%s check %d: deferred progress was lost", kind, deferAt)
			}
		}
	}
}

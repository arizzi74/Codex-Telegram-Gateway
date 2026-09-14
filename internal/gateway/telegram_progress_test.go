package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type progressStoreFake struct {
	*renderStoreFake
	chunks         []registry.DeliveryChunk
	suppressAt     int
	suppressChecks int
	skipped        []string
	marked         []int
	deletions      []registry.TelegramDeletion
	deleted        []string
	delay          time.Duration
	lastRoute      [2]string
}

func (f *progressStoreFake) SuppressProgressDelivery(context.Context, string) (bool, error) {
	f.suppressChecks++
	return f.suppressAt > 0 && f.suppressChecks >= f.suppressAt, nil
}

func (f *progressStoreFake) SkipDelivery(_ context.Context, id string) error {
	f.skipped = append(f.skipped, id)
	return nil
}

func (f *progressStoreFake) DeliveryChunks(context.Context, string) ([]registry.DeliveryChunk, error) {
	return f.chunks, nil
}

func (f *progressStoreFake) PrepareDeliveryChunks(_ context.Context, _ string, messages []json.RawMessage) ([]registry.DeliveryChunk, error) {
	for index, payload := range messages {
		f.chunks = append(f.chunks, registry.DeliveryChunk{Index: index, Payload: payload})
	}
	return f.chunks, nil
}

func (f *progressStoreFake) MarkDeliveryChunkSent(_ context.Context, _ string, index int, _ int64, session, turn, _ string, _ ...string) error {
	f.marked = append(f.marked, index)
	f.chunks[index].Sent = true
	f.lastRoute = [2]string{session, turn}
	return nil
}

func (f *progressStoreFake) ClaimTelegramDeletions(context.Context, int) ([]registry.TelegramDeletion, error) {
	return f.deletions, nil
}

func (f *progressStoreFake) MarkTelegramDeletionDone(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	f.deletions = nil
	return nil
}

func (f *progressStoreFake) RetryTelegramDeletion(_ context.Context, _ string, delay time.Duration, _ string) error {
	f.delay = delay
	return nil
}

type progressAPIFake struct {
	deliveryAPI
	deleteErr error
	deleted   [][2]int64
}

func (a *progressAPIFake) DeleteMessage(_ context.Context, chat, message int64) error {
	a.deleted = append(a.deleted, [2]int64{chat, message})
	return a.deleteErr
}

func TestProgressDeliveryIsTemporarySilentAndKeepsItsTurnRoute(t *testing.T) {
	store := &progressStoreFake{renderStoreFake: renderFixture()}
	api := &progressAPIFake{}
	sender := NewSender(store, api, nil)
	body := strings.Repeat("Progress update. ", 400)
	row := eventRow(t, "agent_progress_message", protocol.Result{TurnID: "turn-progress", Text: body}, testSessionID.String())
	if err := sender.sendDelivery(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if len(api.messages) != 2 || len(store.marked) != 2 {
		t.Fatalf("progress chunks: sends=%d checkpoints=%v", len(api.messages), store.marked)
	}
	for _, message := range api.messages {
		if !message.DisableNotification || message.ChatID != row.ChatID || message.TopicID != row.TopicID || message.Keyboard != nil {
			t.Fatalf("wrong progress delivery: %#v", message)
		}
	}
	if store.lastRoute != [2]string{testSessionID.String(), "turn-progress"} {
		t.Fatalf("wrong progress reply route: %v", store.lastRoute)
	}
	if len(api.deleted) != 0 {
		t.Fatal("progress removed before terminal delivery")
	}
}

func TestProgressDeliveryStopsSendingChunksWhenTurnEnds(t *testing.T) {
	for _, suppressAt := range []int{1, 3} {
		store := &progressStoreFake{renderStoreFake: renderFixture(), suppressAt: suppressAt}
		api := &progressAPIFake{}
		sender := NewSender(store, api, nil)
		row := eventRow(t, "agent_progress_message", protocol.Result{TurnID: "turn-progress", Text: strings.Repeat("x", 8000)}, testSessionID.String())
		row.ID = "progress-delivery"
		if err := sender.sendDelivery(context.Background(), row); err != nil {
			t.Fatal(err)
		}
		wantSent := 0
		if suppressAt == 3 {
			wantSent = 1
		}
		if len(api.messages) != wantSent || len(store.skipped) != 1 || store.skipped[0] != row.ID {
			t.Fatalf("suppressAt=%d: sends=%d skipped=%v", suppressAt, len(api.messages), store.skipped)
		}
	}
}

func TestProgressCleanupRetriesWithoutResendingAndRecoversMissingMessages(t *testing.T) {
	store := &progressStoreFake{renderStoreFake: renderFixture(), deletions: []registry.TelegramDeletion{{ID: "cleanup", ChatID: -123, MessageID: 456, Attempt: 2}}}
	api := &progressAPIFake{deleteErr: &TelegramError{Code: 429, RetryAfter: 45 * time.Second}}
	sender := NewSender(store, api, nil)
	if err := sender.flushDeletions(context.Background(), store, api); err == nil {
		t.Fatal("delete rate limit ignored")
	}
	if store.delay != 45*time.Second || len(store.deleted) != 0 || len(api.messages) != 0 {
		t.Fatalf("cleanup retry affected delivery: delay=%s deleted=%v sends=%d", store.delay, store.deleted, len(api.messages))
	}
	// A restarted sender may repeat a deletion whose remote success was not
	// checkpointed, or the user may already have removed the temporary message.
	api.deleteErr = &TelegramError{Code: 400, Description: "Bad Request: message to delete not found"}
	sender = NewSender(store, api, nil)
	if err := sender.flushDeletions(context.Background(), store, api); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "cleanup" || len(api.deleted) != 2 || api.deleted[1] != [2]int64{-123, 456} {
		t.Fatalf("cleanup recovery: deleted=%v API=%v", store.deleted, api.deleted)
	}
}

func TestProgressCleanupDoesNotDiscardOtherFailures(t *testing.T) {
	for _, failure := range []error{errors.New("network timeout"), &TelegramError{Code: 403, Description: "Forbidden"}, &TelegramError{Code: 400, Description: "Bad Request: message can't be deleted"}} {
		store := &progressStoreFake{renderStoreFake: renderFixture(), deletions: []registry.TelegramDeletion{{ID: "cleanup", ChatID: 123, MessageID: 456, Attempt: 1}}}
		api := &progressAPIFake{deleteErr: failure}
		if err := NewSender(store, api, nil).flushDeletions(context.Background(), store, api); err == nil {
			t.Fatal("cleanup failure discarded")
		}
		if len(store.deleted) != 0 || store.delay != time.Second {
			t.Fatalf("cleanup failure was not retried: deleted=%v delay=%s", store.deleted, store.delay)
		}
	}
}

func TestTurnInterruptedRendersACompleteTerminalNotice(t *testing.T) {
	row := eventRow(t, "turn_interrupted", protocol.Result{TurnID: "turn-progress"}, testSessionID.String())
	text, keyboard, err := testSender(renderFixture(), nil).render(context.Background(), row)
	if err != nil || !strings.Contains(text, "Turn interrupted.") || keyboard != nil {
		t.Fatalf("interrupted render: %q, %v", text, err)
	}
}

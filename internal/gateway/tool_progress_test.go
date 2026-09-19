package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/protocol"
)

type progressReplacementStoreFake struct {
	*progressStoreFake
	target    int64
	markedIDs []int64
	markErr   error
}

func (f *progressReplacementStoreFake) TelegramProgressTarget(context.Context, string) (int64, error) {
	return f.target, nil
}

func (f *progressReplacementStoreFake) MarkDeliveryChunkSent(ctx context.Context, id string, index int, messageID int64, session, turn, approval string, questions ...string) error {
	if f.markErr != nil {
		return f.markErr
	}
	if err := f.progressStoreFake.MarkDeliveryChunkSent(ctx, id, index, messageID, session, turn, approval, questions...); err != nil {
		return err
	}
	f.target = messageID
	f.markedIDs = append(f.markedIDs, messageID)
	return nil
}

type progressReplacementAPIFake struct {
	progressAPIFake
	edits   []SendMessage
	editIDs []int64
	editErr error
}

func (a *progressReplacementAPIFake) EditFormatted(_ context.Context, id int64, message SendMessage) error {
	a.edits = append(a.edits, message)
	a.editIDs = append(a.editIDs, id)
	return a.editErr
}

func progressReplacementFixture() (*progressReplacementStoreFake, *progressReplacementAPIFake) {
	return &progressReplacementStoreFake{progressStoreFake: &progressStoreFake{renderStoreFake: renderFixture()}}, &progressReplacementAPIFake{}
}

func TestToolProgressReplacesPreviousCallInMonospaceAfterSenderRestart(t *testing.T) {
	store, api := progressReplacementFixture()
	ctx := context.Background()
	for i, command := range []string{"printf '<b>👩🏽‍💻 & `literal`</b>'", "go test ./..."} {
		store.chunks = nil
		row := eventRow(t, "tool_progress_message", protocol.Result{TurnID: "turn-tools", Text: command}, testSessionID.String())
		if err := NewSender(store, api, nil).sendDelivery(ctx, row); err != nil {
			t.Fatal(err)
		}
		if len(api.messages) != 1 || len(api.edits) != i {
			t.Fatalf("tool %d created extra message: sends=%d edits=%d", i, len(api.messages), len(api.edits))
		}
	}
	if len(api.editIDs) != 1 || api.editIDs[0] != 99 || len(store.markedIDs) != 2 || store.markedIDs[0] != store.markedIDs[1] {
		t.Fatalf("replacement lost message identity: edits=%v checkpoints=%v", api.editIDs, store.markedIDs)
	}
	for _, message := range []SendMessage{api.messages[0], api.edits[0]} {
		if !message.DisableNotification || message.Keyboard != nil || len(message.Entities) != 1 {
			t.Fatalf("wrong temporary tool formatting: %+v", message)
		}
		entity := message.Entities[0]
		if entity.Type != "pre" || entity.Offset != 0 || entity.Length != len(utf16.Encode([]rune(message.Text))) {
			t.Fatalf("wrong Unicode monospace range: %+v", entity)
		}
	}
	if !strings.Contains(api.messages[0].Text, "<b>👩🏽‍💻 & `literal`</b>") || !strings.Contains(api.edits[0].Text, "go test ./...") || strings.Contains(api.edits[0].Text, "printf") {
		t.Fatal("tool calls were escaped, appended, or lost")
	}
	if store.lastRoute != [2]string{testSessionID.String(), "turn-tools"} || len(api.deleted) != 0 {
		t.Fatal("tool replacement lost its turn route or deleted progress early")
	}
}

func TestToolProgressTruncatesToOneUnicodeSafeMessage(t *testing.T) {
	store, api := progressReplacementFixture()
	row := eventRow(t, "tool_progress_message", protocol.Result{TurnID: "turn-tools", Text: strings.Repeat("🧪<&>\n", 3000)}, testSessionID.String())
	if err := NewSender(store, api, nil).sendDelivery(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if len(api.messages) != 1 {
		t.Fatalf("long call sent %d messages", len(api.messages))
	}
	message := api.messages[0]
	if !utf8.ValidString(message.Text) || len(utf16.Encode([]rune(message.Text))) > 4000 || !strings.HasSuffix(message.Text, "… (truncated)") {
		t.Fatalf("invalid or oversized tool message: bytes=%d", len(message.Text))
	}
}

func TestProgressEditRetriesWithoutSendingDuplicates(t *testing.T) {
	for _, kind := range []string{"agent_progress_message", "tool_progress_message"} {
		t.Run(kind, func(t *testing.T) {
			store, api := progressReplacementFixture()
			store.target = 77
			api.editErr = &TelegramError{Code: 429, RetryAfter: 45 * time.Second}
			row := eventRow(t, kind, protocol.Result{TurnID: "turn-progress", Text: "make test"}, testSessionID.String())
			sender := NewSender(store, api, nil)
			if err := sender.sendDelivery(context.Background(), row); err == nil || telegramRetryDelay(1, err) != 45*time.Second {
				t.Fatalf("edit rate limit lost: %v", err)
			}
			if len(api.messages) != 0 || len(store.markedIDs) != 0 {
				t.Fatal("failed edit sent or checkpointed a duplicate")
			}
			// Telegram reports unchanged content if the previous attempt reached it
			// before a timeout or the local checkpoint failed.
			api.editErr = &TelegramError{Code: 400, Description: "Bad Request: message is not modified: specified new message content and reply markup are exactly the same"}
			if err := sender.sendDelivery(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			if len(api.messages) != 0 || len(store.markedIDs) != 1 || store.markedIDs[0] != 77 {
				t.Fatal("unchanged edit was not checkpointed against the original message")
			}
		})
	}
}

func TestProgressRecreatesOnlyDefinitelyMissingMessages(t *testing.T) {
	for _, kind := range []string{"agent_progress_message", "tool_progress_message"} {
		t.Run(kind, func(t *testing.T) {
			for _, failure := range []error{
				&TelegramError{Code: 400, Description: "Bad Request: message to edit not found"},
				&TelegramError{Code: 400, Description: "Bad Request: message can't be edited"},
				&TelegramError{Code: 403, Description: "Forbidden"},
				errors.New("network timeout"),
			} {
				store, api := progressReplacementFixture()
				store.target = 77
				api.editErr = failure
				row := eventRow(t, kind, protocol.Result{TurnID: "turn-progress", Text: "make test"}, testSessionID.String())
				err := NewSender(store, api, nil).sendDelivery(context.Background(), row)
				missing := strings.Contains(failure.Error(), "message to edit not found")
				if missing && (err != nil || len(api.messages) != 1 || len(store.markedIDs) != 1) {
					t.Fatalf("deleted progress message was not recreated: %v", err)
				}
				if !missing && (err == nil || len(api.messages) != 0 || len(store.markedIDs) != 0) {
					t.Fatalf("uncertain edit created a duplicate: %v", failure)
				}
			}
		})
	}
}

func TestProgressSuppressedBeforeSendOrEdit(t *testing.T) {
	for _, kind := range []string{"agent_progress_message", "tool_progress_message"} {
		t.Run(kind, func(t *testing.T) {
			for _, target := range []int64{0, 77} {
				store, api := progressReplacementFixture()
				store.target = target
				store.suppressAt = 1
				row := eventRow(t, kind, protocol.Result{TurnID: "turn-progress", Text: "make test"}, testSessionID.String())
				if err := NewSender(store, api, nil).sendDelivery(context.Background(), row); err != nil {
					t.Fatal(err)
				}
				if len(api.messages) != 0 || len(api.edits) != 0 || len(store.skipped) != 1 {
					t.Fatal("obsolete progress was sent or edited")
				}
			}
		})
	}
}

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type progressRepairStore struct {
	*progressReplacementStoreFake
	repairs       int
	repairAttempt int
	repairError   error
}

func (s *progressRepairStore) ReplaceUnsentProgressDeliveryChunks(_ context.Context, _ string, attempt int, messages []json.RawMessage) ([]registry.DeliveryChunk, error) {
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

func TestSenderRepairsLegacyMultipartProgressWithoutReplayingSentChunks(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "entirely unsent", true: "partially sent"}[partial], func(t *testing.T) {
			base, api := progressReplacementFixture()
			store := &progressRepairStore{progressReplacementStoreFake: base}
			row := eventRow(t, "agent_progress_message", protocol.Result{TurnID: "turn-progress", Text: "Current progress. " + strings.Repeat("🧪", 4000)}, testSessionID.String())
			row.Attempt = 17
			store.chunks = []registry.DeliveryChunk{
				{Index: 0, Sent: partial, Payload: json.RawMessage(`{"chat_id":99,"text":"old first chunk"}`)},
				{Index: 1, Payload: json.RawMessage(`{"chat_id":99,"text":"old last chunk"}`)},
			}
			if partial {
				store.target = 77
			}
			if err := NewSender(store, api, nil).sendDelivery(context.Background(), row); err != nil {
				t.Fatal(err)
			}
			index := 0
			message := SendMessage{}
			if partial {
				index = 1
				if len(api.messages) != 0 || len(api.edits) != 1 || api.editIDs[0] != 77 {
					t.Fatalf("partial progress created a duplicate: sends=%d edits=%d ids=%v", len(api.messages), len(api.edits), api.editIDs)
				}
				message = api.edits[0]
				if !store.chunks[0].Sent || string(store.chunks[0].Payload) != `{"chat_id":99,"text":"old first chunk"}` {
					t.Fatal("sent checkpoint changed")
				}
			} else {
				if len(api.messages) != 1 || len(api.edits) != 0 {
					t.Fatalf("pending progress was not compacted: sends=%d edits=%d", len(api.messages), len(api.edits))
				}
				message = api.messages[0]
			}
			if store.repairs != 1 || store.repairAttempt != row.Attempt || len(store.marked) != 1 || store.marked[0] != index {
				t.Fatalf("bad repaired progress: repairs=%d attempt=%d marked=%v", store.repairs, store.repairAttempt, store.marked)
			}
			if !strings.HasPrefix(message.Text, "⏳ ") || !strings.Contains(message.Text, "Current progress.") || strings.Contains(message.Text, "old last chunk") || telegramTextLength(message.Text) > 4000 || !strings.HasSuffix(message.Text, "… (truncated)") {
				t.Fatal("repaired message did not show current compact progress")
			}
		})
	}
}

func TestSenderDoesNotSendOldProgressIfRepairIsFenced(t *testing.T) {
	base, api := progressReplacementFixture()
	store := &progressRepairStore{progressReplacementStoreFake: base, repairError: registry.ErrDeliveryLeaseChanged}
	store.chunks = []registry.DeliveryChunk{{Index: 0, Payload: json.RawMessage(`{"text":"old first chunk"}`)}, {Index: 1, Payload: json.RawMessage(`{"text":"old last chunk"}`)}}
	row := eventRow(t, "agent_progress_message", protocol.Result{TurnID: "turn-progress", Text: "Latest progress."}, testSessionID.String())
	if err := NewSender(store, api, nil).sendDelivery(context.Background(), row); !errors.Is(err, registry.ErrDeliveryLeaseChanged) {
		t.Fatalf("repair error = %v", err)
	}
	if len(api.messages) != 0 || len(api.edits) != 0 || len(store.marked) != 0 {
		t.Fatal("fenced progress repair sent or edited a message")
	}
}

func TestProgressRepairDetectsOversizedUnicodeWithoutChangingOtherMessages(t *testing.T) {
	raw, err := json.Marshal(SendMessage{Text: strings.Repeat("🧪", 2200)})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"agent_progress_message", "tool_progress_message"} {
		if !oversizedProgressDelivery(registry.Delivery{Kind: kind}, []registry.DeliveryChunk{{Payload: raw}}) {
			t.Fatalf("oversized Unicode %s was not selected for repair", kind)
		}
	}
	if oversizedProgressDelivery(registry.Delivery{Kind: "final_agent_message"}, []registry.DeliveryChunk{{Payload: raw}, {Payload: raw}}) {
		t.Fatal("final answer selected for progress repair")
	}
	if oversizedProgressDelivery(registry.Delivery{Kind: "agent_progress_message"}, []registry.DeliveryChunk{{Payload: json.RawMessage(`{"text":"existing compact progress"}`)}}) {
		t.Fatal("compact frozen progress selected for repair")
	}
}

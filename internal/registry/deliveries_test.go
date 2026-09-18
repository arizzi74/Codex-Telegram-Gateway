package registry

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestDeliveryFreezesContentAndRecoversOnlyUnsentChunksIntegration(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	_, err := s.AcceptTelegram(ctx, IncomingUpdate{BotID: "bot", UpdateID: 1, UserID: 10, ChatID: 20, Action: "help"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ClaimDeliveries(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %v %v", rows, err)
	}
	id := rows[0].ID
	original := []json.RawMessage{json.RawMessage(`{"chat_id":20,"text":"first"}`), json.RawMessage(`{"chat_id":20,"text":"second","reply_markup":{"inline_keyboard":[]}}`)}
	chunks, err := s.PrepareDeliveryChunks(ctx, id, original)
	if err != nil || len(chunks) != 2 {
		t.Fatalf("prepare: %v %v", chunks, err)
	}
	if err = s.MarkDeliveryChunkSent(ctx, id, 0, 99, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryDelivery(ctx, id, time.Second, "test failure"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=(strftime('%Y-%m-%dT%H:%M:%f','now','-1 second') || '000000Z') WHERE delivery_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ClaimDeliveries(ctx, 1)
	if err != nil || len(rows) != 1 || rows[0].Attempt != 2 {
		t.Fatalf("reclaim: %v %v", rows, err)
	}
	chunks, err = s.PrepareDeliveryChunks(ctx, id, []json.RawMessage{json.RawMessage(`{"text":"new snapshot"}`)})
	if err != nil || len(chunks) != 2 || !chunks[0].Sent || chunks[1].Sent {
		t.Fatalf("checkpoints: %v %v", chunks, err)
	}
	var second map[string]any
	if err = json.Unmarshal(chunks[1].Payload, &second); err != nil || second["text"] != "second" {
		t.Fatalf("frozen content changed: %s", chunks[1].Payload)
	}
	if err = s.ExtendDelivery(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkDeliveryChunkSent(ctx, id, 1, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = s.pool.QueryRow(ctx, `SELECT status FROM telegram_deliveries WHERE delivery_id=$1`, id).Scan(&status); err != nil || status != "sent" {
		t.Fatalf("terminal status %s: %v", status, err)
	}
	rows, err = s.ClaimDeliveries(ctx, 1)
	if err != nil || len(rows) != 0 {
		t.Fatalf("sent delivery was claimed again: %v %v", rows, err)
	}
}

func TestDeliveryClaimRecoversExpiredSendingLeaseIntegration(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	if _, err := s.AcceptTelegram(ctx, IncomingUpdate{BotID: "bot", UpdateID: 1, UserID: 10, ChatID: 20, Action: "help"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ClaimDeliveries(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=(strftime('%Y-%m-%dT%H:%M:%f','now','-1 second') || '000000Z') WHERE delivery_id=$1`, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.ClaimDeliveries(ctx, 1)
	if err != nil || len(recovered) != 1 || recovered[0].ID != rows[0].ID || recovered[0].Attempt != 2 {
		t.Fatalf("crashed sender lease not recovered: %v %v", recovered, err)
	}
}

func frozenSessionDelivery(t *testing.T, store *Store) Delivery {
	t.Helper()
	ctx := context.Background()
	if _, err := store.AcceptTelegram(ctx, IncomingUpdate{BotID: "bot", UpdateID: 1, UserID: 10, ChatID: 20, Action: "help"}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ClaimDeliveries(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %v %v", rows, err)
	}
	row := rows[0]
	row.Payload = json.RawMessage(`{"view":"sessions"}`)
	if _, err := store.pool.Exec(ctx, `UPDATE telegram_deliveries SET payload=$2 WHERE delivery_id=$1`, row.ID, row.Payload); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareDeliveryChunks(ctx, row.ID, []json.RawMessage{
		json.RawMessage(`{"chat_id":20,"text":"historical first"}`),
		json.RawMessage(`{"chat_id":20,"text":"obsolete unsent keyboard"}`),
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestSessionDeliveryRepairPreservesAcceptedMessagesIntegration(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "entirely unsent", true: "partially sent"}[partial], func(t *testing.T) {
			store := integrationStore(t)
			ctx := context.Background()
			row := frozenSessionDelivery(t, store)
			if partial {
				if err := store.MarkDeliveryChunkSent(ctx, row.ID, 0, 99, "", "", ""); err != nil {
					t.Fatal(err)
				}
			}
			replacement := json.RawMessage(`{"chat_id":20,"text":"current page"}`)
			chunks, err := store.ReplaceUnsentSessionDeliveryChunks(ctx, row.ID, row.Attempt, []json.RawMessage{replacement})
			if err != nil {
				t.Fatal(err)
			}
			pendingIndex := 0
			if partial {
				pendingIndex = 1
				if len(chunks) != 2 || !chunks[0].Sent || chunks[0].Index != 0 || string(chunks[0].Payload) != `{"chat_id":20,"text":"historical first"}` {
					t.Fatalf("accepted checkpoint changed: %#v", chunks)
				}
				var messageID int64
				if err := store.pool.QueryRow(ctx, `SELECT telegram_message_id FROM telegram_delivery_chunks WHERE delivery_id=$1 AND chunk_index=0`, row.ID).Scan(&messageID); err != nil || messageID != 99 {
					t.Fatalf("accepted identity = %d, %v", messageID, err)
				}
			}
			if len(chunks) != pendingIndex+1 || chunks[pendingIndex].Sent || chunks[pendingIndex].Index != pendingIndex || string(chunks[pendingIndex].Payload) != string(replacement) {
				t.Fatalf("replacement checkpoint = %#v", chunks)
			}
			if err := store.MarkDeliveryChunkSent(ctx, row.ID, pendingIndex, 100, "", "", ""); err != nil {
				t.Fatal(err)
			}
			var status string
			if err := store.pool.QueryRow(ctx, `SELECT status FROM telegram_deliveries WHERE delivery_id=$1`, row.ID).Scan(&status); err != nil || status != "sent" {
				t.Fatalf("repaired delivery status = %q, %v", status, err)
			}
		})
	}
}

func TestSessionDeliveryRepairFencesClaimsAndRollsBackFailuresIntegration(t *testing.T) {
	for _, test := range []struct {
		name      string
		update    string
		attempt   int
		malformed bool
	}{
		{name: "stale claim", attempt: 2},
		{name: "expired lease", update: `next_attempt_at='2000-01-01T00:00:00.000000000Z'`},
		{name: "unleased delivery", update: `status='failed'`},
		{name: "other UI view", update: `payload='{"view":"help"}'`},
		{name: "error response", update: `payload='{"view":"sessions","error_code":"invalid"}'`},
		{name: "other event", update: `kind='final_message'`},
		{name: "failed replacement insert", malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := integrationStore(t)
			ctx := context.Background()
			row := frozenSessionDelivery(t, store)
			if test.update != "" {
				if _, err := store.pool.Exec(ctx, `UPDATE telegram_deliveries SET `+test.update+` WHERE delivery_id=$1`, row.ID); err != nil {
					t.Fatal(err)
				}
			}
			attempt := row.Attempt
			if test.attempt != 0 {
				attempt = test.attempt
			}
			replacement := json.RawMessage(`{"text":"new page"}`)
			if test.malformed {
				replacement = json.RawMessage(`not JSON`)
			}
			if _, err := store.ReplaceUnsentSessionDeliveryChunks(ctx, row.ID, attempt, []json.RawMessage{replacement}); err == nil {
				t.Fatal("unsafe repair was accepted")
			}
			chunks, err := store.DeliveryChunks(ctx, row.ID)
			if err != nil || len(chunks) != 2 || string(chunks[1].Payload) != `{"chat_id":20,"text":"obsolete unsent keyboard"}` {
				t.Fatalf("rejected repair changed original content: %#v %v", chunks, err)
			}
		})
	}
}

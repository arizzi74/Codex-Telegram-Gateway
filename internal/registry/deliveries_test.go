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

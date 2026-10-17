package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestStoredFileConfirmationStaysWithOriginalChatWithoutStartingTurn(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection,
		Metadata: json.RawMessage(`{"supports_file_input":true}`)}); err != nil {
		t.Fatal(err)
	}
	selection := telegramUpdate(env, 1)
	selection.Action, selection.Target = "connect", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
		t.Fatal(err)
	}
	otherChat := selection
	otherChat.UpdateID, otherChat.ChatID = 20, 777
	if _, err := env.store.AcceptTelegram(ctx, otherChat); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 2)
	in.Action, in.Files = "text", []protocol.File{{Name: "report.pdf", Data: []byte("document bytes")}}
	accepted, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || accepted.CommandID == "" {
		t.Fatalf("accept file-only upload: %#v %v", accepted, err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].Operation != protocol.StoreFiles {
		t.Fatalf("file upload commands: %#v %v", commands, err)
	}
	before, err := env.store.SessionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	selection.UpdateID, selection.Target = 3, other.String()
	if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
		t.Fatal(err)
	}
	text := "File saved: /work/inbound/report.pdf"
	data, err := json.Marshal(protocol.Result{CommandID: accepted.CommandID, Text: text, State: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{Seq: 2, ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(),
		RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "command_completed", OccurredAt: time.Now().UTC(), Data: data}
	for range 2 {
		if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
			t.Fatal(err)
		}
	}
	var deliveries int
	var chat int64
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*),min(chat_id) FROM telegram_deliveries WHERE event_id=$1`, event.ID).Scan(&deliveries, &chat); err != nil || deliveries != 1 || chat != in.ChatID {
		t.Fatalf("file confirmation delivery count=%d chat=%d error=%v", deliveries, chat, err)
	}
	var state string
	var active *string
	if err := env.store.pool.QueryRow(ctx, `SELECT state,active_turn_id FROM sessions WHERE session_id=$1`, env.session).Scan(&state, &active); err != nil || state != before[0].State || active != nil {
		t.Fatalf("file-only upload changed session state=%q active=%v error=%v", state, active, err)
	}
	var turnEvents int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE kind IN ('turn_started','turn_completed','user_message')`).Scan(&turnEvents); err != nil || turnEvents != 0 {
		t.Fatalf("file-only upload created prompt/turn events=%d error=%v", turnEvents, err)
	}
	typing, err := env.store.ListTelegramTypingTargets(ctx, 10)
	if err != nil || len(typing) != 0 {
		t.Fatalf("completed upload kept typing active: %#v %v", typing, err)
	}
}

package registry

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestCreateCallbackCannotRecreatePurgedWorkerReferences(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	wizard := wizardStartTest(t, env, 1, "new")
	approval := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q", Prompt: "Continue?"})
	origin := uuid.NewString()
	payload, err := json.Marshal(AcceptResult{View: "selected", SessionID: env.session.String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload)
        VALUES($1,'bot',20,'ui_response',$2)`, origin, string(payload)); err != nil {
		t.Fatal(err)
	}
	// These descriptors are the snapshots retained by a renderer that began
	// before revocation. Each isolates one reference, including JSON-only ones.
	callbacks := []Callback{
		{Action: "sessions", RuntimeID: env.runtime, Generation: 1},
		{Action: "status", SessionID: env.session},
		{Action: "approval", ApprovalID: approval},
		{Action: "wizard_cancel", WizardID: wizard.WizardID, WizardRevision: wizard.WizardRevision},
		{Action: "sessions", OriginDeliveryID: origin},
	}
	for i := range callbacks {
		callbacks[i].BotID, callbacks[i].UserID, callbacks[i].ChatID = "bot", 10, 20
		callbacks[i].ExpiresAt = time.Now().Add(time.Hour)
		if token, err := env.store.CreateCallback(ctx, callbacks[i]); err != nil || token == "" {
			t.Fatalf("create original callback %d: token=%q error=%v", i, token, err)
		}
	}
	if err := env.store.RevokeWorker(ctx, env.worker); err != nil {
		t.Fatal(err)
	}
	for i, callback := range callbacks[:4] {
		if token, err := env.store.CreateCallback(ctx, callback); token != "" || !errors.Is(err, ErrCallbackInvalid) {
			t.Fatalf("create revoked callback %d: token=%q error=%v", i, token, err)
		}
	}
	if err := env.store.DeleteRevokedWorker(ctx, env.worker); err != nil {
		t.Fatal(err)
	}
	for i, callback := range callbacks {
		if token, err := env.store.CreateCallback(ctx, callback); token != "" || !errors.Is(err, ErrCallbackInvalid) {
			t.Fatalf("recreate purged callback %d: token=%q error=%v", i, token, err)
		}
	}
	var remaining int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_callbacks`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("callbacks after stale rendering: count=%d error=%v", remaining, err)
	}
	// Global navigation remains available when it has no removed target.
	if token, err := env.store.CreateCallback(ctx, Callback{Action: "sessions", BotID: "bot", UserID: 10, ChatID: 20, ExpiresAt: time.Now().Add(time.Hour)}); err != nil || token == "" {
		t.Fatalf("create global callback: token=%q error=%v", token, err)
	}
}

func TestCreateCallbackKeepsHistoricalGenerationAndOrigin(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	origin := insertPickerDelivery(t, env.store, "sessions", 20, 0)
	if err := env.store.MarkDeliverySent(ctx, origin.ID, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{
		ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running",
	}}}); err != nil {
		t.Fatal(err)
	}
	// Creation may retain an old generation or an already-sent picker. Their
	// existing consumption checks remain responsible for deciding usability.
	if token, err := env.store.CreateCallback(ctx, Callback{
		Action: "sessions", BotID: "bot", UserID: 10, ChatID: 20, RuntimeID: env.runtime,
		Generation: 1, OriginDeliveryID: origin.ID, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil || token == "" {
		t.Fatalf("create historical callback: token=%q error=%v", token, err)
	}
}

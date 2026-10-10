package registry

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

func inventoryWorker(t *testing.T, store *Store, name string) eventTestEnv {
	t.Helper()
	ctx := t.Context()
	worker, err := store.CreateWorker(ctx, CreateWorkerInput{Name: name, OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken(uuid.NewString())})
	if err != nil {
		t.Fatal(err)
	}
	env := eventTestEnv{store: store, worker: worker.ID, connection: uuid.New(), runtime: uuid.New(), session: uuid.New()}
	if err := store.BindConnection(ctx, env.worker, env.connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection,
		Metadata: []byte(`{"supports_session_workspaces":true,"supports_webui_session_creation":true}`),
		Runtimes: []Runtime{{ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestWebUIInventoryOmitsRevokedWorkersAndKeepsEmptyOfflineWorkers(t *testing.T) {
	ctx := t.Context()
	revoked := newEventTestEnv(t)
	if err := revoked.store.IngestEvent(ctx, revoked.worker, revoked.connection, revoked.discovery(t)); err != nil {
		t.Fatal(err)
	}
	offline := inventoryWorker(t, revoked.store, "Offline worker")
	if err := offline.store.IngestEvent(ctx, offline.worker, offline.connection, offline.discovery(t)); err != nil {
		t.Fatal(err)
	}
	if err := offline.store.Disconnect(ctx, offline.worker, offline.connection); err != nil {
		t.Fatal(err)
	}
	empty := inventoryWorker(t, revoked.store, "Empty worker")
	fresh, err := revoked.store.CreateWorker(ctx, CreateWorkerInput{Name: "Fresh worker", OS: "linux", Arch: "arm64", TokenHash: auth.HashWorkerToken("fresh-worker-token")})
	if err != nil {
		t.Fatal(err)
	}
	before, err := revoked.store.LiveSessionActivity(ctx)
	if err != nil || len(before.Sessions) != 2 {
		t.Fatalf("initial activity: %+v %v", before, err)
	}
	changes, unsubscribe, err := revoked.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	if err := revoked.store.RevokeWorker(ctx, revoked.worker); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	after, err := revoked.store.LiveSessionActivity(ctx)
	if err != nil || len(after.Sessions) != 1 || after.Sessions[0].SessionID != offline.session.String() || after.InventoryRevision == before.InventoryRevision || after.Revision <= before.Revision {
		t.Fatalf("revocation did not remove live inventory: before=%+v after=%+v err=%v", before, after, err)
	}
	inventory, err := revoked.store.WebUIInventorySnapshot(ctx)
	if err != nil || len(inventory.Workers) != 3 || len(inventory.Runtimes) != 2 || len(inventory.Sessions) != 1 || inventory.Sessions[0].ID != offline.session.String() {
		t.Fatalf("visible inventory: %+v %v", inventory, err)
	}
	wanted := map[uuid.UUID]bool{offline.worker: false, empty.worker: false, fresh.ID: false}
	for _, worker := range inventory.Workers {
		if _, ok := wanted[worker.ID]; !ok || !worker.Enabled {
			t.Fatalf("unexpected worker: %+v", worker)
		}
		wanted[worker.ID] = true
		if worker.ID == empty.worker && !worker.SupportsWebUISessionCreation {
			t.Fatal("empty worker lost new-session capability")
		}
	}
	for id, seen := range wanted {
		if !seen {
			t.Fatalf("enabled worker %s disappeared", id)
		}
	}
	for _, runtime := range inventory.Runtimes {
		if runtime.WorkerID == revoked.worker.String() {
			t.Fatal("revoked worker runtime stayed visible")
		}
	}
	admin, err := revoked.store.AdminDashboardSnapshot(ctx)
	if err != nil || len(admin.Workers) != 4 || len(admin.Runtimes) != 3 || len(admin.Sessions) != 2 {
		t.Fatalf("admin lost retained records: %+v %v", admin, err)
	}
	for _, worker := range admin.Workers {
		if worker.ID == revoked.worker && (worker.Enabled || worker.Connectivity != "disabled") {
			t.Fatalf("admin lost revocation state: %+v", worker)
		}
	}
}

func TestTelegramInventoryLookupsAliasesAndQuestionsOmitRevokedWorkers(t *testing.T) {
	ctx := t.Context()
	revoked := newEventTestEnv(t)
	if err := revoked.store.IngestEvent(ctx, revoked.worker, revoked.connection, revoked.discovery(t)); err != nil {
		t.Fatal(err)
	}
	enabled := inventoryWorker(t, revoked.store, "Offline worker")
	if err := enabled.store.IngestEvent(ctx, enabled.worker, enabled.connection, enabled.discovery(t)); err != nil {
		t.Fatal(err)
	}
	if err := enabled.store.Disconnect(ctx, enabled.worker, enabled.connection); err != nil {
		t.Fatal(err)
	}
	result := acceptModeUpdate(t, revoked, 1, "sessions", "", "")
	if result.View != "runtime_picker" {
		t.Fatalf("initial runtime choices: %+v", result)
	}
	acceptModeUpdate(t, revoked, 2, "select", revoked.session.String(), "")
	aliases, err := revoked.store.ListTelegramSessionAliases(ctx, "bot", 10, 20, 0)
	if err != nil || len(aliases) != 2 {
		t.Fatalf("initial aliases: %+v %v", aliases, err)
	}
	var oldAlias string
	for _, alias := range aliases {
		if alias.SessionID == revoked.session.String() {
			oldAlias = alias.Alias
		}
	}
	if oldAlias == "" {
		t.Fatal("missing revoked-worker session alias")
	}
	question := insertPendingQuestion(t, revoked, revoked.session, "", true, protocol.Question{ID: "q", Prompt: "Revoked worker question?"})
	insertPendingQuestion(t, enabled, enabled.session, "", true, protocol.Question{ID: "q", Prompt: "Offline worker question?"})
	callback, err := revoked.store.CreateCallback(ctx, Callback{Action: "sessions", BotID: "bot", UserID: 10, ChatID: 20, RuntimeID: revoked.runtime, Generation: 1, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := revoked.store.RevokeWorker(ctx, revoked.worker); err != nil {
		t.Fatal(err)
	}
	result = acceptModeUpdate(t, revoked, 3, "sessions", "", "")
	if result.View != "sessions" || result.RuntimeID != enabled.runtime.String() {
		t.Fatalf("revoked runtime still affected automatic choice: %+v", result)
	}
	result = acceptModeUpdate(t, revoked, 4, "sessions", "Main", "")
	if result.RuntimeID != enabled.runtime.String() {
		t.Fatalf("revoked runtime caused an ambiguous name: %+v", result)
	}
	result = acceptModeUpdate(t, revoked, 5, "select", "Thread", "")
	if result.SessionID != enabled.session.String() {
		t.Fatalf("revoked session caused an ambiguous name: %+v", result)
	}
	for i, entry := range []struct{ action, target, callback string }{
		{action: "sessions", target: revoked.runtime.String()},
		{action: "select", target: revoked.session.String()},
		{action: "status", target: revoked.session.String()},
		{action: "session_alias", target: oldAlias},
		{callback: callback},
	} {
		in := telegramUpdate(revoked, int64(10+i))
		in.Action, in.Target, in.CallbackToken = entry.action, entry.target, entry.callback
		result, err := revoked.store.AcceptTelegram(ctx, in)
		if err != nil || result.View != "error" || (result.ErrorCode != "target_unavailable" && result.ErrorCode != "callback_invalid") {
			t.Fatalf("revoked choice %+v: %+v %v", entry, result, err)
		}
	}
	aliases, err = revoked.store.ListTelegramSessionAliases(ctx, "bot", 10, 20, 0)
	if err != nil || len(aliases) != 1 || aliases[0].SessionID != enabled.session.String() {
		t.Fatalf("visible aliases: %+v %v", aliases, err)
	}
	questions, err := revoked.store.ListPendingQuestions(ctx, "bot", 10, 20, 0, 0)
	if err != nil || len(questions.Requests) != 1 || questions.Requests[0].SessionID != enabled.session.String() {
		t.Fatalf("visible questions: %+v %v", questions, err)
	}
	if _, err := revoked.store.PendingApproval(ctx, question); !errors.Is(err, ErrTelegramTarget) {
		t.Fatalf("revoked-worker question still available: %v", err)
	}
}

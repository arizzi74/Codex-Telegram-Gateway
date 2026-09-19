package registry

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func wizardStartTest(t *testing.T, env eventTestEnv, id int64, action string) AcceptResult {
	t.Helper()
	in := telegramUpdate(env, id)
	in.Action = action
	r, err := env.store.AcceptTelegram(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func wizardCallbackTest(t *testing.T, env eventTestEnv, r AcceptResult, action string, modify func(*Callback)) string {
	t.Helper()
	c := Callback{Action: action, BotID: "bot", UserID: 10, ChatID: 20, WizardID: r.WizardID, WizardRevision: r.WizardRevision, Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if r.RuntimeID != "" {
		c.RuntimeID = uuid.MustParse(r.RuntimeID)
	}
	if modify != nil {
		modify(&c)
	}
	token, err := env.store.CreateCallback(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func wizardClickTest(t *testing.T, env eventTestEnv, id int64, token string) AcceptResult {
	t.Helper()
	in := telegramUpdate(env, id)
	in.CallbackToken = token
	r, err := env.store.AcceptTelegram(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func wizardNameTest(t *testing.T, env eventTestEnv, id int64, name string) AcceptResult {
	t.Helper()
	in := telegramUpdate(env, id)
	in.Text = name
	r, err := env.store.AcceptTelegram(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func wizardResultEvent(t *testing.T, env eventTestEnv, r AcceptResult, kind string, result protocol.Result) protocol.Event {
	t.Helper()
	result.CommandID = r.CommandID
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := env.store.EventWatermark(context.Background(), env.worker)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Event{ID: uuid.NewString(), Seq: uint64(seq + 1), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: r.SessionID, Kind: kind, Data: data, OccurredAt: time.Now().UTC()}
}
func wizardLastResponse(t *testing.T, env eventTestEnv) AcceptResult {
	t.Helper()
	var raw []byte
	if err := env.store.pool.QueryRow(context.Background(), `SELECT payload FROM telegram_deliveries WHERE kind='ui_response' ORDER BY rowid DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var r AcceptResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
func wizardBrowseReadyTest(t *testing.T, env eventTestEnv) AcceptResult {
	t.Helper()
	start := wizardStartTest(t, env, 1, "new")
	if start.View != "new_session_name" {
		t.Fatalf("start: %#v", start)
	}
	loading := wizardNameTest(t, env, 2, "My project")
	if loading.View != "workspace_loading" {
		t.Fatalf("loading: %#v", loading)
	}
	event := wizardResultEvent(t, env, loading, "command_completed", protocol.Result{Workspace: &protocol.WorkspacePage{Path: "/home/person/CODEX", Parent: "/home/person", Directories: []protocol.WorkspaceEntry{{Name: "work", Path: "/home/person/CODEX/work"}}}})
	if err := env.store.IngestEvent(context.Background(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	return wizardLastResponse(t, env)
}

func TestSessionWizardNamedCreationPrivateNavigationIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	browse := wizardBrowseReadyTest(t, env)
	if browse.View != "workspace_browser" || browse.UserID != 10 || browse.SessionName != "My project" {
		t.Fatalf("browser %#v", browse)
	}
	// Only paths returned by the worker can become a navigation command.
	forged := wizardCallbackTest(t, env, browse, "wizard_browse", func(c *Callback) { c.Path = "/etc" })
	if r := wizardClickTest(t, env, 3, forged); r.ErrorCode != "callback_invalid" {
		t.Fatalf("forged path: %#v", r)
	}
	create := wizardCallbackTest(t, env, browse, "wizard_create", nil)
	creating := wizardClickTest(t, env, 4, create)
	if creating.View != "session_creating" {
		t.Fatalf("create %#v", creating)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].Operation != protocol.NewSession || commands[0].Arguments.SessionName != "My project" || !commands[0].Arguments.CreateDirectory || commands[0].Arguments.CWD != "/home/person/CODEX" {
		t.Fatalf("frozen create command %#v", commands)
	}
	if r := wizardClickTest(t, env, 5, create); r.ErrorCode != "callback_invalid" {
		t.Fatalf("replayed create %#v", r)
	}
	if r := wizardNameTest(t, env, 6, "do not send to model"); r.ErrorCode != "wizard_pending" {
		t.Fatalf("pending text: %#v", r)
	}
	session := protocol.Session{ID: uuid.NewString(), ThreadID: "new-thread", Name: "My project", CWD: "/home/person/CODEX/My_project", WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), State: "idle", Loaded: true}
	event := wizardResultEvent(t, env, creating, "command_completed", protocol.Result{Session: &session})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	done := wizardLastResponse(t, env)
	if done.View != "selected" || done.SessionID != session.ID || testSelectedSession(t, env.store, telegramUpdate(env, 0)).String() != session.ID {
		t.Fatalf("completed %#v", done)
	}
	var broadcasts int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE kind<>'ui_response'`).Scan(&broadcasts); err != nil || broadcasts != 0 {
		t.Fatalf("private response broadcasts=%d err=%v", broadcasts, err)
	}
}

func TestSessionWizardContextExpiryCancellationAndStaleResultsIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	start := wizardStartTest(t, env, 1, "new")
	invalid := wizardNameTest(t, env, 2, "../escape")
	if invalid.View != "new_session_name" || invalid.ErrorCode != "session_name_invalid" {
		t.Fatalf("invalid name %#v", invalid)
	}
	cancel := wizardCallbackTest(t, env, start, "wizard_cancel", nil)
	cross := telegramUpdate(env, 3)
	cross.CallbackToken = cancel
	cross.UserID = 11
	r, err := env.store.AcceptTelegram(ctx, cross)
	if err != nil || r.ErrorCode != "callback_invalid" {
		t.Fatalf("crossuser %#v %v", r, err)
	}
	// A worker browse response arriving after cancellation cannot revive the browser.
	loading := wizardNameTest(t, env, 4, "Project")
	cancel = wizardCallbackTest(t, env, loading, "wizard_cancel", nil)
	if r := wizardClickTest(t, env, 5, cancel); r.View != "wizard_cancelled" {
		t.Fatalf("cancel %#v", r)
	}
	event := wizardResultEvent(t, env, loading, "command_completed", protocol.Result{Workspace: &protocol.WorkspacePage{Path: "/home/person", Directories: []protocol.WorkspaceEntry{}}})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	if r := wizardLastResponse(t, env); r.View != "wizard_cancelled" {
		t.Fatalf("late response revived wizard %#v", r)
	}
	start = wizardStartTest(t, env, 6, "new")
	expired := wizardCallbackTest(t, env, start, "wizard_cancel", nil)
	if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_session_wizards SET expires_at=$1`, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r := wizardClickTest(t, env, 7, expired); r.ErrorCode != "callback_invalid" {
		t.Fatalf("expired callback %#v", r)
	}
	if r := wizardNameTest(t, env, 8, "must not become prompt"); r.ErrorCode != "wizard_expired" {
		t.Fatalf("expired name %#v", r)
	}
}

func TestSessionWizardRejectsMalformedWorkerPagesAndRecoversCreationFailureIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	wizardStartTest(t, env, 1, "new")
	loading := wizardNameTest(t, env, 2, "Project")
	bad := wizardResultEvent(t, env, loading, "command_completed", protocol.Result{Workspace: &protocol.WorkspacePage{Path: "/home/person", Directories: []protocol.WorkspaceEntry{{Name: "escape", Path: "relative/path"}}}})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, bad); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("bad worker page %v", err)
	}
	good := wizardResultEvent(t, env, loading, "command_completed", protocol.Result{Workspace: &protocol.WorkspacePage{Path: "/home/person", Directories: []protocol.WorkspaceEntry{}}})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, good); err != nil {
		t.Fatal(err)
	}
	browse := wizardLastResponse(t, env)
	creating := wizardClickTest(t, env, 3, wizardCallbackTest(t, env, browse, "wizard_create", nil))
	failed := wizardResultEvent(t, env, creating, "command_failed", protocol.Result{Error: &protocol.Error{Code: "directory_exists", Message: "Exists"}})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, failed); err != nil {
		t.Fatal(err)
	}
	retry := wizardLastResponse(t, env)
	if retry.View != "workspace_browser" || retry.ErrorCode != "directory_exists" {
		t.Fatalf("failed creation recovery %#v", retry)
	}
	renamed := wizardClickTest(t, env, 4, wizardCallbackTest(t, env, retry, "wizard_rename", nil))
	if renamed.View != "new_session_name" {
		t.Fatalf("rename %#v", renamed)
	}
}

func TestDeleteSessionWizardConfirmationFailureAndSuccessIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	setTestSelection(t, env.store, telegramUpdate(env, 0), env.session)
	list := wizardStartTest(t, env, 1, "delete_session")
	if list.View != "delete_sessions" {
		t.Fatalf("list %#v", list)
	}
	pick := wizardCallbackTest(t, env, list, "delete_session_pick", func(c *Callback) { c.SessionID = env.session })
	confirm := wizardClickTest(t, env, 2, pick)
	if confirm.View != "delete_session_confirm" || confirm.SessionName != "Thread" || confirm.CWD != "/work" {
		t.Fatalf("confirm %#v", confirm)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 0 {
		t.Fatalf("deleted before confirmation %#v %v", commands, err)
	}
	token := wizardCallbackTest(t, env, confirm, "delete_session_confirm", func(c *Callback) { c.SessionID = env.session })
	deleting := wizardClickTest(t, env, 3, token)
	if deleting.View != "session_deleting" {
		t.Fatalf("deleting %#v", deleting)
	}
	failure := wizardResultEvent(t, env, deleting, "command_failed", protocol.Result{Error: &protocol.Error{Code: "delete_failed", Message: "failed"}})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, failure); err != nil {
		t.Fatal(err)
	}
	sessions, err := env.store.SessionSnapshot(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].Archived {
		t.Fatalf("failed deletion changed snapshot %#v %v", sessions, err)
	}
	if got := testSelectedSession(t, env.store, telegramUpdate(env, 0)); got != env.session {
		t.Fatalf("failed deletion changed binding %v", got)
	}
	confirm = wizardLastResponse(t, env)
	token = wizardCallbackTest(t, env, confirm, "delete_session_confirm", func(c *Callback) { c.SessionID = env.session })
	deleting = wizardClickTest(t, env, 4, token)
	session := sessions[0]
	session.Archived = true
	session.Deleted = true
	session.Loaded = false
	session.State = "not_loaded"
	success := wizardResultEvent(t, env, deleting, "command_completed", protocol.Result{Session: &session})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, success); err != nil {
		t.Fatal(err)
	}
	done := wizardLastResponse(t, env)
	if done.View != "session_deleted" || done.CWD != "/work" || done.SessionName != "Thread" {
		t.Fatalf("done %#v", done)
	}
	if _, ok := findTestSelectedSession(t, env.store, telegramUpdate(env, 0)); ok {
		t.Fatal("deleted session binding retained")
	}
	sessions, err = env.store.SessionSnapshot(ctx)
	if err != nil || !sessions[0].Archived {
		t.Fatalf("deleted session still visible %#v %v", sessions, err)
	}
}

func TestDeletedSessionTombstoneSurvivesRestartAndStaleDiscoveryIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	discovery := env.discovery(t)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, discovery); err != nil {
		t.Fatal(err)
	}
	setTestSelection(t, env.store, telegramUpdate(env, 0), env.session)
	var session protocol.Session
	if err := json.Unmarshal(discovery.Data, &session); err != nil {
		t.Fatal(err)
	}
	session.Deleted = true
	session.Archived = true
	session.Loaded = false
	session.State = "not_loaded"
	// The worker restarted before its durable deletion reached the gateway.
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(session)
	event := protocol.Event{ID: uuid.NewString(), Seq: 2, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "session_state_changed", Data: payload, OccurredAt: time.Now().UTC()}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	if _, ok := findTestSelectedSession(t, env.store, telegramUpdate(env, 0)); ok {
		t.Fatal("historical deletion retained binding")
	}
	// Even a later cached inventory snapshot cannot resurrect a permanent deletion.
	discovery.ID = uuid.NewString()
	discovery.Seq = 3
	discovery.RuntimeGeneration = 2
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, discovery); err != nil {
		t.Fatal(err)
	}
	sessions, err := env.store.SessionSnapshot(ctx)
	if err != nil || len(sessions) != 1 || !sessions[0].Archived || sessions[0].Loaded {
		t.Fatalf("tombstone revived %#v %v", sessions, err)
	}
}

func TestSessionWizardUnknownOutcomeDoesNotOfferDeletionRetryIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	list := wizardStartTest(t, env, 1, "delete_session")
	confirm := wizardClickTest(t, env, 2, wizardCallbackTest(t, env, list, "delete_session_pick", func(c *Callback) { c.SessionID = env.session }))
	deleting := wizardClickTest(t, env, 3, wizardCallbackTest(t, env, confirm, "delete_session_confirm", func(c *Callback) { c.SessionID = env.session }))
	event := wizardResultEvent(t, env, deleting, "command_result_unknown", protocol.Result{Error: &protocol.Error{Code: "command_outcome_unknown"}})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	r := wizardLastResponse(t, env)
	if r.View != "error" || r.ErrorCode != "command_outcome_unknown" {
		t.Fatalf("unknown outcome %#v", r)
	}
}

func TestSessionWizardRejectedAcknowledgementRecoversOnceIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	wizardStartTest(t, env, 1, "new")
	loading := wizardNameTest(t, env, 2, "My project")
	ack := protocol.CommandAck{CommandID: loading.CommandID, Error: &protocol.Error{Code: protocol.InternalError}}
	if err := env.store.AcknowledgeCommand(ctx, env.worker, env.connection, ack); err != nil {
		t.Fatal(err)
	}
	response := wizardLastResponse(t, env)
	if response.View != "new_session_name" || response.ErrorCode != protocol.InternalError || response.WizardRevision <= loading.WizardRevision {
		t.Fatalf("ack rejection %#v", response)
	}
	var before, after int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := env.store.AcknowledgeCommand(ctx, env.worker, env.connection, ack); err != nil {
		t.Fatal(err)
	}
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries`).Scan(&after); err != nil || after != before {
		t.Fatalf("duplicateack deliveries %d -> %d %v", before, after, err)
	}
}

func TestSessionWizardLateCreationPreservesExplicitDisconnectIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	browse := wizardBrowseReadyTest(t, env)
	creating := wizardClickTest(t, env, 3, wizardCallbackTest(t, env, browse, "wizard_create", nil))
	wizardStartTest(t, env, 4, "disconnect")
	session := protocol.Session{ID: uuid.NewString(), ThreadID: "new-thread", Name: "My project", CWD: "/home/person/CODEX/My_project", WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), State: "idle", Loaded: true}
	event := wizardResultEvent(t, env, creating, "command_completed", protocol.Result{Session: &session})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	response := wizardLastResponse(t, env)
	if response.View != "session_created" {
		t.Fatalf("late creation falsely selected %#v", response)
	}
	if _, ok := findTestSelectedSession(t, env.store, telegramUpdate(env, 0)); ok {
		t.Fatal("disconnect overwritten")
	}
}

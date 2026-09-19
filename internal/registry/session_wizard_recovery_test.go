package registry

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func wizardDeletingTest(t *testing.T, env eventTestEnv) AcceptResult {
	t.Helper()
	if err := env.store.IngestEvent(context.Background(), env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	setTestSelection(t, env.store, telegramUpdate(env, 0), env.session)
	list := wizardStartTest(t, env, 1, "delete_session")
	confirm := wizardClickTest(t, env, 2, wizardCallbackTest(t, env, list, "delete_session_pick", func(c *Callback) { c.SessionID = env.session }))
	return wizardClickTest(t, env, 3, wizardCallbackTest(t, env, confirm, "delete_session_confirm", func(c *Callback) { c.SessionID = env.session }))
}

func TestPendingDeletionCanReleaseChatWithoutCancellingCommandIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	deleting := wizardDeletingTest(t, env)
	pending := wizardNameTest(t, env, 4, "Did the deletion work?")
	if pending.View != "session_deleting" || pending.ErrorCode != "wizard_pending" || pending.WizardID != deleting.WizardID {
		t.Fatalf("pending deletion has no current recovery controls: %#v", pending)
	}
	if r := wizardClickTest(t, env, 5, wizardCallbackTest(t, env, pending, "wizard_cancel", nil)); r.ErrorCode != "callback_invalid" {
		t.Fatalf("accepted deletion misleadingly cancelled: %#v", r)
	}
	dismiss := wizardCallbackTest(t, env, pending, "wizard_dismiss", nil)
	if r := wizardClickTest(t, env, 6, dismiss); r.View != "wizard_dismissed" {
		t.Fatalf("continue chat: %#v", r)
	}
	var status, commandID string
	if err := env.store.pool.QueryRow(context.Background(), `SELECT status FROM commands WHERE command_id=$1`, deleting.CommandID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("dismissing mutated accepted command: %q %v", status, err)
	}
	if err := env.store.pool.QueryRow(context.Background(), `SELECT command_id FROM telegram_session_wizards`).Scan(&commandID); err != nil || commandID != deleting.CommandID {
		t.Fatalf("dismissal lost result association: %q %v", commandID, err)
	}
	if r := wizardNameTest(t, env, 7, "continue the existing conversation"); r.View != "queued" {
		t.Fatalf("chat remains locked after dismissal: %#v", r)
	}
	failure := wizardResultEvent(t, env, deleting, "command_failed", protocol.Result{Error: &protocol.Error{Code: "delete_failed"}})
	if err := env.store.IngestEvent(context.Background(), env.worker, env.connection, failure); err != nil {
		t.Fatal(err)
	}
	if r := wizardLastResponse(t, env); r.View != "error" || r.ErrorCode != "delete_failed" {
		t.Fatalf("late failure reopened dismissed wizard: %#v", r)
	}
	if r := wizardClickTest(t, env, 8, dismiss); r.ErrorCode != "callback_invalid" {
		t.Fatalf("dismissal callback can be replayed: %#v", r)
	}
}

func TestDismissedCreationReportsSuccessWithoutChangingSelectionIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	setTestSelection(t, env.store, telegramUpdate(env, 0), env.session)
	browse := wizardBrowseReadyTest(t, env)
	creating := wizardClickTest(t, env, 3, wizardCallbackTest(t, env, browse, "wizard_create", nil))
	if r := wizardClickTest(t, env, 4, wizardCallbackTest(t, env, creating, "wizard_dismiss", nil)); r.View != "wizard_dismissed" {
		t.Fatalf("dismiss creation: %#v", r)
	}
	session := protocol.Session{ID: uuid.NewString(), ThreadID: "new-thread", Name: "My project", CWD: "/home/person/CODEX/My_project", WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), State: "idle", Loaded: true}
	event := wizardResultEvent(t, env, creating, "command_completed", protocol.Result{Session: &session})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	if r := wizardLastResponse(t, env); r.View != "session_created" || r.SessionID != session.ID {
		t.Fatalf("dismissed creation result lost: %#v", r)
	}
	if got := testSelectedSession(t, env.store, telegramUpdate(env, 0)); got != env.session {
		t.Fatalf("late creation changed selection after Continue chat: %s", got)
	}
}

func TestExpiredDeletionReleasesChatAndStillRecordsLateSuccessIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	deleting := wizardDeletingTest(t, env)
	if _, err := env.store.pool.Exec(ctx, `UPDATE commands SET status='dispatched',expires_at=$2 WHERE command_id=$1`, deleting.CommandID, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := env.store.ExpireCommands(ctx); err != nil {
		t.Fatal(err)
	}
	if r := wizardLastResponse(t, env); r.ErrorCode != "session_action_expired" || r.CommandID != deleting.CommandID {
		t.Fatalf("expired deletion response: %#v", r)
	}
	if r := wizardNameTest(t, env, 4, "back to the conversation"); r.View != "queued" {
		t.Fatalf("expiry retained wizard lock: %#v", r)
	}
	if err := env.store.ExpireCommands(ctx); err != nil {
		t.Fatal(err)
	}
	var notices int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE payload->>'error_code'='session_action_expired'`).Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("duplicate expiry replies=%d %v", notices, err)
	}
	sessions, err := env.store.SessionSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	session := sessions[0]
	session.Deleted, session.Archived, session.Loaded, session.State = true, true, false, "not_loaded"
	event := wizardResultEvent(t, env, deleting, "command_completed", protocol.Result{Session: &session})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	if r := wizardLastResponse(t, env); r.View != "session_deleted" {
		t.Fatalf("late success after lost acknowledgement was hidden: %#v", r)
	}
	if _, selected := findTestSelectedSession(t, env.store, telegramUpdate(env, 0)); selected {
		t.Fatal("confirmed late deletion retained session selection")
	}
}

func TestLegacyTerminalCommandCannotKeepWizardLockedIntegration(t *testing.T) {
	for _, status := range []string{"failed", "expired", "outcome_unknown"} {
		t.Run(status, func(t *testing.T) {
			env := newEventTestEnv(t)
			deleting := wizardDeletingTest(t, env)
			if _, err := env.store.pool.Exec(context.Background(), `UPDATE commands SET status=$2,error_code='unsupported_operation' WHERE command_id=$1`, deleting.CommandID, status); err != nil {
				t.Fatal(err)
			}
			if r := wizardNameTest(t, env, 4, "is the previous request finished?"); r.View != "error" || r.ErrorCode == "wizard_pending" {
				t.Fatalf("orphan wizard not reconciled: %#v", r)
			}
			if r := wizardNameTest(t, env, 5, "continue the conversation"); r.View != "queued" {
				t.Fatalf("legacy terminal command still locks chat: %#v", r)
			}
		})
	}
}

func TestDeleteConfirmationUsesUntitledSessionDirectoryLabelIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET name='',preview='',cwd='/work/Project' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	list := wizardStartTest(t, env, 1, "delete_session")
	confirm := wizardClickTest(t, env, 2, wizardCallbackTest(t, env, list, "delete_session_pick", func(c *Callback) { c.SessionID = env.session }))
	if confirm.SessionName != "Project" || confirm.CWD != "/work/Project" {
		t.Fatalf("confirmation disagrees with session list label: %#v", confirm)
	}
}

func TestUnsupportedDeletionResultReleasesChatIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	deleting := wizardDeletingTest(t, env)
	event := wizardResultEvent(t, env, deleting, "command_failed", protocol.Result{Error: &protocol.Error{Code: protocol.UnsupportedOperation}})
	if err := env.store.IngestEvent(context.Background(), env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	if r := wizardLastResponse(t, env); r.View != "error" || r.ErrorCode != protocol.UnsupportedOperation {
		t.Fatalf("unsupported operation offered a blocking retry: %#v", r)
	}
	if r := wizardNameTest(t, env, 4, "continue the conversation"); r.View != "queued" {
		t.Fatalf("unsupported operation still locks chat: %#v", r)
	}
}

func TestCreationCompletingAfterWizardDeadlineKeepsSelectionIntegration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	setTestSelection(t, env.store, telegramUpdate(env, 0), env.session)
	browse := wizardBrowseReadyTest(t, env)
	creating := wizardClickTest(t, env, 3, wizardCallbackTest(t, env, browse, "wizard_create", nil))
	if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_session_wizards SET expires_at=$1`, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	session := protocol.Session{ID: uuid.NewString(), ThreadID: "late-thread", Name: "My project", CWD: "/home/person/CODEX/My_project", WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), State: "idle", Loaded: true}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, wizardResultEvent(t, env, creating, "command_completed", protocol.Result{Session: &session})); err != nil {
		t.Fatal(err)
	}
	if r := wizardLastResponse(t, env); r.View != "session_created" || testSelectedSession(t, env.store, telegramUpdate(env, 0)) != env.session {
		t.Fatalf("expired workflow overwrote selection or lost result: %#v", r)
	}
}

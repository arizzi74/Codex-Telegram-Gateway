package registry

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func webUICreationTestEnv(t *testing.T) (eventTestEnv, WebUICreationTarget) {
	t.Helper()
	env := newEventTestEnv(t)
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE workers SET heartbeat_metadata='{"supports_session_workspaces":true,"supports_webui_session_creation":true}' WHERE worker_id=$1`, env.worker); err != nil {
		t.Fatal(err)
	}
	return env, WebUICreationTarget{WorkerID: env.worker, RuntimeID: env.runtime, RuntimeGeneration: 1, RequestID: uuid.New()}
}

func TestWebUICreationWithoutSessionsKeepsWorkerAndRuntimeScope(t *testing.T) {
	env, target := webUICreationTestEnv(t)
	ctx := t.Context()
	dashboard, err := env.store.AdminDashboardSnapshot(ctx)
	if err != nil || len(dashboard.Workers) != 1 || len(dashboard.Runtimes) != 1 || len(dashboard.Sessions) != 0 || !dashboard.Workers[0].SupportsWebUISessionCreation {
		t.Fatalf("fresh inventory: %+v %v", dashboard, err)
	}
	queued, err := env.store.QueueWebUISessionCreation(ctx, target, "My project", "~/CODEX/My_project")
	if err != nil || !queued.Pending || queued.CommandID != target.RequestID.String() {
		t.Fatalf("queue: %+v %v", queued, err)
	}
	duplicate, err := env.store.QueueWebUISessionCreation(ctx, target, "My project", "~/CODEX/My_project")
	if err != nil || duplicate.CommandID != queued.CommandID {
		t.Fatalf("duplicate: %+v %v", duplicate, err)
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 {
		t.Fatalf("commands: %+v %v", commands, err)
	}
	command := commands[0]
	if command.Operation != protocol.NewSession || command.WorkerID != env.worker.String() || command.RuntimeID != env.runtime.String() || command.SessionID != "" || command.ThreadID != "" || command.Arguments.SelectionRevision != nil || !command.Arguments.EnsureWorkspace || command.Arguments.CreateDirectory || command.Arguments.HistoryMode != "legacy" {
		t.Fatalf("mutable or wrong creation target: %+v", command)
	}
	if _, err := env.store.QueueWebUISessionCreation(ctx, target, "Other project", "~/CODEX/other"); err == nil {
		t.Fatal("same request identifier accepted changed work")
	}
	if _, err := env.store.WebUICreation(ctx, uuid.New(), target.RequestID, protocol.NewSession); !errors.Is(err, ErrWebUICreationNotFound) {
		t.Fatalf("cross-worker status: %v", err)
	}
	if _, err := env.store.WebUICreation(ctx, env.worker, target.RequestID, protocol.BrowseWorkspace); !errors.Is(err, ErrWebUICreationNotFound) {
		t.Fatalf("cross-operation status: %v", err)
	}
	if err := env.store.AcknowledgeCommand(ctx, env.worker, env.connection, protocol.CommandAck{CommandID: command.ID, Status: "accepted"}); err != nil {
		t.Fatal(err)
	}
	accepted, err := env.store.WebUICreation(ctx, env.worker, target.RequestID, protocol.NewSession)
	if err != nil || accepted.Status != "acknowledged" || !accepted.Pending || accepted.Session != nil {
		t.Fatalf("premature creation success: %+v %v", accepted, err)
	}
	session := protocol.Session{ID: uuid.NewString(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), ThreadID: "created", Name: "My project", CWD: "/home/worker/CODEX/My_project", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(protocol.Result{CommandID: command.ID, State: "completed", Session: &session})
	event := protocol.Event{ID: uuid.NewString(), Seq: 1, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, Kind: "command_completed", OccurredAt: time.Now().UTC(), Data: raw}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	completed, err := env.store.WebUICreation(ctx, env.worker, target.RequestID, protocol.NewSession)
	if err != nil || completed.Pending || completed.Status != "completed" || completed.Session == nil || completed.Session.ID != session.ID {
		t.Fatalf("creation outcome: %+v %v", completed, err)
	}
	// An outcome stays recoverable after the runtime has restarted, without
	// accepting the same UUID as a new execution against the new generation.
	if _, err := env.store.pool.Exec(ctx, `UPDATE runtimes SET generation=2`); err != nil {
		t.Fatal(err)
	}
	completedAgain, err := env.store.QueueWebUISessionCreation(ctx, target, "My project", "~/CODEX/My_project")
	if err != nil || completedAgain.Session == nil || completedAgain.Session.ID != session.ID {
		t.Fatalf("completed replay: %+v %v", completedAgain, err)
	}
	var bindings, deliveries, wizards, count int
	if err := env.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM telegram_bindings),(SELECT count(*) FROM telegram_deliveries),(SELECT count(*) FROM telegram_session_wizards),(SELECT count(*) FROM commands)`).Scan(&bindings, &deliveries, &wizards, &count); err != nil || bindings != 0 || deliveries != 0 || wizards != 0 || count != 1 {
		t.Fatalf("creation changed Telegram or replayed: %d/%d/%d/%d %v", bindings, deliveries, wizards, count, err)
	}
}

func TestWebUIWorkspaceReturnsValidatedWorkerPage(t *testing.T) {
	env, target := webUICreationTestEnv(t)
	ctx := t.Context()
	queued, err := env.store.QueueWebUIWorkspace(ctx, target, protocol.WorkspaceRequest{Path: "~/CODEX"})
	if err != nil || !queued.Pending {
		t.Fatalf("browse queue: %+v %v", queued, err)
	}
	page := protocol.WorkspacePage{Path: "/home/worker/CODEX", Parent: "/home/worker", Directories: []protocol.WorkspaceEntry{{Name: "project", Path: "/home/worker/CODEX/project"}}}
	raw, _ := json.Marshal(protocol.Result{CommandID: target.RequestID.String(), State: "completed", Workspace: &page})
	event := protocol.Event{ID: uuid.NewString(), Seq: 1, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, Kind: "command_completed", OccurredAt: time.Now().UTC(), Data: raw}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	result, err := env.store.WebUICreation(ctx, env.worker, target.RequestID, protocol.BrowseWorkspace)
	if err != nil || result.Pending || result.Workspace == nil || result.Workspace.Path != page.Path || result.Session != nil {
		t.Fatalf("browse result: %+v %v", result, err)
	}
}

func TestWebUICreationRejectsUnavailableStaleAndCrossWorkerTargets(t *testing.T) {
	for _, tc := range []struct{ name, query, code string }{
		{"disabled", `UPDATE workers SET enabled=FALSE`, "worker_unavailable"},
		{"offline", `UPDATE workers SET connectivity='unreachable'`, "worker_unavailable"},
		{"stopped runtime", `UPDATE runtimes SET state='stopped'`, "worker_unavailable"},
		{"changed generation", `UPDATE runtimes SET generation=2`, protocol.StaleRuntime},
		{"old worker", `UPDATE workers SET heartbeat_metadata='{}'`, protocol.UnsupportedOperation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, target := webUICreationTestEnv(t)
			if _, err := env.store.pool.Exec(t.Context(), tc.query); err != nil {
				t.Fatal(err)
			}
			_, err := env.store.QueueWebUISessionCreation(t.Context(), target, "Project", "~/CODEX/Project")
			var failure *protocol.Error
			if !errors.As(err, &failure) || failure.Code != tc.code {
				t.Fatalf("target rejection: %v", err)
			}
			commands, err := env.store.PendingCommands(t.Context(), 10)
			if err != nil || len(commands) != 0 {
				t.Fatalf("rejected work queued: %+v %v", commands, err)
			}
		})
	}
	env, target := webUICreationTestEnv(t)
	target.WorkerID = uuid.New()
	if _, err := env.store.QueueWebUISessionCreation(t.Context(), target, "Project", "~/CODEX/Project"); !errors.Is(err, ErrWebUICreationNotFound) {
		t.Fatalf("cross-worker target: %v", err)
	}
}

func TestWebUICreationUnknownAndFailureStayRecoverableWithoutPrivateErrors(t *testing.T) {
	for _, tc := range []struct{ status, code, expected string }{
		{"failed", "PRIVATE_CODE", "command_failed"},
		{"failed", protocol.CodexUnavailable, protocol.CodexUnavailable},
		{"failed", protocol.StaleRuntime, protocol.StaleRuntime},
		{"failed", protocol.InvalidWorkspace, protocol.InvalidWorkspace},
		{"outcome_unknown", "PRIVATE_CODE", "command_outcome_unknown"},
		{"completed", "", "command_outcome_unknown"},
		{"expired", "", "expired"},
	} {
		t.Run(tc.status+tc.code, func(t *testing.T) {
			env, target := webUICreationTestEnv(t)
			if _, err := env.store.QueueWebUISessionCreation(t.Context(), target, "Project", "~/CODEX/Project"); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.pool.Exec(t.Context(), `UPDATE commands SET status=$2,error_code=$3,error_message='PRIVATE_PATH /home/private' WHERE command_id=$1`, target.RequestID, tc.status, tc.code); err != nil {
				t.Fatal(err)
			}
			result, err := env.store.WebUICreation(t.Context(), env.worker, target.RequestID, protocol.NewSession)
			if err != nil || result.Pending || result.Session != nil || result.ErrorCode != tc.expected {
				t.Fatalf("outcome: %+v %v", result, err)
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "/home/private") {
				t.Fatalf("private worker error exposed: %s", raw)
			}
			if _, err := env.store.QueueWebUISessionCreation(t.Context(), target, "Project", "~/CODEX/Project"); err != nil {
				t.Fatalf("same request cannot recover outcome: %v", err)
			}
			var count int
			if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM commands`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("failure replay created work: %d %v", count, err)
			}
		})
	}
}

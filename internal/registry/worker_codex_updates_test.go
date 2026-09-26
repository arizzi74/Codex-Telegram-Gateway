package registry

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWorkerCodexReportsPreserveWorkerOutcomeAndSurviveRestart(t *testing.T) {
	for _, tc := range []struct {
		state, code string
	}{
		{"completed", ""}, {"up_to_date", ""}, {"failed", "check_failed"},
		{"unsupported", ""}, {"withheld", "rejected_release"}, {"no_runtimes", ""},
	} {
		t.Run(tc.state, func(t *testing.T) {
			ctx := context.Background()
			store := integrationStore(t)
			worker := updateTestWorker(t, store, "Worker")
			connection := uuid.New()
			if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
				t.Fatal(err)
			}
			updateTestAccept(t, store, updateTestInput(1))
			request := pendingUpdate(t, store, worker.ID)
			report := &protocol.CodexUpdateReport{
				State: tc.state, ErrorCode: tc.code, LatestVersion: "0.157.1", CheckedAt: time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC),
				Profiles: []protocol.CodexRuntimeVersion{{ProfileID: "main", InstalledVersion: "0.157.1", RunningVersion: "0.157.1", Support: "supported"}},
			}
			if tc.state == "unsupported" {
				report.Profiles[0].Support = "external"
			}
			if tc.state == "no_runtimes" {
				report.Profiles = nil
			}
			result := protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "completed", Version: "1.1.0", Codex: report}
			if err := store.IngestEvent(ctx, worker.ID, connection, workerUpdateEvent(t, worker.ID, 1, result)); err != nil {
				t.Fatal(err)
			}
			// A retry from an old manager must not erase the runtime report or
			// replace the independent successful worker outcome.
			late := protocol.WorkerUpdateResult{RequestID: request.RequestID, State: "failed", ErrorCode: "late_failure"}
			if err := store.IngestEvent(ctx, worker.ID, connection, workerUpdateEvent(t, worker.ID, 2, late)); err != nil {
				t.Fatal(err)
			}
			path := store.pool.path
			store.Close()
			store, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(store.Close)
			updates, err := store.WorkerUpdateSnapshot(ctx)
			if err != nil || len(updates) != 1 || updates[0].RequestID != request.RequestID || updates[0].State != "completed" || updates[0].Version != "1.1.0" || !reflect.DeepEqual(updates[0].Codex, report) {
				t.Fatalf("persisted worker/Codex outcomes changed: %+v %v", updates, err)
			}
			var raw []byte
			if err := store.pool.QueryRow(ctx, `SELECT payload FROM telegram_deliveries WHERE kind='ui_response' AND json_extract(payload,'$.view')='worker_update_result'`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var notice AcceptResult
			if err := json.Unmarshal(raw, &notice); err != nil || !reflect.DeepEqual(notice.WorkerUpdates, updates) {
				t.Fatalf("frozen completion lost report: %+v %v", notice, err)
			}
		})
	}
}

func TestWorkerCodexMigrationPreservesOldPendingAndCompletedRequests(t *testing.T) {
	ctx := context.Background()
	store := integrationStore(t)
	worker := updateTestWorker(t, store, "Pending worker")
	finished := updateTestWorker(t, store, "Finished worker")
	updateTestAccept(t, store, updateTestInput(1))
	pending := pendingUpdate(t, store, worker.ID)
	done := pendingUpdate(t, store, finished.ID)
	if _, err := store.pool.Exec(ctx, `UPDATE worker_update_requests SET state='up_to_date',version='1.0.0',completed_at=created_at WHERE request_id=$1`, done.RequestID); err != nil {
		t.Fatal(err)
	}
	// Recreate the version-15 schema without rewriting any migration checksum.
	for _, sql := range []string{`ALTER TABLE worker_update_requests DROP COLUMN codex_report`, `DELETE FROM schema_migrations WHERE version=16`} {
		if _, err := store.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if pendingUpdate(t, store, worker.ID) != pending {
		t.Fatal("migration changed pending identity")
	}
	updates, err := store.WorkerUpdateSnapshot(ctx)
	if err != nil || len(updates) != 2 {
		t.Fatalf("migration lost requests: %+v %v", updates, err)
	}
	for _, update := range updates {
		if update.Codex != nil || (update.WorkerID == finished.ID.String() && (update.State != "up_to_date" || update.Version != "1.0.0" || update.RequestID != done.RequestID)) {
			t.Fatalf("migration invented or changed results: %+v", update)
		}
	}
	var watchers int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM worker_update_watchers`).Scan(&watchers); err != nil || watchers != 2 {
		t.Fatalf("migration lost original subscribers: %d %v", watchers, err)
	}
	for _, raw := range []string{"not-json", "null", "[]", "123"} {
		if _, err := store.pool.Exec(ctx, `UPDATE worker_update_requests SET codex_report=$2 WHERE request_id=$1`, pending.RequestID, raw); err == nil {
			t.Fatalf("invalid persisted report accepted: %s", raw)
		}
	}
}

func TestCodexUpdateSummaryKeepsStatesAndVersionsDistinct(t *testing.T) {
	for _, tc := range []struct {
		state, code, want string
	}{
		{"completed", "", "updated"},
		{"up_to_date", "", "up to date"},
		{"failed", "check_failed", "version check failed"},
		{"failed", "update_failed", "runtime update failed"},
		{"failed", "inspection_failed", "could not be verified"},
		{"failed", "worker_update_failed", "installation skipped"},
		{"unsupported", "", "automatic updates unavailable"},
		{"withheld", "rejected_release", "previously failed validation"},
		{"no_runtimes", "", "no runtimes configured"},
	} {
		t.Run(tc.state+tc.code, func(t *testing.T) {
			w := WorkerUpdateStatus{State: "completed", Codex: &protocol.CodexUpdateReport{
				State: tc.state, ErrorCode: tc.code, LatestVersion: "0.157.1",
				Profiles: []protocol.CodexRuntimeVersion{{ProfileID: "pinned", InstalledVersion: "0.156.1", RunningVersion: "0.156.0", Support: "external"}},
			}}
			text := w.CodexSummary()
			for _, want := range []string{tc.want, "Latest stable: 0.157.1", "installed 0.156.1", "running 0.156.0", "externally managed; not updated"} {
				if !strings.Contains(text, want) {
					t.Fatalf("missing %q in %s", want, text)
				}
			}
		})
	}
	if text := (WorkerUpdateStatus{State: "pending"}).CodexSummary(); text != "" {
		t.Fatalf("unfinished request claims a runtime result: %s", text)
	}
	if text := (WorkerUpdateStatus{State: "completed"}).CodexSummary(); !strings.Contains(text, "not reported") {
		t.Fatalf("old worker claims runtime check: %s", text)
	}
}

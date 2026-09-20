package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/workerdb"
)

func asyncCacheTurns(count int, questionAt int) []map[string]any {
	turns := make([]map[string]any, count)
	for index := range turns {
		items := []map[string]any{}
		if index == questionAt {
			items = append(items, asyncRecoveryQuestion("saved-question"))
		}
		turns[index] = map[string]any{"id": fmt.Sprintf("turn-%03d", index), "status": "completed", "items": items}
	}
	return turns
}

func asyncCacheCalls(server *codextest.Server) []codextest.Call {
	var calls []codextest.Call
	for _, call := range server.Calls() {
		if call.Method == "thread/turns/list" {
			calls = append(calls, call)
		}
	}
	return calls
}

func TestAsyncHistoryRecoveryFindsRecentQuestionWithoutScanningOldTurns(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	actor := asyncRecoveryActor(t, a, runtime)
	approval := asyncRecoveryApproval(actor, "saved-question")
	approval.TurnID = "turn-119"
	actor.rememberAsyncQuestion(approval)
	turns := asyncCacheTurns(120, 119)
	turns[119]["items"] = []map[string]any{asyncRecoveryQuestion("saved-question"), asyncRecoveryUserInput(codexadapter.FormatAsyncQuestionAnswer("Which hardware?", "Mac"))}
	server.SetThreads([]map[string]any{{"id": actor.session.ThreadID, "turns": turns}}, nil)
	actor.recoverAsyncQuestions()
	if actor.asyncRecoveredGeneration != runtime.Generation || len(actor.asyncQuestions) != 0 {
		t.Fatalf("latest answer was not reconciled: %#v", actor.asyncQuestions)
	}
	if calls := asyncCacheCalls(server); len(calls) != 1 {
		t.Fatalf("recent question opened old history: %d requests", len(calls))
	}
}

func TestAsyncHistoryRecoveryResumesCompletedPagesAfterSQLiteReopen(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	path := filepath.Join(t.TempDir(), "recovery.db")
	store, err := OpenStore(path, a.store.workerID)
	if err != nil {
		t.Fatal(err)
	}
	a.store = store
	defer func() { _ = a.store.Close() }()
	actor := asyncRecoveryActor(t, a, runtime)
	approval := asyncRecoveryApproval(actor, "saved-question")
	approval.TurnID = "turn-000"
	actor.rememberAsyncQuestion(approval)
	turns := asyncCacheTurns(17, 0)
	turns[16]["items"] = []map[string]any{asyncRecoveryUserInput(codexadapter.FormatAsyncQuestionAnswer("Which hardware?", "Mac"))}
	server.SetThreads([]map[string]any{{"id": actor.session.ThreadID, "turns": turns}}, nil)
	actor.recoverAsyncQuestions()
	if actor.asyncRecoveredGeneration != 0 || len(actor.asyncQuestions) != 1 || len(asyncCacheCalls(server)) != asyncHistoryFreshPages {
		t.Fatalf("incomplete bounded recovery changed pending state: generation=%d pending=%d reads=%d", actor.asyncRecoveredGeneration, len(actor.asyncQuestions), len(asyncCacheCalls(server)))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	a.store, err = OpenStore(path, runtime.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3 && actor.asyncRecoveredGeneration == 0; attempt++ {
		before := len(asyncCacheCalls(server))
		actor.recoverAsyncQuestions()
		if used := len(asyncCacheCalls(server)) - before; used > asyncHistoryFreshPages {
			t.Fatalf("reconciliation exceeded fresh page budget: %d", used)
		}
	}
	if actor.asyncRecoveredGeneration != runtime.Generation || len(actor.asyncQuestions) != 0 {
		t.Fatalf("cached continuation did not finish: generation=%d pending=%#v", actor.asyncRecoveredGeneration, actor.asyncQuestions)
	}
	calls := asyncCacheCalls(server)
	if len(calls) != 19 {
		t.Fatalf("completed pages were reread after restart: got %d requests, want 17 turns plus 2 fresh head reads", len(calls))
	}
	counts := make(map[string]int)
	for _, call := range calls {
		var params struct{ Cursor string }
		_ = json.Unmarshal(call.Params, &params)
		counts[params.Cursor]++
	}
	for cursor, count := range counts {
		if cursor != "" && count != 1 {
			t.Fatalf("older completed cursor %q reread %d times", cursor, count)
		}
	}
}

func TestAsyncHistoryCacheInvalidatesWhenFreshHeadChangesAndOnDeletion(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	actor := asyncRecoveryActor(t, a, runtime)
	approval := asyncRecoveryApproval(actor, "saved-question")
	approval.TurnID = "turn-000"
	pending := map[string]asyncQuestionRecord{approval.RequestID: {Approval: approval, State: "pending"}}
	turns := asyncCacheTurns(17, 0)
	server.SetThreads([]map[string]any{{"id": actor.session.ThreadID, "turns": turns}}, nil)
	client, _, _ := a.manager.Client(runtime.ID)
	if _, err := actor.readPendingQuestionHistory(context.Background(), client, pending); !errors.Is(err, codexadapter.ErrHistoryUnavailable) {
		t.Fatalf("first bounded read = %v", err)
	}
	turns[16]["items"] = []map[string]any{asyncRecoveryUserInput("A new message changes this head")}
	server.SetThreads([]map[string]any{{"id": actor.session.ThreadID, "turns": turns}}, nil)
	before := len(asyncCacheCalls(server))
	if _, err := actor.readPendingQuestionHistory(context.Background(), client, pending); !errors.Is(err, codexadapter.ErrHistoryUnavailable) {
		t.Fatalf("second bounded read = %v", err)
	}
	newCalls := asyncCacheCalls(server)[before:]
	if len(newCalls) != asyncHistoryFreshPages {
		t.Fatalf("changed head reused stale cached pages: %d fresh requests", len(newCalls))
	}
	var secondParams struct{ Cursor string }
	_ = json.Unmarshal(newCalls[1].Params, &secondParams)
	if secondParams.Cursor != "1" {
		t.Fatalf("changed head resumed stale chain at %q", secondParams.Cursor)
	}
	if _, err := a.store.DeleteSession(runtime, actor.session); err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.View(func(tx *workerdb.Tx) error {
		prefix := asyncHistoryPrefix(runtime.ID, actor.session.ThreadID)
		key, _ := tx.Bucket(bucketMeta).Cursor().Seek(prefix)
		if bytes.HasPrefix(key, prefix) {
			return errors.New("deleted session retained cached question history")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncHistoryCacheNeverTreatsActiveOrUnknownTurnsAsCompleted(t *testing.T) {
	for _, state := range []string{"inProgress", "", "future"} {
		if cacheableAsyncHistoryPage(codexadapter.HistoryTurnPage{Turns: []codexadapter.HistoryTurn{{ID: "turn", Status: state}}}) {
			t.Fatalf("cached mutable or unknown turn state %q", state)
		}
	}
}

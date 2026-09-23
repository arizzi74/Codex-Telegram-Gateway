package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func activityChanged(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-changes:
		if !ok {
			t.Fatal("activity subscription closed")
		}
	case <-time.After(time.Second):
		t.Fatal("committed activity did not notify viewer")
	}
}
func activityUnchanged(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case <-changes:
		t.Fatal("unchanged/streamed activity woke viewer")
	default:
	}
}

func TestLiveSessionActivityTracksTurnsQuestionsAnswersAndVisibility(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	changes, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	first, err := env.store.LiveSessionActivity(ctx)
	if err != nil || len(first.Sessions) != 1 || first.Sessions[0].State != "idle" {
		t.Fatalf("initial: %+v %v", first, err)
	}
	progressEvent(t, env, 2, "turn_started", "active-turn", "")
	activityChanged(t, changes)
	running, err := env.store.LiveSessionActivity(ctx)
	if err != nil || running.Revision <= first.Revision || running.Sessions[0].ActiveTurnID != "active-turn" {
		t.Fatalf("running: %+v %v", running, err)
	}
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async-indicator", ThreadID: "thread-1", TurnID: "active-turn", Type: "input", Async: true, Questions: []protocol.Question{{ID: "question", Prompt: "PRIVATE QUESTION", Options: []string{"Yes"}}}}
	asyncQuestionEvent(t, env, 3, 1, "user_input_requested", approval)
	activityChanged(t, changes)
	question, err := env.store.LiveSessionActivity(ctx)
	if err != nil || question.Sessions[0].PendingQuestions != 1 || question.Sessions[0].State != "running" {
		t.Fatalf("question: %+v %v", question, err)
	}
	raw, _ := json.Marshal(question)
	if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "request_payload") || strings.Contains(string(raw), "/work") {
		t.Fatalf("private activity data: %s", raw)
	}
	// A Telegram answer is accepted before the worker emits its resolution.
	token := pendingQuestionCallback(t, env, env.session, uuid.MustParse(approval.ID), "input", "question", "Yes")
	in := telegramUpdate(env, 1)
	in.CallbackToken = token
	answer, err := env.store.AcceptTelegram(ctx, in)
	if err != nil || answer.CommandID == "" {
		t.Fatalf("answer: %+v %v", answer, err)
	}
	activityChanged(t, changes)
	answered, err := env.store.LiveSessionActivity(ctx)
	if err != nil || answered.Sessions[0].PendingQuestions != 0 {
		t.Fatalf("accepted answer remained yellow: %+v %v", answered, err)
	}
	approval.State = "approved"
	asyncQuestionEvent(t, env, 4, 1, "approval_resolved", approval)
	activityChanged(t, changes)
	progressEvent(t, env, 5, "turn_completed", "active-turn", "")
	activityChanged(t, changes)
	idle, err := env.store.LiveSessionActivity(ctx)
	if err != nil || idle.Sessions[0].ActiveTurnID != "" || idle.Sessions[0].State != "idle" {
		t.Fatalf("idle: %+v %v", idle, err)
	}
	discovered := env.discovery(t)
	discovered.ID = uuid.NewString()
	discovered.Seq = 6
	discovered.Kind = "session_state_changed"
	var session protocol.Session
	_ = json.Unmarshal(discovered.Data, &session)
	session.Archived = true
	discovered.Data, _ = json.Marshal(session)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, discovered); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	archived, err := env.store.LiveSessionActivity(ctx)
	if err != nil || len(archived.Sessions) != 0 {
		t.Fatalf("archived session remains visible: %+v %v", archived, err)
	}
	removed, err := env.store.SessionActivitySince(ctx, idle.Revision)
	if err != nil || len(removed.Events) != 1 || removed.Events[0].Event != "session_removed" || removed.Events[0].Session != nil || removed.Events[0].SessionID != env.session.String() {
		t.Fatalf("archive did not remove activity row: %+v %v", removed, err)
	}
}

func TestLiveSessionActivityIgnoresStreamingAndIdleHeartbeats(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	changes, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if _, err := env.store.LiveSessionActivity(ctx); err != nil {
		t.Fatal(err)
	}
	hb := Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}}
	for range 3 {
		if err := env.store.RecordHeartbeat(ctx, hb); err != nil {
			t.Fatal(err)
		}
	}
	activityUnchanged(t, changes)
	progressEvent(t, env, 2, "turn_started", "turn", "")
	activityChanged(t, changes)
	progressEvent(t, env, 3, "agent_progress_message", "turn", "")
	progressEvent(t, env, 4, "tool_progress_message", "turn", "")
	activityUnchanged(t, changes)
	for range 1000 {
		env.store.notifySessionActivity()
	}
	activityChanged(t, changes)
	activityUnchanged(t, changes)
	hb.Runtimes[0].State = "failed"
	if err := env.store.RecordHeartbeat(ctx, hb); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	snapshot, err := env.store.LiveSessionActivity(ctx)
	if err != nil || snapshot.Sessions[0].RuntimeState != "failed" {
		t.Fatalf("runtime failure: %+v %v", snapshot, err)
	}
	if err := env.store.Disconnect(ctx, env.worker, env.connection); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	snapshot, err = env.store.LiveSessionActivity(ctx)
	if err != nil || snapshot.Sessions[0].WorkerConnectivity != "unreachable" {
		t.Fatalf("disconnect: %+v %v", snapshot, err)
	}
	// Callers cannot modify the shared cache backing another viewer.
	snapshot.Sessions[0].State = "corrupted"
	snapshot, err = env.store.LiveSessionActivity(ctx)
	if err != nil || snapshot.Sessions[0].State == "corrupted" {
		t.Fatal("viewer mutated shared snapshot")
	}
}

func TestLiveSessionActivityFiltersStaleAndAnsweredQuestions(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	question := protocol.Question{ID: "q", Prompt: "Question"}
	insertPendingQuestion(t, env, env.session, "", true, question)
	insertPendingQuestion(t, env, env.session, "", false) // Ordinary command approval.
	insertPendingQuestion(t, env, env.session, "previous-turn", false, question)
	answered := insertPendingQuestion(t, env, env.session, "", true, question)
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET input_answers='{"q":["Done"]}' WHERE approval_id=$1`, answered); err != nil {
		t.Fatal(err)
	}
	stale := insertPendingQuestion(t, env, env.session, "", true, question)
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET runtime_generation=0 WHERE approval_id=$1`, stale); err != nil {
		t.Fatal(err)
	}
	snapshot, err := env.store.LiveSessionActivity(ctx)
	if err != nil || len(snapshot.Sessions) != 1 || snapshot.Sessions[0].PendingQuestions != 2 {
		t.Fatalf("pending filters: %+v %v", snapshot, err)
	}
	rows, err := env.store.pool.Query(ctx, "EXPLAIN QUERY PLAN "+sessionActivityQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if !strings.Contains(plan.String(), "sessions_visible_activity_idx") || !strings.Contains(plan.String(), "approvals_session_pending_idx") || strings.Contains(plan.String(), "events") || strings.Contains(plan.String(), "commands") {
		t.Fatalf("activity query inspects history or misses pending index:\n%s", plan.String())
	}
}

func TestLiveSessionActivitySubscriberLimitAndClose(t *testing.T) {
	store := integrationStore(t)
	var cancels []func()
	var channels []<-chan struct{}
	for range 128 {
		channel, cancel, err := store.SubscribeSessionActivity()
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, cancel)
		channels = append(channels, channel)
	}
	if _, _, err := store.SubscribeSessionActivity(); !errors.Is(err, ErrSessionActivityLimit) {
		t.Fatalf("unbounded viewers: %v", err)
	}
	cancels[0]()
	cancels[0]()
	channel, cancel, err := store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	store.Close()
	for _, ch := range append(channels, channel) {
		if _, ok := <-ch; ok {
			t.Fatal("store close retained subscriber")
		}
	}
}

func TestLiveSessionActivityPartialAnswersChangePendingRevision(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async-partial", ThreadID: "thread-1", Type: "input", Async: true, Questions: []protocol.Question{
		{ID: "first", Prompt: "First private question", Options: []string{"Private answer"}},
		{ID: "second", Prompt: "Second private question"},
	}}
	asyncQuestionEvent(t, env, 2, 1, "user_input_requested", approval)
	before, err := env.store.LiveSessionActivity(ctx)
	if err != nil || before.Sessions[0].PendingQuestions != 1 || before.Sessions[0].PendingRevision == "" {
		t.Fatalf("initial pending revision: %+v %v", before, err)
	}
	changes, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	in := telegramUpdate(env, 1)
	in.CallbackToken = pendingQuestionCallback(t, env, env.session, uuid.MustParse(approval.ID), "input", "first", "Private answer")
	if _, err := env.store.AcceptTelegram(ctx, in); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	after, err := env.store.LiveSessionActivity(ctx)
	if err != nil || after.Sessions[0].PendingQuestions != 1 || after.Sessions[0].PendingRevision == before.Sessions[0].PendingRevision {
		t.Fatalf("partial answer did not change revision: %+v %v", after, err)
	}
	raw, _ := json.Marshal(after)
	if strings.Contains(string(raw), "Private") || strings.Contains(string(raw), "first") || strings.Contains(string(raw), approval.ID) {
		t.Fatalf("pending signature leaked question/answer details: %s", raw)
	}
}

func TestPendingActivitySignatureIsStableAcrossAggregationOrder(t *testing.T) {
	first := []byte(`[{"id":"b","answered":["second","first"]},{"id":"a","answered":[]}]`)
	second := []byte(`[{"id":"a","answered":[]},{"id":"b","answered":["first","second"]}]`)
	count, before, err := pendingActivitySignature(first)
	if err != nil || count != 2 || len(before) != 64 {
		t.Fatalf("signature: count=%d revision=%q err=%v", count, before, err)
	}
	count, after, err := pendingActivitySignature(second)
	if err != nil || count != 2 || after != before {
		t.Fatalf("query plan order changed revision: %q -> %q (%v)", before, after, err)
	}
}

func TestSessionActivitySinceRetainsRapidEdgesAndDoesNotReplay(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	changes, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	initial, err := env.store.LiveSessionActivity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	started := progressEvent(t, env, 2, "turn_started", "rapid-turn", "")
	question := protocol.Approval{ID: uuid.NewString(), RequestID: "request", ThreadID: "thread-1", TurnID: "rapid-turn", Type: "input", Async: true, Questions: []protocol.Question{{ID: "private-id", Prompt: "PRIVATE PROMPT"}}}
	asyncQuestionEvent(t, env, 3, 1, "user_input_requested", question)
	question.State = "approved"
	asyncQuestionEvent(t, env, 4, 1, "approval_resolved", question)
	progressEvent(t, env, 5, "turn_completed", "rapid-turn", "")
	// No consumer drained the single-slot wakeup channel between these commits.
	activityChanged(t, changes)
	activityUnchanged(t, changes)
	update, err := env.store.SessionActivitySince(ctx, initial.Revision)
	if err != nil || update.Reset || len(update.Events) != 4 || update.Snapshot.Sessions[0].ActiveTurnID != "" {
		t.Fatalf("rapid transitions lost: %+v %v", update, err)
	}
	want := []string{"turn_started", "question_requested", "question_resolved", "turn_ended"}
	previous := initial.Revision
	for index, event := range update.Events {
		if event.Event != want[index] || event.Session == nil || event.SessionID != env.session.String() || event.Revision <= previous || event.Revision > update.Snapshot.Revision {
			t.Fatalf("edge %d: %+v", index, event)
		}
		previous = event.Revision
	}
	if update.Events[0].Session.ActiveTurnID != "rapid-turn" || update.Events[1].Session.PendingQuestions != 1 || update.Events[2].Session.PendingQuestions != 0 || update.Events[3].Session.ActiveTurnID != "" {
		t.Fatalf("ring contains final rows instead of committed rows: %+v", update)
	}
	raw, _ := json.Marshal(update.Events)
	if strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "private-id") || strings.Contains(string(raw), "/work") {
		t.Fatalf("ring leaked conversation data: %s", raw)
	}
	update.Events[0].Session.State = "corrupted"
	again, err := env.store.SessionActivitySince(ctx, initial.Revision)
	if err != nil || again.Events[0].Session.State == "corrupted" {
		t.Fatal("caller can mutate retained rows")
	}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, started); err != nil {
		t.Fatal(err)
	}
	activityUnchanged(t, changes)
	again, err = env.store.SessionActivitySince(ctx, update.Snapshot.Revision)
	if err != nil || len(again.Events) != 0 || again.Snapshot.Revision != update.Snapshot.Revision {
		t.Fatalf("replay republished an edge: %+v %v", again, err)
	}
}

func TestSessionActivitySinceOverflowAndStaleGeneration(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	_, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	initial, err := env.store.LiveSessionActivity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for index := range sessionActivityRingSize + 2 {
		kind := "turn_started"
		if index%2 == 1 {
			kind = "turn_completed"
		}
		progressEvent(t, env, uint64(index+2), kind, "turn", "")
	}
	update, err := env.store.SessionActivitySince(ctx, initial.Revision)
	if err != nil || !update.Reset || len(update.Events) != 0 || update.Snapshot.Sessions[0].ActiveTurnID != "" {
		t.Fatalf("overflow must replace incomplete replay with snapshot: %+v %v", update, err)
	}
	if len(env.store.activity.events) != sessionActivityRingSize {
		t.Fatalf("unbounded activity ring: %d", len(env.store.activity.events))
	}
	latest, err := env.store.SessionActivitySince(ctx, update.Snapshot.Revision-1)
	if err != nil || latest.Reset || len(latest.Events) != 1 || latest.Events[0].Event != "turn_ended" {
		t.Fatalf("retained cursor unnecessarily reset: %+v %v", latest, err)
	}
	if err := env.store.RecordHeartbeat(ctx, Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []Runtime{{ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 2, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	progressEventAtGeneration(t, env, sessionActivityRingSize+4, 1, "turn_started", "stale-turn", "")
	stale, err := env.store.SessionActivitySince(ctx, update.Snapshot.Revision)
	if err != nil || len(stale.Events) != 0 || stale.Snapshot.Sessions[0].ActiveTurnID != "" {
		t.Fatalf("stale generation republished activity: %+v %v", stale, err)
	}
}

func TestSessionActivitySinceDoesNotExposeHiddenSessionsOrRetainUnobservedEdges(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := t.Context()
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, env.discovery(t)); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "turn_started", "unobserved-turn", "")
	progressEvent(t, env, 3, "turn_completed", "unobserved-turn", "")
	if len(env.store.activity.events) != 0 {
		t.Fatal("unobserved commits retained an unnecessary activity replay")
	}
	_, cancel, err := env.store.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	initial, err := env.store.LiveSessionActivity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Helpers and deleted sessions are represented as archived rows by the
	// worker projection. A newly discovered hidden row must never be replayed.
	hidden := env.discovery(t)
	hidden.ID, hidden.Seq, hidden.SessionID = uuid.NewString(), 4, uuid.NewString()
	var session protocol.Session
	_ = json.Unmarshal(hidden.Data, &session)
	session.ID, session.ThreadID, session.Archived = hidden.SessionID, "hidden-child-thread", true
	hidden.Data, _ = json.Marshal(session)
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, hidden); err != nil {
		t.Fatal(err)
	}
	update, err := env.store.SessionActivitySince(ctx, initial.Revision)
	if err != nil || len(update.Events) != 0 || len(update.Snapshot.Sessions) != 1 || update.Snapshot.Sessions[0].SessionID != env.session.String() {
		t.Fatalf("hidden session appeared in activity: %+v %v", update, err)
	}
}

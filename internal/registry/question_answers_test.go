package registry

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func questionAnswerEdits(t *testing.T, store *Store) map[int64]QuestionAnswerEdit {
	t.Helper()
	rows, err := store.pool.Query(context.Background(), `SELECT payload,event_id FROM telegram_deliveries WHERE kind='question_answered' AND visibility_revoked=0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	edits := make(map[int64]QuestionAnswerEdit)
	for rows.Next() {
		var raw []byte
		var event *string
		if err := rows.Scan(&raw, &event); err != nil {
			t.Fatal(err)
		}
		if event != nil {
			t.Fatal("answer edit is coupled to event visibility")
		}
		var edit QuestionAnswerEdit
		if err := json.Unmarshal(raw, &edit); err != nil {
			t.Fatal(err)
		}
		if _, duplicate := edits[edit.MessageID]; duplicate {
			t.Fatalf("duplicate edit: %#v", edit)
		}
		edits[edit.MessageID] = edit
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return edits
}

func TestQuestionAnswerPartialEditsAllCopiesAndPreservesSelection(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	approval := insertPendingQuestion(t, env, env.session, "", true,
		protocol.Question{ID: "first", Prompt: "Does the pointer disappear?"},
		protocol.Question{ID: "second", Prompt: "Which version?"})
	for _, route := range []struct {
		chat, message int64
		question      string
	}{{20, 101, "first"}, {20, 102, "first"}, {30, 103, "first"}, {20, 104, "second"}} {
		if err := env.store.RecordBotInputRoute(ctx, "bot", route.chat, route.message, env.session, "", approval, route.question); err != nil {
			t.Fatal(err)
		}
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	acceptModeUpdate(t, env, 10, "select", other.String(), "")
	answer := telegramUpdate(env, 11)
	answer.Action, answer.ReplyToMessageID, answer.Text = "text", 102, "Yes, while captured."
	result, err := env.store.AcceptTelegram(ctx, answer)
	if err != nil || result.View != "input_pending" {
		t.Fatalf("partial: %+v %v", result, err)
	}
	want := map[int64]QuestionAnswerEdit{}
	for _, id := range []int64{101, 102, 103} {
		want[id] = QuestionAnswerEdit{MessageID: id, Question: "Does the pointer disappear?", Answer: answer.Text}
	}
	if got := questionAnswerEdits(t, env.store); !reflect.DeepEqual(got, want) {
		t.Fatalf("edits=%#v", got)
	}
	if _, err := env.store.AcceptTelegram(ctx, answer); err != nil {
		t.Fatal(err)
	}
	if got := questionAnswerEdits(t, env.store); !reflect.DeepEqual(got, want) {
		t.Fatalf("replay changed edits=%#v", got)
	}
	var selected string
	if err := env.store.pool.QueryRow(ctx, `SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND chat_id=20`).Scan(&selected); err != nil || selected != other.String() {
		t.Fatalf("selection=%s %v", selected, err)
	}
	answer.UpdateID, answer.ReplyToMessageID, answer.Text = 12, 104, "1.2.3"
	if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || result.CommandID == "" {
		t.Fatalf("complete=%+v %v", result, err)
	}
	want[104] = QuestionAnswerEdit{MessageID: 104, Question: "Which version?", Answer: answer.Text}
	if got := questionAnswerEdits(t, env.store); !reflect.DeepEqual(got, want) {
		t.Fatalf("complete edits=%#v", got)
	}
}

func TestQuestionAnswerTerminalSnapshotShrinkAndLateRoute(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:question", ThreadID: "thread-1", TurnID: "question-turn", Type: "user_input", Async: true, Questions: []protocol.Question{{ID: "first", Prompt: "Original first?"}, {ID: "second", Prompt: "Second?", Secret: true}}}
	asyncQuestionEvent(t, env, 2, 1, "user_input_requested", approval)
	rows, err := env.store.ClaimDeliveries(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var original Delivery
	for _, row := range rows {
		if row.Kind == "user_input_requested" {
			original = row
		}
	}
	if original.ID == "" {
		t.Fatal("no original question delivery")
	}
	if _, err := env.store.PrepareDeliveryChunks(ctx, original.ID, []json.RawMessage{json.RawMessage(`{"text":"Original first?"}`)}); err != nil {
		t.Fatal(err)
	}
	// The worker has removed the first question from its live pending snapshot.
	approval.Questions = approval.Questions[1:]
	asyncQuestionEvent(t, env, 3, 1, "user_input_requested", approval)
	approval.Answers = map[string][]string{"first": {"One"}, "second": {"private value"}}
	approval.State = "answered"
	event := asyncQuestionEvent(t, env, 4, 1, "user_input_answered", approval)
	if len(questionAnswerEdits(t, env.store)) != 0 {
		t.Fatal("edit sent before any question was checkpointed")
	}
	if err := env.store.MarkDeliveryChunkSent(ctx, original.ID, 0, 501, env.session.String(), approval.TurnID, approval.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 502, env.session, approval.TurnID, uuid.MustParse(approval.ID), "second"); err != nil {
		t.Fatal(err)
	}
	want := map[int64]QuestionAnswerEdit{501: {MessageID: 501, Question: "Original first?", Answer: "One"}, 502: {MessageID: 502, Question: "Second?", Answer: "[REDACTED]"}}
	if got := questionAnswerEdits(t, env.store); !reflect.DeepEqual(got, want) {
		t.Fatalf("late edits=%#v", got)
	}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	if got := questionAnswerEdits(t, env.store); !reflect.DeepEqual(got, want) {
		t.Fatalf("event replay=%#v", got)
	}
	// A later repeated answer does not replace the accepted response.
	approval.Answers = map[string][]string{"first": {"Changed by replay"}}
	asyncQuestionEvent(t, env, 5, 1, "user_input_answered", approval)
	if got := questionAnswerEdits(t, env.store); !reflect.DeepEqual(got, want) {
		t.Fatalf("repeated answer=%#v", got)
	}
}

func TestQuestionAnswerRejectsMismatchedIdentityAndUnknownQuestion(t *testing.T) {
	for _, mutate := range []struct {
		name string
		fn   func(*protocol.Approval)
	}{
		{"request", func(a *protocol.Approval) { a.RequestID = "other" }},
		{"thread", func(a *protocol.Approval) { a.ThreadID = "other" }},
		{"turn", func(a *protocol.Approval) { a.TurnID = "other" }},
		{"approval", func(a *protocol.Approval) { a.ID = uuid.NewString() }},
		{"type", func(a *protocol.Approval) { a.Type = "command" }},
		{"question", func(a *protocol.Approval) { a.Answers = map[string][]string{"unknown": {"answer"}} }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			a := protocol.Approval{ID: uuid.NewString(), RequestID: "async:question", ThreadID: "thread-1", TurnID: "turn", Type: "user_input", Async: true, Questions: []protocol.Question{{ID: "q", Prompt: "Q?"}}}
			asyncQuestionEvent(t, env, 2, 1, "user_input_requested", a)
			a.Answers = map[string][]string{"q": {"answer"}}
			mutate.fn(&a)
			raw, _ := json.Marshal(a)
			e := protocol.Event{ID: uuid.NewString(), Seq: 3, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 1, SessionID: env.session.String(), Kind: "user_input_answered", OccurredAt: time.Now().UTC(), Data: raw}
			if err := env.store.IngestEvent(ctx, env.worker, env.connection, e); !errors.Is(err, ErrEventTarget) {
				t.Fatalf("invalid event accepted: %v", err)
			}
			var count int
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_question_answers WHERE answer IS NOT NULL`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalid answer persisted: %d %v", count, err)
			}
		})
	}
}

func TestQuestionAnswerRetryReplacesDraftAndRevokesOldEdit(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	id := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q", Prompt: "Q?"})
	if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 701, env.session, "", id, "q"); err != nil {
		t.Fatal(err)
	}
	answer := telegramUpdate(env, 10)
	answer.Action, answer.ReplyToMessageID, answer.Text = "text", 701, "Original answer"
	first, err := env.store.AcceptTelegram(ctx, answer)
	if err != nil || first.CommandID == "" {
		t.Fatalf("first=%+v %v", first, err)
	}
	claimed, err := env.store.ClaimDeliveries(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var oldEdit string
	for _, row := range claimed {
		if row.Kind == "question_answered" {
			oldEdit = row.ID
		}
	}
	if oldEdit == "" {
		t.Fatal("no old edit")
	}
	tx, err := env.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE commands SET status='failed',error_code='invalid_argument' WHERE command_id=$1`, first.CommandID); err != nil {
		t.Fatal(err)
	}
	if err := recoverAsyncQuestionResponse(ctx, tx, uuid.MustParse(first.CommandID)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	answer.UpdateID, answer.Text = 11, "Corrected answer"
	if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || result.CommandID == "" {
		t.Fatalf("retry=%+v %v", result, err)
	}
	if suppressed, err := env.store.SuppressTelegramDelivery(ctx, oldEdit); err != nil || !suppressed {
		t.Fatalf("old leased edit not suppressed: %v %v", suppressed, err)
	}
	want := map[int64]QuestionAnswerEdit{701: {MessageID: 701, Question: "Q?", Answer: "Corrected answer"}}
	if got := questionAnswerEdits(t, env.store); !reflect.DeepEqual(got, want) {
		t.Fatalf("retry edits=%#v", got)
	}
}

func TestQuestionAnswerHistoricalGenerationOnlyUpdatesSavedRequest(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:historical", ThreadID: "thread-1", TurnID: "old-turn", Type: "user_input", Async: true, Questions: []protocol.Question{{ID: "q", Prompt: "Old question?"}}}
	asyncQuestionEvent(t, env, 2, 1, "user_input_requested", approval)
	if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 801, env.session, "old-turn", uuid.MustParse(approval.ID), "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, env.runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='new-turn',state='running' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET state='cleared' WHERE approval_id=$1`, approval.ID); err != nil {
		t.Fatal(err)
	}
	approval.Answers = map[string][]string{"q": {"Native terminal answer"}}
	asyncQuestionEvent(t, env, 3, 1, "user_input_answered", approval)
	assertAsyncSessionState(t, env, "running", "new-turn")
	want := map[int64]QuestionAnswerEdit{801: {MessageID: 801, Question: "Old question?", Answer: "Native terminal answer"}}
	if got := questionAnswerEdits(t, env.store); !reflect.DeepEqual(got, want) {
		t.Fatalf("historical edits=%#v", got)
	}
	var state string
	if err := env.store.pool.QueryRow(ctx, `SELECT state FROM approvals WHERE approval_id=$1`, approval.ID).Scan(&state); err != nil || state != "cleared" {
		t.Fatalf("old approval revived: %s %v", state, err)
	}
	// The old answer may not be relabelled as a current-generation request.
	raw, _ := json.Marshal(approval)
	event := protocol.Event{ID: uuid.NewString(), Seq: 4, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), RuntimeGeneration: 2, SessionID: env.session.String(), Kind: "user_input_answered", OccurredAt: time.Now().UTC(), Data: raw}
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, event); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("generation relabel accepted: %v", err)
	}
}

func TestQuestionAnswerMigrationPreservesExistingPendingQuestions(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := context.Background()
	id := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q", Prompt: "Existing question?", Secret: true})
	// Reproduce a version-nine database with an existing request, then apply the
	// production migration rather than creating the new rows through events.
	for _, query := range []string{`DROP TABLE telegram_question_answer_edits`, `DROP TABLE telegram_question_answers`, `DROP INDEX bot_message_routes_question_idx`, `DELETE FROM schema_migrations WHERE version=10`} {
		if _, err := env.store.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	var answer *string
	var version int
	if err := env.store.pool.QueryRow(ctx, `SELECT question,answer,version FROM telegram_question_answers WHERE approval_id=$1 AND question_id='q'`, id).Scan(&raw, &answer, &version); err != nil {
		t.Fatal(err)
	}
	var question protocol.Question
	if err := json.Unmarshal(raw, &question); err != nil || question.Prompt != "Existing question?" || !question.Secret || answer != nil || version != 0 {
		t.Fatalf("migration changed request: %+v %v %d %v", question, answer, version, err)
	}
}

func TestQuestionAnswerPromptAndDismissalDoNotInventAnswers(t *testing.T) {
	for _, action := range []string{"input_prompt", "dismiss_input"} {
		t.Run(action, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := context.Background()
			id := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q", Prompt: "Q?"})
			if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 901, env.session, "", id, "q"); err != nil {
				t.Fatal(err)
			}
			update := telegramUpdate(env, 2)
			update.CallbackToken = pendingQuestionCallback(t, env, env.session, id, action, "q", "")
			if result, err := env.store.AcceptTelegram(ctx, update); err != nil || result.ErrorCode != "" {
				t.Fatalf("callback: %+v %v", result, err)
			}
			if edits := questionAnswerEdits(t, env.store); len(edits) != 0 {
				t.Fatalf("non-answer edited question: %#v", edits)
			}
		})
	}
}

package registry

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func queueInputReplyForTest(t *testing.T, env eventTestEnv, approval uuid.UUID, question string, chat, topic int64, textReply bool) uuid.UUID {
	t.Helper()
	tx, err := env.store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	var before int64
	if err = tx.QueryRow(t.Context(), `SELECT COALESCE(max(rowid),0) FROM telegram_deliveries`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	err = queueUIResponse(t.Context(), tx, IncomingUpdate{BotID: "bot", ChatID: chat, TopicID: topic, UserID: 10}, AcceptResult{View: "input_prompt", TextReply: textReply, ApprovalID: approval.String(), QuestionID: question, SessionID: env.session.String(), RuntimeID: env.runtime.String(), UserID: 10})
	if err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err = tx.QueryRow(t.Context(), `SELECT delivery_id FROM telegram_deliveries WHERE rowid>$1`, before).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return id
}

func checkpointInputReplyForTest(t *testing.T, env eventTestEnv, id uuid.UUID, approval uuid.UUID, question string, messages ...int64) {
	t.Helper()
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE telegram_deliveries SET status='sending',next_attempt_at=$2 WHERE delivery_id=$1`, id, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	parts := make([]json.RawMessage, len(messages))
	for i := range parts {
		parts[i] = json.RawMessage(`{"text":"Temporary input helper"}`)
	}
	if _, err := env.store.PrepareDeliveryChunks(t.Context(), id.String(), parts); err != nil {
		t.Fatal(err)
	}
	for i, message := range messages {
		if err := env.store.MarkDeliveryChunkSent(t.Context(), id.String(), i, message, env.session.String(), "", approval.String(), question); err != nil {
			t.Fatal(err)
		}
	}
}

func inputReplyCleanupMessages(t *testing.T, s *Store) map[int64]string {
	t.Helper()
	// Draining the indexed worklist is part of normal gateway delivery polling.
	if _, err := s.ClaimDeliveries(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	rows, err := s.pool.Query(t.Context(), `SELECT json_extract(payload,'$.message_id'),status FROM telegram_deliveries WHERE kind='input_reply_cleanup'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[int64]string{}
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatal(err)
		}
		if _, ok := result[id]; ok {
			t.Fatalf("duplicate helper deletion %d", id)
		}
		result[id] = state
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func helperRetired(t *testing.T, s *Store, id uuid.UUID) bool {
	t.Helper()
	var retired bool
	if err := s.pool.QueryRow(t.Context(), `SELECT retired FROM telegram_input_reply_helpers WHERE delivery_id=$1`, id).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	return retired
}

func TestInputReplyCleanupPartialAnswerPreservesOriginalAndOtherFields(t *testing.T) {
	for _, viaButton := range []bool{false, true} {
		t.Run(fmt.Sprint("button=", viaButton), func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := t.Context()
			approval := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q1", Prompt: "First?", Options: []string{"Yes"}}, protocol.Question{ID: "q2", Prompt: "Second?"})
			first := queueInputReplyForTest(t, env, approval, "q1", 20, 0, true)
			checkpointInputReplyForTest(t, env, first, approval, "q1", 100, 101)
			other := queueInputReplyForTest(t, env, approval, "q2", 20, 0, true)
			checkpointInputReplyForTest(t, env, other, approval, "q2", 102)
			// This ordinary question copy also has input_prompt but no TextReply flag.
			original := queueInputReplyForTest(t, env, approval, "q1", 20, 0, false)
			checkpointInputReplyForTest(t, env, original, approval, "q1", 99)
			in := telegramUpdate(env, 1000)
			in.Action, in.Text, in.ReplyToMessageID = "text", "Yes", 101
			if viaButton {
				in.Action = ""
				in.CallbackToken = pendingQuestionCallback(t, env, env.session, approval, "input", "q1", "Yes")
				in.ReplyToMessageID = 0
			}
			result, err := env.store.AcceptTelegram(ctx, in)
			if err != nil || result.View != "input_pending" {
				t.Fatalf("answer: %+v %v", result, err)
			}
			got := inputReplyCleanupMessages(t, env.store)
			if len(got) != 2 || got[100] == "" || got[101] == "" || helperRetired(t, env.store, other) {
				t.Fatalf("partial cleanup: %#v", got)
			}
			edits := questionAnswerEdits(t, env.store)
			if len(edits) != 1 || edits[99].Answer != "Yes" {
				t.Fatalf("original/helper edit mix: %+v", edits)
			}
			if replay, err := env.store.AcceptTelegram(ctx, in); err != nil || !replay.Duplicate {
				t.Fatal("answer replay changed delivery")
			}
			if len(inputReplyCleanupMessages(t, env.store)) != 2 {
				t.Fatal("replay repeated helper cleanup")
			}
			// Keep the helper route so a late reply cannot become a fresh session prompt.
			late := telegramUpdate(env, 1001)
			late.Action, late.ReplyToMessageID, late.Text = "text", 101, "late text"
			if got, err := env.store.AcceptTelegram(ctx, late); err != nil || got.ErrorCode == "" || got.CommandID != "" {
				t.Fatalf("late helper reply became prompt: %+v %v", got, err)
			}
		})
	}
}

func TestInputReplyCleanupDismissalAndTerminalResolution(t *testing.T) {
	for _, kind := range []string{"telegram-dismiss", "terminal-resolved", "terminal-answer"} {
		t.Run(kind, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			approval := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q1", Prompt: "First?"}, protocol.Question{ID: "q2", Prompt: "Second?"})
			for i, q := range []string{"q1", "q2"} {
				id := queueInputReplyForTest(t, env, approval, q, 20, 0, true)
				checkpointInputReplyForTest(t, env, id, approval, q, int64(200+i))
				if err := env.store.RecordBotInputRoute(t.Context(), "bot", 20, int64(210+i), env.session, "", approval, q); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "telegram-dismiss":
				in := telegramUpdate(env, 2000)
				in.CallbackToken = pendingQuestionCallback(t, env, env.session, approval, "dismiss_input", "", "")
				if result, err := env.store.AcceptTelegram(t.Context(), in); err != nil || result.CommandID == "" {
					t.Fatalf("dismiss: %+v %v", result, err)
				}
			default:
				var raw []byte
				if err := env.store.pool.QueryRow(t.Context(), `SELECT request_payload FROM approvals WHERE approval_id=$1`, approval).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var input protocol.Approval
				if err := json.Unmarshal(raw, &input); err != nil {
					t.Fatal(err)
				}
				eventKind := "approval_resolved"
				input.State = "cleared"
				if kind == "terminal-answer" {
					eventKind = "user_input_answered"
					input.Answers = map[string][]string{"q1": {"One"}, "q2": {"Two"}}
				}
				raw, _ = json.Marshal(input)
				event := protocol.Event{ID: uuid.NewString(), Seq: 2, WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), SessionID: env.session.String(), RuntimeGeneration: 1, Kind: eventKind, OccurredAt: time.Now(), Data: raw}
				if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, event); err != nil {
					t.Fatal(err)
				}
			}
			got := inputReplyCleanupMessages(t, env.store)
			if len(got) != 2 || got[200] == "" || got[201] == "" {
				t.Fatalf("cleanup=%+v", got)
			}
			edits := questionAnswerEdits(t, env.store)
			if kind == "terminal-answer" {
				if len(edits) != 2 || edits[210].Answer != "One" || edits[211].Answer != "Two" {
					t.Fatalf("missing original answers: %+v", edits)
				}
			} else if len(edits) != 0 {
				t.Fatalf("dismiss invented answer: %+v", edits)
			}
		})
	}
}

func TestInputReplyCleanupLateCheckpointAndReopenedQuestion(t *testing.T) {
	env := newHistoryTestEnv(t)
	approval := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q", Prompt: "Retry?"})
	old := queueInputReplyForTest(t, env, approval, "q", 20, 0, true)
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE telegram_deliveries SET status='sending',next_attempt_at=$2 WHERE delivery_id=$1`, old, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.PrepareDeliveryChunks(t.Context(), old.String(), []json.RawMessage{json.RawMessage(`{"text":"late helper"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordBotInputRoute(t.Context(), "bot", 20, 300, env.session, "", approval, "q"); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 3000)
	in.Action, in.ReplyToMessageID, in.Text = "text", 300, "try one"
	result, err := env.store.AcceptTelegram(t.Context(), in)
	if err != nil || result.CommandID == "" {
		t.Fatalf("answer: %+v %v", result, err)
	}
	// Recover a definitely failed submission before the deletion worklist drains.
	if _, err = env.store.pool.Exec(t.Context(), `UPDATE commands SET status='failed',error_code='test_failure' WHERE command_id=$1`, result.CommandID); err != nil {
		t.Fatal(err)
	}
	tx, err := env.store.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = recoverAsyncQuestionResponse(t.Context(), tx, uuid.MustParse(result.CommandID)); err != nil {
		tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !helperRetired(t, env.store, old) {
		t.Fatal("old helper resurrected during failure recovery")
	}
	fresh := queueInputReplyForTest(t, env, approval, "q", 20, 0, true)
	checkpointInputReplyForTest(t, env, fresh, approval, "q", 302)
	if helperRetired(t, env.store, fresh) {
		t.Fatal("retained answer summary killed fresh retry helper")
	}
	// Simulate a delivery cancellation racing an already successful Telegram Send.
	if _, err = env.store.pool.Exec(t.Context(), `UPDATE telegram_deliveries SET status='cancelled' WHERE delivery_id=$1`, old); err != nil {
		t.Fatal(err)
	}
	if err = env.store.MarkDeliveryChunkSent(t.Context(), old.String(), 0, 301, env.session.String(), "", approval.String(), "q"); err != nil {
		t.Fatal(err)
	}
	got := inputReplyCleanupMessages(t, env.store)
	if len(got) != 1 || got[301] == "" {
		t.Fatalf("late checkpoint cleanup: %+v", got)
	}
	if _, exists := questionAnswerEdits(t, env.store)[301]; exists {
		t.Fatal("late helper edited instead of deleted")
	}
	if suppressed, err := env.store.SuppressTelegramDelivery(t.Context(), old.String()); err != nil || !suppressed {
		t.Fatalf("late old delivery escaped fence: %v %v", suppressed, err)
	}
}

func TestInputReplyCleanupEligibilityScopeAndUnchangedHeartbeat(t *testing.T) {
	for _, change := range []string{"turn-end", "question-removed", "archived", "runtime-generation", "thread-changed"} {
		t.Run(change, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			if _, err := env.store.pool.Exec(t.Context(), `UPDATE sessions SET active_turn_id='turn' WHERE session_id=$1`, env.session); err != nil {
				t.Fatal(err)
			}
			async := insertPendingQuestion(t, env, env.session, "turn", true, protocol.Question{ID: "q", Prompt: "Async?"})
			sync := insertPendingQuestion(t, env, env.session, "turn", false, protocol.Question{ID: "q", Prompt: "Blocking?"})
			first := queueInputReplyForTest(t, env, async, "q", 20, 0, true)
			checkpointInputReplyForTest(t, env, first, async, "q", 401)
			second := queueInputReplyForTest(t, env, sync, "q", 20, 0, true)
			checkpointInputReplyForTest(t, env, second, sync, "q", 402)
			if _, err := env.store.pool.Exec(t.Context(), `UPDATE sessions SET active_turn_id=active_turn_id,archived=archived,codex_thread_id=codex_thread_id WHERE session_id=$1`, env.session); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.pool.Exec(t.Context(), `UPDATE runtimes SET generation=generation WHERE runtime_id=$1`, env.runtime); err != nil {
				t.Fatal(err)
			}
			var queued int
			if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_input_reply_helpers WHERE backfill_pending=1`).Scan(&queued); err != nil || queued != 0 {
				t.Fatalf("unchanged heartbeat requeues helpers: %d %v", queued, err)
			}
			query := ""
			target := env.session
			switch change {
			case "turn-end":
				query = `UPDATE sessions SET active_turn_id=NULL WHERE session_id=$1`
			case "question-removed":
				query = `UPDATE approvals SET request_payload=json_set(request_payload,'$.questions',json('[]')) WHERE approval_id=$1`
				target = async
			case "archived":
				query = `UPDATE sessions SET archived=1 WHERE session_id=$1`
			case "runtime-generation":
				query = `UPDATE runtimes SET generation=generation+1 WHERE runtime_id=$1`
				target = env.runtime
			case "thread-changed":
				query = `UPDATE sessions SET codex_thread_id='new-thread' WHERE session_id=$1`
			}
			if _, err := env.store.pool.Exec(t.Context(), query, target); err != nil {
				t.Fatal(err)
			}
			got := inputReplyCleanupMessages(t, env.store)
			switch change {
			case "turn-end":
				if len(got) != 1 || got[402] == "" || helperRetired(t, env.store, first) {
					t.Fatalf("async question lost on turn end: %+v", got)
				}
			case "question-removed":
				if len(got) != 1 || got[401] == "" || helperRetired(t, env.store, second) {
					t.Fatalf("unrelated field/request lost: %+v", got)
				}
			default:
				if len(got) != 2 {
					t.Fatalf("stale helper not cleaned: %+v", got)
				}
			}
		})
	}
}

func TestInputReplyCleanupPostingRevocationAndBoundedBackfill(t *testing.T) {
	env := newHistoryTestEnv(t)
	approval := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q", Prompt: "Cleanup?"})
	helper := queueInputReplyForTest(t, env, approval, "q", 20, 0, true)
	checkpointInputReplyForTest(t, env, helper, approval, "q", 500)
	if err := env.store.ApplyTelegramChatAllowlist(t.Context(), "bot", []int64{99}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE approvals SET state='cleared' WHERE approval_id=$1`, approval); err != nil {
		t.Fatal(err)
	}
	got := inputReplyCleanupMessages(t, env.store)
	if got[500] != "sending" {
		t.Fatalf("posting revocation disabled deletion: %+v", got)
	}
	if err := env.store.ApplyTelegramChatAllowlist(t.Context(), "bot", []int64{99}); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := env.store.pool.QueryRow(t.Context(), `SELECT status FROM telegram_deliveries WHERE kind='input_reply_cleanup'`).Scan(&status); err != nil || status != "sending" {
		t.Fatalf("bulk revocation disabled deletion: %s %v", status, err)
	}
	// Worklist lookups must use a bounded partial index even after large history.
	rows, err := env.store.pool.Query(t.Context(), `EXPLAIN QUERY PLAN SELECT delivery_id FROM telegram_input_reply_helpers WHERE backfill_pending=1 ORDER BY delivery_id LIMIT 50`)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
	}
	rows.Close()
	if !strings.Contains(plan.String(), "telegram_input_reply_pending_idx") {
		t.Fatalf("unindexed cleanup polling: %s", plan.String())
	}
}

func TestInputReplyCleanupLegacyMigrationAndActualSendAge(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	// Recreate the pre-019 schema with actual stored legacy delivery/checkpoint
	// rows, then apply the normal checked migration path.
	for _, trigger := range []string{"telegram_input_reply_approval_changed", "telegram_input_reply_approval_deleted", "telegram_input_reply_session_changed", "telegram_input_reply_runtime_changed"} {
		if _, err := env.store.pool.Exec(ctx, "DROP TRIGGER "+trigger); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{`DROP TABLE telegram_input_reply_messages`, `DROP TABLE telegram_input_reply_helpers`, `DELETE FROM schema_migrations WHERE version=19`} {
		if _, err := env.store.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	approval := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q", Prompt: "Historical question?"})
	old := time.Now().UTC().Add(-72 * time.Hour)
	recent := time.Now().UTC().Add(-time.Hour)
	insertLegacy := func(id uuid.UUID, message int64, sent any, temporary bool, status string) {
		t.Helper()
		payload, _ := json.Marshal(AcceptResult{View: "input_prompt", TextReply: temporary, ApprovalID: approval.String(), QuestionID: "q", SessionID: env.session.String(), RuntimeID: env.runtime.String()})
		var parent any
		if message > 0 {
			parent = message
		}
		if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload,status,telegram_message_id,sent_at,created_at,next_attempt_at) VALUES($1,'bot',20,'ui_response',$2,$3,$4,$5,$6,$7)`, id, string(payload), status, parent, sent, old, time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		if message > 0 {
			if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_delivery_chunks(delivery_id,chunk_index,payload,status,telegram_message_id,sent_at) VALUES($1,0,'{"text":"legacy"}','sent',$2,$3)`, id, message, sent); err != nil {
				t.Fatal(err)
			}
			if err := env.store.RecordBotMessageRoute(ctx, "bot", 20, message, env.session, "", approval); err != nil {
				t.Fatal(err)
			}
			if _, err := env.store.pool.Exec(ctx, `UPDATE bot_message_routes SET question_id='q' WHERE bot_id='bot' AND chat_id=20 AND message_id=$1`, message); err != nil {
				t.Fatal(err)
			}
		}
	}
	ancient := uuid.New()
	insertLegacy(ancient, 601, old, true, "sent")
	fresh := uuid.New()
	insertLegacy(fresh, 602, recent, true, "sent")
	unknown := uuid.New()
	insertLegacy(unknown, 603, nil, true, "sent")
	original := uuid.New()
	insertLegacy(original, 600, recent, false, "sent")
	late := uuid.New()
	insertLegacy(late, 0, nil, true, "sending")
	if _, err := env.store.PrepareDeliveryChunks(ctx, late.String(), []json.RawMessage{json.RawMessage(`{"text":"late legacy helper"}`)}); err != nil {
		t.Fatal(err)
	}
	// The old release may already have queued a Question/Answer edit for a helper.
	oldEdit := uuid.New()
	raw, _ := json.Marshal(QuestionAnswerEdit{MessageID: 602, Question: "Historical question?", Answer: "Done"})
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload) VALUES($1,'bot',20,'question_answered',$2)`, oldEdit, string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `UPDATE approvals SET state='cleared' WHERE approval_id=$1`, approval); err != nil {
		t.Fatal(err)
	}
	if err := env.store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var retiredEdit bool
	if err := env.store.pool.QueryRow(ctx, `SELECT visibility_revoked FROM telegram_deliveries WHERE delivery_id=$1`, oldEdit).Scan(&retiredEdit); err != nil || !retiredEdit {
		t.Fatalf("legacy helper edit survived: %v %v", retiredEdit, err)
	}
	got := inputReplyCleanupMessages(t, env.store)
	if len(got) != 2 || got[602] == "" || got[603] == "" {
		t.Fatalf("legacy age classification: %+v", got)
	}
	if !helperRetired(t, env.store, ancient) {
		t.Fatal("old undeletable helper was not fenced")
	}
	if err := env.store.MarkDeliveryChunkSent(ctx, late.String(), 0, 604, env.session.String(), "", approval.String(), "q"); err != nil {
		t.Fatal(err)
	}
	got = inputReplyCleanupMessages(t, env.store)
	if len(got) != 3 || got[604] == "" {
		t.Fatalf("late send mistaken for old delivery: %+v", got)
	}
	tx, err := env.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = preserveInputQuestions(ctx, tx, approval, []protocol.Question{{ID: "q", Prompt: "Historical question?"}}); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = recordInputAnswerSummaries(ctx, tx, approval, map[string][]string{"q": {"Done"}}, true); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	edits := questionAnswerEdits(t, env.store)
	if len(edits) != 1 || edits[600].Answer != "Done" {
		t.Fatalf("legacy helper confused with original: %+v", edits)
	}
	if err := env.store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if len(inputReplyCleanupMessages(t, env.store)) != 3 {
		t.Fatal("restart duplicated legacy deletion work")
	}
}

func TestInputReplyCleanupBackfillBatchLimit(t *testing.T) {
	env := newHistoryTestEnv(t)
	approval := insertPendingQuestion(t, env, env.session, "", true, protocol.Question{ID: "q", Prompt: "Still pending?"})
	for range inputReplyReconcileBatch + 3 {
		queueInputReplyForTest(t, env, approval, "q", 20, 0, true)
	}
	if _, err := env.store.pool.Exec(t.Context(), `UPDATE telegram_input_reply_helpers SET backfill_pending=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ClaimDeliveries(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_input_reply_helpers WHERE backfill_pending=1`).Scan(&remaining); err != nil || remaining != 3 {
		t.Fatalf("unbounded backfill: %d %v", remaining, err)
	}
	if _, err := env.store.ClaimDeliveries(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_input_reply_helpers WHERE backfill_pending=1`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("backfill did not finish: %d %v", remaining, err)
	}
}

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

func queueVoiceTest(t *testing.T, env eventTestEnv, in IncomingUpdate) AcceptResult {
	t.Helper()
	r, err := env.store.AcceptTelegramVoice(t.Context(), in, TelegramVoice{FileID: "opaque_file_id", FileUniqueID: "opaque_unique_id", MIMEType: "audio/ogg", Duration: 3, FileSize: 100})
	if err != nil || r.View != "voice_transcribing" || r.VoiceJobID == "" {
		t.Fatalf("queue voice: %+v %v", r, err)
	}
	return r
}

func claimVoiceTest(t *testing.T, s *Store) TelegramVoiceJob {
	t.Helper()
	j, ok, err := s.ClaimTelegramVoice(t.Context(), 12*time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim voice: %+v %v %v", j, ok, err)
	}
	return j
}

func TestTelegramVoiceDeduplicatesAndFreezesLiteralTranscript(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	in := telegramUpdate(env, 2)
	in.Action, in.Text, in.Raw = "codex", "reset", json.RawMessage(`{"message":{"voice":{"file_id":"never_keep_raw"}}}`)
	wakeup := env.store.TelegramVoiceJobsChanged()
	r := queueVoiceTest(t, env, in)
	select {
	case <-wakeup:
	default:
		t.Fatal("voice acceptance did not wake processor")
	}
	if duplicate, err := env.store.AcceptTelegramVoice(ctx, in, TelegramVoice{FileID: "changed_file"}); err != nil || !duplicate.Duplicate {
		t.Fatalf("duplicate voice: %+v %v", duplicate, err)
	}
	if duplicate, err := env.store.AcceptTelegram(ctx, in); err != nil || !duplicate.Duplicate {
		t.Fatalf("ordinary retry bypassed voice receipt: %+v %v", duplicate, err)
	}
	other := uuid.New()
	insertRouteSession(t, env, other, "other-thread", "")
	selection := telegramUpdate(env, 3)
	selection.Action, selection.Target = "select", other.String()
	if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
		t.Fatal(err)
	}
	job := claimVoiceTest(t, env.store)
	if job.ID.String() != r.VoiceJobID || job.BotID != "bot" || job.ChatID != 20 || job.UserID != 10 || job.FileID != "opaque_file_id" || job.Attempts != 1 {
		t.Fatalf("claim lost immutable metadata: %+v", job)
	}
	transcript := "/reset everything"
	done, err := env.store.CompleteTelegramVoice(ctx, job.ID, job.LeaseID, transcript)
	if err != nil || done.View != "voice_transcript" || done.Transcript != transcript || done.SessionID != env.session.String() {
		t.Fatalf("complete voice: %+v %v", done, err)
	}
	if _, err := env.store.CompleteTelegramVoice(ctx, job.ID, job.LeaseID, transcript); !errors.Is(err, ErrTelegramVoiceLease) {
		t.Fatalf("completion replay: %v", err)
	}
	commands, err := env.store.PendingCommands(ctx, 10)
	if err != nil || len(commands) != 1 || commands[0].Operation != protocol.StartTurn || commands[0].Arguments.Text != transcript || commands[0].Arguments.Codex != nil || commands[0].SessionID != env.session.String() {
		t.Fatalf("voice command was parsed or rerouted: %+v %v", commands, err)
	}
	var raw, status string
	var file *string
	if err := env.store.pool.QueryRow(ctx, `SELECT update_receipt.raw,job.status,job.file_id FROM telegram_voice_jobs job JOIN telegram_updates update_receipt USING(bot_id,update_id) WHERE job_id=$1`, job.ID).Scan(&raw, &status, &file); err != nil || raw != "{}" || status != "completed" || file != nil {
		t.Fatalf("raw metadata retained: %q %q %v %v", raw, status, file, err)
	}
	var delivery string
	if err := env.store.pool.QueryRow(ctx, `SELECT delivery_id FROM telegram_deliveries WHERE json_extract(payload,'$.view')='voice_transcript'`).Scan(&delivery); err != nil {
		t.Fatal(err)
	}
	if suppress, err := env.store.SuppressTelegramDelivery(ctx, delivery); err != nil || suppress {
		t.Fatalf("selection change hid transcript: %v %v", suppress, err)
	}
}

func TestTelegramVoiceRestartRecoversLeaseWithoutDuplicateCommand(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	queueVoiceTest(t, env, telegramUpdate(env, 2))
	first := claimVoiceTest(t, env.store)
	path := env.store.pool.path
	env.store.Close()
	restarted, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := restarted.ClaimTelegramVoice(ctx, time.Minute); err != nil || ok {
		t.Fatalf("live lease was stolen: %v %v", ok, err)
	}
	if _, err := restarted.pool.Exec(ctx, `UPDATE telegram_voice_jobs SET lease_until=$2 WHERE job_id=$1`, first.ID, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	second := claimVoiceTest(t, restarted)
	if second.ID != first.ID || second.LeaseID == first.LeaseID || second.Attempts != 2 {
		t.Fatalf("expired lease not fenced: first=%+v second=%+v", first, second)
	}
	if _, err := restarted.CompleteTelegramVoice(ctx, first.ID, first.LeaseID, "old result"); !errors.Is(err, ErrTelegramVoiceLease) {
		t.Fatalf("old processor committed: %v", err)
	}
	if _, err := restarted.CompleteTelegramVoice(ctx, second.ID, second.LeaseID, "recovered transcript"); err != nil {
		t.Fatal(err)
	}
	commands, err := restarted.PendingCommands(ctx, 10)
	if err != nil || len(commands) != 1 || commands[0].Arguments.Text != "recovered transcript" {
		t.Fatalf("recovered commands: %+v %v", commands, err)
	}
}

func TestTelegramVoiceQuestionReplyFreezesQuestionAndUsesAnswerEdits(t *testing.T) {
	for _, secret := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "secret"}[secret], func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := t.Context()
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='waiting-turn' WHERE session_id=$1`, env.session); err != nil {
				t.Fatal(err)
			}
			approval := insertPendingQuestion(t, env, env.session, "waiting-turn", false, protocol.Question{ID: "first", Prompt: "First?", Secret: secret}, protocol.Question{ID: "second", Prompt: "Second?"})
			if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 901, env.session, "waiting-turn", approval, "first"); err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			in.ReplyToMessageID = 901
			queueVoiceTest(t, env, in)
			other := uuid.New()
			insertRouteSession(t, env, other, "other-thread", "")
			selection := telegramUpdate(env, 3)
			selection.Action, selection.Target = "select", other.String()
			if _, err := env.store.AcceptTelegram(ctx, selection); err != nil {
				t.Fatal(err)
			}
			job := claimVoiceTest(t, env.store)
			result, err := env.store.CompleteTelegramVoice(ctx, job.ID, job.LeaseID, "spoken answer")
			want := "spoken answer"
			if secret {
				want = "[REDACTED]"
			}
			if err != nil || result.View != "voice_transcript" || result.Transcript != want || result.ApprovalID != "" || result.QuestionID != "" {
				t.Fatalf("question transcript: %+v %v", result, err)
			}
			var answer string
			var version int
			if err := env.store.pool.QueryRow(ctx, `SELECT answer,version FROM telegram_question_answers WHERE approval_id=$1 AND question_id='first'`, approval).Scan(&answer, &version); err != nil || answer != want || version != 1 {
				t.Fatalf("answer summary: %q %d %v", answer, version, err)
			}
			commands, err := env.store.PendingCommands(ctx, 10)
			if err != nil || len(commands) != 0 {
				t.Fatalf("partial question prematurely submitted: %+v %v", commands, err)
			}
			var pending, edits int
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE json_extract(payload,'$.view')='input_pending' AND json_extract(payload,'$.question_id')='second'`).Scan(&pending); err != nil || pending != 1 {
				t.Fatalf("next question missing: %d %v", pending, err)
			}
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE kind='question_answered' AND json_extract(payload,'$.answer')=$1`, want).Scan(&edits); err != nil || edits != 1 {
				t.Fatalf("original question not edited: %d %v", edits, err)
			}
			if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 902, env.session, "waiting-turn", approval, "second"); err != nil {
				t.Fatal(err)
			}
			in.UpdateID, in.ReplyToMessageID = 4, 902
			queueVoiceTest(t, env, in)
			job = claimVoiceTest(t, env.store)
			if _, err := env.store.CompleteTelegramVoice(ctx, job.ID, job.LeaseID, "second answer"); err != nil {
				t.Fatal(err)
			}
			commands, err = env.store.PendingCommands(ctx, 10)
			if err != nil || len(commands) != 1 || commands[0].Operation != protocol.InputResponse || commands[0].ExpectedTurnID != "waiting-turn" || commands[0].SessionID != env.session.String() || commands[0].Arguments.Answers["first"][0] != "spoken answer" || commands[0].Arguments.Answers["second"][0] != "second answer" {
				t.Fatalf("question response: %+v %v", commands, err)
			}
		})
	}
}

func TestTelegramVoiceStaleTargetsCannotSubmit(t *testing.T) {
	for _, scenario := range []string{"turn_finished", "turn_replaced", "generation", "archive", "worker_revoked", "question_already_answered", "question_claimed", "chat_revoked"} {
		t.Run(scenario, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := t.Context()
			if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='captured-turn' WHERE session_id=$1`, env.session); err != nil {
				t.Fatal(err)
			}
			in := telegramUpdate(env, 2)
			var approval uuid.UUID
			if strings.HasPrefix(scenario, "question_") {
				approval = insertPendingQuestion(t, env, env.session, "captured-turn", false, protocol.Question{ID: "first", Prompt: "First?"}, protocol.Question{ID: "second", Prompt: "Second?"})
				if err := env.store.RecordBotInputRoute(ctx, "bot", 20, 900, env.session, "captured-turn", approval, "first"); err != nil {
					t.Fatal(err)
				}
				in.ReplyToMessageID = 900
			}
			queueVoiceTest(t, env, in)
			job := claimVoiceTest(t, env.store)
			var err error
			switch scenario {
			case "turn_finished":
				_, err = env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id=NULL WHERE session_id=$1`, env.session)
			case "turn_replaced":
				_, err = env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='new-turn' WHERE session_id=$1`, env.session)
			case "generation":
				_, err = env.store.pool.Exec(ctx, `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, env.runtime)
			case "archive":
				_, err = env.store.pool.Exec(ctx, `UPDATE sessions SET archived=TRUE WHERE session_id=$1`, env.session)
			case "worker_revoked":
				err = env.store.RevokeWorker(ctx, env.worker)
			case "question_already_answered":
				_, err = env.store.pool.Exec(ctx, `UPDATE approvals SET input_answers='{"first":["newer text"]}' WHERE approval_id=$1`, approval)
			case "question_claimed":
				_, err = env.store.pool.Exec(ctx, `UPDATE approvals SET state='cancelled' WHERE approval_id=$1`, approval)
			case "chat_revoked":
				err = env.store.ApplyTelegramChatAllowlist(ctx, "bot", []int64{999})
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := env.store.CompleteTelegramVoice(ctx, job.ID, job.LeaseID, "never submit this")
			if scenario == "chat_revoked" {
				if !errors.Is(err, ErrTelegramVoiceLease) {
					t.Fatalf("revocation did not fence lease: %+v %v", result, err)
				}
			} else if err != nil || result.View != "error" {
				t.Fatalf("stale target accepted: %+v %v", result, err)
			}
			commands, err := env.store.PendingCommands(ctx, 10)
			if err != nil || len(commands) != 0 {
				t.Fatalf("stale voice queued command: %+v %v", commands, err)
			}
			var status string
			var file *string
			if err := env.store.pool.QueryRow(ctx, `SELECT status,file_id FROM telegram_voice_jobs WHERE job_id=$1`, job.ID).Scan(&status, &file); err != nil || status != "failed" || file != nil {
				t.Fatalf("stale job not terminal: %q %v %v", status, file, err)
			}
		})
	}
}

func TestTelegramVoicePreservesTopicOrderAndAllowsControls(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	first := queueVoiceTest(t, env, telegramUpdate(env, 2))
	later := telegramUpdate(env, 3)
	later.Action, later.Text = "text", "later ordinary text"
	textResult, err := env.store.AcceptTelegram(ctx, later)
	if err != nil || textResult.CommandID == "" {
		t.Fatalf("later text not accepted: %+v %v", textResult, err)
	}
	second := queueVoiceTest(t, env, telegramUpdate(env, 4))
	otherTopic := telegramUpdate(env, 5)
	otherTopic.Action, otherTopic.Text, otherTopic.TopicID = "text", "other topic text", 77
	otherResult, err := env.store.AcceptTelegram(ctx, otherTopic)
	if err != nil || otherResult.CommandID == "" {
		t.Fatal(err)
	}
	control := telegramUpdate(env, 6)
	control.Action = "status"
	if r, err := env.store.AcceptTelegram(ctx, control); err != nil || r.View != "status" {
		t.Fatalf("voice delayed control: %+v %v", r, err)
	}
	commands, err := env.store.PendingCommands(ctx, 10)
	if err != nil || len(commands) != 1 || commands[0].ID != otherResult.CommandID {
		t.Fatalf("topic isolation/order hold: %+v %v", commands, err)
	}
	job := claimVoiceTest(t, env.store)
	if job.ID.String() != first.VoiceJobID {
		t.Fatal("voice FIFO changed")
	}
	if _, ok, err := env.store.ClaimTelegramVoice(ctx, time.Minute); err != nil || ok {
		t.Fatalf("same topic processed concurrently: %v %v", ok, err)
	}
	if _, err := env.store.CompleteTelegramVoice(ctx, job.ID, job.LeaseID, "first voice"); err != nil {
		t.Fatal(err)
	}
	commands, err = env.store.PendingCommands(ctx, 10)
	if err != nil || len(commands) != 3 || commands[0].Arguments.Text != "first voice" || commands[1].ID != textResult.CommandID {
		t.Fatalf("voice completion did not precede later text: %+v %v", commands, err)
	}
	// Both voice receipts saw an idle session. Even if the first command has
	// started, the second remains StartTurn and joins the normal next-turn queue.
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='first-voice-turn' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	job = claimVoiceTest(t, env.store)
	if job.ID.String() != second.VoiceJobID {
		t.Fatal("second voice was lost")
	}
	if _, err := env.store.CompleteTelegramVoice(ctx, job.ID, job.LeaseID, "second voice"); err != nil {
		t.Fatal(err)
	}
	commands, err = env.store.PendingCommands(ctx, 10)
	if err != nil || len(commands) != 4 || commands[2].Arguments.Text != "second voice" || commands[2].Operation != protocol.StartTurn || commands[2].ExpectedTurnID != "" {
		t.Fatalf("idle voice changed into steer: %+v %v", commands, err)
	}
}

func TestTelegramVoiceInterruptCancelsLease(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	if _, err := env.store.pool.Exec(ctx, `UPDATE sessions SET active_turn_id='turn-to-stop' WHERE session_id=$1`, env.session); err != nil {
		t.Fatal(err)
	}
	queueVoiceTest(t, env, telegramUpdate(env, 2))
	job := claimVoiceTest(t, env.store)
	stop := telegramUpdate(env, 3)
	stop.Action = "interrupt"
	r, err := env.store.AcceptTelegram(ctx, stop)
	if err != nil || r.CommandID == "" {
		t.Fatalf("interrupt: %+v %v", r, err)
	}
	if _, err := env.store.CompleteTelegramVoice(ctx, job.ID, job.LeaseID, "too late"); !errors.Is(err, ErrTelegramVoiceLease) {
		t.Fatalf("interrupt did not fence voice: %v", err)
	}
	commands, err := env.store.PendingCommands(ctx, 10)
	if err != nil || len(commands) != 1 || commands[0].Operation != protocol.Interrupt {
		t.Fatalf("voice held interrupt: %+v %v", commands, err)
	}
}

func TestTelegramVoiceRetriesAreBoundedAndMetadataIsScrubbed(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	queueVoiceTest(t, env, telegramUpdate(env, 2))
	for attempt := 1; attempt <= MaxTelegramVoiceAttempts; attempt++ {
		job := claimVoiceTest(t, env.store)
		if job.Attempts != attempt {
			t.Fatalf("attempt=%d want=%d", job.Attempts, attempt)
		}
		if err := env.store.FailTelegramVoice(ctx, job.ID, job.LeaseID, "https://private-token.example/failure", true); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := env.store.ClaimTelegramVoice(ctx, time.Minute); err != nil || ok {
			t.Fatalf("retry ignored backoff: %v %v", ok, err)
		}
		if attempt < MaxTelegramVoiceAttempts {
			next, pending, err := env.store.NextTelegramVoiceAttempt(ctx)
			if err != nil || !pending || !next.After(time.Now()) {
				t.Fatalf("retry wakeup: %v %v %v", next, pending, err)
			}
			if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_voice_jobs SET next_attempt_at=$1`, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, pending, err := env.store.NextTelegramVoiceAttempt(ctx); err != nil || pending {
		t.Fatalf("exhausted retry stayed active: %v %v", pending, err)
	}
	var status, code string
	var file *string
	if err := env.store.pool.QueryRow(ctx, `SELECT status,error_code,file_id FROM telegram_voice_jobs`).Scan(&status, &code, &file); err != nil || status != "failed" || code != "voice_transcription_failed" || file != nil {
		t.Fatalf("retry terminal record: %q %q %v %v", status, code, file, err)
	}
	var failures int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE json_extract(payload,'$.error_code')='voice_transcription_failed'`).Scan(&failures); err != nil || failures != 1 {
		t.Fatalf("retry duplicated feedback: %d %v", failures, err)
	}
}

func TestTelegramVoiceTranscribingCleanupHandlesLateCheckpoint(t *testing.T) {
	for _, sentFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "late", true: "sent"}[sentFirst], func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := t.Context()
			accepted := queueVoiceTest(t, env, telegramUpdate(env, 2))
			rows, err := env.store.ClaimDeliveries(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			var progress Delivery
			for _, r := range rows {
				var result AcceptResult
				if json.Unmarshal(r.Payload, &result) == nil && result.VoiceJobID == accepted.VoiceJobID {
					progress = r
				}
			}
			if progress.ID == "" {
				t.Fatal("transcribing acknowledgement missing")
			}
			if sentFirst {
				if err := env.store.MarkDeliverySent(ctx, progress.ID, 901, env.session.String(), "", ""); err != nil {
					t.Fatal(err)
				}
			}
			job := claimVoiceTest(t, env.store)
			if _, err := env.store.CompleteTelegramVoice(ctx, job.ID, job.LeaseID, "transcribed"); err != nil {
				t.Fatal(err)
			}
			if !sentFirst {
				// Another sender may already have retired the revoked lease.
				if _, err := env.store.pool.Exec(ctx, `UPDATE telegram_deliveries SET next_attempt_at=$2 WHERE delivery_id=$1`, progress.ID, time.Now().Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
				if _, err := env.store.ClaimDeliveries(ctx, 10); err != nil {
					t.Fatal(err)
				}
				if err := env.store.MarkDeliverySent(ctx, progress.ID, 901, env.session.String(), "", ""); err != nil {
					t.Fatal(err)
				}
			}
			var cleanup int
			if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE kind='picker_cleanup' AND json_extract(payload,'$.message_id')=901`).Scan(&cleanup); err != nil || cleanup != 1 {
				t.Fatalf("progress was not cleaned once: %d %v", cleanup, err)
			}
		})
	}
}

func TestTelegramVoiceWorkerPurgeRetainsAnonymousDedupe(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	in := telegramUpdate(env, 2)
	queueVoiceTest(t, env, in)
	if err := env.store.RevokeWorker(ctx, env.worker); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DeleteRevokedWorker(ctx, env.worker); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_voice_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("purge retained voice metadata: %d %v", jobs, err)
	}
	var raw string
	var user, chat *int64
	if err := env.store.pool.QueryRow(ctx, `SELECT raw,user_id,chat_id FROM telegram_updates WHERE bot_id='bot' AND update_id=2`).Scan(&raw, &user, &chat); err != nil || raw != "{}" || user != nil || chat != nil {
		t.Fatalf("voice receipt remained identifiable: %q %v %v %v", raw, user, chat, err)
	}
	if r, err := env.store.AcceptTelegramVoice(context.Background(), in, TelegramVoice{FileID: "opaque_file_id"}); err != nil || !r.Duplicate {
		t.Fatalf("purged voice replay: %+v %v", r, err)
	}
}

func TestTelegramVoiceMetadataLimitsAndPendingOwnerBound(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		voice      TelegramVoice
	}{
		{"large", "voice_too_large", TelegramVoice{FileID: "file", FileSize: MaxTelegramVoiceBytes + 1}},
		{"long", "voice_too_long", TelegramVoice{FileID: "file", Duration: MaxTelegramVoiceDuration + 1}},
		{"missing", "voice_unsupported", TelegramVoice{}},
		{"url", "voice_unsupported", TelegramVoice{FileID: "https://token.example/audio"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			ctx := t.Context()
			in := telegramUpdate(env, 2)
			r, err := env.store.AcceptTelegramVoice(ctx, in, tc.voice)
			if err != nil || r.ErrorCode != tc.code {
				t.Fatalf("invalid metadata: %+v %v", r, err)
			}
			if duplicate, err := env.store.AcceptTelegramVoice(ctx, in, tc.voice); err != nil || !duplicate.Duplicate {
				t.Fatalf("invalid voice redelivery: %+v %v", duplicate, err)
			}
			if _, pending, err := env.store.NextTelegramVoiceAttempt(ctx); err != nil || pending {
				t.Fatalf("invalid voice queued: %v %v", pending, err)
			}
		})
	}
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	for i := 0; i < MaxTelegramVoicePending; i++ {
		queueVoiceTest(t, env, telegramUpdate(env, int64(i+2)))
	}
	r, err := env.store.AcceptTelegramVoice(ctx, telegramUpdate(env, 100), TelegramVoice{FileID: "file"})
	if err != nil || r.ErrorCode != "voice_queue_full" {
		t.Fatalf("owner queue unbounded: %+v %v", r, err)
	}
	other := telegramUpdate(env, 101)
	other.UserID = 11
	other.Action, other.Target = "select", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, other); err != nil {
		t.Fatal(err)
	}
	other.UpdateID = 102
	queueVoiceTest(t, env, other)
	if err := env.store.CancelTelegramVoiceJobs(ctx, "bot", "voice_disabled"); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := env.store.NextTelegramVoiceAttempt(ctx); err != nil || pending {
		t.Fatalf("disabled processor left queue blocking: %v %v", pending, err)
	}
}

func TestTelegramVoiceInterruptCancelsIdleTranscription(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	queueVoiceTest(t, env, telegramUpdate(env, 2))
	job := claimVoiceTest(t, env.store)
	stop := telegramUpdate(env, 3)
	stop.Action = "interrupt"
	r, err := env.store.AcceptTelegram(ctx, stop)
	if err != nil || r.View != "voice_cancelled" || r.CommandID != "" {
		t.Fatalf("idle cancellation: %+v %v", r, err)
	}
	if _, err := env.store.CompleteTelegramVoice(ctx, job.ID, job.LeaseID, "late answer"); !errors.Is(err, ErrTelegramVoiceLease) {
		t.Fatalf("idle cancellation lost fence: %v", err)
	}
	var feedback int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE json_extract(payload,'$.error_code')='voice_cancelled'`).Scan(&feedback); err != nil || feedback != 1 {
		t.Fatalf("idle cancellation feedback duplicated: %d %v", feedback, err)
	}
}

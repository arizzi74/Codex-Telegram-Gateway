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

func completeSpeechVoice(t *testing.T, env eventTestEnv, reply bool, language string) (TelegramVoiceJob, AcceptResult) {
	t.Helper()
	_, err := env.store.AcceptTelegramVoice(t.Context(), telegramUpdate(env, 2), TelegramVoice{FileID: "opaque", ReplyWithAudio: reply})
	if err != nil {
		t.Fatal(err)
	}
	voice := claimVoiceTest(t, env.store)
	result, err := env.store.CompleteTelegramVoiceWithLanguage(t.Context(), voice.ID, voice.LeaseID, "Please explain", language)
	if err != nil || result.View != "voice_transcript" {
		t.Fatalf("complete speech voice: %+v %v", result, err)
	}
	return voice, result
}

func finalSpeechDelivery(t *testing.T, env eventTestEnv, reply bool) (TelegramVoiceJob, Delivery) {
	t.Helper()
	voice, result := completeSpeechVoice(t, env, reply, "it")
	progressEvent(t, env, 2, "turn_started", "voice-turn", result.CommandID)
	progressEvent(t, env, 3, "turn_completed", "voice-turn", "")
	rows, err := env.store.ClaimDeliveries(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range rows {
		if delivery.Kind == "turn_completed" {
			return voice, delivery
		}
	}
	t.Fatal("missing final text delivery")
	return voice, Delivery{}
}

func claimSpeechTest(t *testing.T, store *Store) TelegramSpeechJob {
	t.Helper()
	job, ok, err := store.ClaimTelegramSpeech(t.Context(), 12*time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim speech: %+v %v %v", job, ok, err)
	}
	return job
}

func TestTelegramSpeechWaitsForAllFinalTextAndCorrelatesVoiceTurn(t *testing.T) {
	env := newHistoryTestEnv(t)
	_, final := finalSpeechDelivery(t, env, true)
	if _, ok, err := env.store.ClaimTelegramSpeech(t.Context(), time.Minute); err != nil || ok {
		t.Fatalf("speech preceded text: %v %v", ok, err)
	}
	chunks := []json.RawMessage{json.RawMessage(`{"text":"first"}`), json.RawMessage(`{"text":"last"}`)}
	if _, err := env.store.PrepareDeliveryChunks(t.Context(), final.ID, chunks); err != nil {
		t.Fatal(err)
	}
	if err := env.store.MarkDeliveryChunkSent(t.Context(), final.ID, 0, 100, env.session.String(), "voice-turn", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := env.store.ClaimTelegramSpeech(t.Context(), time.Minute); err != nil || ok {
		t.Fatalf("speech preceded last text chunk: %v %v", ok, err)
	}
	wake := env.store.TelegramVoiceJobsChanged()
	if err := env.store.MarkDeliveryChunkSent(t.Context(), final.ID, 1, 101, env.session.String(), "voice-turn", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("last text chunk did not wake speech processor")
	}
	job := claimSpeechTest(t, env.store)
	if job.DeliveryID != final.ID || job.SessionID != env.session.String() || job.TurnID != "voice-turn" || job.BotID != "bot" || job.ChatID != 20 || job.TopicID != 0 || job.UserID != 10 || job.Language != "it" || job.Text != "turn_completed text" || job.NextChunk != 0 || job.Attempts != 1 {
		t.Fatalf("voice correlation lost: %+v", job)
	}
	if allowed, err := env.store.CheckTelegramSpeech(t.Context(), job.ID, job.LeaseID); err != nil || !allowed {
		t.Fatalf("valid speech lease denied: %v %v", allowed, err)
	}
	if err := env.store.CheckpointTelegramSpeech(t.Context(), job.ID, job.LeaseID, 0, 200, false); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CheckpointTelegramSpeech(t.Context(), job.ID, job.LeaseID, 0, 200, false); err != nil {
		t.Fatalf("duplicate checkpoint failed: %v", err)
	}
	if err := env.store.CheckpointTelegramSpeech(t.Context(), job.ID, job.LeaseID, 0, 201, false); err == nil {
		t.Fatal("conflicting checkpoint accepted")
	}
	if err := env.store.CheckpointTelegramSpeech(t.Context(), job.ID, job.LeaseID, 2, 202, true); err == nil {
		t.Fatal("skipped speech chunk accepted")
	}
	var session, turn string
	if err := env.store.pool.QueryRow(t.Context(), `SELECT session_id,turn_id FROM bot_message_routes WHERE bot_id='bot' AND chat_id=20 AND message_id=200`).Scan(&session, &turn); err != nil || session != job.SessionID || turn != job.TurnID {
		t.Fatalf("speech reply route missing: %q %q %v", session, turn, err)
	}
	if err := env.store.CheckpointTelegramSpeech(t.Context(), job.ID, job.LeaseID, 1, 201, true); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := env.store.NextTelegramSpeechAttempt(t.Context()); err != nil || pending {
		t.Fatalf("completed speech still queued: %v %v", pending, err)
	}
}

func TestTelegramSpeechDoesNotSpeakUnrelatedOrUnrequestedFinals(t *testing.T) {
	for _, scenario := range []string{"nonvoice", "reply disabled", "wrong turn", "unbound turn", "secret question", "wrong chat", "wrong topic"} {
		t.Run(scenario, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			var voice TelegramVoiceJob
			var result AcceptResult
			if scenario != "nonvoice" {
				voice, result = completeSpeechVoice(t, env, scenario != "reply disabled", "fr")
			}
			if scenario == "secret question" {
				if _, err := env.store.pool.Exec(t.Context(), `UPDATE telegram_voice_jobs SET question_secret=1 WHERE job_id=$1`, voice.ID); err != nil {
					t.Fatal(err)
				}
			}
			turn := "voice-turn"
			command := result.CommandID
			if scenario == "unbound turn" {
				command = ""
			}
			progressEvent(t, env, 2, "turn_started", turn, command)
			if scenario == "wrong turn" {
				turn = "unrelated-turn"
			}
			if scenario == "wrong chat" {
				if _, err := env.store.pool.Exec(t.Context(), `UPDATE telegram_bindings SET chat_id=999 WHERE session_id=$1`, env.session); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "wrong topic" {
				if _, err := env.store.pool.Exec(t.Context(), `UPDATE telegram_bindings SET message_thread_id=99 WHERE session_id=$1`, env.session); err != nil {
					t.Fatal(err)
				}
			}
			progressEvent(t, env, 3, "turn_completed", turn, "")
			var jobs int
			if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_speech_jobs`).Scan(&jobs); err != nil || jobs != 0 {
				t.Fatalf("unrelated final created speech: %d %v", jobs, err)
			}
		})
	}
}

func TestTelegramSpeechDeduplicatesFinalEvents(t *testing.T) {
	env := newHistoryTestEnv(t)
	_, result := completeSpeechVoice(t, env, true, "en")
	progressEvent(t, env, 2, "turn_started", "voice-turn", result.CommandID)
	final := progressEvent(t, env, 3, "turn_completed", "voice-turn", "")
	if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, final); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 4, "turn_completed", "voice-turn", "")
	var jobs, deliveries int
	if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_speech_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_deliveries WHERE kind='turn_completed'`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || deliveries != 2 {
		t.Fatalf("duplicate speech created: jobs=%d deliveries=%d", jobs, deliveries)
	}
}

func TestTelegramSpeechResumesConfirmedChunkAfterRestart(t *testing.T) {
	env := newHistoryTestEnv(t)
	_, final := finalSpeechDelivery(t, env, true)
	if err := env.store.MarkDeliverySent(t.Context(), final.ID, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	first := claimSpeechTest(t, env.store)
	if err := env.store.CheckpointTelegramSpeech(t.Context(), first.ID, first.LeaseID, 0, 200, false); err != nil {
		t.Fatal(err)
	}
	path := env.store.pool.path
	env.store.Close()
	restarted, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := restarted.ClaimTelegramSpeech(t.Context(), time.Minute); err != nil || ok {
		t.Fatalf("live speech lease stolen: %v %v", ok, err)
	}
	if _, err := restarted.pool.Exec(t.Context(), `UPDATE telegram_speech_jobs SET lease_until=$2 WHERE job_id=$1`, first.ID, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	second := claimSpeechTest(t, restarted)
	if second.ID != first.ID || second.LeaseID == first.LeaseID || second.NextChunk != 1 || second.Attempts != 2 {
		t.Fatalf("restart lost checkpoint: %+v", second)
	}
	if ok, err := restarted.CheckTelegramSpeech(t.Context(), first.ID, first.LeaseID); err != nil || ok {
		t.Fatal("old speech lease remained authorized")
	}
	if err := restarted.CheckpointTelegramSpeech(t.Context(), first.ID, first.LeaseID, 1, 201, true); !errors.Is(err, ErrTelegramSpeechLease) {
		t.Fatalf("stale checkpoint accepted: %v", err)
	}
	if err := restarted.CheckpointTelegramSpeech(t.Context(), second.ID, second.LeaseID, 1, 201, true); err != nil {
		t.Fatal(err)
	}
	var chunks int
	if err := restarted.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_speech_chunks WHERE job_id=$1`, second.ID).Scan(&chunks); err != nil || chunks != 2 {
		t.Fatalf("confirmed chunks lost: %d %v", chunks, err)
	}
}

func TestTelegramSpeechRechecksAuthorizationAndRetiresRevokedJobs(t *testing.T) {
	for _, scenario := range []string{"chat", "worker", "archive", "generation", "selection", "disabled", "delivery revoked"} {
		t.Run(scenario, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			_, final := finalSpeechDelivery(t, env, true)
			if err := env.store.MarkDeliverySent(t.Context(), final.ID, 100, "", "", ""); err != nil {
				t.Fatal(err)
			}
			job := claimSpeechTest(t, env.store)
			var err error
			switch scenario {
			case "chat":
				err = env.store.ApplyTelegramChatAllowlist(t.Context(), "bot", []int64{999})
			case "worker":
				err = env.store.RevokeWorker(t.Context(), env.worker)
			case "archive":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE sessions SET archived=1 WHERE session_id=$1`, env.session)
			case "generation":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, env.runtime)
			case "selection":
				_, err = env.store.pool.Exec(t.Context(), `DELETE FROM telegram_bindings WHERE session_id=$1`, env.session)
			case "disabled":
				err = env.store.CancelTelegramSpeechJobs(t.Context(), "bot")
			case "delivery revoked":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE telegram_deliveries SET visibility_revoked=1 WHERE delivery_id=$1`, final.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if allowed, err := env.store.CheckTelegramSpeech(t.Context(), job.ID, job.LeaseID); err != nil || allowed {
				t.Fatalf("revoked speech remained authorized: %v %v", allowed, err)
			}
			if scenario == "chat" {
				if err := env.store.ApplyTelegramChatAllowlist(t.Context(), "bot", []int64{}); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok, err := env.store.ClaimTelegramSpeech(t.Context(), time.Minute); err != nil || ok {
				t.Fatalf("revoked speech replayed: %v %v", ok, err)
			}
		})
	}
}

func TestTelegramSpeechRetriesAreBoundedAndRespectTelegramBackoff(t *testing.T) {
	env := newHistoryTestEnv(t)
	_, final := finalSpeechDelivery(t, env, true)
	if err := env.store.MarkDeliverySent(t.Context(), final.ID, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= MaxTelegramSpeechAttempts; attempt++ {
		job := claimSpeechTest(t, env.store)
		if job.Attempts != attempt {
			t.Fatalf("wrong attempt: %+v", job)
		}
		before := time.Now().UTC()
		if err := env.store.FailTelegramSpeech(t.Context(), job.ID, job.LeaseID, "https://private.example/?token=secret", true, 45*time.Second); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := env.store.ClaimTelegramSpeech(t.Context(), time.Minute); err != nil || ok {
			t.Fatalf("speech ignored backoff: %v %v", ok, err)
		}
		if attempt < MaxTelegramSpeechAttempts {
			next, pending, err := env.store.NextTelegramSpeechAttempt(t.Context())
			if err != nil || !pending || next.Before(before.Add(44*time.Second)) {
				t.Fatalf("Retry-After lost: %v %v %v", next, pending, err)
			}
			if _, err := env.store.pool.Exec(t.Context(), `UPDATE telegram_speech_jobs SET next_attempt_at=$1`, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	var status, code string
	if err := env.store.pool.QueryRow(t.Context(), `SELECT status,error_code FROM telegram_speech_jobs`).Scan(&status, &code); err != nil || status != "failed" || code != "speech_failed" || strings.Contains(code, "secret") {
		t.Fatalf("unbounded or unsafe speech failure: %q %q %v", status, code, err)
	}
}

func TestTelegramSpeechWorkerPurgeCascadesCheckpoints(t *testing.T) {
	env := newHistoryTestEnv(t)
	_, final := finalSpeechDelivery(t, env, true)
	if err := env.store.MarkDeliverySent(t.Context(), final.ID, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	job := claimSpeechTest(t, env.store)
	if err := env.store.CheckpointTelegramSpeech(t.Context(), job.ID, job.LeaseID, 0, 200, false); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RevokeWorker(t.Context(), env.worker); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DeleteRevokedWorker(t.Context(), env.worker); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"telegram_speech_jobs", "telegram_speech_chunks", "telegram_voice_jobs", "bot_message_routes"} {
		var count int
		if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("purge retained %s: %d %v", table, count, err)
		}
	}
	if allowed, err := env.store.CheckTelegramSpeech(context.Background(), job.ID, job.LeaseID); err != nil || allowed {
		t.Fatalf("purged speech still authorized: %v %v", allowed, err)
	}
}

func TestTelegramSpeechCommandResultCanBindTurnWithoutStartedCommandID(t *testing.T) {
	env := newHistoryTestEnv(t)
	_, result := completeSpeechVoice(t, env, true, "en")
	progressEvent(t, env, 2, "turn_started", "voice-turn", "")
	progressEvent(t, env, 3, "command_completed", "voice-turn", result.CommandID)
	progressEvent(t, env, 4, "turn_completed", "voice-turn", "")
	var count int
	if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_speech_jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("command result did not bind voice turn: %d %v", count, err)
	}
}

func TestTelegramSpeechLeaseBounds(t *testing.T) {
	env := newHistoryTestEnv(t)
	for _, duration := range []time.Duration{0, -time.Second, 16 * time.Minute} {
		if _, _, err := env.store.ClaimTelegramSpeech(t.Context(), duration); err == nil {
			t.Fatal("invalid speech lease accepted")
		}
	}
	if err := env.store.CheckpointTelegramSpeech(t.Context(), uuid.New(), uuid.New(), -1, 1, false); err == nil {
		t.Fatal("invalid checkpoint index accepted")
	}
}

func TestTelegramSpeechFrozenSteerAndQuestionTarget(t *testing.T) {
	for _, scenario := range []string{"steer", "plain question", "secret question"} {
		t.Run(scenario, func(t *testing.T) {
			env := newHistoryTestEnv(t)
			progressEvent(t, env, 2, "turn_started", "captured-turn", "")
			in := telegramUpdate(env, 2)
			if scenario != "steer" {
				approval := insertPendingQuestion(t, env, env.session, "captured-turn", false, protocol.Question{ID: "answer", Prompt: "Explain?", Secret: scenario == "secret question"})
				if err := env.store.RecordBotInputRoute(t.Context(), "bot", 20, 900, env.session, "captured-turn", approval, "answer"); err != nil {
					t.Fatal(err)
				}
				in.ReplyToMessageID = 900
			}
			if _, err := env.store.AcceptTelegramVoice(t.Context(), in, TelegramVoice{FileID: "opaque", ReplyWithAudio: true}); err != nil {
				t.Fatal(err)
			}
			voice := claimVoiceTest(t, env.store)
			result, err := env.store.CompleteTelegramVoiceWithLanguage(t.Context(), voice.ID, voice.LeaseID, "spoken answer", "it")
			if err != nil || result.View != "voice_transcript" {
				t.Fatalf("complete frozen voice: %+v %v", result, err)
			}
			progressEvent(t, env, 3, "turn_completed", "captured-turn", "")
			var jobs int
			if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_speech_jobs`).Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			want := 1
			if scenario == "secret question" {
				want = 0
			}
			if jobs != want {
				t.Fatalf("frozen turn speech jobs=%d want=%d", jobs, want)
			}
		})
	}
}

func TestTelegramSpeechEmptyCancellationDoesNotWakeIdleProcessors(t *testing.T) {
	env := newHistoryTestEnv(t)
	wake := env.store.TelegramVoiceJobsChanged()
	for range 3 {
		if err := env.store.CancelTelegramSpeechJobs(t.Context(), "bot"); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-wake:
		t.Fatal("empty cancellation wakes its own disabled processor")
	default:
	}
	_, final := finalSpeechDelivery(t, env, true)
	if err := env.store.MarkDeliverySent(t.Context(), final.ID, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	wake = env.store.TelegramVoiceJobsChanged()
	if err := env.store.CancelTelegramSpeechJobs(t.Context(), "bot"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("actual speech cancellation did not wake its lease holder")
	}
	wake = env.store.TelegramVoiceJobsChanged()
	if err := env.store.CancelTelegramSpeechJobs(t.Context(), "bot"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
		t.Fatal("repeated cancellation keeps waking idle processors")
	default:
	}
}

func TestTelegramSpeechDisableRetiresAcceptedVoiceReplyIntent(t *testing.T) {
	env := newHistoryTestEnv(t)
	_, result := completeSpeechVoice(t, env, true, "it")
	wake := env.store.TelegramVoiceJobsChanged()
	if err := env.store.CancelTelegramSpeechJobs(t.Context(), "bot"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
	default:
		t.Fatal("retired voice reply intent did not wake processors")
	}
	wake = env.store.TelegramVoiceJobsChanged()
	if err := env.store.CancelTelegramSpeechJobs(t.Context(), "bot"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake:
		t.Fatal("already retired voice reply intent kept waking processors")
	default:
	}
	progressEvent(t, env, 2, "turn_started", "voice-turn", result.CommandID)
	progressEvent(t, env, 3, "turn_completed", "voice-turn", "")
	var count int
	if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_speech_jobs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("late final replayed disabled voice intent: %d %v", count, err)
	}
}

func TestTelegramSpeechChatRevocationRetiresAcceptedVoiceReplyIntent(t *testing.T) {
	env := newHistoryTestEnv(t)
	_, result := completeSpeechVoice(t, env, true, "it")
	if err := env.store.ApplyTelegramChatAllowlist(t.Context(), "bot", []int64{999}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.ApplyTelegramChatAllowlist(t.Context(), "bot", []int64{}); err != nil {
		t.Fatal(err)
	}
	progressEvent(t, env, 2, "turn_started", "voice-turn", result.CommandID)
	progressEvent(t, env, 3, "turn_completed", "voice-turn", "")
	var count int
	if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_speech_jobs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("late final replayed revoked voice intent: %d %v", count, err)
	}
}

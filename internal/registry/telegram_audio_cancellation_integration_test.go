package registry

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCancelAllTelegramVoiceJobsFencesLeasesAcrossBotIDs(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	for _, bot := range []string{"bot", "retired-bot"} {
		in := telegramUpdate(env, 10)
		in.BotID, in.Action, in.Target = bot, "connect", env.session.String()
		if _, err := env.store.AcceptTelegram(ctx, in); err != nil {
			t.Fatal(err)
		}
		in.UpdateID, in.Action, in.Target = 11, "", ""
		queueVoiceTest(t, env, in)
	}
	leased := claimVoiceTest(t, env.store)
	if err := env.store.CancelAllTelegramVoiceJobs(ctx, "voice_disabled"); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := env.store.NextTelegramVoiceAttempt(ctx); err != nil || pending {
		t.Fatalf("cancelled voice jobs still pending: pending=%v error=%v", pending, err)
	}
	if _, err := env.store.CompleteTelegramVoice(ctx, leased.ID, leased.LeaseID, "late transcript"); !errors.Is(err, ErrTelegramVoiceLease) {
		t.Fatalf("cancelled voice lease was accepted: %v", err)
	}
	var retired int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_voice_jobs WHERE status='failed' AND error_code='voice_disabled' AND file_id IS NULL`).Scan(&retired); err != nil || retired != 2 {
		t.Fatalf("cancelled voice jobs=%d error=%v", retired, err)
	}
}

func TestCancelAllTelegramSpeechJobsReleasesEveryBotsTextAndReplyIntent(t *testing.T) {
	env := newHistoryTestEnv(t)
	ctx := t.Context()
	_, final := finalSpeechDelivery(t, env, true)
	leased := claimSpeechTest(t, env.store)
	if err := env.store.PrepareTelegramSpeech(ctx, leased.ID, leased.LeaseID); err != nil {
		t.Fatal(err)
	}
	// Retired destinations can outlive their token configuration. Keep another
	// durable speech intent under such an ID while the first is actively leased.
	otherDelivery, otherJob := uuid.New(), uuid.New()
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_deliveries(delivery_id,event_id,bot_id,chat_id,message_thread_id,kind,payload)
        SELECT $1,event_id,'retired-bot',chat_id,message_thread_id,kind,payload FROM telegram_deliveries WHERE delivery_id=$2`, otherDelivery, final.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.pool.Exec(ctx, `INSERT INTO telegram_speech_jobs(job_id,delivery_id,worker_id,runtime_id,runtime_generation,session_id,turn_id,bot_id,chat_id,message_thread_id,user_id,expires_at)
        SELECT $1,$2,worker_id,runtime_id,runtime_generation,session_id,turn_id,'retired-bot',chat_id,message_thread_id,user_id,expires_at FROM telegram_speech_jobs WHERE job_id=$3`, otherJob, otherDelivery, leased.ID); err != nil {
		t.Fatal(err)
	}
	in := telegramUpdate(env, 10)
	in.BotID, in.Action, in.Target = "retired-bot", "connect", env.session.String()
	if _, err := env.store.AcceptTelegram(ctx, in); err != nil {
		t.Fatal(err)
	}
	in.UpdateID, in.Action, in.Target = 11, "", ""
	if _, err := env.store.AcceptTelegramVoice(ctx, in, TelegramVoice{FileID: "opaque", ReplyWithAudio: true}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CancelAllTelegramSpeechJobs(ctx); err != nil {
		t.Fatal(err)
	}
	var cancelled, replyIntents int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_speech_jobs WHERE status='cancelled' AND prepared=0 AND lease_id IS NULL`).Scan(&cancelled); err != nil || cancelled != 2 {
		t.Fatalf("cancelled speech jobs=%d error=%v", cancelled, err)
	}
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_voice_jobs WHERE reply_with_audio=1`).Scan(&replyIntents); err != nil || replyIntents != 0 {
		t.Fatalf("reply intents survived cancellation: count=%d error=%v", replyIntents, err)
	}
	if allowed, err := env.store.CheckTelegramSpeech(ctx, leased.ID, leased.LeaseID); err != nil || allowed {
		t.Fatalf("cancelled speech lease remained allowed: allowed=%v error=%v", allowed, err)
	}
}

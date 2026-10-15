package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

const MaxTelegramSpeechAttempts = 4
const telegramSpeechLifetime = time.Hour

var ErrTelegramSpeechLease = errors.New("registry: Telegram speech lease is no longer current")

// ErrTelegramSpeechPending defers a final answer until every audio chunk is ready.
var ErrTelegramSpeechPending = errors.New("registry: Telegram speech preparation is pending")

// A readiness flag is useful only while its owner still holds a live lease.
// Expired jobs release the ordinary text path even if no speech processor runs.
const pendingSpeechPreparationSQL = `EXISTS (
    SELECT 1 FROM telegram_speech_jobs speech
    WHERE delivery.kind='turn_completed'
      AND speech.bot_id=delivery.bot_id AND speech.chat_id=delivery.chat_id AND speech.message_thread_id=delivery.message_thread_id
      AND speech.session_id=json_extract(delivery.payload,'$.session_id') AND speech.turn_id=json_extract(delivery.payload,'$.data.turn_id')
      AND speech.status IN ('pending','processing')
      AND speech.expires_at>` + sqliteNow + `
      AND (speech.prepared=0 OR speech.status='pending' OR speech.lease_until<=` + sqliteNow + `)
)`

// TelegramSpeechJob references a final answer held until its audio is prepared.
// NextChunk resumes after confirmed messages; audio bytes never enter the database.
type TelegramSpeechJob struct {
	ID, LeaseID                          uuid.UUID
	DeliveryID, SessionID, TurnID, BotID string
	ChatID, TopicID, UserID              int64
	Text, Language                       string
	NextChunk, Attempts                  int
	LeaseUntil                           time.Time
}

type telegramSpeechRecord struct {
	TelegramSpeechJob
	status    string
	expiresAt time.Time
}

func enqueueTelegramSpeech(ctx context.Context, tx *dbTx, deliveryID uuid.UUID, event protocol.Event, bot string, chat, topic int64) error {
	if event.Kind != "turn_completed" || event.SessionID == "" || event.RuntimeID == "" {
		return nil
	}
	var result protocol.Result
	if err := json.Unmarshal(event.Data, &result); err != nil {
		return err
	}
	if strings.TrimSpace(result.Text) == "" || strings.TrimSpace(result.TurnID) == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO telegram_speech_jobs
        (job_id,delivery_id,worker_id,runtime_id,runtime_generation,session_id,turn_id,bot_id,chat_id,message_thread_id,user_id,language,expires_at)
        SELECT $1,$2,voice.worker_id,voice.runtime_id,voice.runtime_generation,voice.session_id,$3,voice.bot_id,voice.chat_id,voice.message_thread_id,voice.user_id,voice.transcript_language,$4
        FROM telegram_voice_jobs voice WHERE voice.status='completed' AND voice.reply_with_audio=1 AND voice.question_secret=0
          AND voice.worker_id=$5 AND voice.runtime_id=$6 AND voice.runtime_generation=$7 AND voice.session_id=$8
          AND voice.expected_turn_id=$3 AND voice.bot_id=$9 AND voice.chat_id=$10 AND voice.message_thread_id=$11
        ORDER BY voice.sequence DESC LIMIT 1 ON CONFLICT DO NOTHING`, uuid.New(), deliveryID, result.TurnID, time.Now().UTC().Add(telegramSpeechLifetime), event.WorkerID, event.RuntimeID, int64(event.RuntimeGeneration), event.SessionID, bot, chat, topic)
	return err
}

// Identifiers passed to this function are fixed internal aliases. Text must be
// authorized independently of its delivery's sent status so waiting speech is
// not canceled merely because the normal text sender has not run yet.
func speechAllowedSQL(job string) string {
	return `EXISTS(SELECT 1 FROM telegram_deliveries delivery
        JOIN sessions speech_session ON speech_session.session_id=` + job + `.session_id
        JOIN workers speech_worker ON speech_worker.worker_id=` + job + `.worker_id
        JOIN runtimes speech_runtime ON speech_runtime.runtime_id=` + job + `.runtime_id
        WHERE delivery.delivery_id=` + job + `.delivery_id AND delivery.kind='turn_completed'
          AND delivery.status<>'cancelled' AND delivery.visibility_revoked=0
          AND speech_worker.enabled=1 AND speech_session.archived=0
          AND speech_session.worker_id=` + job + `.worker_id AND speech_session.runtime_id=` + job + `.runtime_id
          AND speech_runtime.worker_id=` + job + `.worker_id AND speech_runtime.generation=` + job + `.runtime_generation
          AND ` + telegramChatAllowedSQL(job+`.bot_id`, job+`.chat_id`) + `
          AND ` + sessionVisibleSQL(job+`.bot_id`, job+`.chat_id`, job+`.message_thread_id`, job+`.session_id`) + `)`
}

const speechColumns = `job.job_id,COALESCE(job.lease_id,'00000000-0000-0000-0000-000000000000'),job.delivery_id,job.session_id,job.turn_id,
    job.bot_id,job.chat_id,job.message_thread_id,job.user_id,COALESCE(json_extract(delivery.payload,'$.data.text'),''),job.language,
    job.next_chunk,job.attempts,COALESCE(job.lease_until,job.created_at),job.status,job.expires_at`

func scanSpeech(row *dbRow) (telegramSpeechRecord, error) {
	var job telegramSpeechRecord
	err := row.Scan(&job.ID, &job.LeaseID, &job.DeliveryID, &job.SessionID, &job.TurnID, &job.BotID, &job.ChatID, &job.TopicID, &job.UserID, &job.Text, &job.Language, &job.NextChunk, &job.Attempts, &job.LeaseUntil, &job.status, &job.expiresAt)
	return job, err
}

// RecoverTelegramSpeech fences the previous gateway's leases before outbound
// services start. Audio is only in memory, so every surviving job must prepare
// its unsent chunks again. Opening the database alone must not perform recovery:
// administrative commands can open it alongside the running gateway.
func (s *Store) RecoverTelegramSpeech(ctx context.Context, botID string) error {
	if strings.TrimSpace(botID) == "" {
		return errors.New("registry: Telegram bot ID is required")
	}
	result, err := s.pool.Exec(ctx, `UPDATE telegram_speech_jobs
        SET status='pending',prepared=0,lease_id=NULL,lease_until=NULL,
            next_attempt_at=CASE WHEN status='processing' THEN `+sqliteNow+` ELSE next_attempt_at END
        WHERE bot_id=$1 AND status IN ('pending','processing') AND (status='processing' OR prepared<>0)`, botID)
	if err == nil && result.RowsAffected() > 0 {
		s.notifyTelegramVoiceJobs()
	}
	return err
}

// ClaimTelegramSpeech prepares audio before releasing the ordinary final text.
// Expired leases lose readiness because the prior owner's in-memory audio is gone.
func (s *Store) ClaimTelegramSpeech(ctx context.Context, leaseDuration time.Duration) (TelegramSpeechJob, bool, error) {
	if leaseDuration <= 0 || leaseDuration > 15*time.Minute {
		return TelegramSpeechJob{}, false, errors.New("registry: invalid speech lease duration")
	}
	var pending bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_speech_jobs WHERE status IN ('pending','processing'))`).Scan(&pending); err != nil {
		return TelegramSpeechJob{}, false, err
	}
	if !pending {
		return TelegramSpeechJob{}, false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TelegramSpeechJob{}, false, err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	cancelled, err := tx.Exec(ctx, `UPDATE telegram_speech_jobs AS job SET status='cancelled',prepared=0,lease_id=NULL,lease_until=NULL,error_code='speech_unauthorized',completed_at=`+sqliteNow+`
        WHERE job.status IN ('pending','processing') AND NOT `+speechAllowedSQL("job"))
	if err != nil {
		return TelegramSpeechJob{}, false, err
	}
	failed, err := tx.Exec(ctx, `UPDATE telegram_speech_jobs SET status='failed',prepared=0,lease_id=NULL,lease_until=NULL,error_code='speech_failed',completed_at=`+sqliteNow+`
        WHERE status IN ('pending','processing') AND (expires_at<=$1 OR (attempts>=$2 AND (status='pending' OR lease_until<=$1)))`, now, MaxTelegramSpeechAttempts)
	if err != nil {
		return TelegramSpeechJob{}, false, err
	}
	job, err := scanSpeech(tx.QueryRow(ctx, `SELECT `+speechColumns+` FROM telegram_speech_jobs job JOIN telegram_deliveries delivery ON delivery.delivery_id=job.delivery_id
        WHERE job.status IN ('pending','processing') AND job.attempts<$2 AND job.expires_at>$1
          AND ((job.status='pending' AND job.next_attempt_at<=$1) OR (job.status='processing' AND job.lease_until<=$1))
          AND NOT EXISTS(SELECT 1 FROM telegram_speech_jobs earlier WHERE earlier.bot_id=job.bot_id AND earlier.chat_id=job.chat_id
            AND earlier.message_thread_id=job.message_thread_id AND earlier.status IN ('pending','processing') AND earlier.sequence<job.sequence)
        ORDER BY job.sequence LIMIT 1`, now, MaxTelegramSpeechAttempts))
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return TelegramSpeechJob{}, false, err
		}
		if cancelled.RowsAffected()+failed.RowsAffected() > 0 {
			s.notifyTelegramVoiceJobs()
		}
		return TelegramSpeechJob{}, false, nil
	}
	if err != nil {
		return TelegramSpeechJob{}, false, err
	}
	job.LeaseID, job.LeaseUntil = uuid.New(), now.Add(leaseDuration)
	job.Attempts++
	if _, err := tx.Exec(ctx, `UPDATE telegram_speech_jobs SET status='processing',prepared=0,lease_id=$2,lease_until=$3,attempts=$4 WHERE job_id=$1`, job.ID, job.LeaseID, job.LeaseUntil, job.Attempts); err != nil {
		return TelegramSpeechJob{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TelegramSpeechJob{}, false, err
	}
	if cancelled.RowsAffected()+failed.RowsAffected() > 0 {
		s.notifyTelegramVoiceJobs()
	}
	return job.TelegramSpeechJob, true, nil
}

func (s *Store) NextTelegramSpeechAttempt(ctx context.Context) (time.Time, bool, error) {
	var next *time.Time
	err := s.pool.QueryRow(ctx, `SELECT min(min(CASE WHEN job.status='pending' THEN job.next_attempt_at ELSE job.lease_until END,job.expires_at))
        FROM telegram_speech_jobs job JOIN telegram_deliveries delivery ON delivery.delivery_id=job.delivery_id
        WHERE job.status IN ('pending','processing')
          AND NOT EXISTS(SELECT 1 FROM telegram_speech_jobs earlier WHERE earlier.bot_id=job.bot_id AND earlier.chat_id=job.chat_id
            AND earlier.message_thread_id=job.message_thread_id AND earlier.status IN ('pending','processing') AND earlier.sequence<job.sequence)`).Scan(&next)
	if err != nil {
		return time.Time{}, false, err
	}
	if next == nil {
		return time.Time{}, false, nil
	}
	return *next, true, nil
}

func currentSpeechLease(ctx context.Context, tx *dbTx, id, lease uuid.UUID) (telegramSpeechRecord, error) {
	job, err := scanSpeech(tx.QueryRow(ctx, `SELECT `+speechColumns+` FROM telegram_speech_jobs job JOIN telegram_deliveries delivery ON delivery.delivery_id=job.delivery_id WHERE job.job_id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return telegramSpeechRecord{}, ErrTelegramSpeechLease
	}
	if err != nil {
		return telegramSpeechRecord{}, err
	}
	if job.status != "processing" || lease == uuid.Nil || job.LeaseID != lease || !job.LeaseUntil.After(time.Now().UTC()) || !job.expiresAt.After(time.Now().UTC()) {
		return telegramSpeechRecord{}, ErrTelegramSpeechLease
	}
	return job, nil
}

func authorizedSpeechLease(ctx context.Context, tx *dbTx, id, lease uuid.UUID) (telegramSpeechRecord, bool, error) {
	job, err := currentSpeechLease(ctx, tx, id, lease)
	if err != nil {
		return telegramSpeechRecord{}, false, err
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT `+speechAllowedSQL("job")+` FROM telegram_speech_jobs job WHERE job.job_id=$1`, id).Scan(&allowed); err != nil {
		return telegramSpeechRecord{}, false, err
	}
	if !allowed {
		if _, err := tx.Exec(ctx, `UPDATE telegram_speech_jobs SET status='cancelled',prepared=0,lease_id=NULL,lease_until=NULL,error_code='speech_unauthorized',completed_at=`+sqliteNow+` WHERE job_id=$1`, id); err != nil {
			return telegramSpeechRecord{}, false, err
		}
	}
	return job, allowed, nil
}

// CheckTelegramSpeech fences every external I/O boundary during preparation
// and upload. Revoked content stays canceled after later reauthorization.
func (s *Store) CheckTelegramSpeech(ctx context.Context, id, lease uuid.UUID) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	_, allowed, err := authorizedSpeechLease(ctx, tx, id, lease)
	if errors.Is(err, ErrTelegramSpeechLease) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if !allowed {
		s.notifyTelegramVoiceJobs()
	}
	return allowed, nil
}

// PrepareTelegramSpeech releases the text gate after every unsent audio chunk
// has been encoded in memory. The same lease owns readiness and later uploads.
func (s *Store) PrepareTelegramSpeech(ctx context.Context, id, lease uuid.UUID) error {
	return s.refreshTelegramSpeech(ctx, id, lease, true)
}

// RenewTelegramSpeech gives preparation or a text-delivery wait another twelve
// minutes without extending the job's lifetime or changing its readiness.
func (s *Store) RenewTelegramSpeech(ctx context.Context, id, lease uuid.UUID) error {
	return s.refreshTelegramSpeech(ctx, id, lease, false)
}

func (s *Store) refreshTelegramSpeech(ctx context.Context, id, lease uuid.UUID, prepared bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, allowed, err := authorizedSpeechLease(ctx, tx, id, lease)
	if err != nil {
		return err
	}
	if allowed {
		if _, err := tx.Exec(ctx, `UPDATE telegram_speech_jobs SET prepared=CASE WHEN $2 THEN 1 ELSE prepared END,lease_until=$3 WHERE job_id=$1`, id, prepared, time.Now().UTC().Add(12*time.Minute)); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if prepared || !allowed {
		s.notifyTelegramVoiceJobs()
	}
	if !allowed {
		return ErrTelegramSpeechLease
	}
	return nil
}

// TelegramSpeechTextSent is the upload barrier: all ordinary text chunks must
// be checkpointed before any prepared audio can be sent.
func (s *Store) TelegramSpeechTextSent(ctx context.Context, id, lease uuid.UUID) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	_, allowed, err := authorizedSpeechLease(ctx, tx, id, lease)
	if err != nil {
		return false, err
	}
	var sent bool
	if allowed {
		if err := tx.QueryRow(ctx, `SELECT job.prepared=1 AND delivery.status='sent'
            FROM telegram_speech_jobs job JOIN telegram_deliveries delivery ON delivery.delivery_id=job.delivery_id WHERE job.job_id=$1`, id).Scan(&sent); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if !allowed {
		s.notifyTelegramVoiceJobs()
		return false, ErrTelegramSpeechLease
	}
	return sent, nil
}

// CheckpointTelegramSpeech records a confirmed Telegram message and extends the
// current lease for another chunk. Telegram has no idempotency key: a crash
// after sendVoice succeeds but before this commit can repeat that one chunk.
func (s *Store) CheckpointTelegramSpeech(ctx context.Context, id, lease uuid.UUID, index int, messageID int64, done bool) error {
	if index < 0 || index >= 1000 || messageID <= 0 {
		return errors.New("registry: invalid speech checkpoint")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	job, err := currentSpeechLease(ctx, tx, id, lease)
	if err != nil {
		return err
	}
	var ready bool
	if err := tx.QueryRow(ctx, `SELECT job.prepared=1 AND delivery.status='sent'
        FROM telegram_speech_jobs job JOIN telegram_deliveries delivery ON delivery.delivery_id=job.delivery_id WHERE job.job_id=$1`, id).Scan(&ready); err != nil {
		return err
	}
	if !ready {
		return ErrTelegramSpeechPending
	}
	if index < job.NextChunk {
		var previous int64
		if err := tx.QueryRow(ctx, `SELECT telegram_message_id FROM telegram_speech_chunks WHERE job_id=$1 AND chunk_index=$2`, id, index).Scan(&previous); err != nil {
			return err
		}
		if previous != messageID {
			return errors.New("registry: conflicting speech checkpoint")
		}
		return tx.Commit(ctx)
	}
	if index != job.NextChunk {
		return errors.New("registry: speech checkpoint skipped a chunk")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_speech_chunks(job_id,chunk_index,telegram_message_id) VALUES($1,$2,$3)`, id, index, messageID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bot_message_routes(bot_id,chat_id,message_id,session_id,turn_id) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, job.BotID, job.ChatID, messageID, job.SessionID, job.TurnID); err != nil {
		return err
	}
	if done {
		_, err = tx.Exec(ctx, `UPDATE telegram_speech_jobs SET status='completed',next_chunk=$2,lease_id=NULL,lease_until=NULL,error_code=NULL,completed_at=`+sqliteNow+` WHERE job_id=$1`, id, index+1)
	} else {
		_, err = tx.Exec(ctx, `UPDATE telegram_speech_jobs SET next_chunk=$2,lease_until=$3,error_code=NULL WHERE job_id=$1`, id, index+1, time.Now().UTC().Add(12*time.Minute))
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.notifyTelegramVoiceJobs()
	return nil
}

func speechErrorCode(code string) string {
	switch code {
	case "speech_disabled", "speech_unauthorized", "speech_unavailable", "speech_invalid", "speech_encoding_failed", "speech_synthesis_failed", "speech_send_failed", "speech_timeout", "speech_cancelled":
		return code
	default:
		return "speech_failed"
	}
}

func (s *Store) FailTelegramSpeech(ctx context.Context, id, lease uuid.UUID, code string, retryable bool, retryAfter ...time.Duration) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	job, err := currentSpeechLease(ctx, tx, id, lease)
	if err != nil {
		return err
	}
	code = speechErrorCode(code)
	delay := time.Duration(1<<job.Attempts) * time.Second
	if len(retryAfter) > 0 && retryAfter[0] > delay {
		delay = retryAfter[0]
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	if retryable && job.Attempts < MaxTelegramSpeechAttempts && job.expiresAt.After(time.Now().UTC().Add(delay+10*time.Second)) {
		_, err = tx.Exec(ctx, `UPDATE telegram_speech_jobs SET status='pending',prepared=0,lease_id=NULL,lease_until=NULL,error_code=$2,next_attempt_at=$3 WHERE job_id=$1`, id, code, time.Now().UTC().Add(delay))
	} else {
		_, err = tx.Exec(ctx, `UPDATE telegram_speech_jobs SET status='failed',prepared=0,lease_id=NULL,lease_until=NULL,error_code=$2,completed_at=`+sqliteNow+` WHERE job_id=$1`, id, code)
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.notifyTelegramVoiceJobs()
	return nil
}

// CancelTelegramSpeechJobs permanently retires queued content when local
// audio replies are disabled. An in-flight API send has the same unavoidable
// checkpoint boundary as ordinary Telegram text delivery.
func (s *Store) CancelTelegramSpeechJobs(ctx context.Context, botID string) error {
	if strings.TrimSpace(botID) == "" {
		return errors.New("registry: Telegram bot ID is required")
	}
	return s.cancelSpeechJobs(ctx, "bot_id=$1", []any{botID})
}

// CancelAllTelegramSpeechJobs releases held text and audio intents for every
// destination when the shared audio reply setting is disabled.
func (s *Store) CancelAllTelegramSpeechJobs(ctx context.Context) error {
	return s.cancelSpeechJobs(ctx, "TRUE", nil)
}

func (s *Store) cancelSpeechJobs(ctx context.Context, predicate string, args []any) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE telegram_speech_jobs SET status='cancelled',prepared=0,lease_id=NULL,lease_until=NULL,error_code='speech_disabled',completed_at=`+sqliteNow+` WHERE `+predicate+` AND status IN ('pending','processing')`, args...)
	if err != nil {
		return err
	}
	flags, err := tx.Exec(ctx, `UPDATE telegram_voice_jobs SET reply_with_audio=0 WHERE `+predicate+` AND reply_with_audio=1`, args...)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if result.RowsAffected()+flags.RowsAffected() > 0 {
		s.notifyTelegramVoiceJobs()
	}
	return nil
}

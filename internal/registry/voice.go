package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

const (
	MaxTelegramVoiceBytes    = 20 << 20
	MaxTelegramVoiceDuration = 300
	MaxTelegramVoicePending  = 20
	MaxTelegramVoiceAttempts = 4 // Initial attempt and three transient retries.
	telegramVoiceLifetime    = 15 * time.Minute
)

// ErrTelegramVoiceLease prevents a delayed processor from committing after a
// restart, cancellation, or another processor reclaiming its expired lease.
var ErrTelegramVoiceLease = errors.New("registry: Telegram voice lease is no longer current")

// TelegramVoice contains Telegram metadata only; file URLs, credentials and
// audio bytes must never be supplied. FileSize may be zero when Telegram omits it.
type TelegramVoice struct {
	FileID         string `json:"file_id"`
	FileUniqueID   string `json:"file_unique_id"`
	MIMEType       string `json:"mime_type"`
	Duration       int    `json:"duration"`
	FileSize       int64  `json:"file_size"`
	ReplyWithAudio bool   `json:"-"`
}

// TelegramVoiceJob is an immutable download request with a fenced lease. The
// processor must recheck its configured user authorization before external I/O
// and completion; chat authorization is also rechecked by the registry.
type TelegramVoiceJob struct {
	ID, LeaseID                       uuid.UUID
	BotID                             string
	UpdateID, UserID, ChatID, TopicID int64
	TelegramVoice
	Attempts   int
	LeaseUntil time.Time
}

type telegramVoiceRecord struct {
	TelegramVoiceJob
	sequence             int64
	target               routeTarget
	approvalID           *uuid.UUID
	questionID           string
	questionSecret       bool
	replyTo              int64
	ttl                  time.Duration
	createdAt, expiresAt time.Time
	status               string
}

type telegramVoiceHub struct {
	mu      sync.Mutex
	changed chan struct{}
	closed  bool
}

// TelegramVoiceJobsChanged is an in-process wakeup, not the source of truth.
// Capture it before claiming work to avoid losing a concurrent acceptance. A
// processor should also wake at most once per second while retries/leases are
// outstanding and use an idle fallback for jobs accepted by another process.
func (s *Store) TelegramVoiceJobsChanged() <-chan struct{} {
	s.voice.mu.Lock()
	defer s.voice.mu.Unlock()
	if s.voice.changed == nil {
		s.voice.changed = make(chan struct{})
		if s.voice.closed {
			close(s.voice.changed)
		}
	}
	return s.voice.changed
}

func (s *Store) notifyTelegramVoiceJobs() {
	s.voice.mu.Lock()
	defer s.voice.mu.Unlock()
	if s.voice.closed {
		return
	}
	if s.voice.changed != nil {
		close(s.voice.changed)
	}
	s.voice.changed = make(chan struct{})
}

func (s *Store) closeTelegramVoiceJobs() {
	s.voice.mu.Lock()
	defer s.voice.mu.Unlock()
	if s.voice.closed {
		return
	}
	s.voice.closed = true
	if s.voice.changed != nil {
		close(s.voice.changed)
	}
}

// AcceptTelegramVoice durably deduplicates and freezes the destination before
// any download or transcription. It deliberately ignores action/text parsing:
// spoken slash commands are always ordinary text or a frozen question answer.
func (s *Store) AcceptTelegramVoice(ctx context.Context, in IncomingUpdate, voice TelegramVoice) (AcceptResult, error) {
	if err := validateIncoming(in); err != nil {
		return AcceptResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AcceptResult{}, err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx, `INSERT INTO telegram_updates(bot_id,update_id,user_id,chat_id,raw)
        VALUES($1,$2,$3,$4,'{}') ON CONFLICT(bot_id,update_id) DO NOTHING`, in.BotID, in.UpdateID, in.UserID, in.ChatID)
	if err != nil {
		return AcceptResult{}, fmt.Errorf("registry: accept voice receipt: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return AcceptResult{Duplicate: true}, tx.Commit(ctx)
	}
	if err := rememberTelegramContext(ctx, tx, in); err != nil {
		return AcceptResult{}, err
	}
	result, err := acceptTelegramVoice(ctx, tx, in, voice)
	if err != nil {
		if !deterministicTelegramError(err) {
			return AcceptResult{}, err
		}
		result = AcceptResult{View: "error", ErrorCode: telegramErrorCode(err)}
	}
	if err := queueUIResponse(ctx, tx, in, result); err != nil {
		return AcceptResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AcceptResult{}, err
	}
	if result.VoiceJobID != "" {
		s.notifyTelegramVoiceJobs()
	}
	return result, nil
}

func acceptTelegramVoice(ctx context.Context, tx *dbTx, in IncomingUpdate, voice TelegramVoice) (AcceptResult, error) {
	code := ""
	switch {
	case voice.FileSize > MaxTelegramVoiceBytes:
		code = "voice_too_large"
	case voice.Duration > MaxTelegramVoiceDuration:
		code = "voice_too_long"
	case strings.TrimSpace(voice.FileID) == "" || len(voice.FileID) > 1024 || len(voice.FileUniqueID) > 1024 || len(voice.MIMEType) > 128 || voice.Duration < 0 || voice.FileSize < 0 || strings.ContainsAny(voice.FileID, ":/\\\r\n\t "):
		code = "voice_unsupported"
	}
	if code != "" {
		return AcceptResult{View: "error", ErrorCode: code}, nil
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT `+telegramChatAllowedSQL("$1", "$2"), in.BotID, in.ChatID).Scan(&allowed); err != nil {
		return AcceptResult{}, err
	}
	if !allowed {
		return AcceptResult{View: "error", ErrorCode: "voice_unauthorized"}, nil
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM telegram_voice_jobs WHERE bot_id=$1 AND user_id=$2 AND status IN ('pending','processing')`, in.BotID, in.UserID).Scan(&count); err != nil {
		return AcceptResult{}, err
	}
	if count >= MaxTelegramVoicePending {
		return AcceptResult{View: "error", ErrorCode: "voice_queue_full"}, nil
	}
	job := telegramVoiceRecord{TelegramVoiceJob: TelegramVoiceJob{ID: uuid.New(), BotID: in.BotID, UpdateID: in.UpdateID, UserID: in.UserID, ChatID: in.ChatID, TopicID: in.TopicID, TelegramVoice: voice}, replyTo: in.ReplyToMessageID, ttl: in.CommandTTL}
	if job.ttl == 0 {
		job.ttl = defaultCommandTTL
	}
	if job.ttl <= 0 {
		return AcceptResult{}, ErrTelegramTarget
	}
	if err := awaitTelegramReplyRoute(ctx, tx, in); err != nil {
		return AcceptResult{}, err
	}
	if in.ReplyToMessageID > 0 {
		var question *string
		var retired bool
		err := tx.QueryRow(ctx, `SELECT route.approval_id,route.question_id,COALESCE(helper.retired,0)
            FROM bot_message_routes route
            LEFT JOIN telegram_input_reply_messages message USING(bot_id,chat_id,message_id)
            LEFT JOIN telegram_input_reply_helpers helper ON helper.delivery_id=message.delivery_id
            WHERE route.bot_id=$1 AND route.chat_id=$2 AND route.message_id=$3`, in.BotID, in.ChatID, in.ReplyToMessageID).Scan(&job.approvalID, &question, &retired)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return AcceptResult{}, err
		}
		if job.approvalID != nil {
			if retired {
				return AcceptResult{}, ErrTelegramTarget
			}
			if question != nil {
				job.questionID = *question
			}
			if err := freezeTelegramVoiceQuestion(ctx, tx, &job); err != nil {
				return AcceptResult{}, err
			}
		}
	}
	if job.approvalID == nil {
		target, err := resolveRoute(ctx, tx, in)
		if err != nil {
			return AcceptResult{}, err
		}
		job.target = target
	}
	var approval any
	if job.approvalID != nil {
		approval = *job.approvalID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_voice_jobs
        (job_id,bot_id,update_id,user_id,chat_id,message_thread_id,reply_to_message_id,
         worker_id,runtime_id,runtime_generation,session_id,thread_id,expected_turn_id,approval_id,question_id,question_secret,
         file_id,file_unique_id,mime_type,duration_seconds,file_size,command_ttl_ns,expires_at,reply_with_audio)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULLIF($15,''),$16,$17,$18,$19,$20,$21,$22,$23,$24)`,
		job.ID, job.BotID, job.UpdateID, job.UserID, job.ChatID, job.TopicID, job.replyTo,
		job.target.workerID, job.target.runtimeID, job.target.generation, job.target.sessionID, job.target.threadID, job.target.activeTurnID, approval, job.questionID, job.questionSecret,
		voice.FileID, voice.FileUniqueID, voice.MIMEType, voice.Duration, voice.FileSize, int64(job.ttl), time.Now().UTC().Add(telegramVoiceLifetime), voice.ReplyWithAudio); err != nil {
		return AcceptResult{}, fmt.Errorf("registry: queue voice: %w", err)
	}
	return AcceptResult{View: "voice_transcribing", VoiceJobID: job.ID.String(), SessionID: job.target.sessionID.String(), RuntimeID: job.target.runtimeID.String()}, nil
}

// freezeTelegramVoiceQuestion validates a reply without mutating the question.
// A later answer to this same question cannot be replaced by a slow transcript.
func freezeTelegramVoiceQuestion(ctx context.Context, tx *dbTx, job *telegramVoiceRecord) error {
	var raw, answersRaw []byte
	var state, turn string
	var claimed *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT worker_id,runtime_id,runtime_generation,session_id,codex_thread_id,COALESCE(codex_turn_id,''),state,response_command_id,request_payload,input_answers
        FROM approvals WHERE approval_id=$1`, *job.approvalID).Scan(&job.target.workerID, &job.target.runtimeID, &job.target.generation, &job.target.sessionID, &job.target.threadID, &turn, &state, &claimed, &raw, &answersRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTelegramTarget
	}
	if err != nil {
		return err
	}
	if state != "pending" || claimed != nil {
		return ErrTelegramTarget
	}
	var request protocol.Approval
	if err := json.Unmarshal(raw, &request); err != nil || (request.Type != "input" && request.Type != "user_input") {
		return ErrTelegramTarget
	}
	if job.questionID == "" && len(request.Questions) == 1 {
		job.questionID = request.Questions[0].ID
	}
	found := false
	for _, q := range request.Questions {
		if q.ID == job.questionID {
			found = true
			job.questionSecret = q.Secret
		}
	}
	if !found || job.questionID == "" {
		return ErrTelegramTarget
	}
	var answers map[string][]string
	if err := json.Unmarshal(answersRaw, &answers); err != nil {
		return err
	}
	if len(answers[job.questionID]) > 0 {
		return ErrTelegramTarget
	}
	current, err := sessionRoute(ctx, tx, job.target.sessionID)
	if err != nil {
		return err
	}
	if current.workerID != job.target.workerID || current.runtimeID != job.target.runtimeID || current.generation != job.target.generation || current.threadID != job.target.threadID {
		return ErrTelegramTarget
	}
	if request.Async {
		turn = ""
	}
	if err := inputApprovalCurrent(ctx, tx, job.target, turn); err != nil {
		return err
	}
	job.target.activeTurnID = turn
	return nil
}

const telegramVoiceColumns = `sequence,job_id,bot_id,update_id,user_id,chat_id,message_thread_id,reply_to_message_id,
    worker_id,runtime_id,runtime_generation,session_id,thread_id,expected_turn_id,approval_id,COALESCE(question_id,''),question_secret,
    COALESCE(file_id,''),COALESCE(file_unique_id,''),COALESCE(mime_type,''),COALESCE(duration_seconds,0),COALESCE(file_size,0),command_ttl_ns,
    status,attempts,COALESCE(lease_id,'00000000-0000-0000-0000-000000000000'),COALESCE(lease_until,created_at),created_at,expires_at,reply_with_audio`

func scanTelegramVoice(row *dbRow) (telegramVoiceRecord, error) {
	var j telegramVoiceRecord
	var ttl int64
	err := row.Scan(&j.sequence, &j.ID, &j.BotID, &j.UpdateID, &j.UserID, &j.ChatID, &j.TopicID, &j.replyTo,
		&j.target.workerID, &j.target.runtimeID, &j.target.generation, &j.target.sessionID, &j.target.threadID, &j.target.activeTurnID, &j.approvalID, &j.questionID, &j.questionSecret,
		&j.FileID, &j.FileUniqueID, &j.MIMEType, &j.Duration, &j.FileSize, &ttl, &j.status, &j.Attempts, &j.LeaseID, &j.LeaseUntil, &j.createdAt, &j.expiresAt, &j.ReplyWithAudio)
	j.ttl = time.Duration(ttl)
	return j, err
}

// ClaimTelegramVoice recovers expired leases and permits one active job per
// bot/chat/topic. An unavailable first job holds only its own conversation.
func (s *Store) ClaimTelegramVoice(ctx context.Context, leaseDuration time.Duration) (TelegramVoiceJob, bool, error) {
	if leaseDuration <= 0 || leaseDuration > 15*time.Minute {
		return TelegramVoiceJob{}, false, errors.New("registry: invalid voice lease duration")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TelegramVoiceJob{}, false, err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	rows, err := tx.Query(ctx, `SELECT job_id FROM telegram_voice_jobs WHERE status IN ('pending','processing')
        AND (expires_at<=$1 OR (attempts>=$2 AND (status='pending' OR lease_until<=$1))) ORDER BY sequence LIMIT 100`, now, MaxTelegramVoiceAttempts)
	if err != nil {
		return TelegramVoiceJob{}, false, err
	}
	var expired []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return TelegramVoiceJob{}, false, err
		}
		expired = append(expired, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return TelegramVoiceJob{}, false, err
	}
	for _, id := range expired {
		job, err := scanTelegramVoice(tx.QueryRow(ctx, `SELECT `+telegramVoiceColumns+` FROM telegram_voice_jobs WHERE job_id=$1`, id))
		if err != nil {
			return TelegramVoiceJob{}, false, err
		}
		if err := finishTelegramVoiceError(ctx, tx, job, "voice_transcription_failed"); err != nil {
			return TelegramVoiceJob{}, false, err
		}
	}
	job, err := scanTelegramVoice(tx.QueryRow(ctx, `SELECT `+telegramVoiceColumns+` FROM telegram_voice_jobs job
        WHERE job.status IN ('pending','processing') AND job.attempts<$2 AND job.expires_at>$1
          AND ((job.status='pending' AND job.next_attempt_at<=$1) OR (job.status='processing' AND job.lease_until<=$1))
          AND NOT EXISTS(SELECT 1 FROM telegram_voice_jobs earlier WHERE earlier.bot_id=job.bot_id AND earlier.chat_id=job.chat_id
            AND earlier.message_thread_id=job.message_thread_id AND earlier.status IN ('pending','processing') AND earlier.sequence<job.sequence)
        ORDER BY job.sequence LIMIT 1`, now, MaxTelegramVoiceAttempts))
	if errors.Is(err, sql.ErrNoRows) {
		return TelegramVoiceJob{}, false, tx.Commit(ctx)
	}
	if err != nil {
		return TelegramVoiceJob{}, false, err
	}
	job.LeaseID = uuid.New()
	job.LeaseUntil = now.Add(leaseDuration)
	job.Attempts++
	if _, err := tx.Exec(ctx, `UPDATE telegram_voice_jobs SET status='processing',attempts=$2,lease_id=$3,lease_until=$4 WHERE job_id=$1`, job.ID, job.Attempts, job.LeaseID, job.LeaseUntil); err != nil {
		return TelegramVoiceJob{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TelegramVoiceJob{}, false, err
	}
	return job.TelegramVoiceJob, true, nil
}

// NextTelegramVoiceAttempt permits an idle processor to wait on notifications
// without polling the database. The deadline includes crash recovery and expiry.
func (s *Store) NextTelegramVoiceAttempt(ctx context.Context) (time.Time, bool, error) {
	var next *time.Time
	err := s.pool.QueryRow(ctx, `SELECT min(min(CASE WHEN job.status='pending' THEN job.next_attempt_at ELSE job.lease_until END,job.expires_at))
        FROM telegram_voice_jobs job WHERE job.status IN ('pending','processing')
          AND NOT EXISTS(SELECT 1 FROM telegram_voice_jobs earlier WHERE earlier.bot_id=job.bot_id AND earlier.chat_id=job.chat_id
            AND earlier.message_thread_id=job.message_thread_id AND earlier.status IN ('pending','processing') AND earlier.sequence<job.sequence)`).Scan(&next)
	if err != nil {
		return time.Time{}, false, err
	}
	if next == nil {
		return time.Time{}, false, nil
	}
	return *next, true, nil
}

func currentTelegramVoiceLease(ctx context.Context, tx *dbTx, id, lease uuid.UUID) (telegramVoiceRecord, error) {
	job, err := scanTelegramVoice(tx.QueryRow(ctx, `SELECT `+telegramVoiceColumns+` FROM telegram_voice_jobs WHERE job_id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return telegramVoiceRecord{}, ErrTelegramVoiceLease
	}
	if err != nil {
		return telegramVoiceRecord{}, err
	}
	if job.status != "processing" || lease == uuid.Nil || job.LeaseID != lease || !job.LeaseUntil.After(time.Now().UTC()) {
		return telegramVoiceRecord{}, ErrTelegramVoiceLease
	}
	return job, nil
}

// CompleteTelegramVoice submits plain transcript text and its UI delivery in
// one transaction. Replays cannot create a second command or second answer.
func (s *Store) CompleteTelegramVoice(ctx context.Context, id, lease uuid.UUID, transcript string) (AcceptResult, error) {
	return s.CompleteTelegramVoiceWithLanguage(ctx, id, lease, transcript, "en")
}

func (s *Store) CompleteTelegramVoiceWithLanguage(ctx context.Context, id, lease uuid.UUID, transcript, language string) (AcceptResult, error) {
	if language != "it" {
		language = "en"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AcceptResult{}, err
	}
	defer tx.Rollback(ctx)
	job, err := currentTelegramVoiceLease(ctx, tx, id, lease)
	if err != nil {
		return AcceptResult{}, err
	}
	in := IncomingUpdate{BotID: job.BotID, UpdateID: job.UpdateID, UserID: job.UserID, ChatID: job.ChatID, TopicID: job.TopicID, ReplyToMessageID: job.replyTo, Action: "text", Text: strings.TrimSpace(transcript), CommandTTL: job.ttl, commandCreatedAt: job.createdAt}
	code := ""
	if in.Text == "" {
		code = "voice_empty"
	} else if !utf8.ValidString(in.Text) || len(in.Text) > protocol.MaxWebUITextBytes {
		code = "voice_transcription_failed"
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT `+telegramChatAllowedSQL("$1", "$2"), job.BotID, job.ChatID).Scan(&allowed); err != nil {
		return AcceptResult{}, err
	}
	if !allowed {
		code = "voice_unauthorized"
	}
	if !job.expiresAt.After(time.Now().UTC()) {
		code = "voice_transcription_failed"
	}
	var result AcceptResult
	if code == "" {
		current, e := sessionRoute(ctx, tx, job.target.sessionID)
		if e != nil {
			err = e
		} else if current.workerID != job.target.workerID || current.runtimeID != job.target.runtimeID || current.generation != job.target.generation || current.threadID != job.target.threadID {
			err = ErrTelegramTarget
		}
		if err == nil && job.approvalID != nil {
			frozen := job.target
			err = freezeTelegramVoiceQuestion(ctx, tx, &job)
			if err == nil && job.target != frozen {
				err = ErrTelegramTarget
			}
			if err == nil {
				result, err = acceptInputApproval(ctx, tx, in, *job.approvalID, job.questionID, false)
			}
		} else if err == nil {
			// An idle receipt freezes StartTurn and can enter the normal
			// next-turn queue. A receipt during a turn freezes Steer and
			// must never guide a different turn after transcription.
			if job.target.activeTurnID != "" && current.activeTurnID != job.target.activeTurnID {
				err = ErrStaleTurn
			} else {
				operation := protocol.StartTurn
				if job.target.activeTurnID != "" {
					operation = protocol.Steer
				}
				var command protocol.Command
				command, err = createTelegramCommand(ctx, tx, in, job.target, operation, job.target.sessionID.String(), job.target.activeTurnID, protocol.Arguments{Text: in.Text})
				if err == nil {
					result = AcceptResult{View: "queued", SessionID: job.target.sessionID.String(), RuntimeID: job.target.runtimeID.String(), CommandID: command.ID}
				}
			}
		}
		if err != nil {
			if !deterministicTelegramError(err) {
				return AcceptResult{}, err
			}
			code = telegramErrorCode(err)
		}
	}
	if code != "" {
		if err := finishTelegramVoiceError(ctx, tx, job, code); err != nil {
			return AcceptResult{}, err
		}
		result = AcceptResult{View: "error", VoiceJobID: id.String(), ErrorCode: code}
	} else {
		if err := retireTelegramVoiceProgress(ctx, tx, job); err != nil {
			return AcceptResult{}, err
		}
		if result.ApprovalID != "" {
			if err := queueUIResponse(ctx, tx, in, result); err != nil {
				return AcceptResult{}, err
			}
			if result.ProgressReposition {
				if err := repositionTelegramProgress(ctx, tx, in); err != nil {
					return AcceptResult{}, err
				}
			}
		}
		result.View = "voice_transcript"
		// The transcript is an ordinary session message. The original
		// question gets its own answer edit; this copy must not acquire
		// a question route or be overwritten by a later question answer.
		result.ApprovalID = ""
		result.QuestionID = ""
		result.VoiceJobID = id.String()
		result.Transcript = in.Text
		if job.questionSecret {
			result.Transcript = "[REDACTED]"
		}
		if err := queueUIResponse(ctx, tx, in, result); err != nil {
			return AcceptResult{}, err
		}
		var command any
		if result.CommandID != "" {
			command = result.CommandID
		}
		if _, err := tx.Exec(ctx, `UPDATE telegram_voice_jobs SET status='completed',completed_at=`+sqliteNow+`,command_id=$2,
        lease_id=NULL,lease_until=NULL,file_id=NULL,file_unique_id=NULL,mime_type=NULL,duration_seconds=NULL,file_size=NULL,error_code=NULL,transcript_language=$3 WHERE job_id=$1`, id, command, language); err != nil {
			return AcceptResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AcceptResult{}, err
	}
	s.notifyTelegramVoiceJobs()
	s.notifySessionActivity()
	return result, nil
}

// FailTelegramVoice stores only a bounded renderer code. Arbitrary upstream
// errors may contain tokens or transcript content and are never persisted.
func (s *Store) FailTelegramVoice(ctx context.Context, id, lease uuid.UUID, code string, retryable bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	job, err := currentTelegramVoiceLease(ctx, tx, id, lease)
	if err != nil {
		return err
	}
	code = telegramVoiceErrorCode(code)
	if retryable && job.Attempts < MaxTelegramVoiceAttempts && job.expiresAt.After(time.Now().UTC().Add(10*time.Second)) {
		delay := time.Duration(1<<job.Attempts) * time.Second
		_, err = tx.Exec(ctx, `UPDATE telegram_voice_jobs SET status='pending',lease_id=NULL,lease_until=NULL,error_code=$2,next_attempt_at=$3 WHERE job_id=$1`, id, code, time.Now().UTC().Add(delay))
	} else {
		err = finishTelegramVoiceError(ctx, tx, job, code)
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

func telegramVoiceErrorCode(code string) string {
	switch code {
	case "voice_disabled", "voice_invalid", "voice_too_large", "voice_too_long", "voice_unsupported", "voice_download_failed", "voice_decode_failed", "voice_transcription_failed", "voice_timeout", "voice_empty", "voice_unauthorized", "voice_cancelled", "voice_unavailable", "target_unavailable", "stale_turn":
		return code
	default:
		return "voice_transcription_failed"
	}
}

func finishTelegramVoiceError(ctx context.Context, tx *dbTx, job telegramVoiceRecord, code string) error {
	code = telegramVoiceErrorCode(code)
	if err := retireTelegramVoiceProgress(ctx, tx, job); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE telegram_voice_jobs SET status='failed',completed_at=`+sqliteNow+`,error_code=$2,
        lease_id=NULL,lease_until=NULL,file_id=NULL,file_unique_id=NULL,mime_type=NULL,duration_seconds=NULL,file_size=NULL WHERE job_id=$1`, job.ID, code); err != nil {
		return err
	}
	in := IncomingUpdate{BotID: job.BotID, UserID: job.UserID, ChatID: job.ChatID, TopicID: job.TopicID}
	return queueUIResponse(ctx, tx, in, AcceptResult{View: "error", VoiceJobID: job.ID.String(), SessionID: job.target.sessionID.String(), RuntimeID: job.target.runtimeID.String(), ErrorCode: code})
}

// CancelTelegramVoiceJobs retires queued/leased jobs when voice support is
// disabled. Existing lease holders cannot later submit their transcript.
func (s *Store) CancelTelegramVoiceJobs(ctx context.Context, botID, code string) error {
	if strings.TrimSpace(botID) == "" {
		return errors.New("registry: Telegram bot ID is required")
	}
	return s.cancelVoiceJobs(ctx, "bot_id=$1", []any{botID}, code)
}

// CancelAllTelegramVoiceJobs applies shared voice settings to every bot,
// including retired bot IDs which no longer have an active API client.
func (s *Store) CancelAllTelegramVoiceJobs(ctx context.Context, code string) error {
	return s.cancelVoiceJobs(ctx, "TRUE", nil, code)
}

func (s *Store) cancelVoiceJobs(ctx context.Context, predicate string, args []any, code string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := cancelTelegramVoiceJobs(ctx, tx, predicate, args, code); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.notifyTelegramVoiceJobs()
	return nil
}

func cancelTelegramVoiceJobs(ctx context.Context, tx *dbTx, predicate string, args []any, code string) error {
	rows, err := tx.Query(ctx, `SELECT job_id FROM telegram_voice_jobs WHERE status IN ('pending','processing') AND `+predicate, args...)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		job, err := scanTelegramVoice(tx.QueryRow(ctx, `SELECT `+telegramVoiceColumns+` FROM telegram_voice_jobs WHERE job_id=$1`, id))
		if err != nil {
			return err
		}
		if err := finishTelegramVoiceError(ctx, tx, job, code); err != nil {
			return err
		}
	}
	return nil
}

// Reuse durable message cleanup and its late-checkpoint handling. A transcript
// that finishes before the acknowledgement is sent cancels that stale send.
func retireTelegramVoiceProgress(ctx context.Context, tx *dbTx, job telegramVoiceRecord) error {
	rows, err := tx.Query(ctx, `SELECT delivery.delivery_id,COALESCE(delivery.telegram_message_id,0),COALESCE(chunk.telegram_message_id,0)
        FROM telegram_deliveries delivery LEFT JOIN telegram_delivery_chunks chunk USING(delivery_id)
        WHERE delivery.kind='ui_response' AND json_extract(delivery.payload,'$.view')='voice_transcribing'
          AND json_extract(delivery.payload,'$.voice_job_id')=$1`, job.ID)
	if err != nil {
		return err
	}
	type message struct {
		delivery      string
		direct, chunk int64
	}
	var messages []message
	for rows.Next() {
		var m message
		if err := rows.Scan(&m.delivery, &m.direct, &m.chunk); err != nil {
			rows.Close()
			return err
		}
		messages = append(messages, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, m := range messages {
		if _, err := tx.Exec(ctx, `UPDATE telegram_deliveries SET visibility_revoked=1,status=CASE WHEN status IN ('pending','failed') THEN 'cancelled' ELSE status END WHERE delivery_id=$1`, m.delivery); err != nil {
			return err
		}
		for _, id := range []int64{m.direct, m.chunk} {
			if id > 0 {
				if _, err := enqueuePickerCleanup(ctx, tx, job.BotID, job.ChatID, job.TopicID, m.delivery, id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

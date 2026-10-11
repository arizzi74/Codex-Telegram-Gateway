package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/voice"
)

type TelegramVoiceStore interface {
	TelegramVoiceJobsChanged() <-chan struct{}
	NextTelegramVoiceAttempt(context.Context) (time.Time, bool, error)
	ClaimTelegramVoice(context.Context, time.Duration) (registry.TelegramVoiceJob, bool, error)
	CompleteTelegramVoice(context.Context, uuid.UUID, uuid.UUID, string) (registry.AcceptResult, error)
	FailTelegramVoice(context.Context, uuid.UUID, uuid.UUID, string, bool) error
	CancelTelegramVoiceJobs(context.Context, string, string) error
}

type voiceEngine interface {
	Start(context.Context) error
	Transcribe(context.Context, []float32) (voice.Result, error)
	Close() error
}

// TelegramVoiceProcessor serializes CPU-heavy work. The helper stays resident
// between jobs; the wakeup channel and slow idle fallback avoid a polling loop.
type TelegramVoiceProcessor struct {
	store  TelegramVoiceStore
	api    TelegramVoiceAPI
	cfg    config.GatewayConfig
	log    *slog.Logger
	engine voiceEngine
	decode func(context.Context, io.Reader) ([]float32, error)
}

func NewTelegramVoiceProcessor(store TelegramVoiceStore, api TelegramVoiceAPI, cfg config.GatewayConfig, logger *slog.Logger) *TelegramVoiceProcessor {
	return &TelegramVoiceProcessor{store: store, api: api, cfg: cfg, log: logger}
}

func (p *TelegramVoiceProcessor) Run(ctx context.Context) {
	if !p.cfg.Voice.Enabled {
		cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := p.store.CancelTelegramVoiceJobs(cleanupCtx, p.cfg.Secrets.BotName, "voice_cancelled"); err != nil && ctx.Err() == nil {
			p.log.Warn("Pending voice-note cancellation deferred")
		}
		return
	}
	if p.engine == nil {
		engine, err := voice.New(voice.Config{BinaryPath: p.cfg.Voice.HelperPath, ModelPath: p.cfg.Voice.ModelPath, Threads: p.cfg.Voice.Threads})
		if err != nil {
			p.log.Error("Local voice transcription configuration is unavailable")
			return
		}
		p.engine = engine
	}
	defer p.engine.Close()
	warmCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err := p.engine.Start(warmCtx)
	cancel()
	if err != nil && ctx.Err() == nil {
		p.log.Warn("Whisper warmup failed; the next voice note will retry")
	}
	idleDelay := time.Second
	for ctx.Err() == nil {
		changed := p.store.TelegramVoiceJobsChanged()
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		job, ok, err := p.store.ClaimTelegramVoice(claimCtx, 12*time.Minute)
		cancel()
		if err != nil && ctx.Err() == nil {
			p.log.Warn("Voice-note queue temporarily unavailable")
		}
		if ok && err == nil {
			p.process(ctx, job)
			idleDelay = time.Second
			continue
		}
		wait := idleDelay
		if err == nil {
			nextCtx, nextCancel := context.WithTimeout(ctx, 5*time.Second)
			next, pending, nextErr := p.store.NextTelegramVoiceAttempt(nextCtx)
			nextCancel()
			if nextErr == nil && pending {
				wait = max(100*time.Millisecond, time.Until(next))
			}
		}
		timer := time.NewTimer(min(wait, 15*time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changed:
			timer.Stop()
			idleDelay = time.Second
		case <-timer.C:
			idleDelay = min(15*time.Second, idleDelay*2)
		}
	}
}

func (p *TelegramVoiceProcessor) authorized(job registry.TelegramVoiceJob) bool {
	if job.BotID != p.cfg.Secrets.BotName || !auth.AuthorizedTelegramUser(job.UserID, p.cfg.AllowedUserIDs) {
		return false
	}
	if len(p.cfg.AllowedChatIDs) == 0 {
		return true
	}
	for _, id := range p.cfg.AllowedChatIDs {
		if id == job.ChatID {
			return true
		}
	}
	return false
}

func (p *TelegramVoiceProcessor) process(ctx context.Context, job registry.TelegramVoiceJob) {
	fail := func(code string, retry bool) {
		// Release a lease during graceful shutdown too. A hard crash is recovered
		// after lease expiry; neither path creates a prompt before DB completion.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := p.store.FailTelegramVoice(saveCtx, job.ID, job.LeaseID, code, retry); err != nil && !errors.Is(err, registry.ErrTelegramVoiceLease) {
			p.log.Warn("Voice-note result checkpoint deferred")
		}
	}
	if !p.authorized(job) {
		fail("voice_unauthorized", false)
		return
	}
	workCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	downloadCtx, downloadCancel := context.WithTimeout(workCtx, 30*time.Second)
	data, err := p.api.DownloadVoice(downloadCtx, job.FileID)
	downloadCancel()
	if err != nil {
		code := "voice_download_failed"
		var mediaErr telegramMediaError
		if errors.As(err, &mediaErr) && string(mediaErr) == "voice_too_large" {
			code = "voice_too_large"
		}
		fail(code, code == "voice_download_failed")
		return
	}
	decode := p.decode
	if decode == nil {
		decode = voice.DecodeOggOpus
	}
	pcm, err := decode(workCtx, bytes.NewReader(data))
	clear(data)
	if err != nil {
		code := "voice_decode_failed"
		switch {
		case errors.Is(err, voice.ErrAudioTooLarge):
			code = "voice_too_large"
		case errors.Is(err, voice.ErrAudioTooLong):
			code = "voice_too_long"
		case errors.Is(err, voice.ErrUnsupportedAudio):
			code = "voice_unsupported"
		}
		fail(code, ctx.Err() != nil)
		return
	}
	defer clear(pcm)
	result, err := p.engine.Transcribe(workCtx, pcm)
	if err != nil {
		// A crashed helper can be safely restarted: no command exists until
		// CompleteTelegramVoice commits. Do not repeat a timed-out inference.
		retry := ctx.Err() != nil || (workCtx.Err() == nil && !errors.Is(err, context.DeadlineExceeded))
		fail("voice_transcription_failed", retry)
		return
	}
	if !p.authorized(job) {
		fail("voice_unauthorized", false)
		return
	}
	completeCtx, completeCancel := context.WithTimeout(ctx, 5*time.Second)
	defer completeCancel()
	if _, err := p.store.CompleteTelegramVoice(completeCtx, job.ID, job.LeaseID, result.Text); err != nil && !errors.Is(err, registry.ErrTelegramVoiceLease) {
		p.log.Warn("Voice-note submission deferred")
		fail("voice_transcription_failed", true)
	}
}

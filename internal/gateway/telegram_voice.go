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

// VoiceConfiguration publishes committed settings. A processor applies changes
// between jobs so changing a model does not discard a recording being decoded.
type VoiceConfiguration interface {
	VoiceConfig() config.VoiceConfig
	Changed() <-chan struct{}
}

type audioInferenceLimiter interface {
	AcquireInference(context.Context) (func(), error)
}

// TelegramVoiceProcessor serializes CPU-heavy work. The helper stays resident
// between jobs; the wakeup channel and slow idle fallback avoid a polling loop.
type TelegramVoiceProcessor struct {
	store     TelegramVoiceStore
	api       TelegramVoiceAPI
	cfg       config.GatewayConfig
	log       *slog.Logger
	engine    voiceEngine
	decode    func(context.Context, io.Reader) ([]float32, error)
	settings  VoiceConfiguration
	newEngine func(config.VoiceConfig) (voiceEngine, error)
}

func NewTelegramVoiceProcessor(store TelegramVoiceStore, api TelegramVoiceAPI, cfg config.GatewayConfig, logger *slog.Logger, settings ...VoiceConfiguration) *TelegramVoiceProcessor {
	p := &TelegramVoiceProcessor{store: store, api: api, cfg: cfg, log: logger}
	if len(settings) > 0 {
		p.settings = settings[0]
	}
	return p
}

func (p *TelegramVoiceProcessor) Run(ctx context.Context) {
	defer func() {
		if p.engine != nil {
			_ = p.engine.Close()
		}
	}()
	initialized := false
	idleDelay := time.Second
	for ctx.Err() == nil {
		var settingsChanged <-chan struct{}
		selected := p.cfg.Voice
		if p.settings != nil {
			// Subscribe before reading to avoid missing a concurrent commit.
			settingsChanged = p.settings.Changed()
			selected = p.settings.VoiceConfig()
		}
		if !initialized || selected != p.cfg.Voice {
			if initialized && p.engine != nil {
				_ = p.engine.Close()
				p.engine = nil
			}
			p.cfg.Voice = selected
			initialized = true
			if !selected.Enabled {
				cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := p.store.CancelTelegramVoiceJobs(cleanupCtx, p.cfg.Secrets.BotName, "voice_cancelled"); err != nil && ctx.Err() == nil {
					p.log.Warn("Pending voice-note cancellation deferred")
				}
				cancel()
			} else {
				if p.engine == nil {
					var err error
					if p.newEngine != nil {
						p.engine, err = p.newEngine(selected)
					} else {
						p.engine, err = voice.New(voice.Config{BinaryPath: selected.HelperPath, ModelPath: selected.ModelPath, Threads: selected.Threads})
					}
					if err != nil {
						p.log.Error("Local voice transcription configuration is unavailable")
						return
					}
				}
				warmCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				err := p.engine.Start(warmCtx)
				cancel()
				if err != nil && ctx.Err() == nil {
					p.log.Warn("Whisper warmup failed; the next voice note will retry")
				}
			}
		}
		if !selected.Enabled {
			if p.settings == nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-settingsChanged:
				continue
			}
		}
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
		case <-settingsChanged:
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
	var release func()
	if limiter, ok := p.settings.(audioInferenceLimiter); ok {
		release, err = limiter.AcquireInference(workCtx)
		if err != nil {
			fail("voice_transcription_failed", ctx.Err() != nil)
			return
		}
	}
	result, err := p.engine.Transcribe(workCtx, pcm)
	if release != nil {
		release()
	}
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
	if store, ok := p.store.(interface {
		CompleteTelegramVoiceWithLanguage(context.Context, uuid.UUID, uuid.UUID, string, string) (registry.AcceptResult, error)
	}); ok {
		_, err = store.CompleteTelegramVoiceWithLanguage(completeCtx, job.ID, job.LeaseID, result.Text, result.Language)
	} else {
		_, err = p.store.CompleteTelegramVoice(completeCtx, job.ID, job.LeaseID, result.Text)
	}
	if err != nil && !errors.Is(err, registry.ErrTelegramVoiceLease) {
		p.log.Warn("Voice-note submission deferred")
		fail("voice_transcription_failed", true)
	}
}

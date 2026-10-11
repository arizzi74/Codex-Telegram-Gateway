package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/audio"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/voice"
)

type TelegramSpeechStore interface {
	TelegramVoiceJobsChanged() <-chan struct{}
	ClaimTelegramSpeech(context.Context, time.Duration) (registry.TelegramSpeechJob, bool, error)
	CheckTelegramSpeech(context.Context, uuid.UUID, uuid.UUID) (bool, error)
	CheckpointTelegramSpeech(context.Context, uuid.UUID, uuid.UUID, int, int64, bool) error
	FailTelegramSpeech(context.Context, uuid.UUID, uuid.UUID, string, bool, ...time.Duration) error
	CancelTelegramSpeechJobs(context.Context, string) error
	NextTelegramSpeechAttempt(context.Context) (time.Time, bool, error)
}

type TelegramSpeechAPI interface {
	SendVoice(context.Context, int64, int64, []byte, int) (int64, error)
}
type speechEngine interface {
	Synthesize(context.Context, string, string, int) ([]float32, error)
}
type speechConfiguration interface {
	Settings() audio.Settings
	Changed() <-chan struct{}
	AcquireInference(context.Context) (func(), error)
}

// TelegramSpeechProcessor is independent of the text sender: model downloads
// or synthesis never delay final text, question delivery, or Telegram webhooks.
type TelegramSpeechProcessor struct {
	store    TelegramSpeechStore
	api      TelegramSpeechAPI
	engine   speechEngine
	settings speechConfiguration
	cfg      config.GatewayConfig
	log      *slog.Logger
	redactor *auth.Redactor
	encode   func(context.Context, []float32, int) ([]byte, error)
}

func NewTelegramSpeechProcessor(store TelegramSpeechStore, api TelegramSpeechAPI, engine speechEngine, settings speechConfiguration, cfg config.GatewayConfig, logger *slog.Logger, redactor *auth.Redactor) *TelegramSpeechProcessor {
	return &TelegramSpeechProcessor{store: store, api: api, engine: engine, settings: settings, cfg: cfg, log: logger, redactor: redactor, encode: voice.EncodeOggOpus}
}

func (p *TelegramSpeechProcessor) Run(ctx context.Context) {
	idle := time.Second
	unloaded := false
	for ctx.Err() == nil {
		changed := p.store.TelegramVoiceJobsChanged()
		settingsChanged := p.settings.Changed()
		settings := p.settings.Settings()
		if !settings.Enabled || !settings.ReplyWithAudio {
			cancelCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := p.store.CancelTelegramSpeechJobs(cancelCtx, p.cfg.Secrets.BotName)
			cancel()
			if err != nil && ctx.Err() == nil {
				p.log.Warn("Audio reply cancellation deferred")
			}
			if !unloaded {
				if engine, ok := p.engine.(interface{ Unload() }); ok {
					engine.Unload()
				}
				unloaded = true
			}
			var retry <-chan time.Time
			var timer *time.Timer
			if err != nil {
				timer = time.NewTimer(idle)
				retry = timer.C
				idle = min(15*time.Second, idle*2)
			} else {
				idle = time.Second
			}
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case <-settingsChanged:
			case <-changed:
			case <-retry:
			}
			if timer != nil {
				timer.Stop()
			}
			continue
		}
		unloaded = false
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		job, ok, err := p.store.ClaimTelegramSpeech(claimCtx, 12*time.Minute)
		cancel()
		if err != nil && ctx.Err() == nil {
			p.log.Warn("Audio reply queue temporarily unavailable")
		}
		if err == nil && ok {
			p.process(ctx, job)
			idle = time.Second
			continue
		}
		wait := idle
		if err == nil {
			nextCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			next, pending, nextErr := p.store.NextTelegramSpeechAttempt(nextCtx)
			cancel()
			if nextErr == nil && pending {
				wait = max(100*time.Millisecond, time.Until(next))
			}
		}
		timer := time.NewTimer(min(wait, 15*time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-settingsChanged:
			timer.Stop()
			idle = time.Second
		case <-changed:
			timer.Stop()
			idle = time.Second
		case <-timer.C:
			idle = min(15*time.Second, idle*2)
		}
	}
}

func (p *TelegramSpeechProcessor) allowed(ctx context.Context, job registry.TelegramSpeechJob) (bool, error) {
	settings := p.settings.Settings()
	if !settings.Enabled || !settings.ReplyWithAudio || job.BotID != p.cfg.Secrets.BotName || !auth.AuthorizedTelegramUser(job.UserID, p.cfg.AllowedUserIDs) || !telegramChatAllowed(p.cfg.AllowedChatIDs, job.ChatID) {
		return false, nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return p.store.CheckTelegramSpeech(checkCtx, job.ID, job.LeaseID)
}

func (p *TelegramSpeechProcessor) process(ctx context.Context, job registry.TelegramSpeechJob) {
	fail := func(code string, retry bool, retryAfter ...time.Duration) {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := p.store.FailTelegramSpeech(saveCtx, job.ID, job.LeaseID, code, retry, retryAfter...); err != nil {
			p.log.Warn("Audio reply checkpoint deferred")
		}
	}
	parts := telegramSpeechParts(job.Text, p.redactor)
	if len(parts) == 0 || job.NextChunk < 0 || job.NextChunk >= len(parts) {
		fail("speech_invalid", false)
		return
	}
	for index := job.NextChunk; index < len(parts); index++ {
		allowed, err := p.allowed(ctx, job)
		if err != nil || !allowed {
			fail("speech_cancelled", err != nil)
			return
		}
		settings := p.settings.Settings()
		language := settings.TTSVoice
		if language == "" || language == "auto" {
			if job.Language == "it" || job.Language == "italian" {
				language = "italian"
			} else {
				language = "english"
			}
		}
		workCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		release, err := p.settings.AcquireInference(workCtx)
		if err != nil {
			cancel()
			fail("speech_synthesis_failed", ctx.Err() != nil)
			return
		}
		pcm, err := p.engine.Synthesize(workCtx, parts[index], language, settings.Threads)
		if err != nil {
			clear(pcm)
			release()
			retry := ctx.Err() != nil || workCtx.Err() == nil
			cancel()
			fail("speech_synthesis_failed", retry)
			return
		}
		duration := (len(pcm) + 23999) / 24000
		encoded, err := p.encode(workCtx, pcm, 24000)
		clear(pcm)
		release()
		cancel()
		if err != nil {
			clear(encoded)
			fail("speech_encoding_failed", ctx.Err() != nil)
			return
		}
		allowed, err = p.allowed(ctx, job)
		if err != nil || !allowed {
			clear(encoded)
			fail("speech_cancelled", err != nil)
			return
		}
		sendCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		messageID, err := p.api.SendVoice(sendCtx, job.ChatID, job.TopicID, encoded, duration)
		clear(encoded)
		cancel()
		if err != nil {
			// Telegram cannot deduplicate sends. Retry explicit rate limits;
			// ambiguous transport failures must not replay an accepted upload.
			var telegramErr *TelegramError
			retry := errors.As(err, &telegramErr) && telegramErr.Code == 429
			var delay time.Duration
			if retry {
				delay = telegramErr.RetryAfter
			}
			fail("speech_send_failed", retry, delay)
			return
		}
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = p.store.CheckpointTelegramSpeech(saveCtx, job.ID, job.LeaseID, index, messageID, index == len(parts)-1)
		cancel()
		if err != nil {
			p.log.Warn("Audio reply delivery checkpoint deferred")
			return
		}
	}
}

// Split the displayed Markdown text at natural boundaries without summarizing
// or omitting content. Parts have stable boundaries for durable checkpoints.
func telegramSpeechParts(markdown string, redactors ...*auth.Redactor) []string {
	var redactor *auth.Redactor
	if len(redactors) > 0 {
		redactor = redactors[0]
	}
	if redactor != nil {
		markdown = redactor.Redact(markdown)
	}
	text, _ := renderTelegramMarkdown(markdown)
	// Formatting can join text which was separated by Markdown delimiters.
	// Apply the text sender's redaction both before and after rendering.
	if redactor != nil {
		text = redactor.Redact(text)
	}
	runes := []rune(strings.TrimSpace(text))
	var parts []string
	for len(runes) > 0 {
		end := min(1000, len(runes))
		if end < len(runes) {
			for i := end - 1; i >= end/2; i-- {
				if runes[i] == '\n' || runes[i] == '.' || runes[i] == '!' || runes[i] == '?' {
					end = i + 1
					break
				}
			}
			if end == 1000 {
				for i := end - 1; i >= end/2; i-- {
					if unicode.IsSpace(runes[i]) {
						end = i + 1
						break
					}
				}
			}
		}
		if part := strings.TrimSpace(string(runes[:end])); part != "" {
			parts = append(parts, part)
		}
		runes = runes[end:]
	}
	return parts
}

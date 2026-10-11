package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/voice"
)

type processorStore struct {
	TelegramVoiceStore
	transcript, code      string
	retry                 bool
	completions, failures int
}

func (s *processorStore) CompleteTelegramVoice(_ context.Context, _, _ uuid.UUID, text string) (registry.AcceptResult, error) {
	s.transcript = text
	s.completions++
	return registry.AcceptResult{}, nil
}
func (s *processorStore) FailTelegramVoice(_ context.Context, _, _ uuid.UUID, code string, retry bool) error {
	s.code = code
	s.retry = retry
	s.failures++
	return nil
}

type processorAPI struct {
	calls int
	data  []byte
	err   error
}

func (a *processorAPI) DownloadVoice(context.Context, string) ([]byte, error) {
	a.calls++
	return a.data, a.err
}

type processorEngine struct {
	voiceEngine
	calls  int
	result voice.Result
	err    error
}

func (e *processorEngine) Transcribe(context.Context, []float32) (voice.Result, error) {
	e.calls++
	return e.result, e.err
}

func TestVoiceProcessorAuthorizationAndLiteralSubmission(t *testing.T) {
	for _, tc := range []struct {
		name       string
		user, chat int64
		bot        string
		allowed    bool
	}{
		{"allowed", 7, 8, "bot", true}, {"revoked user", 9, 8, "bot", false}, {"revoked chat", 7, 9, "bot", false}, {"different bot", 7, 8, "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &processorStore{}
			api := &processorAPI{data: []byte("encoded")}
			engine := &processorEngine{result: voice.Result{Text: "/tginterrupt literal spoken words"}}
			p := NewTelegramVoiceProcessor(store, api, config.GatewayConfig{AllowedUserIDs: []int64{7}, AllowedChatIDs: []int64{8}, Secrets: config.BotSecrets{BotName: "bot"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			p.engine = engine
			samples := []float32{1, 2}
			p.decode = func(context.Context, io.Reader) ([]float32, error) { return samples, nil }
			p.process(context.Background(), registry.TelegramVoiceJob{ID: uuid.New(), LeaseID: uuid.New(), BotID: tc.bot, UserID: tc.user, ChatID: tc.chat})
			if !tc.allowed {
				if api.calls != 0 || engine.calls != 0 || store.completions != 0 || store.code != "voice_unauthorized" {
					t.Fatalf("unauthorized processing: %+v %+v", api, store)
				}
				return
			}
			if api.calls != 1 || engine.calls != 1 || store.completions != 1 || store.transcript != "/tginterrupt literal spoken words" {
				t.Fatalf("wrong transcript processing: %+v", store)
			}
			for _, b := range api.data {
				if b != 0 {
					t.Fatal("encoded audio not cleared")
				}
			}
			for _, s := range samples {
				if s != 0 {
					t.Fatal("PCM not cleared")
				}
			}
		})
	}
}

func TestVoiceProcessorFailuresDoNotSubmit(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		download, decode, infer error
		want                    string
		retry                   bool
	}{
		{"download", errors.New("secret URL must not persist"), nil, nil, "voice_download_failed", true},
		{"oversize", telegramMediaError("voice_too_large"), nil, nil, "voice_too_large", false},
		{"format", nil, voice.ErrUnsupportedAudio, nil, "voice_unsupported", false},
		{"duration", nil, voice.ErrAudioTooLong, nil, "voice_too_long", false},
		{"helper", nil, nil, errors.New("stderr with private transcript"), "voice_transcription_failed", true},
		{"timeout", nil, nil, context.DeadlineExceeded, "voice_transcription_failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &processorStore{}
			api := &processorAPI{err: tc.download}
			engine := &processorEngine{err: tc.infer}
			p := NewTelegramVoiceProcessor(store, api, config.GatewayConfig{AllowedUserIDs: []int64{7}, Secrets: config.BotSecrets{BotName: "bot"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			p.engine = engine
			p.decode = func(context.Context, io.Reader) ([]float32, error) { return []float32{1}, tc.decode }
			p.process(context.Background(), registry.TelegramVoiceJob{ID: uuid.New(), LeaseID: uuid.New(), BotID: "bot", UserID: 7, LeaseUntil: time.Now().Add(time.Minute)})
			if store.completions != 0 || store.failures != 1 || store.code != tc.want || store.retry != tc.retry {
				t.Fatalf("unexpected completion: %+v", store)
			}
		})
	}
}

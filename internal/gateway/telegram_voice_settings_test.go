package gateway

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/voice"
)

type liveVoiceSettings struct {
	mu      sync.Mutex
	value   config.VoiceConfig
	changed chan struct{}
}

func (s *liveVoiceSettings) VoiceConfig() config.VoiceConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value
}
func (s *liveVoiceSettings) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}
func (s *liveVoiceSettings) set(v config.VoiceConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = v
	close(s.changed)
	s.changed = make(chan struct{})
}

type liveVoiceStore struct {
	TelegramVoiceStore
	jobs      chan registry.TelegramVoiceJob
	wake      chan struct{}
	cancelled chan struct{}
	completed chan struct{}
}

func (s *liveVoiceStore) TelegramVoiceJobsChanged() <-chan struct{} { return s.wake }
func (s *liveVoiceStore) NextTelegramVoiceAttempt(context.Context) (time.Time, bool, error) {
	return time.Time{}, false, nil
}
func (s *liveVoiceStore) ClaimTelegramVoice(context.Context, time.Duration) (registry.TelegramVoiceJob, bool, error) {
	select {
	case j := <-s.jobs:
		return j, true, nil
	default:
		return registry.TelegramVoiceJob{}, false, nil
	}
}
func (s *liveVoiceStore) CancelTelegramVoiceJobs(context.Context, string, string) error {
	s.cancelled <- struct{}{}
	return nil
}
func (s *liveVoiceStore) CompleteTelegramVoice(context.Context, uuid.UUID, uuid.UUID, string) (registry.AcceptResult, error) {
	s.completed <- struct{}{}
	return registry.AcceptResult{}, nil
}

type liveVoiceEngine struct {
	started, transcribing, release, closed chan struct{}
}

func (e *liveVoiceEngine) Start(context.Context) error { close(e.started); return nil }
func (e *liveVoiceEngine) Close() error                { close(e.closed); return nil }
func (e *liveVoiceEngine) Transcribe(ctx context.Context, _ []float32) (voice.Result, error) {
	close(e.transcribing)
	select {
	case <-ctx.Done():
		return voice.Result{}, ctx.Err()
	case <-e.release:
		return voice.Result{Text: "test"}, nil
	}
}
func testLiveEngine() *liveVoiceEngine {
	return &liveVoiceEngine{make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})}
}
func awaitVoiceSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("voice processor did not advance")
	}
}

func TestVoiceSettingsHotApplyBetweenRecordings(t *testing.T) {
	source := &liveVoiceSettings{changed: make(chan struct{})}
	store := &liveVoiceStore{jobs: make(chan registry.TelegramVoiceJob, 1), wake: make(chan struct{}, 1), cancelled: make(chan struct{}, 2), completed: make(chan struct{}, 1)}
	first, second := testLiveEngine(), testLiveEngine()
	p := NewTelegramVoiceProcessor(store, &processorAPI{data: []byte("audio")}, config.GatewayConfig{Secrets: config.BotSecrets{BotName: "bot"}, AllowedUserIDs: []int64{7}}, slog.New(slog.NewTextHandler(io.Discard, nil)), source)
	p.newEngine = func(v config.VoiceConfig) (voiceEngine, error) {
		if v.ModelPath == "base" {
			return first, nil
		}
		return second, nil
	}
	p.decode = func(context.Context, io.Reader) ([]float32, error) { return []float32{0}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	defer func() { cancel(); awaitVoiceSignal(t, done) }()
	awaitVoiceSignal(t, store.cancelled)
	source.set(config.VoiceConfig{Enabled: true, ModelPath: "base", Threads: 3})
	awaitVoiceSignal(t, first.started)
	store.jobs <- registry.TelegramVoiceJob{ID: uuid.New(), LeaseID: uuid.New(), BotID: "bot", UserID: 7}
	store.wake <- struct{}{}
	awaitVoiceSignal(t, first.transcribing)
	source.set(config.VoiceConfig{Enabled: true, ModelPath: "small", Threads: 3})
	select {
	case <-first.closed:
		t.Fatal("model change interrupted an active transcription")
	default:
	}
	close(first.release)
	awaitVoiceSignal(t, store.completed)
	awaitVoiceSignal(t, first.closed)
	awaitVoiceSignal(t, second.started)
	source.set(config.VoiceConfig{Enabled: false, ModelPath: "small", Threads: 3})
	awaitVoiceSignal(t, second.closed)
	awaitVoiceSignal(t, store.cancelled)
}

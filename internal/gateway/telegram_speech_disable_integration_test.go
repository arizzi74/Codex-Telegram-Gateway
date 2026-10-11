package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/audio"
	"github.com/iaia/telegramgw/internal/config"
)

type disablingSpeechSettings struct {
	mu      sync.RWMutex
	value   audio.Settings
	changed chan struct{}
	budget  chan struct{}
}

func newDisablingSpeechSettings() *disablingSpeechSettings {
	s := &disablingSpeechSettings{
		value:   audio.Settings{Enabled: true, ReplyWithAudio: true, TTSVoice: "english", Threads: 1},
		changed: make(chan struct{}),
		budget:  make(chan struct{}, 1),
	}
	s.budget <- struct{}{}
	return s
}

func (s *disablingSpeechSettings) Settings() audio.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

func (s *disablingSpeechSettings) Changed() <-chan struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.changed
}

func (s *disablingSpeechSettings) disable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value.Enabled, s.value.ReplyWithAudio = false, false
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *disablingSpeechSettings) AcquireInference(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.budget:
		var once sync.Once
		return func() { once.Do(func() { s.budget <- struct{}{} }) }, nil
	}
}

type cancelingSpeechEngine struct {
	started, canceled chan struct{}
}

func (e *cancelingSpeechEngine) Synthesize(ctx context.Context, _, _ string, _ int) ([]float32, error) {
	close(e.started)
	<-ctx.Done()
	close(e.canceled)
	return nil, ctx.Err()
}

// Disabling must stop inference while it is blocked, release the text gate,
// and return the shared CPU budget without waiting for the synthesis timeout.
func TestSpeechDisableInterruptsInferenceAndReleasesFinalDelivery(t *testing.T) {
	store, sender, api := speechDeliveryFixture(t, true, "This final text remains available when audio is disabled.")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	job, ok, err := store.ClaimTelegramSpeech(ctx, 12*time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim speech before text: %v %v", ok, err)
	}
	settings := newDisablingSpeechSettings()
	engine := &cancelingSpeechEngine{started: make(chan struct{}), canceled: make(chan struct{})}
	processor := NewTelegramSpeechProcessor(store, api, engine, settings, config.GatewayConfig{Secrets: config.BotSecrets{BotName: "bot"}, AllowedUserIDs: []int64{10}, AllowedChatIDs: []int64{20}}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	var encodes atomic.Int32
	processor.encode = func(context.Context, []float32, int) ([]byte, error) {
		encodes.Add(1)
		return nil, errors.New("canceled inference must not reach encoding")
	}
	done := make(chan struct{})
	go func() { defer close(done); processor.process(ctx, job) }()
	t.Cleanup(func() { cancel(); <-done })
	awaitSpeechDeliveryStage(t, engine.started, "blocked speech inference")
	budgetCtx, stopBudgetWait := context.WithTimeout(ctx, 20*time.Millisecond)
	release, budgetErr := settings.AcquireInference(budgetCtx)
	stopBudgetWait()
	if release != nil {
		release()
	}
	if !errors.Is(budgetErr, context.DeadlineExceeded) {
		t.Fatalf("speech did not own the shared CPU budget: %v", budgetErr)
	}
	if err := sender.flush(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoSpeechFinalSent(t, api)
	presence := NewPresence(store, api, nil, PresenceOptions{AllowedChatIDs: []int64{20}})
	now := time.Now()
	if err := presence.flush(ctx, now); err != nil {
		t.Fatal(err)
	}
	if events := api.snapshot(); len(events) != 1 || events[0].kind != "typing" {
		t.Fatalf("pending synthesis was not visible as typing: %+v", events)
	}
	settings.disable()
	awaitSpeechDeliveryStage(t, engine.canceled, "inference cancellation after disabling audio")
	awaitSpeechDeliveryStage(t, done, "disabled speech cleanup")
	if encodes.Load() != 0 {
		t.Fatal("disabled speech reached the encoder")
	}
	budgetCtx, stopBudgetWait = context.WithTimeout(ctx, time.Second)
	release, budgetErr = settings.AcquireInference(budgetCtx)
	stopBudgetWait()
	if budgetErr != nil {
		t.Fatalf("canceled speech retained the shared CPU budget: %v", budgetErr)
	}
	release()
	if targets, err := store.ListTelegramTypingTargets(ctx, 100); err != nil || len(targets) != 0 {
		t.Fatalf("disabled speech kept typing active: %+v %v", targets, err)
	}
	if err := presence.flush(ctx, now.Add(typingRefreshInterval)); err != nil {
		t.Fatal(err)
	}
	if err := sender.flush(ctx); err != nil {
		t.Fatal(err)
	}
	events := api.snapshot()
	if len(events) != 2 || events[0].kind != "typing" || events[1].kind != "text" || !strings.Contains(events[1].text, "audio is disabled") {
		t.Fatalf("disable did not release text without further typing or audio: %+v", events)
	}
	if _, ok, err := store.ClaimTelegramSpeech(ctx, time.Minute); err != nil || ok {
		t.Fatalf("disabled speech was left retryable: %v %v", ok, err)
	}
}

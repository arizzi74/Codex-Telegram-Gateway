package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/audio"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
)

type speechTestSettings struct {
	value              audio.Settings
	acquired, released int
}

func (s *speechTestSettings) Settings() audio.Settings { return s.value }
func (s *speechTestSettings) Changed() <-chan struct{} { return nil }
func (s *speechTestSettings) AcquireInference(context.Context) (func(), error) {
	s.acquired++
	return func() { s.released++ }, nil
}

type speechTestStore struct {
	TelegramSpeechStore
	checks   int
	revokeAt int
	parts    []int
	last     bool
	code     string
	retry    bool
	delay    time.Duration
}

func (s *speechTestStore) CheckTelegramSpeech(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	s.checks++
	return s.revokeAt == 0 || s.checks < s.revokeAt, nil
}
func (s *speechTestStore) CheckpointTelegramSpeech(_ context.Context, _, _ uuid.UUID, index int, _ int64, done bool) error {
	s.parts = append(s.parts, index)
	s.last = done
	return nil
}
func (s *speechTestStore) FailTelegramSpeech(_ context.Context, _, _ uuid.UUID, code string, retry bool, delay ...time.Duration) error {
	s.code, s.retry = code, retry
	if len(delay) > 0 {
		s.delay = delay[0]
	}
	return nil
}

type speechTestEngine struct {
	calls          int
	text, language string
	threads        int
	pcm            []float32
	err            error
	after          func()
}

func (e *speechTestEngine) Synthesize(_ context.Context, text, lang string, threads int) ([]float32, error) {
	e.calls++
	e.text, e.language, e.threads = text, lang, threads
	e.pcm = []float32{.1, .2}
	if e.after != nil {
		e.after()
	}
	return e.pcm, e.err
}

type speechTestAPI struct {
	calls       int
	chat, topic int64
	data        []byte
	err         error
}

func (a *speechTestAPI) SendVoice(_ context.Context, chat, topic int64, data []byte, _ int) (int64, error) {
	a.calls++
	a.chat, a.topic, a.data = chat, topic, data
	return 42, a.err
}
func newSpeechProcessorTest() (*TelegramSpeechProcessor, *speechTestStore, *speechTestEngine, *speechTestAPI, *speechTestSettings, registry.TelegramSpeechJob) {
	s := &speechTestStore{}
	e := &speechTestEngine{}
	api := &speechTestAPI{}
	settings := &speechTestSettings{value: audio.Settings{Enabled: true, ReplyWithAudio: true, TTSVoice: "auto", Threads: 3}}
	p := NewTelegramSpeechProcessor(s, api, e, settings, config.GatewayConfig{Secrets: config.BotSecrets{BotName: "bot"}, AllowedUserIDs: []int64{7}, AllowedChatIDs: []int64{8}}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	p.encode = func(context.Context, []float32, int) ([]byte, error) { return []byte("OggSsynthetic"), nil }
	job := registry.TelegramSpeechJob{ID: uuid.New(), LeaseID: uuid.New(), BotID: "bot", UserID: 7, ChatID: 8, TopicID: 9, Text: "**Completed.** All changes are saved.", Language: "it"}
	return p, s, e, api, settings, job
}

func TestSpeechReplyPreservesContentDestinationAndClearsAudio(t *testing.T) {
	p, s, e, api, settings, job := newSpeechProcessorTest()
	p.process(context.Background(), job)
	if e.calls != 1 || e.text != "Completed. All changes are saved." || e.language != "italian" || e.threads != 3 {
		t.Fatalf("wrong speech input: %+v", e)
	}
	if api.calls != 1 || api.chat != 8 || api.topic != 9 || !s.last || len(s.parts) != 1 || s.parts[0] != 0 || s.code != "" {
		t.Fatalf("wrong delivery: %+v %+v", api, s)
	}
	if settings.acquired != 1 || settings.released != 1 {
		t.Fatal("CPU budget not released")
	}
	for _, v := range e.pcm {
		if v != 0 {
			t.Fatal("generated PCM retained")
		}
	}
	for _, v := range api.data {
		if v != 0 {
			t.Fatal("generated recording retained")
		}
	}
}

func TestSpeechReplyRedactsRawAndRenderedSecrets(t *testing.T) {
	p, _, engine, api, _, job := newSpeechProcessorTest()
	var err error
	p.redactor, err = auth.NewRedactor([]string{"SECRET"}, "hidden")
	if err != nil {
		t.Fatal(err)
	}
	job.Text = "SECRET and SE**CR**ET are protected."
	p.process(t.Context(), job)
	if api.calls != 1 || engine.text != "hidden and hidden are protected." {
		t.Fatalf("speech did not match redacted text: %q", engine.text)
	}
}

func TestSpeechReplyAuthorizationAndDisable(t *testing.T) {
	for _, kind := range []string{"user", "chat", "bot", "disabled", "reply disabled", "revoked before", "revoked after", "disabled during"} {
		t.Run(kind, func(t *testing.T) {
			p, s, e, api, settings, job := newSpeechProcessorTest()
			switch kind {
			case "user":
				job.UserID = 99
			case "chat":
				job.ChatID = 99
			case "bot":
				job.BotID = "other"
			case "disabled":
				settings.value.Enabled = false
			case "reply disabled":
				settings.value.ReplyWithAudio = false
			case "revoked before":
				s.revokeAt = 1
			case "revoked after":
				s.revokeAt = 2
			case "disabled during":
				e.after = func() { settings.value.ReplyWithAudio = false }
			}
			p.process(context.Background(), job)
			if api.calls != 0 || s.code != "speech_cancelled" {
				t.Fatalf("unauthorized audio sent: %+v %+v", api, s)
			}
		})
	}
}

func TestSpeechReplyFailureAndResume(t *testing.T) {
	p, s, e, api, _, job := newSpeechProcessorTest()
	job.Text = strings.Repeat("This is a complete sentence with useful information. ", 50)
	job.NextChunk = 1
	p.process(context.Background(), job)
	if e.calls != len(telegramSpeechParts(job.Text))-1 || len(s.parts) == 0 || s.parts[0] != 1 || !s.last {
		t.Fatalf("wrong resumed chunks: %+v", s)
	}
	for _, tc := range []struct {
		err   error
		retry bool
		delay time.Duration
	}{{errors.New("connection lost"), false, 0}, {&TelegramError{Code: 429, RetryAfter: 23 * time.Second}, true, 23 * time.Second}} {
		p, s, e, api, _, job = newSpeechProcessorTest()
		api.err = tc.err
		p.process(context.Background(), job)
		if len(s.parts) != 0 || s.code != "speech_send_failed" || s.retry != tc.retry || s.delay != tc.delay {
			t.Fatalf("wrong failure handling: %+v", s)
		}
	}
}

func TestSpeechPartsKeepAllDisplayedContent(t *testing.T) {
	text := "# Risultato\n\n" + strings.Repeat("È pronto: **tutto** salvato. Leggi il [rapporto](https://example.com/report).\n", 100)
	plain, _ := renderTelegramMarkdown(text)
	parts := telegramSpeechParts(text)
	if len(parts) < 2 {
		t.Fatal("long reply not split")
	}
	for _, part := range parts {
		if len([]rune(part)) > 1000 {
			t.Fatal("oversized speech chunk")
		}
	}
	if strings.Join(strings.Fields(strings.Join(parts, " ")), " ") != strings.Join(strings.Fields(plain), " ") {
		t.Fatal("speech omitted or summarized displayed text")
	}
}

type speechIdleStore struct {
	TelegramSpeechStore
	changed chan struct{}
	calls   chan struct{}
	cleared bool
}

func (s *speechIdleStore) TelegramVoiceJobsChanged() <-chan struct{} { return s.changed }
func (s *speechIdleStore) CancelTelegramSpeechJobs(context.Context, string) error {
	s.calls <- struct{}{}
	if !s.cleared {
		s.cleared = true
		close(s.changed)
		s.changed = make(chan struct{})
	}
	return nil
}

func TestSpeechDisabledQueueSettlesAfterCancellationNotification(t *testing.T) {
	p, _, _, api, settings, _ := newSpeechProcessorTest()
	settings.value.ReplyWithAudio = false
	store := &speechIdleStore{changed: make(chan struct{}), calls: make(chan struct{}, 10)}
	p.store = store
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	defer func() { cancel(); <-done }()
	// The initial cancellation broadcasts once. Its follow-up is a no-op and
	// must settle into a blocking wait rather than spin on its own wakeups.
	for range 2 {
		select {
		case <-store.calls:
		case <-time.After(time.Second):
			t.Fatal("disabled queue did not process cancellation")
		}
	}
	select {
	case <-store.calls:
		t.Fatal("disabled queue is spinning")
	case <-time.After(100 * time.Millisecond):
	}
	if api.calls != 0 {
		t.Fatal("disabled queue sent audio")
	}
}

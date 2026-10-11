package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/audio"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/voice"
)

type speechDeliveryEvent struct {
	kind, text string
	action     ChatAction
}

// Every interaction is in memory; these tests never construct a Telegram client.
type speechDeliveryAPI struct {
	mu          sync.Mutex
	events      []speechDeliveryEvent
	textCount   int
	blockTextAt int
	textBlocked chan struct{}
	releaseText chan struct{}
	beforeVoice func(context.Context) error
}

func (a *speechDeliveryAPI) record(event speechDeliveryEvent) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
	return int64(len(a.events))
}

func (a *speechDeliveryAPI) snapshot() []speechDeliveryEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]speechDeliveryEvent(nil), a.events...)
}

func (a *speechDeliveryAPI) Send(ctx context.Context, message SendMessage) (int64, error) {
	a.mu.Lock()
	a.events = append(a.events, speechDeliveryEvent{kind: "text", text: message.Text})
	a.textCount++
	blocked := a.textCount == a.blockTextAt
	id := int64(len(a.events))
	a.mu.Unlock()
	if blocked {
		close(a.textBlocked)
		select {
		case <-a.releaseText:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return id, nil
}

func (*speechDeliveryAPI) Edit(context.Context, int64, int64, string, *TelegramKeyboard) error {
	return errors.New("unexpected message edit")
}

func (*speechDeliveryAPI) AnswerCallback(context.Context, string, string) error { return nil }

func (a *speechDeliveryAPI) SendChatAction(_ context.Context, action ChatAction) error {
	a.record(speechDeliveryEvent{kind: "typing", action: action})
	return nil
}

func (a *speechDeliveryAPI) SendVoice(ctx context.Context, _, _ int64, encoded []byte, _ int) (int64, error) {
	if a.beforeVoice != nil {
		if err := a.beforeVoice(ctx); err != nil {
			return 0, err
		}
	}
	pcm, err := voice.DecodeOggOpus(ctx, bytes.NewReader(encoded))
	if err != nil {
		return 0, err
	}
	if len(pcm) != 1600 {
		return 0, errors.New("unexpected generated voice duration")
	}
	return a.record(speechDeliveryEvent{kind: "voice"}), nil
}

type speechDeliveryEngine struct {
	started chan struct{}
	release chan struct{}
}

func (e *speechDeliveryEngine) Synthesize(ctx context.Context, _, _ string, _ int) ([]float32, error) {
	select {
	case e.started <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	pcm := make([]float32, 2400)
	for i := range pcm {
		pcm[i] = float32(.2 * math.Sin(2*math.Pi*440*float64(i)/24000))
	}
	return pcm, nil
}

type speechDeliverySettings struct{}

func (speechDeliverySettings) Settings() audio.Settings {
	return audio.Settings{Enabled: true, ReplyWithAudio: true, TTSVoice: "english", Threads: 1}
}
func (speechDeliverySettings) Changed() <-chan struct{} { return nil }
func (speechDeliverySettings) AcquireInference(context.Context) (func(), error) {
	return func() {}, nil
}

func speechDeliveryFixture(t *testing.T, optedIn bool, finalText string) (*registry.Store, *Sender, *speechDeliveryAPI) {
	t.Helper()
	ctx := t.Context()
	store := testRegistry(t)
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "speech-worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken("test-token")})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtime, session := uuid.New(), uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{ID: runtime, WorkerID: worker.ID, ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	var sequence uint64
	emit := func(kind string, payload any) {
		t.Helper()
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		sequence++
		if err := store.IngestEvent(ctx, worker.ID, connection, protocol.Event{ID: uuid.NewString(), Seq: sequence, WorkerID: worker.ID.String(), RuntimeID: runtime.String(), RuntimeGeneration: 1, SessionID: session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	emit("session_discovered", protocol.Session{ID: session.String(), WorkerID: worker.ID.String(), RuntimeID: runtime.String(), ThreadID: "speech-thread", Name: "Speech session", CWD: "/work", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()})
	selected, err := store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "bot", UpdateID: 1, UserID: 10, ChatID: 20, Action: "select", Target: session.String()})
	if err != nil || selected.ErrorCode != "" {
		t.Fatalf("select session: %+v %v", selected, err)
	}
	api := &speechDeliveryAPI{}
	sender := NewSender(store, api, slog.New(slog.NewTextHandler(io.Discard, nil)), SenderOptions{BotID: "bot", OwnerID: 10, AllowedChatIDs: []int64{20}})
	input := registry.IncomingUpdate{BotID: "bot", UpdateID: 2, UserID: 10, ChatID: 20, Text: "Explain this public test."}
	var accepted registry.AcceptResult
	if optedIn {
		if _, err := store.AcceptTelegramVoice(ctx, input, registry.TelegramVoice{FileID: "synthetic", ReplyWithAudio: true}); err != nil {
			t.Fatal(err)
		}
		job, ok, err := store.ClaimTelegramVoice(ctx, 12*time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim incoming voice: %v %v", ok, err)
		}
		accepted, err = store.CompleteTelegramVoiceWithLanguage(ctx, job.ID, job.LeaseID, input.Text, "en")
	} else {
		accepted, err = store.AcceptTelegram(ctx, input)
	}
	if err != nil || accepted.ErrorCode != "" || accepted.CommandID == "" {
		t.Fatalf("accept prompt: %+v %v", accepted, err)
	}
	// Drain selection/transcription acknowledgements before observing final output.
	for range 8 {
		if err := sender.flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	api.events, api.textCount = nil, 0
	emit("turn_started", protocol.Result{CommandID: accepted.CommandID, TurnID: "speech-turn"})
	emit("turn_completed", protocol.Result{TurnID: "speech-turn", Text: finalText})
	status, err := store.TelegramSessionStatus(ctx, session)
	sessions, snapshotErr := store.SessionSnapshot(ctx)
	if err != nil || snapshotErr != nil || status.QueuedCommands != 0 || len(sessions) != 1 || sessions[0].State != "idle" {
		t.Fatalf("fixture still has running Codex work: status=%+v sessions=%+v errors=%v/%v", status, sessions, err, snapshotErr)
	}
	return store, sender, api
}

func awaitSpeechDeliveryStage(t *testing.T, stage <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-stage:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func assertNoSpeechFinalSent(t *testing.T, api *speechDeliveryAPI) {
	t.Helper()
	for _, event := range api.snapshot() {
		if event.kind == "text" || event.kind == "voice" {
			t.Fatalf("final output escaped while audio was preparing: %+v", event)
		}
	}
}

func TestSpeechDeliveryPreparesAllAudioKeepsTypingAndSendsAllTextFirst(t *testing.T) {
	text := strings.Repeat("This public test sentence keeps every final detail intact. ", 110) + "Final delivery marker."
	store, sender, api := speechDeliveryFixture(t, true, text)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := sender.flush(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoSpeechFinalSent(t, api)
	job, ok, err := store.ClaimTelegramSpeech(ctx, 12*time.Minute)
	if err != nil || !ok {
		t.Fatalf("audio could not start before text delivery: %v %v", ok, err)
	}
	api.beforeVoice = func(ctx context.Context) error {
		sent, err := store.TelegramSpeechTextSent(ctx, job.ID, job.LeaseID)
		if err != nil {
			return err
		}
		if !sent {
			return errors.New("voice upload preceded the last durable text checkpoint")
		}
		return nil
	}
	parts := telegramSpeechParts(job.Text)
	if len(parts) < 3 {
		t.Fatal("fixture must exercise several audio chunks")
	}
	engine := &speechDeliveryEngine{started: make(chan struct{}), release: make(chan struct{})}
	processor := NewTelegramSpeechProcessor(store, api, engine, speechDeliverySettings{}, config.GatewayConfig{Secrets: config.BotSecrets{BotName: "bot"}, AllowedUserIDs: []int64{10}, AllowedChatIDs: []int64{20}}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	lastEncoding, finishEncoding := make(chan struct{}), make(chan struct{})
	encoded := 0
	processor.encode = func(ctx context.Context, pcm []float32, rate int) ([]byte, error) {
		result, err := voice.EncodeOggOpus(ctx, pcm, rate)
		if err != nil {
			return nil, err
		}
		encoded++
		if encoded == len(parts) {
			close(lastEncoding)
			select {
			case <-finishEncoding:
			case <-ctx.Done():
				clear(result)
				return nil, ctx.Err()
			}
		}
		api.record(speechDeliveryEvent{kind: "encoded"})
		return result, nil
	}
	done := make(chan struct{})
	go func() { defer close(done); processor.process(ctx, job) }()
	t.Cleanup(func() { cancel(); <-done })
	presence := NewPresence(store, api, nil, PresenceOptions{AllowedChatIDs: []int64{20}})
	now := time.Now()
	for index := range parts {
		awaitSpeechDeliveryStage(t, engine.started, "audio synthesis")
		if err := sender.flush(ctx); err != nil {
			t.Fatal(err)
		}
		assertNoSpeechFinalSent(t, api)
		if err := presence.flush(ctx, now.Add(time.Duration(index)*typingRefreshInterval)); err != nil {
			t.Fatal(err)
		}
		engine.release <- struct{}{}
	}
	awaitSpeechDeliveryStage(t, lastEncoding, "last audio encoding")
	if err := sender.flush(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoSpeechFinalSent(t, api)
	typing := 0
	for _, event := range api.snapshot() {
		if event.kind == "typing" {
			typing++
			if event.action != (ChatAction{ChatID: 20, Action: "typing"}) {
				t.Fatalf("typing reached the wrong destination: %+v", event.action)
			}
		}
	}
	if typing != len(parts) {
		t.Fatalf("typing stopped after turn_completed: got %d refreshes, want %d", typing, len(parts))
	}
	prepared := store.TelegramVoiceJobsChanged()
	close(finishEncoding)
	awaitSpeechDeliveryStage(t, prepared, "audio readiness")
	api.blockTextAt, api.textBlocked, api.releaseText = 2, make(chan struct{}), make(chan struct{})
	textDone := make(chan error, 1)
	go func() { textDone <- sender.flush(ctx) }()
	awaitSpeechDeliveryStage(t, api.textBlocked, "last text chunk")
	sent, err := store.TelegramSpeechTextSent(ctx, job.ID, job.LeaseID)
	if err != nil || sent {
		t.Fatalf("unfinished text was treated as sent: %v %v", sent, err)
	}
	for _, event := range api.snapshot() {
		if event.kind == "voice" {
			t.Fatal("audio preceded the final text checkpoint")
		}
	}
	close(api.releaseText)
	if err := <-textDone; err != nil {
		t.Fatal(err)
	}
	awaitSpeechDeliveryStage(t, done, "all audio uploads")
	encodedCount, textCount, voiceCount := 0, 0, 0
	var deliveredText strings.Builder
	for _, event := range api.snapshot() {
		switch event.kind {
		case "encoded":
			encodedCount++
		case "text":
			if encodedCount != len(parts) || voiceCount != 0 {
				t.Fatal("text was not between complete encoding and the first audio upload")
			}
			textCount++
			deliveredText.WriteString(event.text)
		case "voice":
			if textCount != 2 {
				t.Fatalf("voice preceded every text chunk: %d sent", textCount)
			}
			voiceCount++
		}
	}
	if textCount != 2 || voiceCount != len(parts) || !strings.Contains(deliveredText.String(), "Final delivery marker.") {
		t.Fatalf("final delivery was incomplete: text=%d audio=%d want_audio=%d", textCount, voiceCount, len(parts))
	}
	if targets, err := store.ListTelegramTypingTargets(ctx, 100); err != nil || len(targets) != 0 {
		t.Fatalf("completed speech kept typing active: %+v %v", targets, err)
	}
}

func TestSpeechDeliveryLeavesPlainTextRepliesIndependent(t *testing.T) {
	store, sender, api := speechDeliveryFixture(t, false, "This ordinary text answer is immediately available.")
	if _, ok, err := store.ClaimTelegramSpeech(t.Context(), time.Minute); err != nil || ok {
		t.Fatalf("plain text created a speech job: %v %v", ok, err)
	}
	if err := sender.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	events := api.snapshot()
	if len(events) != 1 || events[0].kind != "text" || !strings.Contains(events[0].text, "immediately available") {
		t.Fatalf("plain text was held for speech: %+v", events)
	}
	if targets, err := store.ListTelegramTypingTargets(t.Context(), 100); err != nil || len(targets) != 0 {
		t.Fatalf("completed plain text unexpectedly kept typing active: %+v %v", targets, err)
	}
}

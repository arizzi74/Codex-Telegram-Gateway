package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/audio"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/voice"
)

type routingBot struct {
	cfg config.GatewayConfig
	api *TelegramClient
}

type routingResolver struct {
	mu   sync.RWMutex
	bots map[string]routingBot
}

func (r *routingResolver) ResolveTelegramBot(id string) (config.GatewayConfig, *TelegramClient, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	bot, ok := r.bots[id]
	return bot.cfg, bot.api, ok
}
func (r *routingResolver) TelegramBotIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var ids []string
	for id := range r.bots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func (r *routingResolver) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.bots, id)
}

type routingRequest struct {
	token, method string
	chat, message int64
	keyboard      *TelegramKeyboard
}
type routingFixture struct {
	mu       sync.Mutex
	requests []routingRequest
}

func (f *routingFixture) count(token, method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, request := range f.requests {
		if request.token == token && request.method == method {
			count++
		}
	}
	return count
}

func newRoutingFixture(t *testing.T) (*routingResolver, *routingFixture) {
	t.Helper()
	fixture := &routingFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if path[0] == "file" {
			_, _ = io.WriteString(w, strings.TrimPrefix(path[1], "bot"))
			return
		}
		request := routingRequest{token: strings.TrimPrefix(path[0], "bot"), method: path[1]}
		var payload struct {
			ChatID    int64             `json:"chat_id"`
			MessageID int64             `json:"message_id"`
			Keyboard  *TelegramKeyboard `json:"reply_markup"`
		}
		if request.method != "sendVoice" {
			_ = json.NewDecoder(r.Body).Decode(&payload)
			request.chat, request.message = payload.ChatID, payload.MessageID
			request.keyboard = payload.Keyboard
		}
		fixture.mu.Lock()
		fixture.requests = append(fixture.requests, request)
		fixture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch request.method {
		case "getMe":
			_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"id":1,"is_bot":true,"username":%q}}`, request.token+"_bot")
		case "getFile":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"file_path":"voice/shared.ogg"}}`)
		case "sendMessage", "sendVoice":
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":88}}`)
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(server.Close)
	resolver := &routingResolver{bots: make(map[string]routingBot)}
	for index, token := range []string{"first", "second"} {
		id := fmt.Sprintf("telegram:%d", index+1)
		api := NewTelegramClient(token)
		api.endpoint, api.http = server.URL, server.Client()
		resolver.bots[id] = routingBot{cfg: config.GatewayConfig{Secrets: config.BotSecrets{BotName: id, BotUsername: token + "_bot", WLID: 7}, AllowedUserIDs: []int64{7}, AllowedChatIDs: []int64{20}}, api: api}
	}
	return resolver, fixture
}

func TestResolvedSenderCreatesCallbackKeyboardForOnlyTheDeliveryBot(t *testing.T) {
	resolver, fixture := newRoutingFixture(t)
	store := testRegistry(t)
	ctx := t.Context()
	worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "routing-worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken("test-token")})
	if err != nil {
		t.Fatal(err)
	}
	connection, runtime := uuid.New(), uuid.New()
	if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{ID: runtime, WorkerID: worker.ID, ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	if result, err := store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "telegram:2", UpdateID: 1, UserID: 7, ChatID: 20, Action: "instances"}); err != nil || result.View != "instances" {
		t.Fatalf("queue bot B menu: %+v %v", result, err)
	}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "telegram:1", OwnerID: 99, AllowedChatIDs: []int64{999}, BotResolver: resolver})
	if err := sender.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if fixture.count("second", "sendMessage") != 1 || fixture.count("first", "sendMessage") != 0 || len(fixture.requests) != 1 {
		t.Fatalf("menu routed to the wrong bot: %+v", fixture.requests)
	}
	keyboard := fixture.requests[0].keyboard
	if keyboard == nil || len(keyboard.Rows) != 1 || len(keyboard.Rows[0]) != 1 {
		t.Fatalf("callback keyboard missing: %+v", keyboard)
	}
	token := strings.TrimPrefix(keyboard.Rows[0][0].Data, "cb:")
	input := registry.IncomingUpdate{BotID: "telegram:1", UpdateID: 2, UserID: 7, ChatID: 20, CallbackToken: token}
	if result, err := store.AcceptTelegram(ctx, input); err != nil || result.ErrorCode != "callback_invalid" {
		t.Fatalf("bot A consumed bot B callback: %+v %v", result, err)
	}
	input.BotID = "telegram:2"
	if result, err := store.AcceptTelegram(ctx, input); err != nil || result.ErrorCode != "" || result.View != "sessions" || result.RuntimeID != runtime.String() {
		t.Fatalf("bot B could not consume its callback: %+v %v", result, err)
	}
	if sender.options.BotID != "telegram:1" || sender.options.OwnerID != 99 {
		t.Fatal("delivery changed the shared sender identity")
	}
}

func TestSenderRoutesIdenticalMessageIdentitiesThroughTheirOwnBots(t *testing.T) {
	for kind, method := range map[string]string{"ui_response": "sendMessage", "agent_progress_message": "editMessageText", "question_answered": "editMessageText", "picker_cleanup": "deleteMessage", "input_reply_cleanup": "deleteMessage"} {
		t.Run(kind, func(t *testing.T) {
			resolver, fixture := newRoutingFixture(t)
			fallback := &chatAuthorizationAPI{}
			for _, id := range []string{"telegram:1", "telegram:2"} {
				row := registry.Delivery{ID: id, BotID: id, ChatID: 20, Kind: kind, Payload: json.RawMessage(`{"message_id":88}`)}
				store := &chatAuthorizationStore{checkpointStore: &checkpointStore{row: row, chunks: []registry.DeliveryChunk{{Payload: json.RawMessage(`{"chat_id":20,"text":"private message"}`)}}}, target: 88}
				if err := NewSender(store, fallback, nil, SenderOptions{BotResolver: resolver}).flush(t.Context()); err != nil {
					t.Fatal(err)
				}
				if len(store.marked) != 1 || store.skipped != 0 {
					t.Fatalf("delivery was not checkpointed: %+v", store)
				}
			}
			if fixture.count("first", method) != 1 || fixture.count("second", method) != 1 || len(fallback.messages)+len(fallback.edits)+len(fallback.deleted) != 0 {
				t.Fatalf("cross-bot operation: requests=%+v fallback=%+v", fixture.requests, fallback)
			}
			for _, request := range fixture.requests {
				if request.chat != 20 || (method != "sendMessage" && request.message != 88) {
					t.Fatalf("wrong destination: %+v", request)
				}
			}
		})
	}
}

func TestSenderNeverFallsBackForDisabledUnknownOrRevokedBots(t *testing.T) {
	for _, id := range []string{"telegram:1", "unknown", ""} {
		t.Run(id, func(t *testing.T) {
			resolver, fixture := newRoutingFixture(t)
			resolver.remove("telegram:1")
			row := registry.Delivery{ID: "delivery", BotID: id, ChatID: 20, Kind: "ui_response"}
			store := &chatAuthorizationStore{checkpointStore: &checkpointStore{row: row}}
			fallback := &chatAuthorizationAPI{}
			if err := NewSender(store, fallback, nil, SenderOptions{BotID: "telegram:2", BotResolver: resolver}).flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if store.skipped != 1 || len(fixture.requests) != 0 || len(fallback.messages) != 0 {
				t.Fatalf("unresolved bot was used: skipped=%d requests=%+v", store.skipped, fixture.requests)
			}
		})
	}
	resolver, fixture := newRoutingFixture(t)
	row := registry.Delivery{ID: "delivery", BotID: "telegram:1", ChatID: 20, Kind: "agent_progress_message"}
	store := &chatAuthorizationStore{checkpointStore: &checkpointStore{row: row, chunks: []registry.DeliveryChunk{{Payload: json.RawMessage(`{"chat_id":20,"text":"private progress"}`)}}}, target: 88, beforeTarget: func() { resolver.remove("telegram:1") }}
	if err := NewSender(store, nil, nil, SenderOptions{BotResolver: resolver}).flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.skipped != 1 || len(fixture.requests) != 0 {
		t.Fatal("bot revocation during a registry read did not stop the edit")
	}
}

type routingDeletionStore struct {
	TelegramProgressStore
	rows []registry.TelegramDeletion
	done []string
}

func (s *routingDeletionStore) ClaimTelegramDeletions(context.Context, int) ([]registry.TelegramDeletion, error) {
	return s.rows, nil
}
func (s *routingDeletionStore) MarkTelegramDeletionDone(_ context.Context, id string) error {
	s.done = append(s.done, id)
	return nil
}

func TestDeletionAndPresenceUseBotIdentityWithIdenticalChats(t *testing.T) {
	resolver, fixture := newRoutingFixture(t)
	store := &routingDeletionStore{}
	for _, id := range []string{"telegram:1", "telegram:2", "unknown"} {
		store.rows = append(store.rows, registry.TelegramDeletion{ID: id, BotID: id, ChatID: 20, MessageID: 88})
	}
	fallback := &chatAuthorizationAPI{}
	if err := NewSender(nil, fallback, nil, SenderOptions{BotResolver: resolver}).flushDeletions(t.Context(), store, fallback); err != nil {
		t.Fatal(err)
	}
	if len(store.done) != 3 || fixture.count("first", "deleteMessage") != 1 || fixture.count("second", "deleteMessage") != 1 || len(fallback.deleted) != 0 {
		t.Fatalf("wrong deletion routes: %+v", fixture.requests)
	}
	presenceStore := &presenceStore{targets: []registry.TelegramTypingTarget{{BotID: "telegram:1", ChatID: 20}, {BotID: "telegram:2", ChatID: 20}, {BotID: "unknown", ChatID: 20}}}
	presenceFallback := &presenceAPI{}
	if err := NewPresence(presenceStore, presenceFallback, nil, PresenceOptions{BotResolver: resolver}).flush(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if fixture.count("first", "sendChatAction") != 1 || fixture.count("second", "sendChatAction") != 1 || presenceFallback.count() != 0 {
		t.Fatalf("wrong presence routes: %+v", fixture.requests)
	}
}

func TestSharedVoiceEngineDownloadsEachBotsOwnFileAndRejectsRetiredBots(t *testing.T) {
	resolver, fixture := newRoutingFixture(t)
	store := &processorStore{}
	engine := &processorEngine{result: voice.Result{Text: "transcript"}}
	fallback := &processorAPI{}
	processor := NewTelegramVoiceProcessor(store, fallback, config.GatewayConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil))).WithBotResolver(resolver)
	processor.engine = engine
	decoded := 0
	processor.decode = func(_ context.Context, reader io.Reader) ([]float32, error) {
		encoded, err := io.ReadAll(reader)
		want := []string{"first", "second"}[decoded]
		if string(encoded) != want {
			t.Errorf("decoded %q, want recording from %s", encoded, want)
		}
		decoded++
		return []float32{1}, err
	}
	for _, id := range []string{"telegram:1", "telegram:2", "unknown"} {
		processor.process(t.Context(), registry.TelegramVoiceJob{ID: uuid.New(), LeaseID: uuid.New(), BotID: id, UserID: 7, ChatID: 20, TelegramVoice: registry.TelegramVoice{FileID: "same-file-id"}})
	}
	if engine.calls != 2 || store.completions != 2 || store.failures != 1 || fallback.calls != 0 || fixture.count("first", "getFile") != 1 || fixture.count("second", "getFile") != 1 {
		t.Fatalf("voice processing crossed bots: engine=%d store=%+v requests=%+v", engine.calls, store, fixture.requests)
	}
	processor.decode = func(context.Context, io.Reader) ([]float32, error) {
		resolver.remove("telegram:1")
		return []float32{1}, nil
	}
	processor.process(t.Context(), registry.TelegramVoiceJob{ID: uuid.New(), LeaseID: uuid.New(), BotID: "telegram:1", UserID: 7, ChatID: 20, TelegramVoice: registry.TelegramVoice{FileID: "same-file-id"}})
	if engine.calls != 2 || store.completions != 2 || store.failures != 2 {
		t.Fatal("retired bot was transcribed or submitted after download")
	}
}

func TestSharedSpeechEngineSendsEachReplyThroughItsOwnBot(t *testing.T) {
	resolver, fixture := newRoutingFixture(t)
	store := &speechTestStore{}
	engine := &speechTestEngine{}
	settings := &speechTestSettings{value: audio.Settings{Enabled: true, ReplyWithAudio: true}}
	processor := NewTelegramSpeechProcessor(store, nil, engine, settings, config.GatewayConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil).WithBotResolver(resolver)
	processor.encode = func(context.Context, []float32, int) ([]byte, error) { return []byte("OggS"), nil }
	for _, id := range []string{"telegram:1", "telegram:2", "unknown"} {
		processor.process(t.Context(), registry.TelegramSpeechJob{ID: uuid.New(), LeaseID: uuid.New(), BotID: id, UserID: 7, ChatID: 20, Text: "Reply text"})
	}
	if engine.calls != 2 || fixture.count("first", "sendVoice") != 1 || fixture.count("second", "sendVoice") != 1 || store.code != "speech_cancelled" {
		t.Fatalf("wrong speech routes: engine=%d requests=%+v code=%s", engine.calls, fixture.requests, store.code)
	}
	store.prepare = func() { resolver.remove("telegram:1") }
	processor.process(t.Context(), registry.TelegramSpeechJob{ID: uuid.New(), LeaseID: uuid.New(), BotID: "telegram:1", UserID: 7, ChatID: 20, Text: "Retired reply"})
	if fixture.count("first", "sendVoice") != 1 {
		t.Fatal("retired bot sent prepared speech")
	}
}

func TestResolvedSessionMenusKeepIndependentBotCachesAndContinueAfterIdentityFailure(t *testing.T) {
	resolver, fixture := newRoutingFixture(t)
	store := &sessionMenuFixture{contexts: []registry.TelegramMenuContext{{UserID: 7, ChatID: 20}}, aliases: []registry.TelegramSessionAlias{{Alias: "_project", Name: "Project"}}}
	menus := NewSessionMenus(store, nil, nil, SessionMenuOptions{BotResolver: resolver})
	for range 2 {
		if err := menus.flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, token := range []string{"first", "second"} {
		if fixture.count(token, "setMyCommands") != 2 { // One global menu and one chat scope.
			t.Fatalf("wrong/repeated menu writes for %s: %+v", token, fixture.requests)
		}
	}
	resolver.mu.Lock()
	bot := resolver.bots["telegram:1"]
	bot.cfg.Secrets.BotUsername = "wrong_bot"
	resolver.bots["telegram:1"] = bot
	resolver.mu.Unlock()
	store.aliases[0].Name = "Renamed"
	if err := menus.flush(t.Context()); err == nil {
		t.Fatal("identity mismatch was accepted")
	}
	if fixture.count("first", "setMyCommands") != 2 || fixture.count("second", "setMyCommands") != 3 {
		t.Fatalf("one bot identity failure blocked another bot: %+v", fixture.requests)
	}
}

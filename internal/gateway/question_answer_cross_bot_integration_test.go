package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

// Use actual bot resolution, rendering, HTTP clients and durable checkpoints.
// Each bot deliberately returns the same message IDs in the same chat.
type crossBotQuestions struct {
	t                           *testing.T
	store                       *registry.Store
	worker, connection, runtime uuid.UUID
	first, second               uuid.UUID
	sequence                    uint64
	sender                      *Sender
	mu                          sync.Mutex
	apis                        map[string]*questionSyncAPI
}

func newCrossBotQuestions(t *testing.T, ownerPrivate ...bool) *crossBotQuestions {
	t.Helper()
	ctx := t.Context()
	env := &crossBotQuestions{t: t, store: testRegistry(t), connection: uuid.New(), runtime: uuid.New(), first: uuid.New(), second: uuid.New(), apis: make(map[string]*questionSyncAPI)}
	worker, err := env.store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "shared-question-worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken("test-token")})
	if err != nil {
		t.Fatal(err)
	}
	env.worker = worker.ID
	if err := env.store.BindConnection(ctx, env.worker, env.connection); err != nil {
		t.Fatal(err)
	}
	if err := env.store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: env.worker, ConnectionID: env.connection, Runtimes: []registry.Runtime{{ID: env.runtime, WorkerID: env.worker, ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		path := strings.Split(strings.TrimPrefix(request.URL.Path, "/bot"), "/")
		var payload struct {
			SendMessage
			MessageID int64 `json:"message_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		env.mu.Lock()
		defer env.mu.Unlock()
		api := env.apis[path[0]]
		if api == nil {
			t.Errorf("unknown bot %q", path[0])
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch path[1] {
		case "sendMessage":
			id, _ := api.Send(request.Context(), payload.SendMessage)
			_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, id)
		case "editMessageText":
			_ = api.EditFormatted(request.Context(), payload.MessageID, payload.SendMessage)
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case "deleteMessage":
			_ = api.DeleteMessage(request.Context(), payload.ChatID, payload.MessageID)
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Errorf("unexpected Telegram method %q", path[1])
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	resolver := &routingResolver{bots: make(map[string]routingBot)}
	privateConnections := len(ownerPrivate) > 0 && ownerPrivate[0]
	for index, bot := range []string{"first", "second"} {
		env.apis[bot] = &questionSyncAPI{live: make(map[int64]SendMessage)}
		now := time.Now().UTC()
		if err := env.store.SaveTelegramBotRecord(ctx, registry.TelegramBotRecord{ID: bot, TelegramID: int64(index + 1), OwnerID: 10, Username: bot + "_bot", Enabled: true, EncryptedCredentials: []byte("test-only"), CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		api := NewTelegramClient(bot)
		api.endpoint, api.http = server.URL, server.Client()
		allowedChats := []int64{20}
		if privateConnections {
			allowedChats = []int64{10, 20}
		}
		resolver.bots[bot] = routingBot{cfg: config.GatewayConfig{Secrets: config.BotSecrets{WLID: 10}, AllowedChatIDs: allowedChats}, api: api}
	}
	env.sender = NewSender(env.store, nil, nil, SenderOptions{BotResolver: resolver})
	for _, session := range []uuid.UUID{env.first, env.second} {
		env.emit(session, "session_discovered", protocol.Session{ID: session.String(), WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), ThreadID: session.String(), Name: "Question session", CWD: "/work", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()})
	}
	for index, bot := range []string{"first", "second"} {
		selected := []uuid.UUID{env.first, env.second}[index]
		result, err := env.store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: bot, UpdateID: 1, UserID: 10, ChatID: 20, Action: "select", Target: selected.String()})
		if err != nil || result.ErrorCode != "" {
			t.Fatalf("select %s: %+v %v", bot, result, err)
		}
		if privateConnections {
			if _, err := env.store.SetWebUITelegramBot(ctx, selected, bot); err != nil {
				t.Fatalf("connect owner-private %s: %v", bot, err)
			}
		}
	}
	env.flush()
	return env
}

type deliveredQuestionCopy struct {
	bot     string
	id      int64
	message SendMessage
}

func (e *crossBotQuestions) questionCopies(prompt string) []deliveredQuestionCopy {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	var copies []deliveredQuestionCopy
	for bot, api := range e.apis {
		for id, message := range api.live {
			if strings.Contains(message.Text, prompt) {
				copies = append(copies, deliveredQuestionCopy{bot: bot, id: id, message: message})
			}
		}
	}
	return copies
}

func TestConnectedQuestionBotControlsRenderedDeliveryAndAllCopiesSettle(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		connected, sourceFlag, recipientFlag bool
		want                                 []string
	}{
		{name: "connected default off", connected: true, want: []string{"first/10", "first/20"}},
		{name: "recipient opt in cannot broaden", connected: true, recipientFlag: true, want: []string{"first/10", "first/20"}},
		{name: "source opt in broadcasts to recipient default off", connected: true, sourceFlag: true, want: []string{"first/10", "first/20", "second/10", "second/20"}},
		{name: "unconnected session broadcasts by default", want: []string{"first/20", "second/20"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newCrossBotQuestions(t, tc.connected)
			ctx := t.Context()
			for bot, enabled := range map[string]bool{"first": tc.sourceFlag, "second": tc.recipientFlag} {
				if enabled {
					if err := env.store.SetTelegramBotBroadcastQuestions(ctx, bot, true); err != nil {
						t.Fatal(err)
					}
				}
			}
			const prompt = "Continue with the selected option?"
			approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:connected", ThreadID: env.first.String(), Type: "user_input", Async: true,
				Questions: []protocol.Question{{ID: "question", Prompt: prompt, Options: []string{"Yes", "No"}}}}
			env.emit(env.first, "user_input_requested", approval)
			env.flush()
			copies := env.questionCopies(prompt)
			got := []string{}
			var answer deliveredQuestionCopy
			for _, copy := range copies {
				got = append(got, fmt.Sprintf("%s/%d", copy.bot, copy.message.ChatID))
				if copy.message.ChatID == 20 && (answer.bot == "" || copy.bot == "second") {
					answer = copy
				}
			}
			slices.Sort(got)
			slices.Sort(tc.want)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("rendered question destinations=%v, want %v", got, tc.want)
			}
			// Copies delivered while broadcast was enabled must settle even
			// after it is disabled, and remain answerable from the other bot.
			if tc.sourceFlag {
				if err := env.store.SetTelegramBotBroadcastQuestions(ctx, "first", false); err != nil {
					t.Fatal(err)
				}
			}
			accepted, err := env.store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: answer.bot, UpdateID: 2, UserID: 10, ChatID: answer.message.ChatID,
				CallbackToken: questionButton(t, answer.message, "Yes"), CallbackMessageID: answer.id})
			if err != nil || accepted.CommandID == "" || accepted.SessionID != env.first.String() {
				t.Fatalf("answer delivered copy: %+v %v", accepted, err)
			}
			env.flush()
			for _, copy := range copies {
				env.assertAnswer(copy.bot, copy.id, prompt, "Yes")
			}
			commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
			if err != nil || len(commands) != 1 || commands[0].Operation != protocol.InputResponse {
				t.Fatalf("expected one accepted response: %+v %v", commands, err)
			}
			env.assertSelections()
		})
	}
}

func TestConnectedQuestionNativePartialUsesCurrentSourcePreference(t *testing.T) {
	env := newCrossBotQuestions(t, true)
	ctx := t.Context()
	if err := env.store.SetTelegramBotBroadcastQuestions(ctx, "first", true); err != nil {
		t.Fatal(err)
	}
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:connected-native", ThreadID: env.first.String(), Type: "user_input", Async: true,
		Questions: []protocol.Question{{ID: "first", Prompt: "Initial connected question?"}, {ID: "second", Prompt: "Remaining connected question?"}}}
	env.emit(env.first, "user_input_requested", approval)
	env.flush()
	copies := env.questionCopies("Initial connected question?")
	if len(copies) != 4 {
		t.Fatalf("initial broadcast copies=%+v", copies)
	}
	if err := env.store.SetTelegramBotBroadcastQuestions(ctx, "first", false); err != nil {
		t.Fatal(err)
	}
	approval.Answers = map[string][]string{"first": {"Native answer"}}
	env.emit(env.first, "user_input_answered", approval)
	approval.Questions = approval.Questions[1:]
	env.emit(env.first, "user_input_requested", approval)
	env.flush()
	for _, copy := range copies {
		env.assertAnswer(copy.bot, copy.id, "Initial connected question?", "Native answer")
	}
	next := env.questionCopies("Remaining connected question?")
	if len(next) != 2 {
		t.Fatalf("remaining source question copies=%+v", next)
	}
	for _, copy := range next {
		if copy.bot != "first" {
			t.Fatalf("native partial question escaped connected bot: %+v", copy)
		}
	}
	env.assertSelections()
}

func TestConnectedQuestionQueuedBroadcastStopsBeforeSend(t *testing.T) {
	env := newCrossBotQuestions(t, true)
	ctx := t.Context()
	if err := env.store.SetTelegramBotBroadcastQuestions(ctx, "first", true); err != nil {
		t.Fatal(err)
	}
	const prompt = "Question queued before broadcast was disabled?"
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:queued-connected", ThreadID: env.first.String(), Type: "user_input", Async: true,
		Questions: []protocol.Question{{ID: "question", Prompt: prompt}}}
	env.emit(env.first, "user_input_requested", approval)
	leased, err := env.store.ClaimDeliveries(ctx, 100)
	if err != nil || len(leased) != 4 {
		t.Fatalf("initial broadcast leases=%+v %v", leased, err)
	}
	if err := env.store.SetTelegramBotBroadcastQuestions(ctx, "first", false); err != nil {
		t.Fatal(err)
	}
	for _, delivery := range leased {
		if err := env.sender.sendDelivery(ctx, delivery); err != nil {
			t.Fatal(err)
		}
	}
	copies := env.questionCopies(prompt)
	if len(copies) != 2 {
		t.Fatalf("revalidated question copies=%+v", copies)
	}
	for _, copy := range copies {
		if copy.bot != "first" {
			t.Fatalf("leased broadcast escaped flag off: %+v", copy)
		}
	}
}

func (e *crossBotQuestions) emit(session uuid.UUID, kind string, value any) protocol.Event {
	e.t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		e.t.Fatal(err)
	}
	e.sequence++
	event := protocol.Event{ID: uuid.NewString(), Seq: e.sequence, WorkerID: e.worker.String(), RuntimeID: e.runtime.String(), RuntimeGeneration: 1, SessionID: session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: raw}
	if err := e.store.IngestEvent(e.t.Context(), e.worker, e.connection, event); err != nil {
		e.t.Fatal(err)
	}
	return event
}

func (e *crossBotQuestions) flush() {
	e.t.Helper()
	for range 20 {
		rows, err := e.store.ClaimDeliveries(e.t.Context(), 1)
		if err != nil {
			e.t.Fatal(err)
		}
		if len(rows) == 0 {
			return
		}
		if err := e.sender.sendDelivery(e.t.Context(), rows[0]); err != nil {
			e.t.Fatal(err)
		}
	}
	e.t.Fatal("question delivery queue did not drain")
}

func (e *crossBotQuestions) message(bot string, id int64) SendMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.apis[bot].live[id]
}

func questionButton(t *testing.T, message SendMessage, label string) string {
	t.Helper()
	if message.Keyboard != nil {
		for _, row := range message.Keyboard.Rows {
			for _, button := range row {
				if button.Text == label {
					return strings.TrimPrefix(button.Data, "cb:")
				}
			}
		}
	}
	t.Fatalf("missing %q button: %+v", label, message)
	return ""
}

func (e *crossBotQuestions) assertAnswer(bot string, id int64, question, answer string) {
	e.t.Helper()
	message := e.message(bot, id)
	if message.Text != "Question: "+question+"\n\nAnswer: "+answer || message.Keyboard == nil || message.Keyboard.ForceReply || len(message.Keyboard.Rows) > 0 {
		e.t.Fatalf("%s question %d was not replaced by its answer: %+v", bot, id, message)
	}
}

func (e *crossBotQuestions) assertSelections() {
	e.t.Helper()
	for index, bot := range []string{"first", "second"} {
		want := []uuid.UUID{e.first, e.second}[index]
		result, err := e.store.AcceptTelegram(e.t.Context(), registry.IncomingUpdate{BotID: bot, UpdateID: 100, UserID: 10, ChatID: 20, Action: "status"})
		if err != nil || result.SessionID != want.String() {
			e.t.Fatalf("%s selection changed: %+v %v", bot, result, err)
		}
	}
}

func TestSharedQuestionsAdvanceAndEditEveryBotWithoutChangingSelections(t *testing.T) {
	env := newCrossBotQuestions(t)
	ctx := t.Context()
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:shared", ThreadID: env.first.String(), Type: "user_input", Async: true, Questions: []protocol.Question{{ID: "choice", Prompt: "Which option?", Options: []string{"One", "Two"}}, {ID: "details", Prompt: "Any details?"}}}
	env.emit(env.first, "user_input_requested", approval)
	env.flush()
	stale := questionButton(t, env.message("first", 2), "Two")
	answer := registry.IncomingUpdate{BotID: "second", UpdateID: 2, UserID: 10, ChatID: 20, CallbackToken: questionButton(t, env.message("second", 2), "One"), CallbackMessageID: 2}
	if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || result.View != "input_pending" || result.QuestionID != "details" {
		t.Fatalf("background-session choice: %+v %v", result, err)
	}
	env.flush()
	for _, bot := range []string{"first", "second"} {
		env.assertAnswer(bot, 2, "Which option?", "One")
		message := env.message(bot, 3)
		if !strings.Contains(message.Text, "Any details?") || message.Keyboard == nil {
			t.Fatalf("%s did not receive the next question: %+v", bot, message)
		}
	}
	// The other bot's old controls cannot replace an accepted partial answer.
	if result, err := env.store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "first", UpdateID: 2, UserID: 10, ChatID: 20, CallbackToken: stale, CallbackMessageID: 2}); err != nil || result.ErrorCode == "" {
		t.Fatalf("stale other-bot choice replaced the answer: %+v %v", result, err)
	}
	answer = registry.IncomingUpdate{BotID: "first", UpdateID: 3, UserID: 10, ChatID: 20, Action: "text", ReplyToMessageID: 3, Text: "Keep the current setting.\nMore details here."}
	if result, err := env.store.AcceptTelegram(ctx, answer); err != nil || result.CommandID == "" || result.SessionID != env.first.String() {
		t.Fatalf("free-text answer: %+v %v", result, err)
	}
	env.flush()
	for _, bot := range []string{"first", "second"} {
		env.assertAnswer(bot, 2, "Which option?", "One")
		env.assertAnswer(bot, 3, "Any details?", answer.Text)
		page, err := env.store.ListPendingQuestions(ctx, bot, 10, 20, 0, 0)
		if err != nil || len(page.Requests) != 0 {
			t.Fatalf("%s still lists answered request: %+v %v", bot, page, err)
		}
	}
	commands, err := env.store.PendingCommandsForWorker(ctx, env.worker, 10)
	if err != nil || len(commands) != 1 || commands[0].Operation != protocol.InputResponse || commands[0].SessionID != env.first.String() || commands[0].Arguments.Answers["choice"][0] != "One" || commands[0].Arguments.Answers["details"][0] != answer.Text {
		t.Fatalf("shared answers did not submit one complete request: %+v %v", commands, err)
	}
	env.assertSelections()
}

func TestNativePartialQuestionAnswersAdvanceEveryBot(t *testing.T) {
	env := newCrossBotQuestions(t)
	approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:native-shared", ThreadID: env.second.String(), Type: "user_input", Async: true, Questions: []protocol.Question{{ID: "first", Prompt: "First question?"}, {ID: "second", Prompt: "Second question?"}}}
	env.emit(env.second, "user_input_requested", approval)
	env.flush()
	approval.Answers = map[string][]string{"first": {"From the terminal"}}
	answered := env.emit(env.second, "user_input_answered", approval)
	// Workers reannounce the remaining questions after native partial input.
	approval.Questions = approval.Questions[1:]
	env.emit(env.second, "user_input_requested", approval)
	env.flush()
	for _, bot := range []string{"first", "second"} {
		env.assertAnswer(bot, 2, "First question?", "From the terminal")
		if message := env.message(bot, 3); !strings.Contains(message.Text, "Second question?") || message.Keyboard == nil {
			t.Fatalf("%s missed the question following a native answer: %+v", bot, message)
		}
	}
	if err := env.store.IngestEvent(t.Context(), env.worker, env.connection, answered); err != nil {
		t.Fatal(err)
	}
	env.flush()
	env.mu.Lock()
	for bot, api := range env.apis {
		if len(api.messages) != 3 || api.editsCount != 1 {
			t.Errorf("native answer replay duplicated work for %s: sends=%d edits=%d", bot, len(api.messages), api.editsCount)
		}
	}
	env.mu.Unlock()
	env.assertSelections()
}

func TestSharedQuestionAnswerEditsCopyCheckpointedAfterAnswer(t *testing.T) {
	for _, source := range []string{"telegram", "terminal"} {
		t.Run(source, func(t *testing.T) {
			env := newCrossBotQuestions(t)
			ctx := t.Context()
			approval := protocol.Approval{ID: uuid.NewString(), RequestID: "async:late-shared", ThreadID: env.second.String(), Type: "user_input", Async: true, Questions: []protocol.Question{{ID: "question", Prompt: "Keep this setting?"}}}
			env.emit(env.second, "user_input_requested", approval)
			copies, err := env.store.ClaimDeliveries(ctx, 2)
			if err != nil || len(copies) != 2 {
				t.Fatalf("initial broadcast: %+v %v", copies, err)
			}
			var late registry.Delivery
			for _, copy := range copies {
				if copy.BotID == "second" {
					late = copy
					continue
				}
				if err := env.sender.sendDelivery(ctx, copy); err != nil {
					t.Fatal(err)
				}
			}
			answer := "Yes, keep it."
			if source == "telegram" {
				result, err := env.store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "first", UpdateID: 2, UserID: 10, ChatID: 20, Action: "text", ReplyToMessageID: 2, Text: answer})
				if err != nil || result.CommandID == "" || result.SessionID != env.second.String() {
					t.Fatalf("background free-text answer: %+v %v", result, err)
				}
			} else {
				approval.Answers = map[string][]string{"question": {answer}}
				approval.State = "answered"
				env.emit(env.second, "user_input_answered", approval)
				approval.State = "cleared"
				env.emit(env.second, "approval_resolved", approval)
			}
			env.flush()
			env.assertAnswer("first", 2, "Keep this setting?", answer)
			// Another bot had already leased the broadcast when the answer won.
			// Its eventual checkpoint must acquire the same durable edit.
			if err := env.sender.sendDelivery(ctx, late); err != nil {
				t.Fatal(err)
			}
			env.flush()
			env.assertAnswer("second", 2, "Keep this setting?", answer)
			env.mu.Lock()
			for bot, api := range env.apis {
				if api.editsCount != 1 {
					t.Errorf("%s received %d answer edits, want one", bot, api.editsCount)
				}
			}
			env.mu.Unlock()
			env.assertSelections()
		})
	}
}

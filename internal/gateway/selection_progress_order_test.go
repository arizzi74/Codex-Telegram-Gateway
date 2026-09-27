package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func TestSessionSelectionRestoresCommentaryBeforeTool(t *testing.T) {
	for _, order := range [][]string{
		{"agent_progress_message", "tool_progress_message"},
		{"tool_progress_message", "agent_progress_message"},
		{"tool_progress_message"},
	} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			ctx := t.Context()
			store := testRegistry(t)
			worker, err := store.CreateWorker(ctx, registry.CreateWorkerInput{Name: "test-worker", OS: "linux", Arch: "amd64", TokenHash: auth.HashWorkerToken("test-token")})
			if err != nil {
				t.Fatal(err)
			}
			connection, runtime, idle, running := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			if err := store.BindConnection(ctx, worker.ID, connection); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: worker.ID, ConnectionID: connection, Runtimes: []registry.Runtime{{ID: runtime, WorkerID: worker.ID, ProfileID: "main", Name: "Main", Generation: 1, State: "running"}}}); err != nil {
				t.Fatal(err)
			}
			var sequence uint64
			event := func(session uuid.UUID, kind string, payload any) {
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
			for _, id := range []uuid.UUID{idle, running} {
				event(id, "session_discovered", protocol.Session{ID: id.String(), WorkerID: worker.ID.String(), RuntimeID: runtime.String(), ThreadID: id.String(), Name: "Session " + id.String(), CWD: "/work", State: "idle", Loaded: true, UpdatedAt: time.Now().UTC()})
			}
			api := &answerProgressAPI{live: map[int64]SendMessage{}}
			options := SenderOptions{BotID: "bot", OwnerID: 10}
			sender := NewSender(store, api, nil, options)
			flush := func() {
				t.Helper()
				if err := sender.flush(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var update int64
			selectSession := func(id uuid.UUID) {
				t.Helper()
				update++
				result, err := store.AcceptTelegram(ctx, registry.IncomingUpdate{BotID: "bot", UpdateID: update, UserID: 10, ChatID: 20, Action: "select", Target: id.String()})
				if err != nil || result.ErrorCode != "" {
					t.Fatalf("select: %+v %v", result, err)
				}
			}
			selectSession(idle)
			flush()
			event(running, "turn_started", protocol.Result{TurnID: "active-turn"})
			for _, kind := range order {
				event(running, kind, protocol.Result{TurnID: "active-turn", Text: "Latest " + kind})
			}
			// Repeat the switch to verify that retired slots are recreated in the
			// same order, independently of event chronology or sender restarts.
			for range 2 {
				start := len(api.messages)
				selectSession(running)
				flush()
				if len(api.messages) != start+1 || !strings.HasPrefix(api.messages[start].Text, "Connected to ") {
					t.Fatal("connection confirmation did not precede restored output")
				}
				for range order {
					sender = NewSender(store, api, nil, options)
					flush()
				}
				if len(api.messages) != start+1+len(order) {
					t.Fatalf("wrong restored message count: %d", len(api.messages)-start)
				}
				if len(order) == 2 && !strings.HasPrefix(api.messages[start+1].Text, "⏳ ") {
					t.Fatalf("commentary did not come first: %s", api.messages[start+1].Text)
				}
				tool := api.messages[len(api.messages)-1]
				if !strings.HasPrefix(tool.Text, "🔧 ") || len(tool.Entities) != 1 || tool.Entities[0].Type != "pre" {
					t.Fatalf("tool did not come last with monospace formatting: %+v", tool)
				}
				// Updates replace their existing messages without changing order.
				before := len(api.messages)
				event(running, "tool_progress_message", protocol.Result{TurnID: "active-turn", Text: "Updated tool"})
				flush()
				if len(api.messages) != before || len(api.editIDs) == 0 || api.editIDs[len(api.editIDs)-1] != int64(before) {
					t.Fatal("live tool update did not reuse the last restored slot")
				}
				selectSession(idle)
				flush()
				for range order {
					if err := sender.flushDeletions(ctx, store, api); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

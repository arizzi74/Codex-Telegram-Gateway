package gateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/registry"
)

type chatAuthorizationStore struct {
	*checkpointStore
	TelegramProgressStore
	skipped      int
	target       int64
	beforeTarget func()
}

func (s *chatAuthorizationStore) SkipDelivery(context.Context, string) error {
	s.skipped++
	return nil
}
func (s *chatAuthorizationStore) SuppressProgressDelivery(context.Context, string) (bool, error) {
	return false, nil
}
func (s *chatAuthorizationStore) TelegramProgressTarget(context.Context, string) (int64, error) {
	if s.beforeTarget != nil {
		s.beforeTarget()
	}
	return s.target, nil
}

type chatAuthorizationAPI struct {
	deliveryAPI
	edits                []SendMessage
	deleted              [][2]int64
	afterSend, afterEdit func()
	editError            error
}

func (a *chatAuthorizationAPI) Send(ctx context.Context, message SendMessage) (int64, error) {
	id, err := a.deliveryAPI.Send(ctx, message)
	if a.afterSend != nil {
		a.afterSend()
	}
	return id, err
}
func (a *chatAuthorizationAPI) EditFormatted(_ context.Context, _ int64, message SendMessage) error {
	a.edits = append(a.edits, message)
	if a.afterEdit != nil {
		a.afterEdit()
	}
	return a.editError
}
func (a *chatAuthorizationAPI) DeleteMessage(_ context.Context, chat, message int64) error {
	a.deleted = append(a.deleted, [2]int64{chat, message})
	return nil
}

func TestSenderAuthorizesActualFrozenDestinationForEveryDeliveryKind(t *testing.T) {
	for _, kind := range []string{"ui_response", "approval_requested", "user_input_requested", "turn_completed", "question_answered", "agent_progress_message", "tool_progress_message"} {
		t.Run(kind, func(t *testing.T) {
			// The envelope is allowed, but an old frozen message targets a revoked
			// chat. Test both fresh progress sends and edits of existing progress.
			for _, target := range []int64{0, 88} {
				row := registry.Delivery{ID: "old-delivery", BotID: "bot", ChatID: 20, Kind: kind, Payload: json.RawMessage(`{"message_id":88}`)}
				store := &chatAuthorizationStore{checkpointStore: &checkpointStore{row: row, chunks: []registry.DeliveryChunk{{Payload: json.RawMessage(`{"chat_id":30,"text":"private frozen content"}`)}}}, target: target}
				api := &chatAuthorizationAPI{}
				if err := NewSender(store, api, nil, SenderOptions{AllowedChatIDs: []int64{20}}).flush(t.Context()); err != nil {
					t.Fatal(err)
				}
				if len(api.messages)+len(api.edits) != 0 || store.skipped != 1 || len(store.marked) != 0 || store.delay != 0 {
					t.Fatalf("revoked destination sent or retried: sends=%d edits=%d skipped=%d", len(api.messages), len(api.edits), store.skipped)
				}
			}
		})
	}
}

func TestSenderRechecksAuthorizationBetweenFrozenChunks(t *testing.T) {
	row := registry.Delivery{ID: "multipart", ChatID: 20, Kind: "ui_response"}
	store := &chatAuthorizationStore{checkpointStore: &checkpointStore{row: row, chunks: []registry.DeliveryChunk{
		{Index: 0, Payload: json.RawMessage(`{"chat_id":20,"text":"first"}`)},
		{Index: 1, Payload: json.RawMessage(`{"chat_id":20,"text":"remaining private content"}`)},
	}}}
	allowed := []int64{20}
	api := &chatAuthorizationAPI{afterSend: func() { allowed[0] = 30 }}
	if err := NewSender(store, api, nil, SenderOptions{AllowedChatIDs: allowed}).flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(api.messages) != 1 || len(store.marked) != 1 || store.marked[0] != 0 || store.skipped != 1 {
		t.Fatalf("revocation did not stop remaining chunks: sends=%d marked=%v skipped=%d", len(api.messages), store.marked, store.skipped)
	}
}

func TestProgressRechecksAuthorizationAfterTargetLookupAndBeforeFallback(t *testing.T) {
	for _, duringEdit := range []bool{false, true} {
		row := registry.Delivery{ID: "progress", ChatID: 20, Kind: "agent_progress_message"}
		allowed := []int64{20}
		revoke := func() { allowed[0] = 30 }
		store := &chatAuthorizationStore{checkpointStore: &checkpointStore{row: row, chunks: []registry.DeliveryChunk{{Payload: json.RawMessage(`{"chat_id":20,"text":"progress"}`)}}}, target: 88}
		api := &chatAuthorizationAPI{}
		if duringEdit {
			api.afterEdit, api.editError = revoke, &TelegramError{Code: 400, Description: "Bad Request: message to edit not found"}
		} else {
			store.beforeTarget = revoke
		}
		if err := NewSender(store, api, nil, SenderOptions{AllowedChatIDs: allowed}).flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		wantEdits := 0
		if duringEdit {
			wantEdits = 1
		}
		if len(api.edits) != wantEdits || len(api.messages) != 0 || store.skipped != 1 {
			t.Fatalf("revoked progress escaped: edits=%d sends=%d skipped=%d", len(api.edits), len(api.messages), store.skipped)
		}
	}
}

func TestSenderAllowsUnrestrictedChatsAndRevokedPickerDeletion(t *testing.T) {
	for _, kind := range []string{"ui_response", "picker_cleanup"} {
		row := registry.Delivery{ID: "delivery", ChatID: 20, Kind: kind, Payload: json.RawMessage(`{"message_id":88}`)}
		store := &chatAuthorizationStore{checkpointStore: &checkpointStore{row: row, chunks: []registry.DeliveryChunk{{Payload: json.RawMessage(`{"chat_id":20,"text":"help"}`)}}}}
		api := &chatAuthorizationAPI{}
		var allowed []int64
		if kind == "picker_cleanup" {
			allowed = []int64{30}
		}
		if err := NewSender(store, api, nil, SenderOptions{AllowedChatIDs: allowed}).flush(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(api.messages)+len(api.deleted) != 1 || len(store.marked) != 1 || store.skipped != 0 {
			t.Fatal("unrestricted send or revoked-chat cleanup was suppressed")
		}
	}
}

type revokingPresenceStore struct {
	*presenceStore
	revoke func()
}

func (s *revokingPresenceStore) IsTelegramTypingTargetActive(context.Context, registry.TelegramTypingTarget) (bool, error) {
	s.revoke()
	return true, nil
}

func TestPresenceRechecksAllowlistAfterBackgroundQueue(t *testing.T) {
	allowed := []int64{20}
	store := &revokingPresenceStore{presenceStore: &presenceStore{targets: []registry.TelegramTypingTarget{{BotID: "bot", ChatID: 20}}}, revoke: func() { allowed[0] = 30 }}
	api := &presenceAPI{}
	if err := NewPresence(store, api, nil, PresenceOptions{AllowedChatIDs: allowed}).flush(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if api.count() != 0 {
		t.Fatal("queued typing reached revoked chat")
	}
}

type revokingMenuStore struct {
	*sessionMenuFixture
	revoke func()
}

func (s *revokingMenuStore) ListTelegramSessionAliases(ctx context.Context, bot string, user, chat, topic int64) ([]registry.TelegramSessionAlias, error) {
	s.revoke()
	return s.sessionMenuFixture.ListTelegramSessionAliases(ctx, bot, user, chat, topic)
}

func TestSessionMenuRechecksAllowlistAfterRenderingAndKeepsCleanup(t *testing.T) {
	allowed := []int64{20}
	fixture := &sessionMenuFixture{identity: "bot", contexts: []registry.TelegramMenuContext{{UserID: 7, ChatID: 20}}, aliases: []registry.TelegramSessionAlias{{Alias: "_private", Name: "Private project"}}}
	store := &revokingMenuStore{sessionMenuFixture: fixture, revoke: func() { allowed[0] = 30 }}
	menu := NewSessionMenus(store, fixture, nil, SessionMenuOptions{BotID: "bot", AllowedUserIDs: []int64{7}, AllowedChatIDs: allowed})
	if err := menu.flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fixture.setScopes) != 0 || len(fixture.deletedScopes) != 1 {
		t.Fatal("revoked chat received session aliases or lost cleanup")
	}
}

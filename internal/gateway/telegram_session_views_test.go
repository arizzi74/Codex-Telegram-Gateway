package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type sessionViewStore struct {
	*renderStoreFake
	presentation registry.TelegramSessionPresentation
	aliases      []registry.TelegramSessionAlias
}

func (s *sessionViewStore) TelegramSessionPresentation(context.Context, string, int64, int64, string) (registry.TelegramSessionPresentation, error) {
	return s.presentation, nil
}

func (s *sessionViewStore) ListTelegramSessionAliases(context.Context, string, int64, int64, int64) ([]registry.TelegramSessionAlias, error) {
	return s.aliases, nil
}

type selectionDeliveryStore struct {
	*checkpointStore
	checks     int
	suppressAt int
	skipped    []string
}

func (s *selectionDeliveryStore) SuppressTelegramDelivery(context.Context, string) (bool, error) {
	s.checks++
	return s.checks >= s.suppressAt, nil
}

func (s *selectionDeliveryStore) SkipDelivery(_ context.Context, id string) error {
	s.skipped = append(s.skipped, id)
	return nil
}

func TestSenderRechecksSessionSelectionBeforeEveryFinalChunk(t *testing.T) {
	row := eventRow(t, "turn_completed", protocol.Result{Text: "Long answer"}, testSessionID.String())
	row.ID = "delivery-final"
	store := &selectionDeliveryStore{checkpointStore: &checkpointStore{DeliveryStore: renderFixture(), row: row,
		chunks: []registry.DeliveryChunk{
			{Index: 0, Payload: json.RawMessage(`{"chat_id":42,"text":"first answer chunk"}`)},
			{Index: 1, Payload: json.RawMessage(`{"chat_id":42,"text":"must stay hidden after selection changes"}`)},
		}}, suppressAt: 3}
	api := &deliveryAPI{}
	if err := NewSender(store, api, nil).sendDelivery(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if len(api.messages) != 1 || len(store.marked) != 1 || len(store.skipped) != 1 || store.skipped[0] != row.ID {
		t.Fatalf("selection switch leaked a chunk: messages=%v marked=%v skipped=%v", api.messages, store.marked, store.skipped)
	}
}

func TestMultisessionEveryChunkGetsFullNameAndModeCanChangeBeforeSend(t *testing.T) {
	name := "A complete session name with spaces and 🧪 Unicode that stays readable"
	store := &sessionViewStore{renderStoreFake: renderFixture(), presentation: registry.TelegramSessionPresentation{Name: name, Marker: "🟦"}}
	sender := NewSender(store, nil, nil)
	row := eventRow(t, "turn_completed", protocol.Result{Text: strings.Repeat("Answer with Unicode 🧪. ", 450)}, testSessionID.String())
	chunks, err := sender.renderDeliveryMessages(context.Background(), row)
	if err != nil || len(chunks) < 2 {
		t.Fatalf("chunks=%d err=%v", len(chunks), err)
	}
	store.presentation.MultiSession = true
	for _, chunk := range chunks {
		message, err := sender.deliveryMessageForSend(context.Background(), row, chunk)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(message.Text, "🟦 "+name+"\n\n") || telegramTextLength(message.Text) > 4000 || !utf8.ValidString(message.Text) {
			t.Fatalf("missing full session header or oversized message: %q", message.Text)
		}
		if len(message.Entities) != 1 || message.Entities[0].Type != "bold" || message.Entities[0].Length != telegramTextLength("🟦 "+name) {
			t.Fatalf("header entities=%+v", message.Entities)
		}
		raw, _ := json.Marshal(message)
		if strings.Contains(string(raw), "_session_") {
			t.Fatal("checkpoint metadata leaked to Telegram")
		}
	}
	store.presentation.MultiSession = false
	message, err := sender.deliveryMessageForSend(context.Background(), row, chunks[0])
	if err != nil || strings.HasPrefix(message.Text, "🟦 ") || len(message.Entities) != 0 {
		t.Fatalf("multisession off not honored for frozen chunk: %+v err=%v", message, err)
	}
}

func TestMultisessionToolProgressKeepsOneMessageAndMonospaceBody(t *testing.T) {
	store := &sessionViewStore{renderStoreFake: renderFixture(), presentation: registry.TelegramSessionPresentation{Name: "Session 🧪", Marker: "🟩", MultiSession: true}}
	sender := NewSender(store, nil, nil)
	row := eventRow(t, "tool_progress_message", protocol.Result{TurnID: "turn-a", Text: strings.Repeat("run 🧪 tool\n", 1500)}, testSessionID.String())
	chunks, err := sender.renderDeliveryMessages(context.Background(), row)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("progress chunks=%d err=%v", len(chunks), err)
	}
	message, err := sender.deliveryMessageForSend(context.Background(), row, chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	if telegramTextLength(message.Text) > 4000 || !message.DisableNotification || !strings.HasSuffix(message.Text, "… (truncated)") {
		t.Fatalf("invalid tool progress: %+v", message)
	}
	if len(message.Entities) != 2 || message.Entities[0].Type != "bold" || message.Entities[1].Type != "pre" || message.Entities[1].Offset != telegramTextLength("🟩 Session 🧪\n\n") || message.Entities[1].Offset+message.Entities[1].Length != telegramTextLength(message.Text) {
		t.Fatalf("wrong tool font entities: %+v", message.Entities)
	}
}

func TestMultisessionImportedLongPreviewDoesNotBlockDelivery(t *testing.T) {
	store := &sessionViewStore{renderStoreFake: renderFixture(), presentation: registry.TelegramSessionPresentation{Name: strings.Repeat("Long imported preview 🧪 ", 1000), Marker: "🟩", MultiSession: true}}
	sender := NewSender(store, nil, nil)
	row := eventRow(t, "turn_completed", protocol.Result{Text: "The final answer stays readable."}, testSessionID.String())
	chunks, err := sender.renderDeliveryMessages(context.Background(), row)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("oversized preview blocked delivery: chunks=%d err=%v", len(chunks), err)
	}
	message, err := sender.deliveryMessageForSend(context.Background(), row, chunks[0])
	if err != nil || telegramTextLength(message.Text) > 4000 || !utf8.ValidString(message.Text) || !strings.Contains(message.Text, "…\n\n") || !strings.Contains(message.Text, "The final answer stays readable.") {
		t.Fatalf("long preview delivery=%q err=%v", message.Text, err)
	}
}

func TestQuestionNamesFullOriginatingSessionAndExplainsIndependentReply(t *testing.T) {
	store := renderFixture()
	store.sessions[0].Name = ""
	store.sessions[0].Preview = "A full originating session preview well beyond the old thirty-six-character label"
	row := eventRow(t, "user_input_requested", protocol.Approval{ID: testApproval.String(), Questions: []protocol.Question{{ID: "answer", Prompt: "Which option?"}}}, testSessionID.String())
	text, _, err := testSender(store, nil).render(context.Background(), row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "❓ "+store.sessions[0].Preview+"\n") || !strings.Contains(text, "keeps your current session selected") {
		t.Fatalf("question origin or reply instructions missing: %q", text)
	}
	if len(store.callbacks) != 1 || store.callbacks[0].SessionID != testSessionID || store.callbacks[0].ApprovalID != testApproval {
		t.Fatalf("question targets changed: %+v", store.callbacks)
	}
}

func TestMultisessionStatusShowsSessionCommandsAndColorLimitation(t *testing.T) {
	store := &sessionViewStore{renderStoreFake: renderFixture(), aliases: []registry.TelegramSessionAlias{{SessionID: testSessionID.String(), Name: "Auth Fix", Alias: "_auth_fix"}}}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
	text, _, err := sender.render(context.Background(), uiRow(t, registry.AcceptResult{View: "multisession", MultiSession: true}))
	if err != nil || !strings.Contains(text, "/_auth_fix — Auth Fix") || !strings.Contains(text, "does not support choosing") || !strings.Contains(text, "Type /_ to see session command suggestions") {
		t.Fatalf("mode status=%q err=%v", text, err)
	}
	if strings.Contains(text, "/-auth_fix") || store.aliases[0].Alias != "_auth_fix" {
		t.Fatal("shortcut display does not match the native menu alias")
	}
	text, _, err = sender.render(context.Background(), uiRow(t, registry.AcceptResult{View: "multisession"}))
	if err != nil || !strings.Contains(text, "mode is off") {
		t.Fatalf("off status=%q err=%v", text, err)
	}
}

func TestLiveUserPromptIsLabeledAsCodexHistory(t *testing.T) {
	text, _, err := testSender(renderFixture(), nil).render(context.Background(), eventRow(t, "user_message", protocol.Result{Text: "Build this feature"}, testSessionID.String()))
	if err != nil || text != "👤 You · Codex\n\nBuild this feature" {
		t.Fatalf("user prompt=%q err=%v", text, err)
	}
}

func TestGatewayHelpOmitsCommandsReplacedBySessionPicker(t *testing.T) {
	for _, text := range []string{helpText(), telegramErrorText("wizard_expired"), telegramErrorText("internal_error")} {
		if strings.Contains(text, "/tgnew") || strings.Contains(text, "/tgconnect") {
			t.Fatalf("removed command advertised: %q", text)
		}
	}
}

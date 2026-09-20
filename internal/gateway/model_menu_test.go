package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type modelRenderStore struct {
	*renderStoreFake
	commandID uuid.UUID
	requester int64
}

func (s *modelRenderStore) ModelRequester(_ context.Context, id uuid.UUID) (int64, error) {
	if id != s.commandID {
		return 0, registry.ErrTelegramTarget
	}
	return s.requester, nil
}

func TestModelMenuStepsRetainRequesterTargetAndExactChoices(t *testing.T) {
	steps := map[string][]protocol.ModelOption{
		"models": {
			{Args: "--menu sample-model", Label: "Sample model (default)"},
			{Args: "--menu another-model", Label: "Another model"},
			{Args: "--cancel", Label: "Cancel"},
		},
		"reasoning": {
			{Args: "sample-model medium", Label: "Medium (default)"},
			{Args: "sample-model high", Label: "High"},
			{Args: "--menu", Label: "Back to models"},
			{Args: "--cancel", Label: "Cancel"},
		},
	}
	for step, options := range steps {
		t.Run(step, func(t *testing.T) {
			store := &modelRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
			result := protocol.Result{CommandID: store.commandID.String(), Text: "Choose " + step + ".", ModelMenu: &protocol.ModelMenu{Options: options}}
			row := eventRow(t, "command_completed", result, testSessionID.String())
			row.ChatID, row.TopicID = 101, 3
			sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
			text, keyboard, err := sender.render(t.Context(), row)
			if err != nil || !strings.Contains(text, "auth-fix") || !strings.Contains(text, result.Text) {
				t.Fatalf("menu identity/instructions: %q %v", text, err)
			}
			if keyboard == nil || len(keyboard.Rows) != len(options) || len(store.callbacks) != len(options) {
				t.Fatalf("missing choices: %#v", keyboard)
			}
			for i, cb := range store.callbacks {
				if cb.Action != "model" || cb.UserID != 77 || cb.BotID != "bot" || cb.ChatID != 101 || cb.TopicID != 3 ||
					cb.SessionID != testSessionID || cb.RuntimeID != testRuntimeID || cb.Generation != 7 || cb.QuestionID != result.CommandID || cb.Decision != options[i].Args || cb.ExpiresAt.IsZero() {
					t.Fatalf("lost requester or frozen choice: %#v", cb)
				}
				button := keyboard.Rows[i][0]
				if button.Text != options[i].Label || !strings.HasPrefix(button.Data, "cb:") || len(button.Data) > 64 || strings.Contains(button.Data, options[i].Args) {
					t.Fatalf("invalid button/opaque token: %#v", button)
				}
			}
		})
	}
}

func TestModelMenuBoundsMaximumUnicodeKeyboard(t *testing.T) {
	store := &modelRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
	options := make([]protocol.ModelOption, protocol.ModelMenuPageSize+3)
	for i := range options {
		options[i] = protocol.ModelOption{Args: fmt.Sprintf("--menu model-%d", i), Label: strings.Repeat("🧠", 120)}
	}
	result := protocol.Result{CommandID: store.commandID.String(), ModelMenu: &protocol.ModelMenu{Options: options}}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
	_, keyboard, err := sender.render(t.Context(), eventRow(t, "command_completed", result, testSessionID.String()))
	if err != nil || keyboard == nil || len(keyboard.Rows) != len(options) || len(store.callbacks) != len(options) {
		t.Fatalf("maximum Unicode keyboard failed: %#v %v", keyboard, err)
	}
	for _, row := range keyboard.Rows {
		if len(row) != 1 || !utf8.ValidString(row[0].Text) || telegramTextLength(row[0].Text) > 80 || len(row[0].Text) > 160 {
			t.Fatalf("label exceeded Telegram bounds: %#v", row)
		}
		row[0].Data = strings.Repeat("x", 64)
	}
	markup, err := json.Marshal(keyboard)
	if err != nil || len(markup) > sessionKeyboardMaxBytes {
		t.Fatalf("maximum keyboard exceeds budget: %d bytes, %v", len(markup), err)
	}
}

func TestModelMenuRedactsBeforeTruncatingLabels(t *testing.T) {
	store := &modelRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
	secret := "PRIVATE_" + strings.Repeat("s", 95)
	redactor, err := auth.NewRedactor([]string{secret}, "")
	if err != nil {
		t.Fatal(err)
	}
	result := protocol.Result{CommandID: store.commandID.String(), Text: "Choose " + secret + ".", ModelMenu: &protocol.ModelMenu{Options: []protocol.ModelOption{
		{Args: "--menu sample-model", Label: secret + " model"},
		{Args: "--cancel", Label: "Cancel"},
	}}}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42, Redactor: redactor})
	text, keyboard, err := sender.render(t.Context(), eventRow(t, "command_completed", result, testSessionID.String()))
	if err != nil || keyboard == nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(text, "PRIVATE_") || strings.Contains(keyboard.Rows[0][0].Text, "PRIVATE_") || !strings.Contains(keyboard.Rows[0][0].Text, "[REDACTED]") {
		t.Fatalf("secret was exposed before truncation: %q %#v", text, keyboard)
	}
}

func TestModelMenuRejectsInvalidOrUnownedResultsBeforeCallbacks(t *testing.T) {
	for _, failure := range []string{"empty", "duplicate", "control", "unknown-command", "no-requester", "no-generation", "no-session", "no-runtime", "too-many", "keyboard-budget"} {
		t.Run(failure, func(t *testing.T) {
			store := &modelRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
			result := protocol.Result{CommandID: store.commandID.String(), ModelMenu: &protocol.ModelMenu{Options: []protocol.ModelOption{{Args: "--menu sample-model", Label: "Sample model"}}}}
			switch failure {
			case "empty":
				result.ModelMenu.Options = nil
			case "duplicate":
				result.ModelMenu.Options = append(result.ModelMenu.Options, result.ModelMenu.Options[0])
			case "control":
				result.ModelMenu.Options[0].Args = "--menu sample-model\n"
			case "unknown-command":
				result.CommandID = uuid.NewString()
			case "no-requester":
				store.requester = 0
			case "too-many", "keyboard-budget":
				count := protocol.ModelMenuPageSize + 3
				if failure == "too-many" {
					count++
				}
				result.ModelMenu.Options = make([]protocol.ModelOption, count)
				for i := range result.ModelMenu.Options {
					result.ModelMenu.Options[i] = protocol.ModelOption{Args: fmt.Sprintf("--menu model-%d", i), Label: strings.Repeat("&", 120)}
				}
			}
			row := eventRow(t, "command_completed", result, testSessionID.String())
			var event protocol.Event
			if err := json.Unmarshal(row.Payload, &event); err != nil {
				t.Fatal(err)
			}
			switch failure {
			case "no-generation":
				event.RuntimeGeneration = 0
			case "no-session":
				event.SessionID = ""
			case "no-runtime":
				event.RuntimeID = ""
			}
			row.Payload, _ = json.Marshal(event)
			sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
			if _, _, err := sender.render(t.Context(), row); err == nil || len(store.callbacks) != 0 {
				t.Fatalf("invalid menu created callbacks: %v %#v", err, store.callbacks)
			}
		})
	}
}

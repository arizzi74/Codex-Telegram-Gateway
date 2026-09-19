package gateway

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type permissionRenderStore struct {
	*renderStoreFake
	commandID uuid.UUID
	requester int64
}

func TestPermissionsMenuBoundsLargeProfileKeyboard(t *testing.T) {
	store := &permissionRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
	options := []protocol.PermissionOption{
		{ID: "ask-for-approval", Label: "Ask for approval"},
		{ID: "approve-for-me", Label: "Approve for me"},
		{ID: "full-access", Label: "Full Access"},
		{ID: "read-only", Label: "Read Only"},
	}
	for i := range 45 {
		name := strings.Repeat("名", 75) + strconv.Itoa(i)
		options = append(options, protocol.PermissionOption{ID: "profile:" + name, Label: name, Description: "Configured profile."})
	}
	options = append(options, protocol.PermissionOption{ID: "cancel", Label: "Cancel"})
	result := protocol.Result{CommandID: store.commandID.String(), Text: "Choose permissions.", Permissions: &protocol.PermissionMenu{Options: options}}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
	text, keyboard, err := sender.render(t.Context(), eventRow(t, "command_completed", result, testSessionID.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "/permissions profile:NAME") || len(keyboard.Rows) >= len(options) || len(store.callbacks) != len(keyboard.Rows) {
		t.Fatalf("large menu did not omit callbacks and explain fallback: rows=%d callbacks=%d text=%q", len(keyboard.Rows), len(store.callbacks), text)
	}
	shown := make(map[string]bool)
	for i, cb := range store.callbacks {
		shown[cb.Decision] = true
		button := &keyboard.Rows[i][0]
		if !strings.HasPrefix(button.Data, "cb:") || strings.Contains(button.Data, cb.Decision) {
			t.Fatal("choice was not represented by an opaque callback")
		}
		// Check the reserved worst case, independently of the short fake token.
		button.Data = strings.Repeat("x", 64)
	}
	for _, id := range []string{"ask-for-approval", "approve-for-me", "full-access", "read-only", "cancel"} {
		if !shown[id] {
			t.Fatalf("required control was omitted: %s", id)
		}
	}
	markup, err := json.Marshal(keyboard)
	if err != nil || len(markup) > sessionKeyboardMaxBytes {
		t.Fatalf("keyboard exceeds budget: %d bytes, %v", len(markup), err)
	}
	for _, option := range options {
		if !shown[option.ID] && !strings.Contains(text, option.Label) {
			t.Fatal("omitted profile lost its full name")
		}
	}
}

func TestPermissionsMenuRedactsBeforeTruncatingButtons(t *testing.T) {
	store := &permissionRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
	secret := "PRIVATE_" + strings.Repeat("s", 95)
	redactor, err := auth.NewRedactor([]string{secret}, "")
	if err != nil {
		t.Fatal(err)
	}
	result := protocol.Result{CommandID: store.commandID.String(), Permissions: &protocol.PermissionMenu{Options: []protocol.PermissionOption{
		{ID: "profile:custom", Label: secret + " profile", Description: "Configured profile."},
		{ID: "cancel", Label: "Cancel"},
	}}}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42, Redactor: redactor})
	text, keyboard, err := sender.render(t.Context(), eventRow(t, "command_completed", result, testSessionID.String()))
	if err != nil || strings.Contains(text, "PRIVATE_") || strings.Contains(keyboard.Rows[0][0].Text, "PRIVATE_") || !strings.Contains(keyboard.Rows[0][0].Text, "[REDACTED]") {
		t.Fatalf("secret was truncated before redaction: text=%q keyboard=%#v err=%v", text, keyboard, err)
	}
}

func (s *permissionRenderStore) PermissionsRequester(_ context.Context, id uuid.UUID) (int64, error) {
	if id != s.commandID {
		return 0, registry.ErrTelegramTarget
	}
	return s.requester, nil
}

func TestPermissionsMenuRetainsRequesterAndSession(t *testing.T) {
	store := &permissionRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
	result := protocol.Result{CommandID: store.commandID.String(), Text: "Choose permissions for this session.", Permissions: &protocol.PermissionMenu{Options: []protocol.PermissionOption{
		{ID: "auto", Label: "Ask for approval", Description: "Edit workspace files."},
		{ID: "full-access", Label: "Full Access", Description: "SECRET_DESCRIPTION"},
	}}}
	row := eventRow(t, "command_completed", result, testSessionID.String())
	row.ChatID, row.TopicID = 101, 3
	redactor, err := auth.NewRedactor([]string{"SECRET_DESCRIPTION"}, "")
	if err != nil {
		t.Fatal(err)
	}
	sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42, Redactor: redactor})
	text, keyboard, err := sender.render(t.Context(), row)
	if err != nil || !strings.Contains(text, "auth-fix") || !strings.Contains(text, result.Text) || strings.Contains(text, "SECRET_DESCRIPTION") {
		t.Fatalf("menu: %q %v", text, err)
	}
	if keyboard == nil || len(keyboard.Rows) != 2 || len(store.callbacks) != 2 {
		t.Fatalf("choices: %#v", keyboard)
	}
	for i, cb := range store.callbacks {
		if cb.Action != "permissions" || cb.UserID != 77 || cb.SessionID != testSessionID || cb.RuntimeID != testRuntimeID || cb.Generation != 7 ||
			cb.ChatID != 101 || cb.TopicID != 3 || cb.BotID != "bot" || cb.QuestionID != result.CommandID || cb.Decision != result.Permissions.Options[i].ID {
			t.Fatalf("lost request context: %#v", cb)
		}
		if !strings.HasPrefix(keyboard.Rows[i][0].Data, "cb:") || strings.Contains(keyboard.Rows[i][0].Data, cb.Decision) {
			t.Fatal("choice was not represented by an opaque callback")
		}
	}
}

func TestPermissionsMenuRejectsInvalidOrUnownedResults(t *testing.T) {
	for _, failure := range []string{"empty", "duplicate", "control", "unknown-command", "no-requester", "no-generation"} {
		t.Run(failure, func(t *testing.T) {
			store := &permissionRenderStore{renderStoreFake: renderFixture(), commandID: uuid.New(), requester: 77}
			result := protocol.Result{CommandID: store.commandID.String(), Permissions: &protocol.PermissionMenu{Options: []protocol.PermissionOption{{ID: "auto", Label: "Ask for approval"}}}}
			switch failure {
			case "empty":
				result.Permissions.Options = nil
			case "duplicate":
				result.Permissions.Options = append(result.Permissions.Options, result.Permissions.Options[0])
			case "control":
				result.Permissions.Options[0].ID = "auto\n"
			case "unknown-command":
				result.CommandID = uuid.NewString()
			case "no-requester":
				store.requester = 0
			}
			row := eventRow(t, "command_completed", result, testSessionID.String())
			if failure == "no-generation" {
				var event protocol.Event
				if err := json.Unmarshal(row.Payload, &event); err != nil {
					t.Fatal(err)
				}
				event.RuntimeGeneration = 0
				row.Payload, _ = json.Marshal(event)
			}
			sender := NewSender(store, nil, nil, SenderOptions{BotID: "bot", OwnerID: 42})
			if _, _, err := sender.render(t.Context(), row); err == nil || len(store.callbacks) != 0 {
				t.Fatalf("invalid menu created callbacks: %v %#v", err, store.callbacks)
			}
		})
	}
}

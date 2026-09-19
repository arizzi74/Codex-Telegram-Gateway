package gateway

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func sessionWizardView(view string) bool {
	switch view {
	case "new_session_name", "workspace_loading", "workspace_browser", "session_creating", "delete_sessions", "delete_session_confirm", "session_deleting", "session_deleted", "session_created", "wizard_cancelled":
		return true
	}
	return false
}

func (s *Sender) sessionWizardCallback(ctx context.Context, row registry.Delivery, response registry.AcceptResult, callback registry.Callback) (string, error) {
	if response.WizardID == "" || response.WizardRevision <= 0 {
		return "", errors.New("render session wizard: missing wizard identity")
	}
	callback.WizardID, callback.WizardRevision = response.WizardID, response.WizardRevision
	callback.UserID = response.UserID
	return s.callback(ctx, row, callback)
}

func (s *Sender) renderWizardRuntimePicker(ctx context.Context, row registry.Delivery, response registry.AcceptResult) (string, *TelegramKeyboard, error) {
	inv, err := s.inventory(ctx)
	if err != nil {
		return "", nil, err
	}
	keyboard := &TelegramKeyboard{}
	for _, runtime := range inv.runtimes {
		id, err := requiredUUID("runtime", runtime.ID)
		if err != nil {
			return "", nil, err
		}
		token, err := s.sessionWizardCallback(ctx, row, response, registry.Callback{Action: "wizard_runtime", RuntimeID: id, Generation: int64(runtime.Generation)})
		if err != nil {
			return "", nil, err
		}
		worker := inv.workerByID[runtime.WorkerID]
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: s.sessionListField(workerLabel(worker)+" / "+runtimeLabel(runtime), 72, 240), Data: token}})
	}
	cancel, err := s.sessionWizardCallback(ctx, row, response, registry.Callback{Action: "wizard_cancel"})
	if err != nil {
		return "", nil, err
	}
	keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: "Cancel", Data: cancel}})
	if len(inv.runtimes) == 0 {
		return "No runtimes are available.", keyboard, nil
	}
	verb := "create a session"
	if response.Action == "delete_session" {
		verb = "choose a session to delete"
	}
	return "Choose a worker runtime to " + verb + ".", keyboard, nil
}

func (s *Sender) renderSessionWizard(ctx context.Context, row registry.Delivery, response registry.AcceptResult) (string, *TelegramKeyboard, error) {
	if response.View == "wizard_cancelled" {
		return "Cancelled.", nil, nil
	}
	if response.View == "session_created" {
		return "✅ Created session: " + response.SessionName + "\n\nWorking directory:\n" + response.CWD + "\n\nYour current session selection was kept.", nil, nil
	}
	if response.View == "session_deleted" {
		return "✅ Deleted Codex session: " + response.SessionName + "\n\nWorking directory and files kept:\n" + response.CWD, nil, nil
	}
	if response.View == "session_deleting" {
		return "Deleting Codex session: " + response.SessionName + "\nThe working directory and files will be kept.", nil, nil
	}
	if response.View == "session_creating" {
		folder, err := protocol.SessionDirectoryName(response.SessionName)
		if err != nil {
			return "", nil, err
		}
		return "Creating session: " + response.SessionName + "\n\nWorking directory:\n" + filepath.Join(response.CWD, folder), nil, nil
	}
	if response.View == "delete_sessions" {
		runtimeID, err := requiredUUID("runtime", response.RuntimeID)
		if err != nil {
			return "", nil, err
		}
		inv, err := s.inventory(ctx)
		if err != nil {
			return "", nil, err
		}
		return s.renderSessionList(ctx, row, runtimeID, inv, response.SessionPage, &response)
	}
	keyboard := &TelegramKeyboard{}
	button := func(label string, callback registry.Callback) error {
		token, err := s.sessionWizardCallback(ctx, row, response, callback)
		if err != nil {
			return err
		}
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: label, Data: token}})
		return nil
	}
	var text string
	switch response.View {
	case "new_session_name":
		text = "What would you like to name the new session?\n\nSend the session name as your next message. Spaces become underscores in its folder name: My Project → My_Project."
	case "workspace_loading":
		text = "Loading folders for " + response.SessionName + "…"
	case "workspace_browser":
		page := response.Workspace
		if page == nil || page.Path == "" {
			return "", nil, errors.New("render folder browser: missing directory page")
		}
		folder, err := protocol.SessionDirectoryName(response.SessionName)
		if err != nil {
			return "", nil, err
		}
		var body strings.Builder
		fmt.Fprintf(&body, "New session: %s\nFolder name: %s\n\nChoose the parent folder:\n%s\n\nCreate here will use:\n%s", response.SessionName, folder, page.Path, filepath.Join(page.Path, folder))
		if len(page.Directories) > 0 {
			body.WriteString("\n\nSubfolders:")
		}
		for i, entry := range page.Directories {
			number := page.Offset + i + 1
			fmt.Fprintf(&body, "\n%d. %s", number, entry.Name)
			if err := button(fmt.Sprintf("Open %d", number), registry.Callback{Action: "wizard_browse", Path: entry.Path}); err != nil {
				return "", nil, err
			}
		}
		if len(page.Directories) == 0 {
			body.WriteString("\n\nNo subfolders in this directory.")
		}
		if page.Parent != "" {
			if err := button("↑ Parent folder", registry.Callback{Action: "wizard_browse", Path: page.Parent}); err != nil {
				return "", nil, err
			}
		}
		if page.Offset > 0 {
			if err := button("‹ Previous folders", registry.Callback{Action: "wizard_browse", Path: page.Path, Offset: max(0, page.Offset-protocol.WorkspacePageSize)}); err != nil {
				return "", nil, err
			}
		}
		if page.HasMore {
			if err := button("More folders ›", registry.Callback{Action: "wizard_browse", Path: page.Path, Offset: page.Offset + len(page.Directories)}); err != nil {
				return "", nil, err
			}
		}
		if err := button("Create here", registry.Callback{Action: "wizard_create"}); err != nil {
			return "", nil, err
		}
		if err := button("Change name", registry.Callback{Action: "wizard_rename"}); err != nil {
			return "", nil, err
		}
		text = body.String()
	case "delete_session_confirm":
		sessionID, err := requiredUUID("session", response.SessionID)
		if err != nil {
			return "", nil, err
		}
		runtimeID, err := requiredUUID("runtime", response.RuntimeID)
		if err != nil {
			return "", nil, err
		}
		text = "Delete Codex session \"" + response.SessionName + "\"?\n\nThis permanently removes the conversation and any child sessions it created.\n\nThe working directory and all its files will be kept:\n" + response.CWD
		if err := button("Delete session", registry.Callback{Action: "delete_session_confirm", SessionID: sessionID, RuntimeID: runtimeID}); err != nil {
			return "", nil, err
		}
	default:
		return "", nil, errors.New("render session wizard: unsupported view")
	}
	if err := button("Cancel", registry.Callback{Action: "wizard_cancel"}); err != nil {
		return "", nil, err
	}
	return text, keyboard, nil
}

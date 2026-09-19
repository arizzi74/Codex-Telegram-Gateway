package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func wizardResponse(view string) registry.AcceptResult {
	return registry.AcceptResult{View: view, WizardID: uuid.NewString(), WizardRevision: 3, SessionName: "My Project", CWD: "/work/My_Project", RuntimeID: testRuntimeID.String(), UserID: 73}
}

func TestWorkspaceBrowserShowsFullNamesAndScopedNavigation(t *testing.T) {
	store := renderFixture()
	response := wizardResponse("workspace_browser")
	response.Workspace = &protocol.WorkspacePage{Path: "/work/projects", Parent: "/work", Offset: 12, HasMore: true}
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("folder %d %s", i, strings.Repeat("long name ", 15))
		response.Workspace.Directories = append(response.Workspace.Directories, protocol.WorkspaceEntry{Name: name, Path: "/work/projects/" + name})
	}
	text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, response))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"New session: My Project", "Folder name: My_Project", "/work/projects/My_Project", response.Workspace.Directories[0].Name, response.Workspace.Directories[11].Name} {
		if !strings.Contains(text, want) {
			t.Fatalf("browser omitted %q", want)
		}
	}
	assertSessionPageBudget(t, text, keyboard)
	actions := map[string]int{}
	for _, callback := range store.callbacks {
		actions[callback.Action]++
		if callback.UserID != 73 || callback.ChatID != 99 || callback.TopicID != 4 || callback.BotID != "bot" || callback.WizardID != response.WizardID || callback.WizardRevision != 3 {
			t.Fatalf("wizard callback lost scope: %+v", callback)
		}
	}
	if actions["wizard_browse"] != 15 || actions["wizard_create"] != 1 || actions["wizard_rename"] != 1 || actions["wizard_cancel"] != 1 {
		t.Fatalf("browser controls=%v", actions)
	}
}

func TestDeletePickerMatchesSessionPagesAndKeepsFullNames(t *testing.T) {
	store := sessionPagesFixture(23)
	store.sessions[1].Name = strings.Repeat("A full session name ", 30)
	response := wizardResponse("delete_sessions")
	seen := map[uuid.UUID]bool{}
	for page := 0; page < 3; page++ {
		store.callbacks = nil
		response.SessionPage = page
		text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, response))
		if err != nil {
			t.Fatal(err)
		}
		assertSessionPageBudget(t, text, keyboard)
		if !strings.Contains(text, "23 sessions") || !strings.Contains(text, "files will be kept") {
			t.Fatal("delete list omitted context", text)
		}
		for _, callback := range store.callbacks {
			if callback.Action == "select" || callback.Action == "new" {
				t.Fatal("delete list created a connection or new-session button")
			}
			if callback.Action == "delete_session_pick" {
				if seen[callback.SessionID] {
					t.Fatal("duplicate session on delete pages")
				}
				seen[callback.SessionID] = true
				for _, session := range store.sessions {
					if session.ID == callback.SessionID.String() && !strings.Contains(text, strings.TrimSpace(session.Name)) {
						t.Fatal("delete list truncated session name")
					}
				}
			}
			if callback.WizardID != response.WizardID || callback.UserID != 73 {
				t.Fatal("delete callback lost authorization scope")
			}
		}
	}
	if len(seen) != 23 {
		t.Fatalf("listed %d sessions", len(seen))
	}
}

func TestWizardErrorsRetainControlsAndDeletionKeepsFolderText(t *testing.T) {
	for _, view := range []string{"new_session_name", "workspace_browser", "delete_session_confirm"} {
		t.Run(view, func(t *testing.T) {
			store := renderFixture()
			response := wizardResponse(view)
			response.SessionID = testSessionID.String()
			response.ErrorCode = "session_busy"
			response.Workspace = &protocol.WorkspacePage{Path: "/work", Directories: []protocol.WorkspaceEntry{}}
			text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, response))
			if err != nil || keyboard == nil || len(keyboard.Rows) == 0 || !strings.Contains(text, "active turn") {
				t.Fatalf("recoverable wizard error lost controls: %q %+v %v", text, keyboard, err)
			}
			if view == "delete_session_confirm" && (!strings.Contains(text, "My Project") || !strings.Contains(text, "/work/My_Project") || !strings.Contains(text, "files will be kept")) {
				t.Fatal("delete confirmation lacks exact target")
			}
		})
	}
	store := renderFixture()
	response := wizardResponse("session_deleted")
	response.SessionID = uuid.NewString() // deleted session no longer exists in inventory
	text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, response))
	if err != nil || keyboard != nil || !strings.Contains(text, "Deleted Codex session: My Project") || !strings.Contains(text, "files kept:") {
		t.Fatalf("delete completion=%q %v", text, err)
	}
}

func TestWizardRuntimePickerFreezesRuntimeAndUser(t *testing.T) {
	store := renderFixture()
	response := wizardResponse("runtime_picker")
	response.Action = "delete_session"
	text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, response))
	if err != nil || keyboard == nil || !strings.Contains(text, "session to delete") {
		t.Fatalf("runtime picker=%q %v", text, err)
	}
	runtimes := 0
	for _, callback := range store.callbacks {
		if callback.Action == "wizard_runtime" {
			runtimes++
			if callback.RuntimeID == uuid.Nil || callback.Generation <= 0 || callback.UserID != 73 || callback.WizardID != response.WizardID {
				t.Fatalf("unscoped runtime choice %+v", callback)
			}
		}
	}
	if runtimes != 2 {
		t.Fatalf("runtime choices=%d", runtimes)
	}
}

func TestPendingWizardMessagesOfferCurrentControlsWithoutClaimingCancellation(t *testing.T) {
	for _, view := range []string{"session_deleting", "session_creating", "runtime_picker"} {
		t.Run(view, func(t *testing.T) {
			store := renderFixture()
			response := wizardResponse(view)
			response.Action = "delete_session"
			response.ErrorCode = "wizard_pending"
			text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, response))
			if err != nil || keyboard == nil || len(keyboard.Rows) == 0 || !strings.Contains(text, "not sent as a prompt") {
				t.Fatalf("pending wizard is a dead end: %q %+v %v", text, keyboard, err)
			}
			if view == "runtime_picker" {
				return
			}
			if len(store.callbacks) != 1 || store.callbacks[0].Action != "wizard_dismiss" || store.callbacks[0].WizardID != response.WizardID || store.callbacks[0].WizardRevision != response.WizardRevision || store.callbacks[0].UserID != response.UserID {
				t.Fatalf("mutation has no scoped Continue chat control: %+v", store.callbacks)
			}
			if !strings.Contains(text, "does not cancel this request") || keyboard.Rows[0][0].Text != "Continue chat" {
				t.Fatalf("dismissal misrepresents accepted mutation: %q %+v", text, keyboard)
			}
		})
	}
	store := renderFixture()
	text, keyboard, err := testSender(store, nil).render(context.Background(), uiRow(t, wizardResponse("wizard_dismissed")))
	if err != nil || keyboard != nil || !strings.Contains(text, "has not been cancelled") || !strings.Contains(text, "may still complete") {
		t.Fatalf("dismissal response=%q %+v %v", text, keyboard, err)
	}
}

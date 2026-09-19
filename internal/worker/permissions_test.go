package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/protocol"
)

func installPermissionCatalog(t *testing.T, server *codextest.Server, autoReview bool) {
	t.Helper()
	for method, result := range map[string]any{
		"permissionProfile/list": map[string]any{"data": []map[string]any{
			{"id": ":read-only", "allowed": true}, {"id": ":workspace", "allowed": true}, {"id": ":danger-full-access", "allowed": true},
			{"id": "MyProfile", "allowed": true, "description": "Managed project access"}, {"id": "disabled", "allowed": false},
		}},
		"experimentalFeature/list": map[string]any{"data": []map[string]any{{"name": "guardian_approval", "enabled": autoReview}}},
		"configRequirements/read":  map[string]any{"requirements": nil},
	} {
		if err := server.SetMethodResult(method, result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPermissionsMenuNeverResumesOrMutatesColdThread(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	installPermissionCatalog(t, server, true)
	session := installColdSession(a, runtime, "cold-permissions")
	actor := a.actorForThread(runtime.ID, session.ThreadID)
	client, _, _ := a.manager.Client(runtime.ID)
	result, err := actor.executeCodexCommand(context.Background(), client, "permissions", "")
	if err != nil || result.Permissions == nil {
		t.Fatalf("menu = %#v, %v", result, err)
	}
	for _, id := range []string{"ask-for-approval", "approve-for-me", "full-access", "read-only", "profile:MyProfile", "cancel"} {
		if !permissionOptionPresent(result, id) {
			t.Errorf("missing %s: %#v", id, result.Permissions.Options)
		}
	}
	if permissionOptionPresent(result, "profile:disabled") {
		t.Fatal("disallowed profile offered")
	}
	for _, call := range server.Calls() {
		if call.Method == "thread/resume" || call.Method == "thread/settings/update" || call.Method == "turn/start" {
			t.Fatalf("read-only menu called %s", call.Method)
		}
		if call.Method == "experimentalFeature/list" && strings.Contains(string(call.Params), "threadId") {
			t.Fatal("cold feature read supplied an unloaded thread")
		}
	}
}

func TestPermissionsFullAccessRequiresConfirmationAndRechecksRequirements(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	installPermissionCatalog(t, server, false)
	session := installColdSession(a, runtime, "permission-confirm")
	actor := a.actorForThread(runtime.ID, session.ThreadID)
	client, _, _ := a.manager.Client(runtime.ID)
	result, err := actor.executeCodexCommand(context.Background(), client, "permissions", "full-access")
	if err != nil || !permissionOptionPresent(result, "confirm-full-access") || !permissionOptionPresent(result, "cancel") {
		t.Fatalf("confirmation = %#v, %v", result, err)
	}
	if hasCall(server.Calls(), "thread/settings/update") || hasCall(server.Calls(), "thread/resume") {
		t.Fatal("asking for confirmation changed the session")
	}
	if _, err := actor.executeCodexCommand(context.Background(), client, "permissions", "cancel"); err != nil {
		t.Fatal(err)
	}
	if err := server.SetMethodResult("configRequirements/read", map[string]any{"requirements": map[string]any{"allowedApprovalPolicies": []string{"on-request"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := actor.executeCodexCommand(context.Background(), client, "permissions", "confirm-full-access"); err == nil {
		t.Fatal("confirmation bypassed changed requirements")
	}
	if hasCall(server.Calls(), "thread/settings/update") {
		t.Fatal("disallowed confirmation mutated settings")
	}
}

func TestPermissionsPresetsApplyPolicyReviewerAndProfileTogether(t *testing.T) {
	for _, test := range []struct{ choice, profile, policy, reviewer string }{
		{"workspace-write", ":workspace", "on-request", "user"},
		{"readonly", ":read-only", "on-request", "user"},
		{"approve-for-me", ":workspace", "on-request", "auto_review"},
		{"confirm-full-access", ":danger-full-access", "never", "user"},
		{"profile:MyProfile", "MyProfile", "", ""},
	} {
		t.Run(test.choice, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			installPermissionCatalog(t, server, true)
			session := installColdSession(a, runtime, "permission-update")
			command := agentCommand(runtime, session, protocol.CodexCommand)
			command.Arguments = protocol.Arguments{Codex: &protocol.CodexCommandPayload{Name: "permissions", Args: test.choice}}
			if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
				t.Fatalf("apply ack = %#v, %v", ack, err)
			}
			waitFor(t, func() bool {
				record, found, err := a.store.LoadCommand(command.ID)
				return err == nil && found && (record.State == CommandCompleted || record.State == CommandFailed)
			})
			record, _, err := a.store.LoadCommand(command.ID)
			if err != nil || record.Result == nil || record.State != CommandCompleted || record.Result.Permissions != nil || !strings.Contains(record.Result.Text, "subsequent turns") {
				t.Fatalf("apply = %#v, %v", record, err)
			}
			var update map[string]string
			for _, call := range server.Calls() {
				if call.Method == "thread/settings/update" {
					if err := json.Unmarshal(call.Params, &update); err != nil {
						t.Fatal(err)
					}
				}
			}
			if !hasCall(server.Calls(), "thread/resume") || countCall(server.Calls(), "thread/settings/update") != 1 || hasCall(server.Calls(), "turn/start") {
				t.Fatalf("incorrect lifecycle: %#v", server.Calls())
			}
			if update["permissions"] != test.profile || update["approvalPolicy"] != test.policy || update["approvalsReviewer"] != test.reviewer || update["threadId"] != session.ThreadID {
				t.Fatalf("update = %#v", update)
			}
			if _, exists := update["sandboxPolicy"]; exists {
				t.Fatal("profile combined with sandbox")
			}
		})
	}
}

func TestPermissionsHideDisabledGuardianAndRequirementChoices(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	installPermissionCatalog(t, server, false)
	if err := server.SetMethodResult("configRequirements/read", map[string]any{"requirements": map[string]any{"allowedApprovalPolicies": []string{"on-request"}, "allowedApprovalsReviewers": []string{"user"}}}); err != nil {
		t.Fatal(err)
	}
	session := installSession(a, runtime, "permission-restrictions", "")
	actor := a.actorForThread(runtime.ID, session.ThreadID)
	client, _, _ := a.manager.Client(runtime.ID)
	result, err := actor.executeCodexCommand(context.Background(), client, "permissions", "")
	if err != nil {
		t.Fatal(err)
	}
	if permissionOptionPresent(result, "approve-for-me") || permissionOptionPresent(result, "full-access") || !permissionOptionPresent(result, "ask-for-approval") {
		t.Fatalf("unexpected options: %#v", result.Permissions)
	}
	if _, err := actor.executeCodexCommand(context.Background(), client, "permissions", "approve-for-me"); err == nil {
		t.Fatal("disabled feature was selectable")
	}
	if hasCall(server.Calls(), "thread/settings/update") {
		t.Fatal("disabled feature mutated settings")
	}
}

func TestPermissionsActiveTurnCanInspectAndCancelButNotApply(t *testing.T) {
	a, runtime, server, cleanup := testAgent(t)
	defer cleanup()
	installPermissionCatalog(t, server, true)
	session := installSession(a, runtime, "permission-busy", "active-turn")
	actor := a.actorForThread(runtime.ID, session.ThreadID)
	client, _, _ := a.manager.Client(runtime.ID)
	for _, args := range []string{"", "cancel", "full-access"} {
		if codexCommandNeedsIdle("permissions", args) {
			t.Fatalf("%q incorrectly requires idle", args)
		}
		if _, err := actor.executeCodexCommand(context.Background(), client, "permissions", args); err != nil {
			t.Fatalf("inspect %q: %v", args, err)
		}
	}
	for _, args := range []string{"read-only", "confirm-full-access"} {
		if !codexCommandNeedsIdle("permissions", args) {
			t.Fatalf("%q incorrectly permits running turn", args)
		}
		if _, err := actor.executeCodexCommand(context.Background(), client, "permissions", args); err == nil {
			t.Fatalf("applied %q during active turn", args)
		}
	}
	if hasCall(server.Calls(), "thread/settings/update") || hasCall(server.Calls(), "thread/resume") {
		t.Fatal("active thread was mutated")
	}
}

func permissionOptionPresent(result protocol.Result, id string) bool {
	if result.Permissions != nil {
		for _, option := range result.Permissions.Options {
			if option.ID == id {
				return true
			}
		}
	}
	return false
}

package codexadapter

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPermissionProfilesHonorPaginationAndWorkspace(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	type result struct {
		profiles []PermissionProfile
		err      error
	}
	done := make(chan result, 1)
	go func() {
		profiles, err := client.ListPermissionProfiles(context.Background(), "/work/project")
		done <- result{profiles, err}
	}()
	first := fake.next(t)
	if method(t, first) != "permissionProfile/list" || string(params(t, first)["cwd"]) != `"/work/project"` {
		t.Fatalf("first request: %#v", first)
	}
	fake.respond(t, first, map[string]any{"data": []map[string]any{{"id": ":workspace", "allowed": true}}, "nextCursor": "next"})
	second := fake.next(t)
	if string(params(t, second)["cursor"]) != `"next"` {
		t.Fatalf("second request: %#v", second)
	}
	fake.respond(t, second, map[string]any{"data": []map[string]any{{"id": "restricted", "description": "Managed profile", "allowed": false}}})
	got := <-done
	if got.err != nil || len(got.profiles) != 2 || got.profiles[1].Allowed || got.profiles[1].Description != "Managed profile" {
		t.Fatalf("profiles: %#v, %v", got.profiles, got.err)
	}
}

func TestPermissionPresetSettingsUseNamedProfileAndReviewer(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	profile, policy, reviewer := ":workspace", "on-request", "auto_review"
	done := make(chan error, 1)
	go func() {
		done <- client.UpdateThreadSettings(context.Background(), "selected", ThreadSettingsUpdate{PermissionProfile: &profile, ApprovalPolicy: &policy, ApprovalsReviewer: &reviewer})
	}()
	request := fake.next(t)
	var got map[string]string
	if err := json.Unmarshal(request["params"], &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got["threadId"] != "selected" || got["permissions"] != profile || got["approvalPolicy"] != policy || got["approvalsReviewer"] != reviewer {
		t.Fatalf("settings params: %#v", got)
	}
	fake.respond(t, request, map[string]any{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateThreadSettings(context.Background(), "selected", ThreadSettingsUpdate{PermissionProfile: &profile, SandboxMode: "workspace-write"}); err == nil {
		t.Fatal("accepted mutually exclusive permission fields")
	}
}

func TestPermissionRequirementsPreserveEmptyRestrictions(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	type result struct {
		requirements PermissionRequirements
		err          error
	}
	done := make(chan result, 1)
	go func() {
		requirements, err := client.ReadPermissionRequirements(context.Background())
		done <- result{requirements, err}
	}()
	request := fake.next(t)
	if method(t, request) != "configRequirements/read" {
		t.Fatalf("method: %#v", request)
	}
	fake.respond(t, request, map[string]any{"requirements": map[string]any{"allowedApprovalPolicies": []any{}, "allowedApprovalsReviewers": []any{}}})
	got := <-done
	if got.err != nil || got.requirements.ApprovalPolicies == nil || got.requirements.ApprovalsReviewers == nil {
		t.Fatalf("requirements: %#v, %v", got.requirements, got.err)
	}
}

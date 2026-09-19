package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

type permissionChoice struct {
	protocol.PermissionOption
	profile, policy, reviewer string
}

func (s *sessionActor) codexPermissions(ctx context.Context, client *codexadapter.Client, args string) (protocol.Result, error) {
	if strings.EqualFold(strings.TrimSpace(args), "cancel") {
		return protocol.Result{Text: "Permission change cancelled. Session settings are unchanged."}, nil
	}
	choiceID := strings.ToLower(strings.TrimSpace(args))
	switch choiceID {
	case "readonly":
		choiceID = "read-only"
	case "workspace", "workspace-write", "default", "auto":
		choiceID = "ask-for-approval"
	case "danger-full-access":
		choiceID = "full-access"
	}
	if strings.HasPrefix(args, "profile:") {
		choiceID = args // Configured profile identifiers are case-sensitive.
	}
	choices, err := s.permissionChoices(ctx, client)
	if err != nil {
		return protocol.Result{}, err
	}
	if args == "" {
		result := protocol.Result{Text: "Choose permissions for this session. Changes apply to subsequent turns and do not change your global Codex configuration."}
		if len(choices) == 0 {
			result.Text = "No permission presets are available under this runtime's configuration requirements."
			return result, nil
		}
		result.Permissions = &protocol.PermissionMenu{}
		for _, choice := range choices {
			if len(result.Permissions.Options) == 49 {
				result.Text += "\nAdditional configured profiles can be selected with /permissions profile:NAME."
				break
			}
			result.Permissions.Options = append(result.Permissions.Options, choice.PermissionOption)
		}
		result.Permissions.Options = append(result.Permissions.Options, protocol.PermissionOption{ID: "cancel", Label: "Cancel"})
		return result, nil
	}
	lookup := choiceID
	if lookup == "confirm-full-access" {
		lookup = "full-access"
	}
	var selected *permissionChoice
	for i := range choices {
		if choices[i].ID == lookup {
			selected = &choices[i]
			break
		}
	}
	if selected == nil {
		return protocol.Result{}, validationError("This permission option is unavailable or disallowed by Codex requirements. Run /permissions to see current choices.")
	}
	if choiceID == "full-access" {
		return protocol.Result{
			Text: "Enable Full Access for this session? Codex will be able to edit files outside the workspace and run commands with network access without asking for approval.",
			Permissions: &protocol.PermissionMenu{Options: []protocol.PermissionOption{
				{ID: "confirm-full-access", Label: "Enable Full Access"},
				{ID: "cancel", Label: "Cancel"},
			}},
		}, nil
	}
	if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
		return protocol.Result{}, err
	}
	update := codexadapter.ThreadSettingsUpdate{PermissionProfile: &selected.profile}
	if selected.policy != "" {
		update.ApprovalPolicy, update.ApprovalsReviewer = &selected.policy, &selected.reviewer
	}
	if err := client.UpdateThreadSettings(ctx, s.session.ThreadID, update); err != nil {
		return protocol.Result{}, err
	}
	return protocol.Result{Text: "Session permissions set to “" + selected.Label + "” for subsequent turns."}, nil
}

func (s *sessionActor) permissionChoices(ctx context.Context, client *codexadapter.Client) ([]permissionChoice, error) {
	profiles, err := client.ListPermissionProfiles(ctx, s.session.CWD)
	if err != nil {
		return nil, err
	}
	requirements, err := client.ReadPermissionRequirements(ctx)
	if err != nil {
		return nil, err
	}
	threadID := ""
	if s.session.Loaded && s.session.State != "not_loaded" {
		threadID = s.session.ThreadID
	}
	autoReview, err := client.FeatureEnabled(ctx, threadID, "guardian_approval")
	if err != nil && !errors.Is(err, codexadapter.ErrMethodUnavailable) {
		return nil, err
	}
	if threadID == "" {
		cfg, err := client.ReadEffectiveConfig(ctx, s.session.CWD)
		if err != nil {
			return nil, err
		}
		if enabled, exists := cfg.Features["guardian_approval"]; exists {
			autoReview = enabled
		}
	}
	allowed := map[string]bool{}
	for _, profile := range profiles {
		allowed[profile.ID] = profile.Allowed
	}
	var choices []permissionChoice
	addPreset := func(id, label, description, profile, policy, reviewer string) {
		if !allowed[profile] || !permissionPolicyAllowed(requirements, policy, reviewer) {
			return
		}
		choices = append(choices, permissionChoice{protocol.PermissionOption{ID: id, Label: label, Description: description}, profile, policy, reviewer})
	}
	addPreset("ask-for-approval", "Ask for approval", "Read and edit workspace files and run commands. Ask before accessing the internet or editing outside the workspace.", ":workspace", "on-request", "user")
	if autoReview {
		addPreset("approve-for-me", "Approve for me", "Codex automatically reviews approval requests for risk.", ":workspace", "on-request", "auto_review")
	}
	addPreset("full-access", "Full Access", "Edit files outside the workspace and access the internet without approval. Requires confirmation.", ":danger-full-access", "never", "user")
	addPreset("read-only", "Read Only", "Read files. Ask before editing files or accessing the internet.", ":read-only", "on-request", "user")
	seen := map[string]bool{}
	for _, profile := range profiles {
		id := "profile:" + profile.ID
		if !profile.Allowed || strings.HasPrefix(profile.ID, ":") || profile.ID == "" || len(id) > 256 || strings.IndexFunc(id, unicode.IsControl) >= 0 || seen[id] {
			continue
		}
		seen[id] = true
		label := permissionText(s.agent.redactor.Redact(profile.ID), 120)
		description := permissionText(s.agent.redactor.Redact(profile.Description), 500)
		if label == "" {
			continue
		}
		if description == "" {
			description = "Configured permission profile. Keeps the current approval policy and reviewer."
		}
		choices = append(choices, permissionChoice{PermissionOption: protocol.PermissionOption{ID: id, Label: label, Description: description}, profile: profile.ID})
	}
	return choices, nil
}

func permissionPolicyAllowed(requirements codexadapter.PermissionRequirements, policy, reviewer string) bool {
	if requirements.ApprovalPolicies != nil {
		found := false
		for _, raw := range requirements.ApprovalPolicies {
			var allowed string
			if json.Unmarshal(raw, &allowed) == nil && allowed == policy {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return requirements.ApprovalsReviewers == nil || containsString(requirements.ApprovalsReviewers, reviewer)
}

func permissionText(text string, limit int) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return string(runes)
}

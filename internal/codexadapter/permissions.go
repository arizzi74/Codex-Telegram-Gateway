package codexadapter

import (
	"context"
	"encoding/json"
	"errors"
)

// PermissionProfile contains only menu metadata. The app-server resolves and
// enforces each profile; clients must not reconstruct managed permissions.
type PermissionProfile struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Allowed     bool   `json:"allowed"`
}

func (c *Client) ListPermissionProfiles(ctx context.Context, cwd string) ([]PermissionProfile, error) {
	var profiles []PermissionProfile
	cursor := ""
	for page := 0; page < 10; page++ {
		var reply struct {
			Data       []PermissionProfile `json:"data"`
			NextCursor string              `json:"nextCursor"`
		}
		params := map[string]any{"limit": 100, "cwd": cwd}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.request(ctx, "permissionProfile/list", params, &reply, false); err != nil {
			return nil, err
		}
		profiles = append(profiles, reply.Data...)
		if reply.NextCursor == "" {
			return profiles, nil
		}
		if reply.NextCursor == cursor {
			return nil, errors.New("permission profile pagination did not advance")
		}
		cursor = reply.NextCursor
	}
	return nil, errors.New("permission profile catalog is too large")
}

type PermissionRequirements struct {
	ApprovalPolicies   []json.RawMessage `json:"allowedApprovalPolicies"`
	ApprovalsReviewers []string          `json:"allowedApprovalsReviewers"`
}

func (c *Client) ReadPermissionRequirements(ctx context.Context) (PermissionRequirements, error) {
	var reply struct {
		Requirements PermissionRequirements `json:"requirements"`
	}
	if err := c.request(ctx, "configRequirements/read", map[string]any{}, &reply, false); err != nil {
		return PermissionRequirements{}, err
	}
	return reply.Requirements, nil
}

// FeatureEnabled reads enablement without changing config or loading a thread.
// A thread ID must identify an already loaded thread.
func (c *Client) FeatureEnabled(ctx context.Context, threadID, name string) (bool, error) {
	cursor := ""
	for page := 0; page < 10; page++ {
		var reply struct {
			Data []struct {
				Name    string `json:"name"`
				Enabled bool   `json:"enabled"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		params := map[string]any{"limit": 100}
		if threadID != "" {
			params["threadId"] = threadID
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.request(ctx, "experimentalFeature/list", params, &reply, false); err != nil {
			return false, err
		}
		for _, feature := range reply.Data {
			if feature.Name == name {
				return feature.Enabled, nil
			}
		}
		if reply.NextCursor == "" {
			return false, nil
		}
		if reply.NextCursor == cursor {
			return false, errors.New("feature pagination did not advance")
		}
		cursor = reply.NextCursor
	}
	return false, errors.New("feature catalog is too large")
}

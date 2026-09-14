package codexadapter

// This file contains the small, typed surface used by Telegram slash
// commands.  Keep it deliberately narrower than JSON-RPC: callers cannot
// choose a method name or pass arbitrary params.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type EffectiveConfig struct {
	Model              string
	ReasoningEffort    string
	ReasoningSummary   string
	ApprovalPolicy     string
	ApprovalsReviewer  string
	SandboxMode        string
	ServiceTier        string
	ModelContextWindow int64
}

func (c *Client) ReadEffectiveConfig(ctx context.Context, cwd string) (EffectiveConfig, error) {
	var reply struct {
		Config struct {
			Model              string          `json:"model"`
			ReasoningEffort    string          `json:"model_reasoning_effort"`
			ReasoningSummary   string          `json:"model_reasoning_summary"`
			ApprovalPolicy     json.RawMessage `json:"approval_policy"`
			ApprovalsReviewer  string          `json:"approvals_reviewer"`
			SandboxMode        string          `json:"sandbox_mode"`
			ServiceTier        string          `json:"service_tier"`
			ModelContextWindow int64           `json:"model_context_window"`
		} `json:"config"`
	}
	params := map[string]any{"includeLayers": false}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if err := c.request(ctx, "config/read", params, &reply, false); err != nil {
		return EffectiveConfig{}, err
	}
	approval := stringScalar(reply.Config.ApprovalPolicy)
	return EffectiveConfig{
		Model: reply.Config.Model, ReasoningEffort: reply.Config.ReasoningEffort,
		ReasoningSummary: reply.Config.ReasoningSummary, ApprovalPolicy: approval,
		ApprovalsReviewer: reply.Config.ApprovalsReviewer, SandboxMode: reply.Config.SandboxMode,
		ServiceTier: reply.Config.ServiceTier, ModelContextWindow: reply.Config.ModelContextWindow,
	}, nil
}

func stringScalar(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

type TokenUsage struct {
	LifetimeTokens      *int64
	PeakDailyTokens     *int64
	CurrentStreakDays   *int64
	LongestStreakDays   *int64
	LongestRunningTurn  *int64
	ThreadCreditsMicros *int64
	ThreadUSDMicros     *int64
}

func (c *Client) ReadTokenUsage(ctx context.Context, threadID string) (TokenUsage, error) {
	var reply struct {
		Summary struct {
			LifetimeTokens     *int64 `json:"lifetimeTokens"`
			PeakDailyTokens    *int64 `json:"peakDailyTokens"`
			CurrentStreakDays  *int64 `json:"currentStreakDays"`
			LongestStreakDays  *int64 `json:"longestStreakDays"`
			LongestRunningTurn *int64 `json:"longestRunningTurnSec"`
		} `json:"summary"`
		ThreadUsage *struct {
			CreditsMicros int64  `json:"estimatedUsageCreditsMicros"`
			USDMicros     *int64 `json:"estimatedUsageUsdMicros"`
		} `json:"threadUsage"`
	}
	params := map[string]any{}
	if threadID != "" {
		params["threadId"] = threadID
	}
	if err := c.request(ctx, "account/usage/read", params, &reply, false); err != nil {
		return TokenUsage{}, err
	}
	usage := TokenUsage{LifetimeTokens: reply.Summary.LifetimeTokens, PeakDailyTokens: reply.Summary.PeakDailyTokens, CurrentStreakDays: reply.Summary.CurrentStreakDays, LongestStreakDays: reply.Summary.LongestStreakDays, LongestRunningTurn: reply.Summary.LongestRunningTurn}
	if reply.ThreadUsage != nil {
		usage.ThreadCreditsMicros = &reply.ThreadUsage.CreditsMicros
		usage.ThreadUSDMicros = reply.ThreadUsage.USDMicros
	}
	return usage, nil
}

type RateLimitWindow struct {
	UsedPercent        int
	WindowDurationMins *int64
	ResetsAt           *int64
}

type RateLimits struct {
	PlanType             string
	OrdinaryUsageAllowed *bool
	Primary              *RateLimitWindow
	Secondary            *RateLimitWindow
}

func (c *Client) ReadRateLimits(ctx context.Context) (RateLimits, error) {
	type window struct {
		UsedPercent int    `json:"usedPercent"`
		Duration    *int64 `json:"windowDurationMins"`
		ResetsAt    *int64 `json:"resetsAt"`
	}
	var reply struct {
		OrdinaryUsageAllowed *bool `json:"ordinaryUsageAllowed"`
		RateLimits           struct {
			PlanType  string  `json:"planType"`
			Primary   *window `json:"primary"`
			Secondary *window `json:"secondary"`
		} `json:"rateLimits"`
	}
	if err := c.request(ctx, "account/rateLimits/read", map[string]any{"excludeResetCreditDetails": true}, &reply, false); err != nil {
		return RateLimits{}, err
	}
	result := RateLimits{PlanType: reply.RateLimits.PlanType, OrdinaryUsageAllowed: reply.OrdinaryUsageAllowed}
	if reply.RateLimits.Primary != nil {
		w := reply.RateLimits.Primary
		result.Primary = &RateLimitWindow{UsedPercent: w.UsedPercent, WindowDurationMins: w.Duration, ResetsAt: w.ResetsAt}
	}
	if reply.RateLimits.Secondary != nil {
		w := reply.RateLimits.Secondary
		result.Secondary = &RateLimitWindow{UsedPercent: w.UsedPercent, WindowDurationMins: w.Duration, ResetsAt: w.ResetsAt}
	}
	return result, nil
}

type ModelInfo struct {
	ID                     string
	Model                  string
	DisplayName            string
	Description            string
	DefaultReasoningEffort string
	ReasoningEfforts       []string
	ServiceTiers           []ServiceTierInfo
	SupportsPersonality    bool
	Default                bool
}

type ServiceTierInfo struct{ ID, Name, Description string }

func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var models []ModelInfo
	cursor := ""
	for len(models) < 200 {
		var reply struct {
			Data []struct {
				ID, Model, DisplayName, Description, DefaultReasoningEffort string
				IsDefault, SupportsPersonality                              bool
				SupportedReasoningEfforts                                   []struct {
					Effort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
				ServiceTiers []struct{ ID, Name, Description string } `json:"serviceTiers"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.request(ctx, "model/list", params, &reply, false); err != nil {
			return nil, err
		}
		for _, item := range reply.Data {
			model := ModelInfo{ID: item.ID, Model: item.Model, DisplayName: item.DisplayName, Description: item.Description, DefaultReasoningEffort: item.DefaultReasoningEffort, SupportsPersonality: item.SupportsPersonality, Default: item.IsDefault}
			for _, effort := range item.SupportedReasoningEfforts {
				model.ReasoningEfforts = append(model.ReasoningEfforts, effort.Effort)
			}
			for _, tier := range item.ServiceTiers {
				model.ServiceTiers = append(model.ServiceTiers, ServiceTierInfo{tier.ID, tier.Name, tier.Description})
			}
			models = append(models, model)
		}
		if reply.NextCursor == "" {
			break
		}
		cursor = reply.NextCursor
	}
	return models, nil
}

type ThreadSettingsUpdate struct {
	Model, Effort, Personality, ApprovalPolicy, PermissionProfile *string
	ServiceTier                                                   *string // nil omits; pointer to empty string clears the tier.
	SandboxMode                                                   string  // "read-only" or "workspace-write".
	PlanMode                                                      *bool
}

func (c *Client) UpdateThreadSettings(ctx context.Context, threadID string, update ThreadSettingsUpdate) error {
	if threadID == "" {
		return errors.New("thread id is required")
	}
	params := map[string]any{"threadId": threadID}
	put := func(key string, value *string) {
		if value != nil {
			params[key] = *value
		}
	}
	put("model", update.Model)
	put("effort", update.Effort)
	put("personality", update.Personality)
	put("approvalPolicy", update.ApprovalPolicy)
	put("permissions", update.PermissionProfile)
	if update.ServiceTier != nil {
		if *update.ServiceTier == "" {
			params["serviceTier"] = nil
		} else {
			params["serviceTier"] = *update.ServiceTier
		}
	}
	if update.SandboxMode != "" {
		switch update.SandboxMode {
		case "read-only":
			params["sandboxPolicy"] = map[string]any{"type": "readOnly", "networkAccess": false}
		case "workspace-write":
			params["sandboxPolicy"] = map[string]any{"type": "workspaceWrite", "networkAccess": false}
		default:
			return fmt.Errorf("unsupported sandbox mode %q", update.SandboxMode)
		}
	}
	if update.PlanMode != nil {
		mode := "default"
		if *update.PlanMode {
			mode = "plan"
		}
		settings := map[string]any{"model": ""}
		if update.Model != nil {
			settings["model"] = *update.Model
		}
		if update.Effort != nil {
			settings["reasoning_effort"] = *update.Effort
		}
		params["collaborationMode"] = map[string]any{"mode": mode, "settings": settings}
	}
	if len(params) == 1 {
		return errors.New("at least one thread setting is required")
	}
	return c.request(ctx, "thread/settings/update", params, nil, false)
}

func (c *Client) CompactThread(ctx context.Context, threadID string) error {
	if threadID == "" {
		return errors.New("thread id is required")
	}
	return c.request(ctx, "thread/compact/start", map[string]any{"threadId": threadID}, nil, false)
}

func (c *Client) RenameThread(ctx context.Context, threadID, name string) error {
	if threadID == "" || name == "" {
		return errors.New("thread id and name are required")
	}
	return c.request(ctx, "thread/name/set", map[string]any{"threadId": threadID, "name": name}, nil, false)
}

func (c *Client) ForkThread(ctx context.Context, threadID string) (Thread, error) {
	if threadID == "" {
		return Thread{}, errors.New("thread id is required")
	}
	var reply struct {
		Thread json.RawMessage `json:"thread"`
	}
	if err := c.request(ctx, "thread/fork", map[string]any{"threadId": threadID, "excludeTurns": true}, &reply, false); err != nil {
		return Thread{}, err
	}
	thread, err := decodeThread(reply.Thread)
	if err != nil {
		return Thread{}, fmt.Errorf("decode thread/fork thread: %w", err)
	}
	if thread.ID == "" {
		return Thread{}, errors.New("thread/fork response omitted thread id")
	}
	return thread, nil
}

func (c *Client) StartReview(ctx context.Context, threadID, instructions string) (Turn, error) {
	if threadID == "" {
		return Turn{}, errors.New("thread id is required")
	}
	target := map[string]any{"type": "uncommittedChanges"}
	if instructions != "" {
		target = map[string]any{"type": "custom", "instructions": instructions}
	}
	var reply struct {
		Turn json.RawMessage `json:"turn"`
	}
	if err := c.request(ctx, "review/start", map[string]any{"threadId": threadID, "target": target, "delivery": "inline"}, &reply, false); err != nil {
		return Turn{}, err
	}
	turn, err := decodeTurn(reply.Turn)
	if err != nil {
		return Turn{}, fmt.Errorf("decode review/start turn: %w", err)
	}
	if turn.ID == "" {
		return Turn{}, errors.New("review/start response omitted turn id")
	}
	turn.ThreadID = threadID
	return turn, nil
}

type Goal struct {
	Objective, Status           string
	TokenBudget                 *int64
	TokensUsed, TimeUsedSeconds int64
}

func (c *Client) ReadGoal(ctx context.Context, threadID string) (*Goal, error) {
	var reply struct {
		Goal *struct {
			Objective, Status           string
			TokenBudget                 *int64
			TokensUsed, TimeUsedSeconds int64
		} `json:"goal"`
	}
	if err := c.request(ctx, "thread/goal/get", map[string]any{"threadId": threadID}, &reply, false); err != nil {
		return nil, err
	}
	if reply.Goal == nil {
		return nil, nil
	}
	return &Goal{reply.Goal.Objective, reply.Goal.Status, reply.Goal.TokenBudget, reply.Goal.TokensUsed, reply.Goal.TimeUsedSeconds}, nil
}

func (c *Client) SetGoal(ctx context.Context, threadID, objective, status string) error {
	params := map[string]any{"threadId": threadID}
	if objective != "" {
		params["objective"] = objective
	}
	if status != "" {
		params["status"] = status
	}
	return c.request(ctx, "thread/goal/set", params, nil, false)
}

func (c *Client) ClearGoal(ctx context.Context, threadID string) error {
	return c.request(ctx, "thread/goal/clear", map[string]any{"threadId": threadID}, nil, false)
}

type MCPServer struct {
	Name, AuthStatus, RuntimeStatus string
	ToolCount, ResourceCount        int
}

func (c *Client) ListMCPServers(ctx context.Context, threadID string, verbose bool) ([]MCPServer, error) {
	detail := "toolsAndAuthOnly"
	if verbose {
		detail = "full"
	}
	var all []MCPServer
	cursor := ""
	for len(all) < 500 {
		var reply struct {
			Data []struct {
				Name, AuthStatus, RuntimeStatus string
				Tools                           map[string]json.RawMessage
				Resources                       []json.RawMessage
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		params := map[string]any{"detail": detail, "limit": 100}
		if threadID != "" {
			params["threadId"] = threadID
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.request(ctx, "mcpServerStatus/list", params, &reply, false); err != nil {
			return nil, err
		}
		for _, v := range reply.Data {
			all = append(all, MCPServer{v.Name, v.AuthStatus, v.RuntimeStatus, len(v.Tools), len(v.Resources)})
		}
		if reply.NextCursor == "" {
			break
		}
		cursor = reply.NextCursor
	}
	return all, nil
}

type AppInfo struct {
	ID, Name, Description string
	Enabled, Accessible   bool
	PluginNames           []string
}

func (c *Client) ListApps(ctx context.Context, threadID string) ([]AppInfo, error) {
	var reply struct {
		Apps []struct {
			ID, RuntimeName   string
			Enabled, Callable bool
		} `json:"apps"`
	}
	params := map[string]any{"forceRefresh": false}
	if threadID != "" {
		params["threadId"] = threadID
	}
	if err := c.request(ctx, "app/installed", params, &reply, false); err != nil {
		return nil, err
	}
	all := make([]AppInfo, 0, len(reply.Apps))
	for _, v := range reply.Apps {
		name := v.RuntimeName
		if name == "" {
			name = v.ID
		}
		all = append(all, AppInfo{ID: v.ID, Name: name, Enabled: v.Enabled, Accessible: v.Callable})
	}
	return all, nil
}

type SkillInfo struct {
	Name, Description, Scope string
	Enabled                  bool
}

func (c *Client) ListSkills(ctx context.Context, cwd string) ([]SkillInfo, []string, error) {
	var reply struct {
		Data []struct {
			Skills []struct {
				Name, Description, Scope string
				Enabled                  bool
			}
			Errors []struct{ Message string }
		} `json:"data"`
	}
	if err := c.request(ctx, "skills/list", map[string]any{"cwds": []string{cwd}, "forceReload": false}, &reply, false); err != nil {
		return nil, nil, err
	}
	var skills []SkillInfo
	var problems []string
	for _, entry := range reply.Data {
		for _, v := range entry.Skills {
			skills = append(skills, SkillInfo{v.Name, v.Description, v.Scope, v.Enabled})
		}
		for _, e := range entry.Errors {
			problems = append(problems, e.Message)
		}
	}
	return skills, problems, nil
}

type BackgroundTerminal struct {
	ProcessID, ItemID, Command, CWD string
	OSPID                           *uint32
	CPUPercent                      *float64
	RSSKB                           *uint64
}

func (c *Client) ListBackgroundTerminals(ctx context.Context, threadID string) ([]BackgroundTerminal, error) {
	var all []BackgroundTerminal
	cursor := ""
	for len(all) < 500 {
		var reply struct {
			Data []struct {
				ProcessID, ItemID, Command, CWD string
				OSPID                           *uint32  `json:"osPid"`
				CPUPercent                      *float64 `json:"cpuPercent"`
				RSSKB                           *uint64  `json:"rssKb"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		params := map[string]any{"threadId": threadID, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.request(ctx, "thread/backgroundTerminals/list", params, &reply, false); err != nil {
			return nil, err
		}
		for _, v := range reply.Data {
			all = append(all, BackgroundTerminal{v.ProcessID, v.ItemID, v.Command, v.CWD, v.OSPID, v.CPUPercent, v.RSSKB})
		}
		if reply.NextCursor == "" {
			break
		}
		cursor = reply.NextCursor
	}
	return all, nil
}

func (c *Client) CleanBackgroundTerminals(ctx context.Context, threadID string) error {
	return c.request(ctx, "thread/backgroundTerminals/clean", map[string]any{"threadId": threadID}, nil, false)
}

func (c *Client) ArchiveThread(ctx context.Context, threadID string) error {
	return c.request(ctx, "thread/archive", map[string]any{"threadId": threadID}, nil, false)
}

type HookInfo struct {
	Key, Event, Source, Trust string
	Enabled                   bool
}

func (c *Client) ListHooks(ctx context.Context, cwd string) ([]HookInfo, []string, error) {
	var reply struct {
		Data []struct {
			Hooks []struct {
				Key, EventName, Source, TrustStatus string
				Enabled                             bool
			} `json:"hooks"`
			Errors   []struct{ Message string } `json:"errors"`
			Warnings []string                   `json:"warnings"`
		} `json:"data"`
	}
	if err := c.request(ctx, "hooks/list", map[string]any{"cwds": []string{cwd}}, &reply, false); err != nil {
		return nil, nil, err
	}
	var hooks []HookInfo
	var problems []string
	for _, entry := range reply.Data {
		for _, h := range entry.Hooks {
			hooks = append(hooks, HookInfo{h.Key, h.EventName, h.Source, h.TrustStatus, h.Enabled})
		}
		for _, problem := range entry.Errors {
			problems = append(problems, problem.Message)
		}
		problems = append(problems, entry.Warnings...)
	}
	return hooks, problems, nil
}

type PluginInfo struct {
	ID, Name, Marketplace string
	Installed, Enabled    bool
}

func (c *Client) ListPlugins(ctx context.Context, cwd string) ([]PluginInfo, []string, error) {
	var reply struct {
		Marketplaces []struct {
			Name    string
			Plugins []struct {
				ID, Name           string
				Installed, Enabled bool
			} `json:"plugins"`
		} `json:"marketplaces"`
		Errors []struct{ Message string } `json:"marketplaceLoadErrors"`
	}
	if err := c.request(ctx, "plugin/list", map[string]any{"cwds": []string{cwd}, "forceRefetch": false}, &reply, false); err != nil {
		return nil, nil, err
	}
	var plugins []PluginInfo
	for _, marketplace := range reply.Marketplaces {
		for _, plugin := range marketplace.Plugins {
			plugins = append(plugins, PluginInfo{plugin.ID, plugin.Name, marketplace.Name, plugin.Installed, plugin.Enabled})
		}
	}
	var problems []string
	for _, problem := range reply.Errors {
		problems = append(problems, problem.Message)
	}
	return plugins, problems, nil
}

func (c *Client) SetMemoryMode(ctx context.Context, threadID, mode string) error {
	if mode != "enabled" && mode != "disabled" {
		return fmt.Errorf("invalid memory mode %q", mode)
	}
	return c.request(ctx, "thread/memoryMode/set", map[string]any{"threadId": threadID, "mode": mode}, nil, false)
}

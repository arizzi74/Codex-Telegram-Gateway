package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

const initPrompt = `Generate a file named AGENTS.md that serves as a contributor guide for this repository.
Before writing, check whether AGENTS.md already exists in the current working directory. If it does, do not overwrite or modify it.
Your goal is to produce a clear, concise, and well-structured document with descriptive headings and actionable explanations for each section.
Follow the outline below, but adapt as needed — add sections if relevant, and omit those that do not apply to this project.

Document Requirements

- Title the document "Repository Guidelines".
- Use Markdown headings (#, ##, etc.) for structure.
- Keep the document concise. 200-400 words is optimal.
- Keep explanations short, direct, and specific to this repository.
- Provide examples where helpful (commands, directory paths, naming patterns).
- Maintain a professional, instructional tone.

Recommended Sections

Project Structure & Module Organization

- Outline the project structure, including where the source code, tests, and assets are located.

Build, Test, and Development Commands

- List key commands for building, testing, and running locally (e.g., npm test, make build).
- Briefly explain what each command does.

Coding Style & Naming Conventions

- Specify indentation rules, language-specific style preferences, and naming patterns.
- Include any formatting or linting tools used.

Testing Guidelines

- Identify testing frameworks and coverage requirements.
- State test naming conventions and how to run tests.

Commit & Pull Request Guidelines

- Summarize commit message conventions found in the project’s Git history.
- Outline pull request requirements (descriptions, linked issues, screenshots, etc.).

(Optional) Add other sections if relevant, such as Security & Configuration Tips, Architecture Overview, or Agent-Specific Instructions.`

func (s *sessionActor) executeCodexCommand(ctx context.Context, client *codexadapter.Client, name, args string) (protocol.Result, error) {
	name, args = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(args)
	switch name {
	case "status":
		text, err := s.codexStatus(ctx, client)
		return protocol.Result{Text: text}, err
	case "usage":
		text, err := codexUsage(ctx, client, s.session.ThreadID)
		return protocol.Result{Text: text}, err
	case "model":
		text, err := s.codexModel(ctx, client, args)
		return protocol.Result{Text: text}, err
	case "reasoning":
		text, err := s.codexReasoning(ctx, client, args)
		return protocol.Result{Text: text}, err
	case "permissions":
		text, err := s.codexPermissions(ctx, client, args)
		return protocol.Result{Text: text}, err
	case "approvals":
		text, err := s.codexApprovals(ctx, client, args)
		return protocol.Result{Text: text}, err
	case "fast":
		text, err := s.codexFast(ctx, client, args)
		return protocol.Result{Text: text}, err
	case "plan":
		text, err := s.codexPlan(ctx, client, args)
		return protocol.Result{Text: text}, err
	case "personality":
		text, err := s.codexPersonality(ctx, client, args)
		return protocol.Result{Text: text}, err
	case "compact":
		if args != "" {
			return protocol.Result{}, usageError("compact")
		}
		if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
			return protocol.Result{}, err
		}
		if err := client.CompactThread(ctx, s.session.ThreadID); err != nil {
			return protocol.Result{}, err
		}
		return protocol.Result{Text: "Compaction started. Codex will report when it finishes.", State: "running"}, nil
	case "review":
		if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
			return protocol.Result{}, err
		}
		turn, err := client.StartReview(ctx, s.session.ThreadID, args)
		if err != nil {
			return protocol.Result{}, err
		}
		s.markSpecialTurn(turn.ID)
		return protocol.Result{Text: "Review started.", TurnID: turn.ID, State: "running"}, nil
	case "rename":
		if args == "" {
			return protocol.Result{}, validationError("usage: /rename NAME")
		}
		if len([]rune(args)) > 120 {
			return protocol.Result{}, validationError("thread name is too long (maximum 120 characters)")
		}
		if err := client.RenameThread(ctx, s.session.ThreadID, args); err != nil {
			return protocol.Result{}, err
		}
		s.session.Name = args
		s.save()
		return protocol.Result{Text: "Thread renamed to “" + args + "”."}, nil
	case "fork":
		if args != "" {
			return protocol.Result{}, usageError("fork")
		}
		return s.codexFork(ctx, client)
	case "goal":
		text, err := s.codexGoal(ctx, client, args)
		return protocol.Result{Text: text}, err
	case "diff":
		if args != "" {
			return protocol.Result{}, usageError("diff")
		}
		text, err := s.workspaceDiff(ctx)
		return protocol.Result{Text: text}, err
	case "init":
		if args != "" {
			return protocol.Result{}, usageError("init")
		}
		if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
			return protocol.Result{}, err
		}
		turn, err := client.StartTurn(ctx, s.session.ThreadID, initPrompt)
		if err != nil {
			return protocol.Result{}, err
		}
		s.markSpecialTurn(turn.ID)
		return protocol.Result{Text: "Codex is preparing AGENTS.md.", TurnID: turn.ID, State: "running"}, nil
	case "mcp":
		threadID := ""
		if s.session.Loaded {
			threadID = s.session.ThreadID
		}
		text, err := codexMCP(ctx, client, threadID, args)
		return protocol.Result{Text: text}, err
	case "apps":
		threadID := ""
		if s.session.Loaded {
			threadID = s.session.ThreadID
		}
		text, err := codexApps(ctx, client, threadID, args)
		return protocol.Result{Text: text}, err
	case "skills":
		text, err := codexSkills(ctx, client, s.session.CWD, args)
		return protocol.Result{Text: text}, err
	case "plugins":
		text, err := codexPlugins(ctx, client, s.session.CWD, args)
		return protocol.Result{Text: text}, err
	case "hooks":
		text, err := codexHooks(ctx, client, s.session.CWD, args)
		return protocol.Result{Text: text}, err
	case "memories":
		text, err := s.codexMemories(ctx, client, args)
		return protocol.Result{Text: text}, err
	case "ps":
		if !s.session.Loaded {
			return protocol.Result{Text: "No background terminals are running; this thread is not loaded."}, nil
		}
		text, err := codexPS(ctx, client, s.session.ThreadID, args)
		return protocol.Result{Text: text}, err
	case "stop", "clean":
		if args != "" {
			return protocol.Result{}, usageError(name)
		}
		if !s.session.Loaded {
			return protocol.Result{Text: "No background terminals to stop; this thread is not loaded."}, nil
		}
		if err := client.CleanBackgroundTerminals(ctx, s.session.ThreadID); err != nil {
			return protocol.Result{}, err
		}
		return protocol.Result{Text: "Stopped this thread’s background terminals."}, nil
	case "archive":
		if args != "" {
			return protocol.Result{}, usageError("archive")
		}
		if err := client.ArchiveThread(ctx, s.session.ThreadID); err != nil {
			return protocol.Result{}, err
		}
		s.session.Archived, s.session.Loaded, s.session.State = true, false, "not_loaded"
		s.save()
		if err := s.agent.emit(s.runtime, s.session.ID, "session_state_changed", s.session); err != nil {
			return protocol.Result{}, err
		}
		return protocol.Result{Text: "Thread archived."}, nil
	case "debug-config":
		cfg, err := client.ReadEffectiveConfig(ctx, s.session.CWD)
		if err != nil {
			return protocol.Result{}, err
		}
		return protocol.Result{Text: formatConfig(cfg)}, nil
	case "copy":
		text, err := codexLastResponse(ctx, client, s.session.ThreadID, args)
		return protocol.Result{Text: text}, err
	case "approve":
		return protocol.Result{Text: terminalGuidance(name, "retrying an automatic approval review requires the original denial event")}, nil
	case "delete":
		return protocol.Result{Text: "Use `/tgdeletesession` to choose and delete a Codex session while keeping its working directory. Use `/archive` for reversible archival."}, nil
	case "side", "btw":
		return protocol.Result{Text: terminalGuidance(name, "side conversations need the interactive agent-thread picker")}, nil
	case "mention":
		return protocol.Result{Text: terminalGuidance(name, "file mentions need the interactive file picker")}, nil
	case "rollout":
		return protocol.Result{Text: terminalGuidance(name, "transcript filesystem paths stay local to the worker")}, nil
	case "app", "ide", "import", "logout", "feedback", "experimental":
		return protocol.Result{Text: terminalGuidance(name, "this command changes or opens a local Codex integration")}, nil
	case "raw", "keymap", "vim", "statusline", "title", "theme", "pets", "pet", "setup-default-sandbox", "sandbox-add-read-dir":
		return protocol.Result{Text: terminalGuidance(name, "this setting belongs to the interactive terminal UI")}, nil
	default:
		return protocol.Result{}, validationError("unsupported Codex command %q", name)
	}
}

// CodexCommandValidationError is a definite local rejection. It is safe for
// Agent.executionError to report it without marking the thread outcome
// unknown, because no RPC was sent for the rejected command.
type CodexCommandValidationError struct{ Message string }

func (e *CodexCommandValidationError) Error() string { return e.Message }
func validationError(format string, args ...any) error {
	return &CodexCommandValidationError{Message: fmt.Sprintf(format, args...)}
}
func usageError(name string) error { return validationError("/%s does not accept arguments", name) }

func (s *sessionActor) ensureCodexThreadLoaded(ctx context.Context, client *codexadapter.Client) error {
	if _, err := canonicalWorkspace(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots); err != nil {
		return validationError("session workspace is not allowed")
	}
	if s.session.ActiveTurnID != "" || s.session.State == "running" || s.session.State == "waiting_approval" || s.session.State == "waiting_input" {
		return validationError("this thread has an active turn")
	}
	if s.session.Loaded && s.session.State != "not_loaded" {
		return nil
	}
	thread, err := client.ResumeThread(ctx, s.session.ThreadID, codexadapter.ThreadOptions{})
	if err != nil {
		return err
	}
	if thread.ID != s.session.ThreadID {
		return errors.New("Codex resumed a different thread")
	}
	s.session.Loaded = true
	if thread.ActiveTurnID != "" || thread.Status == "active" || thread.Status == "running" {
		s.session.ActiveTurnID, s.session.State = thread.ActiveTurnID, "running"
		s.save()
		return fmt.Errorf("%w: resumed thread already has an active turn", codexadapter.ErrStaleTurn)
	}
	s.session.State = "idle"
	s.save()
	return nil
}

func (s *sessionActor) markSpecialTurn(turnID string) {
	s.session.ActiveTurnID, s.session.State, s.session.Loaded = turnID, "running", true
	s.resetMessages()
	s.save()
}

func (s *sessionActor) codexFork(ctx context.Context, client *codexadapter.Client) (protocol.Result, error) {
	thread, err := client.ForkThread(ctx, s.session.ThreadID)
	if err != nil {
		return protocol.Result{}, err
	}
	cwd, err := canonicalWorkspace(thread.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil {
		return protocol.Result{}, fmt.Errorf("forked thread workspace is not allowed: %w", err)
	}
	thread.CWD = cwd
	created, err := s.agent.store.UpsertSession(sessionFromThread(s.runtime, thread, true))
	if err != nil {
		return protocol.Result{}, err
	}
	if created.Name == "" {
		created.Name = "Fork of " + displaySessionName(s.session)
	}
	if s.agent.actorForThread(s.runtime.ID, created.ThreadID) == nil {
		if err := s.agent.emit(s.runtime, created.ID, "session_discovered", created); err != nil {
			return protocol.Result{}, err
		}
	}
	s.agent.onSession(s.runtime, created)
	return protocol.Result{Text: "Forked into “" + displaySessionName(created) + "”.", Session: &created}, nil
}

func displaySessionName(session protocol.Session) string {
	if strings.TrimSpace(session.Name) != "" {
		return session.Name
	}
	if len(session.ThreadID) > 12 {
		return session.ThreadID[:12]
	}
	return session.ThreadID
}

func (s *sessionActor) codexStatus(ctx context.Context, client *codexadapter.Client) (string, error) {
	thread, err := client.ReadThread(ctx, s.session.ThreadID, false)
	if err != nil {
		return "", err
	}
	cfg, err := client.ReadEffectiveConfig(ctx, s.session.CWD)
	if err != nil {
		return "", err
	}
	model := thread.Model
	if model == "" {
		model = cfg.Model
	}
	effort := threadReasoningEffort(thread)
	if effort == "" {
		effort = cfg.ReasoningEffort
	}
	lines := []string{"Codex session", "Model: " + valueOr(model, "default"), "Reasoning: " + valueOr(effort, "default"), "Workspace: " + s.session.CWD}
	lines = append(lines, s.rolloutStatus(client, thread)...)
	if cfg.ApprovalPolicy != "" {
		lines = append(lines, "Configured approval policy: "+cfg.ApprovalPolicy)
	} else {
		lines = append(lines, "Approval policy: unavailable in the read-only thread snapshot")
	}
	if cfg.SandboxMode != "" {
		lines = append(lines, "Configured sandbox: "+cfg.SandboxMode)
	} else {
		lines = append(lines, "Sandbox: unavailable in the read-only thread snapshot")
	}
	if cfg.ServiceTier != "" {
		lines = append(lines, "Service tier: "+cfg.ServiceTier)
	}
	if cfg.ModelContextWindow > 0 {
		lines = append(lines, "Context window: "+formatInt(cfg.ModelContextWindow)+" tokens")
	}
	if usage, usageErr := client.ReadTokenUsage(ctx, s.session.ThreadID); usageErr == nil {
		lines = append(lines, tokenUsageLines(usage)...)
	} else {
		lines = append(lines, "Token usage: unavailable ("+shortError(usageErr)+")")
	}
	if limits, limitsErr := client.ReadRateLimits(ctx); limitsErr == nil {
		lines = append(lines, rateLimitLines(limits)...)
	}
	return strings.Join(lines, "\n"), nil
}

func threadReasoningEffort(thread codexadapter.Thread) string {
	var wire struct {
		ReasoningEffort string `json:"reasoningEffort"`
	}
	_ = json.Unmarshal(thread.Raw, &wire)
	return wire.ReasoningEffort
}

func (s *sessionActor) codexModel(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	models, err := client.ListModels(ctx)
	if err != nil {
		return "", err
	}
	if args == "" {
		lines := []string{"Available models:"}
		for _, m := range models {
			marker := ""
			if m.Default {
				marker = " (default)"
			}
			efforts := strings.Join(m.ReasoningEfforts, ", ")
			if efforts != "" {
				efforts = " — reasoning: " + efforts
			}
			lines = append(lines, "• "+m.ID+marker+efforts)
		}
		return strings.Join(lines, "\n"), nil
	}
	fields := strings.Fields(args)
	if len(fields) > 2 {
		return "", validationError("usage: /model MODEL [EFFORT]")
	}
	var selected *codexadapter.ModelInfo
	for i := range models {
		if fields[0] == models[i].ID || fields[0] == models[i].Model {
			selected = &models[i]
			break
		}
	}
	if selected == nil {
		return "", validationError("unknown model %q; run /model to list available models", fields[0])
	}
	selectedModel := selected.Model
	if selectedModel == "" {
		selectedModel = selected.ID
	}
	update := codexadapter.ThreadSettingsUpdate{Model: &selectedModel}
	if len(fields) == 2 {
		if !containsString(selected.ReasoningEfforts, fields[1]) {
			return "", validationError("model %s does not offer reasoning effort %q", fields[0], fields[1])
		}
		update.Effort = &fields[1]
	}
	if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
		return "", err
	}
	if err := client.UpdateThreadSettings(ctx, s.session.ThreadID, update); err != nil {
		return "", err
	}
	text := "Model set to " + selectedModel + "."
	if len(fields) == 2 {
		text = "Model set to " + selectedModel + " with " + fields[1] + " reasoning."
	}
	return text, nil
}

func (s *sessionActor) codexReasoning(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	thread, err := client.ReadThread(ctx, s.session.ThreadID, false)
	if err != nil {
		return "", err
	}
	if args == "" {
		return "Reasoning effort: " + valueOr(threadReasoningEffort(thread), "default"), nil
	}
	if len(strings.Fields(args)) != 1 {
		return "", validationError("usage: /reasoning EFFORT")
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return "", err
	}
	allowed := false
	for _, m := range models {
		if (m.ID == thread.Model || m.Model == thread.Model) && containsString(m.ReasoningEfforts, args) {
			allowed = true
		}
	}
	if !allowed {
		return "", validationError("reasoning effort %q is not available for model %s", args, valueOr(thread.Model, "current"))
	}
	if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
		return "", err
	}
	if err := client.UpdateThreadSettings(ctx, s.session.ThreadID, codexadapter.ThreadSettingsUpdate{Effort: &args}); err != nil {
		return "", err
	}
	return "Reasoning effort set to " + args + ".", nil
}

func (s *sessionActor) codexPermissions(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	if args == "" {
		cfg, err := client.ReadEffectiveConfig(ctx, s.session.CWD)
		if err != nil {
			return "", err
		}
		return "Configured sandbox: " + valueOr(cfg.SandboxMode, "unavailable") + "\nConfigured approval policy: " + valueOr(cfg.ApprovalPolicy, "unavailable"), nil
	}
	mode := strings.ToLower(args)
	if mode == "readonly" {
		mode = "read-only"
	}
	if mode == "workspace" {
		mode = "workspace-write"
	}
	if mode != "read-only" && mode != "workspace-write" {
		return "", validationError("usage: /permissions [read-only|workspace-write]")
	}
	if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
		return "", err
	}
	if err := client.UpdateThreadSettings(ctx, s.session.ThreadID, codexadapter.ThreadSettingsUpdate{SandboxMode: mode}); err != nil {
		return "", err
	}
	return "Session sandbox set to " + mode + " (network access remains disabled).", nil
}

func (s *sessionActor) codexApprovals(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	if args == "" {
		cfg, err := client.ReadEffectiveConfig(ctx, s.session.CWD)
		if err != nil {
			return "", err
		}
		return "Configured approval policy: " + valueOr(cfg.ApprovalPolicy, "unavailable"), nil
	}
	policy := strings.ToLower(args)
	if policy != "untrusted" && policy != "on-request" && policy != "never" {
		return "", validationError("usage: /approvals [untrusted|on-request|never]")
	}
	if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
		return "", err
	}
	if err := client.UpdateThreadSettings(ctx, s.session.ThreadID, codexadapter.ThreadSettingsUpdate{ApprovalPolicy: &policy}); err != nil {
		return "", err
	}
	return "Approval policy set to " + policy + ".", nil
}

func (s *sessionActor) codexFast(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	if args == "" || args == "status" {
		cfg, err := client.ReadEffectiveConfig(ctx, s.session.CWD)
		if err != nil {
			return "", err
		}
		return "Configured service tier: " + valueOr(cfg.ServiceTier, "default"), nil
	}
	if args != "on" && args != "off" {
		return "", validationError("usage: /fast [status|on|off]")
	}
	if args == "on" {
		thread, err := client.ReadThread(ctx, s.session.ThreadID, false)
		if err != nil {
			return "", err
		}
		models, err := client.ListModels(ctx)
		if err != nil {
			return "", err
		}
		supported := false
		for _, model := range models {
			if model.ID != thread.Model && model.Model != thread.Model {
				continue
			}
			for _, tier := range model.ServiceTiers {
				if tier.ID == "fast" {
					supported = true
				}
			}
		}
		if !supported {
			return "", validationError("model %s does not offer the fast service tier", valueOr(thread.Model, "current"))
		}
	}
	if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
		return "", err
	}
	tier := "fast"
	if args == "off" {
		tier = ""
	}
	if err := client.UpdateThreadSettings(ctx, s.session.ThreadID, codexadapter.ThreadSettingsUpdate{ServiceTier: &tier}); err != nil {
		return "", err
	}
	if args == "on" {
		return "Fast service tier enabled for subsequent turns.", nil
	}
	return "Fast service tier disabled for subsequent turns.", nil
}

func (s *sessionActor) codexPlan(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	if args == "" {
		args = "on"
	}
	on := args == "on" || args == "plan"
	if !on && args != "off" && args != "default" {
		return "", validationError("usage: /plan on|off")
	}
	if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
		return "", err
	}
	thread, err := client.ReadThread(ctx, s.session.ThreadID, false)
	if err != nil {
		return "", err
	}
	update := codexadapter.ThreadSettingsUpdate{PlanMode: &on, Model: &thread.Model}
	if effort := threadReasoningEffort(thread); effort != "" {
		update.Effort = &effort
	}
	if err := client.UpdateThreadSettings(ctx, s.session.ThreadID, update); err != nil {
		return "", err
	}
	if on {
		return "Plan mode enabled for subsequent turns.", nil
	}
	return "Plan mode disabled.", nil
}

func (s *sessionActor) codexPersonality(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	if args == "" {
		return "Available personalities: friendly, pragmatic, none", nil
	}
	value := strings.ToLower(args)
	if value != "friendly" && value != "pragmatic" && value != "none" {
		return "", validationError("usage: /personality [friendly|pragmatic|none]")
	}
	if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
		return "", err
	}
	if err := client.UpdateThreadSettings(ctx, s.session.ThreadID, codexadapter.ThreadSettingsUpdate{Personality: &value}); err != nil {
		return "", err
	}
	return "Personality set to " + value + ".", nil
}

func (s *sessionActor) codexGoal(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	if args == "" {
		goal, err := client.ReadGoal(ctx, s.session.ThreadID)
		if err != nil {
			return "", err
		}
		if goal == nil {
			return "No goal is set for this thread.", nil
		}
		lines := []string{"Goal: " + goal.Objective, "Status: " + goal.Status, "Tokens used: " + formatInt(goal.TokensUsed), "Time used: " + (time.Duration(goal.TimeUsedSeconds) * time.Second).String()}
		if goal.TokenBudget != nil {
			lines = append(lines, "Token budget: "+formatInt(*goal.TokenBudget))
		}
		return strings.Join(lines, "\n"), nil
	}
	lower := strings.ToLower(args)
	if lower == "clear" {
		if err := client.ClearGoal(ctx, s.session.ThreadID); err != nil {
			return "", err
		}
		return "Goal cleared.", nil
	}
	statuses := map[string]string{"pause": "paused", "paused": "paused", "resume": "active", "active": "active", "complete": "complete", "blocked": "blocked"}
	if status, ok := statuses[lower]; ok {
		if err := client.SetGoal(ctx, s.session.ThreadID, "", status); err != nil {
			return "", err
		}
		return "Goal status set to " + status + ".", nil
	}
	if err := client.SetGoal(ctx, s.session.ThreadID, args, "active"); err != nil {
		return "", err
	}
	return "Goal set.", nil
}

func (s *sessionActor) codexMemories(ctx context.Context, client *codexadapter.Client, args string) (string, error) {
	if args == "" {
		return "Usage: /memories enabled|disabled", nil
	}
	mode := strings.ToLower(args)
	if mode == "on" {
		mode = "enabled"
	}
	if mode == "off" {
		mode = "disabled"
	}
	if mode != "enabled" && mode != "disabled" {
		return "", validationError("usage: /memories enabled|disabled")
	}
	if err := s.ensureCodexThreadLoaded(ctx, client); err != nil {
		return "", err
	}
	if err := client.SetMemoryMode(ctx, s.session.ThreadID, mode); err != nil {
		return "", err
	}
	return "Memory mode set to " + mode + ".", nil
}

func codexUsage(ctx context.Context, client *codexadapter.Client, threadID string) (string, error) {
	usage, err := client.ReadTokenUsage(ctx, threadID)
	if err != nil {
		return "", err
	}
	limits, limitErr := client.ReadRateLimits(ctx)
	lines := append([]string{"Codex usage"}, tokenUsageLines(usage)...)
	if limitErr == nil {
		lines = append(lines, rateLimitLines(limits)...)
	} else {
		lines = append(lines, "Rate limits: unavailable ("+shortError(limitErr)+")")
	}
	return strings.Join(lines, "\n"), nil
}
func tokenUsageLines(u codexadapter.TokenUsage) []string {
	var lines []string
	if u.LifetimeTokens != nil {
		lines = append(lines, "Lifetime tokens: "+formatInt(*u.LifetimeTokens))
	}
	if u.PeakDailyTokens != nil {
		lines = append(lines, "Peak daily tokens: "+formatInt(*u.PeakDailyTokens))
	}
	if u.ThreadCreditsMicros != nil {
		lines = append(lines, fmt.Sprintf("Thread credits: %.4f", float64(*u.ThreadCreditsMicros)/1e6))
	}
	if len(lines) == 0 {
		lines = append(lines, "Token usage: no totals reported")
	}
	return lines
}
func rateLimitLines(r codexadapter.RateLimits) []string {
	var lines []string
	if r.PlanType != "" {
		lines = append(lines, "Plan: "+r.PlanType)
	}
	if r.Primary != nil {
		lines = append(lines, fmt.Sprintf("Primary limit used: %d%%", r.Primary.UsedPercent))
	}
	if r.Secondary != nil {
		lines = append(lines, fmt.Sprintf("Secondary limit used: %d%%", r.Secondary.UsedPercent))
	}
	return lines
}

func codexMCP(ctx context.Context, client *codexadapter.Client, threadID, args string) (string, error) {
	verbose := args == "verbose"
	if args != "" && !verbose {
		return "", validationError("usage: /mcp [verbose]")
	}
	servers, err := client.ListMCPServers(ctx, threadID, verbose)
	if err != nil {
		return "", err
	}
	if len(servers) == 0 {
		if threadID == "" {
			return "No MCP servers are configured globally. The selected thread is not loaded, so no thread runtime was started.", nil
		}
		return "No MCP servers are configured.", nil
	}
	header := "MCP servers:"
	if threadID == "" {
		header = "MCP servers (global configuration; selected thread not loaded):"
	}
	lines := []string{header}
	for _, v := range servers {
		status := valueOr(v.RuntimeStatus, "not started")
		lines = append(lines, fmt.Sprintf("• %s — %s, auth %s, %d tools", v.Name, status, v.AuthStatus, v.ToolCount))
	}
	return strings.Join(lines, "\n"), nil
}
func codexApps(ctx context.Context, client *codexadapter.Client, threadID, args string) (string, error) {
	if args != "" {
		return "", usageError("apps")
	}
	apps, err := client.ListApps(ctx, threadID)
	if err != nil {
		return "", err
	}
	if len(apps) == 0 {
		if threadID == "" {
			return "No apps are available globally. The selected thread is not loaded, so no thread runtime was started.", nil
		}
		return "No apps are available.", nil
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	shown := apps
	if len(shown) > 50 {
		shown = shown[:50]
	}
	header := fmt.Sprintf("Apps (%d total, showing %d):", len(apps), len(shown))
	if threadID == "" {
		header = fmt.Sprintf("Apps (global configuration; %d total, showing %d; selected thread not loaded):", len(apps), len(shown))
	}
	lines := []string{header}
	for _, v := range shown {
		state := "disabled"
		if v.Enabled {
			state = "enabled"
		}
		if v.Accessible {
			state += ", callable"
		}
		lines = append(lines, "• "+v.Name+" — "+state)
	}
	return strings.Join(lines, "\n"), nil
}
func codexSkills(ctx context.Context, client *codexadapter.Client, cwd, args string) (string, error) {
	if args != "" {
		return "", usageError("skills")
	}
	skills, problems, err := client.ListSkills(ctx, cwd)
	if err != nil {
		return "", err
	}
	lines := []string{"Skills:"}
	for _, v := range skills {
		state := "disabled"
		if v.Enabled {
			state = "enabled"
		}
		lines = append(lines, "• "+v.Name+" ("+v.Scope+", "+state+")")
	}
	if len(skills) == 0 {
		lines = append(lines, "No skills found.")
	}
	for _, p := range problems {
		lines = append(lines, "Warning: "+p)
	}
	return strings.Join(lines, "\n"), nil
}
func codexPlugins(ctx context.Context, client *codexadapter.Client, cwd, args string) (string, error) {
	plugins, problems, err := client.ListPlugins(ctx, cwd)
	if err != nil {
		return "", err
	}
	query := strings.ToLower(strings.TrimSpace(args))
	if query != "" {
		filtered := plugins[:0]
		for _, plugin := range plugins {
			haystack := strings.ToLower(plugin.ID + " " + plugin.Name + " " + plugin.Marketplace)
			if strings.Contains(haystack, query) {
				filtered = append(filtered, plugin)
			}
		}
		plugins = filtered
	}
	sort.SliceStable(plugins, func(i, j int) bool {
		if plugins[i].Installed != plugins[j].Installed {
			return plugins[i].Installed
		}
		return strings.ToLower(plugins[i].Name) < strings.ToLower(plugins[j].Name)
	})
	shown := plugins
	if len(shown) > 50 {
		shown = shown[:50]
	}
	header := fmt.Sprintf("Plugins (%d matches, showing %d):", len(plugins), len(shown))
	if query == "" {
		header = fmt.Sprintf("Plugins (%d total, showing %d; use /plugins SEARCH to filter):", len(plugins), len(shown))
	}
	lines := []string{header}
	for _, v := range shown {
		state := "available"
		if v.Installed {
			state = "installed"
		}
		if !v.Enabled {
			state += ", disabled"
		}
		lines = append(lines, "• "+v.Name+" — "+state)
	}
	if len(plugins) == 0 {
		lines = append(lines, "No plugins found.")
	}
	for _, p := range problems {
		lines = append(lines, "Warning: "+p)
	}
	return strings.Join(lines, "\n"), nil
}
func codexHooks(ctx context.Context, client *codexadapter.Client, cwd, args string) (string, error) {
	if args != "" {
		return "", usageError("hooks")
	}
	hooks, problems, err := client.ListHooks(ctx, cwd)
	if err != nil {
		return "", err
	}
	lines := []string{"Hooks:"}
	for _, v := range hooks {
		state := "disabled"
		if v.Enabled {
			state = "enabled"
		}
		lines = append(lines, "• "+v.Key+" — "+v.Event+", "+state)
	}
	if len(hooks) == 0 {
		lines = append(lines, "No hooks configured.")
	}
	for _, p := range problems {
		lines = append(lines, "Warning: "+p)
	}
	return strings.Join(lines, "\n"), nil
}
func codexPS(ctx context.Context, client *codexadapter.Client, threadID, args string) (string, error) {
	if args != "" {
		return "", usageError("ps")
	}
	terms, err := client.ListBackgroundTerminals(ctx, threadID)
	if err != nil {
		return "", err
	}
	if len(terms) == 0 {
		return "No background terminals are running for this thread.", nil
	}
	lines := []string{"Background terminals:"}
	for _, v := range terms {
		pid := ""
		if v.OSPID != nil {
			pid = fmt.Sprintf(" pid=%d", *v.OSPID)
		}
		lines = append(lines, "• "+v.ProcessID+pid+" — "+v.Command)
	}
	return strings.Join(lines, "\n"), nil
}

func (s *sessionActor) workspaceDiff(ctx context.Context) (string, error) {
	cwd, err := canonicalWorkspace(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil {
		return "", err
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = cwd
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
		return string(output), nil
	}
	unstaged, err := run("--no-pager", "diff", "--no-ext-diff", "--no-textconv")
	if err != nil {
		return "", err
	}
	staged, err := run("--no-pager", "diff", "--cached", "--no-ext-diff", "--no-textconv")
	if err != nil {
		return "", err
	}
	untracked, err := run("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return "", err
	}
	parts := []string{}
	if staged != "" {
		parts = append(parts, "Staged changes:\n"+staged)
	}
	if unstaged != "" {
		parts = append(parts, "Unstaged changes:\n"+unstaged)
	}
	if untracked != "" {
		parts = append(parts, "Untracked files:\n"+untracked)
	}
	if len(parts) == 0 {
		return "Working tree is clean.", nil
	}
	text := strings.Join(parts, "\n")
	if len(text) > 40000 {
		text = text[:40000] + "\n… diff truncated; run /diff in a local Codex terminal for the full output."
	}
	return text, nil
}

func codexLastResponse(ctx context.Context, client *codexadapter.Client, threadID, args string) (string, error) {
	if args != "" {
		return "", usageError("copy")
	}
	text, err := client.LastAgentResponse(ctx, threadID)
	if err != nil {
		return "", err
	}
	if text != "" {
		return text, nil
	}
	return "No agent response is available in this thread snapshot.", nil
}

func formatConfig(c codexadapter.EffectiveConfig) string {
	return strings.Join([]string{"Effective Codex configuration", "Model: " + valueOr(c.Model, "default"), "Reasoning: " + valueOr(c.ReasoningEffort, "default"), "Approval policy: " + valueOr(c.ApprovalPolicy, "default"), "Approvals reviewer: " + valueOr(c.ApprovalsReviewer, "default"), "Sandbox: " + valueOr(c.SandboxMode, "default"), "Service tier: " + valueOr(c.ServiceTier, "default")}, "\n")
}
func terminalGuidance(name, reason string) string {
	return fmt.Sprintf("/%s is available in the local Codex terminal because %s. Open this thread locally and run /%s there.", name, reason, name)
}
func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func shortError(err error) string {
	if errors.Is(err, codexadapter.ErrMethodUnavailable) {
		return "unsupported by this Codex version"
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	if len(text) > 120 {
		text = text[:120] + "…"
	}
	return text
}
func formatInt(value int64) string {
	negative := value < 0
	if negative {
		value = -value
	}
	raw := strconv.FormatInt(value, 10)
	for i := len(raw) - 3; i > 0; i -= 3 {
		raw = raw[:i] + "," + raw[i:]
	}
	if negative {
		return "-" + raw
	}
	return raw
}

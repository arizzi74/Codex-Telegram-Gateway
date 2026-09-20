package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/iaia/telegramgw/internal/telegramcommands"
)

const callbackLifetime = 15 * time.Minute

const (
	sessionPageSize         = 10
	sessionKeyboardMaxBytes = 4096
)

type telegramRenderStore interface {
	ListWorkers(context.Context) ([]registry.Worker, error)
	TelegramSessionStatus(context.Context, uuid.UUID) (registry.SessionStatus, error)
	PendingApproval(context.Context, uuid.UUID) (protocol.Approval, error)
}

type renderInventory struct {
	workers       []registry.Worker
	runtimes      []protocol.Runtime
	workerByID    map[string]registry.Worker
	runtimeByID   map[string]protocol.Runtime
	sessionByID   map[string]protocol.Session
	sessionsByRun map[string][]protocol.Session
}

func (s *Sender) render(ctx context.Context, row registry.Delivery) (string, *TelegramKeyboard, error) {
	var text string
	var keyboard *TelegramKeyboard
	var err error
	if row.Kind == "ui_response" {
		text, keyboard, err = s.renderUIResponse(ctx, row)
	} else {
		text, keyboard, err = s.renderEvent(ctx, row)
	}
	if err != nil {
		return "", nil, err
	}
	if s.options.Redactor != nil {
		text = s.options.Redactor.Redact(text)
		if keyboard != nil {
			for i := range keyboard.Rows {
				for j := range keyboard.Rows[i] {
					keyboard.Rows[i][j].Text = s.options.Redactor.Redact(keyboard.Rows[i][j].Text)
				}
			}
		}
	}
	return text, keyboard, nil
}

func (s *Sender) renderUIResponse(ctx context.Context, row registry.Delivery) (string, *TelegramKeyboard, error) {
	var response registry.AcceptResult
	if err := json.Unmarshal(row.Payload, &response); err != nil {
		return "", nil, fmt.Errorf("render Telegram UI response: %w", err)
	}
	if sessionWizardView(response.View) || (response.View == "runtime_picker" && response.WizardID != "") {
		var text string
		var keyboard *TelegramKeyboard
		var err error
		if response.View == "runtime_picker" {
			text, keyboard, err = s.renderWizardRuntimePicker(ctx, row, response)
		} else {
			text, keyboard, err = s.renderSessionWizard(ctx, row, response)
		}
		if err == nil && response.ErrorCode != "" {
			text = telegramErrorText(response.ErrorCode) + "\n\n" + text
		}
		return text, keyboard, err
	}
	if response.ErrorCode != "" || response.View == "error" {
		return telegramErrorText(response.ErrorCode), nil, nil
	}
	switch response.View {
	case "help", "start":
		return helpText(), nil, nil
	case "codex_help":
		return codexHelpText(), nil, nil
	case "multisession":
		return s.renderMultiSession(ctx, row, response)
	case "questions":
		return s.renderPendingQuestions(ctx, row, response)
	case "approval_prompt":
		return s.renderPendingApproval(ctx, row, response)
	case "instances":
		inventory, err := s.inventory(ctx)
		if err != nil {
			return "", nil, err
		}
		return s.renderInstances(ctx, row, inventory)
	case "runtime_picker":
		if response.WizardID != "" {
			return s.renderWizardRuntimePicker(ctx, row, response)
		}
		if response.Action != "sessions" && response.Action != "new" {
			return "", nil, errors.New("render Telegram runtime picker: missing action")
		}
		inventory, err := s.inventory(ctx)
		if err != nil {
			return "", nil, err
		}
		return s.renderRuntimePicker(ctx, row, response.Action, inventory)
	case "sessions":
		runtimeID, err := requiredUUID("runtime", response.RuntimeID)
		if err != nil {
			return "", nil, err
		}
		inventory, err := s.inventory(ctx)
		if err != nil {
			return "", nil, err
		}
		return s.renderSessions(ctx, row, runtimeID, inventory, response.SessionPage)
	case "selected":
		_, session, runtime, worker, err := s.selectedIdentity(ctx, response.SessionID, response.RuntimeID)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("Connected to %s\n\nHost: %s\nRuntime: %s\nWorkspace: %s\nStatus: %s\n\nMessages in this chat now target this session.", sessionLabel(session), workerLabel(worker), runtimeLabel(runtime), displayValue(session.CWD), titleCase(session.State)), nil, nil
	case "status":
		return s.renderStatus(ctx, response)
	case "disconnected":
		return "Selection cleared. Running runtimes and sessions were not changed.", nil, nil
	case "queued":
		return s.renderQueued(ctx, response)
	case "input_pending", "input_prompt":
		return s.renderPendingInput(ctx, row, response)
	case "new":
		return "Creating a new session.", nil, nil
	case "":
		return "Updated.", nil, nil
	default:
		return "Updated: " + humanize(response.View) + ".", nil, nil
	}
}

func (s *Sender) renderInstances(ctx context.Context, row registry.Delivery, inv renderInventory) (string, *TelegramKeyboard, error) {
	if len(inv.workers) == 0 {
		return "No workers are registered.", nil, nil
	}
	var text strings.Builder
	text.WriteString("Workers and runtimes")
	keyboard := &TelegramKeyboard{}
	for _, worker := range inv.workers {
		text.WriteString("\n\n" + connectivityIcon(worker.Connectivity) + " " + workerLabel(worker) + " · " + displayValue(worker.Connectivity))
		found := false
		for _, runtime := range inv.runtimes {
			if runtime.WorkerID != worker.ID.String() {
				continue
			}
			found = true
			sessions, running := inv.sessionsByRun[runtime.ID], 0
			for _, session := range sessions {
				if session.ActiveTurnID != "" || strings.EqualFold(session.State, "running") {
					running++
				}
			}
			fmt.Fprintf(&text, "\n  %s · %s · %d sessions · %d running", runtimeLabel(runtime), displayValue(runtime.State), len(sessions), running)
			runtimeID, err := requiredUUID("runtime", runtime.ID)
			if err != nil {
				return "", nil, err
			}
			token, err := s.callback(ctx, row, registry.Callback{Action: "sessions", RuntimeID: runtimeID, Generation: int64(runtime.Generation)})
			if err != nil {
				return "", nil, fmt.Errorf("render instances callback: %w", err)
			}
			keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: "Sessions · " + workerLabel(worker) + " / " + runtimeLabel(runtime), Data: token}})
		}
		if !found {
			text.WriteString("\n  No runtimes")
		}
	}
	if len(keyboard.Rows) == 0 {
		keyboard = nil
	}
	return text.String(), keyboard, nil
}

func (s *Sender) renderRuntimePicker(ctx context.Context, row registry.Delivery, action string, inv renderInventory) (string, *TelegramKeyboard, error) {
	if len(inv.runtimes) == 0 {
		return "No runtimes are available.", nil, nil
	}
	keyboard := &TelegramKeyboard{}
	for _, runtime := range inv.runtimes {
		worker := inv.workerByID[runtime.WorkerID]
		runtimeID, err := requiredUUID("runtime", runtime.ID)
		if err != nil {
			return "", nil, err
		}
		token, err := s.callback(ctx, row, registry.Callback{Action: action, RuntimeID: runtimeID, Generation: int64(runtime.Generation)})
		if err != nil {
			return "", nil, fmt.Errorf("render runtime picker callback: %w", err)
		}
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: workerLabel(worker) + " / " + runtimeLabel(runtime), Data: token}})
	}
	verb := "list sessions"
	if action == "new" {
		verb = "create the session"
	}
	return "Choose a runtime to " + verb + ".", keyboard, nil
}

func (s *Sender) renderSessions(ctx context.Context, row registry.Delivery, runtimeID uuid.UUID, inv renderInventory, page int) (string, *TelegramKeyboard, error) {
	return s.renderSessionList(ctx, row, runtimeID, inv, page, nil)
}

func (s *Sender) renderSessionList(ctx context.Context, row registry.Delivery, runtimeID uuid.UUID, inv renderInventory, page int, wizard *registry.AcceptResult) (string, *TelegramKeyboard, error) {
	if page < 0 {
		return "", nil, errors.New("render sessions: invalid page")
	}
	runtime, ok := inv.runtimeByID[runtimeID.String()]
	if !ok {
		return "", nil, errors.New("render sessions: runtime is unavailable")
	}
	worker := inv.workerByID[runtime.WorkerID]
	sessions := append([]protocol.Session(nil), inv.sessionsByRun[runtime.ID]...)
	// Activity updates must not shuffle sessions between pages. Names and IDs
	// give the picker a stable order even while turns and heartbeats continue.
	sort.Slice(sessions, func(i, j int) bool {
		left, right := strings.ToLower(sessionLabel(sessions[i])), strings.ToLower(sessionLabel(sessions[j]))
		if left != right {
			return left < right
		}
		return sessions[i].ID < sessions[j].ID
	})
	pages := max(1, (len(sessions)+sessionPageSize-1)/sessionPageSize)
	page = min(page, pages-1)
	start := page * sessionPageSize
	end := min(start+sessionPageSize, len(sessions))
	var text strings.Builder
	title := "Sessions · "
	if wizard != nil {
		title = "Delete a session · "
	}
	text.WriteString(title + s.sessionListField(workerLabel(worker), 64, 256) + " / " + s.sessionListField(runtimeLabel(runtime), 64, 256))
	fmt.Fprintf(&text, "\nPage %d of %d · %d sessions", page+1, pages, len(sessions))
	keyboard := &TelegramKeyboard{}
	if len(sessions) == 0 {
		text.WriteString("\n\nNo sessions found.")
	}
	for index, session := range sessions[start:end] {
		number := start + index + 1
		label := s.sessionListLabel(session)
		fmt.Fprintf(&text, "\n\n%d. %s %s\n%s · %s\n%s", number, sessionIcon(session), label, s.sessionListField(titleCase(session.State), 24, 96), sessionAvailability(session), s.sessionListField(displayValue(session.CWD), 160, 640))
		sessionID, err := requiredUUID("session", session.ID)
		if err != nil {
			return "", nil, err
		}
		if wizard != nil {
			token, err := s.sessionWizardCallback(ctx, row, *wizard, registry.Callback{Action: "delete_session_pick", SessionID: sessionID, RuntimeID: runtimeID, Generation: int64(runtime.Generation)})
			if err != nil {
				return "", nil, err
			}
			keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: fmt.Sprintf("Delete %d", number), Data: token}})
			continue
		}
		connect, err := s.callback(ctx, row, registry.Callback{Action: "select", SessionID: sessionID, RuntimeID: runtimeID, Generation: int64(runtime.Generation)})
		if err != nil {
			return "", nil, fmt.Errorf("render session connect callback: %w", err)
		}
		status, err := s.callback(ctx, row, registry.Callback{Action: "status", SessionID: sessionID, RuntimeID: runtimeID, Generation: int64(runtime.Generation)})
		if err != nil {
			return "", nil, fmt.Errorf("render session status callback: %w", err)
		}
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: fmt.Sprintf("Connect %d", number), Data: connect}, {Text: fmt.Sprintf("Status %d", number), Data: status}})
	}
	var navigation []TelegramButton
	for _, target := range []struct {
		label string
		page  int
	}{{"‹ Previous", page - 1}, {"Next ›", page + 1}} {
		if target.page < 0 || target.page >= pages {
			continue
		}
		var token string
		var err error
		callback := registry.Callback{Action: "sessions", RuntimeID: runtimeID, Generation: int64(runtime.Generation), SessionPage: target.page}
		if wizard != nil {
			callback.Action = "delete_sessions"
			token, err = s.sessionWizardCallback(ctx, row, *wizard, callback)
		} else {
			token, err = s.callback(ctx, row, callback)
		}
		if err != nil {
			return "", nil, fmt.Errorf("render session page callback: %w", err)
		}
		navigation = append(navigation, TelegramButton{Text: target.label, Data: token})
	}
	if len(navigation) > 0 {
		keyboard.Rows = append(keyboard.Rows, navigation)
	}
	if wizard != nil {
		text.WriteString("\n\nSelect a session to delete its Codex conversation. Its working directory and files will be kept.")
		token, err := s.sessionWizardCallback(ctx, row, *wizard, registry.Callback{Action: "wizard_cancel"})
		if err != nil {
			return "", nil, err
		}
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: "Cancel", Data: token}})
		return text.String(), keyboard, nil
	}
	create, err := s.callback(ctx, row, registry.Callback{Action: "new", RuntimeID: runtimeID, Generation: int64(runtime.Generation)})
	if err != nil {
		return "", nil, fmt.Errorf("render new session callback: %w", err)
	}
	keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: "New session", Data: create}})
	return text.String(), keyboard, nil
}

// Bound secondary fields independently. Full session names belong in the
// message body; ordinary delivery splitting handles pages longer than a message.
// Redact before truncation to avoid exposing part of a configured secret.
func (s *Sender) sessionListField(value string, maxUnits, maxBytes int) string {
	if s.options.Redactor != nil {
		value = s.options.Redactor.Redact(value)
	}
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= maxBytes && telegramTextLength(value) <= maxUnits {
		return value
	}
	units, size := 0, 0
	for index, r := range value {
		next := len(string(r))
		if units+utf16.RuneLen(r) > maxUnits-1 || size+next > maxBytes-len("…") {
			return value[:index] + "…"
		}
		units += utf16.RuneLen(r)
		size += next
	}
	return value
}

func (s *Sender) sessionListLabel(session protocol.Session) string {
	if s.options.Redactor != nil {
		session.Name = s.options.Redactor.Redact(session.Name)
		session.Preview = s.options.Redactor.Redact(session.Preview)
		session.CWD = s.options.Redactor.Redact(session.CWD)
	}
	// This view has numbered controls, so neither names nor preview-based
	// labels need the compact label used by buttons elsewhere.
	for _, value := range []string{session.Name, session.Preview} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return sessionLabel(session)
}

func (s *Sender) renderStatus(ctx context.Context, response registry.AcceptResult) (string, *TelegramKeyboard, error) {
	_, session, runtime, worker, err := s.selectedIdentity(ctx, response.SessionID, response.RuntimeID)
	if err != nil {
		return "", nil, err
	}
	store, ok := s.store.(telegramRenderStore)
	if !ok {
		return "", nil, errors.New("render status: registry does not provide status reads")
	}
	sessionID, err := requiredUUID("session", session.ID)
	if err != nil {
		return "", nil, err
	}
	status, err := store.TelegramSessionStatus(ctx, sessionID)
	if err != nil {
		return "", nil, fmt.Errorf("render status snapshot: %w", err)
	}
	if status.SessionID != session.ID || status.RuntimeID != runtime.ID {
		return "", nil, errors.New("render status: snapshot target mismatch")
	}
	activeTurn := "none"
	if session.ActiveTurnID != "" {
		activeTurn = shortID(session.ActiveTurnID)
	}
	lastEvent := "none"
	if status.LastEventAt != nil {
		lastEvent = status.LastEventAt.UTC().Format(time.RFC3339)
	}
	branch := session.GitBranch
	if branch == "" {
		branch = "none"
	}
	text := fmt.Sprintf("Status · %s\n\nWorker: %s\nRuntime: %s\nRuntime generation: %d\nSession: %s\nCodex thread: %s\nWorkspace: %s\nGit branch: %s\nConnectivity: %s\nSession state: %s\nActive turn: %s\nPending approvals: %d\nQueued commands: %d\nLast event: %s", humanIdentity(worker, runtime, session), workerLabel(worker), runtimeLabel(runtime), runtime.Generation, sessionLabel(session), shortID(session.ThreadID), displayValue(session.CWD), branch, displayValue(worker.Connectivity), displayValue(session.State), activeTurn, status.PendingApprovals, status.QueuedCommands, lastEvent)
	return text, nil, nil
}

func (s *Sender) renderQueued(ctx context.Context, response registry.AcceptResult) (string, *TelegramKeyboard, error) {
	if response.SessionID != "" {
		_, session, _, _, err := s.selectedIdentity(ctx, response.SessionID, response.RuntimeID)
		if err != nil {
			return "", nil, err
		}
		return "Queued for " + sessionLabel(session) + ".", nil, nil
	}
	if response.RuntimeID != "" {
		inv, err := s.inventory(ctx)
		if err != nil {
			return "", nil, err
		}
		runtime, ok := inv.runtimeByID[response.RuntimeID]
		if !ok {
			return "", nil, errors.New("render queued command: runtime is unavailable")
		}
		return "New session queued on " + runtimeLabel(runtime) + ".", nil, nil
	}
	return "Command queued.", nil, nil
}

func (s *Sender) renderPendingInput(ctx context.Context, row registry.Delivery, response registry.AcceptResult) (string, *TelegramKeyboard, error) {
	approvalID, err := requiredUUID("approval", response.ApprovalID)
	if err != nil {
		return "", nil, err
	}
	if response.QuestionID == "" {
		return "", nil, errors.New("render pending input: missing question ID")
	}
	store, ok := s.store.(telegramRenderStore)
	if !ok {
		return "", nil, errors.New("render pending input: registry does not provide pending approvals")
	}
	approval, err := store.PendingApproval(ctx, approvalID)
	if err != nil {
		if errors.Is(err, registry.ErrTelegramTarget) {
			return "This input request is no longer pending.", nil, nil
		}
		return "", nil, fmt.Errorf("render pending input request: %w", err)
	}
	question, ok := findQuestion(approval.Questions, response.QuestionID)
	if !ok {
		return "", nil, errors.New("render pending input: question is unavailable")
	}
	_, session, runtime, worker, err := s.selectedIdentity(ctx, response.SessionID, response.RuntimeID)
	if err != nil {
		return "", nil, err
	}
	return s.renderQuestion(ctx, row, humanIdentity(worker, runtime, session), session, runtime, runtime.Generation, approvalID, approval, question)
}

func (s *Sender) renderEvent(ctx context.Context, row registry.Delivery) (string, *TelegramKeyboard, error) {
	var event protocol.Event
	if err := json.Unmarshal(row.Payload, &event); err != nil {
		return "", nil, fmt.Errorf("render Telegram event: %w", err)
	}
	if event.Kind == "" || event.Kind != row.Kind {
		return "", nil, errors.New("render Telegram event: delivery kind mismatch")
	}
	identity := "Codex"
	var session protocol.Session
	var runtime protocol.Runtime
	if event.SessionID != "" {
		_, ss, rt, worker, err := s.selectedIdentity(ctx, event.SessionID, event.RuntimeID)
		if err != nil {
			return "", nil, err
		}
		session, runtime, identity = ss, rt, humanIdentity(worker, rt, ss)
	} else if event.RuntimeID != "" {
		if _, err := requiredUUID("runtime", event.RuntimeID); err != nil {
			return "", nil, err
		}
		inv, err := s.inventory(ctx)
		if err != nil {
			return "", nil, err
		}
		var ok bool
		runtime, ok = inv.runtimeByID[event.RuntimeID]
		if !ok {
			return "", nil, errors.New("render Telegram event: runtime is unavailable")
		}
		identity = workerLabel(inv.workerByID[runtime.WorkerID]) + " / " + runtimeLabel(runtime)
	}
	switch event.Kind {
	case "user_message":
		var result protocol.Result
		if err := json.Unmarshal(event.Data, &result); err != nil {
			return "", nil, fmt.Errorf("render user prompt: %w", err)
		}
		if strings.TrimSpace(result.Text) == "" {
			return "", nil, errors.New("render user prompt: missing message")
		}
		return "👤 You · Codex\n\n" + result.Text, nil, nil
	case "approval_requested":
		var approval protocol.Approval
		if err := json.Unmarshal(event.Data, &approval); err != nil {
			return "", nil, fmt.Errorf("render approval request: %w", err)
		}
		return s.renderApproval(ctx, row, identity+"\nSession: "+s.sessionListLabel(session), event, approval)
	case "user_input_requested":
		var approval protocol.Approval
		if err := json.Unmarshal(event.Data, &approval); err != nil {
			return "", nil, fmt.Errorf("render input request: %w", err)
		}
		if len(approval.Questions) == 0 {
			return "", nil, errors.New("render input request: no questions")
		}
		approvalID, err := requiredUUID("approval", approval.ID)
		if err != nil {
			return "", nil, err
		}
		return s.renderQuestion(ctx, row, identity, session, runtime, event.RuntimeGeneration, approvalID, approval, approval.Questions[0])
	case "agent_progress_message", "tool_progress_message":
		var result protocol.Result
		if err := json.Unmarshal(event.Data, &result); err != nil {
			return "", nil, fmt.Errorf("render progress output: %w", err)
		}
		if result.TurnID == "" || strings.TrimSpace(result.Text) == "" {
			return "", nil, errors.New("render progress output: missing turn or message")
		}
		if event.Kind == "tool_progress_message" {
			return "🔧 " + identity + "\n\n" + strings.TrimSpace(result.Text), nil, nil
		}
		return "⏳ " + identity + "\n\n" + strings.TrimSpace(result.Text), nil, nil
	case "final_agent_message", "turn_completed":
		var result protocol.Result
		if err := json.Unmarshal(event.Data, &result); err != nil {
			return "", nil, fmt.Errorf("render final output: %w", err)
		}
		body := strings.TrimSpace(result.Text)
		if body == "" {
			body = "Turn completed."
		}
		return "✅ " + identity + "\n\n" + body, nil, nil
	case "turn_interrupted":
		return "⏹ " + identity + "\n\nTurn interrupted.", nil, nil
	case "turn_failed", "runtime_failed", "command_failed":
		var result protocol.Result
		if err := json.Unmarshal(event.Data, &result); err != nil {
			return "", nil, fmt.Errorf("render failure: %w", err)
		}
		message := "The turn failed."
		if event.Kind == "runtime_failed" {
			message = "The runtime stopped unexpectedly."
		} else if event.Kind == "command_failed" {
			message = "The command failed."
		}
		if result.Error != nil && strings.TrimSpace(result.Error.Message) != "" {
			message += "\n\n" + result.Error.Message
			if result.Error.Code != "" {
				message += " (" + result.Error.Code + ")"
			}
		}
		return "❌ " + identity + "\n\n" + message, nil, nil
	case "runtime_degraded":
		var degraded protocol.Runtime
		if err := json.Unmarshal(event.Data, &degraded); err != nil {
			return "", nil, fmt.Errorf("render degraded runtime: %w", err)
		}
		version := "unknown"
		if degraded.CodexVersion != "" {
			version = degraded.CodexVersion
		}
		return "⚠️ " + identity + "\n\nRuntime compatibility is degraded. Some Codex features may be unavailable.\nState: " + displayValue(degraded.State) + "\nCodex version: " + version + "\n\nUse /tgstatus and check the worker logs before retrying failed operations.", nil, nil
	case "command_completed":
		var result protocol.Result
		if err := json.Unmarshal(event.Data, &result); err != nil {
			return "", nil, fmt.Errorf("render completed command: %w", err)
		}
		if result.History != nil {
			parts, keyboard, err := s.renderHistory(ctx, row, event, result.History, result.CommandID)
			return strings.Join(parts, "\n\n"), keyboard, err
		}
		if result.Permissions != nil {
			return s.renderPermissions(ctx, row, identity, event, result)
		}
		if result.Session == nil || result.Session.ID == event.SessionID {
			if strings.TrimSpace(result.Text) != "" {
				return identity + "\n\n" + result.Text, nil, nil
			}
			return "✅ " + identity + "\n\nCommand completed.", nil, nil
		}
		_, created, createdRuntime, createdWorker, err := s.selectedIdentity(ctx, result.Session.ID, result.Session.RuntimeID)
		if err != nil {
			return "", nil, err
		}
		return "✅ New session ready · " + humanIdentity(createdWorker, createdRuntime, created) + "\n\nWorkspace: " + displayValue(created.CWD) + "\nStatus: " + titleCase(created.State) + "\n\nReply to this message to use this session, or select it with /tgsessions.", nil, nil
	case "command_result_unknown":
		return "⚠️ " + identity + "\n\nThe command outcome is unknown. Use /tgstatus before retrying.", nil, nil
	default:
		return titleCase(event.Kind) + " · " + identity, nil, nil
	}
}

func (s *Sender) renderApproval(ctx context.Context, row registry.Delivery, identity string, event protocol.Event, approval protocol.Approval) (string, *TelegramKeyboard, error) {
	if len(approval.Decisions) == 0 {
		return "⚠️ " + identity + "\n\nCodex requested approval without a supported decision. Open the local Codex terminal to review this request.\n" + approval.Summary, nil, nil
	}
	approvalID, err := requiredUUID("approval", approval.ID)
	if err != nil {
		return "", nil, err
	}
	sessionID, err := requiredUUID("session", event.SessionID)
	if err != nil {
		return "", nil, err
	}
	runtimeID, err := requiredUUID("runtime", event.RuntimeID)
	if err != nil {
		return "", nil, err
	}
	text := "⚠️ Approval required · " + identity + "\n\nAction: " + humanize(approval.Type)
	if strings.TrimSpace(approval.Summary) != "" {
		text += "\n" + approval.Summary
	}
	keyboard := &TelegramKeyboard{}
	for _, decision := range approval.Decisions {
		if strings.TrimSpace(decision) == "" {
			return "", nil, errors.New("render approval request: empty decision")
		}
		token, err := s.callback(ctx, row, registry.Callback{Action: "approval", SessionID: sessionID, RuntimeID: runtimeID, ApprovalID: approvalID, Generation: int64(event.RuntimeGeneration), Decision: decision})
		if err != nil {
			return "", nil, fmt.Errorf("render approval callback: %w", err)
		}
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: decisionLabel(decision), Data: token}})
	}
	return text, keyboard, nil
}

func (s *Sender) renderQuestion(ctx context.Context, row registry.Delivery, identity string, session protocol.Session, runtime protocol.Runtime, generation uint64, approvalID uuid.UUID, approval protocol.Approval, question protocol.Question) (string, *TelegramKeyboard, error) {
	if question.ID == "" || strings.TrimSpace(question.Prompt) == "" {
		return "", nil, errors.New("render input request: invalid question")
	}
	sessionID, err := requiredUUID("session", session.ID)
	if err != nil {
		return "", nil, err
	}
	runtimeID, err := requiredUUID("runtime", runtime.ID)
	if err != nil {
		return "", nil, err
	}
	heading := strings.TrimSpace(question.Header)
	if heading == "" {
		heading = "Input requested"
	}
	text := "❓ " + s.sessionListLabel(session) + "\n" + heading + " · " + identity + "\n\n" + question.Prompt
	if len(approval.Questions) > 1 {
		for i, candidate := range approval.Questions {
			if candidate.ID == question.ID {
				text += fmt.Sprintf("\n\nQuestion %d of %d", i+1, len(approval.Questions))
				break
			}
		}
	}
	keyboard := &TelegramKeyboard{}
	for _, option := range question.Options {
		if strings.TrimSpace(option) == "" {
			return "", nil, errors.New("render input request: empty option")
		}
		token, err := s.callback(ctx, row, registry.Callback{Action: "input", SessionID: sessionID, RuntimeID: runtimeID, ApprovalID: approvalID, Generation: int64(generation), QuestionID: question.ID, Answer: option})
		if err != nil {
			return "", nil, fmt.Errorf("render input option callback: %w", err)
		}
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: option, Data: token}})
	}
	token, err := s.callback(ctx, row, registry.Callback{Action: "input_prompt", SessionID: sessionID, RuntimeID: runtimeID, ApprovalID: approvalID, Generation: int64(generation), QuestionID: question.ID})
	if err != nil {
		return "", nil, fmt.Errorf("render input reply callback: %w", err)
	}
	keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: "Reply with text", Data: token}})
	if approval.Async {
		token, err := s.callback(ctx, row, registry.Callback{Action: "dismiss_input", SessionID: sessionID, RuntimeID: runtimeID, ApprovalID: approvalID, Generation: int64(generation)})
		if err != nil {
			return "", nil, fmt.Errorf("render input dismissal callback: %w", err)
		}
		keyboard.Rows = append(keyboard.Rows, []TelegramButton{{Text: "Dismiss question", Data: token}})
		text += "\n\nYou can dismiss this pending question without sending an answer."
	}
	text += "\n\nReply to this message with your answer, or tap Reply with text for a fresh prompt. Your answer goes to this session and keeps your current session selected."
	return text, keyboard, nil
}

func (s *Sender) selectedIdentity(ctx context.Context, sessionText, runtimeText string) (renderInventory, protocol.Session, protocol.Runtime, registry.Worker, error) {
	sessionID, err := requiredUUID("session", sessionText)
	if err != nil {
		return renderInventory{}, protocol.Session{}, protocol.Runtime{}, registry.Worker{}, err
	}
	inv, err := s.inventory(ctx)
	if err != nil {
		return renderInventory{}, protocol.Session{}, protocol.Runtime{}, registry.Worker{}, err
	}
	session, ok := inv.sessionByID[sessionID.String()]
	if !ok {
		return inv, session, protocol.Runtime{}, registry.Worker{}, errors.New("render Telegram identity: session is unavailable")
	}
	if runtimeText != "" && session.RuntimeID != runtimeText {
		return inv, session, protocol.Runtime{}, registry.Worker{}, errors.New("render Telegram identity: runtime target mismatch")
	}
	runtime, ok := inv.runtimeByID[session.RuntimeID]
	if !ok {
		return inv, session, runtime, registry.Worker{}, errors.New("render Telegram identity: runtime is unavailable")
	}
	worker, ok := inv.workerByID[runtime.WorkerID]
	if !ok {
		return inv, session, runtime, worker, errors.New("render Telegram identity: worker is unavailable")
	}
	return inv, session, runtime, worker, nil
}

func (s *Sender) inventory(ctx context.Context) (renderInventory, error) {
	store, ok := s.store.(telegramRenderStore)
	if !ok {
		return renderInventory{}, errors.New("render Telegram inventory: registry does not provide worker reads")
	}
	workers, err := store.ListWorkers(ctx)
	if err != nil {
		return renderInventory{}, fmt.Errorf("render worker snapshot: %w", err)
	}
	runtimes, err := s.store.RuntimeSnapshot(ctx)
	if err != nil {
		return renderInventory{}, fmt.Errorf("render runtime snapshot: %w", err)
	}
	sessions, err := s.store.SessionSnapshot(ctx)
	if err != nil {
		return renderInventory{}, fmt.Errorf("render session snapshot: %w", err)
	}
	inv := renderInventory{workers: workers, runtimes: runtimes, workerByID: map[string]registry.Worker{}, runtimeByID: map[string]protocol.Runtime{}, sessionByID: map[string]protocol.Session{}, sessionsByRun: map[string][]protocol.Session{}}
	for _, worker := range workers {
		inv.workerByID[worker.ID.String()] = worker
	}
	for _, runtime := range runtimes {
		inv.runtimeByID[runtime.ID] = runtime
	}
	for _, session := range sessions {
		if session.Archived {
			continue
		}
		inv.sessionByID[session.ID] = session
		inv.sessionsByRun[session.RuntimeID] = append(inv.sessionsByRun[session.RuntimeID], session)
	}
	return inv, nil
}

func (s *Sender) callback(ctx context.Context, row registry.Delivery, callback registry.Callback) (string, error) {
	if callback.UserID == 0 && row.Kind == "ui_response" {
		var response registry.AcceptResult
		if json.Unmarshal(row.Payload, &response) == nil {
			callback.UserID = response.UserID
		}
	}
	if callback.UserID == 0 {
		callback.UserID = s.options.OwnerID
	}
	if s.options.BotID == "" || callback.UserID == 0 {
		return "", errors.New("render Telegram callback: sender identity is not configured")
	}
	callback.BotID, callback.ChatID, callback.TopicID = s.options.BotID, row.ChatID, row.TopicID
	callback.ExpiresAt = time.Now().Add(callbackLifetime)
	token, err := s.store.CreateCallback(ctx, callback)
	if err != nil {
		return "", err
	}
	if token == "" || len("cb:"+token) > 64 {
		return "", errors.New("render Telegram callback: invalid opaque token")
	}
	return "cb:" + token, nil
}

func requiredUUID(kind, value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("render Telegram %s: invalid identity", kind)
	}
	return id, nil
}
func findQuestion(questions []protocol.Question, id string) (protocol.Question, bool) {
	for _, question := range questions {
		if question.ID == id {
			return question, true
		}
	}
	return protocol.Question{}, false
}

func helpText() string {
	return "Gateway commands:\n/tgstart — getting started\n/tghelp — show this guide\n/tginstances — list workers and runtimes\n/tgsessions — list, select, or create sessions\n/tgstatus — show gateway session and queue state\n/tghistory [count] — show saved Codex prompts\n/tglastmessages [count] — show the last user and Codex messages (default 1)\n/tgmultisession [on|off] — toggle messages from all sessions\n/tgdisconnect — clear the selection\n/tgdeletesession — delete a session and keep its folder\n/tgsteer <text> — guide the active turn\n/tginterrupt — stop the active turn\n/tgquestions — show pending questions and approvals from all sessions\n/tginput [<approval-id> <question-id> <answer>] — show requests or answer one\n\nType /_ for session command suggestions in either mode. You can use /_<session_alias> <message> to send to a session without changing your selection, or /_<session_alias> to select it. /tgmultisession controls which sessions send messages here.\n\nCodex commands use their usual names: /status, /model, /compact, /review and more. Use /help for the full list or the bot menu."
}

func codexHelpText() string {
	var out strings.Builder
	out.WriteString("Codex commands for the selected session:\n")
	for _, command := range telegramcommands.CodexCommands() {
		fmt.Fprintf(&out, "/%s — %s\n", command.Command, command.Description)
	}
	out.WriteString("\nSend a command without arguments to see its options. Terminal-only controls explain how to use the attached CLI. Gateway commands begin with /tg; use /tghelp. Telegram menu names use underscores in place of CLI hyphens.")
	return out.String()
}
func telegramErrorText(code string) string {
	switch code {
	case "image_too_large":
		return "The image is too large. Send an image of 10 MiB or less."
	case "image_download_failed":
		return "The image could not be downloaded from Telegram. Please send it again."
	case "image_unsupported":
		return "This attachment is not a supported image. Send a JPEG, PNG, WebP or GIF image, with an optional caption."
	case "image_worker_upgrade":
		return "The selected worker needs an update before it can receive images. Please resend the image after the worker updates."
	case "image_input_reply":
		return "This input request needs a text answer. Send the image as a separate message to the selected session."
	case "unknown_command":
		return "Unknown command. Use /tghelp for gateway controls or /help for Codex commands."
	case "command_too_long":
		return "The command arguments are too long. Send fewer than 16,384 bytes."
	case "input_usage":
		return "Use /tgquestions to open pending requests, or /tginput <approval-id> <question-id> <answer> to answer one."
	case "history_usage":
		return "Use /tghistory to show the latest 10 saved Codex prompts, or /tghistory <count> with a count from 1 to 50."
	case "last_messages_usage":
		return "Use /tglastmessages to show the last message, or /tglastmessages <count> with a count from 1 to 50. Both user and Codex messages are included."
	case "multisession_usage":
		return "Use /tgmultisession to toggle multisession mode, or /tgmultisession on or off."
	case "session_alias_unavailable":
		return "This session command is unavailable. Use /tgsessions to choose a session, or /tgmultisession on to refresh the session commands."
	case "session_answer_ambiguous":
		return "This session has multiple questions waiting. Reply to the specific question or use its buttons."
	case "command_outcome_unknown":
		return "Codex did not confirm the outcome. Check /tgsessions and the working directory before trying again."
	case "session_name_invalid":
		return "Choose a session name of up to 120 bytes without slashes, backslashes, or control characters."
	case "wizard_pending":
		return "Your message was not sent as a prompt. Use the current session action's buttons below to finish, cancel, or continue chatting."
	case "session_action_expired":
		return "The worker did not confirm this session action before it timed out. You can send prompts again. The request may still complete; check /tgsessions before trying it again."
	case "wizard_expired":
		return "This session action expired. Start again with /tgsessions or /tgdeletesession."
	case "session_busy":
		return "This session or one of its child sessions has an active turn or pending work. Wait for it to finish or interrupt it before deleting the session."
	case "invalid_workspace":
		return "The folder is unavailable, outside the allowed workspaces, or the new folder already exists. Choose another parent folder or session name."
	case "internal_error":
		return "The worker could not complete this session action. Ensure the worker is up to date, then start again with /tgsessions or /tgdeletesession."
	case "unsupported_operation":
		return "The worker needs an update to support this session action. Update it and start again."
	case "callback_invalid":
		return "This button is expired, already used, or no longer valid."
	case "stale_turn":
		return "The active turn changed or ended. Use /tgstatus and try again."
	case "target_unavailable", "":
		return "No available target was found. Use /tginstances or /tgsessions to choose one."
	default:
		return "Request could not be completed: " + humanize(code) + "."
	}
}
func humanIdentity(worker registry.Worker, runtime protocol.Runtime, session protocol.Session) string {
	return workerLabel(worker) + " / " + runtimeLabel(runtime) + " / " + sessionLabel(session)
}
func workerLabel(worker registry.Worker) string {
	if value := strings.TrimSpace(worker.Name); value != "" {
		return value
	}
	if value := strings.TrimSpace(worker.Hostname); value != "" {
		return value
	}
	return "Worker"
}
func runtimeLabel(runtime protocol.Runtime) string {
	if value := strings.TrimSpace(runtime.Name); value != "" {
		return value
	}
	if value := strings.TrimSpace(runtime.ProfileID); value != "" {
		return value
	}
	return "Runtime"
}
func sessionLabel(session protocol.Session) string {
	if value := strings.TrimSpace(session.Name); value != "" {
		return value
	}
	if value := strings.TrimSpace(session.Preview); value != "" {
		runes := []rune(value)
		if len(runes) > 36 {
			return string(runes[:35]) + "…"
		}
		return value
	}
	if value := strings.TrimSpace(session.CWD); value != "" {
		if base := filepath.Base(value); base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	return "Session"
}
func sessionAvailability(session protocol.Session) string {
	if session.Archived {
		return "Archived"
	}
	if session.Loaded {
		return "Loaded"
	}
	return "Persisted"
}
func sessionIcon(session protocol.Session) string {
	if session.ActiveTurnID != "" || strings.EqualFold(session.State, "running") {
		return "🟢"
	}
	if session.Archived || strings.EqualFold(session.State, "failed") {
		return "🔴"
	}
	return "⚪"
}
func connectivityIcon(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "online", "connected":
		return "🟢"
	case "unreachable", "degraded":
		return "🟡"
	case "offline", "disabled":
		return "🔴"
	default:
		return "⚪"
	}
}
func decisionLabel(decision string) string {
	switch decision {
	case "accept", "approved", "approve", "acceptOnce":
		return "Approve once"
	case "acceptForSession", "approve_session":
		return "Approve session"
	case "decline", "declined", "reject":
		return "Decline"
	case "cancel":
		return "Cancel"
	default:
		return titleCase(decision)
	}
}
func displayValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return strings.TrimSpace(value)
}
func humanize(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "update"
	}
	var text strings.Builder
	for i, r := range value {
		if r == '_' || r == '-' {
			text.WriteByte(' ')
			continue
		}
		if i > 0 && r >= 'A' && r <= 'Z' {
			text.WriteByte(' ')
		}
		text.WriteRune(r)
	}
	return strings.ToLower(strings.Join(strings.Fields(text.String()), " "))
}
func titleCase(value string) string {
	value = humanize(value)
	runes := []rune(value)
	if len(runes) > 0 && runes[0] >= 'a' && runes[0] <= 'z' {
		runes[0] -= 'a' - 'A'
	}
	return string(runes)
}
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "unknown"
	}
	return id
}

// RenderDelivery is retained for callers that only need a store-free fallback.
// Sender.render provides the complete registry-backed Telegram UI.
func RenderDelivery(kind string, payload json.RawMessage) string {
	if kind == "ui_response" {
		var response registry.AcceptResult
		if json.Unmarshal(payload, &response) != nil {
			return "Update available."
		}
		if response.ErrorCode != "" || response.View == "error" {
			return telegramErrorText(response.ErrorCode)
		}
		if response.View == "help" || response.View == "start" {
			return helpText()
		}
		if response.View == "disconnected" {
			return "Selection cleared. Running runtimes and sessions were not changed."
		}
		return titleCase(response.View) + "."
	}
	var event protocol.Event
	if json.Unmarshal(payload, &event) != nil {
		return "Update available."
	}
	var result protocol.Result
	_ = json.Unmarshal(event.Data, &result)
	if (event.Kind == "final_agent_message" || event.Kind == "turn_completed") && strings.TrimSpace(result.Text) != "" {
		return result.Text
	}
	return titleCase(event.Kind) + "."
}

package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

const sessionWizardTTL = 30 * time.Minute

type sessionWizard struct {
	ID                              string
	Revision                        int64
	Kind, Phase, RuntimeID          string
	Generation                      int64
	SessionID, Name, CWD, CommandID string
	Workspace                       *protocol.WorkspacePage
	ExpiresAt                       time.Time
}

func (w sessionWizard) result(view string, in IncomingUpdate) AcceptResult {
	return AcceptResult{View: view, WizardID: w.ID, WizardRevision: w.Revision, RuntimeID: w.RuntimeID, SessionID: w.SessionID, SessionName: w.Name, CWD: w.CWD, Workspace: w.Workspace, UserID: in.UserID}
}

func readSessionWizard(ctx context.Context, tx *dbTx, in IncomingUpdate) (sessionWizard, error) {
	var w sessionWizard
	var workspace []byte
	err := tx.QueryRow(ctx, `SELECT wizard_id, revision, kind, phase, COALESCE(runtime_id,''), runtime_generation, COALESCE(session_id,''), session_name, cwd, COALESCE(workspace,'null'), COALESCE(command_id,''), expires_at FROM telegram_session_wizards WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4`, in.BotID, in.UserID, in.ChatID, in.TopicID).Scan(&w.ID, &w.Revision, &w.Kind, &w.Phase, &w.RuntimeID, &w.Generation, &w.SessionID, &w.Name, &w.CWD, &workspace, &w.CommandID, &w.ExpiresAt)
	if err != nil {
		return w, err
	}
	if err := json.Unmarshal(workspace, &w.Workspace); err != nil {
		return w, fmt.Errorf("registry: decode workspace wizard: %w", err)
	}
	return w, nil
}

func saveSessionWizard(ctx context.Context, tx *dbTx, in IncomingUpdate, w sessionWizard) error {
	workspace, err := json.Marshal(w.Workspace)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO telegram_session_wizards (wizard_id,bot_id,user_id,chat_id,message_thread_id,revision,kind,phase,runtime_id,runtime_generation,session_id,session_name,cwd,workspace,command_id,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,NULLIF($11,''),$12,$13,$14,NULLIF($15,''),$16) ON CONFLICT(bot_id,user_id,chat_id,message_thread_id) DO UPDATE SET wizard_id=excluded.wizard_id,revision=excluded.revision,kind=excluded.kind,phase=excluded.phase,runtime_id=excluded.runtime_id,runtime_generation=excluded.runtime_generation,session_id=excluded.session_id,session_name=excluded.session_name,cwd=excluded.cwd,workspace=excluded.workspace,command_id=excluded.command_id,expires_at=excluded.expires_at`, w.ID, in.BotID, in.UserID, in.ChatID, in.TopicID, w.Revision, w.Kind, w.Phase, w.RuntimeID, w.Generation, w.SessionID, w.Name, w.CWD, string(workspace), w.CommandID, w.ExpiresAt)
	if err != nil {
		return fmt.Errorf("registry: save session wizard: %w", err)
	}
	return nil
}

func (s *Store) startSessionWizard(ctx context.Context, tx *dbTx, in IncomingUpdate, kind string) (AcceptResult, error) {
	runtime, picker, err := resolveOptionalRuntime(ctx, tx, in.Target)
	if err != nil {
		return AcceptResult{}, err
	}
	w := sessionWizard{ID: uuid.NewString(), Revision: 1, Kind: kind, Phase: "runtime", ExpiresAt: time.Now().UTC().Add(sessionWizardTTL)}
	if !picker {
		w.RuntimeID = runtime.runtimeID.String()
		w.Generation = runtime.generation
		w.Phase = wizardFirstPhase(kind)
	}
	// Starting another workflow supersedes any late automatic selection from an older one.
	if err := bumpSelectionRevision(ctx, tx, in); err != nil {
		return AcceptResult{}, err
	}
	if err := saveSessionWizard(ctx, tx, in, w); err != nil {
		return AcceptResult{}, err
	}
	if picker {
		result := w.result("runtime_picker", in)
		result.Action = "new"
		if kind == "delete" {
			result.Action = "delete_session"
		}
		return result, nil
	}
	return w.result(wizardFirstView(kind), in), nil
}

func wizardFirstPhase(kind string) string {
	if kind == "delete" {
		return "sessions"
	}
	return "name"
}
func wizardFirstView(kind string) string {
	if kind == "delete" {
		return "delete_sessions"
	}
	return "new_session_name"
}

// Plain replies belong to the active wizard before normal reply/session routing.
// During async steps they are never accidentally sent to a model as prompts.
func (s *Store) acceptWizardText(ctx context.Context, tx *dbTx, in IncomingUpdate) (bool, AcceptResult, error) {
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if action != "" && action != "text" {
		return false, AcceptResult{}, nil
	}
	w, err := readSessionWizard(ctx, tx, in)
	if errors.Is(err, sql.ErrNoRows) {
		return false, AcceptResult{}, nil
	}
	if err != nil {
		return true, AcceptResult{}, err
	}
	if w.Phase == "done" || w.Phase == "cancelled" {
		return false, AcceptResult{}, nil
	}
	if !w.ExpiresAt.After(time.Now()) {
		w.Phase = "cancelled"
		w.Revision++
		if err := saveSessionWizard(ctx, tx, in, w); err != nil {
			return true, AcceptResult{}, err
		}
		return true, AcceptResult{View: "error", ErrorCode: "wizard_expired"}, nil
	}
	if w.Kind != "new" || w.Phase != "name" {
		result := w.result("error", in)
		result.ErrorCode = "wizard_pending"
		return true, result, nil
	}
	if len(in.Images) > 0 {
		result := w.result("new_session_name", in)
		result.ErrorCode = "session_name_invalid"
		return true, result, nil
	}
	name, err := protocol.NormalizeSessionName(in.Text)
	if err != nil {
		result := w.result("new_session_name", in)
		result.ErrorCode = "session_name_invalid"
		return true, result, nil
	}
	runtime, err := resolveRuntime(ctx, tx, w.RuntimeID)
	if err != nil {
		return true, AcceptResult{}, err
	}
	if runtime.generation != w.Generation {
		return true, AcceptResult{}, ErrCallbackInvalid
	}
	w.Name = name
	w.Revision++
	result, err := queueWorkspaceBrowse(ctx, tx, in, &w, runtime, "", 0)
	return true, result, err
}

func isWizardCallback(action string) bool {
	switch action {
	case "wizard_runtime", "wizard_browse", "wizard_create", "wizard_cancel", "wizard_rename", "delete_sessions", "delete_session_pick", "delete_session_confirm":
		return true
	}
	return false
}

func (s *Store) consumeWizardCallback(ctx context.Context, tx *dbTx, in IncomingUpdate, action string, p callbackPayload, sessionID *uuid.UUID) (AcceptResult, error) {
	w, err := readSessionWizard(ctx, tx, in)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if err != nil {
		return AcceptResult{}, err
	}
	if w.ID != p.WizardID || w.Revision != p.WizardRevision || !w.ExpiresAt.After(time.Now()) || w.Phase == "done" || w.Phase == "cancelled" {
		return AcceptResult{}, ErrCallbackInvalid
	}
	if action == "wizard_cancel" {
		if w.Phase == "creating" || w.Phase == "deleting" {
			return AcceptResult{}, ErrCallbackInvalid
		}
		w.Revision++
		w.Phase = "cancelled"
		if err := bumpSelectionRevision(ctx, tx, in); err != nil {
			return AcceptResult{}, err
		}
		if err := saveSessionWizard(ctx, tx, in, w); err != nil {
			return AcceptResult{}, err
		}
		return w.result("wizard_cancelled", in), nil
	}
	if action == "wizard_runtime" {
		if w.Phase != "runtime" || p.RuntimeID == "" {
			return AcceptResult{}, ErrCallbackInvalid
		}
		runtime, err := resolveRuntime(ctx, tx, p.RuntimeID)
		if err != nil {
			return AcceptResult{}, callbackTargetError(err)
		}
		if !callbackMatchesTarget(p, runtime) {
			return AcceptResult{}, ErrCallbackInvalid
		}
		w.RuntimeID = runtime.runtimeID.String()
		w.Generation = runtime.generation
		w.Revision++
		w.Phase = wizardFirstPhase(w.Kind)
		if err := saveSessionWizard(ctx, tx, in, w); err != nil {
			return AcceptResult{}, err
		}
		return w.result(wizardFirstView(w.Kind), in), nil
	}
	runtime, err := resolveRuntime(ctx, tx, w.RuntimeID)
	if err != nil {
		return AcceptResult{}, callbackTargetError(err)
	}
	if runtime.generation != w.Generation || !callbackMatchesTarget(p, runtime) {
		return AcceptResult{}, ErrCallbackInvalid
	}
	switch action {
	case "wizard_rename":
		if w.Kind != "new" || w.Phase != "browse" {
			return AcceptResult{}, ErrCallbackInvalid
		}
		w.Phase = "name"
		w.CommandID = ""
		w.Revision++
		if err := saveSessionWizard(ctx, tx, in, w); err != nil {
			return AcceptResult{}, err
		}
		return w.result("new_session_name", in), nil
	case "wizard_browse":
		if w.Kind != "new" || w.Phase != "browse" || !wizardBrowseAllowed(w.Workspace, p.Path, p.Offset) {
			return AcceptResult{}, ErrCallbackInvalid
		}
		w.Revision++
		return queueWorkspaceBrowse(ctx, tx, in, &w, runtime, p.Path, p.Offset)
	case "wizard_create":
		if w.Kind != "new" || w.Phase != "browse" || w.Workspace == nil {
			return AcceptResult{}, ErrCallbackInvalid
		}
		command, err := createTelegramCommand(ctx, tx, in, runtime, protocol.NewSession, "", "", protocol.Arguments{CWD: w.Workspace.Path, SessionName: w.Name, CreateDirectory: true})
		if err != nil {
			return AcceptResult{}, err
		}
		w.Phase = "creating"
		w.CommandID = command.ID
		w.CWD = w.Workspace.Path
		w.Revision++
		if err := saveSessionWizard(ctx, tx, in, w); err != nil {
			return AcceptResult{}, err
		}
		result := w.result("session_creating", in)
		result.CommandID = command.ID
		return result, nil
	case "delete_sessions":
		if w.Kind != "delete" || (w.Phase != "sessions" && w.Phase != "confirm") {
			return AcceptResult{}, ErrCallbackInvalid
		}
		w.Phase = "sessions"
		w.SessionID = ""
		w.Name = ""
		w.CWD = ""
		w.Revision++
		if err := saveSessionWizard(ctx, tx, in, w); err != nil {
			return AcceptResult{}, err
		}
		result := w.result("delete_sessions", in)
		result.SessionPage = p.SessionPage
		return result, nil
	case "delete_session_pick":
		if w.Kind != "delete" || w.Phase != "sessions" || sessionID == nil {
			return AcceptResult{}, ErrCallbackInvalid
		}
		target, err := sessionRoute(ctx, tx, *sessionID)
		if err != nil {
			return AcceptResult{}, callbackTargetError(err)
		}
		if target.runtimeID != runtime.runtimeID {
			return AcceptResult{}, ErrCallbackInvalid
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF(name,''),NULLIF(preview,''),codex_thread_id),COALESCE(cwd,'') FROM sessions WHERE session_id=$1`, *sessionID).Scan(&w.Name, &w.CWD); err != nil {
			return AcceptResult{}, err
		}
		w.SessionID = sessionID.String()
		w.Phase = "confirm"
		w.Revision++
		if err := saveSessionWizard(ctx, tx, in, w); err != nil {
			return AcceptResult{}, err
		}
		return w.result("delete_session_confirm", in), nil
	case "delete_session_confirm":
		if w.Kind != "delete" || w.Phase != "confirm" || sessionID == nil || w.SessionID != sessionID.String() {
			return AcceptResult{}, ErrCallbackInvalid
		}
		target, err := sessionRoute(ctx, tx, *sessionID)
		if err != nil {
			return AcceptResult{}, callbackTargetError(err)
		}
		if target.runtimeID != runtime.runtimeID || target.generation != w.Generation {
			return AcceptResult{}, ErrCallbackInvalid
		}
		if target.activeTurnID != "" {
			result := w.result("delete_session_confirm", in)
			result.ErrorCode = "session_busy"
			return result, nil
		}
		command, err := createTelegramCommand(ctx, tx, in, target, protocol.DeleteSession, target.sessionID.String(), "", protocol.Arguments{CWD: w.CWD})
		if err != nil {
			return AcceptResult{}, err
		}
		w.Phase = "deleting"
		w.CommandID = command.ID
		w.Revision++
		if err := saveSessionWizard(ctx, tx, in, w); err != nil {
			return AcceptResult{}, err
		}
		result := w.result("session_deleting", in)
		result.CommandID = command.ID
		return result, nil
	}
	return AcceptResult{}, ErrCallbackInvalid
}

func queueWorkspaceBrowse(ctx context.Context, tx *dbTx, in IncomingUpdate, w *sessionWizard, runtime routeTarget, path string, offset int) (AcceptResult, error) {
	command, err := createTelegramCommand(ctx, tx, in, runtime, protocol.BrowseWorkspace, "", "", protocol.Arguments{Workspace: &protocol.WorkspaceRequest{Path: path, Offset: offset}})
	if err != nil {
		return AcceptResult{}, err
	}
	w.Phase = "browsing"
	w.CommandID = command.ID
	if err := saveSessionWizard(ctx, tx, in, *w); err != nil {
		return AcceptResult{}, err
	}
	result := w.result("workspace_loading", in)
	result.CommandID = command.ID
	return result, nil
}

// Navigation is limited to entries issued on the last accepted worker page.
func wizardBrowseAllowed(page *protocol.WorkspacePage, path string, offset int) bool {
	if page == nil || path == "" || offset < 0 || offset > 1_000_000 {
		return false
	}
	if path == page.Path {
		return (page.HasMore && offset == page.Offset+protocol.WorkspacePageSize) || (page.Offset > 0 && offset == max(0, page.Offset-protocol.WorkspacePageSize))
	}
	if offset != 0 {
		return false
	}
	if path == page.Parent && page.Parent != "" {
		return true
	}
	for _, entry := range page.Directories {
		if path == entry.Path {
			return true
		}
	}
	return false
}

func validWorkspacePage(page *protocol.WorkspacePage, request *protocol.WorkspaceRequest) bool {
	if page == nil || request == nil || page.Offset != request.Offset || len(page.Directories) > protocol.WorkspacePageSize || (page.HasMore && len(page.Directories) != protocol.WorkspacePageSize) || !validWorkspacePath(page.Path) || (page.Parent != "" && !validWorkspacePath(page.Parent)) {
		return false
	}
	if request.Path != "" && filepath.Clean(request.Path) != page.Path {
		return false
	}
	if page.Parent != "" && (page.Parent == page.Path || filepath.Dir(page.Path) != page.Parent) {
		return false
	}
	for _, entry := range page.Directories {
		if entry.Name == "" || !utf8.ValidString(entry.Name) || len(entry.Name) > 255 || strings.ContainsAny(entry.Name, "/\\\x00") || entry.Name == "." || entry.Name == ".." || !validWorkspacePath(entry.Path) {
			return false
		}
	}
	return true
}
func validWorkspacePath(path string) bool {
	return path != "" && len(path) <= 4096 && utf8.ValidString(path) && !strings.ContainsRune(path, 0) && filepath.IsAbs(path) && filepath.Clean(path) == path
}

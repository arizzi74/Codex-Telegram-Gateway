package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

// Wizard responses are private replies to their initiating Telegram context.
// They must never broadcast directory listings or deletion results to a session.
func applySessionWizardEvent(ctx context.Context, tx *dbTx, workerID uuid.UUID, event protocol.Event, target eventTarget) (bool, error) {
	var result protocol.Result
	if json.Unmarshal(event.Data, &result) != nil {
		return false, nil
	}
	if result.CommandID == "" {
		if result.Workspace != nil {
			return true, ErrEventTarget
		}
		return false, nil
	}
	id, err := uuid.Parse(result.CommandID)
	if err != nil {
		return true, ErrEventTarget
	}
	var raw []byte
	var operation string
	var botID *string
	var userID, chatID, topicID *int64
	err = tx.QueryRow(ctx, `SELECT operation,payload,telegram_bot_id,telegram_user_id,telegram_chat_id,telegram_message_thread_id FROM commands WHERE command_id=$1 AND worker_id=$2 AND runtime_id IS $3 AND runtime_generation=$4 AND session_id IS $5`, id, workerID, target.runtimeID, int64(event.RuntimeGeneration), target.sessionID).Scan(&operation, &raw, &botID, &userID, &chatID, &topicID)
	if errors.Is(err, sql.ErrNoRows) {
		return true, ErrEventTarget
	}
	if err != nil {
		return true, fmt.Errorf("registry: read wizard result command: %w", err)
	}
	var command protocol.Command
	if json.Unmarshal(raw, &command) != nil {
		return true, ErrEventTarget
	}
	managedNew := protocol.Operation(operation) == protocol.NewSession && command.Arguments.CreateDirectory
	if protocol.Operation(operation) != protocol.BrowseWorkspace && protocol.Operation(operation) != protocol.DeleteSession && !managedNew {
		if result.Workspace != nil {
			return true, ErrEventTarget
		}
		return false, nil
	}
	if event.Kind != "command_completed" && event.Kind != "command_failed" && event.Kind != "command_result_unknown" {
		return true, ErrEventTarget
	}
	success := event.Kind == "command_completed"
	if result.TurnID != "" || result.History != nil || target.runtimeID == nil || (success && result.Error != nil) {
		return true, ErrEventTarget
	}
	if protocol.Operation(operation) == protocol.BrowseWorkspace {
		if result.Session != nil || (success && !validWorkspacePage(result.Workspace, command.Arguments.Workspace)) || (!success && result.Workspace != nil) {
			return true, ErrEventTarget
		}
	} else if result.Workspace != nil {
		return true, ErrEventTarget
	}
	if managedNew && success {
		folder, err := protocol.SessionDirectoryName(command.Arguments.SessionName)
		if err != nil || result.Session == nil || result.Session.Name != command.Arguments.SessionName || result.Session.CWD != filepath.Join(command.Arguments.CWD, folder) || result.Session.Archived || result.Session.ActiveTurnID != "" {
			return true, ErrEventTarget
		}
	}
	if !success && result.Session != nil {
		return true, ErrEventTarget
	}
	if protocol.Operation(operation) == protocol.DeleteSession && success {
		if target.sessionID == nil || result.Session == nil || result.Session.ID != target.sessionID.String() || !result.Session.Archived || !result.Session.Deleted || result.Session.ThreadID != command.ThreadID || result.Session.Loaded || result.Session.ActiveTurnID != "" {
			return true, ErrEventTarget
		}
	}
	if err := updateCommandOutcome(ctx, tx, id, workerID, event, target, result); err != nil {
		return true, err
	}
	// A confirmed permanent deletion must also survive replay after a worker
	// restart. Its immutable command pins the same worker/runtime/session/thread.
	// Ordinary creation snapshots still require the current generation.
	deleted := protocol.Operation(operation) == protocol.DeleteSession
	if success && result.Session != nil {
		if deleted {
			if err := applyDeletedSession(ctx, tx, workerID, *target.runtimeID, target.sessionID, *result.Session); err != nil {
				return true, err
			}
		} else if target.runtimeCurrent {
			if err := upsertProtocolSession(ctx, tx, workerID, *target.runtimeID, target.sessionID, *result.Session); err != nil {
				return true, err
			}
		}
	}
	if botID == nil || userID == nil || chatID == nil {
		return true, nil
	}
	in := IncomingUpdate{BotID: *botID, UserID: *userID, ChatID: *chatID}
	if topicID != nil {
		in.TopicID = *topicID
	}
	w, err := readSessionWizard(ctx, tx, in)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	if !target.runtimeCurrent || w.CommandID != id.String() || w.RuntimeID != target.runtimeID.String() || w.Generation != int64(event.RuntimeGeneration) || !w.ExpiresAt.After(time.Now()) {
		return true, nil
	}
	if (operation == string(protocol.BrowseWorkspace) && w.Phase != "browsing") || (managedNew && w.Phase != "creating") || (operation == string(protocol.DeleteSession) && w.Phase != "deleting") {
		return true, nil
	}
	w.Revision++
	w.CommandID = ""
	view := ""
	if success {
		switch protocol.Operation(operation) {
		case protocol.BrowseWorkspace:
			w.Workspace = result.Workspace
			w.CWD = result.Workspace.Path
			w.Phase = "browse"
			view = "workspace_browser"
		case protocol.NewSession:
			w.SessionID = result.Session.ID
			w.CWD = result.Session.CWD
			w.Phase = "done"
			view = "selected"
			if err := bindCommandSession(ctx, tx, id, uuid.MustParse(result.Session.ID)); err != nil {
				return true, err
			}
			var selected bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM telegram_bindings WHERE bot_id=$1 AND user_id=$2 AND chat_id=$3 AND message_thread_id=$4 AND session_id=$5)`, in.BotID, in.UserID, in.ChatID, in.TopicID, result.Session.ID).Scan(&selected); err != nil {
				return true, err
			}
			if !selected {
				view = "session_created"
			}

		case protocol.DeleteSession:
			w.Phase = "done"
			view = "session_deleted"
		}
	} else if event.Kind == "command_result_unknown" {
		w.Phase = "done"
		view = "error"
	} else {
		switch protocol.Operation(operation) {
		case protocol.DeleteSession:
			w.Phase = "confirm"
			view = "delete_session_confirm"
		default:
			if w.Workspace == nil {
				w.Phase = "name"
				view = "new_session_name"
			} else {
				w.Phase = "browse"
				view = "workspace_browser"
			}
		}
	}
	if err := saveSessionWizard(ctx, tx, in, w); err != nil {
		return true, err
	}
	response := w.result(view, in)
	if !success {
		response.ErrorCode = "command_failed"
		if result.Error != nil && result.Error.Code != "" {
			response.ErrorCode = result.Error.Code
		}
		if event.Kind == "command_result_unknown" {
			response.ErrorCode = "command_outcome_unknown"
		}
	}
	if err := queueUIResponse(ctx, tx, in, response); err != nil {
		return true, err
	}
	return true, nil
}

// Old workers can reject an unfamiliar operation in their acknowledgement
// without producing an event. Recover the private wizard from that rejection
// as well, once only, using the same immutable command/context association.
func recoverSessionWizardAcknowledgement(ctx context.Context, tx *dbTx, commandID uuid.UUID, code string) error {
	var in IncomingUpdate
	err := tx.QueryRow(ctx, `SELECT bot_id,user_id,chat_id,message_thread_id FROM telegram_session_wizards WHERE command_id=$1`, commandID).Scan(&in.BotID, &in.UserID, &in.ChatID, &in.TopicID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	w, err := readSessionWizard(ctx, tx, in)
	if err != nil {
		return err
	}
	if !w.ExpiresAt.After(time.Now()) || (w.Phase != "browsing" && w.Phase != "creating" && w.Phase != "deleting") {
		return nil
	}
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT generation FROM runtimes WHERE runtime_id=$1`, w.RuntimeID).Scan(&generation); err != nil {
		return err
	}
	if generation != w.Generation {
		return nil
	}
	view := ""
	if w.Kind == "delete" {
		w.Phase = "confirm"
		view = "delete_session_confirm"
	} else if w.Workspace == nil {
		w.Phase = "name"
		view = "new_session_name"
	} else {
		w.Phase = "browse"
		view = "workspace_browser"
	}
	w.Revision++
	w.CommandID = ""
	if err := saveSessionWizard(ctx, tx, in, w); err != nil {
		return err
	}
	result := w.result(view, in)
	result.ErrorCode = code
	if result.ErrorCode == "" {
		result.ErrorCode = "command_failed"
	}
	return queueUIResponse(ctx, tx, in, result)
}

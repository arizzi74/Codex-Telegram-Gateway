package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrWorkerNotRevoked means permanent deletion requires prior revocation.
var ErrWorkerNotRevoked = errors.New("registry: worker must be revoked before deletion")

// DeleteRevokedWorker permanently removes a revoked worker and its gateway
// records. It does not contact the worker or touch its projects or native Codex
// files. An IMMEDIATE transaction fences concurrent writers for the entire
// reference discovery and deletion; enabled workers are refused before writes.
func (s *Store) DeleteRevokedWorker(ctx context.Context, workerID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("registry: begin worker deletion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT enabled FROM workers WHERE worker_id=$1`, workerID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWorkerNotFound
	}
	if err != nil {
		return fmt.Errorf("registry: read worker for deletion: %w", err)
	}
	if enabled {
		return ErrWorkerNotRevoked
	}
	if err := scopeDeletedWorker(ctx, tx, workerID); err != nil {
		return fmt.Errorf("registry: discover worker records: %w", err)
	}

	// The journal normally prohibits deletion altogether. Save and restore the
	// exact guards inside this writer transaction: no other connection can write
	// while they are absent, and a rollback restores both the rows and the DDL.
	guards := []struct{ name, sql string }{
		{name: "commands_preserve_records_trigger"},
		{name: "events_preserve_records_trigger"},
	}
	for i := range guards {
		if err := tx.QueryRow(ctx, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=$1`, guards[i].name).Scan(&guards[i].sql); err != nil {
			return fmt.Errorf("registry: read worker journal guard: %w", err)
		}
		if _, err := tx.Exec(ctx, `DROP TRIGGER `+guards[i].name); err != nil {
			return fmt.Errorf("registry: suspend worker journal guard: %w", err)
		}
	}
	for _, query := range deletedWorkerQueries {
		if _, err := tx.Exec(ctx, query, workerID); err != nil {
			return fmt.Errorf("registry: delete worker records: %w", err)
		}
	}
	for _, guard := range guards {
		if _, err := tx.Exec(ctx, guard.sql); err != nil {
			return fmt.Errorf("registry: restore worker journal guard: %w", err)
		}
	}
	for _, table := range []string{"_worker_delete_messages", "_worker_delete_ids"} {
		if _, err := tx.Exec(ctx, `DROP TABLE temp.`+table); err != nil {
			return fmt.Errorf("registry: clear worker deletion scope: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("registry: commit worker deletion: %w", err)
	}
	s.notifySessionActivity()
	return nil
}

// Only worker-owned identities and the identities of their dependent Telegram
// records enter this set. Shared chat preferences, selection revision counters,
// administrator logins/drafts and push subscriptions are deliberately retained.
func scopeDeletedWorker(ctx context.Context, tx *dbTx, workerID uuid.UUID) error {
	for _, query := range []string{
		`CREATE TEMP TABLE _worker_delete_ids (kind TEXT NOT NULL,id TEXT NOT NULL,PRIMARY KEY(kind,id)) WITHOUT ROWID`,
		`CREATE TEMP TABLE _worker_delete_messages (bot_id TEXT NOT NULL,chat_id INTEGER NOT NULL,message_id INTEGER NOT NULL,PRIMARY KEY(bot_id,chat_id,message_id)) WITHOUT ROWID`,
		`INSERT INTO _worker_delete_ids VALUES ('worker',$1)`,
	} {
		if _, err := tx.Exec(ctx, query, workerID); err != nil {
			return err
		}
	}
	for _, source := range []struct{ kind, table, column string }{
		{"runtime", "runtimes", "runtime_id"},
		{"session", "sessions", "session_id"},
		{"command", "commands", "command_id"},
		{"event", "events", "event_id"},
		{"approval", "approvals", "approval_id"},
		{"request", "worker_update_requests", "request_id"},
		{"enrollment", "worker_enrollments", "enrollment_id"},
		{"voice", "telegram_voice_jobs", "job_id"},
	} {
		query := `INSERT INTO _worker_delete_ids SELECT '` + source.kind + `',` + source.column + ` FROM ` + source.table + ` WHERE worker_id=$1`
		if _, err := tx.Exec(ctx, query, workerID); err != nil {
			return err
		}
	}
	// Deliveries and callbacks refer to one another through JSON, while helper
	// cleanup deliveries sometimes contain only a destination and message ID.
	// Compute their transitive closure before removing any source of identity.
	for {
		var changed int64
		for _, query := range deletedWorkerScopeQueries {
			result, err := tx.Exec(ctx, query)
			if err != nil {
				return err
			}
			changed += result.RowsAffected()
		}
		if changed == 0 {
			return nil
		}
	}
}

// JSON references can appear inside nested result/session/worker-update objects
// and rendered keyboards. Match complete string values, including opaque cb:
// tokens, without treating arbitrary message text or numeric chat IDs as IDs.
func deletedWorkerJSON(expression string) string {
	return `EXISTS (SELECT 1 FROM json_tree(` + expression + `) atom WHERE atom.type='text'
        AND (atom.atom IN (SELECT id FROM _worker_delete_ids)
          OR atom.atom IN (SELECT 'cb:'||id FROM _worker_delete_ids WHERE kind='callback')))`
}

var deletedWorkerScopeQueries = []string{
	`INSERT OR IGNORE INTO _worker_delete_ids SELECT 'wizard',wizard_id FROM telegram_session_wizards
        WHERE runtime_id IN (SELECT id FROM _worker_delete_ids WHERE kind='runtime')
          OR session_id IN (SELECT id FROM _worker_delete_ids WHERE kind='session')
          OR command_id IN (SELECT id FROM _worker_delete_ids WHERE kind='command')
          OR ` + deletedWorkerJSON(`COALESCE(workspace,'null')`),
	`INSERT OR IGNORE INTO _worker_delete_ids SELECT 'callback',token FROM telegram_callbacks
        WHERE session_id IN (SELECT id FROM _worker_delete_ids WHERE kind='session')
          OR command_id IN (SELECT id FROM _worker_delete_ids WHERE kind='command')
          OR approval_id IN (SELECT id FROM _worker_delete_ids WHERE kind='approval')
          OR ` + deletedWorkerJSON(`payload`),
	`INSERT OR IGNORE INTO _worker_delete_ids SELECT 'delivery',delivery_id FROM telegram_deliveries
        WHERE event_id IN (SELECT id FROM _worker_delete_ids WHERE kind='event')
          OR ` + deletedWorkerJSON(`payload`) + `
          OR delivery_id IN (SELECT delivery_id FROM telegram_delivery_chunks WHERE ` + deletedWorkerJSON(`payload`) + `)
          OR delivery_id IN (SELECT delivery_id FROM telegram_progress_messages
              WHERE runtime_id IN (SELECT id FROM _worker_delete_ids WHERE kind='runtime')
                 OR session_id IN (SELECT id FROM _worker_delete_ids WHERE kind='session'))
          OR delivery_id IN (SELECT delivery_id FROM telegram_input_reply_helpers
              WHERE approval_id IN (SELECT id FROM _worker_delete_ids WHERE kind='approval'))
          OR delivery_id IN (SELECT delivery_id FROM telegram_question_answer_edits
              WHERE approval_id IN (SELECT id FROM _worker_delete_ids WHERE kind='approval'))
          OR (kind IN ('picker_cleanup','input_reply_cleanup','question_answered') AND EXISTS (
              SELECT 1 FROM _worker_delete_messages message
              WHERE message.bot_id=telegram_deliveries.bot_id AND message.chat_id=telegram_deliveries.chat_id
                AND message.message_id=json_extract(telegram_deliveries.payload,'$.message_id')))`,
	`INSERT OR IGNORE INTO _worker_delete_messages SELECT bot_id,chat_id,message_id FROM bot_message_routes
        WHERE session_id IN (SELECT id FROM _worker_delete_ids WHERE kind='session')
          OR approval_id IN (SELECT id FROM _worker_delete_ids WHERE kind='approval')`,
	`INSERT OR IGNORE INTO _worker_delete_messages SELECT bot_id,chat_id,telegram_message_id FROM telegram_progress_messages
        WHERE runtime_id IN (SELECT id FROM _worker_delete_ids WHERE kind='runtime')
          OR session_id IN (SELECT id FROM _worker_delete_ids WHERE kind='session')
          OR delivery_id IN (SELECT id FROM _worker_delete_ids WHERE kind='delivery')`,
	`INSERT OR IGNORE INTO _worker_delete_messages SELECT bot_id,chat_id,message_id FROM telegram_input_reply_messages
        WHERE delivery_id IN (SELECT id FROM _worker_delete_ids WHERE kind='delivery')`,
	`INSERT OR IGNORE INTO _worker_delete_messages SELECT bot_id,chat_id,telegram_message_id FROM telegram_deliveries
        WHERE delivery_id IN (SELECT id FROM _worker_delete_ids WHERE kind='delivery') AND telegram_message_id>0`,
	`INSERT OR IGNORE INTO _worker_delete_messages SELECT delivery.bot_id,delivery.chat_id,chunk.telegram_message_id
        FROM telegram_delivery_chunks chunk JOIN telegram_deliveries delivery USING(delivery_id)
        WHERE delivery.delivery_id IN (SELECT id FROM _worker_delete_ids WHERE kind='delivery') AND chunk.telegram_message_id>0`,
}

var deletedWorkerQueries = []string{
	// Preserve anonymous receipt identities so delayed webhook retries cannot
	// execute against a different selection. Remove their content and actor/chat
	// linkage while the worker's command and message identities still exist.
	// Raw command text can embed an ID, e.g. /tgsessions <runtime UUID>, so raw
	// receipts also match IDs within strings; graph discovery uses exact values.
	`UPDATE telegram_updates SET raw='{}',user_id=NULL,chat_id=NULL WHERE EXISTS (SELECT 1 FROM telegram_voice_jobs voice
        WHERE voice.worker_id=$1 AND voice.bot_id=telegram_updates.bot_id AND voice.update_id=telegram_updates.update_id)
		OR EXISTS (SELECT 1 FROM commands command
        WHERE command.worker_id=$1 AND command.telegram_bot_id=telegram_updates.bot_id
          AND command.telegram_update_id=telegram_updates.update_id)
		OR EXISTS (SELECT 1 FROM json_tree(raw) atom JOIN _worker_delete_ids identity
          ON instr(lower(atom.atom),lower(identity.id))>0 WHERE atom.type='text')
        OR EXISTS (SELECT 1 FROM _worker_delete_messages message
          WHERE message.bot_id=telegram_updates.bot_id AND message.chat_id=telegram_updates.chat_id
            AND (message.message_id=json_extract(raw,'$.message.reply_to_message.message_id')
              OR message.message_id=json_extract(raw,'$.callback_query.message.message_id')
              OR message.message_id=json_extract(raw,'$.callback_query.message.reply_to_message.message_id')))`,
	`DELETE FROM telegram_input_reply_messages WHERE delivery_id IN (SELECT id FROM _worker_delete_ids WHERE kind='delivery')`,
	`DELETE FROM telegram_question_answer_edits WHERE delivery_id IN (SELECT id FROM _worker_delete_ids WHERE kind='delivery')
        OR approval_id IN (SELECT id FROM _worker_delete_ids WHERE kind='approval')`,
	`DELETE FROM telegram_input_reply_helpers WHERE delivery_id IN (SELECT id FROM _worker_delete_ids WHERE kind='delivery')
        OR approval_id IN (SELECT id FROM _worker_delete_ids WHERE kind='approval')`,
	`DELETE FROM telegram_progress_messages WHERE delivery_id IN (SELECT id FROM _worker_delete_ids WHERE kind='delivery')
        OR session_id IN (SELECT id FROM _worker_delete_ids WHERE kind='session')
        OR runtime_id IN (SELECT id FROM _worker_delete_ids WHERE kind='runtime')`,
	`DELETE FROM telegram_callbacks WHERE token IN (SELECT id FROM _worker_delete_ids WHERE kind='callback')`,
	`DELETE FROM bot_message_routes WHERE session_id IN (SELECT id FROM _worker_delete_ids WHERE kind='session')
        OR approval_id IN (SELECT id FROM _worker_delete_ids WHERE kind='approval')`,
	`DELETE FROM telegram_bindings WHERE session_id IN (SELECT id FROM _worker_delete_ids WHERE kind='session')`,
	`DELETE FROM telegram_session_aliases WHERE session_id IN (SELECT id FROM _worker_delete_ids WHERE kind='session')`,
	`DELETE FROM telegram_session_wizards WHERE wizard_id IN (SELECT id FROM _worker_delete_ids WHERE kind='wizard')`,
	`DELETE FROM webpush_deliveries WHERE session_id IN (SELECT id FROM _worker_delete_ids WHERE kind='session')
        OR event_id IN (SELECT id FROM _worker_delete_ids WHERE kind='event')`,
	`DELETE FROM telegram_question_answers WHERE approval_id IN (SELECT id FROM _worker_delete_ids WHERE kind='approval')`,
	`DELETE FROM telegram_voice_jobs WHERE worker_id=$1`,
	`DELETE FROM approvals WHERE worker_id=$1`,
	// Delivery chunks cascade; helper, edit and progress dependents were removed.
	`DELETE FROM telegram_deliveries WHERE delivery_id IN (SELECT id FROM _worker_delete_ids WHERE kind='delivery')`,
	`DELETE FROM commands WHERE worker_id=$1`,
	`DELETE FROM events WHERE worker_id=$1`,
	`DELETE FROM session_settings WHERE worker_id=$1`,
	`DELETE FROM sessions WHERE worker_id=$1`,
	`DELETE FROM runtimes WHERE worker_id=$1`,
	`DELETE FROM worker_update_watchers WHERE request_id IN (SELECT id FROM _worker_delete_ids WHERE kind='request')`,
	`DELETE FROM worker_update_requests WHERE worker_id=$1`,
	`DELETE FROM worker_enrollments WHERE worker_id=$1`,
	`DELETE FROM worker_event_watermarks WHERE worker_id=$1`,
	`DELETE FROM workers WHERE worker_id=$1`,
}

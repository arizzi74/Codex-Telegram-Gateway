package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

const webPushMaxAttempts = 6

// Eligibility mirrors the visible session inventory. Workers represent helper
// and subagent sessions as archived; deleted tombstones are excluded explicitly.
const webPushEligible = ` FROM webpush_deliveries d
    JOIN webpush_subscriptions p ON p.subscription_id=d.subscription_id
    JOIN admin_credentials c ON c.credential_id=p.credential_id
    JOIN admin_sessions a ON a.session_id=p.admin_session_id
    JOIN sessions s ON s.session_id=d.session_id
    JOIN workers w ON w.worker_id=s.worker_id
    WHERE c.revoked_at IS NULL AND a.revoked_at IS NULL AND w.enabled=TRUE
      AND s.archived=FALSE AND COALESCE(json_extract(s.metadata,'$.deleted'),0)=0`

func enqueueWebPushDeliveries(ctx context.Context, tx *dbTx, event protocol.Event, target eventTarget) (bool, error) {
	if !target.runtimeCurrent || target.sessionID == nil || target.runtimeID == nil {
		return false, nil
	}
	switch event.Kind {
	case "turn_completed", "turn_failed", "turn_interrupted":
	default:
		return false, nil
	}
	var result protocol.Result
	if json.Unmarshal(event.Data, &result) != nil || strings.TrimSpace(result.TurnID) == "" || len(result.TurnID) > 512 {
		return false, nil
	}
	// Never replay historical completions when registering a new device or
	// reconnecting a worker. Fresh terminal events may be delayed briefly offline.
	now := time.Now().UTC()
	if event.OccurredAt.Before(now.Add(-15*time.Minute)) || event.OccurredAt.After(now.Add(2*time.Minute)) {
		return false, nil
	}
	var visible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE session_id=$1 AND archived=FALSE AND COALESCE(json_extract(metadata,'$.deleted'),0)=0)`, *target.sessionID).Scan(&visible); err != nil {
		return false, err
	}
	if !visible {
		return false, nil
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM webpush_deliveries`).Scan(&total); err != nil {
		return false, err
	}
	if total >= 8192 {
		return false, nil
	}
	rows, err := tx.Query(ctx, `SELECT p.subscription_id FROM webpush_subscriptions p
        JOIN admin_credentials c ON c.credential_id=p.credential_id JOIN admin_sessions a ON a.session_id=p.admin_session_id
        WHERE c.revoked_at IS NULL AND a.revoked_at IS NULL AND p.created_at<=$1
        ORDER BY p.subscription_id LIMIT $2`, event.OccurredAt, 8192-total)
	if err != nil {
		return false, err
	}
	var subscriptions []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		subscriptions = append(subscriptions, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	queued := false
	for _, subscription := range subscriptions {
		ct, err := tx.Exec(ctx, `INSERT INTO webpush_deliveries(delivery_id,subscription_id,event_id,session_id,runtime_generation,turn_id,next_attempt_at,expires_at,created_at)
          VALUES($1,$2,$3,$4,$5,$6,$7,$8,$7) ON CONFLICT(subscription_id,session_id,runtime_generation,turn_id) DO NOTHING`,
			uuid.New(), subscription, event.ID, *target.sessionID, int64(event.RuntimeGeneration), result.TurnID, now, now.Add(15*time.Minute))
		if err != nil {
			return false, err
		}
		queued = queued || ct.RowsAffected() > 0
	}
	return queued, nil
}

// ClaimWebPushDeliveries atomically leases a small batch. Sending remains
// outside SQLite transactions; use limit=1 for a sequential network sender.
// Successful rows retain their turn key for 24h to suppress duplicate terminal
// events with different worker sequences. Event replay itself is rejected by
// the permanent ordered worker ledger before it can reach this outbox.
func (s *Store) ClaimWebPushDeliveries(ctx context.Context, limit int) ([]WebPushDelivery, error) {
	if limit < 1 || limit > 16 {
		limit = 1
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	if _, err = tx.Exec(ctx, `DELETE FROM webpush_deliveries WHERE delivery_id IN
        (SELECT delivery_id FROM webpush_deliveries WHERE expires_at<$1 ORDER BY expires_at LIMIT 256)`, now.Add(-24*time.Hour)); err != nil {
		return nil, err
	}
	// Drop inaccessible or expired queued entries in bounded batches. Even if
	// a backlog exceeds the maintenance batch, the claim below filters every row.
	if _, err = tx.Exec(ctx, `UPDATE webpush_deliveries SET status='discarded',lease_id=NULL WHERE delivery_id IN
        (SELECT pending.delivery_id FROM webpush_deliveries pending WHERE pending.status IN ('pending','sending')
         AND (pending.expires_at<=$1 OR pending.attempt>=$2 OR NOT EXISTS(SELECT 1`+webPushEligible+` AND d.delivery_id=pending.delivery_id)) LIMIT 256)`, now, webPushMaxAttempts); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT d.delivery_id,d.event_id,d.session_id,d.attempt,d.expires_at,p.subscription_id,p.endpoint,p.p256dh,p.auth`+webPushEligible+`
        AND d.status IN ('pending','sending') AND d.next_attempt_at<=$1 AND d.expires_at>$1 AND d.attempt<$2
        ORDER BY d.next_attempt_at,d.delivery_id LIMIT $3`, now, webPushMaxAttempts, limit)
	if err != nil {
		return nil, err
	}
	var deliveries []WebPushDelivery
	for rows.Next() {
		var d WebPushDelivery
		if err = rows.Scan(&d.ID, &d.EventID, &d.SessionID, &d.Attempt, &d.ExpiresAt, &d.Subscription.ID, &d.Subscription.Endpoint, &d.Subscription.P256DH, &d.Subscription.Auth); err != nil {
			rows.Close()
			return nil, err
		}
		d.LeaseID = uuid.New()
		d.Attempt++
		deliveries = append(deliveries, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, d := range deliveries {
		if _, err = tx.Exec(ctx, `UPDATE webpush_deliveries SET status='sending',lease_id=$2,attempt=$3,next_attempt_at=$4 WHERE delivery_id=$1`, d.ID, d.LeaseID, d.Attempt, now.Add(90*time.Second)); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return deliveries, nil
}

// FinishWebPushDelivery fences late completions by their lease. No response or
// provider diagnostic text is persisted, since those often contain endpoints.
func (s *Store) FinishWebPushDelivery(ctx context.Context, id, leaseID uuid.UUID, result string) error {
	if result != "delivered" && result != "retry" && result != "gone" && result != "discard" {
		return errors.New("registry: invalid Web Push delivery result")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var subscription uuid.UUID
	var attempt int
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT subscription_id,attempt,expires_at FROM webpush_deliveries WHERE delivery_id=$1 AND lease_id=$2 AND status='sending'`, id, leaseID).Scan(&subscription, &attempt, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if result == "gone" {
		if _, err = tx.Exec(ctx, `DELETE FROM webpush_subscriptions WHERE subscription_id=$1`, subscription); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	status := "discarded"
	next := time.Now().UTC()
	if result == "delivered" {
		status = "delivered"
	}
	if result == "retry" && attempt < webPushMaxAttempts {
		next = next.Add(time.Duration(1<<uint(attempt-1)) * 30 * time.Second)
		if next.Before(expires) {
			status = "pending"
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE webpush_deliveries SET status=$3,lease_id=NULL,next_attempt_at=$4 WHERE delivery_id=$1 AND lease_id=$2`, id, leaseID, status, next); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

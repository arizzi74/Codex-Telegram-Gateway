package registry

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func webPushTestKeys(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(key.Bytes()), base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
}
func webPushTestLogin(t *testing.T, s *Store) (string, []byte) {
	t.Helper()
	id := []byte(uuid.NewString())
	err := insertAdminCredential(context.Background(), s.pool, AdminCredential{ID: id, UserHandle: []byte("webpush-test-owner"), CredentialJSON: []byte(`{"authenticator":{"signCount":1}}`)})
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateAdminSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return token, id
}
func webPushTestSubscription(t *testing.T) WebPushSubscription {
	t.Helper()
	_, key := webPushTestKeys(t)
	return WebPushSubscription{Endpoint: "https://push.example.test/" + uuid.NewString(), P256DH: key, Auth: base64.RawURLEncoding.EncodeToString(make([]byte, 16))}
}
func webPushTestRegister(t *testing.T, s *Store, token string) WebPushSubscription {
	t.Helper()
	sub, err := s.UpsertWebPushSubscription(context.Background(), token, webPushTestSubscription(t))
	if err != nil {
		t.Fatal(err)
	}
	return sub
}
func webPushTerminal(t *testing.T, e eventTestEnv, seq uint64, kind, turn string) protocol.Event {
	t.Helper()
	data, err := json.Marshal(protocol.Result{TurnID: turn, Text: "PRIVATE_CONVERSATION_NOT_FOR_PUSH"})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Event{ID: uuid.NewString(), Seq: seq, WorkerID: e.worker.String(), RuntimeID: e.runtime.String(), RuntimeGeneration: 1, SessionID: e.session.String(), Kind: kind, OccurredAt: time.Now().UTC(), Data: data}
}
func webPushIngest(t *testing.T, e eventTestEnv, event protocol.Event) {
	t.Helper()
	if err := e.store.IngestEvent(context.Background(), e.worker, e.connection, event); err != nil {
		t.Fatal(err)
	}
}
func webPushCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	var count int
	if err := s.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestWebPushKeysAtomicPersistence(t *testing.T) {
	ctx := context.Background()
	s := integrationStore(t)
	if _, err := s.EnsureWebPushKeys(ctx, "", ""); !errors.Is(err, ErrWebPushKeysMissing) {
		t.Fatalf("missing keys: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan WebPushKeys, 8)
	failures := make(chan error, 8)
	for range 8 {
		private, public := webPushTestKeys(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys, err := s.EnsureWebPushKeys(ctx, private, public)
			if err != nil {
				failures <- err
			} else {
				results <- keys
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	stored, err := s.EnsureWebPushKeys(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for result := range results {
		if result != stored {
			t.Fatal("concurrent key initializers disagreed")
		}
	}
	if _, err = s.EnsureWebPushKeys(ctx, "invalid", "invalid"); !errors.Is(err, ErrWebPushInvalidSubscription) {
		t.Fatal("invalid candidate accepted")
	}
}

func TestWebPushSubscriptionAuthorizationAndLimits(t *testing.T) {
	ctx := context.Background()
	s := integrationStore(t)
	token, credential := webPushTestLogin(t, s)
	other, _ := webPushTestLogin(t, s)
	sub := webPushTestRegister(t, s, token)
	if _, err := s.UpsertWebPushSubscription(ctx, other, sub); !errors.Is(err, ErrWebPushInvalidSubscription) {
		t.Fatalf("endpoint ownership transfer: %v", err)
	}
	if _, err := s.GetWebPushSubscription(ctx, other, sub.ID); !errors.Is(err, ErrWebPushSubscriptionNotFound) {
		t.Fatalf("read foreign subscription: %v", err)
	}
	if err := s.DeleteWebPushSubscription(ctx, other, sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebPushSubscription(ctx, token, sub.ID); err != nil {
		t.Fatal("foreign delete removed subscription")
	}
	updated, err := s.UpsertWebPushSubscription(ctx, token, sub)
	if err != nil || updated.ID != sub.ID {
		t.Fatalf("upsert not idempotent: %v", err)
	}
	for range 15 {
		webPushTestRegister(t, s, token)
	}
	if _, err = s.UpsertWebPushSubscription(ctx, token, webPushTestSubscription(t)); !errors.Is(err, ErrWebPushSubscriptionLimit) {
		t.Fatalf("device cap: %v", err)
	}
	if err = s.RevokeAdminCredential(ctx, credential); err != nil {
		t.Fatal(err)
	}
	if n := webPushCount(t, s, "webpush_subscriptions"); n != 0 {
		t.Fatalf("credential revocation retained %d devices", n)
	}
	if _, err = s.UpsertWebPushSubscription(ctx, token, sub); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("revoked login accepted: %v", err)
	}
	for _, mutate := range []func(*WebPushSubscription){func(s *WebPushSubscription) { s.Endpoint = "http://push.example.test/a" }, func(s *WebPushSubscription) { s.Endpoint = "https://user:pass@push.example.test/a" }, func(s *WebPushSubscription) { s.P256DH = "not-a-key" }, func(s *WebPushSubscription) { s.Auth = "not-a-key" }} {
		bad := webPushTestSubscription(t)
		mutate(&bad)
		if _, err = s.UpsertWebPushSubscription(ctx, other, bad); !errors.Is(err, ErrWebPushInvalidSubscription) {
			t.Fatal("malformed capability accepted")
		}
	}
}

func TestWebPushOutboxAtomicDedupeAndRestart(t *testing.T) {
	ctx := context.Background()
	e := newEventTestEnv(t)
	webPushIngest(t, e, e.discovery(t))
	token, _ := webPushTestLogin(t, e.store)
	sub := webPushTestRegister(t, e.store, token)
	wake, cancel := e.store.SubscribeWebPushNotifications()
	defer cancel()
	event := webPushTerminal(t, e, 2, "turn_completed", "turn-1")
	webPushIngest(t, e, event)
	select {
	case <-wake:
	default:
		t.Fatal("committed completion did not wake sender")
	}
	webPushIngest(t, e, event)
	duplicate := webPushTerminal(t, e, 3, "turn_interrupted", "turn-1")
	webPushIngest(t, e, duplicate)
	if n := webPushCount(t, e.store, "webpush_deliveries"); n != 1 {
		t.Fatalf("terminal duplicate/replay made %d deliveries", n)
	}
	select {
	case <-wake:
		t.Fatal("duplicate event woke sender")
	default:
	}
	claimed, err := e.store.ClaimWebPushDeliveries(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim %#v: %v", claimed, err)
	}
	d := claimed[0]
	if d.Subscription.ID != sub.ID || d.EventID.String() != event.ID || d.SessionID != e.session || d.Attempt != 1 {
		t.Fatalf("wrong scoped delivery: %#v", d)
	}
	encoded, _ := json.Marshal(d)
	if bytesContain(encoded, "PRIVATE_CONVERSATION") {
		t.Fatal("conversation leaked in delivery")
	}
	if again, err := e.store.ClaimWebPushDeliveries(ctx, 1); err != nil || len(again) != 0 {
		t.Fatal("leased delivery claimed twice")
	}
	// Reopen the same durable file: a gateway restart retains the lease rather
	// than blindly replaying a possibly accepted network request.
	var path string
	if err = e.store.pool.QueryRow(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if again, err := reopened.ClaimWebPushDeliveries(ctx, 1); err != nil || len(again) != 0 {
		t.Fatal("restart lost pending lease")
	}
	if err = reopened.FinishWebPushDelivery(ctx, d.ID, d.LeaseID, "delivered"); err != nil {
		t.Fatal(err)
	}
	if again, err := e.store.ClaimWebPushDeliveries(ctx, 1); err != nil || len(again) != 0 {
		t.Fatal("delivered item replayed")
	}
	// A failure after the event/outbox INSERT must roll back both, including
	// the worker watermark. The trigger targets the final watermark write.
	if _, err = e.store.pool.Exec(ctx, `CREATE TEMP TRIGGER reject_push_event BEFORE UPDATE ON worker_event_watermarks BEGIN SELECT RAISE(ABORT,'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	failed := webPushTerminal(t, e, 4, "turn_failed", "turn-2")
	if err = e.store.IngestEvent(ctx, e.worker, e.connection, failed); err == nil {
		t.Fatal("rollback trigger ignored")
	}
	if n := webPushCount(t, e.store, "webpush_deliveries"); n != 1 {
		t.Fatalf("rollback retained outbox item: %d", n)
	}
	select {
	case <-wake:
		t.Fatal("rolled back event woke sender")
	default:
	}
	if _, err = e.store.pool.Exec(ctx, `DROP TRIGGER reject_push_event`); err != nil {
		t.Fatal(err)
	}
	webPushIngest(t, e, failed)
	if n := webPushCount(t, e.store, "webpush_deliveries"); n != 2 {
		t.Fatal("retry did not atomically commit")
	}
}

func bytesContain(b []byte, s string) bool { return strings.Contains(string(b), s) }

func TestWebPushCompletionEligibility(t *testing.T) {
	for _, scenario := range []string{"old-generation", "archived-helper", "deleted", "before-registration", "old-backlog", "future-clock", "missing-turn", "nonterminal"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			e := newEventTestEnv(t)
			webPushIngest(t, e, e.discovery(t))
			token, _ := webPushTestLogin(t, e.store)
			webPushTestRegister(t, e.store, token)
			event := webPushTerminal(t, e, 2, "turn_completed", "turn-x")
			switch scenario {
			case "old-generation":
				event.RuntimeGeneration = 0
			case "archived-helper":
				_, _ = e.store.pool.Exec(ctx, `UPDATE sessions SET archived=TRUE WHERE session_id=$1`, e.session)
			case "deleted":
				_, _ = e.store.pool.Exec(ctx, `UPDATE sessions SET metadata='{"deleted":true}' WHERE session_id=$1`, e.session)
			case "before-registration":
				event.OccurredAt = time.Now().UTC().Add(-time.Minute)
			case "old-backlog":
				event.OccurredAt = time.Now().UTC().Add(-time.Hour)
			case "future-clock":
				event.OccurredAt = time.Now().UTC().Add(time.Hour)
			case "missing-turn":
				event.Data = []byte(`{"text":"no turn identity"}`)
			case "nonterminal":
				event.Kind = "turn_started"
			}
			webPushIngest(t, e, event)
			if n := webPushCount(t, e.store, "webpush_deliveries"); n != 0 {
				t.Fatalf("ineligible event enqueued %d pushes", n)
			}
		})
	}
}

func TestWebPushDeliveryLeaseRetryExpiryAndRevocation(t *testing.T) {
	ctx := context.Background()
	e := newEventTestEnv(t)
	webPushIngest(t, e, e.discovery(t))
	token, _ := webPushTestLogin(t, e.store)
	webPushTestRegister(t, e.store, token)
	webPushIngest(t, e, webPushTerminal(t, e, 2, "turn_failed", "turn-1"))
	claim := func() WebPushDelivery {
		t.Helper()
		rows, err := e.store.ClaimWebPushDeliveries(ctx, 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("claim count %d err %v", len(rows), err)
		}
		return rows[0]
	}
	first := claim()
	if _, err := e.store.pool.Exec(ctx, `UPDATE webpush_deliveries SET next_attempt_at=$1`, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	second := claim()
	if first.LeaseID == second.LeaseID || second.Attempt != 2 {
		t.Fatal("expired claim not fenced")
	}
	if err := e.store.FinishWebPushDelivery(ctx, first.ID, first.LeaseID, "gone"); err != nil {
		t.Fatal(err)
	}
	if n := webPushCount(t, e.store, "webpush_subscriptions"); n != 1 {
		t.Fatal("stale sender deleted device")
	}
	if err := e.store.FinishWebPushDelivery(ctx, second.ID, second.LeaseID, "retry"); err != nil {
		t.Fatal(err)
	}
	if rows, err := e.store.ClaimWebPushDeliveries(ctx, 1); err != nil || len(rows) != 0 {
		t.Fatal("retry ignored backoff")
	}
	if _, err := e.store.pool.Exec(ctx, `UPDATE webpush_deliveries SET next_attempt_at=$1,attempt=5`, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	final := claim()
	if err := e.store.FinishWebPushDelivery(ctx, final.ID, final.LeaseID, "retry"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := e.store.pool.QueryRow(ctx, `SELECT status FROM webpush_deliveries`).Scan(&state); err != nil || state != "discarded" {
		t.Fatalf("retry cap %s %v", state, err)
	}
	webPushIngest(t, e, webPushTerminal(t, e, 3, "turn_interrupted", "turn-2"))
	if _, err := e.store.pool.Exec(ctx, `UPDATE admin_sessions SET expires_at=$1 WHERE token_hash=$2`, time.Now().Add(-time.Hour), hashSecret(token)); err != nil {
		t.Fatal(err)
	}
	// Background opt-in survives normal eight-hour cookie expiry.
	expiredLogin := claim()
	if err := e.store.FinishWebPushDelivery(ctx, expiredLogin.ID, expiredLogin.LeaseID, "delivered"); err != nil {
		t.Fatal(err)
	}
	webPushIngest(t, e, webPushTerminal(t, e, 4, "turn_completed", "turn-3"))
	if err := e.store.RevokeAdminSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	if n := webPushCount(t, e.store, "webpush_subscriptions"); n != 0 {
		t.Fatal("logout retained device")
	}
	if n := webPushCount(t, e.store, "webpush_deliveries"); n != 0 {
		t.Fatal("logout retained outbox")
	}
}

func TestWebPushClaimRechecksSessionAndExpiry(t *testing.T) {
	for _, scenario := range []string{"archived", "deleted", "worker-disabled", "credential-revoked", "session-revoked", "delivery-expired", "provider-gone"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			e := newEventTestEnv(t)
			webPushIngest(t, e, e.discovery(t))
			token, credential := webPushTestLogin(t, e.store)
			webPushTestRegister(t, e.store, token)
			webPushIngest(t, e, webPushTerminal(t, e, 2, "turn_completed", "turn-1"))
			var err error
			switch scenario {
			case "archived":
				_, err = e.store.pool.Exec(ctx, `UPDATE sessions SET archived=TRUE`)
			case "deleted":
				_, err = e.store.pool.Exec(ctx, `UPDATE sessions SET metadata='{"deleted":true}'`)
			case "worker-disabled":
				_, err = e.store.pool.Exec(ctx, `UPDATE workers SET enabled=FALSE`)
			case "credential-revoked":
				_, err = e.store.pool.Exec(ctx, `UPDATE admin_credentials SET revoked_at=$1 WHERE credential_id=$2`, time.Now(), credential)
			case "session-revoked":
				_, err = e.store.pool.Exec(ctx, `UPDATE admin_sessions SET revoked_at=$1`, time.Now())
			case "delivery-expired":
				_, err = e.store.pool.Exec(ctx, `UPDATE webpush_deliveries SET expires_at=$1`, time.Now().Add(-time.Second))
			case "provider-gone":
				var rows []WebPushDelivery
				rows, err = e.store.ClaimWebPushDeliveries(ctx, 1)
				if err == nil && len(rows) == 1 {
					err = e.store.FinishWebPushDelivery(ctx, rows[0].ID, rows[0].LeaseID, "gone")
				} else {
					err = fmt.Errorf("initial claim: %v", err)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if rows, err := e.store.ClaimWebPushDeliveries(ctx, 1); err != nil || len(rows) != 0 {
				t.Fatalf("ineligible delivery claimed %d %v", len(rows), err)
			}
		})
	}
}

package registry

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func insertPickerDelivery(t *testing.T, store *Store, view string, chat, topic int64) Delivery {
	t.Helper()
	id := uuid.NewString()
	payload, err := json.Marshal(AcceptResult{View: view, Action: "sessions"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), `INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,message_thread_id,kind,payload,status,next_attempt_at) VALUES($1,'bot',$2,$3,'ui_response',$4,'sending',$5)`, id, chat, topic, string(payload), time.Now().UTC().Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	return Delivery{ID: id, BotID: "bot", ChatID: chat, TopicID: topic, Kind: "ui_response", Payload: payload}
}

func pickerCallback(t *testing.T, env eventTestEnv, action, origin string) string {
	t.Helper()
	callback := Callback{Action: action, BotID: "bot", UserID: 1, ChatID: 20, TopicID: 3, OriginDeliveryID: origin, RuntimeID: env.runtime, Generation: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if action == "select" || action == "connect" || action == "status" {
		callback.SessionID = env.session
	}
	token, err := env.store.CreateCallback(t.Context(), callback)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func acceptPicker(t *testing.T, env eventTestEnv, updateID int64, token string, message int64) AcceptResult {
	t.Helper()
	result, err := env.store.AcceptTelegram(t.Context(), IncomingUpdate{BotID: "bot", UserID: 1, ChatID: 20, TopicID: 3, UpdateID: updateID, CallbackToken: token, CallbackMessageID: message})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func finishPickerCleanup(t *testing.T, store *Store, row Delivery, wantMessage int64) {
	t.Helper()
	var cleanup PickerCleanup
	if err := json.Unmarshal(row.Payload, &cleanup); err != nil || row.Kind != "picker_cleanup" || cleanup.MessageID != wantMessage {
		t.Fatalf("cleanup = %+v %+v, error %v", row, cleanup, err)
	}
	if err := store.MarkDeliverySent(t.Context(), row.ID, wantMessage, "", "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestSessionPickerTransitionsRetireExactPreviousStep(t *testing.T) {
	for _, test := range []struct{ action, view, next string }{
		{"sessions", "runtime_picker", "sessions"},
		{"sessions", "sessions", "sessions"},
		{"select", "sessions", "selected"},
		{"connect", "instances", "selected"},
		{"new", "sessions", "new_session_name"},
	} {
		t.Run(test.view+"/"+test.action, func(t *testing.T) {
			env := progressEnv(t)
			source := insertPickerDelivery(t, env.store, test.view, 20, 3)
			if err := env.store.MarkDeliverySent(t.Context(), source.ID, 100, "", "", ""); err != nil {
				t.Fatal(err)
			}
			unrelated := insertPickerDelivery(t, env.store, "sessions", 20, 3)
			if err := env.store.MarkDeliverySent(t.Context(), unrelated.ID, 200, "", "", ""); err != nil {
				t.Fatal(err)
			}
			token := pickerCallback(t, env, test.action, source.ID)
			result := acceptPicker(t, env, 1, token, 100)
			if result.View != test.next || result.PreviousPickerID != source.ID {
				t.Fatalf("result = %+v", result)
			}
			rows := claimProgress(t, env.store, 1)
			if skip, err := env.store.SuppressTelegramDelivery(t.Context(), rows[0].ID); skip || err != nil {
				t.Fatalf("cleanup suppressed: %v %v", skip, err)
			}
			var responseID string
			if err := env.store.pool.QueryRow(t.Context(), `SELECT delivery_id FROM telegram_deliveries WHERE json_extract(payload,'$.previous_picker_id')=$1`, source.ID).Scan(&responseID); err != nil {
				t.Fatal(err)
			}
			if skip, err := env.store.SuppressTelegramDelivery(t.Context(), responseID); skip || !errors.Is(err, ErrTelegramPickerCleanupPending) {
				t.Fatalf("next selection bypassed cleanup: %v %v", skip, err)
			}
			if err := env.store.RetryDelivery(t.Context(), rows[0].ID, time.Hour, "retry Telegram deletion"); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(t.Context(), env.store.pool.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			claimProgress(t, reopened, 0)
			if _, err := reopened.pool.Exec(t.Context(), `UPDATE telegram_deliveries SET next_attempt_at=$2 WHERE delivery_id=$1`, rows[0].ID, time.Now().UTC().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			finishPickerCleanup(t, reopened, claimProgress(t, reopened, 1)[0], 100)
			next := claimProgress(t, reopened, 1)[0]
			if next.ID != responseID {
				t.Fatalf("wrong successor: %+v", next)
			}
			if skip, err := reopened.SuppressTelegramDelivery(t.Context(), next.ID); skip || err != nil {
				t.Fatalf("completed cleanup still blocks next step: %v %v", skip, err)
			}
			var revoked bool
			if err := reopened.pool.QueryRow(t.Context(), `SELECT visibility_revoked FROM telegram_deliveries WHERE delivery_id=$1`, unrelated.ID).Scan(&revoked); err != nil || revoked {
				t.Fatalf("unrelated picker affected: %v %v", revoked, err)
			}
		})
	}
}

func TestSessionPickerLateMultipartCheckpointAndProgressOrdering(t *testing.T) {
	env := progressEnv(t)
	ctx := t.Context()
	source := insertPickerDelivery(t, env.store, "sessions", 20, 3)
	if _, err := env.store.PrepareDeliveryChunks(ctx, source.ID, []json.RawMessage{json.RawMessage(`{"text":"list"}`), json.RawMessage(`{"text":"buttons"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.MarkDeliveryChunkSent(ctx, source.ID, 0, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	token := pickerCallback(t, env, "select", source.ID)
	if result := acceptPicker(t, env, 1, token, 100); result.View != "selected" {
		t.Fatalf("result = %+v", result)
	}
	progressEvent(t, env, 2, "turn_started", "turn-a", "")
	progressEvent(t, env, 3, "agent_progress_message", "turn-a", "")
	progressEvent(t, env, 4, "tool_progress_message", "turn-a", "")
	finishPickerCleanup(t, env.store, claimProgress(t, env.store, 1)[0], 100)
	claimProgress(t, env.store, 0) // The previous network send is still in flight.
	if skip, err := env.store.SuppressTelegramDelivery(ctx, source.ID); !skip || err != nil {
		t.Fatalf("retired multipart list can still send: %v %v", skip, err)
	}
	if err := env.store.MarkDeliveryChunkSent(ctx, source.ID, 1, 101, "", "", ""); err != nil {
		t.Fatal(err)
	}
	finishPickerCleanup(t, env.store, claimProgress(t, env.store, 1)[0], 101)
	confirmation := claimProgress(t, env.store, 1)[0]
	if confirmation.Kind != "ui_response" {
		t.Fatalf("progress preceded confirmation: %+v", confirmation)
	}
	if err := env.store.MarkDeliverySent(ctx, confirmation.ID, 102, "", "", ""); err != nil {
		t.Fatal(err)
	}
	claimProgress(t, env.store, 2)
	if result := acceptPicker(t, env, 1, token, 100); !result.Duplicate {
		t.Fatalf("duplicate accepted: %+v", result)
	}
	if result := acceptPicker(t, env, 2, token, 100); result.ErrorCode != "callback_invalid" || result.PreviousPickerID != "" {
		t.Fatalf("used callback accepted: %+v", result)
	}
	var cleanups int
	if err := env.store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_deliveries WHERE kind='picker_cleanup'`).Scan(&cleanups); err != nil || cleanups != 2 {
		t.Fatalf("duplicate cleanup = %d, %v", cleanups, err)
	}
}

func TestSessionPickerCleanupRejectedAndUnrelatedCallbacks(t *testing.T) {
	for _, test := range []string{"status", "expired", "wrong_user", "stale_runtime", "other_topic", "other_chat", "wrong_view"} {
		t.Run(test, func(t *testing.T) {
			env := progressEnv(t)
			view, action, chat, topic := "sessions", "select", int64(20), int64(3)
			switch test {
			case "status":
				action = "status"
			case "other_topic":
				topic++
			case "other_chat":
				chat++
			case "wrong_view":
				view = "input_prompt"
			}
			source := insertPickerDelivery(t, env.store, view, chat, topic)
			if err := env.store.MarkDeliverySent(t.Context(), source.ID, 100, "", "", ""); err != nil {
				t.Fatal(err)
			}
			token := pickerCallback(t, env, action, source.ID)
			var err error
			switch test {
			case "expired":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE telegram_callbacks SET expires_at=$1 WHERE token=$2`, time.Now().UTC().Add(-time.Hour), token)
			case "wrong_user":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE telegram_callbacks SET telegram_user_id=2 WHERE token=$1`, token)
			case "stale_runtime":
				_, err = env.store.pool.Exec(t.Context(), `UPDATE runtimes SET generation=2 WHERE runtime_id=$1`, env.runtime)
			}
			if err != nil {
				t.Fatal(err)
			}
			result := acceptPicker(t, env, 1, token, 100)
			var count int
			if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_deliveries WHERE kind='picker_cleanup'`).Scan(&count); err != nil || count != 0 || result.PreviousPickerID != "" {
				t.Fatalf("unrelated callback cleaned source: %+v count=%d %v", result, count, err)
			}
			var revoked bool
			if err := env.store.pool.QueryRow(t.Context(), `SELECT visibility_revoked FROM telegram_deliveries WHERE delivery_id=$1`, source.ID).Scan(&revoked); err != nil || revoked {
				t.Fatalf("source revoked: %v %v", revoked, err)
			}
		})
	}
}

func TestSessionPickerLegacyCallbackCleansExactAuthenticatedMessage(t *testing.T) {
	env := progressEnv(t)
	token := pickerCallback(t, env, "select", "")
	result := acceptPicker(t, env, 1, token, 105)
	if result.View != "selected" || result.PreviousPickerID == "" {
		t.Fatalf("result = %+v", result)
	}
	cleanup := claimProgress(t, env.store, 1)[0]
	if cleanup.ID != result.PreviousPickerID {
		t.Fatalf("legacy dependency = %+v", result)
	}
	finishPickerCleanup(t, env.store, cleanup, 105)
	claimProgress(t, env.store, 1)
}

func TestSessionPickerRetiredSiblingCannotChangeSelectionOrStartWizard(t *testing.T) {
	env := progressEnv(t)
	source := insertPickerDelivery(t, env.store, "sessions", 20, 3)
	if err := env.store.MarkDeliverySent(t.Context(), source.ID, 100, "", "", ""); err != nil {
		t.Fatal(err)
	}
	first := pickerCallback(t, env, "select", source.ID)
	newSession := pickerCallback(t, env, "new", source.ID)
	other := uuid.New()
	insertRouteSession(t, env, other, "thread-other", "")
	otherEnv := env
	otherEnv.session = other
	sibling := pickerCallback(t, otherEnv, "select", source.ID)
	if result := acceptPicker(t, env, 1, first, 100); result.View != "selected" {
		t.Fatalf("first = %+v", result)
	}
	for i, token := range []string{sibling, newSession} {
		if result := acceptPicker(t, env, int64(i+2), token, 100); result.ErrorCode != "callback_invalid" {
			t.Fatalf("retired sibling accepted = %+v", result)
		}
	}
	var selected string
	if err := env.store.pool.QueryRow(t.Context(), `SELECT session_id FROM telegram_bindings WHERE bot_id='bot' AND chat_id=20 AND message_thread_id=3 AND user_id=1`).Scan(&selected); err != nil || selected != env.session.String() {
		t.Fatalf("retired sibling changed selection: %s %v", selected, err)
	}
	var wizards int
	if err := env.store.pool.QueryRow(t.Context(), `SELECT count(*) FROM telegram_session_wizards`).Scan(&wizards); err != nil || wizards != 0 {
		t.Fatalf("retired new callback started wizard: %d %v", wizards, err)
	}
}

func TestSessionPickerCleanupUsesOutstandingQueueIndex(t *testing.T) {
	env := progressEnv(t)
	plan := pollingQueryPlan(t, env.store, `SELECT `+pendingPickerCleanupSQL+` FROM telegram_deliveries delivery WHERE delivery.delivery_id=$1`, uuid.NewString())
	if !strings.Contains(plan, "SEARCH cleanup USING INDEX telegram_deliveries_queue_idx") || strings.Contains(plan, "SCAN cleanup") || strings.Contains(plan, "SCAN source") {
		t.Fatalf("picker dependency scans completed history:\n%s", plan)
	}
}

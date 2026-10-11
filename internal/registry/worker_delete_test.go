package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
)

func TestDeleteRevokedWorkerPurgesReferencesAndPreservesOtherState(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	token, _ := webPushTestLogin(t, s)
	sub := webPushTestRegister(t, s, token)
	if _, err := s.PutAdminDraft(ctx, token, uuid.New(), 1, draftCiphertext('p')); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO telegram_chat_modes(bot_id,user_id,chat_id,message_thread_id,multi_session) VALUES('bot',100,20,0,1)`,
		`INSERT INTO telegram_selection_revisions(bot_id,user_id,chat_id,message_thread_id,revision) VALUES('bot',100,20,0,19)`,
		`INSERT INTO telegram_chat_policies(bot_id,allowed_chat_ids) VALUES('bot','[20]')`,
	} {
		if _, err := s.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.CreateWorkerEnrollment(ctx, "restricted"); err != nil {
		t.Fatal(err)
	}
	survivor := seedDeletedWorkerFixture(t, s, sub.ID.String(), 10)
	baseline := workerDeletionSnapshot(t, s)
	target := seedDeletedWorkerFixture(t, s, sub.ID.String(), 100)
	if err := s.RevokeWorker(ctx, target.worker); err != nil {
		t.Fatal(err)
	}
	guards := workerDeletionGuards(t, s)
	changes, unsubscribe, err := s.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	if err := s.DeleteRevokedWorker(ctx, target.worker); err != nil {
		t.Fatal(err)
	}
	activityChanged(t, changes)
	after := workerDeletionSnapshot(t, s)
	for _, receipt := range baseline["telegram_updates"] {
		found := false
		for _, retained := range after["telegram_updates"] {
			found = found || retained == receipt
		}
		if !found {
			t.Errorf("unrelated Telegram receipt changed or disappeared: %s", receipt)
		}
	}
	// Global Telegram receipt identities remain anonymous to prevent delayed
	// duplicate webhooks from running old input on a new selected worker.
	delete(baseline, "telegram_updates")
	delete(after, "telegram_updates")
	if !reflect.DeepEqual(after, baseline) {
		for table, want := range baseline {
			if !reflect.DeepEqual(after[table], want) {
				t.Errorf("table %s after deletion = %v, want unrelated rows %v", table, after[table], want)
			}
		}
	}
	var receipts, anonymous int
	if err := s.pool.QueryRow(ctx, `SELECT count(*),sum(raw='{}' AND user_id IS NULL AND chat_id IS NULL)
        FROM telegram_updates WHERE update_id BETWEEN 100 AND 105`).Scan(&receipts, &anonymous); err != nil || receipts != 6 || anonymous != receipts {
		t.Fatalf("worker receipts were not anonymized: receipts=%d anonymous=%d err=%v", receipts, anonymous, err)
	}
	for update := int64(100); update <= 105; update++ {
		result, err := s.AcceptTelegram(ctx, IncomingUpdate{BotID: "bot", UpdateID: update, UserID: 10, ChatID: 20, Action: "text", Text: "old private prompt"})
		if err != nil || !result.Duplicate {
			t.Fatalf("old update %d replay = %+v, %v", update, result, err)
		}
	}
	replayed := workerDeletionSnapshot(t, s)
	delete(replayed, "telegram_updates")
	if !reflect.DeepEqual(replayed, after) {
		t.Fatal("anonymous update replay changed commands, selections or other state")
	}
	workerDeletionCheckIntegrity(t, s, guards, survivor)
	if err := s.CheckConnection(ctx, target.worker, target.connection); !errors.Is(err, ErrConnectionFenced) {
		t.Fatalf("purged connection accepted: %v", err)
	}
	if err := s.IngestEvent(ctx, target.worker, target.connection, target.discovery(t)); !errors.Is(err, ErrConnectionFenced) {
		t.Fatalf("purged event ingestion accepted: %v", err)
	}
	if err := s.RecordHeartbeat(ctx, Heartbeat{WorkerID: target.worker, ConnectionID: target.connection}); !errors.Is(err, ErrConnectionFenced) {
		t.Fatalf("purged heartbeat accepted: %v", err)
	}
	if err := s.DeleteRevokedWorker(ctx, target.worker); !errors.Is(err, ErrWorkerNotFound) {
		t.Fatalf("second deletion = %v", err)
	}
}

func TestDeleteRevokedWorkerRefusesEnabledAndUnknownWithoutWrites(t *testing.T) {
	s := integrationStore(t)
	env := seedDeletedWorkerFixture(t, s, "", 10)
	before := workerDeletionSnapshot(t, s)
	guards := workerDeletionGuards(t, s)
	changes, unsubscribe, err := s.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	for _, tc := range []struct {
		id   uuid.UUID
		want error
	}{{env.worker, ErrWorkerNotRevoked}, {uuid.New(), ErrWorkerNotFound}, {uuid.Nil, ErrWorkerNotFound}} {
		if err := s.DeleteRevokedWorker(t.Context(), tc.id); !errors.Is(err, tc.want) {
			t.Errorf("delete %s = %v, want %v", tc.id, err, tc.want)
		}
		if after := workerDeletionSnapshot(t, s); !reflect.DeepEqual(after, before) {
			t.Fatal("refused deletion changed database rows")
		}
	}
	select {
	case <-changes:
		t.Fatal("refused deletion notified activity")
	default:
	}
	workerDeletionCheckIntegrity(t, s, guards, env)
}

func TestDeleteRevokedWorkerRollsBackRowsAndJournalGuards(t *testing.T) {
	s := integrationStore(t)
	env := seedDeletedWorkerFixture(t, s, "", 10)
	if err := s.RevokeWorker(t.Context(), env.worker); err != nil {
		t.Fatal(err)
	}
	// Fail after commands/events/dependent rows were already deleted, proving
	// rollback restores all earlier row deletions and transactional trigger DDL.
	if _, err := s.pool.Exec(t.Context(), `CREATE TRIGGER injected_worker_delete_failure BEFORE DELETE ON workers
        BEGIN SELECT RAISE(ABORT,'injected worker deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := workerDeletionSnapshot(t, s)
	guards := workerDeletionGuards(t, s)
	changes, unsubscribe, err := s.SubscribeSessionActivity()
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	if err := s.DeleteRevokedWorker(t.Context(), env.worker); err == nil || !strings.Contains(err.Error(), "injected worker deletion failure") {
		t.Fatalf("failure injection error = %v", err)
	}
	if after := workerDeletionSnapshot(t, s); !reflect.DeepEqual(after, before) {
		t.Fatal("failed deletion changed database rows")
	}
	select {
	case <-changes:
		t.Fatal("rolled back deletion notified activity")
	default:
	}
	workerDeletionCheckIntegrity(t, s, guards, env)
	if _, err := s.pool.Exec(t.Context(), `DROP TRIGGER injected_worker_delete_failure`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRevokedWorker(t.Context(), env.worker); err != nil {
		t.Fatalf("retry after rollback failed: %v", err)
	}
}

// Populate every worker-dependent table, including JSON-only references and
// reverse links from rendered keyboards, cleanup messages and wizard results.
func seedDeletedWorkerFixture(t *testing.T, s *Store, subscription string, sequence int64) eventTestEnv {
	t.Helper()
	ctx := t.Context()
	env := inventoryWorker(t, s, fmt.Sprintf("worker-%d", sequence))
	event := env.discovery(t)
	if err := s.IngestEvent(ctx, env.worker, env.connection, event); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("seed %s: %v", query, err)
		}
	}
	command, approval, request, wizard := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	delivery, helper, edit, progress, keyboard, nested, cleanup := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	callback := "worker-callback-" + env.worker.String()
	exec(`INSERT INTO commands(command_id,source,worker_id,runtime_id,runtime_generation,session_id,operation,payload,status,
        telegram_bot_id,telegram_update_id,telegram_user_id,telegram_chat_id)
        VALUES($1,'telegram',$2,$3,1,$4,'read_history',$5,'completed','bot',$6,$6,20)`,
		command, env.worker, env.runtime, env.session, fmt.Sprintf(`{"worker_id":%q,"private":"history"}`, env.worker), sequence)
	exec(`INSERT INTO approvals(approval_id,worker_id,runtime_id,runtime_generation,session_id,codex_request_id,codex_thread_id,
        approval_type,request_payload,state,requested_at,response_command_id)
        VALUES($1,$2,$3,1,$4,'request','thread-1','input','{"questions":[{"id":"q","prompt":"Private?"}]}','approved',$5,$6)`,
		approval, env.worker, env.runtime, env.session, time.Now(), command)
	exec(`INSERT INTO telegram_question_answers(approval_id,question_id,question,answer) VALUES($1,'q','{"id":"q"}','private answer')`, approval)
	exec(`INSERT INTO worker_update_requests(request_id,worker_id,worker_name,codex_report) VALUES($1,$2,'Worker','{"profiles":[]}')`, request, env.worker)
	exec(`INSERT INTO worker_update_watchers(request_id,bot_id,user_id,chat_id) VALUES($1,'bot',$2,20)`, request, sequence)
	exec(`INSERT INTO worker_enrollments(enrollment_id,code_hash,service_access,created_at,expires_at,consumed_at,worker_id)
        VALUES($1,$2,'full',$3,$4,$3,$5)`, uuid.New(), auth.HashWorkerToken(uuid.NewString()), time.Now(), time.Now().Add(time.Hour), env.worker)
	exec(`INSERT INTO telegram_bindings(bot_id,user_id,chat_id,session_id) VALUES('bot',$1,20,$2)`, sequence, env.session)
	exec(`INSERT OR IGNORE INTO telegram_session_aliases(session_id,alias) VALUES($1,$2)`, env.session, fmt.Sprintf("s%d", sequence))
	exec(`INSERT INTO bot_message_routes(bot_id,chat_id,message_id,session_id,approval_id,question_id) VALUES('bot',20,$1,$2,$3,'q')`, sequence+1000, env.session, approval)
	exec(`INSERT INTO session_settings(session_id,worker_id,runtime_id,runtime_generation,revision,model,reasoning_effort) VALUES($1,$2,$3,1,1,'model','high')`, env.session, env.worker, env.runtime)
	exec(`INSERT INTO telegram_session_wizards(wizard_id,bot_id,user_id,chat_id,revision,kind,phase,runtime_id,expires_at)
        VALUES($1,'bot',$2,20,1,'new','workspace',$3,$4)`, wizard, sequence, env.runtime, time.Now().Add(time.Hour))
	for index, entry := range []struct {
		column string
		id     uuid.UUID
	}{{"session_id", env.session}, {"command_id", command}} {
		exec(`INSERT INTO telegram_session_wizards(wizard_id,bot_id,user_id,chat_id,revision,kind,phase,`+entry.column+`,expires_at)
            VALUES($1,'bot',$2,21,1,'new','workspace',$3,$4)`, uuid.New(), sequence+int64(index), entry.id, time.Now().Add(time.Hour))
	}
	for _, entry := range []struct {
		id      uuid.UUID
		kind    string
		payload string
	}{
		{delivery, "turn_completed", `{"text":"private event"}`},
		{helper, "ui_response", fmt.Sprintf(`{"view":"input_prompt","approval_id":%q,"question_id":"q"}`, approval)},
		{edit, "question_answered", fmt.Sprintf(`{"message_id":%d}`, sequence+1000)},
		{progress, "progress", `{"text":"working"}`},
		{keyboard, "ui_response", `{"view":"instances"}`},
		{nested, "ui_response", fmt.Sprintf(`{"data":{"session":{"session_id":%q}},"worker_updates":[{"request_id":%q}]}`, env.session, request)},
		{cleanup, "input_reply_cleanup", fmt.Sprintf(`{"message_id":%d}`, sequence+2000)},
	} {
		exec(`INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload,telegram_message_id) VALUES($1,'bot',20,$2,$3,$4)`, entry.id, entry.kind, entry.payload, sequence+3000)
	}
	exec(`UPDATE telegram_deliveries SET event_id=$2 WHERE delivery_id=$1`, delivery, event.ID)
	exec(`INSERT INTO telegram_delivery_chunks(delivery_id,chunk_index,payload,telegram_message_id) VALUES($1,0,$2,$3)`, delivery, `{"text":"private history chunk"}`, sequence+4000)
	exec(`INSERT INTO telegram_delivery_chunks(delivery_id,chunk_index,payload) VALUES($1,0,$2)`, keyboard,
		fmt.Sprintf(`{"reply_markup":{"inline_keyboard":[[{"callback_data":%q}]]}}`, "cb:"+callback))
	exec(`INSERT INTO telegram_input_reply_helpers(delivery_id,approval_id,question_id) VALUES($1,$2,'q')`, helper, approval)
	exec(`INSERT INTO telegram_input_reply_messages(bot_id,chat_id,message_id,delivery_id) VALUES('bot',20,$1,$2)`, sequence+2000, helper)
	exec(`INSERT INTO telegram_question_answer_edits(delivery_id,approval_id,question_id,version) VALUES($1,$2,'q',0)`, edit, approval)
	exec(`INSERT INTO telegram_progress_messages(cleanup_id,delivery_id,chunk_index,bot_id,chat_id,message_thread_id,telegram_message_id,
        runtime_id,runtime_generation,session_id,turn_id) VALUES($1,$2,0,'bot',20,0,$3,$4,1,$5,'turn')`, uuid.New(), progress, sequence+5000, env.runtime, env.session)
	for _, payload := range []string{
		fmt.Sprintf(`{"runtime_id":%q}`, env.runtime),
		fmt.Sprintf(`{"wizard_id":%q}`, wizard),
		fmt.Sprintf(`{"origin_delivery_id":%q}`, nested),
	} {
		exec(`INSERT INTO telegram_callbacks(token,action,telegram_user_id,payload,expires_at) VALUES($1,'sessions',$2,$3,$4)`,
			uuid.NewString(), sequence, payload, time.Now().Add(time.Hour))
	}
	exec(`INSERT INTO telegram_callbacks(token,action,telegram_user_id,session_id,command_id,approval_id,payload,expires_at)
        VALUES($1,'input',$2,$3,$4,$5,'{}',$6)`, callback, sequence, env.session, command, approval, time.Now().Add(time.Hour))
	if subscription != "" {
		exec(`INSERT INTO webpush_deliveries(delivery_id,subscription_id,event_id,session_id,runtime_generation,turn_id,next_attempt_at,expires_at,created_at)
            VALUES($1,$2,$3,$4,1,'turn',$5,$6,$5)`, uuid.New(), subscription, event.ID, env.session, time.Now(), time.Now().Add(time.Hour))
	}
	for offset, raw := range []string{
		`{"message":{"text":"private original prompt"}}`,
		fmt.Sprintf(`{"callback_query":{"data":%q}}`, "cb:"+callback),
		fmt.Sprintf(`{"message":{"reply_to_message":{"message_id":%d}}}`, sequence+1000),
		fmt.Sprintf(`{"callback_query":{"message":{"message_id":%d}}}`, sequence+4000),
		fmt.Sprintf(`{"nested":{"worker_id":%q}}`, env.worker),
		fmt.Sprintf(`{"message":{"text":%q}}`, "/tgsessions "+strings.ToUpper(env.runtime.String())),
	} {
		exec(`INSERT INTO telegram_updates(bot_id,update_id,user_id,chat_id,raw) VALUES('bot',$1,$2,20,$3)`, sequence+int64(offset), sequence, raw)
	}
	return env
}

func workerDeletionSnapshot(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	rows, err := s.pool.db.QueryContext(t.Context(), `SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	snapshot := make(map[string][]string, len(tables))
	for _, table := range tables {
		rows, err := s.pool.db.QueryContext(t.Context(), `SELECT * FROM "`+table+`"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		snapshot[table] = nil
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			snapshot[table] = append(snapshot[table], string(encoded))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(snapshot[table])
	}
	return snapshot
}

func workerDeletionGuards(t *testing.T, s *Store) map[string]string {
	t.Helper()
	guards := make(map[string]string)
	for _, name := range []string{"commands_preserve_records_trigger", "events_preserve_records_trigger"} {
		var sql string
		if err := s.pool.QueryRow(t.Context(), `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=$1`, name).Scan(&sql); err != nil {
			t.Fatal(err)
		}
		guards[name] = sql
	}
	return guards
}

func workerDeletionCheckIntegrity(t *testing.T, s *Store, guards map[string]string, survivor eventTestEnv) {
	t.Helper()
	if got := workerDeletionGuards(t, s); !reflect.DeepEqual(got, guards) {
		t.Fatalf("journal guards changed: got %v want %v", got, guards)
	}
	var enabled bool
	if err := s.pool.QueryRow(t.Context(), `PRAGMA foreign_keys`).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("foreign key enforcement changed: %v %v", enabled, err)
	}
	rows, err := s.pool.Query(t.Context(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	broken := rows.Next()
	err = rows.Err()
	rows.Close()
	if broken || err != nil {
		t.Fatalf("foreign key integrity failed: broken=%v err=%v", broken, err)
	}
	for _, table := range []string{"commands", "events"} {
		if _, err := s.pool.Exec(t.Context(), `DELETE FROM `+table+` WHERE worker_id=$1`, survivor.worker); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("ordinary DELETE %s lost immutability: %v", table, err)
		}
	}
	var temporary int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM sqlite_temp_schema WHERE name LIKE '_worker_delete_%'`).Scan(&temporary); err != nil || temporary != 0 {
		t.Fatalf("temporary deletion scope leaked: %d %v", temporary, err)
	}
}

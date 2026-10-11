package registry

import (
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/migrations"
)

func TestTelegramSpeechPreparationMigrationPreservesQueueAndCheckpoints(t *testing.T) {
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "speech-v23.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `CREATE TABLE schema_migrations(version INTEGER NOT NULL PRIMARY KEY,checksum BLOB NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, file := range migrations.Files {
		version, err := migrationVersion(file)
		if err != nil {
			t.Fatal(err)
		}
		if version > 23 {
			continue
		}
		content, err := migrations.SQL.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), string(content)); err != nil {
			t.Fatalf("apply deployed schema %s: %v", file, err)
		}
		checksum := sha256.Sum256(content)
		if _, err := tx.Exec(t.Context(), `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, version, checksum[:]); err != nil {
			t.Fatal(err)
		}
	}
	worker, runtime, session, delivery, job, lease := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, query := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO workers(worker_id,name,os,arch,auth_token_hash) VALUES($1,'worker','linux','arm64',zeroblob(32))`, []any{worker}},
		{`INSERT INTO runtimes(runtime_id,worker_id,profile_id,name,generation,state) VALUES($1,$2,'main','Runtime',1,'running')`, []any{runtime, worker}},
		{`INSERT INTO sessions(session_id,worker_id,runtime_id,codex_thread_id,state) VALUES($1,$2,$3,'thread','idle')`, []any{session, worker, runtime}},
		{`INSERT INTO telegram_deliveries(delivery_id,bot_id,chat_id,kind,payload,status) VALUES($1,'bot',20,'turn_completed','{}','sent')`, []any{delivery}},
		{`INSERT INTO telegram_speech_jobs(job_id,delivery_id,worker_id,runtime_id,runtime_generation,session_id,turn_id,bot_id,chat_id,user_id,status,next_chunk,attempts,lease_id,lease_until,expires_at)
            VALUES($1,$2,$3,$4,1,$5,'turn','bot',20,10,'processing',1,2,$6,$7,$8)`, []any{job, delivery, worker, runtime, session, lease, time.Now().Add(10 * time.Minute), time.Now().Add(time.Hour)}},
		{`INSERT INTO telegram_speech_chunks(job_id,chunk_index,telegram_message_id) VALUES($1,0,200)`, []any{job}},
	} {
		if _, err := tx.Exec(t.Context(), query.sql, query.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	var status string
	var prepared bool
	var next, attempts, chunks int
	var retainedLease uuid.UUID
	if err := s.pool.QueryRow(t.Context(), `SELECT status,prepared,next_chunk,attempts,lease_id,(SELECT count(*) FROM telegram_speech_chunks WHERE job_id=$1)
        FROM telegram_speech_jobs WHERE job_id=$1`, job).Scan(&status, &prepared, &next, &attempts, &retainedLease, &chunks); err != nil {
		t.Fatal(err)
	}
	if status != "processing" || prepared || next != 1 || attempts != 2 || retainedLease != lease || chunks != 1 {
		t.Fatalf("upgrade changed queued speech: status=%s prepared=%v next=%d attempts=%d lease=%s chunks=%d", status, prepared, next, attempts, retainedLease, chunks)
	}
	if err := s.CheckMigrations(t.Context()); err != nil {
		t.Fatal(err)
	}
}

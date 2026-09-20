// Package worker contains the local worker runtime support.
package worker

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

const DefaultStoreLockTimeout = time.Second

var (
	bucketMeta     = []byte("meta")
	bucketCommands = []byte("commands")
	bucketOutbox   = []byte("outbox")
	bucketRuntimes = []byte("runtimes")
	bucketSessions = []byte("sessions")
	keyWorkerID    = []byte("worker_id")
	keyNextEvent   = []byte("next_event_seq")
	keyLastAck     = []byte("last_gateway_acked_event_seq")
	keySchema      = []byte("schema_version")
)

// CommandState describes durable command-ledger state.
type CommandState string

const (
	CommandReceived       CommandState = "received"
	CommandExecuting      CommandState = "executing"
	CommandCompleted      CommandState = "completed"
	CommandFailed         CommandState = "failed"
	CommandOutcomeUnknown CommandState = "outcome_unknown"
)

// CommandRecord is a command and its durable execution outcome.
type CommandRecord struct {
	Command    protocol.Command `json:"command"`
	ReceivedAt time.Time        `json:"received_at"`
	State      CommandState     `json:"state"`
	Result     *protocol.Result `json:"result,omitempty"`
}

// ReceiveResult reports whether a command was written by this call. A
// duplicate always returns its previously recorded ledger entry.
type ReceiveResult struct {
	Accepted bool
	Record   CommandRecord
}

// Store is a transactional worker-local ledger and durable event outbox.
type Store struct {
	db       *workerdb.DB
	workerID string
}

// OpenStore opens a 0600 SQLite database and binds it permanently to workerID.
// A second worker receives a lock-timeout error rather than sharing the
// file. Existing files that group or others can read are rejected.
func OpenStore(path, workerID string) (*Store, error) {
	if _, err := uuid.Parse(workerID); err != nil {
		return nil, errors.New("worker store: worker_id must be a UUID")
	}
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("worker store: state file permissions must be 0600 or stricter")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("worker store: stat state file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("worker store: create state directory: %w", err)
	}
	db, err := workerdb.Open(path, 0o600, &workerdb.Options{Timeout: DefaultStoreLockTimeout, WorkerID: workerID})
	if err != nil {
		return nil, fmt.Errorf("worker store: open: %w", err)
	}
	s := &Store{db: db, workerID: workerID}
	if err := db.Update(func(tx *workerdb.Tx) error {
		for _, bucket := range [][]byte{bucketMeta, bucketCommands, bucketOutbox, bucketRuntimes, bucketSessions} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		if schema := meta.Get(keySchema); schema != nil && string(schema) != "1" {
			return errors.New("worker store: unsupported schema version")
		}
		if err := meta.Put(keySchema, []byte("1")); err != nil {
			return err
		}
		stored := string(meta.Get(keyWorkerID))
		if stored == "" {
			if err := meta.Put(keyWorkerID, []byte(workerID)); err != nil {
				return err
			}
		} else if stored != workerID {
			return errors.New("worker store: worker_id does not match enrolled identity")
		}
		if meta.Get(keyNextEvent) == nil {
			if err := meta.Put(keyNextEvent, sequenceKey(1)); err != nil {
				return err
			}
		}
		if meta.Get(keyLastAck) == nil {
			if err := meta.Put(keyLastAck, sequenceKey(0)); err != nil {
				return err
			}
		}
		return recoverExecuting(tx)
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("worker store: initialize: %w", err)
	}
	return s, nil
}

// Close flushes and releases the exclusive database lock.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Receive atomically records a command before it is acknowledged. Calling it
// again with the same ID never overwrites a prior result.
func (s *Store) Receive(command protocol.Command) (ReceiveResult, error) {
	if err := command.Validate(); err != nil {
		return ReceiveResult{}, fmt.Errorf("worker store: invalid command: %w", err)
	}
	if command.WorkerID != s.workerID {
		return ReceiveResult{}, errors.New("worker store: command targets another worker")
	}
	var result ReceiveResult
	err := s.db.Update(func(tx *workerdb.Tx) error {
		b := tx.Bucket(bucketCommands)
		if value := b.Get([]byte(command.ID)); value != nil {
			record, err := decodeCommand(value)
			if err != nil {
				return err
			}
			result = ReceiveResult{Record: record}
			return nil
		}
		record := CommandRecord{Command: command, ReceivedAt: time.Now().UTC(), State: CommandReceived}
		value, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(command.ID), value); err != nil {
			return err
		}
		result = ReceiveResult{Accepted: true, Record: record}
		return nil
	})
	return result, err
}

// SetCommandState persists state and an optional result. Executing commands
// are converted to outcome_unknown when the store is next opened.
func (s *Store) SetCommandState(commandID string, state CommandState, result *protocol.Result) error {
	if !validCommandState(state) {
		return errors.New("worker store: invalid command state")
	}
	return s.db.Update(func(tx *workerdb.Tx) error {
		b := tx.Bucket(bucketCommands)
		value := b.Get([]byte(commandID))
		if value == nil {
			return errors.New("worker store: command not found")
		}
		record, err := decodeCommand(value)
		if err != nil {
			return err
		}
		record.State, record.Result = state, result
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return b.Put([]byte(commandID), encoded)
	})
}

// LoadCommand returns a durable ledger entry when it exists.
func (s *Store) LoadCommand(commandID string) (CommandRecord, bool, error) {
	var record CommandRecord
	found := false
	err := s.db.View(func(tx *workerdb.Tx) error {
		value := tx.Bucket(bucketCommands).Get([]byte(commandID))
		if value == nil {
			return nil
		}
		var err error
		record, err = decodeCommand(value)
		found = err == nil
		return err
	})
	return record, found, err
}

// PendingCommands returns only commands never entered execution. Restarted
// executing commands are deliberately marked outcome_unknown, never replayed.
func (s *Store) PendingCommands() ([]CommandRecord, error) {
	var records []CommandRecord
	err := s.db.View(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketCommands).ForEach(func(_, value []byte) error {
			record, err := decodeCommand(value)
			if err != nil {
				return err
			}
			if record.State == CommandReceived {
				records = append(records, record)
			}
			return nil
		})
	})
	sort.SliceStable(records, func(i, j int) bool { return records[i].ReceivedAt.Before(records[j].ReceivedAt) })
	return records, err
}

// AppendEvent assigns an event UUID and strictly increasing sequence then
// persists the event in the outbox in the same transaction.
func (s *Store) AppendEvent(event protocol.Event) (protocol.Event, error) {
	if event.WorkerID != "" && event.WorkerID != s.workerID {
		return protocol.Event{}, errors.New("worker store: event targets another worker")
	}
	var persisted protocol.Event
	err := s.db.Update(func(tx *workerdb.Tx) error {
		var err error
		persisted, err = appendEvent(tx, event)
		return err
	})
	return persisted, err
}

// OutboxAfter lists unacknowledged durable events with sequence greater than
// seq, in sequence order.
func (s *Store) OutboxAfter(seq uint64) ([]protocol.Event, error) {
	if seq >= math.MaxInt64 {
		return nil, errors.New("worker store: event sequence exceeds supported range")
	}
	var events []protocol.Event
	err := s.db.View(func(tx *workerdb.Tx) error {
		cursor := tx.Bucket(bucketOutbox).Cursor()
		for key, value := cursor.Seek(sequenceKey(seq + 1)); key != nil; key, value = cursor.Next() {
			var event protocol.Event
			if err := json.Unmarshal(value, &event); err != nil {
				return err
			}
			events = append(events, event)
		}
		return nil
	})
	return events, err
}

// EventWatermarks returns the last gateway ACK and highest locally persisted
// event sequence. A reconnect must not invent history after data loss.
func (s *Store) EventWatermarks() (acked, high uint64, err error) {
	err = s.db.View(func(tx *workerdb.Tx) error {
		meta := tx.Bucket(bucketMeta)
		acked = parseSequence(meta.Get(keyLastAck))
		next := parseSequence(meta.Get(keyNextEvent))
		if next > 0 {
			high = next - 1
		}
		return nil
	})
	return
}

// RecordResult commits a command outcome and its notification together so a
// crash cannot leave a completed ledger entry without its gateway event.
func (s *Store) RecordResult(commandID string, state CommandState, result *protocol.Result, event protocol.Event) (protocol.Event, error) {
	if !validCommandState(state) {
		return protocol.Event{}, errors.New("worker store: invalid command state")
	}
	if event.WorkerID != "" && event.WorkerID != s.workerID {
		return protocol.Event{}, errors.New("worker store: event targets another worker")
	}
	var saved protocol.Event
	err := s.db.Update(func(tx *workerdb.Tx) error {
		b := tx.Bucket(bucketCommands)
		raw := b.Get([]byte(commandID))
		if raw == nil {
			return errors.New("worker store: command not found")
		}
		record, err := decodeCommand(raw)
		if err != nil {
			return err
		}
		record.State = state
		record.Result = result
		data, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err = b.Put([]byte(commandID), data); err != nil {
			return err
		}
		saved, err = appendEvent(tx, event)
		return err
	})
	return saved, err
}

// AckThrough rejects acknowledgements beyond the persisted high-water mark.
// Valid acknowledgements are monotonic and only delete events at or below seq.
func (s *Store) AckThrough(seq uint64) error {
	return s.db.Update(func(tx *workerdb.Tx) error {
		meta := tx.Bucket(bucketMeta)
		next := parseSequence(meta.Get(keyNextEvent))
		high := uint64(0)
		if next > 0 {
			high = next - 1
		}
		if seq > high {
			return errors.New("worker store: acknowledgement exceeds event high-water mark")
		}
		acked := parseSequence(meta.Get(keyLastAck))
		if seq <= acked {
			return nil
		}
		b := tx.Bucket(bucketOutbox)
		cursor := b.Cursor()
		for key, _ := cursor.First(); key != nil && parseSequence(key) <= seq; key, _ = cursor.Next() {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
		return meta.Put(keyLastAck, sequenceKey(seq))
	})
}

// RuntimeForProfile retrieves the stable local runtime identity for a profile.
func (s *Store) RuntimeForProfile(profileID string) (protocol.Runtime, bool, error) {
	var runtime protocol.Runtime
	found := false
	err := s.db.View(func(tx *workerdb.Tx) error {
		value := tx.Bucket(bucketRuntimes).Get([]byte(profileID))
		if value == nil {
			return nil
		}
		if err := json.Unmarshal(value, &runtime); err != nil {
			return err
		}
		found = true
		return nil
	})
	return runtime, found, err
}

// BeginRuntime persists a new generation before the caller spawns a process.
func (s *Store) BeginRuntime(profileID, name, defaultCWD string) (protocol.Runtime, error) {
	if profileID == "" {
		return protocol.Runtime{}, errors.New("worker store: runtime profile_id is required")
	}
	var runtime protocol.Runtime
	err := s.db.Update(func(tx *workerdb.Tx) error {
		b := tx.Bucket(bucketRuntimes)
		if value := b.Get([]byte(profileID)); value != nil {
			if err := json.Unmarshal(value, &runtime); err != nil {
				return err
			}
			if runtime.Generation >= math.MaxInt64 {
				return errors.New("worker store: runtime generation exhausted")
			}
			runtime.Generation++
		} else {
			runtime = protocol.Runtime{ID: uuid.NewString(), WorkerID: s.workerID, ProfileID: profileID, Generation: 1}
		}
		runtime.WorkerID, runtime.ProfileID, runtime.Name, runtime.DefaultCWD, runtime.PID, runtime.State = s.workerID, profileID, name, defaultCWD, 0, "starting"
		encoded, err := json.Marshal(runtime)
		if err != nil {
			return err
		}
		return b.Put([]byte(profileID), encoded)
	})
	return runtime, err
}

// UpsertSession preserves one stable UUID for each runtime/thread pair.
func (s *Store) UpsertSession(session protocol.Session) (protocol.Session, error) {
	if _, err := uuid.Parse(session.RuntimeID); err != nil || session.ThreadID == "" {
		return protocol.Session{}, errors.New("worker store: session runtime_id and thread_id are required")
	}
	if session.WorkerID != "" && session.WorkerID != s.workerID {
		return protocol.Session{}, errors.New("worker store: session targets another worker")
	}
	var saved protocol.Session
	err := s.db.Update(func(tx *workerdb.Tx) error {
		b := tx.Bucket(bucketSessions)
		key := sessionKey(session.RuntimeID, session.ThreadID)
		saved = session
		if value := b.Get(key); value != nil {
			var old protocol.Session
			if err := json.Unmarshal(value, &old); err != nil {
				return err
			}
			// Permanent Codex deletion wins over an older in-flight discovery
			// snapshot or notification. This identity can never be restored.
			if old.Deleted {
				saved = old
				return nil
			}
			saved.ID = old.ID
		} else if saved.ID == "" {
			saved.ID = uuid.NewString()
		}
		if _, err := uuid.Parse(saved.ID); err != nil {
			return errors.New("worker store: session_id must be a UUID")
		}
		saved.WorkerID = s.workerID
		saved.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		return b.Put(key, encoded)
	})
	return saved, err
}

// ArchiveDiscoveredSession hides a previously discovered session from gateway
// selection while retaining its identity and history. It never archives the
// underlying Codex thread. The expected snapshot prevents a discovery scan
// from hiding a session changed concurrently by an actor or notification.
// Persist the inventory change and its durable event together so reconnecting
// gateways also receive tombstones for sessions discovered before an upgrade.
func (s *Store) ArchiveDiscoveredSession(runtime protocol.Runtime, expected protocol.Session) (protocol.Session, bool, error) {
	candidate := expected
	candidate.Archived, candidate.Loaded, candidate.ActiveTurnID, candidate.State = true, false, "", "not_loaded"
	return s.changeDiscoveredSessionVisibility(runtime, expected, candidate)
}

// RestoreDiscoveredSession makes a locally hidden session selectable again
// after discovery finds an eligible Codex thread. The identity and expected
// archived snapshot must still match, and the change is atomic with its event.
func (s *Store) RestoreDiscoveredSession(runtime protocol.Runtime, expected, candidate protocol.Session) (protocol.Session, bool, error) {
	if candidate.Archived || candidate.ID != expected.ID || candidate.WorkerID != expected.WorkerID ||
		candidate.RuntimeID != expected.RuntimeID || candidate.ThreadID != expected.ThreadID {
		return protocol.Session{}, false, errors.New("worker store: invalid discovered session restore target")
	}
	return s.changeDiscoveredSessionVisibility(runtime, expected, candidate)
}

func (s *Store) changeDiscoveredSessionVisibility(runtime protocol.Runtime, expected, candidate protocol.Session) (protocol.Session, bool, error) {
	if _, err := uuid.Parse(runtime.ID); err != nil || runtime.WorkerID != s.workerID ||
		runtime.Generation == 0 || runtime.Generation > math.MaxInt64 ||
		expected.WorkerID != s.workerID || expected.RuntimeID != runtime.ID || expected.ThreadID == "" {
		return protocol.Session{}, false, errors.New("worker store: invalid discovered session visibility target")
	}
	if _, err := uuid.Parse(expected.ID); err != nil {
		return protocol.Session{}, false, errors.New("worker store: invalid discovered session identity")
	}
	var saved protocol.Session
	changed := false
	err := s.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketSessions)
		key := sessionKey(runtime.ID, expected.ThreadID)
		value := bucket.Get(key)
		if value == nil {
			return nil
		}
		var current protocol.Session
		if err := json.Unmarshal(value, &current); err != nil {
			return err
		}
		saved = current
		if current.Deleted {
			return nil
		}
		previous := expected
		current.UpdatedAt, previous.UpdatedAt = time.Time{}, time.Time{}
		if saved.Archived == candidate.Archived || !reflect.DeepEqual(current, previous) || !saved.UpdatedAt.Equal(expected.UpdatedAt) {
			return nil
		}
		saved = candidate
		saved.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		if err := bucket.Put(key, encoded); err != nil {
			return err
		}
		if _, err := appendEvent(tx, protocol.Event{
			WorkerID: s.workerID, RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation,
			SessionID: saved.ID, Kind: "session_state_changed", Data: encoded,
		}); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return protocol.Session{}, false, err
	}
	return saved, changed, nil
}

// ListSessions returns every session, optionally limited to a runtime ID.
func (s *Store) ListSessions(runtimeID string) ([]protocol.Session, error) {
	var sessions []protocol.Session
	err := s.db.View(func(tx *workerdb.Tx) error {
		return tx.Bucket(bucketSessions).ForEach(func(_, value []byte) error {
			var session protocol.Session
			if err := json.Unmarshal(value, &session); err != nil {
				return err
			}
			if runtimeID == "" || session.RuntimeID == runtimeID {
				sessions = append(sessions, session)
			}
			return nil
		})
	})
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.Before(sessions[j].UpdatedAt) })
	return sessions, err
}

func recoverExecuting(tx *workerdb.Tx) error {
	return tx.Bucket(bucketCommands).ForEach(func(key, value []byte) error {
		record, err := decodeCommand(value)
		if err != nil {
			return err
		}
		if record.State != CommandExecuting {
			return nil
		}
		record.State = CommandOutcomeUnknown
		record.Result = &protocol.Result{CommandID: record.Command.ID, State: string(CommandOutcomeUnknown), Error: &protocol.Error{Code: protocol.OutcomeUnknown, Message: "worker restarted while command execution was in progress"}}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bucketCommands).Put(key, encoded); err != nil {
			return err
		}
		data, err := json.Marshal(record.Result)
		if err != nil {
			return err
		}
		_, err = appendEvent(tx, protocol.Event{WorkerID: record.Command.WorkerID, RuntimeID: record.Command.RuntimeID, RuntimeGeneration: record.Command.RuntimeGeneration, SessionID: record.Command.SessionID, Kind: "command_result_unknown", Data: data})
		return err
	})
}

func appendEvent(tx *workerdb.Tx, event protocol.Event) (protocol.Event, error) {
	meta := tx.Bucket(bucketMeta)
	next := parseSequence(meta.Get(keyNextEvent))
	if next == 0 || next >= math.MaxInt64 {
		return protocol.Event{}, errors.New("worker store: invalid or exhausted event sequence")
	}
	if event.WorkerID != "" && event.WorkerID != string(meta.Get(keyWorkerID)) {
		return protocol.Event{}, errors.New("worker store: event identity mismatch")
	}
	if !event.Durable() {
		return protocol.Event{}, errors.New("worker store: transient events cannot consume durable sequences")
	}
	event.Seq = next
	event.ID = uuid.NewString()
	event.WorkerID = string(meta.Get(keyWorkerID))
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return protocol.Event{}, err
	}
	if err := tx.Bucket(bucketOutbox).Put(sequenceKey(next), encoded); err != nil {
		return protocol.Event{}, err
	}
	return event, meta.Put(keyNextEvent, sequenceKey(next+1))
}
func validCommandState(state CommandState) bool {
	switch state {
	case CommandReceived, CommandExecuting, CommandCompleted, CommandFailed, CommandOutcomeUnknown:
		return true
	}
	return false
}
func decodeCommand(value []byte) (CommandRecord, error) {
	var record CommandRecord
	err := json.Unmarshal(value, &record)
	return record, err
}
func sequenceKey(seq uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, seq)
	return key
}
func parseSequence(value []byte) uint64 {
	if len(value) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(value)
}
func sessionKey(runtimeID, threadID string) []byte { return []byte(runtimeID + "\x00" + threadID) }

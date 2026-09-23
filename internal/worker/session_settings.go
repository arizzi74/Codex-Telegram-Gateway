package worker

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

// Settings notifications arrive on the durable observer's ordered event
// stream. Do not synthesize another update from a command acknowledgment: a
// second client's newer selection can already be queued behind that reply.
func (s *sessionActor) observeThreadSettings(event codexadapter.Event) {
	if event.ThreadID != s.session.ThreadID || event.Settings == nil || s.session.Archived || s.session.Deleted || !workspaceAllowed(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots) {
		return
	}
	settings, err := s.agent.store.recordSessionSettings(s.runtime, s.session, *event.Settings)
	if err != nil {
		s.agent.report(err)
		return
	}
	if settings != nil {
		s.session.Settings = settings
	}
}

// Keep immutable copies so actor snapshots cannot share a mutable preference
// pointer. An empty effort is a confirmed model default, not missing data.
func currentSessionSettings(incoming, previous *protocol.SessionSettings, generation uint64) *protocol.SessionSettings {
	var current *protocol.SessionSettings
	for _, value := range []*protocol.SessionSettings{previous, incoming} {
		if value == nil || value.RuntimeGeneration != generation || value.Validate() != nil {
			continue
		}
		if current == nil || value.Revision > current.Revision {
			copy := *value
			current = &copy
		}
	}
	return current
}

// The preference revision and its durable session event commit together. A
// worker restart cannot lose an acknowledged choice or reuse its revision.
func (s *Store) recordSessionSettings(runtime protocol.Runtime, session protocol.Session, preferences codexadapter.CurrentThreadSettings) (*protocol.SessionSettings, error) {
	if runtime.WorkerID != s.workerID || runtime.ID != session.RuntimeID || session.WorkerID != s.workerID || session.ID == "" || session.ThreadID == "" || runtime.Generation == 0 || runtime.Generation > math.MaxInt64 {
		return nil, errors.New("worker settings: invalid session target")
	}
	var result *protocol.SessionSettings
	err := s.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketSessions)
		key := sessionKey(runtime.ID, session.ThreadID)
		var previous protocol.Session
		if value := bucket.Get(key); value == nil {
			return errors.New("worker settings: session is missing")
		} else if err := json.Unmarshal(value, &previous); err != nil {
			return err
		}
		if previous.ID != session.ID || previous.WorkerID != s.workerID || previous.RuntimeID != runtime.ID || previous.ThreadID != session.ThreadID {
			return errors.New("worker settings: session identity changed")
		}
		if previous.Deleted || previous.Archived || (previous.Settings != nil && previous.Settings.RuntimeGeneration > runtime.Generation) {
			return nil
		}
		settings := protocol.SessionSettings{RuntimeGeneration: runtime.Generation, Revision: 1, Model: preferences.Model, ReasoningEffort: preferences.ReasoningEffort}
		if err := settings.Validate(); err != nil {
			return err
		}
		if previous.Settings != nil && previous.Settings.RuntimeGeneration == runtime.Generation {
			settings.Revision = previous.Settings.Revision
			if settings.Model == previous.Settings.Model && settings.ReasoningEffort == previous.Settings.ReasoningEffort {
				result = &settings
				return nil
			}
			if settings.Revision >= math.MaxInt64 {
				return errors.New("worker settings: revision exhausted")
			}
			settings.Revision++
		}
		if err := settings.Validate(); err != nil {
			return err
		}
		session.Settings = &settings
		session.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(session)
		if err != nil {
			return err
		}
		if err := bucket.Put(key, encoded); err != nil {
			return err
		}
		if _, err := s.appendEvent(tx, protocol.Event{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: session.ID, Kind: "session_state_changed", Data: encoded}); err != nil {
			return err
		}
		result = &settings
		return nil
	})
	return result, err
}

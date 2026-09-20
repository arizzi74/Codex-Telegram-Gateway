package worker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/iaia/telegramgw/internal/workerdb"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// deleteSession uses a fresh app-server snapshot without resuming the selected
// thread. A cold thread can be deleted directly, preserving its original CWD.
func (s *sessionActor) deleteSession(ctx context.Context, client *codexadapter.Client, c protocol.Command) (protocol.Result, error) {
	if c.SessionID != s.session.ID || c.ThreadID != s.session.ThreadID || c.RuntimeID != s.runtime.ID {
		return protocol.Result{}, &protocol.Error{Code: protocol.UnknownSession, Message: "Session target does not match."}
	}
	if c.RuntimeGeneration != s.runtime.Generation {
		return protocol.Result{}, &protocol.Error{Code: protocol.StaleRuntime, Message: "Runtime generation no longer matches."}
	}
	if s.session.Archived || s.session.Deleted {
		return protocol.Result{}, &protocol.Error{Code: protocol.UnknownSession, Message: "Session is no longer available."}
	}
	busy := func() bool {
		return !updateSessionIdle(s.session) || s.activeCommand != nil || s.awaitingTurnStart || len(s.queue) != 0 || len(s.pending) != 0 || len(s.eventQueue) != 0 || len(s.requestQueue) != 0
	}
	if busy() {
		return protocol.Result{}, deleteSessionBusy()
	}
	cwd, err := auth.CanonicalWorkspace(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil {
		return protocol.Result{}, &protocol.Error{Code: protocol.InvalidWorkspace, Message: "Session workspace is not allowed."}
	}
	thread, err := client.ReadThreadForDeletion(ctx, s.session.ThreadID)
	if err != nil {
		return protocol.Result{}, err
	}
	if !thread.UserSession() {
		return protocol.Result{}, &protocol.Error{Code: protocol.UnknownSession, Message: "The selected thread is not a user session."}
	}
	remoteCWD, err := auth.CanonicalWorkspace(thread.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil || remoteCWD != cwd || (c.Arguments.CWD != "" && c.Arguments.CWD != s.session.CWD) {
		return protocol.Result{}, &protocol.Error{Code: protocol.InvalidWorkspace, Message: "The session working directory changed. Select the session again before deleting it."}
	}
	switch thread.Status {
	case "idle", "notLoaded", "systemError":
	default:
		return protocol.Result{}, deleteSessionBusy()
	}
	if thread.ActiveTurnID != "" || busy() {
		return protocol.Result{}, deleteSessionBusy()
	}
	if err := verifyDeleteDescendantsIdle(ctx, client, thread); err != nil {
		return protocol.Result{}, err
	}
	if busy() {
		return protocol.Result{}, deleteSessionBusy()
	}
	if err := client.DeleteThread(ctx, s.session.ThreadID); err != nil {
		return protocol.Result{}, err
	}
	saved, err := s.agent.store.DeleteSession(s.runtime, s.session)
	if err != nil {
		return protocol.Result{}, err
	}
	s.session = saved
	return protocol.Result{State: "completed", Session: &saved, Text: "Session deleted. The working directory and its files were kept."}, nil
}

// App-server deletion also removes spawned descendants. A parent may be idle
// while one of those descendants still has a turn running in another client.
// Inspect loaded threads without filtering out subagents, and follow ancestry
// only for busy entries so unrelated active user sessions do not block deletion.
func verifyDeleteDescendantsIdle(ctx context.Context, client *codexadapter.Client, root codexadapter.Thread) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	loaded, complete, err := loadedThreadIDs(ctx, client)
	if err != nil {
		return err
	}
	if !complete {
		return deleteSessionBusy()
	}
	metadata := map[string]codexadapter.Thread{root.ID: root}
	for id := range loaded {
		thread, err := client.ReadThreadForDeletion(ctx, id)
		if err != nil {
			return err
		}
		metadata[id] = thread
	}
	for id := range loaded {
		thread := metadata[id]
		if thread.ActiveTurnID == "" && (thread.Status == "idle" || thread.Status == "notLoaded" || thread.Status == "systemError") {
			continue
		}
		seen := map[string]bool{}
		for {
			if thread.ID == root.ID {
				return deleteSessionBusy()
			}
			if seen[thread.ID] || len(seen) >= 256 {
				return deleteSessionBusy()
			}
			seen[thread.ID] = true
			if thread.ParentThreadID == "" {
				if !thread.UserSession() {
					return deleteSessionBusy() // Unknown helper ancestry cannot be proven unrelated.
				}
				break
			}
			parent, found := metadata[thread.ParentThreadID]
			if !found {
				parent, err = client.ReadThread(ctx, thread.ParentThreadID, false)
				if err != nil {
					return err
				}
				if parent.ID != thread.ParentThreadID {
					return deleteSessionBusy()
				}
				metadata[parent.ID] = parent
			}
			thread = parent
		}
	}
	return nil
}

func deleteSessionBusy() error {
	return &protocol.Error{Code: protocol.SessionBusy, Message: "This session has a running turn or pending work. Wait for it to finish before deleting it.", Retryable: true}
}

// observeDeletedThread handles a known but currently archived inventory entry
// that has no actor, such as one retained across a worker restart.
func (a *Agent) observeDeletedThread(runtime protocol.Runtime, threadID string) {
	sessions, err := a.store.ListSessions(runtime.ID)
	if err != nil {
		a.report(err)
		return
	}
	for _, session := range sessions {
		if session.ThreadID == threadID {
			_, err := a.store.DeleteSession(runtime, session)
			a.report(err)
			return
		}
	}
}

// deleted handles thread/deleted notifications from any app-server client,
// including notifications for descendants removed with their parent thread.
func (s *sessionActor) deleted() {
	saved, err := s.agent.store.DeleteSession(s.runtime, s.session)
	if err != nil {
		s.agent.report(err)
		return
	}
	s.session = saved
	for _, command := range s.queue {
		_, err := s.agent.reject(command, protocol.UnknownSession, "The Codex session was deleted before this command could run.")
		s.agent.report(err)
	}
	s.queue = nil
	if s.activeCommand != nil {
		s.agent.report(s.agent.executionError(*s.activeCommand, &protocol.Error{Code: protocol.UnknownSession, Message: "The Codex session was deleted."}))
	}
	s.activeCommand = nil
	s.awaitingTurnStart = false
	s.resetMessages()
	for key := range s.pending {
		s.resolve(key, "cleared")
	}
}

// DeleteSession durably records an app-server-confirmed permanent deletion and
// its gateway event in one transaction. Unlike discovery archival, this marker
// cannot be undone by a stale scan and does not touch any filesystem directory.
func (s *Store) DeleteSession(runtime protocol.Runtime, expected protocol.Session) (protocol.Session, error) {
	if runtime.WorkerID != s.workerID || expected.WorkerID != s.workerID || expected.RuntimeID != runtime.ID || expected.ThreadID == "" || expected.ID == "" || runtime.Generation == 0 {
		return protocol.Session{}, errors.New("worker store: invalid permanent session deletion target")
	}
	var saved protocol.Session
	err := s.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketSessions)
		key := sessionKey(runtime.ID, expected.ThreadID)
		value := bucket.Get(key)
		if value == nil {
			return errors.New("worker store: deleted session identity is missing")
		}
		if err := json.Unmarshal(value, &saved); err != nil {
			return err
		}
		if saved.ID != expected.ID {
			return errors.New("worker store: deleted session identity changed")
		}
		if err := purgeAsyncHistoryCache(tx, runtime.ID, expected.ThreadID); err != nil {
			return err
		}
		if saved.Deleted {
			return nil
		}
		saved.State, saved.Archived, saved.Deleted, saved.Loaded, saved.ActiveTurnID = "not_loaded", true, true, false, ""
		if saved.Stats != nil {
			saved.Stats.ActiveSince = nil
		}
		saved.UpdatedAt = time.Now().UTC()
		encoded, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		if err := bucket.Put(key, encoded); err != nil {
			return err
		}
		_, err = appendEvent(tx, protocol.Event{WorkerID: s.workerID, RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: saved.ID, Kind: "session_state_changed", Data: encoded})
		return err
	})
	return saved, err
}

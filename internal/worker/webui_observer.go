package worker

import (
	"context"
	"errors"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

// A browser may load a persisted thread and immediately submit its first turn,
// before periodic discovery subscribes the worker's durable Telegram observer.
// Resume that same already validated thread on the observer connection before
// enabling the browser. This does not submit or replay any prompt.
func (a *Agent) observeWebUISession(ctx context.Context, runtime protocol.Runtime, session protocol.Session) error {
	if err := a.lockWebUIAdmission(ctx); err != nil {
		return err
	}
	defer a.updateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.update != nil {
		return ErrUpdatePrepared
	}
	client, current, ok := a.manager.Client(runtime.ID)
	if !ok || current.Generation != runtime.Generation || current.State != "running" {
		return errors.New("web UI observer runtime changed")
	}
	if session.RuntimeID != current.ID || session.WorkerID != current.WorkerID || session.ThreadID == "" || session.Archived || session.Deleted || !workspaceAllowed(session.CWD, a.cfg.AllowedWorkspaceRoots) {
		return errors.New("web UI observer session is unavailable")
	}
	select {
	case <-client.Done():
		return codexadapter.ErrClosed
	default:
	}
	ready, epoch := a.manager.webUIObserverState(current, client, session.ThreadID)
	if ready {
		return nil
	}
	// Do not rely on the discovery map here: it reserves a subscription before
	// its RPC completes. Await our own idempotent resume acknowledgment instead.
	thread, err := client.ResumeThread(ctx, session.ThreadID, codexadapter.ThreadOptions{})
	if err != nil {
		return err
	}
	if thread.ID != session.ThreadID || !thread.UserSession() || !workspaceAllowed(thread.CWD, a.cfg.AllowedWorkspaceRoots) {
		return errors.New("web UI observer returned an unavailable session")
	}
	if thread.ActiveTurnID == "" && (thread.Status == "active" || thread.Status == "running") {
		state, err := client.ReadThreadState(ctx, session.ThreadID)
		if err != nil {
			return err
		}
		thread.ActiveTurnID = state.ActiveTurnID
	}
	candidate := sessionFromThread(current, thread, true)
	candidate.ID, candidate.Stats = session.ID, session.Stats
	if err := a.onSessionContext(ctx, current, candidate); err != nil {
		return err
	}
	a.manager.mu.Lock()
	defer a.manager.mu.Unlock()
	managed := a.manager.runtimes[current.ID]
	if managed == nil || managed.client != client || managed.runtime.Generation != current.Generation || managed.runtime.State != "running" {
		return errors.New("web UI observer runtime changed")
	}
	if managed.observerEpoch[session.ThreadID] != epoch {
		return errors.New("web UI observer session closed during admission")
	}
	managed.subscriptions[session.ThreadID] = struct{}{}
	if managed.observerReady == nil {
		managed.observerReady = make(map[string]struct{})
	}
	managed.observerReady[session.ThreadID] = struct{}{}
	return nil
}

// The discovery reservation is deliberately insufficient: readiness requires
// both a successful native resume and installation of that generation's actor
// snapshot. The map belongs to one managed runtime/client and resets on restart.
func (m *RuntimeManager) webUIObserverState(runtime protocol.Runtime, client *codexadapter.Client, threadID string) (bool, uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	managed := m.runtimes[runtime.ID]
	if managed == nil || managed.client != client || managed.runtime.Generation != runtime.Generation || managed.runtime.State != "running" {
		return false, 0
	}
	_, ready := managed.observerReady[threadID]
	return ready, managed.observerEpoch[threadID]
}

func (m *RuntimeManager) markWebUIObserverReady(runtime protocol.Runtime, client *codexadapter.Client, threadID string, epoch uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	managed := m.runtimes[runtime.ID]
	if managed == nil || managed.client != client || managed.runtime.Generation != runtime.Generation || managed.runtime.State != "running" {
		return
	}
	if managed.observerEpoch[threadID] != epoch {
		return
	}
	if _, subscribed := managed.subscriptions[threadID]; !subscribed {
		return // A close notification invalidated this subscription.
	}
	if managed.observerReady == nil {
		managed.observerReady = make(map[string]struct{})
	}
	managed.observerReady[threadID] = struct{}{}
}

func (m *RuntimeManager) forgetWebUIObserver(runtime protocol.Runtime, threadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if managed := m.runtimes[runtime.ID]; managed != nil && managed.runtime.Generation == runtime.Generation {
		delete(managed.subscriptions, threadID)
		delete(managed.observerReady, threadID)
		if managed.observerEpoch == nil {
			managed.observerEpoch = make(map[string]uint64)
		}
		managed.observerEpoch[threadID]++
	}
}

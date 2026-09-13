package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

const (
	discoveryPageSize = 100
	discoveryLimit    = 1000
	discoveryInterval = 30 * time.Second
)

// RuntimeHooks route adapter activity to the session coordinator. OnSession
// receives existing sessions during reconciliation without overwriting their
// durable actor state; first discoveries are persisted before this callback.
type RuntimeHooks struct {
	OnSession func(protocol.Runtime, protocol.Session)
	OnEvent   func(protocol.Runtime, codexadapter.Event)
	OnRequest func(protocol.Runtime, codexadapter.Request)
	OnError   func(error)
}

// RuntimeManager supervises configured local Codex app-server profiles.
type RuntimeManager struct {
	cfg   config.WorkerConfig
	store *Store
	log   *slog.Logger
	hooks RuntimeHooks

	mu       sync.RWMutex
	runtimes map[string]*managedRuntime
	start    func(context.Context, codexadapter.Config) (*codexadapter.Client, error)
	ready    chan struct{}
}

type managedRuntime struct {
	runtime       protocol.Runtime
	client        *codexadapter.Client
	subscriptions map[string]struct{}
	degraded      bool
}

// runtimePersistenceError marks failures which have crossed the durable local
// state boundary. Only these are forwarded through RuntimeHooks.OnError, which
// the Agent treats as fatal; RPC discovery trouble is handled by the runtime.
type runtimePersistenceError struct{ err error }

func (e *runtimePersistenceError) Error() string { return e.err.Error() }
func (e *runtimePersistenceError) Unwrap() error { return e.err }

func persistenceError(err error) error {
	if err == nil {
		return nil
	}
	return &runtimePersistenceError{err: err}
}

func isPersistenceError(err error) bool {
	var persistence *runtimePersistenceError
	return errors.As(err, &persistence)
}

// NewRuntimeManager constructs a supervisor. It validates all configured
// working directories against the already canonicalized local allowlist.
func NewRuntimeManager(cfg config.WorkerConfig, store *Store, logger *slog.Logger, hooks RuntimeHooks) (*RuntimeManager, error) {
	if store == nil {
		return nil, errors.New("runtime manager: store is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, profile := range cfg.Runtimes {
		if !workspaceAllowed(profile.WorkingDirectory, cfg.AllowedWorkspaceRoots) {
			return nil, errors.New("runtime manager: runtime working_directory is outside allowed workspace roots")
		}
	}
	m := &RuntimeManager{cfg: cfg, store: store, log: logger, hooks: hooks, runtimes: make(map[string]*managedRuntime), ready: make(chan struct{})}
	m.start = func(ctx context.Context, options codexadapter.Config) (*codexadapter.Client, error) {
		base := filepath.Join(filepath.Dir(cfg.StateFile), "runtimes")
		if err := os.MkdirAll(base, 0o700); err != nil {
			return nil, err
		}
		dir, err := os.MkdirTemp(base, "runtime-")
		if err != nil {
			return nil, err
		}
		shared, err := codexadapter.StartShared(ctx, options, filepath.Join(dir, "app.sock"))
		if err != nil {
			return nil, err
		}
		return shared.Client, nil
	}
	return m, nil
}

// Run starts autostart profiles in independent goroutines and closes all child
// clients when the worker context ends. Gateway connection state is irrelevant.
func (m *RuntimeManager) Run(ctx context.Context) error {
	var group sync.WaitGroup
	var initial sync.WaitGroup
	for _, profile := range m.cfg.Runtimes {
		if profile.Autostart {
			group.Add(1)
			initial.Add(1)
			go func(profile config.RuntimeProfile) { defer group.Done(); m.supervise(ctx, profile, initial.Done) }(profile)
		}
	}
	go func() { initial.Wait(); close(m.ready) }()
	<-ctx.Done()
	m.closeAll()
	group.Wait()
	return nil
}

// Ready closes when each autostart profile has completed its first startup and
// discovery attempt, allowing the connection to send an initial runtime view.
func (m *RuntimeManager) Ready() <-chan struct{} { return m.ready }

// Snapshot returns the current runtime records for heartbeat and hello.
func (m *RuntimeManager) Snapshot() []protocol.Runtime {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]protocol.Runtime, 0, len(m.runtimes))
	for _, runtime := range m.runtimes {
		result = append(result, runtime.runtime)
	}
	return result
}

// Client returns the currently live adapter client and generation-fenced runtime.
func (m *RuntimeManager) Client(runtimeID string) (*codexadapter.Client, protocol.Runtime, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	runtime := m.runtimes[runtimeID]
	if runtime == nil || runtime.client == nil {
		return nil, protocol.Runtime{}, false
	}
	return runtime.client, runtime.runtime, true
}

func (m *RuntimeManager) supervise(ctx context.Context, profile config.RuntimeProfile, initialDone func()) {
	var attempts []time.Time
	var initial sync.Once
	defer initial.Do(initialDone)
	for ctx.Err() == nil {
		runtime, client, err := m.startProfile(ctx, profile)
		if err != nil {
			initial.Do(initialDone)
			m.log.Error("runtime start failed", "profile_id", profile.ID, "error", err)
			if !shouldRestart(profile.RestartPolicy, &attempts, time.Now()) {
				return
			}
			if !sleepContext(ctx, time.Second) {
				return
			}
			continue
		}
		m.install(runtime, client)
		m.emitRuntime(runtime, "runtime_started")
		if err := m.discover(ctx, runtime, client); err != nil {
			// Discovery RPC failures mean this runtime cannot currently be
			// reconciled. They are not a worker persistence failure and must not
			// tear down the gateway connection through RuntimeHooks.OnError.
			m.log.Warn("runtime initial discovery failed", "runtime_id", runtime.ID, "error", err)
			if isPersistenceError(err) {
				m.report(err)
			}
			_ = client.Close()
			m.markRuntimeFailed(runtime)
			initial.Do(initialDone)
			if !shouldRestart(profile.RestartPolicy, &attempts, time.Now()) {
				return
			}
			if !sleepContext(ctx, time.Second) {
				return
			}
			continue
		}
		initial.Do(initialDone)
		stopForwarding := m.forwardAdapter(ctx, runtime, client)
		select {
		case <-ctx.Done():
			_ = client.Close()
			stopForwarding()
			m.markRuntimeStopped(runtime)
			return
		case <-client.Done():
			stopForwarding()
			if ctx.Err() != nil {
				m.markRuntimeStopped(runtime)
				return
			}
		}
		m.remove(runtime.ID, client)
		m.markRuntimeFailed(runtime)
		if ctx.Err() != nil || !shouldRestart(profile.RestartPolicy, &attempts, time.Now()) {
			return
		}
		if !sleepContext(ctx, time.Second) {
			return
		}
	}
}

func (m *RuntimeManager) startProfile(ctx context.Context, profile config.RuntimeProfile) (protocol.Runtime, *codexadapter.Client, error) {
	// BeginRuntime commits the next generation before a process exists.
	runtime, err := m.store.BeginRuntime(profile.ID, profile.Name, profile.WorkingDirectory)
	if err != nil {
		return protocol.Runtime{}, nil, err
	}
	client, err := m.start(ctx, codexadapter.Config{Command: profile.CodexBinary, WorkingDirectory: profile.WorkingDirectory, Stderr: io.Discard})
	if err != nil {
		runtime.State = "failed"
		m.setRuntime(runtime, nil)
		m.emitRuntime(runtime, "runtime_failed")
		return runtime, nil, err
	}
	runtime.PID, runtime.State = client.PID(), "running"
	runtime.LocalSocket = client.LocalSocket()
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	version, versionErr := codexadapter.ExecutableVersion(versionCtx, profile.CodexBinary)
	cancel()
	if versionErr == nil {
		runtime.CodexVersion = version
		m.log.Info("runtime started", "profile_id", profile.ID, "codex_version", version)
	}
	return runtime, client, nil
}

// forwardAdapter starts all adapter readers under a private context and
// returns a join function. supervise calls that function before a client is
// removed or a runtime is restarted, so no hook can outlive the worker store.
func (m *RuntimeManager) forwardAdapter(ctx context.Context, runtime protocol.Runtime, client *codexadapter.Client) func() {
	forwardCtx, cancel := context.WithCancel(ctx)
	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		for {
			select {
			case <-forwardCtx.Done():
				return
			case event, ok := <-client.Events():
				if !ok {
					return
				}
				if m.hooks.OnEvent != nil {
					m.hooks.OnEvent(runtime, event)
				}
			}
		}
	}()
	go func() {
		defer group.Done()
		for {
			select {
			case <-forwardCtx.Done():
				return
			case request, ok := <-client.Requests():
				if !ok {
					return
				}
				if m.hooks.OnRequest != nil {
					m.hooks.OnRequest(runtime, request)
				}
			}
		}
	}()
	go func() {
		defer group.Done()
		ticker := time.NewTicker(discoveryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-forwardCtx.Done():
				return
			case <-client.Done():
				return
			case <-ticker.C:
				if err := m.discover(ctx, runtime, client); err != nil {
					m.log.Warn("runtime discovery failed", "runtime_id", runtime.ID, "error", err)
					if isPersistenceError(err) {
						m.report(err)
					}
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			group.Wait()
		})
	}
}

func (m *RuntimeManager) discover(ctx context.Context, runtime protocol.Runtime, client *codexadapter.Client) error {
	for _, method := range []string{"thread/list", "thread/loaded/list", "thread/read"} {
		if methodKnownUnavailable(client, method) {
			return nil
		}
	}
	threads, err := client.ListAllThreads(ctx, discoveryPageSize, discoveryLimit)
	if err != nil {
		if errors.Is(err, codexadapter.ErrMethodUnavailable) {
			m.markRuntimeDegraded(runtime, err)
			return nil
		}
		return err
	}
	loaded, err := loadedThreadIDs(ctx, client)
	if err != nil {
		if errors.Is(err, codexadapter.ErrMethodUnavailable) {
			m.markRuntimeDegraded(runtime, err)
			return nil
		}
		return err
	}
	// A loaded thread may not yet have a stored log or appear in a history
	// page. Read its current turn independently, without changing selection.
	indexed := make(map[string]int, len(threads))
	for index, thread := range threads {
		indexed[thread.ID] = index
	}
	for id := range loaded {
		thread, readErr := client.ReadThread(ctx, id, true)
		if readErr != nil {
			if errors.Is(readErr, codexadapter.ErrMethodUnavailable) {
				m.markRuntimeDegraded(runtime, readErr)
				return nil
			}
			return readErr
		}
		if index, ok := indexed[id]; ok {
			threads[index] = thread
		} else {
			threads = append(threads, thread)
		}
	}
	existing, err := m.store.ListSessions(runtime.ID)
	if err != nil {
		return persistenceError(err)
	}
	byThread := make(map[string]protocol.Session, len(existing))
	for _, session := range existing {
		byThread[session.ThreadID] = session
	}
	for _, thread := range threads {
		if thread.ID == "" {
			continue
		}
		candidate := sessionFromThread(runtime, thread, loaded[thread.ID])
		if candidate.CWD == "" || !workspaceAllowed(candidate.CWD, m.cfg.AllowedWorkspaceRoots) {
			m.log.Warn("skip discovered thread outside allowed workspace", "runtime_id", runtime.ID, "thread_id", thread.ID)
			continue
		}
		if loaded[thread.ID] {
			resumed, subscribed, resumeErr := m.subscribeLoadedThread(ctx, runtime, client, thread.ID)
			if resumeErr != nil {
				if errors.Is(resumeErr, codexadapter.ErrMethodUnavailable) {
					m.markRuntimeDegraded(runtime, resumeErr)
				} else {
					return resumeErr
				}
			} else if subscribed {
				// thread/resume returns a summary and may omit cwd/status. Keep the
				// workspace-validated thread/read snapshot, but retain a newer
				// active turn if this implementation happened to include one.
				if resumed.ID != thread.ID {
					return errors.New("runtime manager: resumed a different thread")
				}
				if resumed.ActiveTurnID != "" {
					candidate.ActiveTurnID = resumed.ActiveTurnID
					candidate.State = "running"
				}
			}
		}
		if prior, found := byThread[thread.ID]; found {
			if prior.CWD == "" || !workspaceAllowed(prior.CWD, m.cfg.AllowedWorkspaceRoots) {
				m.log.Warn("skip persisted thread outside allowed workspace", "runtime_id", runtime.ID, "thread_id", thread.ID)
				continue
			}
			candidate.ID = prior.ID
			if m.hooks.OnSession != nil {
				m.hooks.OnSession(runtime, candidate)
			}
			continue
		}
		saved, err := m.store.UpsertSession(candidate)
		if err != nil {
			return persistenceError(err)
		}
		if err := m.emit(runtime, "session_discovered", saved); err != nil {
			return persistenceError(err)
		}
		if m.hooks.OnSession != nil {
			m.hooks.OnSession(runtime, saved)
		}
	}
	return nil
}

func methodKnownUnavailable(client *codexadapter.Client, method string) bool {
	available, observed := client.Capabilities().Methods[method]
	return observed && !available
}

func loadedThreadIDs(ctx context.Context, client *codexadapter.Client) (map[string]bool, error) {
	loaded := map[string]bool{}
	cursor := ""
	for len(loaded) < discoveryLimit {
		page, err := client.LoadedThreads(ctx, cursor, discoveryPageSize)
		if err != nil {
			return nil, err
		}
		for _, thread := range page.Threads {
			loaded[thread.ID] = true
			if len(loaded) == discoveryLimit {
				break
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return loaded, nil
}

// subscribeLoadedThread performs the only allowed reconciliation resume: an
// already-loaded, workspace-validated thread with no options beyond threadId.
// The per-managed-runtime map resets when a new client/generation is installed.
func (m *RuntimeManager) subscribeLoadedThread(ctx context.Context, runtime protocol.Runtime, client *codexadapter.Client, threadID string) (codexadapter.Thread, bool, error) {
	if methodKnownUnavailable(client, "thread/resume") {
		// The first -32601 already marked this runtime degraded. Do not keep
		// probing an app-server version that has declared the required RPC absent.
		return codexadapter.Thread{}, false, nil
	}
	m.mu.Lock()
	current := m.runtimes[runtime.ID]
	if current == nil || current.client != client || current.runtime.Generation != runtime.Generation {
		m.mu.Unlock()
		return codexadapter.Thread{}, false, codexadapter.ErrClosed
	}
	if _, found := current.subscriptions[threadID]; found {
		m.mu.Unlock()
		return codexadapter.Thread{}, false, nil
	}
	// Reserve before issuing RPC so concurrent discovery ticks cannot attach the
	// same client/thread twice. Release the reservation on failure for retry.
	current.subscriptions[threadID] = struct{}{}
	m.mu.Unlock()
	thread, err := client.ResumeThread(ctx, threadID, codexadapter.ThreadOptions{})
	if err != nil {
		m.mu.Lock()
		if current := m.runtimes[runtime.ID]; current != nil && current.client == client && current.runtime.Generation == runtime.Generation {
			delete(current.subscriptions, threadID)
		}
		m.mu.Unlock()
		return codexadapter.Thread{}, false, err
	}
	return thread, true, nil
}

func (m *RuntimeManager) markRuntimeFailed(runtime protocol.Runtime) {
	runtime.State, runtime.PID = "failed", 0
	m.setRuntime(runtime, nil)
	m.emitRuntime(runtime, "runtime_failed")
	sessions, err := m.store.ListSessions(runtime.ID)
	if err != nil {
		m.log.Error("load failed runtime sessions", "error", err)
		return
	}
	for _, session := range sessions {
		session.State, session.ActiveTurnID, session.Loaded = "not_loaded", "", false
		saved, err := m.store.UpsertSession(session)
		if err != nil {
			m.report(err)
			continue
		}
		if err := m.emit(runtime, "session_state_changed", saved); err != nil {
			m.report(err)
		}
		if m.hooks.OnSession != nil {
			m.hooks.OnSession(runtime, saved)
		}
	}
}

func (m *RuntimeManager) markRuntimeStopped(runtime protocol.Runtime) {
	runtime.State, runtime.PID = "stopped", 0
	m.setRuntime(runtime, nil)
	m.emitRuntime(runtime, "runtime_stopped")
}

func (m *RuntimeManager) install(runtime protocol.Runtime, client *codexadapter.Client) {
	m.setRuntime(runtime, client)
}
func (m *RuntimeManager) setRuntime(runtime protocol.Runtime, client *codexadapter.Client) {
	m.mu.Lock()
	m.runtimes[runtime.ID] = &managedRuntime{runtime: runtime, client: client, subscriptions: make(map[string]struct{})}
	m.mu.Unlock()
}

func (m *RuntimeManager) markRuntimeDegraded(runtime protocol.Runtime, cause error) {
	runtime.State = "degraded"
	m.mu.Lock()
	current := m.runtimes[runtime.ID]
	if current == nil || current.runtime.Generation != runtime.Generation || current.degraded {
		m.mu.Unlock()
		return
	}
	current.runtime = runtime
	current.degraded = true
	m.mu.Unlock()
	m.log.Warn("runtime required RPC unavailable; running degraded", "runtime_id", runtime.ID, "error", cause)
	if err := m.emit(runtime, "runtime_degraded", runtime); err != nil {
		m.report(err)
	}
}
func (m *RuntimeManager) remove(runtimeID string, client *codexadapter.Client) {
	m.mu.Lock()
	if current := m.runtimes[runtimeID]; current != nil && current.client == client {
		delete(m.runtimes, runtimeID)
	}
	m.mu.Unlock()
}
func (m *RuntimeManager) closeAll() {
	m.mu.RLock()
	clients := make([]*codexadapter.Client, 0, len(m.runtimes))
	for _, runtime := range m.runtimes {
		if runtime.client != nil {
			clients = append(clients, runtime.client)
		}
	}
	m.mu.RUnlock()
	for _, client := range clients {
		_ = client.Close()
	}
}
func (m *RuntimeManager) emitRuntime(runtime protocol.Runtime, kind string) {
	if err := m.emit(runtime, kind, runtime); err != nil {
		m.report(err)
	}
}
func (m *RuntimeManager) emit(runtime protocol.Runtime, kind string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	event := protocol.Event{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, Kind: kind, Data: raw}
	if session, ok := data.(protocol.Session); ok {
		event.SessionID = session.ID
	}
	_, err = m.store.AppendEvent(event)
	return err
}
func (m *RuntimeManager) report(err error) {
	if err == nil {
		return
	}
	m.log.Error("runtime manager persistence failure", "error", err)
	if m.hooks.OnError != nil {
		m.hooks.OnError(err)
	}
}
func workspaceAllowed(path string, roots []string) bool {
	_, err := canonicalWorkspace(path, roots)
	return err == nil
}
func canonicalWorkspace(path string, roots []string) (string, error) {
	return auth.CanonicalWorkspace(path, roots)
}
func sessionFromThread(runtime protocol.Runtime, thread codexadapter.Thread, loaded bool) protocol.Session {
	state := "not_loaded"
	if loaded {
		state = "idle"
	}
	if thread.ActiveTurnID != "" || thread.Status == "running" || thread.Status == "active" {
		state = "running"
	}
	if thread.Status == "systemError" {
		state = "failed"
	}
	return protocol.Session{WorkerID: runtime.WorkerID, RuntimeID: runtime.ID, ThreadID: thread.ID, Name: thread.Name, Preview: thread.Preview, CWD: thread.CWD, State: state, ActiveTurnID: thread.ActiveTurnID, Loaded: loaded}
}
func shouldRestart(policy string, attempts *[]time.Time, now time.Time) bool {
	if policy != "on-failure" {
		return false
	}
	cutoff := now.Add(-10 * time.Minute)
	retained := (*attempts)[:0]
	for _, attempt := range *attempts {
		if attempt.After(cutoff) {
			retained = append(retained, attempt)
		}
	}
	*attempts = retained
	if len(*attempts) >= 5 {
		return false
	}
	*attempts = append(*attempts, now)
	return true
}
func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

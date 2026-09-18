package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
	bolt "go.etcd.io/bbolt"
)

const updateLeaseDuration = 120 * time.Second

var ErrUpdatePrepared = errors.New("worker update: command admission is temporarily paused")

// UpdateLease acknowledges the running worker's fenced admission state. An
// updater must verify PID against its service manager, then stop well before
// ExpiresAt. Merely reading a status file never authorizes a safe restart.
type UpdateLease struct {
	WorkerID  string    `json:"worker_id"`
	PID       int       `json:"pid"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}
type updateRequest struct {
	Action   string `json:"action"`
	WorkerID string `json:"worker_id"`
	Token    string `json:"token,omitempty"`
}
type updateResponse struct {
	Lease   *UpdateLease `json:"lease,omitempty"`
	Aborted bool         `json:"aborted,omitempty"`
	Error   string       `json:"error,omitempty"`
}
type updateState struct {
	lease   UpdateLease
	timer   *time.Timer
	proxies []*attachmentProxy
}

func updateControlPath(stateFile string) string {
	path := stateFile + ".control.sock"
	if len(path) <= 100 {
		return path
	}
	// Keep the shortened endpoint in the state directory: a worker service's
	// PrivateTmp namespace may differ from the updater's /tmp namespace.
	digest := sha256.Sum256([]byte(stateFile))
	return filepath.Join(filepath.Dir(stateFile), fmt.Sprintf("%x.sock", digest[:4]))
}

// RequestUpdate sends a maintenance request to the live worker over its private
// Unix socket; it never opens the worker's exclusively owned state database.
func RequestUpdate(ctx context.Context, cfg config.WorkerConfig, action, token string) (*UpdateLease, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if len(updateControlPath(cfg.StateFile)) > 100 {
		return nil, errors.New("worker update: state directory path is too long for a private control socket")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", updateControlPath(cfg.StateFile))
	if err != nil {
		return nil, errors.New("worker update coordination unavailable; the running worker must support update prepare")
	}
	defer connection.Close()
	deadline, _ := ctx.Deadline()
	_ = connection.SetDeadline(deadline)
	if err := json.NewEncoder(connection).Encode(updateRequest{Action: action, WorkerID: cfg.WorkerID, Token: token}); err != nil {
		return nil, err
	}
	var response updateResponse
	if err := json.NewDecoder(io.LimitReader(connection, 8192)).Decode(&response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	if action == "prepare" {
		if response.Lease == nil || response.Lease.WorkerID != cfg.WorkerID || response.Lease.PID <= 0 || response.Lease.Token == "" || !response.Lease.ExpiresAt.After(time.Now()) {
			return nil, errors.New("worker update: invalid lease response")
		}
	} else if !response.Aborted {
		return nil, errors.New("worker update: abort was not acknowledged")
	}
	return response.Lease, nil
}

func (a *Agent) startUpdateControl(ctx context.Context) (func(), error) {
	path := updateControlPath(a.cfg.StateFile)
	if len(path) > 100 {
		return nil, errors.New("worker update: state directory path is too long for a private control socket")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("worker update: control path is not a socket")
		}
		// OpenStore already holds the exclusive worker lock, so a prior worker
		// cannot still own this control endpoint.
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	run, cancel := context.WithCancel(ctx)
	var group sync.WaitGroup
	var mu sync.Mutex
	connections := map[net.Conn]struct{}{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			mu.Lock()
			connections[connection] = struct{}{}
			group.Add(1)
			mu.Unlock()
			go func() {
				defer group.Done()
				defer connection.Close()
				defer func() { mu.Lock(); delete(connections, connection); mu.Unlock() }()
				_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
				requestCtx, stop := context.WithTimeout(run, 5*time.Second)
				defer stop()
				var request updateRequest
				response := updateResponse{}
				if err := json.NewDecoder(io.LimitReader(connection, 8192)).Decode(&request); err != nil {
					response.Error = "worker update: invalid request"
				} else if request.WorkerID != a.cfg.WorkerID {
					response.Error = "worker update: worker identity mismatch"
				} else {
					switch request.Action {
					case "prepare":
						lease, err := a.prepareUpdate(requestCtx, updateLeaseDuration)
						if err != nil {
							response.Error = err.Error()
						} else {
							response.Lease = &lease
						}
					case "abort":
						if err := a.abortUpdate(request.Token); err != nil {
							response.Error = err.Error()
						} else {
							response.Aborted = true
						}
					default:
						response.Error = "worker update: invalid action"
					}
				}
				_ = json.NewEncoder(connection).Encode(response)
			}()
		}
	}()
	return func() {
		cancel()
		_ = listener.Close()
		<-done
		mu.Lock()
		for connection := range connections {
			_ = connection.Close()
		}
		mu.Unlock()
		group.Wait()
		a.updateMu.Lock()
		a.clearUpdateLocked()
		a.updateMu.Unlock()
	}, nil
}

func (a *Agent) prepareUpdate(ctx context.Context, ttl time.Duration) (UpdateLease, error) {
	if err := ctx.Err(); err != nil {
		return UpdateLease{}, err
	}
	if !a.updateMu.TryLock() {
		return UpdateLease{}, errors.New("worker update: command admission is busy")
	}
	defer a.updateMu.Unlock()
	if a.update != nil {
		return UpdateLease{}, errors.New("worker update: another update is already prepared")
	}
	if !a.manager.lifecycle.TryLock() {
		return UpdateLease{}, errors.New("worker update: runtime startup or discovery is in progress")
	}
	held := true
	var proxies []*attachmentProxy
	defer func() {
		if held {
			for _, proxy := range proxies {
				proxy.resume()
			}
			a.manager.lifecycle.Unlock()
		}
	}()
	a.manager.mu.RLock()
	for _, runtime := range a.manager.runtimes {
		if runtime.runtime.State == "starting" || runtime.runtime.State == "degraded" {
			a.manager.mu.RUnlock()
			return UpdateLease{}, errors.New("worker update: runtime is not settled")
		}
		if runtime.client != nil && runtime.runtime.LocalSocket != "" {
			proxy := a.manager.attachments[runtime.runtime.ID]
			if proxy == nil {
				a.manager.mu.RUnlock()
				return UpdateLease{}, errors.New("worker update: runtime attachment admission is not managed")
			}
			if err := proxy.pause(); err != nil {
				a.manager.mu.RUnlock()
				return UpdateLease{}, err
			}
			proxies = append(proxies, proxy)
		}
	}
	a.manager.mu.RUnlock()
	// Ask live app servers as well as worker actors. Native CLI turns may belong
	// to loaded threads outside the gateway's workspace discovery allowlist.
	if err := a.manager.verifyUpdateIdle(ctx); err != nil {
		return UpdateLease{}, err
	}
	a.mu.RLock()
	actors := make([]*sessionActor, 0, len(a.sessions))
	for _, actor := range a.sessions {
		actors = append(actors, actor)
	}
	a.mu.RUnlock()
	for _, actor := range actors {
		reply := make(chan bool, 1)
		select {
		case actor.updateChecks <- reply:
		case <-ctx.Done():
			return UpdateLease{}, errors.New("worker update: session is busy")
		}
		select {
		case idle := <-reply:
			if !idle {
				return UpdateLease{}, errors.New("worker update: session has active or queued work")
			}
		case <-ctx.Done():
			return UpdateLease{}, errors.New("worker update: session is busy")
		}
	}
	if err := a.store.checkUpdateIdle(); err != nil {
		return UpdateLease{}, err
	}
	// A server notification or approval can arrive while the other runtime and
	// actor checks are in progress. Recheck native activity behind the admission
	// fence before handing the updater a restart lease.
	for _, proxy := range proxies {
		if err := proxy.checkIdle(); err != nil {
			return UpdateLease{}, err
		}
	}
	lease := UpdateLease{WorkerID: a.cfg.WorkerID, PID: os.Getpid(), Token: uuid.NewString(), ExpiresAt: time.Now().UTC().Add(ttl)}
	a.update = &updateState{lease: lease, proxies: proxies}
	a.update.timer = time.AfterFunc(ttl, func() { _ = a.abortUpdate(lease.Token) })
	held = false
	return lease, nil
}
func (a *Agent) abortUpdate(token string) error {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	if a.update == nil {
		return errors.New("worker update: no prepared update")
	}
	if token == "" || token != a.update.lease.Token {
		return errors.New("worker update: lease token does not match")
	}
	a.clearUpdateLocked()
	return nil
}
func (a *Agent) clearUpdateLocked() {
	if a.update == nil {
		return
	}
	a.update.timer.Stop()
	for _, proxy := range a.update.proxies {
		proxy.resume()
	}
	a.update = nil
	a.manager.lifecycle.Unlock()
}
func (s *Store) checkUpdateIdle() error {
	return s.db.View(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketCommands).ForEach(func(_, value []byte) error {
			record, err := decodeCommand(value)
			if err != nil {
				return err
			}
			if record.State == CommandReceived || record.State == CommandExecuting {
				return errors.New("worker update: commands are queued or executing")
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.Bucket(bucketSessions).ForEach(func(_, value []byte) error {
			var session protocol.Session
			if err := json.Unmarshal(value, &session); err != nil {
				return err
			}
			if !updateSessionIdle(session) {
				return errors.New("worker update: a session has an active turn or pending response")
			}
			return nil
		}); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		if parseSequence(meta.Get(keyLastAck))+1 != parseSequence(meta.Get(keyNextEvent)) {
			return errors.New("worker update: durable events are awaiting gateway acknowledgement")
		}
		return nil
	})
}
func updateSessionIdle(session protocol.Session) bool {
	if session.ActiveTurnID != "" {
		return false
	}
	switch session.State {
	case "idle", "not_loaded", "failed", "":
		return true
	default:
		return false
	}
}
func (m *RuntimeManager) verifyUpdateIdle(ctx context.Context) error {
	m.mu.RLock()
	runtimes := make([]*managedRuntime, 0, len(m.runtimes))
	for _, runtime := range m.runtimes {
		runtimes = append(runtimes, runtime)
	}
	m.mu.RUnlock()
	for _, runtime := range runtimes {
		if runtime.client == nil {
			continue
		}
		if !runtime.client.UpdateQuiescent() {
			return errors.New("worker update: runtime has pending or unconfirmed RPCs; finish work and stop the worker service before updating")
		}
		cursor := ""
		seen := map[string]bool{}
		count := 0
		for {
			page, err := runtime.client.LoadedThreads(ctx, cursor, discoveryPageSize)
			if err != nil {
				return fmt.Errorf("worker update: cannot verify loaded threads: %w", err)
			}
			for _, entry := range page.Threads {
				count++
				if count > discoveryLimit {
					return errors.New("worker update: too many loaded threads to verify")
				}
				thread, err := runtime.client.ReadThread(ctx, entry.ID, true)
				if err != nil {
					return fmt.Errorf("worker update: cannot verify native thread: %w", err)
				}
				if thread.ActiveTurnID != "" || (thread.Status != "idle" && thread.Status != "notLoaded" && thread.Status != "not_loaded") {
					return errors.New("worker update: a native CLI thread is active or its idle state cannot be verified")
				}
			}
			if page.NextCursor == "" {
				break
			}
			if seen[page.NextCursor] {
				return errors.New("worker update: repeated loaded-thread cursor")
			}
			seen[page.NextCursor] = true
			cursor = page.NextCursor
		}
		if !runtime.client.UpdateQuiescent() {
			return errors.New("worker update: runtime RPC state changed while checking idle")
		}
	}
	return nil
}

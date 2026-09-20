package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

type WorkerRegistry interface {
	AuthenticateWorker(context.Context, string) (registry.Worker, error)
	RegisterConnection(context.Context, string, uuid.UUID, protocol.Hello) (registry.Worker, error)
	RecordHeartbeat(context.Context, registry.Heartbeat) error
	EventWatermark(context.Context, uuid.UUID) (int64, error)
	Disconnect(context.Context, uuid.UUID, uuid.UUID) error
	MarkUnreachable(context.Context, time.Duration) (int64, error)
	CheckConnection(context.Context, uuid.UUID, uuid.UUID) error
}

type Hub struct {
	store                  WorkerRegistry
	log                    *slog.Logger
	heartbeat, unreachable time.Duration
	mu                     sync.RWMutex
	peers                  map[string]*peer
	limiter                *failureLimiter
	// EventHandler must durably commit before returning nil. Without a handler,
	// events are deliberately not ACKed, so the worker retains its outbox.
	EventHandler func(context.Context, uuid.UUID, uuid.UUID, protocol.Event) error
	AckHandler   func(context.Context, uuid.UUID, uuid.UUID, protocol.CommandAck) error
}

type peer struct {
	workerID, connectionID      uuid.UUID
	supportsWorkerUpdate        bool
	supportsImageInput          bool
	supportsSessionWorkspaces   bool
	supportsSessionDeletion     bool
	supportsConversationHistory bool
	conn                        *websocket.Conn
	ctx                         context.Context
	cancel                      context.CancelFunc
	writes                      chan writeRequest
}

type writeRequest struct {
	envelope protocol.Envelope
	done     chan error
}

func NewHub(store WorkerRegistry, logger *slog.Logger, heartbeat, unreachable time.Duration) *Hub {
	return &Hub{store: store, log: logger, heartbeat: heartbeat, unreachable: unreachable, peers: map[string]*peer{}, limiter: &failureLimiter{entries: map[string]failureWindow{}}}
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Forwarding headers are ignored here. The listener is loopback-only; nginx
	// adds its own independent public-IP rate limit in the deployment config.
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !h.limiter.Allow(ip, time.Now()) {
		http.Error(w, "try again later", http.StatusTooManyRequests)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || len(auth) > 512 {
		h.limiter.Fail(ip, time.Now())
		http.Error(w, "unauthorized", 401)
		return
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	worker, err := h.store.AuthenticateWorker(ctx, token)
	cancel()
	if err != nil {
		if errors.Is(err, registry.ErrInvalidToken) {
			h.limiter.Fail(ip, time.Now())
			http.Error(w, "unauthorized", 401)
		} else {
			http.Error(w, "registry unavailable", 503)
		}
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxFrameBytes)
	ctx, cancel = context.WithCancel(r.Context())
	defer cancel()
	helloCtx, helloCancel := context.WithTimeout(ctx, 10*time.Second)
	frame, err := readEnvelope(helloCtx, conn)
	helloCancel()
	if err != nil || frame.Type != "hello" {
		conn.Close(websocket.StatusPolicyViolation, "hello required")
		return
	}
	hello, err := protocol.Payload[protocol.Hello](frame)
	if err != nil || hello.WorkerID != worker.ID.String() || hello.ProtocolMin > protocol.Version || hello.ProtocolMax < protocol.Version || hello.ProtocolMin < 1 || hello.ProtocolMax < hello.ProtocolMin || (hello.OS != "linux" && hello.OS != "darwin") || (hello.Arch != "amd64" && hello.Arch != "arm64") {
		conn.Close(websocket.StatusPolicyViolation, "invalid worker hello")
		return
	}
	connectionID := uuid.New()
	registerCtx, registerCancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = h.store.RegisterConnection(registerCtx, token, connectionID, hello)
	registerCancel()
	token = ""
	if err != nil {
		conn.Close(websocket.StatusPolicyViolation, "connection rejected")
		return
	}
	// v0.5.19 introduced these operations before explicit capability flags.
	// Only that known release gets the compatibility fallback; subsequent
	// builds advertise their actual capabilities, including development builds.
	legacySessionSupport := strings.TrimPrefix(hello.WorkerVersion, "v") == "0.5.19"
	p := &peer{workerID: worker.ID, connectionID: connectionID, supportsImageInput: hello.SupportsImageInput,
		supportsWorkerUpdate:        hello.SupportsWorkerUpdate,
		supportsSessionWorkspaces:   hello.SupportsSessionWorkspaces || legacySessionSupport,
		supportsSessionDeletion:     hello.SupportsSessionDeletion || legacySessionSupport,
		supportsConversationHistory: hello.SupportsConversationHistory,
		conn:                        conn, ctx: ctx, cancel: cancel, writes: make(chan writeRequest, 128)}
	h.mu.Lock()
	old := h.peers[hello.WorkerID]
	h.peers[hello.WorkerID] = p
	h.mu.Unlock()
	if old != nil {
		old.cancel()
		old.conn.CloseNow()
	}
	defer func() {
		cancel()
		h.mu.Lock()
		if h.peers[hello.WorkerID] == p {
			delete(h.peers, hello.WorkerID)
		}
		h.mu.Unlock()
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := h.store.Disconnect(cleanup, worker.ID, connectionID); err != nil {
			h.log.Warn("worker disconnect persistence failed", "worker_id", worker.ID, "error", err)
		}
	}()
	go p.writer()
	watermark, err := h.store.EventWatermark(ctx, worker.ID)
	if err != nil || watermark < 0 {
		return
	}
	if err = p.send(ctx, "hello_ack", protocol.HelloAck{ConnectionID: connectionID.String(), HeartbeatIntervalSeconds: max(1, int(h.heartbeat/time.Second)), ResumeFromEventSeq: uint64(watermark)}); err != nil {
		return
	}
	h.log.Info("worker connected", "worker_id", worker.ID, "connection_id", connectionID)
	for {
		readCtx, done := context.WithTimeout(ctx, h.unreachable)
		e, err := readEnvelope(readCtx, conn)
		done()
		if err != nil {
			return
		}
		opCtx, done := context.WithTimeout(ctx, 10*time.Second)
		err = h.handle(opCtx, p, e)
		done()
		if err != nil {
			h.log.Warn("worker message rejected", "worker_id", worker.ID, "type", e.Type, "error", err)
			return
		}
	}
}

func (h *Hub) handle(ctx context.Context, p *peer, e protocol.Envelope) error {
	switch e.Type {
	case "heartbeat":
		hb, err := protocol.Payload[protocol.Heartbeat](e)
		if err != nil {
			return err
		}
		if hb.WorkerID != p.workerID.String() {
			return errors.New("heartbeat identity mismatch")
		}
		runtimes, err := registryRuntimes(p.workerID, hb.Runtimes)
		if err != nil {
			return err
		}
		return h.store.RecordHeartbeat(ctx, registry.Heartbeat{WorkerID: p.workerID, ConnectionID: p.connectionID, Runtimes: runtimes, Metadata: e.Payload})
	case "worker_event":
		event, err := protocol.Payload[protocol.Event](e)
		if err != nil {
			return err
		}
		if event.WorkerID != p.workerID.String() {
			return errors.New("event identity mismatch")
		}
		if h.EventHandler == nil {
			return errors.New("event ingestion unavailable")
		}
		if err = h.EventHandler(ctx, p.workerID, p.connectionID, event); err != nil {
			return err
		}
		if event.Durable() {
			return p.send(ctx, "event_ack", protocol.EventAck{Seq: event.Seq})
		}
		return nil
	case "command_ack":
		ack, err := protocol.Payload[protocol.CommandAck](e)
		if err != nil {
			return err
		}
		if h.AckHandler == nil {
			return errors.New("command ingestion unavailable")
		}
		return h.AckHandler(ctx, p.workerID, p.connectionID, ack)
	default:
		return errors.New("unexpected worker message")
	}
}

func registryRuntimes(workerID uuid.UUID, rs []protocol.Runtime) ([]registry.Runtime, error) {
	result := make([]registry.Runtime, 0, len(rs))
	for _, r := range rs {
		id, err := uuid.Parse(r.ID)
		if err != nil || r.Generation > math.MaxInt64 {
			return nil, errors.New("invalid runtime identity")
		}
		if r.WorkerID != "" && r.WorkerID != workerID.String() {
			return nil, errors.New("runtime worker mismatch")
		}
		pid := int64(r.PID)
		result = append(result, registry.Runtime{ID: id, WorkerID: workerID, ProfileID: r.ProfileID, Name: r.Name, Generation: int64(r.Generation), PID: &pid, State: r.State, CodexVersion: r.CodexVersion, DefaultCWD: r.DefaultCWD})
	}
	return result, nil
}

func readEnvelope(ctx context.Context, conn *websocket.Conn) (protocol.Envelope, error) {
	kind, data, err := conn.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	if kind != websocket.MessageText {
		return protocol.Envelope{}, errors.New("text frames required")
	}
	return protocol.Decode(data)
}

func (p *peer) writer() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case req := <-p.writes:
			data, err := json.Marshal(req.envelope)
			if err == nil {
				ctx, cancel := context.WithTimeout(p.ctx, 15*time.Second)
				err = p.conn.Write(ctx, websocket.MessageText, data)
				cancel()
			}
			req.done <- err
			if err != nil {
				p.cancel()
				return
			}
		}
	}
}

func (p *peer) send(ctx context.Context, kind string, payload any) error {
	e, err := protocol.NewEnvelope(kind, payload)
	if err != nil {
		return err
	}
	req := writeRequest{envelope: e, done: make(chan error, 1)}
	select {
	case p.writes <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

func (h *Hub) SendCommand(ctx context.Context, c protocol.Command) error {
	if err := c.Validate(); err != nil {
		return err
	}
	h.mu.RLock()
	p := h.peers[c.WorkerID]
	h.mu.RUnlock()
	if p == nil {
		return errors.New("worker is not connected")
	}
	if len(c.Arguments.Images) > 0 && !p.supportsImageInput {
		return errors.New("worker must be updated before receiving image input")
	}
	if err := h.store.CheckConnection(ctx, p.workerID, p.connectionID); err != nil {
		return err
	}
	needsWorkspaces := c.Operation == protocol.BrowseWorkspace || (c.Operation == protocol.NewSession && (c.Arguments.CreateDirectory || c.Arguments.SessionName != ""))
	needsConversationHistory := c.Operation == protocol.ReadHistory && c.Arguments.History != nil && c.Arguments.History.Messages
	if (needsWorkspaces && !p.supportsSessionWorkspaces) || (c.Operation == protocol.DeleteSession && !p.supportsSessionDeletion) || (needsConversationHistory && !p.supportsConversationHistory) {
		// Old workers disconnect before acknowledging unknown operations. A
		// transport error here would just replay the command until expiry.
		// Record a terminal rejection through the same durable path as a worker
		// rejection so the wizard can recover and explain the required update.
		if h.AckHandler == nil {
			return errors.New("command ingestion unavailable")
		}
		return h.AckHandler(ctx, p.workerID, p.connectionID, protocol.CommandAck{CommandID: c.ID, Status: "rejected",
			Error: &protocol.Error{Code: protocol.UnsupportedOperation, Message: "Update the worker to support this session action, then try again."}})
	}
	return p.send(ctx, "command", c)
}

func (h *Hub) ConnectedWorkers() []uuid.UUID {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]uuid.UUID, 0, len(h.peers))
	for _, p := range h.peers {
		if p.ctx.Err() == nil {
			ids = append(ids, p.workerID)
		}
	}
	return ids
}

func (h *Hub) Run(ctx context.Context) {
	timer := time.NewTicker(h.heartbeat)
	defer timer.Stop()
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, p := range h.peers {
			p.cancel()
			p.conn.CloseNow()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := h.store.MarkUnreachable(opCtx, h.unreachable)
			cancel()
			if err != nil {
				h.log.Warn("worker liveness sweep failed", "error", err)
			}
		}
	}
}

type failureWindow struct {
	since time.Time
	count int
}
type failureLimiter struct {
	mu      sync.Mutex
	entries map[string]failureWindow
}

func (l *failureLimiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	return now.Sub(entry.since) >= time.Minute || entry.count < 20
}
func (l *failureLimiter) Fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) >= 4096 {
		for k, v := range l.entries {
			if now.Sub(v.since) >= time.Minute {
				delete(l.entries, k)
			}
		}
		if len(l.entries) >= 4096 {
			return
		}
	}
	e := l.entries[key]
	if now.Sub(e.since) >= time.Minute {
		e = failureWindow{since: now}
	}
	e.count++
	l.entries[key] = e
}

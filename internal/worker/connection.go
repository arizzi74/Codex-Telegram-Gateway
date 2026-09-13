package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/iaia/telegramgw/internal/buildinfo"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

const (
	minimumReconnectDelay = time.Second
	maximumReconnectDelay = 30 * time.Second
	outboxPollInterval    = 200 * time.Millisecond
)

// Connection owns only the Worker-to-Gateway transport. It neither starts nor
// stops local Codex runtimes; connection loss leaves runtime supervision alone.
type Connection struct {
	connected atomic.Bool
	cfg       config.WorkerConfig
	store     *Store
	log       *slog.Logger
	snapshot  func() []protocol.Runtime
	onCommand func(context.Context, protocol.Command) (protocol.CommandAck, error)
	startedAt time.Time

	// dial is replaceable only by package tests, allowing an httptest TLS client
	// without a production configuration switch for insecure TLS.
	dial func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
}

func (c *Connection) Connected() bool { return c.connected.Load() }

// NewConnection constructs an outbound-only worker transport. onCommand must
// durably receive the command before it returns an accepted acknowledgement.
func NewConnection(cfg config.WorkerConfig, store *Store, logger *slog.Logger, snapshot func() []protocol.Runtime, onCommand func(context.Context, protocol.Command) (protocol.CommandAck, error)) (*Connection, error) {
	if store == nil {
		return nil, errors.New("worker connection: store is required")
	}
	if _, err := uuid.Parse(cfg.WorkerID); err != nil {
		return nil, errors.New("worker connection: worker_id must be a UUID")
	}
	endpoint, err := url.Parse(cfg.GatewayURL)
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" {
		return nil, errors.New("worker connection: gateway_url must be an absolute wss URL")
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("worker connection: worker_id, gateway_url, and token_file are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if snapshot == nil {
		snapshot = func() []protocol.Runtime { return nil }
	}
	if onCommand == nil {
		return nil, errors.New("worker connection: command handler is required")
	}
	return &Connection{cfg: cfg, store: store, log: logger, snapshot: snapshot, onCommand: onCommand, startedAt: time.Now(), dial: websocket.Dial}, nil
}

// Run reconnects until ctx is cancelled. A failed connection never affects
// local runtimes; it only delays the next outbound dial.
func (c *Connection) Run(ctx context.Context) error {
	delay := minimumReconnectDelay
	for {
		if ctx.Err() != nil {
			return nil
		}
		started := time.Now()
		err := c.connect(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) >= 10*time.Second {
			delay = minimumReconnectDelay
		} else {
			delay = min(maximumReconnectDelay, delay*2)
		}
		wait := jitter(delay)
		c.log.Warn("gateway connection ended; reconnecting", "error", err, "delay", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (c *Connection) connect(ctx context.Context) error {
	token, err := readEnrollmentToken(c.cfg.TokenFile)
	if err != nil {
		return err
	}
	options := &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}}, CompressionMode: websocket.CompressionDisabled}
	dialCtx, stopDial := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := c.dial(dialCtx, c.cfg.GatewayURL, options)
	stopDial()
	if err != nil {
		return fmt.Errorf("dial gateway: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxFrameBytes)
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	writes := make(chan outbound, 128)
	sent := &sentWatermark{}
	writerDone := make(chan struct{})
	go runWriter(connectionCtx, conn, writes, sent, cancel, writerDone)
	defer func() { cancel(); <-writerDone }()

	lastAcked, _, err := c.store.EventWatermarks()
	if err != nil {
		return fmt.Errorf("load local event watermark: %w", err)
	}
	hostname, _ := os.Hostname()
	hello := protocol.Hello{WorkerID: c.cfg.WorkerID, WorkerName: c.cfg.Name, Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, WorkerVersion: buildinfo.Version, ProtocolMin: protocol.Version, ProtocolMax: protocol.Version, LastAckedEventSeq: lastAcked, Runtimes: c.snapshot()}
	handshakeCtx, stopHandshake := context.WithTimeout(connectionCtx, 10*time.Second)
	if err := sendEnvelope(handshakeCtx, writes, "hello", hello); err != nil {
		stopHandshake()
		return err
	}
	ackEnvelope, err := readWorkerEnvelope(handshakeCtx, conn)
	stopHandshake()
	if err != nil {
		return fmt.Errorf("read hello acknowledgement: %w", err)
	}
	if ackEnvelope.Type != "hello_ack" {
		return errors.New("worker connection: hello_ack required")
	}
	helloAck, err := protocol.Payload[protocol.HelloAck](ackEnvelope)
	if err != nil || helloAck.HeartbeatIntervalSeconds < 1 || helloAck.HeartbeatIntervalSeconds > 60 {
		return errors.New("worker connection: invalid hello_ack")
	}
	if _, err := uuid.Parse(helloAck.ConnectionID); err != nil {
		return errors.New("worker connection: invalid connection ID")
	}
	if err := c.acceptGatewayWatermark(helloAck.ResumeFromEventSeq); err != nil {
		return err
	}
	c.connected.Store(true)
	defer c.connected.Store(false)
	sent.set(helloAck.ResumeFromEventSeq)

	// Start reading before replay: the gateway can ACK while a large outbox is
	// being written, and the single writer remains free to drain the replay.
	readErr := make(chan error, 1)
	go func() { readErr <- c.readLoop(connectionCtx, conn, writes, sent) }()

	heartbeat := time.NewTicker(time.Duration(helloAck.HeartbeatIntervalSeconds) * time.Second)
	poll := time.NewTicker(outboxPollInterval)
	defer heartbeat.Stop()
	defer poll.Stop()
	if err := c.replayOutbox(connectionCtx, writes, sent); err != nil {
		return err
	}
	for {
		select {
		case <-connectionCtx.Done():
			return connectionCtx.Err()
		case err := <-readErr:
			return err
		case <-heartbeat.C:
			if err := sendEnvelope(connectionCtx, writes, "heartbeat", protocol.Heartbeat{WorkerID: c.cfg.WorkerID, UptimeSeconds: int64(time.Since(c.startedAt).Seconds()), Runtimes: c.snapshot()}); err != nil {
				return err
			}
		case <-poll.C:
			if err := c.replayOutbox(connectionCtx, writes, sent); err != nil {
				return err
			}
		}
	}
}

func (c *Connection) readLoop(ctx context.Context, conn *websocket.Conn, writes chan<- outbound, sent *sentWatermark) error {
	for {
		e, err := readWorkerEnvelope(ctx, conn)
		if err != nil {
			return err
		}
		switch e.Type {
		case "event_ack":
			ack, err := protocol.Payload[protocol.EventAck](e)
			if err != nil {
				return err
			}
			if ack.Seq > sent.get() {
				return errors.New("worker connection: gateway acknowledged an event not sent on this connection")
			}
			if err := c.store.AckThrough(ack.Seq); err != nil {
				return fmt.Errorf("gateway event acknowledgement rejected: %w", err)
			}
		case "command":
			command, err := protocol.Payload[protocol.Command](e)
			if err != nil {
				return err
			}
			if command.WorkerID != c.cfg.WorkerID {
				return errors.New("worker connection: command targets another worker")
			}
			if err := command.Validate(); err != nil {
				return fmt.Errorf("worker connection: invalid command: %w", err)
			}
			c.handleCommand(ctx, writes, command)
		default:
			return fmt.Errorf("worker connection: unexpected gateway envelope %q", e.Type)
		}
	}
}

func (c *Connection) handleCommand(ctx context.Context, writes chan<- outbound, command protocol.Command) {
	ack, err := c.onCommand(ctx, command)
	if err != nil {
		ack = protocol.CommandAck{CommandID: command.ID, Status: "rejected", Error: &protocol.Error{Code: protocol.InternalError, Message: "worker command handling failed"}}
	}
	if ack.CommandID == "" {
		ack.CommandID = command.ID
	}
	if err := sendEnvelope(ctx, writes, "command_ack", ack); err != nil && ctx.Err() == nil {
		c.log.Warn("send command acknowledgement", "command_id", command.ID, "error", err)
	}
}

func (c *Connection) replayOutbox(ctx context.Context, writes chan<- outbound, sent *sentWatermark) error {
	events, err := c.store.OutboxAfter(sent.get())
	if err != nil {
		return err
	}
	for _, event := range events {
		if err := sendEventEnvelope(ctx, writes, event); err != nil {
			return err
		}
	}
	return nil
}

func (c *Connection) acceptGatewayWatermark(gateway uint64) error {
	lastAcked, highWater, err := c.store.EventWatermarks()
	if err != nil {
		return err
	}
	if gateway > highWater {
		return errors.New("worker connection: gateway watermark exceeds local event high-water mark")
	}
	if gateway < lastAcked {
		return errors.New("worker connection: gateway watermark rolled back below locally acknowledged events")
	}
	if gateway > lastAcked {
		return c.store.AckThrough(gateway)
	}
	return nil
}

type outbound struct {
	envelope protocol.Envelope
	eventSeq uint64
	done     chan error
}

func runWriter(ctx context.Context, conn *websocket.Conn, writes <-chan outbound, sent *sentWatermark, cancel context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-writes:
			if request.eventSeq != 0 {
				sent.set(request.eventSeq)
			}
			data, err := json.Marshal(request.envelope)
			if err == nil {
				writeCtx, stop := context.WithTimeout(ctx, 15*time.Second)
				err = conn.Write(writeCtx, websocket.MessageText, data)
				stop()
			}
			request.done <- err
			if err != nil {
				cancel()
				return
			}
		}
	}
}

func sendEnvelope(ctx context.Context, writes chan<- outbound, kind string, payload any) error {
	e, err := protocol.NewEnvelope(kind, payload)
	if err != nil {
		return err
	}
	request := outbound{envelope: e, done: make(chan error, 1)}
	select {
	case writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sendEventEnvelope(ctx context.Context, writes chan<- outbound, event protocol.Event) error {
	e, err := protocol.NewEnvelope("worker_event", event)
	if err != nil {
		return err
	}
	request := outbound{envelope: e, eventSeq: event.Seq, done: make(chan error, 1)}
	select {
	case writes <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func readWorkerEnvelope(ctx context.Context, conn *websocket.Conn) (protocol.Envelope, error) {
	kind, data, err := conn.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	if kind != websocket.MessageText {
		return protocol.Envelope{}, errors.New("worker connection: text frame required")
	}
	return protocol.Decode(data)
}

func readEnrollmentToken(path string) (string, error) {
	if err := config.CheckSecretFilePermissions(path); err != nil {
		return "", fmt.Errorf("worker connection: token file: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("worker connection: read token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("worker connection: token file is empty")
	}
	return token, nil
}

func jitter(delay time.Duration) time.Duration {
	return delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))
}

type sentWatermark struct {
	mu    sync.RWMutex
	value uint64
}

func (s *sentWatermark) get() uint64 { s.mu.RLock(); defer s.mu.RUnlock(); return s.value }
func (s *sentWatermark) set(seq uint64) {
	s.mu.Lock()
	if seq > s.value {
		s.value = seq
	}
	s.mu.Unlock()
}

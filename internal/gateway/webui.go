package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

var (
	ErrWebUIUnavailable = errors.New("worker is disconnected; reconnect when it is online")
	ErrWebUIUnsupported = errors.New("update this worker to enable the web interface")
	ErrWebUILimit       = errors.New("too many open web sessions; disconnect another tab first")
)

// WebUIStream is a transient, connection-fenced app-server relay. Its messages
// never enter the durable command queue: reconnecting cannot replay a prompt.
type WebUIStream struct {
	hub         *Hub
	peer        *peer
	id          string
	ctx         context.Context
	cancel      context.CancelFunc
	frames      chan protocol.WebUIFrame
	once        sync.Once
	mu          sync.Mutex
	err         error
	queuedBytes int // protected by hub.mu
	inputBytes  int // in-flight sends; protected by hub.mu
}

func (h *Hub) OpenWebUI(ctx context.Context, session registry.AdminSession) (*WebUIStream, error) {
	h.mu.Lock()
	p := h.peers[session.WorkerID]
	if p == nil || p.ctx.Err() != nil {
		h.mu.Unlock()
		return nil, ErrWebUIUnavailable
	}
	if !p.supportsWebUI {
		h.mu.Unlock()
		return nil, ErrWebUIUnsupported
	}
	if len(h.webuis) >= 64 {
		h.mu.Unlock()
		return nil, ErrWebUILimit
	}
	workerCount := 0
	for _, stream := range h.webuis {
		if stream.peer == p {
			workerCount++
		}
	}
	if workerCount >= 8 {
		h.mu.Unlock()
		return nil, ErrWebUILimit
	}
	streamCtx, cancel := context.WithCancel(ctx)
	s := &WebUIStream{hub: h, peer: p, id: uuid.NewString(), ctx: streamCtx, cancel: cancel, frames: make(chan protocol.WebUIFrame, 64)}
	h.webuis[s.id] = s
	h.mu.Unlock()
	if err := h.store.CheckConnection(ctx, p.workerID, p.connectionID); err != nil {
		s.Close()
		return nil, ErrWebUIUnavailable
	}
	openCtx, done := context.WithTimeout(ctx, 10*time.Second)
	err := p.send(openCtx, "webui", protocol.WebUIFrame{ID: s.id, Action: "open", RuntimeID: session.RuntimeID, RuntimeGeneration: session.RuntimeGeneration, SessionID: session.ID})
	done()
	if err != nil {
		s.Close()
		return nil, ErrWebUIUnavailable
	}
	go func() {
		select {
		case <-p.ctx.Done():
			s.stop(ErrWebUIUnavailable)
		case <-streamCtx.Done():
		}
		s.Close()
	}()
	return s, nil
}

func (s *WebUIStream) Frames() <-chan protocol.WebUIFrame { return s.frames }

// Consumed releases capacity after the frame has been written or discarded.
func (s *WebUIStream) Consumed(frame protocol.WebUIFrame) {
	s.hub.mu.Lock()
	n := min(webUIFrameSize(frame), s.queuedBytes)
	s.queuedBytes -= n
	s.hub.webuiQueuedBytes -= n
	s.hub.mu.Unlock()
}

func webUIFrameSize(frame protocol.WebUIFrame) int {
	return len(frame.Data) + len(frame.Error) + 1024
}
func (s *WebUIStream) Done() <-chan struct{} { return s.ctx.Done() }
func (s *WebUIStream) Err() error            { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *WebUIStream) stop(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.cancel()
}

// Check keeps revocation and replacement fences in force even for read-only
// browser connections that are receiving output without submitting input.
func (s *WebUIStream) Check(ctx context.Context) error {
	if s.ctx.Err() != nil || s.peer.ctx.Err() != nil {
		return ErrWebUIUnavailable
	}
	s.hub.mu.RLock()
	current := s.hub.peers[s.peer.workerID.String()] == s.peer
	s.hub.mu.RUnlock()
	if !current {
		return ErrWebUIUnavailable
	}
	return s.hub.store.CheckConnection(ctx, s.peer.workerID, s.peer.connectionID)
}

func (s *WebUIStream) Send(ctx context.Context, data json.RawMessage) error {
	if len(data) == 0 || len(data) > protocol.MaxWebUIInputBytes || !json.Valid(data) {
		return errors.New("invalid web request")
	}
	if err := s.Check(ctx); err != nil {
		return err
	}
	s.hub.mu.Lock()
	if s.inputBytes+len(data) > 20<<20 || s.hub.webuiInputBytes+len(data) > 64<<20 {
		s.hub.mu.Unlock()
		return errors.New("web input byte limit exceeded")
	}
	s.inputBytes += len(data)
	s.hub.webuiInputBytes += len(data)
	s.hub.mu.Unlock()
	release := func() {
		s.hub.mu.Lock()
		s.inputBytes -= len(data)
		s.hub.webuiInputBytes -= len(data)
		s.hub.mu.Unlock()
	}
	return s.peer.sendWithCompletion(ctx, "webui", protocol.WebUIFrame{ID: s.id, Action: "input", Data: data}, release)
}

func (s *WebUIStream) Close() {
	s.once.Do(func() {
		s.cancel()
		s.hub.mu.Lock()
		delete(s.hub.webuis, s.id)
		// Drop queued frames but retain accounting for the consumer's current
		// frame until its write returns and calls Consumed.
	drain:
		for {
			select {
			case frame := <-s.frames:
				n := min(webUIFrameSize(frame), s.queuedBytes)
				s.queuedBytes -= n
				s.hub.webuiQueuedBytes -= n
			default:
				break drain
			}
		}
		s.hub.mu.Unlock()
		// Detach only. Backend work belongs to app-server, not this viewer.
		if s.peer.ctx.Err() == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.peer.send(ctx, "webui", protocol.WebUIFrame{ID: s.id, Action: "close"})
		}
	})
}

func (h *Hub) handleWebUI(p *peer, envelope protocol.Envelope) error {
	f, err := protocol.Payload[protocol.WebUIFrame](envelope)
	if err != nil {
		return errors.New("invalid web relay frame")
	}
	if _, err := uuid.Parse(f.ID); err != nil {
		return errors.New("invalid web relay identity")
	}
	if f.Action != "ready" && f.Action != "output" && f.Action != "error" {
		return errors.New("invalid web relay action")
	}
	h.mu.Lock()
	s := h.webuis[f.ID]
	// Late output for a detached browser is harmless. A different worker or
	// connection can never publish into another worker's browser stream.
	if s == nil {
		h.mu.Unlock()
		return nil
	}
	if s.peer != p {
		h.mu.Unlock()
		return errors.New("web relay connection mismatch")
	}
	if f.Action == "output" && (len(f.Data) == 0 || !json.Valid(f.Data)) {
		h.mu.Unlock()
		return errors.New("invalid web relay output")
	}
	// History pages can be large. Bound bytes as well as frame count, globally
	// and per viewer, rather than allowing dozens of 16 MiB frames per tab.
	n := webUIFrameSize(f)
	if s.queuedBytes+n > 20<<20 || h.webuiQueuedBytes+n > 64<<20 {
		h.mu.Unlock()
		s.stop(errors.New("web output buffer is full; reconnect to refresh the conversation"))
		return nil
	}
	s.queuedBytes += n
	h.webuiQueuedBytes += n
	select {
	case <-s.ctx.Done():
		s.queuedBytes -= n
		h.webuiQueuedBytes -= n
		h.mu.Unlock()
	case s.frames <- f:
		h.mu.Unlock()
	default:
		s.queuedBytes -= n
		h.webuiQueuedBytes -= n
		h.mu.Unlock()
		s.stop(errors.New("web connection fell behind; reconnect to refresh the conversation"))
	}
	return nil
}

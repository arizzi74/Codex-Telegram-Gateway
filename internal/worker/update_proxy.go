package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const nativeMessageLimit = 32 << 20

// attachmentProxy fences individual RPCs, rather than the lifetime of a CLI.
// Keeping the backend reader alive after a frontend disconnect lets accepted
// requests settle without assuming that socket closure cancels server work.
type attachmentProxy struct {
	mu                sync.Mutex
	listener          *net.UnixListener
	server            *http.Server
	backend           string
	paused            bool
	resumed           chan struct{}
	closed            bool
	uncertain         bool
	uncertaintyReason string
	observationGap    string
	observationEpoch  uint64
	pausedEpoch       uint64
	queueThreads      map[string]struct{}
	deletedThreads    map[string]struct{}
	sessions          map[*attachmentSession]struct{}
	group             sync.WaitGroup
	done              chan struct{}
	closeOnce         sync.Once
}

type attachmentSession struct {
	proxy        *attachmentProxy
	ctx          context.Context
	cancel       context.CancelFunc
	frontRaw     net.Conn
	backRaw      net.Conn
	front        *websocket.Conn
	back         *websocket.Conn
	initializing bool
	frontGone    bool
	forwarding   int
	activity     nativeActivity
	stopOnce     sync.Once
}

type attachmentConnectionKey struct{}

func newAttachmentProxy(path, backend string) (*attachmentProxy, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	p := &attachmentProxy{listener: listener, backend: backend, sessions: make(map[*attachmentSession]struct{}), done: make(chan struct{})}
	p.server = &http.Server{
		Handler: http.HandlerFunc(p.accept), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10,
		ConnContext: func(ctx context.Context, connection net.Conn) context.Context {
			return context.WithValue(ctx, attachmentConnectionKey{}, connection)
		},
	}
	go func() { defer close(p.done); _ = p.server.Serve(listener) }()
	return p, nil
}

func (p *attachmentProxy) accept(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	if p.closed || p.paused {
		p.mu.Unlock()
		http.Error(w, "Worker update in progress; reconnect shortly.", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &attachmentSession{proxy: p, ctx: ctx, cancel: cancel, frontRaw: r.Context().Value(attachmentConnectionKey{}).(net.Conn), initializing: true}
	p.sessions[s] = struct{}{}
	p.observationEpoch++
	p.group.Add(1)
	p.mu.Unlock()
	defer p.group.Done()
	defer func() {
		s.stop()
		p.mu.Lock()
		// Losing the server before accepted work settles is an unknown outcome,
		// not evidence that an update may safely interrupt it.
		p.observationEpoch++
		for thread := range s.activity.queueThreads {
			if p.queueThreads == nil {
				p.queueThreads = make(map[string]struct{})
			}
			p.queueThreads[thread] = struct{}{}
		}
		// A lost queue read cannot create work, but it may have been the
		// terminal's first observation of already queued prompts.
		for _, request := range s.activity.requests {
			if request.method == "thread/queue/list" && request.thread != "" {
				if p.queueThreads == nil {
					p.queueThreads = make(map[string]struct{})
				}
				p.queueThreads[request.thread] = struct{}{}
			}
		}
		if s.activity.disconnectedError() != nil {
			p.uncertain = true
			if p.uncertaintyReason == "" {
				p.uncertaintyReason = s.activity.uncertaintyReason
				if p.uncertaintyReason == "" {
					p.uncertaintyReason = "backend disconnected before native work was confirmed complete"
				}
			}
		} else if p.observationGap == "" {
			if s.activity.observationGap != "" {
				p.observationGap = s.activity.observationGap
			} else if len(s.activity.requests) > 0 {
				p.observationGap = "native connection ended with only read-only requests outstanding"
			}
		}
		delete(p.sessions, s)
		p.mu.Unlock()
	}()

	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: false, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, "unix", p.backend)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed || s.ctx.Err() != nil {
			p.mu.Unlock()
			connection.Close()
			return nil, context.Canceled
		}
		s.backRaw = connection
		p.mu.Unlock()
		return connection, nil
	}}
	defer transport.CloseIdleConnections()
	var protocols []string
	for _, value := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if value = strings.TrimSpace(value); value != "" {
			protocols = append(protocols, value)
		}
	}
	headers := r.Header.Clone()
	for key := range headers {
		if strings.HasPrefix(strings.ToLower(key), "sec-websocket-") || strings.EqualFold(key, "Connection") || strings.EqualFold(key, "Upgrade") {
			headers.Del(key)
		}
	}
	handshake, stopHandshake := context.WithTimeout(ctx, 5*time.Second)
	back, _, err := websocket.Dial(handshake, "ws://codex.local"+r.URL.RequestURI(), &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport}, HTTPHeader: headers, Subprotocols: protocols,
		CompressionMode: websocket.CompressionDisabled,
	})
	stopHandshake()
	if err != nil {
		http.Error(w, "Codex runtime is unavailable; reconnect shortly.", http.StatusServiceUnavailable)
		return
	}
	defer back.CloseNow()
	selected := []string(nil)
	if back.Subprotocol() != "" {
		selected = []string{back.Subprotocol()}
	}
	front, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, Subprotocols: selected, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer front.CloseNow()
	front.SetReadLimit(nativeMessageLimit)
	back.SetReadLimit(nativeMessageLimit)
	p.mu.Lock()
	s.front, s.back, s.initializing = front, back, false
	p.observationEpoch++
	p.mu.Unlock()
	backendDone := make(chan struct{})
	go func() { defer close(backendDone); s.fromBackend() }()
	s.fromFrontend()
	<-backendDone
}

func (s *attachmentSession) stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		s.proxy.mu.Lock()
		back := s.backRaw
		s.proxy.mu.Unlock()
		// Close raw sockets before WebSocket cleanup so a blocked peer cannot
		// keep the worker's shutdown or update lease alive.
		_ = s.frontRaw.Close()
		if back != nil {
			_ = back.Close()
		}
	})
}

func (s *attachmentSession) fromFrontend() {
	p := s.proxy
	defer func() {
		p.mu.Lock()
		s.frontGone = true
		p.observationEpoch++
		settled := s.activity.idle() && s.forwarding == 0
		p.mu.Unlock()
		_ = s.frontRaw.Close()
		if settled {
			s.stop()
		}
	}()
	for {
		kind, payload, err := s.front.Read(s.ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageText {
			return
		}
		if !s.forwardClient(payload) {
			return
		}
	}
}

func (s *attachmentSession) forwardClient(payload []byte) bool {
	p := s.proxy
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return false
		}
		if p.paused {
			resumed := p.resumed
			p.mu.Unlock()
			if nativeResponseMessage(payload) {
				// A late server request invalidates preparation. Preserve the
				// CLI's answer until abort reopens admission rather than dropping
				// it or letting it execute behind a prepared restart lease.
				select {
				case <-resumed:
					continue
				case <-s.ctx.Done():
					return false
				}
			}
			return s.rejectDuringUpdate(payload)
		}
		s.forwarding++
		p.observationEpoch++
		s.activity.clientMessage(payload)
		p.mu.Unlock()
		err := s.back.Write(s.ctx, websocket.MessageText, payload)
		p.mu.Lock()
		s.forwarding--
		p.mu.Unlock()
		if err != nil {
			s.stop()
			return false
		}
		return true
	}
}

func nativeResponseMessage(payload []byte) bool {
	message, ok := nativeObject(payload)
	if !ok {
		return false
	}
	_, method := message["method"]
	_, result := message["result"]
	_, failure := message["error"]
	_, id := nativeRequestID(message["id"])
	return !method && id && result != failure
}

func (s *attachmentSession) rejectDuringUpdate(payload []byte) bool {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(payload, &request) != nil || request.Method == "" || len(request.ID) == 0 || string(request.ID) == "null" {
		return false
	}
	response, err := json.Marshal(map[string]any{"id": request.ID, "error": map[string]any{
		"code": -32002, "message": "Worker update in progress; retry after reconnecting.",
	}})
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(s.ctx, time.Second)
	defer cancel()
	return s.front.Write(ctx, websocket.MessageText, response) == nil
}

func (s *attachmentSession) fromBackend() {
	p := s.proxy
	defer s.stop()
	for {
		kind, payload, err := s.back.Read(s.ctx)
		if err != nil {
			p.mu.Lock()
			if s.ctx.Err() == nil && websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				// A settled connection loss is an observation gap, not evidence
				// of unfinished work. The final session cleanup separately keeps
				// unconfirmed accepted operations as a hard blocker.
				p.observationEpoch++
				if p.observationGap == "" {
					p.observationGap = "native backend connection ended unexpectedly"
				}
			}
			p.mu.Unlock()
			return
		}
		if kind != websocket.MessageText {
			p.mu.Lock()
			p.uncertain = true
			p.observationEpoch++
			if p.uncertaintyReason == "" {
				p.uncertaintyReason = "native backend sent a non-JSON-RPC frame"
			}
			p.mu.Unlock()
			return
		}
		p.mu.Lock()
		s.forwarding++
		p.observationEpoch++
		s.activity.serverMessage(payload)
		if s.activity.deletedThread != "" {
			p.forgetQueuedThreadLocked(s.activity.deletedThread)
		}
		frontGone := s.frontGone
		p.mu.Unlock()
		if !frontGone {
			err = s.front.Write(s.ctx, kind, payload)
		}
		p.mu.Lock()
		s.forwarding--
		if err != nil {
			s.frontGone = true
		}
		settled := s.frontGone && s.activity.idle() && s.forwarding == 0
		p.mu.Unlock()
		if settled {
			return
		}
	}
}

func (p *attachmentProxy) idleLocked() error {
	if err := p.settledLocked(); err != nil {
		return err
	}
	if p.observationGap != "" {
		return fmt.Errorf("worker update: native CLI activity needs fresh verification: %s", p.observationGap)
	}
	for session := range p.sessions {
		if session.activity.observationGap != "" {
			return fmt.Errorf("worker update: native CLI activity needs fresh verification: %s", session.activity.observationGap)
		}
	}
	return nil
}

// settledLocked permits only recoverable observation gaps. Accepted work with
// unknown outcomes cannot be cleared by a snapshot of currently idle threads.
func (p *attachmentProxy) settledLocked() error {
	if p.closed {
		return errors.New("worker update: attachment proxy is closed")
	}
	if p.uncertain {
		if p.uncertaintyReason != "" {
			return fmt.Errorf("worker update: native CLI activity could not be verified: %s", p.uncertaintyReason)
		}
		return errors.New("worker update: native CLI activity could not be verified")
	}
	for session := range p.sessions {
		if session.initializing {
			return errors.New("worker update: native CLI connection is still initializing")
		}
		if session.forwarding > 0 {
			return errors.New("worker update: native CLI requests are still in flight")
		}
		if err := session.activity.settledError(); err != nil {
			if session.activity.uncertain && session.activity.uncertaintyReason != "" {
				return fmt.Errorf("%w: %s", err, session.activity.uncertaintyReason)
			}
			return err
		}
	}
	return nil
}

func (p *attachmentProxy) pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.settledLocked(); err != nil {
		return err
	}
	if !p.paused {
		p.resumed = make(chan struct{})
		p.paused = true
		p.pausedEpoch = p.observationEpoch
	}
	return nil
}

// confirmIdle is called only after live runtime, actor, and durable state checks
// succeed behind the admission fence. A notification arriving during those
// checks invalidates that evidence; retry instead of erasing new activity.
func (p *attachmentProxy) confirmIdle() error {
	p.mu.Lock()
	if !p.paused || p.pausedEpoch != p.observationEpoch {
		p.mu.Unlock()
		return errors.New("worker update: native CLI activity changed during idle verification; retry when settled")
	}
	if err := p.settledLocked(); err != nil {
		p.mu.Unlock()
		return err
	}
	p.observationGap = ""
	p.queueThreads = nil
	var detached []*attachmentSession
	for session := range p.sessions {
		session.activity.observationGap = ""
		session.activity.queueThreads = nil
		if session.frontGone {
			detached = append(detached, session)
		}
	}
	p.mu.Unlock()
	for _, session := range detached {
		session.stop()
	}
	return nil
}

func (p *attachmentProxy) queuesToVerify() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	threads := make(map[string]struct{}, len(p.queueThreads))
	for thread := range p.queueThreads {
		threads[thread] = struct{}{}
	}
	for session := range p.sessions {
		for thread := range session.activity.queueThreads {
			threads[thread] = struct{}{}
		}
	}
	result := make([]string, 0, len(threads))
	for thread := range threads {
		if _, deleted := p.deletedThreads[thread]; !deleted {
			result = append(result, thread)
		}
	}
	return result
}

// Confirmed permanent deletion removes the queue, not accepted operations that
// still require their own completion. Thread IDs are not reused; remember this
// evidence until the proxy/runtime is replaced so a late queue-read reply cannot
// restore a verification obligation for a thread that no longer exists.
func (p *attachmentProxy) forgetQueuedThread(thread string) {
	if thread == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forgetQueuedThreadLocked(thread)
}

func (p *attachmentProxy) forgetQueuedThreadLocked(thread string) {
	if p.deletedThreads == nil {
		p.deletedThreads = make(map[string]struct{})
	}
	p.deletedThreads[thread] = struct{}{}
	delete(p.queueThreads, thread)
	for session := range p.sessions {
		delete(session.activity.queueThreads, thread)
	}
	p.observationEpoch++
}

func (p *attachmentProxy) checkIdle() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idleLocked()
}

func (p *attachmentProxy) resume() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused {
		p.paused = false
		close(p.resumed)
		p.resumed = nil
	}
}

func (p *attachmentProxy) close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		sessions := make([]*attachmentSession, 0, len(p.sessions))
		for session := range p.sessions {
			sessions = append(sessions, session)
		}
		p.mu.Unlock()
		for _, session := range sessions {
			session.stop()
		}
		_ = p.server.Close()
		_ = p.listener.Close()
		<-p.done
		p.group.Wait()
	})
}

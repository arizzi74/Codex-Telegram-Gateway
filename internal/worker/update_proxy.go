package worker

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// attachmentProxy is the only published native CLI endpoint. The app-server's
// underlying socket remains private to the worker. Tracking entire connections
// makes an attached terminal a conservative update blocker, even while idle.
type attachmentProxy struct {
	mu         sync.Mutex
	listener   *net.UnixListener
	backend    string
	paused     bool
	closed     bool
	nativeUsed bool
	clients    map[net.Conn]struct{}
	group      sync.WaitGroup
	done       chan struct{}
}

func newAttachmentProxy(path, backend string) (*attachmentProxy, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	p := &attachmentProxy{listener: listener, backend: backend, clients: map[net.Conn]struct{}{}, done: make(chan struct{})}
	go p.serve()
	return p, nil
}
func (p *attachmentProxy) serve() {
	defer close(p.done)
	for {
		client, err := p.listener.AcceptUnix()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.paused || p.closed {
			p.mu.Unlock()
			client.Close()
			continue
		}
		p.clients[client] = struct{}{}
		// Socket closure does not prove that every submitted native RPC has
		// finished inside Codex. Keep a lifetime latch instead of guessing a
		// quiet period after disconnect; an operator can stop the worker once
		// native work finishes and then install the staged update.
		p.nativeUsed = true
		p.group.Add(1)
		p.mu.Unlock()
		go p.forward(client)
	}
}
func (p *attachmentProxy) forward(client *net.UnixConn) {
	defer p.group.Done()
	defer func() { client.Close(); p.mu.Lock(); delete(p.clients, client); p.mu.Unlock() }()
	// Bound the only operation before both ends are registered for shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", p.backend)
	if err != nil {
		return
	}
	backend := connection.(*net.UnixConn)
	defer backend.Close()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.clients[backend] = struct{}{}
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.clients, backend); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(backend, client); _ = backend.CloseWrite(); close(done) }()
	_, _ = io.Copy(client, backend)
	_ = client.CloseWrite()
	// An errored/disconnected peer must not strand the opposite copier.
	_ = client.CloseRead()
	_ = backend.CloseWrite()
	<-done
}
func (p *attachmentProxy) pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("worker update: attachment proxy is closed")
	}
	if len(p.clients) > 0 {
		return errors.New("worker update: a local CLI is attached")
	}
	if p.nativeUsed {
		return errors.New("worker update: this runtime was used by a native CLI; finish native work and stop the worker service before updating")
	}
	p.paused = true
	return nil
}
func (p *attachmentProxy) resume() { p.mu.Lock(); p.paused = false; p.mu.Unlock() }
func (p *attachmentProxy) close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		_ = p.listener.Close()
		for connection := range p.clients {
			_ = connection.Close()
		}
	}
	p.mu.Unlock()
	<-p.done
	p.group.Wait()
}

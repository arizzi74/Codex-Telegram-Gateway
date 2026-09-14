package codexadapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// dialProxyWebSocket performs the app-server WebSocket upgrade through the
// CLI's byte proxy. The proxy is deliberately used as a net.Conn: it does not
// convert JSONL into WebSocket frames itself.
func dialProxyWebSocket(ctx context.Context, in io.WriteCloser, out io.ReadCloser) (*websocket.Conn, error) {
	connection := &stdioConn{reader: out, writer: in}
	var used atomic.Bool
	transport := &http.Transport{
		Proxy:             nil,
		ForceAttemptHTTP2: false,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			if !used.CompareAndSwap(false, true) {
				return nil, errors.New("app-server proxy accepted more than one dial")
			}
			return connection, nil
		},
	}
	client := &http.Client{Transport: transport}
	ws, _, err := websocket.Dial(ctx, "ws://codex.local/", &websocket.DialOptions{
		HTTPClient:      client,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("WebSocket upgrade through app-server proxy: %w", err)
	}
	ws.SetReadLimit(maxJSONRPCMessageBytes)
	return ws, nil
}

// stdioConn supplies an HTTP/WebSocket client with a stream backed by the
// proxy's stdin/stdout. Deadlines are intentionally no-ops: cancellation is
// implemented by closing the process pipes.
type stdioConn struct {
	reader io.ReadCloser
	writer io.WriteCloser
	close  sync.Once
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.reader.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.writer.Write(p) }
func (c *stdioConn) Close() error {
	var err error
	c.close.Do(func() {
		if closeErr := c.reader.Close(); closeErr != nil {
			err = closeErr
		}
		if closeErr := c.writer.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	})
	return err
}
func (c *stdioConn) LocalAddr() net.Addr              { return proxyAddr("stdio-local") }
func (c *stdioConn) RemoteAddr() net.Addr             { return proxyAddr("stdio-remote") }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

type proxyAddr string

func (a proxyAddr) Network() string { return "stdio" }
func (a proxyAddr) String() string  { return string(a) }

// jsonlWSBridge preserves Client's JSONL transport invariant while sending one
// JSON-RPC object per WebSocket text frame to the Unix app-server listener.
type jsonlWSBridge struct {
	inReader  *io.PipeReader
	inWriter  *io.PipeWriter
	outReader *io.PipeReader
	outWriter *io.PipeWriter
	ws        *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan error
	finish    sync.Once
}

func newJSONLWSBridge(ws *websocket.Conn) *jsonlWSBridge {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &jsonlWSBridge{inReader: inReader, inWriter: inWriter, outReader: outReader, outWriter: outWriter, ws: ws, ctx: ctx, cancel: cancel, done: make(chan error, 1)}
	go bridge.writeLoop()
	go bridge.readLoop()
	return bridge
}

func (b *jsonlWSBridge) transport(close func() error) Transport {
	return Transport{In: b.inWriter, Out: b.outReader, Wait: b.done, Close: close}
}

func (b *jsonlWSBridge) writeLoop() {
	scanner := bufio.NewScanner(b.inReader)
	scanner.Buffer(make([]byte, 64*1024), maxJSONRPCMessageBytes)
	for scanner.Scan() {
		payload := append([]byte(nil), scanner.Bytes()...)
		if err := b.ws.Write(b.ctx, websocket.MessageText, payload); err != nil {
			b.closeWith(fmt.Errorf("write app-server WebSocket frame: %w", err))
			return
		}
	}
	if err := scanner.Err(); err != nil {
		b.closeWith(fmt.Errorf("read JSONL bridge input: %w", err))
	} else {
		b.closeWith(ErrClosed)
	}
}

func (b *jsonlWSBridge) readLoop() {
	for {
		kind, payload, err := b.ws.Read(b.ctx)
		if err != nil {
			b.closeWith(fmt.Errorf("read app-server WebSocket frame: %w", err))
			return
		}
		if kind != websocket.MessageText {
			b.closeWith(fmt.Errorf("unexpected app-server WebSocket message type %d", kind))
			return
		}
		payload = append(payload, '\n')
		if _, err := b.outWriter.Write(payload); err != nil {
			b.closeWith(fmt.Errorf("write JSONL bridge output: %w", err))
			return
		}
	}
}

func (b *jsonlWSBridge) closeWith(err error) {
	if err == nil {
		err = ErrClosed
	}
	b.finish.Do(func() {
		b.cancel()
		_ = b.ws.CloseNow()
		_ = b.inReader.Close()
		_ = b.inWriter.Close()
		_ = b.outReader.Close()
		_ = b.outWriter.Close()
		b.done <- err
	})
}

func (b *jsonlWSBridge) Close() error {
	b.closeWith(ErrClosed)
	return nil
}

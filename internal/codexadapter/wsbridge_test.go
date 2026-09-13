package codexadapter

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type pipeReadCloser struct{ net.Conn }

func (c pipeReadCloser) Read(p []byte) (int, error) { return c.Conn.Read(p) }

type pipeWriteCloser struct{ net.Conn }

func (c pipeWriteCloser) Write(p []byte) (int, error) { return c.Conn.Write(p) }

type pipeResponseWriter struct {
	header http.Header
	conn   net.Conn
	rw     *bufio.ReadWriter
}

func (w *pipeResponseWriter) Header() http.Header         { return w.header }
func (w *pipeResponseWriter) Write(p []byte) (int, error) { return w.rw.Write(p) }
func (w *pipeResponseWriter) WriteHeader(status int) {
	_, _ = fmt.Fprintf(w.rw, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	for key, values := range w.header {
		for _, value := range values {
			_, _ = fmt.Fprintf(w.rw, "%s: %s\r\n", key, value)
		}
	}
	_, _ = w.rw.WriteString("\r\n")
	_ = w.rw.Flush()
}
func (w *pipeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) { return w.conn, w.rw, nil }

func TestJSONLWebSocketBridgeThroughProxyStream(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		rw := bufio.NewReadWriter(bufio.NewReader(serverConn), bufio.NewWriter(serverConn))
		request, err := http.ReadRequest(rw.Reader)
		if err != nil {
			serverDone <- err
			return
		}
		response := &pipeResponseWriter{header: make(http.Header), conn: serverConn, rw: rw}
		ws, err := websocket.Accept(response, request, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			serverDone <- err
			return
		}
		defer ws.CloseNow()
		kind, payload, err := ws.Read(context.Background())
		if err != nil {
			serverDone <- err
			return
		}
		if kind != websocket.MessageText || string(payload) != `{"method":"initialize"}` {
			serverDone <- io.ErrUnexpectedEOF
			return
		}
		serverDone <- ws.Write(context.Background(), websocket.MessageText, []byte(`{"id":1,"result":{}}`))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ws, err := dialProxyWebSocket(ctx, pipeWriteCloser{clientConn}, pipeReadCloser{clientConn})
	if err != nil {
		t.Fatalf("dial through proxy stream: %v", err)
	}
	bridge := newJSONLWSBridge(ws)
	defer bridge.Close()
	transport := bridge.transport(bridge.Close)
	if _, err := transport.In.Write([]byte("{\"method\":\"initialize\"}\n")); err != nil {
		t.Fatalf("write bridge JSONL: %v", err)
	}
	line, err := bufio.NewReader(transport.Out).ReadString('\n')
	if err != nil {
		t.Fatalf("read bridge JSONL: %v", err)
	}
	if line != "{\"id\":1,\"result\":{}}\n" {
		t.Fatalf("bridge line = %q", line)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("fake websocket server: %v", err)
	}
}

package codexadapter

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/coder/websocket"
)

// TranscriptSource is a disposable read-only attachment to an already-owned
// native runtime. It owns only its connection: Close never kills a process,
// deletes a socket, resumes a thread, or changes the durable observer.
type TranscriptSource struct{ client *Client }

func OpenTranscriptSource(ctx context.Context, socket string) (*TranscriptSource, error) {
	if !filepath.IsAbs(socket) {
		return nil, errors.New("transcript source requires the protected local runtime socket")
	}
	transport := &http.Transport{Proxy: nil, ForceAttemptHTTP2: false, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	ws, _, err := websocket.Dial(ctx, "ws://codex.local/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxJSONRPCMessageBytes)
	bridge := newJSONLWSBridge(ws)
	client := New(bridge.transport(bridge.Close), Config{ClientInfo: ClientInfo{Name: "telegramgw_transcript"}, RequestTimeout: 15 * time.Second})
	if err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return &TranscriptSource{client: client}, nil
}

func (s *TranscriptSource) ReadPage(ctx context.Context, thread, cursor, direction string) (TranscriptTurnPage, error) {
	return s.client.ReadTranscriptTurnPage(ctx, thread, cursor, direction, true)
}

func (s *TranscriptSource) Close() error { return s.client.Close() }

// Package codextest provides an in-memory JSONL app-server fixture for worker
// tests. It deliberately speaks the adapter's wire protocol, not worker APIs.
package codextest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

type Call struct {
	Method string
	Params json.RawMessage
}
type Response struct {
	ID     json.RawMessage
	Result json.RawMessage
	Error  json.RawMessage
}
type Server struct {
	in          *bufio.Scanner
	out         *json.Encoder
	raw         io.Writer
	writeMu     sync.Mutex
	close       func()
	mu          sync.Mutex
	calls       []Call
	responses   []Response
	next        int
	threads     []map[string]any
	loaded      []string
	unavailable map[string]bool
	rpcErrors   map[string]rpcError
	delays      map[string]time.Duration
}

type rpcError struct {
	code    int
	message string
}

// SetThreads controls the fixture's reconciliation response. Thread values
// use the app-server wire field names (id, cwd, name, preview, status).
func (s *Server) SetThreads(threads []map[string]any, loaded []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads = append([]map[string]any(nil), threads...)
	s.loaded = append([]string(nil), loaded...)
}

// SetMethodUnavailable makes one RPC method return JSON-RPC -32601.
func (s *Server) SetMethodUnavailable(method string, unavailable bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unavailable == nil {
		s.unavailable = map[string]bool{}
	}
	s.unavailable[method] = unavailable
}

// SetRPCError makes method return a JSON-RPC failure. It is useful for tests
// that need to preserve the app-server's failure boundary rather than mock a
// worker-domain error.
func (s *Server) SetRPCError(method string, code int, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rpcErrors == nil {
		s.rpcErrors = map[string]rpcError{}
	}
	if message == "" {
		delete(s.rpcErrors, method)
		return
	}
	s.rpcErrors[method] = rpcError{code: code, message: message}
}

// SetResponseDelay delays replies for method. It lets tests exercise caller
// deadlines while keeping the fixture on the real JSONL transport.
func (s *Server) SetResponseDelay(method string, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delays == nil {
		s.delays = map[string]time.Duration{}
	}
	s.delays[method] = delay
}

func New(ctx context.Context) (*codexadapter.Client, *Server, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &Server{in: bufio.NewScanner(inR), out: json.NewEncoder(outW), raw: outW, unavailable: map[string]bool{}, rpcErrors: map[string]rpcError{}, delays: map[string]time.Duration{}, close: func() { _ = inR.Close(); _ = inW.Close(); _ = outR.Close(); _ = outW.Close() }}
	go s.serve()
	c := codexadapter.New(codexadapter.Transport{In: inW, Out: outR, Close: func() error { s.close(); return nil }}, codexadapter.Config{})
	if err := c.Initialize(ctx); err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	return c, s, nil
}
func (s *Server) Close() { s.close() }
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}
func (s *Server) Responses() []Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Response(nil), s.responses...)
}
func (s *Server) Emit(method string, params any) error {
	return s.write(map[string]any{"method": method, "params": params})
}
func (s *Server) Request(method string, id any, params any) error {
	return s.write(map[string]any{"method": method, "id": id, "params": params})
}

// EmitRaw writes one raw JSONL record verbatim. Tests use it to make the
// adapter observe malformed input while retaining the same fixture process.
func (s *Server) EmitRaw(record string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := io.WriteString(s.raw, record); err != nil {
		return err
	}
	if len(record) == 0 || record[len(record)-1] != '\n' {
		_, err := io.WriteString(s.raw, "\n")
		return err
	}
	return nil
}

func (s *Server) write(value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.out.Encode(value)
}

func (s *Server) serve() {
	for s.in.Scan() {
		var request struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(s.in.Bytes(), &request) != nil {
			continue
		}
		if request.Method == "" {
			s.mu.Lock()
			s.responses = append(s.responses, Response{append(json.RawMessage(nil), request.ID...), append(json.RawMessage(nil), request.Result...), nil})
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		s.calls = append(s.calls, Call{request.Method, append(json.RawMessage(nil), request.Params...)})
		s.next++
		n := s.next
		unavailable := s.unavailable[request.Method]
		rpcFailure, hasRPCFailure := s.rpcErrors[request.Method]
		delay := s.delays[request.Method]
		s.mu.Unlock()
		// JSON-RPC notifications such as initialized have no response ID.
		if len(request.ID) == 0 {
			continue
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		if unavailable {
			_ = s.write(map[string]any{"id": json.RawMessage(request.ID), "error": map[string]any{"code": -32601, "message": "method not found"}})
			continue
		}
		if hasRPCFailure {
			_ = s.write(map[string]any{"id": json.RawMessage(request.ID), "error": map[string]any{"code": rpcFailure.code, "message": rpcFailure.message}})
			continue
		}
		var result any = map[string]any{}
		switch request.Method {
		case "thread/start":
			result = map[string]any{"thread": map[string]any{"id": "thread-new"}}
		case "thread/resume":
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(request.Params, &p)
			result = map[string]any{"thread": s.thread(p.ThreadID)}
		case "thread/read":
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(request.Params, &p)
			result = map[string]any{"thread": s.thread(p.ThreadID)}
		case "turn/start":
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(request.Params, &p)
			result = map[string]any{"turn": map[string]any{"id": "turn-" + string(rune('0'+n)), "threadId": p.ThreadID}}
		case "review/start":
			var p struct {
				ThreadID string `json:"threadId"`
			}
			_ = json.Unmarshal(request.Params, &p)
			result = map[string]any{"turn": map[string]any{"id": "turn-review-" + string(rune('0'+n)), "threadId": p.ThreadID}}
		case "turn/steer":
			result = map[string]any{"turnId": "turn-steered"}
		case "thread/list":
			s.mu.Lock()
			threads := append([]map[string]any(nil), s.threads...)
			s.mu.Unlock()
			result = map[string]any{"data": threads}
		case "thread/loaded/list":
			s.mu.Lock()
			loaded := append([]string(nil), s.loaded...)
			s.mu.Unlock()
			result = map[string]any{"data": loaded}
		}
		_ = s.write(map[string]any{"id": json.RawMessage(request.ID), "result": result})
	}
}

func (s *Server) thread(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, thread := range s.threads {
		if thread["id"] == id {
			copy := make(map[string]any, len(thread))
			for k, v := range thread {
				copy[k] = v
			}
			return copy
		}
	}
	return map[string]any{"id": id}
}

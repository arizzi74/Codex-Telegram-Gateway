package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func (s *Server) webuiRoutes() {
	s.webuiPushRoutes()
	s.webuiPWARoutes()
	s.webuiDraftRoutes()
	for _, asset := range []struct{ name, contentType string }{
		{"session-auth.js", "application/javascript; charset=utf-8"},
		{"session-auth.css", "text/css; charset=utf-8"},
	} {
		s.mux.HandleFunc("/tgw/admin/static/"+asset.name, func(w http.ResponseWriter, r *http.Request) {
			serveAsset(w, r, "static/"+asset.name, asset.contentType)
		})
	}
	s.mux.HandleFunc("/tgw/webui/", s.webuiPage)
	for _, asset := range []struct{ name, contentType string }{
		{"webui.css", "text/css; charset=utf-8"},
		{"webui.js", "application/javascript; charset=utf-8"},
		{"webui-format.js", "application/javascript; charset=utf-8"},
		{"webui-commands.js", "application/javascript; charset=utf-8"},
		{"webui-command-ui.js", "application/javascript; charset=utf-8"},
		{"webui-drafts.js", "application/javascript; charset=utf-8"},
	} {
		s.mux.HandleFunc("/tgw/webui/static/"+asset.name, func(w http.ResponseWriter, r *http.Request) {
			serveAsset(w, r, "static/"+asset.name, asset.contentType)
		})
	}
	s.mux.HandleFunc("/tgw/api/v1/webui/sessions", s.webuiSessions)
	s.mux.HandleFunc("/tgw/api/v1/webui/sessions/delete", s.webuiSessionDelete)
	s.mux.HandleFunc("/tgw/api/v1/webui/connect", s.webuiConnect)
	s.mux.HandleFunc("/tgw/api/v1/webui/commands", s.webuiCommands)
	s.mux.HandleFunc("/tgw/api/v1/webui/activity", s.webuiActivity)
}

func (s *Server) webuiPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/tgw/webui/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	// The shell contains no private data. API and streaming endpoints require
	// the same passkey session as the admin console.
	s.setCSRF(w)
	page, err := versionedWebUIPage()
	if err != nil {
		fail(w, err)
		return
	}
	// Local attachment previews use revocable blob URLs; keep remote and data
	// images disallowed, and leave the admin console policy unchanged.
	w.Header().Set("Content-Security-Policy", w.Header().Get("Content-Security-Policy")+"; img-src 'self' blob:; worker-src 'self'; manifest-src 'self'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (s *Server) webuiSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	d, err := s.store.AdminDashboardSnapshot(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	s.redactDashboard(&d)
	writeJSON(w, http.StatusOK, map[string]any{"sessions": d.Sessions, "workers": d.Workers})
}

func (s *Server) webuiConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	// WebSocket upgrades are not protected by browser CORS. Require the exact
	// configured HTTPS Origin, in addition to secure SameSite passkey cookies.
	if !s.sameOrigin(w, r) {
		return
	}
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	if s.webui == nil {
		http.Error(w, "web interface is unavailable", http.StatusServiceUnavailable)
		return
	}
	id, err := uuid.Parse(r.URL.Query().Get("session_id"))
	if err != nil {
		bad(w)
		return
	}
	d, err := s.store.AdminDashboardSnapshot(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	var selected *registry.AdminSession
	for i := range d.Sessions {
		if d.Sessions[i].ID == id.String() {
			selected = &d.Sessions[i]
			break
		}
	}
	if selected == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(protocol.MaxWebUIInputBytes)
	stream, err := s.webui.OpenWebUI(ctx, *selected)
	if err != nil {
		// WebSocket clients cannot read failed upgrade response bodies. Deliver
		// actionable worker/update errors after an authenticated upgrade.
		body, _ := json.Marshal(map[string]string{"type": "error", "message": err.Error()})
		writeCtx, done := context.WithTimeout(ctx, 5*time.Second)
		_ = conn.Write(writeCtx, websocket.MessageText, body)
		done()
		status := websocket.StatusTryAgainLater
		if errors.Is(err, gateway.ErrWebUILimit) || errors.Is(err, gateway.ErrWebUIUnsupported) {
			status = websocket.StatusPolicyViolation
		}
		_ = conn.Close(status, "Worker unavailable")
		return
	}
	defer stream.Close()
	s.redactDashboard(&d)
	authCookie, _ := r.Cookie(adminCookie)
	// Only the reader goroutine reads, and only this handler writes. A slow
	// browser cannot block the worker's event or Telegram delivery streams.
	// Backpressure prevents several maximum-size pasted images being buffered
	// per browser while a worker send or authentication check is pending.
	inputs := make(chan []byte)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			kind, body, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if kind != websocket.MessageText {
				_ = conn.Close(websocket.StatusUnsupportedData, "JSON text required")
				return
			}
			select {
			case inputs <- body:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); conn.CloseNow(); <-readDone }()
	write := func(v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return false
		}
		writeCtx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		return conn.Write(writeCtx, websocket.MessageText, data) == nil
	}
	failStream := func(message string, code websocket.StatusCode) {
		write(map[string]any{"type": "error", "message": message})
		_ = conn.Close(code, message)
	}
	authenticated := func() bool {
		checkCtx, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		_, err := s.store.ValidateAdminSession(checkCtx, authCookie.Value)
		return err == nil
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	readyTimer := time.NewTimer(30 * time.Second)
	defer readyTimer.Stop()
	ready := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-readDone:
			return
		case <-stream.Done():
			failStream("Worker connection lost. Reconnect to refresh; prompts are never resent.", websocket.StatusTryAgainLater)
			return
		case <-readyTimer.C:
			if !ready {
				failStream("Worker took too long to connect. Please try again.", websocket.StatusTryAgainLater)
				return
			}
		case <-ticker.C:
			if !authenticated() {
				failStream("Your session expired. Sign in again.", websocket.StatusCode(4001))
				return
			}
			checkCtx, done := context.WithTimeout(ctx, 5*time.Second)
			err := stream.Check(checkCtx)
			done()
			if err != nil {
				failStream("Worker disconnected. Reconnect when it is online.", websocket.StatusTryAgainLater)
				return
			}
			if !write(map[string]string{"type": "heartbeat"}) {
				return
			}
		case body := <-inputs:
			if !ready {
				failStream("Wait for the session to connect.", websocket.StatusPolicyViolation)
				return
			}
			if !authenticated() {
				failStream("Your session expired. Sign in again.", websocket.StatusCode(4001))
				return
			}
			sendCtx, done := context.WithTimeout(ctx, 10*time.Second)
			err := stream.Send(sendCtx, body)
			done()
			if err != nil {
				failStream("Request could not be delivered. Check the conversation before retrying.", websocket.StatusTryAgainLater)
				return
			}
		case frame := <-stream.Frames():
			ok := func() bool {
				// Include the in-flight write in the byte budget: a slow socket
				// must not hold a large page outside the global queue limit.
				defer stream.Consumed(frame)
				switch frame.Action {
				case "ready":
					ready = true
					readyTimer.Stop()
					return write(map[string]any{"type": "ready", "session": selected})
				case "output":
					data, err := s.redactWebUI(frame.Data)
					if err != nil {
						failStream("Invalid worker response.", websocket.StatusInternalError)
						return false
					}
					return write(data)
				case "error":
					message := frame.Error
					if s.redactor != nil {
						message = s.redactor.Redact(message)
					}
					if message == "" {
						message = "Session connection failed. Please reconnect."
					}
					// WebSocket close reasons are limited; the JSON frame carries detail.
					write(map[string]any{"type": "error", "message": message})
					_ = conn.Close(websocket.StatusTryAgainLater, "Session connection failed")
					return false
				}
				return true
			}()
			if !ok {
				return
			}
		}
	}
}

// Redact strings without altering JSON numbers (JSON-RPC IDs can be large).
func (s *Server) redactWebUI(raw json.RawMessage) (json.RawMessage, error) {
	if s.redactor == nil {
		return raw, nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			return s.redactor.Redact(x)
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		case map[string]any:
			for key, item := range x {
				x[key] = walk(item)
			}
		}
		return v
	}
	return json.Marshal(walk(value))
}

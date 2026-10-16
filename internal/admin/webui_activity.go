package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/iaia/telegramgw/internal/registry"
)

const webUIActivityAuthInterval = 15 * time.Second

// webuiActivity streams gateway state for every visible session, independently
// of the selected thread connection. It subscribes to committed registry
// changes, never to another worker runtime and never polls saved conversations.
func (s *Server) webuiActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		method(w)
		return
	}
	if !s.sameOrigin(w, r) {
		return
	}
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}
	version := r.URL.Query().Get("v")
	if version != "" && version != "1" && version != "2" && version != "3" {
		http.Error(w, "Unsupported activity protocol version.", http.StatusBadRequest)
		return
	}
	legacy := version == ""
	wireVersion := 1
	if version == "2" {
		wireVersion = 2
	} else if version == "3" {
		wireVersion = 3
	}
	changes, unsubscribe, err := s.store.SubscribeSessionActivity()
	if err != nil {
		http.Error(w, "Too many activity viewers. Close another tab and retry.", http.StatusTooManyRequests)
		return
	}
	defer unsubscribe()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(256)
	ctx := conn.CloseRead(r.Context())
	credential, _ := r.Cookie(adminCookie)
	write := func(value any) bool {
		raw, err := json.Marshal(value)
		if err != nil {
			return false
		}
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return conn.Write(writeCtx, websocket.MessageText, raw) == nil
	}
	authenticated := func() bool {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := s.store.ValidateAdminSession(checkCtx, credential.Value); err != nil {
			write(map[string]string{"type": "error", "message": "Your session expired. Sign in again."})
			_ = conn.Close(websocket.StatusCode(4001), "Sign in required")
			return false
		}
		return true
	}
	sequence, revision := uint64(0), uint64(0)
	inventoryRevision := ""
	telegramBotsRevision := ""
	readTelegramBots := func() (registry.WebUITelegramBotSnapshot, error) {
		readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		return s.store.WebUITelegramBots(readCtx)
	}
	previous := make(map[string]registry.SessionActivity)
	snapshot := func(activity registry.SessionActivitySnapshot) bool {
		if !authenticated() {
			return false
		}
		raw, err := json.Marshal(activity.Sessions)
		if err != nil {
			return false
		}
		raw, err = s.redactWebUI(raw)
		if err != nil {
			return false
		}
		previous = make(map[string]registry.SessionActivity, len(activity.Sessions))
		for _, session := range activity.Sessions {
			previous[session.SessionID] = session
		}
		revision = activity.Revision
		inventoryRevision = activity.InventoryRevision
		if legacy {
			return write(map[string]any{"type": "activity", "revision": revision, "sessions": json.RawMessage(raw)})
		}
		frame := map[string]any{"type": "activity_snapshot", "version": wireVersion, "sequence": sequence, "revision": revision,
			"inventory_revision": inventoryRevision, "sessions": json.RawMessage(raw)}
		if wireVersion >= 3 {
			bots, err := readTelegramBots()
			if err != nil {
				return false
			}
			telegramBotsRevision = bots.Revision
			frame["telegram_bots"], frame["telegram_bots_revision"] = bots.Bots, bots.Revision
			if !authenticated() {
				return false
			}
		}
		return write(frame)
	}
	if !authenticated() {
		return
	}
	readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	initial, err := s.store.LiveSessionActivity(readCtx)
	stop()
	if err != nil || !snapshot(initial) {
		return
	}
	emit := func(event registry.SessionActivityEvent) bool {
		// Check after the registry read and before each retained edge: revocation
		// may happen while a database query or a slow client's batch is waiting.
		if !authenticated() {
			return false
		}
		raw, err := json.Marshal(event.Session)
		if err != nil {
			return false
		}
		raw, err = s.redactWebUI(raw)
		if err != nil {
			return false
		}
		sequence++
		if event.Session == nil {
			delete(previous, event.SessionID)
		} else {
			previous[event.SessionID] = *event.Session
		}
		name := event.Event
		if wireVersion == 1 && name == "session_settings_changed" {
			name = "session_changed"
		}
		return write(map[string]any{"type": "activity_event", "version": wireVersion, "sequence": sequence, "revision": event.Revision,
			"event": name, "session_id": event.SessionID, "session": json.RawMessage(raw)})
	}
	telegramBotUpdates := func() bool {
		if wireVersion < 3 {
			return true
		}
		bots, err := readTelegramBots()
		if err != nil {
			return false
		}
		if bots.Revision == telegramBotsRevision {
			return true
		}
		// Bot routing is global metadata, so this event intentionally has no
		// session/session_id. Use the same sequence as session activity edges.
		if !authenticated() {
			return false
		}
		sequence++
		telegramBotsRevision = bots.Revision
		return write(map[string]any{"type": "activity_event", "version": wireVersion, "sequence": sequence, "revision": revision,
			"event": "telegram_bots_changed", "telegram_bots": bots.Bots, "telegram_bots_revision": bots.Revision})
	}
	updates := func() bool {
		if !authenticated() {
			return false
		}
		readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		update, err := s.store.SessionActivitySince(readCtx, revision)
		stop()
		if err != nil {
			return false
		}
		if legacy {
			return snapshot(update.Snapshot)
		}
		if update.Reset {
			sequence++
			return snapshot(update.Snapshot)
		}
		// Preserve committed edges such as rapid start/end or question/answer.
		// Afterwards reconcile the latest rows for generic invalidations (a
		// Telegram answer, heartbeat or worker disconnect) without history reads.
		for _, event := range update.Events {
			if !emit(event) {
				return false
			}
		}
		current := make(map[string]bool, len(update.Snapshot.Sessions))
		for _, session := range update.Snapshot.Sessions {
			current[session.SessionID] = true
			before, exists := previous[session.SessionID]
			if exists && before == session {
				continue
			}
			var old *registry.SessionActivity
			if exists {
				old = &before
			}
			if !emit(registry.SessionActivityEvent{Revision: update.Snapshot.Revision, Event: registry.SessionActivityEventName(old, &session), SessionID: session.SessionID, Session: &session}) {
				return false
			}
		}
		for id := range previous {
			if !current[id] && !emit(registry.SessionActivityEvent{Revision: update.Snapshot.Revision, Event: "session_removed", SessionID: id}) {
				return false
			}
		}
		// A worker can enroll, disconnect or acquire its first runtime without
		// having any sessions. Send an additive snapshot that old tabs already
		// understand, with an opaque inventory revision for current browsers.
		if wireVersion >= 2 && inventoryRevision != update.Snapshot.InventoryRevision {
			sequence++
			return snapshot(update.Snapshot)
		}
		revision = update.Snapshot.Revision
		return telegramBotUpdates()
	}
	ticker := time.NewTicker(webUIActivityAuthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				return
			}
			if !updates() {
				return
			}
		case <-ticker.C:
			if !authenticated() {
				return
			}
			// Also refresh metadata at heartbeat boundaries: another gateway
			// process may have changed routing without this process's wakeup.
			if wireVersion >= 3 && (!telegramBotUpdates() || !authenticated()) {
				return
			}
			if legacy {
				if !write(map[string]string{"type": "heartbeat"}) {
					return
				}
			} else if !write(map[string]any{"type": "heartbeat", "version": wireVersion, "sequence": sequence}) {
				return
			}
		}
	}
}

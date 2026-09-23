package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

const (
	webUIHistoryTTL       = 10 * time.Minute
	webUIHistoryBytes     = 64 << 20
	webUIHistorySnapshots = 32
	webUIHistoryCursors   = 512
	webUIHistoryItemBytes = 128 << 10
	webUIHistoryPageBytes = 3 << 20
	webUIHistoryTraversal = 32
)

// The interface makes the source lifetime explicit. Production attaches a
// disposable socket client; no full legacy turn is read on the observer.
type webUIHistorySource interface {
	ReadPage(context.Context, string, string, string) (codexadapter.TranscriptTurnPage, error)
	Close() error
}

type webUIHistoryRequest struct {
	Cursor, Direction string
	Limit             int
}
type webUIHistoryEntry struct {
	TurnID          string          `json:"turnId"`
	Item            json.RawMessage `json:"item"`
	TurnStartedAt   *int64          `json:"turnStartedAt,omitempty"`
	TurnCompletedAt *int64          `json:"turnCompletedAt,omitempty"`
	TurnStatus      string          `json:"turnStatus,omitempty"`
}
type webUIHistoryPage struct {
	Data       []webUIHistoryEntry `json:"data"`
	NextCursor string              `json:"nextCursor,omitempty"`
}
type webUIHistoryScope struct {
	Runtime, Thread string
	Generation      uint64
}
type webUIHistorySnapshot struct {
	id            string
	scope         webUIHistoryScope
	turn          codexadapter.TranscriptTurn
	bytes         int
	created, used time.Time
}
type webUIHistoryPosition struct {
	scope        webUIHistoryScope
	direction    string
	snapshot     string
	index        int
	nativeCursor string
	done         bool
	created      time.Time
}
type webUIHistoryCache struct {
	mu        sync.Mutex
	snapshots map[string]*webUIHistorySnapshot
	cursors   map[string]webUIHistoryPosition
	bytes     int
}

func (c *webUIHistoryCache) prune(now time.Time) {
	if c.snapshots == nil {
		c.snapshots = make(map[string]*webUIHistorySnapshot)
		c.cursors = make(map[string]webUIHistoryPosition)
	}
	for id, snapshot := range c.snapshots {
		if now.Sub(snapshot.created) >= webUIHistoryTTL {
			c.bytes -= snapshot.bytes
			delete(c.snapshots, id)
		}
	}
	for id, cursor := range c.cursors {
		if now.Sub(cursor.created) >= webUIHistoryTTL || (cursor.snapshot != "" && c.snapshots[cursor.snapshot] == nil) {
			delete(c.cursors, id)
		}
	}
}
func (c *webUIHistoryCache) position(scope webUIHistoryScope, request webUIHistoryRequest) (webUIHistoryPosition, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(time.Now())
	if request.Cursor == "" {
		return webUIHistoryPosition{scope: scope, direction: request.Direction}, nil
	}
	position, ok := c.cursors[request.Cursor]
	if !ok || position.scope != scope || position.direction != request.Direction {
		return webUIHistoryPosition{}, validationError("Conversation page expired or belongs to another session. Refresh the conversation.")
	}
	return position, nil
}
func (c *webUIHistoryCache) continuation(position webUIHistoryPosition) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(time.Now())
	if len(c.cursors) >= webUIHistoryCursors {
		var oldest string
		var age time.Time
		for id, value := range c.cursors {
			if oldest == "" || value.created.Before(age) {
				oldest, age = id, value.created
			}
		}
		delete(c.cursors, oldest)
	}
	id := "worker:" + uuid.NewString()
	position.created = time.Now()
	c.cursors[id] = position
	return id
}
func (c *webUIHistoryCache) snapshot(id string) (*webUIHistorySnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(time.Now())
	snapshot := c.snapshots[id]
	if snapshot == nil {
		return nil, validationError("Conversation page expired. Refresh the conversation.")
	}
	snapshot.used = time.Now()
	return snapshot, nil
}
func (c *webUIHistoryCache) completed(scope webUIHistoryScope, turnID string) *webUIHistorySnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(time.Now())
	for _, snapshot := range c.snapshots {
		if snapshot.scope == scope && snapshot.turn.ID == turnID && webUIHistoryTerminal(snapshot.turn.Status) {
			snapshot.used = time.Now()
			return snapshot
		}
	}
	return nil
}
func (c *webUIHistoryCache) insert(scope webUIHistoryScope, turn codexadapter.TranscriptTurn) (*webUIHistorySnapshot, error) {
	bytes := 0
	for _, item := range turn.Items {
		bytes += len(item) + 32
	}
	if bytes > webUIHistoryBytes {
		return nil, validationError("This source turn exceeds the bounded history cache. Use the Codex terminal to inspect it.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(time.Now())
	for len(c.snapshots) >= webUIHistorySnapshots || c.bytes+bytes > webUIHistoryBytes {
		var oldest *webUIHistorySnapshot
		for _, snapshot := range c.snapshots {
			if oldest == nil || snapshot.used.Before(oldest.used) {
				oldest = snapshot
			}
		}
		if oldest == nil {
			break
		}
		c.bytes -= oldest.bytes
		delete(c.snapshots, oldest.id)
	}
	snapshot := &webUIHistorySnapshot{id: uuid.NewString(), scope: scope, turn: turn, bytes: bytes, created: time.Now(), used: time.Now()}
	c.snapshots[snapshot.id] = snapshot
	c.bytes += bytes
	return snapshot, nil
}

func (a *Agent) webUIHistory(ctx context.Context, runtime protocol.Runtime, session protocol.Session, history webUIHistoryRequest) (json.RawMessage, error) {
	reply, err := a.dispatchWebUIActor(webUIActorCommand{ctx: ctx, runtime: runtime, session: session, history: &history})
	return reply.history, err
}
func (s *sessionActor) webUIHistory(request webUIActorCommand) (json.RawMessage, error) {
	ctx := request.ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.runtime.ID != s.runtime.ID || request.runtime.Generation != s.runtime.Generation || request.session.ID != s.session.ID || request.session.ThreadID != s.session.ThreadID || request.session.WorkerID != s.agent.cfg.WorkerID || request.session.RuntimeID != s.runtime.ID || s.session.Archived || s.session.Deleted {
		return nil, validationError("The conversation target changed. Reconnect to refresh it.")
	}
	client, runtime, ok := s.agent.manager.Client(s.runtime.ID)
	if !ok || runtime.Generation != request.runtime.Generation || runtime.State != "running" {
		return nil, validationError("The conversation runtime is unavailable.")
	}
	if _, err := auth.CanonicalWorkspace(s.session.CWD, s.agent.cfg.AllowedWorkspaceRoots); err != nil {
		return nil, validationError("The conversation workspace is no longer allowed.")
	}
	// Recheck source identity and the persisted workspace even for cache hits.
	// The browser separately validates authoritative live cwd on resume.
	thread, err := client.ReadThread(ctx, s.session.ThreadID, false)
	if err != nil {
		return nil, err
	}
	if thread.ID != s.session.ThreadID || !thread.UserSession() {
		return nil, validationError("The conversation is not a user session.")
	}
	if _, err := auth.CanonicalWorkspace(thread.CWD, s.agent.cfg.AllowedWorkspaceRoots); err != nil {
		return nil, validationError("The conversation workspace is no longer allowed.")
	}
	history := *request.history
	if history.Limit < 1 || history.Limit > 20 || (history.Direction != "asc" && history.Direction != "desc") || len(history.Cursor) > 4096 {
		return nil, validationError("Invalid bounded conversation request.")
	}
	scope := webUIHistoryScope{Runtime: runtime.ID, Generation: runtime.Generation, Thread: s.session.ThreadID}
	position, err := s.agent.webHistory.position(scope, history)
	if err != nil {
		return nil, err
	}
	page := webUIHistoryPage{Data: make([]webUIHistoryEntry, 0, history.Limit)}
	seenCursors := make(map[string]bool)
	var source webUIHistorySource
	defer func() {
		if source != nil {
			_ = source.Close()
		}
	}()
	for traversed := 0; len(page.Data) < history.Limit && !position.done; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var snapshot *webUIHistorySnapshot
		if position.snapshot != "" {
			snapshot, err = s.agent.webHistory.snapshot(position.snapshot)
			if err != nil {
				return nil, err
			}
		} else {
			if traversed >= webUIHistoryTraversal {
				break
			}
			traversed++
			if seenCursors[position.nativeCursor] {
				return nil, errors.New("Codex repeated a conversation cursor")
			}
			seenCursors[position.nativeCursor] = true
			metadata, readErr := client.ReadTranscriptTurnPage(ctx, s.session.ThreadID, position.nativeCursor, history.Direction, false)
			if readErr != nil {
				return nil, readErr
			}
			if len(metadata.Data) == 0 {
				position.done = true
				break
			}
			turn := metadata.Data[0]
			if webUIHistoryTerminal(turn.Status) {
				snapshot = s.agent.webHistory.completed(scope, turn.ID)
			}
			if snapshot == nil {
				if source == nil {
					if open := s.agent.historySource; open != nil {
						source, err = open(ctx, runtime.LocalSocket)
					} else {
						source, err = codexadapter.OpenTranscriptSource(ctx, runtime.LocalSocket)
					}
					if err != nil {
						return nil, err
					}
				}
				full, readErr := source.ReadPage(ctx, s.session.ThreadID, position.nativeCursor, history.Direction)
				if readErr != nil {
					return nil, readErr
				}
				if len(full.Data) != 1 || full.Data[0].ID != turn.ID {
					return nil, validationError("Conversation changed while it was loading. Refresh the conversation.")
				}
				turn = full.Data[0]
				turn.Items, err = webUISanitizeHistoryItems(s.agent.redactor, turn.Items)
				if err != nil {
					return nil, err
				}
				snapshot, err = s.agent.webHistory.insert(scope, turn)
				if err != nil {
					return nil, err
				}
			}
			position.snapshot = snapshot.id
			position.nativeCursor = metadata.NextCursor
			position.index = 0
			if history.Direction == "desc" {
				position.index = len(snapshot.turn.Items) - 1
			}
		}
		for position.index >= 0 && position.index < len(snapshot.turn.Items) && len(page.Data) < history.Limit {
			page.Data = append(page.Data, webUIHistoryEntry{TurnID: snapshot.turn.ID, Item: snapshot.turn.Items[position.index], TurnStartedAt: webUIHistoryTimestamp(snapshot.turn.StartedAt), TurnCompletedAt: webUIHistoryTimestamp(snapshot.turn.CompletedAt), TurnStatus: snapshot.turn.Status})
			if history.Direction == "desc" {
				position.index--
			} else {
				position.index++
			}
		}
		if position.index < 0 || position.index >= len(snapshot.turn.Items) {
			position.snapshot = ""
			position.done = position.nativeCursor == ""
		}
	}
	if !position.done {
		page.NextCursor = s.agent.webHistory.continuation(position)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return nil, err
	}
	if len(raw) > webUIHistoryPageBytes {
		return nil, validationError("Conversation page exceeds the display limit.")
	}
	return raw, nil
}

func webUIHistoryTerminal(status string) bool {
	return status == "completed" || status == "interrupted" || status == "failed"
}

func webUIHistoryTimestamp(value *int64) *int64 {
	if value == nil || *value <= 0 || *value > 253402300799 {
		return nil
	}
	return value
}
func webUISanitizeHistoryItems(redactor *auth.Redactor, items []json.RawMessage) ([]json.RawMessage, error) {
	result := make([]json.RawMessage, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, raw := range items {
		var item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &item) != nil || item.ID == "" || len(item.ID) > 512 || item.Type == "" || len(item.Type) > 128 || seen[item.ID] {
			return nil, errors.New("Codex returned invalid transcript item identity")
		}
		seen[item.ID] = true
		visible, err := redactWebUIJSON(redactor, raw)
		if err != nil {
			return nil, err
		}
		if len(visible) > webUIHistoryItemBytes {
			// Never retain attachments or giant tool buffers just to render a
			// bounded transcript page. Preserve identity without exposing a
			// partial credential caused by truncating before redaction.
			notice := fmt.Sprintf("This %s entry exceeds the Web UI display limit. View it in the Codex terminal.", item.Type)
			placeholder := map[string]any{"id": item.ID, "type": item.Type, "text": notice, "displayNotice": notice}
			var source map[string]json.RawMessage
			_ = json.Unmarshal(visible, &source)
			for _, key := range []string{"status", "createdAt", "phase", "role"} {
				if len(source[key]) <= 256 && len(source[key]) > 0 {
					placeholder[key] = source[key]
				}
			}
			if item.Type == "reasoning" {
				placeholder["summary"] = []string{notice}
			}
			visible, err = json.Marshal(placeholder)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, visible)
	}
	return result, nil
}

package worker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

const historyTimestampTailBytes = 4 << 20

// enrichHistoryTimestamps adds message times without asking app-server to
// reload the transcript. Discovery has already validated the rollout path;
// an absent cache entry simply retains the explicitly labelled turn time.
func (s *sessionActor) enrichHistoryTimestamps(client *codexadapter.Client, messages []protocol.HistoryMessage) {
	if len(messages) == 0 || s.agent.manager == nil {
		return
	}
	info, ok := client.InitializeInfo()
	if !ok || info.CodexHome == "" {
		return
	}
	path := s.agent.manager.stats.historyRolloutPath(info.CodexHome, s.session.ThreadID)
	if path != "" {
		enrichRolloutHistoryTimestamps(info.CodexHome, path, s.session.ThreadID, messages)
	}
}

// The discovery cache has at most statsCacheSize entries. Using its existing
// identities avoids another index, directory traversal, or metadata RPC.
func (c *rolloutStatsCache) historyRolloutPath(home, threadID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var path string
	var newest time.Time
	for key, entry := range c.entries {
		parts := strings.Split(key, "\x00")
		if len(parts) == 4 && parts[1] == home && parts[3] == threadID && (path == "" || entry.used.After(newest)) {
			path, newest = filepath.Join(home, parts[2]), entry.used
		}
	}
	return path
}

// Only exact saved item identities are matched: identical text in different
// prompts must never borrow another prompt's time. The small tail bound keeps
// on-demand history reads independent of the size of the saved session store.
func enrichRolloutHistoryTimestamps(home, path, threadID string, messages []protocol.HistoryMessage) {
	rel, err := filepath.Rel(home, path)
	if err != nil || filepath.IsAbs(rel) || filepath.Ext(rel) != ".jsonl" {
		return
	}
	normalized := filepath.ToSlash(rel)
	if !strings.HasPrefix(normalized, "sessions/") && !strings.HasPrefix(normalized, "archived_sessions/") {
		return
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return
	}
	defer root.Close()
	file, err := root.Open(rel)
	if err != nil {
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return
	}
	line, err := bufio.NewReader(io.LimitReader(file, statsLineBytes)).ReadBytes('\n')
	if err != nil {
		return
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != threadID {
		return
	}
	start := max(int64(0), stat.Size()-historyTimestampTailBytes)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return
	}
	wanted := make(map[string]historyTimestampTarget, len(messages))
	requested := make(map[string]bool, len(messages))
	for _, message := range messages {
		if message.TurnID != "" && message.ItemID != "" && (message.Role == "user" || message.Role == "assistant") {
			if requested[message.ItemID] {
				wanted[message.ItemID] = historyTimestampTarget{}
			} else {
				wanted[message.ItemID] = historyTimestampTarget{message.TurnID, message.Role}
			}
			requested[message.ItemID] = true
		}
	}
	times := readHistoryMessageTimes(io.LimitReader(file, stat.Size()-start), start > 0, wanted)
	if after, err := file.Stat(); err != nil || after.Size() < stat.Size() || (after.Size() == stat.Size() && !after.ModTime().Equal(stat.ModTime())) {
		return
	}
	for i := range messages {
		if stamp, ok := times[messages[i].ItemID]; ok {
			messages[i].Timestamp = &stamp
			messages[i].TimestampSource = "message"
		}
	}
}

type historyTimestampTarget struct {
	turn, role string
}

func readHistoryMessageTimes(reader io.Reader, discardFirst bool, wanted map[string]historyTimestampTarget) map[string]time.Time {
	result := make(map[string]time.Time)
	seen := make(map[string]bool)
	r := bufio.NewReaderSize(reader, 64<<10)
	var prefix []byte
	var turn string
	for {
		fragment, err := r.ReadSlice('\n')
		if !discardFirst && len(prefix) < statsLineBytes {
			prefix = append(prefix, fragment[:min(statsLineBytes-len(prefix), len(fragment))]...)
		}
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			if !discardFirst {
				if nextTurn, boundary := historyRecordTurn(prefix); boundary {
					turn = nextTurn
				}
				id, role, stamp := historyRecordIdentity(prefix)
				if target := wanted[id]; turn != "" && target.turn == turn && target.role == role && role != "" {
					if seen[id] {
						// Compaction or malformed transcripts may repeat an ID.
						// An ambiguous record is never a verified message time.
						delete(result, id)
					} else if stamp != nil {
						result[id] = *stamp
					}
					seen[id] = true
				}
			}
			prefix, discardFirst = nil, false
		}
		if err != nil && err != bufio.ErrBufferFull {
			return result // Incomplete final records are deliberately ignored.
		}
	}
}

// An item ID is only guaranteed unique within its turn. The tail may begin
// after the turn boundary; those records keep their original turn fallback.
func historyRecordTurn(prefix []byte) (string, bool) {
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			Type   string `json:"type"`
			TurnID string `json:"turn_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(prefix, &record) != nil {
		switch rolloutRecordType(prefix) {
		case "turn_context", "session_meta", "event_msg":
			return "", true
		}
		return "", false
	}
	if record.Type == "session_meta" {
		return "", true
	}
	if record.Type == "turn_context" || (record.Type == "event_msg" && (record.Payload.Type == "task_started" || record.Payload.Type == "turn_started")) {
		return record.Payload.TurnID, true
	}
	return "", false
}

// Parse only fields before content. Official rollouts place identity fields
// before image data; this also handles large image records without decoding or
// retaining their payload. Unsupported field order safely keeps the fallback.
func historyRecordIdentity(prefix []byte) (id, role string, stamp *time.Time) {
	if len(prefix) < statsLineBytes {
		var record struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Role string `json:"role"`
			} `json:"payload"`
		}
		if json.Unmarshal(prefix, &record) != nil || record.Type != "response_item" || record.Payload.Type != "message" || record.Payload.ID == "" || (record.Payload.Role != "user" && record.Payload.Role != "assistant") {
			return "", "", nil
		}
		stamp = statsTime(record.Timestamp)
		if stamp == nil || stamp.Unix() <= 0 || stamp.Year() > 9999 {
			return "", "", nil
		}
		return record.Payload.ID, record.Payload.Role, stamp
	}
	dec := json.NewDecoder(bytes.NewReader(prefix))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return "", "", nil
	}
	var kind, timestamp string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			break
		}
		switch key {
		case "type":
			if dec.Decode(&kind) != nil {
				return "", "", nil
			}
		case "timestamp":
			if dec.Decode(&timestamp) != nil {
				return "", "", nil
			}
		case "payload":
			if kind != "response_item" {
				return "", "", nil
			}
			if token, err := dec.Token(); err != nil || token != json.Delim('{') {
				return "", "", nil
			}
			var messageType string
			for dec.More() {
				field, err := dec.Token()
				if err != nil {
					return "", "", nil
				}
				switch field {
				case "type":
					if dec.Decode(&messageType) != nil {
						return "", "", nil
					}
				case "id":
					if dec.Decode(&id) != nil {
						return "", "", nil
					}
				case "role":
					if dec.Decode(&role) != nil {
						return "", "", nil
					}
				default:
					return "", "", nil
				}
				if messageType == "message" && id != "" && (role == "user" || role == "assistant") {
					stamp = statsTime(timestamp)
					if stamp == nil || stamp.Unix() <= 0 || stamp.Year() > 9999 {
						return "", "", nil
					}
					return id, role, stamp
				}
			}
			return "", "", nil
		default:
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return "", "", nil
			}
		}
	}
	return "", "", nil
}

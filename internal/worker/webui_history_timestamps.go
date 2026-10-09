package worker

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

// Capture matching metadata before redaction and display-size projection. A
// reconstructed item can then retain its timestamp association throughout its
// snapshot lifetime without keeping an additional copy of private text.
func webUIHistoryTimestampQueries(turn codexadapter.TranscriptTurn) []rolloutTimestampQuery {
	queries := make([]rolloutTimestampQuery, len(turn.Items))
	counts := make(map[string]int)
	messages := 0
	sequence := ""
	sequenceComplete := true
	var previousSynthetic uint64
	ordered := true
	for i, raw := range turn.Items {
		var item struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Phase   string          `json:"phase"`
			Text    *string         `json:"text"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		query := rolloutTimestampQuery{TurnID: turn.ID, ItemID: item.ID}
		switch item.Type {
		case "userMessage":
			query.Role = "user"
			messages++
			if text, ok := webUIHistoryUserTimestampText(item.Content); ok {
				query.Digest = rolloutMessageDigest(query.Role, text)
			}
		case "agentMessage":
			query.Role = "assistant"
			query.Phase = item.Phase
			if item.Text != nil && *item.Text != "" {
				messages++
				query.Digest = rolloutMessageDigest(query.Role, *item.Text)
			}
		case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall":
			query.Role = "tool"
		case "contextCompaction":
			query.Role = "compaction"
			query.Digest = rolloutMessageDigest(query.Role, "")
		}
		if query.Role == "user" || query.Role == "compaction" || (query.Role == "assistant" && item.Text != nil && *item.Text != "") {
			if query.Digest == "" {
				sequenceComplete = false
			} else {
				sequence = rolloutTimestampSequenceDigest(sequence, query.Role, query.Phase, query.Digest)
			}
		}
		if query.Role == "user" || query.Role == "assistant" || query.Role == "compaction" {
			if ordinal, ok := webUIHistorySyntheticOrdinal(item.ID); ok {
				query.Synthetic = true
				if ordinal <= previousSynthetic {
					ordered = false
				}
				previousSynthetic = ordinal
			}
		}
		if query.Digest != "" {
			key := query.Role + "\x00" + query.Phase + "\x00" + query.Digest
			query.Occurrence = counts[key]
			counts[key]++
		}
		queries[i] = query
	}
	for i := range queries {
		query := &queries[i]
		query.ExpectedOccurrences = counts[query.Role+"\x00"+query.Phase+"\x00"+query.Digest]
		query.ExpectedMessages = messages
		if sequenceComplete {
			query.ExpectedSequenceDigest = sequence
		}
		if query.Synthetic && !ordered {
			// A synthetic ID must never fall through to exact-ID matching:
			// that counter has no relationship to IDs in response mirrors.
			query.Digest, query.ExpectedOccurrences = "", 0
		}
	}
	return queries
}

// Codex's saved-thread reconstruction assigns a global, increasing item-N
// counter to user and agent events. Arbitrary IDs cannot opt into content
// matching; exact source identities remain available independently.
func webUIHistorySyntheticOrdinal(id string) (uint64, bool) {
	if !strings.HasPrefix(id, "item-") {
		return 0, false
	}
	value := strings.TrimPrefix(id, "item-")
	ordinal, err := strconv.ParseUint(value, 10, 64)
	return ordinal, err == nil && ordinal > 0 && strconv.FormatUint(ordinal, 10) == value
}

// Text-only projections are exact. Attachment labels, truncation, and unknown
// content cannot provide a verified fingerprint and keep the turn fallback.
func webUIHistoryUserTimestampText(raw json.RawMessage) (string, bool) {
	var parts []struct {
		Type string  `json:"type"`
		Text *string `json:"text"`
	}
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var text strings.Builder
	for _, part := range parts {
		if part.Type != "text" || part.Text == nil {
			return "", false
		}
		text.WriteString(*part.Text)
	}
	return text.String(), true
}

func (s *sessionActor) enrichWebUIHistoryTimestamps(client *codexadapter.Client, thread codexadapter.Thread, entries []webUIHistoryEntry, queries []rolloutTimestampQuery) {
	if len(entries) == 0 || s.agent.manager == nil {
		return
	}
	info, initialized := client.InitializeInfo()
	if !initialized || info.CodexHome == "" {
		return
	}
	// The lightweight thread read already performed for history authorization
	// supplies the rollout path; no extra app-server call is needed per page.
	var wire struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(thread.Raw, &wire)
	if wire.Path == "" {
		wire.Path = s.agent.manager.stats.historyRolloutPath(info.CodexHome, s.session.ThreadID)
	}
	if wire.Path != "" {
		enrichWebUIHistoryTimestampEntries(&s.agent.manager.stats, info.CodexHome, wire.Path, s.session.ThreadID, entries, queries)
	}
}

func enrichWebUIHistoryTimestampEntries(cache *rolloutStatsCache, home, path, threadID string, entries []webUIHistoryEntry, queries []rolloutTimestampQuery) {
	if len(entries) == 0 || len(entries) > 20 || len(queries) != len(entries) {
		return
	}
	pending := make([]rolloutTimestampQuery, 0, len(queries))
	positions := make([]int, 0, len(queries))
	for i, query := range queries {
		if entries[i].RecordedAtMS == nil && query.Role != "" {
			pending = append(pending, query)
			positions = append(positions, i)
		}
	}
	if len(pending) == 0 {
		return
	}
	results := cache.rolloutTimestampCandidates(home, path, threadID, pending)
	for i, result := range results {
		if i >= len(positions) || !result.Found {
			continue
		}
		// Rollout timestamps record when a saved record was written. They do
		// not establish the start or completion of an item lifecycle.
		stamp := result.Timestamp.UnixMilli()
		if stamp > 0 && stamp <= 253402300799999 {
			entries[positions[i]].RecordedAtMS = &stamp
		}
	}
}

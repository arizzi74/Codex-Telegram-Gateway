package worker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

const (
	statsScanBytes = 16 << 20
	statsTailBytes = 4 << 20
	statsLineBytes = 256 << 10
	statsCacheSize = 128
)

// Progress through old transcripts is incremental and bounded per discovery.
// A separate bounded tail supplies recent activity while older prompt counts
// catch up. Files are never loaded wholesale or handed to a model.
type rolloutStatsCache struct {
	mu      sync.Mutex
	entries map[string]*rolloutStatsEntry
}

type rolloutStatsEntry struct {
	fileInfo os.FileInfo
	offset   int64
	scan     rolloutStatsScan
	tail     rolloutStatsScan
	observed time.Time
	used     time.Time
}

type rolloutStatsScan struct {
	stats           protocol.SessionStats
	partial         []byte
	discard         bool
	gap             bool
	promptEvents    int64
	promptResponses int64
	replyEvents     int64
	replyResponses  int64
	activeTurn      string
	activeSince     *time.Time
}

func (m *RuntimeManager) sessionStats(client *codexadapter.Client, thread codexadapter.Thread) *protocol.SessionStats {
	var stats *protocol.SessionStats
	info, initialized := client.InitializeInfo()
	var wire struct {
		Path string `json:"path"`
	}
	if initialized && info.CodexHome != "" && json.Unmarshal(thread.Raw, &wire) == nil && wire.Path != "" {
		stats = m.stats.read(info.CodexHome, wire.Path, thread.ID, thread.ActiveTurnID, m.statsRedactor)
	}
	if stats == nil {
		if thread.CreatedAt <= 0 && thread.Model == "" {
			return nil
		}
		stats = &protocol.SessionStats{ObservedAt: time.Now().UTC()}
	}
	if thread.CreatedAt > 0 {
		created := time.Unix(thread.CreatedAt, 0).UTC()
		stats.CreatedAt = &created
	}
	if stats.Model == "" {
		stats.Model = thread.Model
	}
	return stats
}

func (c *rolloutStatsCache) read(home, path, threadID, activeTurn string, redactor *auth.Redactor) *protocol.SessionStats {
	rel, err := filepath.Rel(home, path)
	if err != nil || filepath.IsAbs(rel) || filepath.Ext(rel) != ".jsonl" {
		return nil
	}
	normalized := filepath.ToSlash(rel)
	if !strings.HasPrefix(normalized, "sessions/") && !strings.HasPrefix(normalized, "archived_sessions/") {
		return nil
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil
	}
	defer root.Close()
	file, err := root.Open(rel)
	if err != nil {
		return nil
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil
	}
	metaLine, err := bufio.NewReader(io.LimitReader(file, statsLineBytes)).ReadBytes('\n')
	if err != nil {
		return nil
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if json.Unmarshal(metaLine, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != threadID {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*rolloutStatsEntry)
	}
	key := home + "\x00" + rel + "\x00" + threadID
	entry := c.entries[key]
	if entry == nil || !os.SameFile(entry.fileInfo, stat) || stat.Size() < entry.fileInfo.Size() || (stat.Size() == entry.fileInfo.Size() && !stat.ModTime().Equal(entry.fileInfo.ModTime())) {
		if entry == nil && len(c.entries) >= statsCacheSize {
			oldestKey := ""
			var oldest time.Time
			for k, e := range c.entries {
				if oldestKey == "" || e.used.Before(oldest) {
					oldestKey, oldest = k, e.used
				}
			}
			delete(c.entries, oldestKey)
		}
		entry = &rolloutStatsEntry{fileInfo: stat}
		entry.scan.stats.PromptCount, entry.scan.stats.AssistantMessageCount = statsNumber(0), statsNumber(0)
		c.entries[key] = entry
	}
	entry.used = time.Now().UTC()
	changed := entry.observed.IsZero() || stat.Size() != entry.fileInfo.Size() || !stat.ModTime().Equal(entry.fileInfo.ModTime())
	if entry.offset < stat.Size() {
		if _, err := file.Seek(entry.offset, io.SeekStart); err != nil {
			return nil
		}
		consumed, err := entry.scan.read(io.LimitReader(file, min(int64(statsScanBytes), stat.Size()-entry.offset)), redactor)
		if err != nil {
			return nil
		}
		entry.offset += consumed
		entry.observed = entry.used
	}
	entry.fileInfo = stat
	if entry.offset < stat.Size() && changed {
		start := max(int64(0), stat.Size()-statsTailBytes)
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return nil
		}
		entry.tail = rolloutStatsScan{discard: start > 0}
		if _, err := entry.tail.read(io.LimitReader(file, statsTailBytes), redactor); err != nil {
			return nil
		}
	}
	stats := entry.scan.stats
	turn, since := entry.scan.activeTurn, entry.scan.activeSince
	if entry.offset < stat.Size() {
		mergeRecentStats(&stats, entry.tail.stats)
		if entry.tail.activeTurn != "" {
			turn, since = entry.tail.activeTurn, entry.tail.activeSince
		}
	}
	stats.HistoryComplete = entry.offset == stat.Size() && len(entry.scan.partial) == 0 && !entry.scan.discard && !entry.scan.gap
	stats.ObservedAt = entry.observed
	if activeTurn != "" && turn == activeTurn {
		stats.ActiveSince = since
	}
	return &stats
}

func mergeRecentStats(dst *protocol.SessionStats, recent protocol.SessionStats) {
	if recent.LastMessageRole != "" && (dst.LastMessageAt == nil || recent.LastMessageAt == nil || !recent.LastMessageAt.Before(*dst.LastMessageAt)) {
		dst.LastMessage, dst.LastMessageRole, dst.LastMessageAt = recent.LastMessage, recent.LastMessageRole, recent.LastMessageAt
	}
	if recent.TotalTokens != nil {
		dst.TotalTokens, dst.InputTokens, dst.CachedInputTokens = recent.TotalTokens, recent.InputTokens, recent.CachedInputTokens
		dst.OutputTokens, dst.ReasoningOutputTokens = recent.OutputTokens, recent.ReasoningOutputTokens
		dst.ContextTokens, dst.ContextWindow = recent.ContextTokens, recent.ContextWindow
	}
	if recent.Model != "" {
		dst.Model = recent.Model
	}
	if recent.ReasoningEffort != "" {
		dst.ReasoningEffort = recent.ReasoningEffort
	}
}

// read retains at most one bounded partial line between scans. Oversized tool
// and image records are discarded through their newline, then scanning resumes.
func (s *rolloutStatsScan) read(reader io.Reader, redactor *auth.Redactor) (int64, error) {
	r := bufio.NewReaderSize(reader, 64<<10)
	var consumed int64
	for {
		fragment, err := r.ReadSlice('\n')
		consumed += int64(len(fragment))
		if !s.discard {
			remaining := statsLineBytes - len(s.partial)
			s.partial = append(s.partial, fragment[:min(remaining, len(fragment))]...)
			if len(fragment) > remaining {
				s.discard = true
			}
		}
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			if s.discard {
				if len(s.partial) > 0 {
					switch rolloutRecordType(s.partial) {
					case "response_item":
						s.oversizedResponseMessage(s.partial, redactor)
					case "event_msg":
						if !s.oversizedUserMessage(s.partial, redactor) {
							s.gap = true
						}
					case "session_meta", "turn_context", "":
						s.gap = true
					}
				}
			} else {
				s.record(s.partial, redactor)
			}
			s.partial, s.discard = nil, false
		}
		if err == io.EOF {
			return consumed, nil
		}
		if err != nil && err != bufio.ErrBufferFull {
			return consumed, err
		}
	}
}

// Images also occur on user_message events. Their huge images/local_images
// arrays must not prevent counting an otherwise ordinary prompt. Read only the
// small fields preceding those arrays; never retain or display attachment data.
func (s *rolloutStatsScan) oversizedUserMessage(prefix []byte, redactor *auth.Redactor) bool {
	dec := json.NewDecoder(bytes.NewReader(prefix))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	var kind, timestamp string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return false
		}
		switch key {
		case "type":
			if dec.Decode(&kind) != nil {
				return false
			}
		case "timestamp":
			if dec.Decode(&timestamp) != nil {
				return false
			}
		case "payload":
			if kind != "event_msg" {
				return false
			}
			if token, err := dec.Token(); err != nil || token != json.Delim('{') {
				return false
			}
			var eventType, message string
			for dec.More() {
				field, err := dec.Token()
				if err != nil {
					break
				}
				switch field {
				case "type":
					err = dec.Decode(&eventType)
				case "message":
					err = dec.Decode(&message)
				default:
					var ignored json.RawMessage
					err = dec.Decode(&ignored)
				}
				if err != nil {
					break
				}
			}
			if eventType == "item_completed" || eventType == "item_started" {
				return true
			}
			if eventType != "user_message" {
				return false
			}
			s.countMessage("user", false)
			if message == "" {
				message = "[Large prompt or attachment]"
			}
			s.lastMessage("user", message, timestamp, redactor)
			return true
		default:
			var ignored json.RawMessage
			if dec.Decode(&ignored) != nil {
				return false
			}
		}
	}
	return false
}

func rolloutRecordType(prefix []byte) string {
	dec := json.NewDecoder(bytes.NewReader(prefix))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return ""
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return ""
		}
		if key == "type" {
			var kind string
			_ = dec.Decode(&kind)
			return kind
		}
		var ignored json.RawMessage
		if dec.Decode(&ignored) != nil {
			return ""
		}
	}
	return ""
}

func (s *rolloutStatsScan) record(line []byte, redactor *auth.Redactor) {
	var kind struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &kind) != nil {
		s.gap = true
		return
	}
	switch kind.Type {
	case "session_meta", "turn_context":
	case "response_item":
		if kind.Payload.Type != "message" {
			return
		}
	case "event_msg":
		switch kind.Payload.Type {
		case "task_started", "turn_started", "user_message", "agent_message", "token_count":
		default:
			return
		}
	default:
		return
	}
	var record struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Type      string                `json:"type"`
			Role      string                `json:"role"`
			Message   string                `json:"message"`
			Phase     string                `json:"phase"`
			Timestamp string                `json:"timestamp"`
			TurnID    string                `json:"turn_id"`
			StartedAt *int64                `json:"started_at"`
			Model     string                `json:"model"`
			Effort    string                `json:"effort"`
			Content   []statsMessageContent `json:"content"`
			Info      *struct {
				Total struct {
					Total     *int64 `json:"total_tokens"`
					Input     *int64 `json:"input_tokens"`
					Cached    *int64 `json:"cached_input_tokens"`
					Output    *int64 `json:"output_tokens"`
					Reasoning *int64 `json:"reasoning_output_tokens"`
				} `json:"total_token_usage"`
				Last struct {
					Total *int64 `json:"total_tokens"`
				} `json:"last_token_usage"`
				Window *int64 `json:"model_context_window"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &record) != nil {
		s.gap = true
		return
	}
	p := record.Payload
	switch record.Type {
	case "session_meta":
		s.stats.CreatedAt = statsTime(p.Timestamp)
	case "turn_context":
		s.stats.Model, s.stats.ReasoningEffort = p.Model, p.Effort
	case "response_item":
		if p.Type == "message" {
			s.responseMessage(p.Role, p.Phase, p.Content, record.Timestamp, redactor)
		}
	case "event_msg":
		switch p.Type {
		case "task_started", "turn_started":
			if p.TurnID != s.activeTurn {
				s.promptEvents, s.promptResponses, s.replyEvents, s.replyResponses = 0, 0, 0, 0
			}
			s.activeTurn, s.activeSince = p.TurnID, statsTime(record.Timestamp)
			if p.StartedAt != nil && *p.StartedAt > 0 {
				t := time.Unix(*p.StartedAt, 0).UTC()
				s.activeSince = &t
			}
		case "user_message":
			s.countMessage("user", false)
			s.lastMessage("user", p.Message, record.Timestamp, redactor)
		case "agent_message":
			if p.Phase != "" && p.Phase != "final_answer" {
				return
			}
			s.countMessage("assistant", false)
			s.lastMessage("assistant", p.Message, record.Timestamp, redactor)
		case "token_count":
			if p.Info == nil {
				return
			}
			s.stats.TotalTokens, s.stats.InputTokens, s.stats.CachedInputTokens = nonnegativeStats(p.Info.Total.Total), nonnegativeStats(p.Info.Total.Input), nonnegativeStats(p.Info.Total.Cached)
			s.stats.OutputTokens, s.stats.ReasoningOutputTokens = nonnegativeStats(p.Info.Total.Output), nonnegativeStats(p.Info.Total.Reasoning)
			s.stats.ContextTokens, s.stats.ContextWindow = nonnegativeStats(p.Info.Last.Total), nonnegativeStats(p.Info.Window)
		}
	}
}

func (s *rolloutStatsScan) lastMessage(role, message, timestamp string, redactor *auth.Redactor) {
	if redactor != nil {
		message = redactor.Redact(message)
	}
	message = strings.TrimSpace(message)
	if message == "" && role == "user" {
		message = "[Non-text prompt]"
	}
	runes := []rune(message)
	if len(runes) > 600 {
		message = string(runes[:600]) + "…"
	}
	s.stats.LastMessage, s.stats.LastMessageRole, s.stats.LastMessageAt = message, role, statsTime(timestamp)
}

type statsMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Codex versions differ in whether they save legacy user/agent events, model
// message items, or both. Within each turn the larger count includes every
// saved message without counting the two representations twice. Resetting at
// turn boundaries also handles a session that spans a runtime upgrade.
func (s *rolloutStatsScan) countMessage(role string, response bool) {
	events, responses, total := &s.promptEvents, &s.promptResponses, &s.stats.PromptCount
	if role == "assistant" {
		events, responses, total = &s.replyEvents, &s.replyResponses, &s.stats.AssistantMessageCount
	}
	before := max(*events, *responses)
	if response {
		*responses++
	} else {
		*events++
	}
	if max(*events, *responses) > before {
		*total = incrementStats(*total)
	}
}

func (s *rolloutStatsScan) responseMessage(role, phase string, content []statsMessageContent, timestamp string, redactor *auth.Redactor) {
	if role != "user" && role != "assistant" {
		return
	}
	if role == "assistant" && phase != "" && phase != "final_answer" {
		return
	}
	var text strings.Builder
	for _, part := range content {
		if role == "user" && part.Type == "input_text" && statsContextualUserText(part.Text) {
			return
		}
	}
	for index, part := range content {
		switch part.Type {
		case "input_image":
			text.WriteString("[Image]")
		case "input_text", "output_text":
			if role == "user" {
				if part.Type != "input_text" {
					continue
				}
				trimmed := strings.TrimSpace(part.Text)
				if index+1 < len(content) && content[index+1].Type == "input_image" && (trimmed == "<image>" || strings.HasPrefix(trimmed, "<image name=")) {
					continue
				}
				if index > 0 && content[index-1].Type == "input_image" && trimmed == "</image>" {
					continue
				}
			}
			text.WriteString(part.Text)
		}
	}
	message := text.String()
	if role == "user" {
		if _, suffix, found := strings.Cut(message, "## My request for Codex:"); found {
			message = strings.TrimSpace(suffix)
		}
	}
	s.countMessage(role, true)
	s.lastMessage(role, message, timestamp, redactor)
}

var statsExternalContext = regexp.MustCompile(`^<external_([A-Za-z0-9_]+)>`)
var statsInternalContext = regexp.MustCompile(`^<codex_internal_context source="[a-z][a-z0-9_]*">`)

// These are the contextual fragments excluded by Codex's user-message parser.
// Match complete wrappers; ordinary user-authored XML remains a prompt.
func statsContextualUserText(text string) bool {
	text = strings.TrimSpace(text)
	lower := strings.ToLower(text)
	for _, pair := range [][2]string{
		{"# agents.md instructions", "</instructions>"},
		{"<user_instructions>", "</user_instructions>"},
		{"<environment_context>", "</environment_context>"},
		{"<skill>", "</skill>"},
		{"<user_shell_command>", "</user_shell_command>"},
		{"<turn_aborted>", "</turn_aborted>"},
		{"<subagent_notification>", "</subagent_notification>"},
		{"<recommended_plugins>", "</recommended_plugins>"},
	} {
		if strings.HasPrefix(lower, pair[0]) && strings.HasSuffix(lower, pair[1]) {
			return true
		}
	}
	if strings.HasPrefix(text, "<goal_context>") && strings.HasSuffix(text, "</goal_context>") {
		return true
	}
	if match := statsExternalContext.FindStringSubmatch(text); match != nil && strings.HasSuffix(text, "</external_"+match[1]+">") {
		return true
	}
	if statsInternalContext.MatchString(text) && strings.HasSuffix(text, "</codex_internal_context>") {
		return true
	}
	if strings.HasPrefix(text, "<hook_prompt ") && strings.Contains(text, `hook_run_id="`) && strings.HasSuffix(text, "</hook_prompt>") {
		return true
	}
	return strings.HasPrefix(text, "Warning: The maximum number of unified exec processes you can keep open is") ||
		strings.HasPrefix(text, "Warning: Your account was flagged for potentially high-risk cyber activity") ||
		(strings.HasPrefix(text, "Warning: apply_patch was requested via ") && strings.HasSuffix(text, "Use the apply_patch tool instead of exec_command."))
}

// Salvage the bounded text before a large image, without reading attachment
// bytes into memory. If the message cannot be fully classified, counts remain
// explicitly incomplete instead of silently reporting an exact total.
func (s *rolloutStatsScan) oversizedResponseMessage(prefix []byte, redactor *auth.Redactor) {
	dec := json.NewDecoder(bytes.NewReader(prefix))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		s.gap = true
		return
	}
	var timestamp, kind, role, phase string
	var content []statsMessageContent
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			s.gap = true
			return
		}
		if key == "timestamp" {
			if dec.Decode(&timestamp) != nil {
				s.gap = true
				return
			}
			continue
		}
		if key != "payload" {
			var ignored json.RawMessage
			if dec.Decode(&ignored) != nil {
				s.gap = true
				return
			}
			continue
		}
		if token, err := dec.Token(); err != nil || token != json.Delim('{') {
			s.gap = true
			return
		}
		for dec.More() {
			field, err := dec.Token()
			if err != nil {
				break
			}
			switch field {
			case "type":
				err = dec.Decode(&kind)
				if err == nil && kind != "message" {
					return
				}
			case "role":
				err = dec.Decode(&role)
			case "phase":
				err = dec.Decode(&phase)
			case "content":
				var token json.Token
				token, err = dec.Token()
				if err != nil || token != json.Delim('[') {
					break
				}
				for dec.More() {
					var part statsMessageContent
					token, err = dec.Token()
					if err != nil || token != json.Delim('{') {
						break
					}
					for dec.More() {
						var itemKey json.Token
						itemKey, err = dec.Token()
						if err != nil {
							break
						}
						if itemKey == "type" {
							err = dec.Decode(&part.Type)
						} else if itemKey == "text" {
							err = dec.Decode(&part.Text)
						} else {
							var ignored json.RawMessage
							err = dec.Decode(&ignored)
						}
						if err != nil {
							break
						}
					}
					if part.Type == "input_image" || part.Text != "" {
						content = append(content, part)
					}
					if err != nil {
						break
					}
					_, err = dec.Token()
					if err != nil {
						break
					}
				}
				if err == nil {
					_, err = dec.Token()
				}
			default:
				var ignored json.RawMessage
				err = dec.Decode(&ignored)
			}
			if err != nil {
				break
			}
		}
		if kind == "message" {
			s.gap = true
			if role == "user" && len(content) > 0 {
				s.responseMessage(role, phase, content, timestamp, redactor)
			}
		}
		return
	}
	s.gap = true
}

func statsTime(value string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

func statsNumber(value int64) *int64 { return &value }
func incrementStats(value *int64) *int64 {
	if value == nil {
		return statsNumber(1)
	}
	return statsNumber(*value + 1)
}
func nonnegativeStats(value *int64) *int64 {
	if value != nil && *value < 0 {
		return nil
	}
	return value
}

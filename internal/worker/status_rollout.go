package worker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

const statusTailBytes = 16 << 20

// rolloutStatus reads only status fields from the selected thread's private
// transcript. App-server does not expose historical context token counts over
// thread/read, so this mirrors the token_count information consumed by the TUI.
func (s *sessionActor) rolloutStatus(client *codexadapter.Client, thread codexadapter.Thread) []string {
	info, ok := client.InitializeInfo()
	if !ok || info.CodexHome == "" {
		return nil
	}
	var wire struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(thread.Raw, &wire) != nil || wire.Path == "" {
		return nil
	}
	return readRolloutStatus(info.CodexHome, wire.Path, thread.ID)
}

func readRolloutStatus(home, path, threadID string) []string {
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
	file, err := root.Open(rel) // OpenRoot prevents symlink traversal outside HOME.
	if err != nil {
		return nil
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil
	}
	first := bufio.NewReader(io.LimitReader(file, 1<<20))
	line, err := first.ReadBytes('\n')
	if err != nil {
		return nil
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != threadID {
		return nil
	}
	start := max(int64(0), stat.Size()-statusTailBytes)
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	reader := bufio.NewReader(io.LimitReader(file, statusTailBytes))
	if start > 0 {
		_, _ = reader.ReadBytes('\n')
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	var last struct {
		Total    int64
		Context  int64
		Window   int64
		Approval string
		Sandbox  string
		Roots    []string
	}
	for scanner.Scan() {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Type     string `json:"type"`
				Approval string `json:"approval_policy"`
				Sandbox  struct {
					Type  string   `json:"type"`
					Roots []string `json:"writable_roots"`
				} `json:"sandbox_policy"`
				Info *struct {
					Total struct {
						Tokens int64 `json:"total_tokens"`
					} `json:"total_token_usage"`
					Context struct {
						Tokens int64 `json:"total_tokens"`
					} `json:"last_token_usage"`
					Window int64 `json:"model_context_window"`
				} `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		if record.Type == "turn_context" {
			last.Approval, last.Sandbox, last.Roots = record.Payload.Approval, record.Payload.Sandbox.Type, record.Payload.Sandbox.Roots
		}
		if record.Type == "event_msg" && record.Payload.Type == "token_count" && record.Payload.Info != nil {
			last.Total, last.Context, last.Window = record.Payload.Info.Total.Tokens, record.Payload.Info.Context.Tokens, record.Payload.Info.Window
		}
	}
	if scanner.Err() != nil {
		return nil
	}
	var lines []string
	if last.Window > 0 {
		lines = append(lines, fmt.Sprintf("Last recorded context: %s / %s tokens (%.0f%% used)", formatInt(last.Context), formatInt(last.Window), 100*float64(last.Context)/float64(last.Window)))
	} else if last.Context > 0 {
		lines = append(lines, "Last recorded context: "+formatInt(last.Context)+" tokens")
	}
	if last.Total > 0 {
		lines = append(lines, "Session tokens: "+formatInt(last.Total))
	}
	if last.Approval != "" {
		lines = append(lines, "Last turn approval policy: "+last.Approval)
	}
	if last.Sandbox != "" {
		lines = append(lines, "Last turn sandbox: "+last.Sandbox)
	}
	if len(last.Roots) > 0 {
		lines = append(lines, "Last turn writable roots: "+strings.Join(last.Roots, ", "))
	}
	return lines
}

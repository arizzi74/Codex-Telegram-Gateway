package worker

import (
	"encoding/json"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

// Feed the timestamp index once per inventory pass. Only authoritative user
// sessions inside this worker's workspace policy may contribute rollout paths.
// The index owns the shared read budget and rotates among eligible files.
func (m *RuntimeManager) discoverTimestampIndex(client *codexadapter.Client, threads []codexadapter.Thread) {
	info, initialized := client.InitializeInfo()
	if !initialized || info.CodexHome == "" {
		return
	}
	files := make([]rolloutTimestampFile, 0, len(threads))
	for _, thread := range threads {
		if thread.ID == "" || !thread.UserSession() || thread.CWD == "" || !workspaceAllowed(thread.CWD, m.cfg.AllowedWorkspaceRoots) {
			continue
		}
		var wire struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(thread.Raw, &wire) == nil && wire.Path != "" {
			files = append(files, rolloutTimestampFile{Path: wire.Path, ThreadID: thread.ID})
		}
	}
	m.stats.discoverRolloutTimestampPass(info.CodexHome, files)
}

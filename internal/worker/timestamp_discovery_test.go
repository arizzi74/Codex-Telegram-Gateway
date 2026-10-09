package worker

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/codexadapter/codextest"
	"github.com/iaia/telegramgw/internal/config"
)

func TestTimestampDiscoveryUsesOnlyAuthorizedUserInventory(t *testing.T) {
	home := t.TempDir()
	path := historyTimestampFixture(t, home, "visible")
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client, server, err := codextest.New(t.Context(), codexadapter.InitializeInfo{CodexHome: home})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	allowed := t.TempDir()
	m := &RuntimeManager{cfg: config.WorkerConfig{AllowedWorkspaceRoots: []string{allowed}}, stats: rolloutStatsCache{store: store, namespace: "test"}}
	wire, _ := json.Marshal(map[string]string{"path": path})
	threads := []codexadapter.Thread{
		{ID: "visible", CWD: allowed, Source: "cli", Raw: wire},
		{ID: "child", CWD: allowed, Source: "cli", ParentThreadID: "visible", Raw: wire},
		{ID: "hidden", CWD: allowed, Source: "unknown", Raw: wire},
		{ID: "ephemeral", CWD: allowed, Ephemeral: true, Raw: wire},
		{ID: "outside", CWD: t.TempDir(), Source: "cli", Raw: wire},
		{ID: "missing-cwd", Source: "cli", Raw: wire},
	}
	m.discoverTimestampIndex(client, threads)
	if got := store.loadRolloutTimestampPath("test", home, "visible"); got != path {
		t.Fatalf("visible rollout mapping = %q", got)
	}
	for _, thread := range threads[1:] {
		if got := store.loadRolloutTimestampPath("test", home, thread.ID); got != "" {
			t.Errorf("ineligible %s entered timestamp inventory", thread.ID)
		}
	}
}

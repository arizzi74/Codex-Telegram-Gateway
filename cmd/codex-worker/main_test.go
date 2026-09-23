package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/config"
)

func TestExportWorkerConfigNormalizesRelativePaths(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "source")
	workspace := filepath.Join(configDir, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(configDir, "token")
	if err := os.WriteFile(token, []byte("cwk_test"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "worker.json")
	raw := map[string]any{
		"worker_id":               uuid.NewString(),
		"name":                    "relative-paths",
		"state_file":              "state/worker.db",
		"gateway_url":             "wss://gateway.example.test/api/v1/workers/connect",
		"token_file":              "token",
		"allowed_workspace_roots": []string{"workspace"},
		"runtimes": []map[string]any{{
			"id": "main", "codex_binary": "bin/codex", "working_directory": "workspace", "autostart": true, "restart_policy": "on-failure",
		}},
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	exported, err := exportWorkerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.WorkerConfig
	if err := json.Unmarshal(exported, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.StateFile != filepath.Join(configDir, "state", "worker.db") || cfg.TokenFile != token {
		t.Fatalf("normalized files = state %q token %q", cfg.StateFile, cfg.TokenFile)
	}
	if cfg.GatewayURL != "wss://gateway.example.test/tgw/api/v1/workers/connect" {
		t.Fatalf("legacy gateway URL was not migrated: %q", cfg.GatewayURL)
	}
	if len(cfg.AllowedWorkspaceRoots) != 1 || cfg.AllowedWorkspaceRoots[0] != workspace {
		t.Fatalf("normalized roots = %#v", cfg.AllowedWorkspaceRoots)
	}
	if len(cfg.Runtimes) != 1 || cfg.Runtimes[0].WorkingDirectory != workspace || cfg.Runtimes[0].CodexBinary != filepath.Join(configDir, "bin", "codex") {
		t.Fatalf("normalized runtime = %#v", cfg.Runtimes)
	}
}

func TestDefaultConfigPathUsesHomeDotConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := defaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".config", "codex-worker", "config.json"); path != want {
		t.Fatalf("default config path = %q, want %q", path, want)
	}
}

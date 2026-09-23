package releasemanager

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

// Exercise the same owned app-server/socket/proxy handshake used by workers,
// with no production settings, credentials, session history or model requests.
func (m *Manager) preflightCodexRuntime(ctx context.Context, distribution *codexDistribution) error {
	if m.codexPreflight != nil {
		return m.codexPreflight(ctx, distribution)
	}
	dir, err := os.MkdirTemp("", "codex-preflight-")
	if err != nil {
		return errors.New("cannot create isolated Codex compatibility check")
	}
	defer os.RemoveAll(dir)
	config := `model_provider = "isolated"
model = "compatibility-check"
check_for_update_on_startup = false
[model_providers.isolated]
name = "Offline compatibility check"
base_url = "http://127.0.0.1:1"
wire_api = "responses"
requires_openai_auth = false
`
	if err = os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); err != nil {
		return errors.New("cannot configure isolated Codex compatibility check")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	runtime, err := codexadapter.StartShared(ctx, codexadapter.Config{Command: distribution.Binary, WorkingDirectory: dir, Env: codexPreflightEnvironment(dir), Stderr: io.Discard, RequestTimeout: 10 * time.Second}, filepath.Join(dir, "server.sock"))
	if err != nil {
		return errors.New("candidate Codex runtime failed the isolated app-server compatibility check")
	}
	defer runtime.Close()
	loaded, err := runtime.LoadedThreads(ctx, "", 1)
	if err != nil || len(loaded.Threads) != 0 || loaded.NextCursor != "" {
		return errors.New("candidate Codex runtime failed the isolated protocol compatibility check")
	}
	if err = runtime.Close(); err != nil {
		return errors.New("candidate Codex compatibility check did not shut down cleanly")
	}
	return nil
}

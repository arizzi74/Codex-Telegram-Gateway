package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
)

// attachmentSocketPath is independent of the app-server PID, generation and
// private backend directory. Native Codex TUIs can reconnect to this same
// endpoint after a worker update and perform their own initialize/resume flow.
// The gateway never replays their RPCs or resubmits their prompts.
func attachmentSocketPath(stateFile, profileID string) (string, error) {
	if !filepath.IsAbs(stateFile) || profileID == "" {
		return "", errors.New("runtime attachment: absolute state file and profile ID are required")
	}
	stateFile = filepath.Clean(stateFile)
	digest := sha256.Sum256([]byte(stateFile + "\x00" + profileID))
	path := filepath.Join(filepath.Dir(stateFile), "runtimes", fmt.Sprintf("attach-%x.sock", digest[:8]))
	if len(path) > 100 {
		return "", errors.New("runtime attachment: state directory path is too long for a private socket")
	}
	return path, nil
}

// openAttachment runs under the runtime lifecycle lock with the worker's state
// database exclusively held. Join the previous proxy before reclaiming its
// name, so a late close cannot unlink the replacement listener.
func (m *RuntimeManager) openAttachment(runtime protocol.Runtime, backend string) (*attachmentProxy, string, error) {
	path, err := attachmentSocketPath(m.cfg.StateFile, runtime.ProfileID)
	if err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	previous := m.attachments[runtime.ID]
	delete(m.attachments, runtime.ID)
	m.mu.Unlock()
	if previous != nil {
		previous.close()
	}
	if err := prepareAttachmentSocket(path); err != nil {
		return nil, "", err
	}
	proxy, err := newAttachmentProxy(path, backend)
	if err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	m.attachments[runtime.ID] = proxy
	m.mu.Unlock()
	return proxy, path, nil
}

func prepareAttachmentSocket(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("runtime attachment: create socket directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("runtime attachment: inspect socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("runtime attachment: socket directory must be a private directory (0700)")
	}
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("runtime attachment: inspect socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("runtime attachment: existing path is not a socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err == nil {
		_ = connection.Close()
		return errors.New("runtime attachment: refusing to replace an active socket")
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return errors.New("runtime attachment: cannot verify that the existing socket is stale")
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("runtime attachment: inspect stale socket: %w", err)
	}
	if !os.SameFile(info, current) || current.Mode()&os.ModeSocket == 0 {
		return errors.New("runtime attachment: socket changed while checking it")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("runtime attachment: remove stale socket: %w", err)
	}
	return nil
}

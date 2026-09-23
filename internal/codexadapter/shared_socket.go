package codexadapter

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Codex 0.156 publishes a rendezvous symlink into a reserved, private socket
// directory. Its target is SHA256(canonical parent + advertised filename).
// Keep this narrow contract rather than allowing arbitrary socket symlinks:
// https://github.com/openai/codex/blob/rust-v0.156.0/codex-rs/app-server-transport/src/transport/unix_socket.rs
// https://github.com/openai/codex/blob/rust-v0.156.0/codex-rs/uds/src/daemon_directory.rs
func sharedDaemonSocketDirectory() (string, error) {
	// Codex deliberately ignores TMPDIR. macOS aliases /tmp to /private/tmp.
	root, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return "", fmt.Errorf("resolve app-server socket root: %w", err)
	}
	return filepath.Join(root, fmt.Sprintf("codex-daemon-%d", os.Geteuid())), nil
}

func protectedSocketPath(path, directory string) (string, error) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(filepath.Join(parent, filepath.Base(path))))
	return filepath.Join(directory, fmt.Sprintf("%x", digest)), nil
}

func socketOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

type sharedSocket struct {
	path, endpoint          string
	aliasInfo, endpointInfo os.FileInfo
	directory               string
	directoryInfo           os.FileInfo
}

// An older server may publish its direct socket a moment before chmod(0600).
// Wait for the server to restrict it; never chmod through an untrusted alias.
var errSocketPermissionsPending = errors.New("app-server socket permissions are not yet private (0600)")

func inspectSharedSocket(path, daemonDirectory string) (*sharedSocket, error) {
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !parent.IsDir() || parent.Mode().Perm() != 0o700 || !socketOwnedByCurrentUser(parent) {
		return nil, errors.New("app-server listener directory is not user-owned and private (0700)")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !socketOwnedByCurrentUser(info) {
		return nil, errors.New("app-server listener path is not owned by the current user")
	}
	socket := &sharedSocket{path: path, endpoint: path, aliasInfo: info, endpointInfo: info, directory: filepath.Dir(path), directoryInfo: parent}
	if info.Mode()&os.ModeSymlink != 0 {
		expected, err := protectedSocketPath(path, daemonDirectory)
		if err != nil {
			return nil, err
		}
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		if target != expected {
			return nil, errors.New("app-server listener symlink does not match its protected socket path")
		}
		directoryInfo, err := os.Lstat(daemonDirectory)
		if err != nil {
			return nil, err
		}
		if !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o700 || !socketOwnedByCurrentUser(directoryInfo) {
			return nil, errors.New("app-server protected socket directory is not user-owned and private (0700)")
		}
		physical, err := os.Lstat(target)
		if err != nil {
			// A valid alias can briefly be dangling during startup. The startup
			// deadline and child-exit watcher bound retries of ENOENT.
			return nil, err
		}
		socket.endpoint, socket.endpointInfo = target, physical
		socket.directory, socket.directoryInfo = daemonDirectory, directoryInfo
	}
	if socket.endpointInfo.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("app-server listener target is not a Unix socket")
	}
	if !socketOwnedByCurrentUser(socket.endpointInfo) {
		return nil, errors.New("app-server listener target is not owned by the current user")
	}
	if socket.endpointInfo.Mode().Perm() != 0o600 {
		return nil, errSocketPermissionsPending
	}
	current, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, current) {
		return nil, errors.New("app-server listener changed during validation")
	}
	return socket, nil
}

// Only unlink the objects captured after startup validation. A replacement
// endpoint (or a replacement private directory) belongs to someone else.
func (s *sharedSocket) remove() error {
	directoryInfo, err := os.Lstat(s.directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && !os.SameFile(s.directoryInfo, directoryInfo) {
		return errors.New("app-server socket directory changed before cleanup")
	}
	var result error
	if err == nil {
		result = removeCapturedSocket(s.endpoint, s.endpointInfo)
	}
	if s.path != s.endpoint {
		result = errors.Join(result, removeCapturedSocket(s.path, s.aliasInfo))
	}
	return result
}

func removeCapturedSocket(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(expected, current) {
		return errors.New("app-server listener changed before cleanup")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove shared app-server socket: %w", err)
	}
	return nil
}

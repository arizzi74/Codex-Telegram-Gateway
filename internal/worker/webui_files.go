package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

const (
	maxWebUIFileTransfers = 2
	webUIFileIdleTimeout  = 30 * time.Second
	webUIFileLifetime     = 5 * time.Minute
)

type webUIFileTransfer struct {
	pool    *webUIRelays
	id      string
	target  protocol.WebUIFrame
	ctx     context.Context
	cancel  context.CancelFunc
	input   chan protocol.WebUIFileRead
	offset  int64 // protected by pool.mu
	pending bool  // one outstanding credit, protected by pool.mu
}

// File bytes must bypass text redaction: transforming base64 would silently
// corrupt a download. Neither file frames nor their payloads are logged.
func (p *webUIRelays) sendFile(ctx context.Context, frame protocol.WebUIFrame) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return sendEnvelope(sendCtx, p.writes, "webui", frame)
}

func (p *webUIRelays) failFile(id, message string) {
	_ = p.sendFile(p.ctx, protocol.WebUIFrame{ID: id, Action: "error", Error: message})
}

func (p *webUIRelays) cancelFile(id string) {
	p.mu.Lock()
	if transfer := p.files[id]; transfer != nil {
		transfer.cancel()
	}
	p.mu.Unlock()
}

func (p *webUIRelays) openFile(frame protocol.WebUIFrame) {
	p.mu.Lock()
	if p.clients[frame.ID] != nil || p.files[frame.ID] != nil || len(p.files) >= maxWebUIFileTransfers || len(p.clients)+len(p.files) >= maxWebUIConnections {
		p.mu.Unlock()
		p.failFile(frame.ID, "Too many file downloads or connection already open.")
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, webUIFileLifetime)
	transfer := &webUIFileTransfer{pool: p, id: frame.ID, target: frame, ctx: ctx, cancel: cancel, input: make(chan protocol.WebUIFileRead, 1)}
	p.files[frame.ID] = transfer
	p.wg.Add(1)
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		defer func() {
			transfer.cancel()
			p.mu.Lock()
			delete(p.files, frame.ID)
			p.mu.Unlock()
		}()
		if err := transfer.run(); err != nil && transfer.ctx.Err() == nil {
			// Every failure string in this path is static and safe to expose.
			p.failFile(transfer.id, err.Error())
		}
	}()
}

func (p *webUIRelays) receiveFile(frame protocol.WebUIFrame) bool {
	p.mu.Lock()
	transfer := p.files[frame.ID]
	p.mu.Unlock()
	if transfer == nil {
		return false
	}
	if frame.Action == "close" {
		transfer.cancel()
		return true
	}
	request, err := protocol.DecodeWebUIFileRead(frame.Data)
	p.mu.Lock()
	if err != nil || transfer.ctx.Err() != nil || transfer.pending || request.Offset != transfer.offset {
		p.mu.Unlock()
		transfer.cancel()
		p.failFile(frame.ID, "The file download offset is invalid or a read is already pending.")
		return true
	}
	transfer.pending = true
	p.mu.Unlock()
	select {
	case transfer.input <- request:
	case <-transfer.ctx.Done():
	default:
		transfer.cancel()
		p.failFile(frame.ID, "The file download exceeded the read limit.")
	}
	return true
}

func (transfer *webUIFileTransfer) run() error {
	p := transfer.pool
	runtime, session, err := p.target(transfer.target)
	if err != nil {
		return errors.New("The file's session or runtime is unavailable. Refresh the session list.")
	}
	cwd, err := auth.CanonicalWorkspace(session.CWD, p.c.cfg.AllowedWorkspaceRoots)
	if err != nil {
		return errors.New("The file's session workspace is unavailable.")
	}
	request, err := protocol.DecodeWebUIFileRequest(transfer.target.Data)
	if err != nil {
		return errors.New("The file path is invalid.")
	}
	home, _ := os.UserHomeDir()
	file, info, err := openWebUIFile(cwd, home, request.Path, p.c.cfg.AllowedWorkspaceRoots, []string{p.c.cfg.TokenFile, p.c.cfg.StateFile})
	if err != nil {
		return err
	}
	defer file.Close()
	initial, err := file.Stat()
	if err != nil || initial.Size() != info.Size || !singleLinkedWebUIFile(initial) {
		return errors.New("The worker cannot read this file.")
	}
	// The session may be retargeted while the filesystem open is in progress.
	if err := transfer.fence(runtime, session, cwd); err != nil {
		return err
	}
	metadata, _ := json.Marshal(info)
	if err := p.sendFile(transfer.ctx, protocol.WebUIFrame{ID: transfer.id, Action: "ready", Data: metadata}); err != nil {
		return errors.New("The file download connection ended.")
	}
	timer := time.NewTimer(webUIFileIdleTimeout)
	defer timer.Stop()
	for {
		var credit protocol.WebUIFileRead
		select {
		case <-transfer.ctx.Done():
			return transfer.ctx.Err()
		case <-timer.C:
			return errors.New("The file download expired. Open the file again.")
		case credit = <-transfer.input:
		}
		if err := transfer.fence(runtime, session, cwd); err != nil {
			return err
		}
		if !unchangedWebUIFile(file, initial) {
			return errors.New("The file changed during the download. Open it again.")
		}
		remaining := info.Size - credit.Offset
		if remaining < 0 || remaining > protocol.MaxWebUIFileBytes {
			return errors.New("The file download offset is invalid.")
		}
		data := make([]byte, min(int64(protocol.WebUIFileChunkBytes), remaining))
		if _, err := io.ReadFull(file, data); err != nil || !unchangedWebUIFile(file, initial) {
			return errors.New("The file changed during the download. Open it again.")
		}
		next := credit.Offset + int64(len(data))
		chunk := protocol.WebUIFileChunk{Offset: credit.Offset, Data: data, EOF: next == info.Size}
		encoded, _ := json.Marshal(chunk)
		p.mu.Lock()
		transfer.offset = next
		transfer.pending = false
		p.mu.Unlock()
		if err := p.sendFile(transfer.ctx, protocol.WebUIFrame{ID: transfer.id, Action: "output", Data: encoded}); err != nil {
			return errors.New("The file download connection ended.")
		}
		if chunk.EOF {
			return nil
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(webUIFileIdleTimeout)
	}
}

func (transfer *webUIFileTransfer) fence(runtime protocol.Runtime, session protocol.Session, cwd string) error {
	currentRuntime, currentSession, err := transfer.pool.target(transfer.target)
	if err != nil || currentRuntime.ID != runtime.ID || currentRuntime.Generation != runtime.Generation || currentSession.ThreadID != session.ThreadID || currentSession.CWD != session.CWD {
		return errors.New("The file's session or runtime changed. Refresh the session list.")
	}
	currentCWD, err := auth.CanonicalWorkspace(currentSession.CWD, transfer.pool.c.cfg.AllowedWorkspaceRoots)
	if err != nil || currentCWD != cwd {
		return errors.New("The file's session workspace changed. Open it again.")
	}
	return nil
}

func unchangedWebUIFile(file *os.File, before os.FileInfo) bool {
	after, err := file.Stat()
	return err == nil && after.Mode().IsRegular() && singleLinkedWebUIFile(after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func singleLinkedWebUIFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

// All path components are opened relative to held directory descriptors and
// O_NOFOLLOW rejects symlinks atomically. This also prevents aliases into
// credential directories when a session uses a broad workspace such as home.
func openWebUIFile(cwd, home, rawPath string, roots, privatePaths []string) (*os.File, protocol.WebUIFileInfo, error) {
	var metadata protocol.WebUIFileInfo
	path, err := protocol.NormalizeWebUIFilePath(rawPath)
	if err != nil {
		return nil, metadata, errors.New("The file path is invalid.")
	}
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		if component == ".." {
			return nil, metadata, errors.New("Parent directory traversal is not allowed.")
		}
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home == "" {
			return nil, metadata, errors.New("The worker home directory is unavailable.")
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	relative, err := filepath.Rel(cwd, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, metadata, errors.New("The file is outside this session's workspace.")
	}
	if protectedWebUIFilePath(path, privatePaths) {
		return nil, metadata, errors.New("Worker credentials and configuration files cannot be downloaded.")
	}
	directory, err := openWebUIFileDirectory(cwd, roots)
	if err != nil {
		return nil, metadata, errors.New("The worker cannot open this session's workspace.")
	}
	defer func() { directory.Close() }()
	components := strings.Split(relative, string(filepath.Separator))
	for index, component := range components {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if index < len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(int(directory.Fd()), component, flags, 0)
		if err != nil {
			return nil, metadata, errors.New("The file is unavailable or uses a symbolic link.")
		}
		opened := os.NewFile(uintptr(fd), component)
		if index < len(components)-1 {
			// Keep only the next directory open; each child is already anchored.
			directory.Close()
			directory = opened
			continue
		}
		stat, err := opened.Stat()
		if err != nil || !stat.Mode().IsRegular() || stat.Size() < 0 || stat.Size() > protocol.MaxWebUIFileBytes {
			opened.Close()
			return nil, metadata, errors.New("Only regular files up to 128 MiB can be downloaded.")
		}
		// Hard links can disguise credentials under an innocent artifact name.
		if !singleLinkedWebUIFile(stat) {
			opened.Close()
			return nil, metadata, errors.New("Files with multiple hard links cannot be downloaded.")
		}
		metadata = protocol.WebUIFileInfo{Name: filepath.Base(path), Path: path, Size: stat.Size()}
		return opened, metadata, nil
	}
	return nil, metadata, errors.New("The file path is invalid.")
}

// Validate membership, then walk even the configured root's own components
// without following symlinks. Opening a canonical workspace path directly can
// still follow a symlink swapped in after canonicalization, including one
// pointing at another directory inside the allowed root.
func openWebUIFileDirectory(cwd string, roots []string) (*os.File, error) {
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return nil, errors.New("invalid canonical workspace")
	}
	canonicalRoots, err := auth.CanonicalWorkspaceRoots(roots)
	if err != nil {
		return nil, err
	}
	allowed := false
	for _, root := range canonicalRoots {
		relative, err := filepath.Rel(root, cwd)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, errors.New("workspace is outside allowed roots")
	}
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	directory, err := root.Open(".")
	root.Close()
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(cwd, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		fd, err := unix.Openat(int(directory.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
		directory.Close()
		if err != nil {
			return nil, err
		}
		directory = os.NewFile(uintptr(fd), component)
	}
	return directory, nil
}

func protectedWebUIFilePath(path string, privatePaths []string) bool {
	for _, component := range strings.Split(path, string(filepath.Separator)) {
		component = strings.ToLower(component)
		switch component {
		case ".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".codex", ".config", ".telegramgw", ".git", ".netrc", ".npmrc", ".pypirc", ".git-credentials", ".gitconfig", ".bash_history", ".zsh_history", "keyrings":
			return true
		}
		if component == ".env" || strings.HasPrefix(component, ".env.") {
			return true
		}
	}
	for _, protected := range []string{"/etc", "/proc", "/sys", "/dev", "/run"} {
		if path == protected || strings.HasPrefix(path, protected+string(filepath.Separator)) {
			return true
		}
	}
	for _, private := range privatePaths {
		if private == "" {
			continue
		}
		absolute, err := filepath.Abs(private)
		if err != nil {
			continue
		}
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if path == absolute+suffix {
				return true
			}
		}
	}
	return false
}

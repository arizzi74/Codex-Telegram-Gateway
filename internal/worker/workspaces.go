package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
)

// defaultSessionParent uses the worker user's home, independently of the
// runtime's initial cwd. Explicitly restricted workers keep their root limits.
func defaultSessionParent(home string, roots []string) (string, error) {
	for _, path := range append([]string{filepath.Join(home, "CODEX"), home}, roots...) {
		if path == "" {
			continue
		}
		if canonical, err := auth.CanonicalWorkspace(path, roots); err == nil {
			return canonical, nil
		}
	}
	return "", workspaceError("No accessible workspace root is available.")
}

func browseWorkspace(home string, roots []string, request *protocol.WorkspaceRequest) (*protocol.WorkspacePage, error) {
	if err := request.Validate(); err != nil {
		return nil, workspaceError("The folder browser request is invalid.")
	}
	path := request.Path
	var err error
	if path == "" {
		path, err = defaultSessionParent(home, roots)
	} else if !filepath.IsAbs(path) {
		return nil, workspaceError("Select an absolute folder path from the browser.")
	}
	if err != nil {
		return nil, err
	}
	path, err = auth.CanonicalWorkspace(path, roots)
	if err != nil {
		return nil, workspaceError("This folder is unavailable or outside the worker's allowed workspace roots.")
	}
	root, err := openWorkspaceRoot(path, roots)
	if err != nil {
		return nil, workspaceError("The worker cannot open this folder. Choose another folder.")
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return nil, workspaceError("The worker cannot read this folder. Choose another folder.")
	}
	entries, readErr := directory.ReadDir(-1)
	directory.Close()
	if readErr != nil {
		return nil, workspaceError("The worker cannot read this folder. Choose another folder.")
	}
	page := &protocol.WorkspacePage{Path: path, Directories: []protocol.WorkspaceEntry{}, Offset: request.Offset}
	parent := filepath.Dir(path)
	if parent != path {
		if parent, err = auth.CanonicalWorkspace(parent, roots); err == nil {
			page.Parent = parent
		}
	}
	var directories []protocol.WorkspaceEntry
	for _, entry := range entries {
		if !utf8.ValidString(entry.Name()) {
			continue
		}
		// Resolve each candidate as well: a symlink in an otherwise allowed
		// directory must not expose another part of the worker filesystem.
		candidate, err := auth.CanonicalWorkspace(filepath.Join(path, entry.Name()), roots)
		if err != nil || len(candidate) > 4096 {
			continue
		}
		directories = append(directories, protocol.WorkspaceEntry{Name: entry.Name(), Path: candidate})
	}
	sort.Slice(directories, func(i, j int) bool { return directories[i].Name < directories[j].Name })
	start := min(request.Offset, len(directories))
	end := min(start+protocol.WorkspacePageSize, len(directories))
	page.Directories = append(page.Directories, directories[start:end]...)
	page.HasMore = end < len(directories)
	return page, nil
}

// createSessionWorkspace creates exactly one child and never reuses or removes
// an existing path. The caller records execution before invoking this mutation.
func createSessionWorkspace(parent, name string, roots []string) (string, error) {
	childName, err := protocol.SessionDirectoryName(name)
	if err != nil {
		return "", workspaceError(err.Error())
	}
	parent, err = auth.CanonicalWorkspace(parent, roots)
	if err != nil {
		return "", workspaceError("The selected parent folder is unavailable or outside the worker's allowed workspace roots.")
	}
	root, err := openWorkspaceRoot(parent, roots)
	if err != nil {
		return "", workspaceError("The worker cannot open the selected parent folder.")
	}
	defer root.Close()
	if err := root.Mkdir(childName, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", workspaceError(fmt.Sprintf("A file or folder named %q already exists here. Choose another session name or parent folder.", childName))
		}
		return "", workspaceError("The worker cannot create a folder here. Choose a writable parent folder.")
	}
	child := filepath.Join(parent, childName)
	canonical, err := auth.CanonicalWorkspace(child, roots)
	if err != nil || canonical != child {
		return "", workspaceError("The selected folder changed while creating the workspace. Inspect it before retrying.")
	}
	return canonical, nil
}

func workspaceError(message string) error {
	return &protocol.Error{Code: protocol.InvalidWorkspace, Message: message}
}

// Resolve descendants through an allowed root handle, so replacing a parent
// with a symlink between validation and open cannot redirect a mkdir outside
// the configured root. os.Root confines all relative lookups to that handle.
func openWorkspaceRoot(path string, roots []string) (*os.Root, error) {
	canonicalRoots, err := auth.CanonicalWorkspaceRoots(roots)
	if err != nil {
		return nil, err
	}
	for _, allowed := range canonicalRoots {
		relative, err := filepath.Rel(allowed, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			continue
		}
		root, err := os.OpenRoot(allowed)
		if err != nil {
			return nil, err
		}
		workspace, err := root.OpenRoot(relative)
		root.Close()
		return workspace, err
	}
	return nil, errors.New("workspace is outside allowed roots")
}

func (a *Agent) browseWorkspace(c protocol.Command) (protocol.CommandAck, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return a.reject(c, protocol.InvalidWorkspace, "The worker user's home directory is unavailable.")
	}
	page, err := browseWorkspace(home, a.cfg.AllowedWorkspaceRoots, c.Arguments.Workspace)
	if err != nil {
		var failure *protocol.Error
		if errors.As(err, &failure) {
			return a.reject(c, failure.Code, failure.Message)
		}
		return a.reject(c, protocol.InvalidWorkspace, "The selected folder is unavailable.")
	}
	_, err = a.record(c, CommandCompleted, &protocol.Result{CommandID: c.ID, State: "completed", Workspace: page}, "command_completed")
	return protocol.CommandAck{CommandID: c.ID, Status: "accepted"}, err
}

package protocol

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const WorkspacePageSize = 12
const MaxSessionNameBytes = 120

type WorkspaceRequest struct {
	Path   string `json:"path,omitempty"`
	Offset int    `json:"offset,omitempty"`
}

func (w *WorkspaceRequest) Validate() error {
	if w == nil || w.Offset < 0 || w.Offset > 1_000_000 || len(w.Path) > 4096 || strings.ContainsRune(w.Path, 0) || !utf8.ValidString(w.Path) {
		return errors.New("invalid workspace browser request")
	}
	return nil
}

type WorkspaceEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type WorkspacePage struct {
	Path        string           `json:"path"`
	Parent      string           `json:"parent,omitempty"`
	Directories []WorkspaceEntry `json:"directories"`
	Offset      int              `json:"offset"`
	HasMore     bool             `json:"has_more"`
}

// NormalizeSessionName preserves the display name while ensuring that it can
// also name one new directory. Controls and separators cannot become paths.
func NormalizeSessionName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", errors.New("Session name must contain valid text.")
	}
	for _, ch := range name {
		if ch == '/' || ch == '\\' || unicode.IsControl(ch) {
			return "", errors.New("Session name cannot contain slashes or control characters.")
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || len(name) > MaxSessionNameBytes {
		return "", errors.New("Session name must contain 1–120 UTF-8 bytes and cannot be a single or double dot.")
	}
	return name, nil
}

func SessionDirectoryName(name string) (string, error) {
	name, err := NormalizeSessionName(name)
	if err != nil {
		return "", err
	}
	return strings.Map(func(ch rune) rune {
		if unicode.IsSpace(ch) {
			return '_'
		}
		return ch
	}, name), nil
}

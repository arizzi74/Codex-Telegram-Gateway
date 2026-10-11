package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxWebUIFileBytes   = 128 << 20
	WebUIFileChunkBytes = 64 << 10
	MaxWebUIFilePath    = 4096
)

// File transfers use connection-local WebUIFrames and never enter an outbox.
// A ready frame contains WebUIFileInfo. Each input credits exactly one chunk at
// the next offset; output contains WebUIFileChunk, including EOF for empty files.
type WebUIFileRequest struct {
	Path string `json:"path"`
}

type WebUIFileInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type WebUIFileRead struct {
	Offset int64 `json:"offset"`
}

type WebUIFileChunk struct {
	Offset int64  `json:"offset"`
	Data   []byte `json:"data"`
	EOF    bool   `json:"eof"`
}

func DecodeWebUIFileRequest(data []byte) (WebUIFileRequest, error) {
	var request WebUIFileRequest
	if len(data) == 0 || len(data) > 6*MaxWebUIFilePath+64 || decodeWebUIFileJSON(data, &request) != nil {
		return request, errors.New("invalid web UI file request")
	}
	if _, err := NormalizeWebUIFilePath(request.Path); err != nil {
		return request, err
	}
	return request, nil
}

func DecodeWebUIFileRead(data []byte) (WebUIFileRead, error) {
	var request struct {
		Offset *int64 `json:"offset"`
	}
	if len(data) == 0 || len(data) > 128 || decodeWebUIFileJSON(data, &request) != nil || request.Offset == nil || *request.Offset < 0 || *request.Offset > MaxWebUIFileBytes {
		return WebUIFileRead{}, errors.New("invalid web UI file offset")
	}
	return WebUIFileRead{Offset: *request.Offset}, nil
}

func decodeWebUIFileJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing file request data")
	}
	return nil
}

var webUIFileLineAnchor = regexp.MustCompile(`(?:#L[0-9]+(?:C[0-9]+)?(?:-L?[0-9]+(?:C[0-9]+)?)?|:[0-9]+(?::[0-9]+)?)$`)

// NormalizeWebUIFilePath removes editor line anchors and accepts only local
// file URLs. Plain filesystem paths are never URL-decoded. Home expansion and
// workspace confinement belong to the worker that owns the file.
func NormalizeWebUIFilePath(path string) (string, error) {
	if !validWebUIFilePath(path) {
		return "", errors.New("invalid web UI file path")
	}
	if strings.HasPrefix(strings.ToLower(path), "file:") {
		uri, err := url.Parse(path)
		if err != nil || !strings.EqualFold(uri.Scheme, "file") || uri.Opaque != "" || uri.User != nil || (uri.Host != "" && !strings.EqualFold(uri.Host, "localhost")) || uri.RawQuery != "" || uri.ForceQuery || !strings.HasPrefix(uri.Path, "/") {
			return "", errors.New("invalid local file URL")
		}
		if uri.Fragment != "" && !webUIFileLineAnchor.MatchString("#"+uri.Fragment) {
			return "", errors.New("invalid local file URL anchor")
		}
		path = uri.Path
	} else {
		path = webUIFileLineAnchor.ReplaceAllString(path, "")
	}
	if !validWebUIFilePath(path) || strings.Contains(path, "://") {
		return "", errors.New("invalid web UI file path")
	}
	return path, nil
}

func validWebUIFilePath(path string) bool {
	if path == "" || len(path) > MaxWebUIFilePath || strings.TrimSpace(path) == "" || !utf8.ValidString(path) || strings.Contains(path, `\`) {
		return false
	}
	for _, ch := range path {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}

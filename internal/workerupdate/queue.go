// Package workerupdate shares durable update requests between the worker and an
// independently supervised updater. It never opens the running worker database.
package workerupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func Directory(stateFile string) string { return stateFile + ".updates" }

func requestPath(stateFile, id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id || !filepath.IsAbs(stateFile) {
		return "", errors.New("invalid worker update request path")
	}
	return filepath.Join(Directory(stateFile), id+".json"), nil
}

// Create is idempotent and uses an atomic link so a crash cannot expose a
// partially written request and a duplicate cannot replace an existing one.
func Create(stateFile string, request protocol.WorkerUpdateRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	path, err := requestPath(stateFile, request.RequestID)
	if err != nil {
		return err
	}
	if _, err := uuid.Parse(request.WorkerID); err != nil {
		return errors.New("invalid worker update identity")
	}
	if old, err := Read(stateFile, request.RequestID); err == nil {
		if old != request {
			return errors.New("worker update request identity changed")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if err = atomicWrite(path, data, false); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	old, err := Read(stateFile, request.RequestID)
	if err == nil && old != request {
		return errors.New("worker update request identity changed")
	}
	return err
}

func Read(stateFile, id string) (protocol.WorkerUpdateRequest, error) {
	var request protocol.WorkerUpdateRequest
	path, err := requestPath(stateFile, id)
	if err != nil {
		return request, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return request, err
	}
	err = json.Unmarshal(data, &request)
	if err == nil {
		err = request.Validate()
	}
	if err == nil && request.RequestID != id {
		err = errors.New("worker update request identity mismatch")
	}
	return request, err
}

func List(stateFile string) ([]protocol.WorkerUpdateRequest, error) {
	entries, err := os.ReadDir(Directory(stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var requests []protocol.WorkerUpdateRequest
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".result.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if _, err := uuid.Parse(id); err != nil {
			continue
		}
		request, err := Read(stateFile, id)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, nil
}

func Result(stateFile, id string) (protocol.WorkerUpdateResult, error) {
	var result protocol.WorkerUpdateResult
	path, err := requestPath(stateFile, id)
	if err != nil {
		return result, err
	}
	data, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".result.json")
	if err != nil {
		return result, err
	}
	err = json.Unmarshal(data, &result)
	if err == nil {
		err = result.Validate()
	}
	if err == nil && result.RequestID != id {
		err = errors.New("worker update result identity mismatch")
	}
	return result, err
}

func Complete(stateFile string, result protocol.WorkerUpdateResult) error {
	if err := result.Validate(); err != nil {
		return err
	}
	path, err := requestPath(stateFile, result.RequestID)
	if err != nil {
		return err
	}
	if _, err := Read(stateFile, result.RequestID); err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	err = atomicWrite(strings.TrimSuffix(path, ".json")+".result.json", data, false)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

// WritePrivate atomically replaces a private supervisor plist.
func WritePrivate(path string, data []byte) error { return atomicWrite(path, data, true) }

func atomicWrite(path string, data []byte, replace bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".update-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if replace {
		err = os.Rename(f.Name(), path)
	} else {
		err = os.Link(f.Name(), path)
	}
	if err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

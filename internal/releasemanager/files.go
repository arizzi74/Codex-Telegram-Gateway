package releasemanager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func FileExists(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, os.ErrNotExist)
}

func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// AtomicWrite never truncates an executable currently running or follows a
// destination symlink. Existing permissions and ownership survive replacement.
func AtomicWrite(path string, data []byte, mode os.FileMode, owner *Ownership) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	old, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if !old.Mode().IsRegular() {
			return errors.New("refusing to replace a non-regular file")
		}
		mode = old.Mode().Perm()
		if stat, ok := old.Sys().(*syscall.Stat_t); ok {
			owner = &Ownership{UID: int(stat.Uid), GID: int(stat.Gid)}
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer os.Remove(temporary)
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if owner != nil && os.Geteuid() == 0 {
		if err = f.Chown(owner.UID, owner.GID); err != nil {
			return err
		}
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}

func AtomicCopy(source, destination string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return AtomicWrite(destination, data, mode, nil)
}

func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(path, append(data, '\n'), 0600, nil)
}

func ReadJSON(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4*1024*1024 {
		return nil, errors.New("JSON configuration exceeds size limit")
	}
	var value map[string]any
	if err = json.Unmarshal(data, &value); err != nil || value == nil {
		return nil, errors.New("expected a JSON configuration object")
	}
	return value, nil
}

func Lock(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("update lock must be a regular file")
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, &BusyError{Reason: "another installation or update is already running"}
		}
		return nil, err
	}
	return f.Close, nil
}

func SavedSettings(l *Layout) (map[string]any, error) {
	v, err := ReadJSON(l.State)
	if err != nil {
		return nil, err
	}
	if v["component"] != l.Component {
		return nil, errors.New("installer state belongs to another component")
	}
	return v, nil
}

func SaveSettings(l *Layout, repo, version string) error {
	return WriteJSON(l.State, map[string]any{"schema": 1, "component": l.Component, "repo": repo, "version": version, "updated_at": time.Now().UTC().Format(time.RFC3339Nano)})
}

func requiredString(value map[string]any, key string) (string, error) {
	s, ok := value[key].(string)
	if !ok || s == "" {
		return "", fmt.Errorf("configuration requires %s", key)
	}
	return s, nil
}

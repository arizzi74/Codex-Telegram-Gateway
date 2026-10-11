package speech

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed licenses/*
var notices embed.FS

// Preserve attribution beside downloaded assets even when an installer keeps
// only the self-contained gateway binary from the distribution archive.
func writeNotices(dir string) error {
	entries, err := notices.ReadDir("licenses")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		data, err := notices.ReadFile("licenses/" + entry.Name())
		if err != nil {
			return err
		}
		if err := writeNotice(dir, entry.Name(), data); err != nil {
			return err
		}
	}
	return nil
}

func writeNotice(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".notice-")
	if err != nil {
		return err
	}
	defer f.Close()
	defer os.Remove(f.Name())
	if err := writeAll(f, data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}

package releasemanager

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// backupSQLiteFromRoot is called only after the gateway has stopped. SQLite
// must never reopen a service-writable absolute path as root. Open the database
// and its recovery files through the retained directory descriptor, copy them
// into a private directory, then use SQLite's backup API on that private copy.
// The WAL is essential: copying the main file alone would lose committed writes.
func backupSQLiteFromRoot(ctx context.Context, database *updateDatabase, backup string) error {
	if err := database.validateSourcePaths(); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(backup), ".sqlite-snapshot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	type source struct {
		name string
		file *os.File
		info os.FileInfo
	}
	var sources []source
	var absent []string
	defer func() {
		for _, src := range sources {
			src.file.Close()
		}
	}()
	for _, suffix := range []string{"", "-wal", "-journal"} {
		name := database.rel + suffix
		// O_NONBLOCK avoids hanging if a regular file is swapped for a FIFO;
		// O_NOFOLLOW rejects final-component symlinks atomically. Root confines
		// all parent traversal, even during directory replacement.
		file, err := database.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			absent = append(absent, name)
			continue
		}
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
			file.Close()
			return errors.New("SQLite snapshot sources must be regular files with no hard links")
		}
		sources = append(sources, source{name: name, file: file, info: info})
	}
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		suffix := src.name[len(database.rel):]
		destination, err := os.OpenFile(filepath.Join(stage, "gateway.db"+suffix), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(destination, src.file, src.info.Size())
		closeErr := destination.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
	}
	// Fail rather than accept a torn snapshot if anything changed while being
	// copied. SQLite also checks recovery state and the final backup integrity.
	for _, src := range sources {
		after, err := src.file.Stat()
		if err != nil {
			return err
		}
		current, err := database.root.Lstat(src.name)
		if err != nil || !os.SameFile(src.info, current) || src.info.Size() != after.Size() || !src.info.ModTime().Equal(after.ModTime()) {
			return errors.New("gateway database changed during its stopped-service snapshot")
		}
	}
	for _, name := range absent {
		if _, err := database.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			return errors.New("gateway database recovery files changed during its stopped-service snapshot")
		}
	}
	if err := database.validateSourcePaths(); err != nil {
		return err
	}
	return sqliteBackup(ctx, filepath.Join(stage, "gateway.db"), backup)
}

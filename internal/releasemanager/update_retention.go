package releasemanager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const retainedUpdateVersions = 3

var updateBackupName = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9]+$`)

type updateBackup struct {
	name     string
	version  [3]uint64
	created  time.Time
	complete bool
	info     os.FileInfo
}

// pruneUpdateBackups must run under the installation lock, only after the new
// service has passed its readiness check and its settings have been saved. An
// unsuccessful update may still need every snapshot for rollback or recovery.
// Unknown entries are preserved and reported; only recognized update snapshots
// are eligible for deletion.
func pruneUpdateBackups(l *Layout, currentVersion string) (removed int, retErr error) {
	current, err := ParseVersion(currentVersion)
	if err != nil {
		return 0, fmt.Errorf("cannot prune update backups: %w", err)
	}
	if l.Component != "worker" && l.Component != "gateway" {
		return 0, errors.New("cannot prune update backups for an unknown component")
	}
	info, err := os.Lstat(l.Backups)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("update backup directory must be a real directory")
	}
	root, err := os.OpenRoot(l.Backups)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	openedInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		return 0, errors.New("update backup directory changed while opening it")
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, err
	}
	var backups []updateBackup
	for _, entry := range entries {
		backup, err := inspectUpdateBackup(root, entry.Name(), l.Component)
		if err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("preserved backup entry %q: %w", entry.Name(), err))
			continue
		}
		if compareUpdateBackupVersions(backup.version, current) > 0 {
			retErr = errors.Join(retErr, fmt.Errorf("preserved backup entry %q: version is newer than installed %s", entry.Name(), currentVersion))
			continue
		}
		backups = append(backups, backup)
	}
	// Prefer the latest copy within each stable version, but never let an
	// incomplete attempt displace a complete snapshot of an older version.
	sort.Slice(backups, func(i, j int) bool {
		if comparison := compareUpdateBackupVersions(backups[i].version, backups[j].version); comparison != 0 {
			return comparison > 0
		}
		if !backups[i].created.Equal(backups[j].created) {
			return backups[i].created.After(backups[j].created)
		}
		return backups[i].name > backups[j].name
	})
	retained := make(map[[3]uint64]bool)
	for _, backup := range backups {
		if backup.version != current && backup.complete && !retained[backup.version] && len(retained) < retainedUpdateVersions {
			retained[backup.version] = true
			continue
		}
		// Confinement also prevents a replaced nested symlink from directing
		// removal outside this tree. Preserve an entry replaced since inspection.
		info, err := root.Lstat(backup.name)
		if err != nil || !os.SameFile(info, backup.info) {
			retErr = errors.Join(retErr, fmt.Errorf("preserved backup entry %q: directory changed before removal", backup.name))
			continue
		}
		if err := root.RemoveAll(backup.name); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove backup entry %q: %w", backup.name, err))
			continue
		}
		removed++
	}
	return removed, retErr
}

func inspectUpdateBackup(root *os.Root, name, component string) (updateBackup, error) {
	var result updateBackup
	if !updateBackupName.MatchString(name) {
		return result, errors.New("unrecognized backup name")
	}
	created, err := time.Parse("20060102T150405Z", name[:16])
	if err != nil {
		return result, errors.New("invalid backup timestamp")
	}
	info, err := root.Lstat(name)
	if err != nil {
		return result, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("backup must be a real directory")
	}
	dir, err := root.OpenRoot(name)
	if err != nil {
		return result, err
	}
	defer dir.Close()
	openedInfo, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, openedInfo) {
		return result, errors.New("backup directory changed while opening it")
	}
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return result, err
	}
	allowed := map[string]bool{
		"00-update.json": true, "01-codex-telegramgw": true,
		"02-codex-" + component: true, "03-codex-telegramgw": true,
	}
	required := []string{"02-codex-" + component, "03-codex-telegramgw"}
	if component == "worker" {
		allowed["04-codex-local"] = true
		required = append(required, "04-codex-local")
	} else {
		allowed["gateway.db"] = true
		// Gateway files are snapshotted before the service stops and its
		// database is backed up. A binary-only attempt is not a full rollback.
		required = append(required, "gateway.db")
	}
	present := make(map[string]bool)
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return result, fmt.Errorf("unrecognized snapshot file %q", entry.Name())
		}
		fileInfo, err := dir.Lstat(entry.Name())
		if err != nil {
			return result, err
		}
		if !fileInfo.Mode().IsRegular() {
			return result, fmt.Errorf("snapshot file %q is not a regular file", entry.Name())
		}
		if entry.Name() == "00-update.json" && fileInfo.Size() > 4*1024*1024 {
			return result, errors.New("snapshot metadata exceeds size limit")
		}
		present[entry.Name()] = fileInfo.Size() > 0
	}
	data, err := dir.ReadFile("00-update.json")
	if err != nil {
		return result, fmt.Errorf("read snapshot metadata: %w", err)
	}
	var metadata struct {
		Schema    int    `json:"schema"`
		Component string `json:"component"`
		Repo      string `json:"repo"`
		Version   string `json:"version"`
		Pending   bool   `json:"pending"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return result, errors.New("invalid snapshot metadata")
	}
	if metadata.Schema != 1 || metadata.Component != component || ValidateRepo(metadata.Repo) != nil {
		return result, errors.New("unrecognized snapshot metadata")
	}
	version, err := ParseVersion(metadata.Version)
	if err != nil || strings.TrimSpace(metadata.Version) != metadata.Version {
		return result, errors.New("invalid snapshot version")
	}
	complete := !metadata.Pending
	for _, file := range required {
		complete = complete && present[file]
	}
	return updateBackup{name: name, version: version, created: created, complete: complete, info: info}, nil
}

func compareUpdateBackupVersions(a, b [3]uint64) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

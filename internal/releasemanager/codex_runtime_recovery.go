package releasemanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// A recovery journal is written before maintenance changes any installation.
// It contains identities, not copies of Codex data or session history.
type codexRecovery struct {
	Schema           int                    `json:"schema"`
	Phase            string                 `json:"phase"`
	Restart          bool                   `json:"restart"`
	ActiveInstall    int                    `json:"active_install"`
	Snapshots        []codexRuntimeSnapshot `json:"snapshots"`
	RejectedVersions []string               `json:"rejected_versions,omitempty"`
}
type codexRuntimeSnapshot struct {
	CandidateReleaseDir string            `json:"candidate_release_dir,omitempty"`
	CandidateObserved   bool              `json:"candidate_observed,omitempty"`
	Distribution        codexDistribution `json:"distribution"`
	ReleaseDir          string            `json:"release_dir"`
	BinarySHA256        string            `json:"binary_sha256"`
	MetadataSHA256      string            `json:"metadata_sha256"`
	Profiles            map[string]bool   `json:"profiles"`
}

func (j *codexRecovery) rejectVersion(version string) {
	if !slices.Contains(j.RejectedVersions, version) {
		j.RejectedVersions = append(j.RejectedVersions, version)
	}
}
func validateCodexRecoverySize(journal *codexRecovery) error {
	if len(journal.Snapshots) == 0 || len(journal.Snapshots) > 32 || len(journal.RejectedVersions) > 64 {
		return errors.New("Codex recovery journal exceeds its supported installation count")
	}
	homes := map[string]bool{}
	profiles := map[string]bool{}
	for _, snapshot := range journal.Snapshots {
		if homes[snapshot.Distribution.Home] || len(snapshot.Profiles) == 0 || len(snapshot.Profiles) > 128 {
			return errors.New("invalid Codex recovery installation set")
		}
		homes[snapshot.Distribution.Home] = true
		for profile := range snapshot.Profiles {
			if profile == "" || len(profile) > 256 || profiles[profile] {
				return errors.New("invalid Codex recovery profile set")
			}
			profiles[profile] = true
		}
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	limit := 60 << 10
	if journal.Phase == "prepared" {
		limit = 56 << 10
	}
	if err != nil || len(data) > limit {
		return errors.New("Codex recovery journal exceeds its safe size limit")
	}
	return nil
}
func writeCodexRecovery(l *Layout, journal *codexRecovery) error {
	if err := validateCodexRecoverySize(journal); err != nil {
		return err
	}
	return WriteJSON(codexRecoveryPath(l), journal)
}
func codexRecoveryPath(l *Layout) string {
	return filepath.Join(filepath.Dir(l.State), "codex-recovery.json")
}
func loadCodexRecovery(l *Layout) (*codexRecovery, error) {
	path := codexRecoveryPath(l)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !codexRollbackOwnedPath(path, false) || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return nil, errors.New("cannot safely read Codex recovery journal")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read Codex recovery journal")
	}
	var journal codexRecovery
	if json.Unmarshal(data, &journal) != nil || journal.Schema != 1 || len(journal.Snapshots) == 0 || len(journal.Snapshots) > 32 || len(journal.RejectedVersions) > 64 {
		return nil, errors.New("invalid Codex recovery journal")
	}
	switch journal.Phase {
	case "prepared", "installing", "starting", "rollback", "committed":
	default:
		return nil, errors.New("invalid Codex recovery phase")
	}
	homes := map[string]bool{}
	if journal.Phase == "committed" {
		return &journal, nil
	}
	for _, snapshot := range journal.Snapshots {
		if homes[snapshot.Distribution.Home] || len(snapshot.Profiles) == 0 || len(snapshot.Profiles) > 128 {
			return nil, errors.New("invalid Codex recovery installation set")
		}
		homes[snapshot.Distribution.Home] = true
		if err := validateCodexSnapshot(snapshot); err != nil {
			return nil, err
		}
	}
	for _, version := range journal.RejectedVersions {
		if _, err := ParseVersion(version); err != nil {
			return nil, errors.New("invalid rejected Codex recovery version")
		}
	}
	return &journal, nil
}
func snapshotCodexRuntime(distribution *codexDistribution, profiles map[string]bool) (codexRuntimeSnapshot, error) {
	snapshot := codexRuntimeSnapshot{Distribution: *distribution, Profiles: profiles}
	current := filepath.Join(distribution.Home, "packages", "standalone", "current")
	if info, err := os.Lstat(current); err != nil || info.Mode()&os.ModeSymlink == 0 {
		return snapshot, errors.New("Codex current release is not a standalone symlink")
	}
	release, err := filepath.EvalSymlinks(current)
	if err != nil {
		return snapshot, errors.New("cannot preserve the current Codex release")
	}
	snapshot.ReleaseDir = release
	snapshot.BinarySHA256, err = codexFileHash(filepath.Join(release, "bin", "codex"))
	if err != nil {
		return snapshot, err
	}
	snapshot.MetadataSHA256, err = codexFileHash(filepath.Join(release, "codex-package.json"))
	if err != nil {
		return snapshot, err
	}
	return snapshot, validateCodexSnapshot(snapshot)
}
func codexRollbackOwnedPath(path string, directory bool) bool {
	if !codexOwnedPath(path, directory) {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().Perm()&0022 == 0
}
func codexFileHash(path string) (string, error) {
	if !codexRollbackOwnedPath(path, false) {
		return "", errors.New("previous Codex release is not a trusted owned file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot verify previous Codex release")
	}
	defer file.Close()
	digest := sha256.New()
	if _, err = io.Copy(digest, file); err != nil {
		return "", errors.New("cannot hash previous Codex release")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
func validateCodexSnapshot(snapshot codexRuntimeSnapshot) error {
	d := snapshot.Distribution
	if !filepath.IsAbs(d.Home) || filepath.Clean(d.Home) != d.Home || !filepath.IsAbs(d.InstallDir) || filepath.Clean(d.InstallDir) != d.InstallDir || d.Binary != filepath.Join(d.InstallDir, "codex") {
		return errors.New("invalid Codex recovery installation paths")
	}
	standalone := filepath.Join(d.Home, "packages", "standalone")
	releases := filepath.Join(standalone, "releases")
	if filepath.Dir(snapshot.ReleaseDir) != releases || filepath.Clean(snapshot.ReleaseDir) != snapshot.ReleaseDir {
		return errors.New("previous Codex release is outside its installation")
	}
	for _, dir := range []string{d.Home, filepath.Join(d.Home, "packages"), standalone, releases, snapshot.ReleaseDir, filepath.Join(snapshot.ReleaseDir, "bin"), d.InstallDir} {
		if !codexOwnedPath(dir, true) || ((dir == snapshot.ReleaseDir || dir == filepath.Join(snapshot.ReleaseDir, "bin")) && !codexRollbackOwnedPath(dir, true)) {
			return errors.New("previous Codex release ownership changed")
		}
	}
	metadataPath := filepath.Join(snapshot.ReleaseDir, "codex-package.json")
	binaryHash, err := codexFileHash(filepath.Join(snapshot.ReleaseDir, "bin", "codex"))
	if err != nil {
		return err
	}
	metadataHash, err := codexFileHash(metadataPath)
	if err != nil {
		return err
	}
	if len(snapshot.BinarySHA256) != 64 || len(snapshot.MetadataSHA256) != 64 || snapshot.BinarySHA256 != binaryHash || snapshot.MetadataSHA256 != metadataHash {
		return errors.New("previous Codex release changed; automatic rollback refused")
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil || len(data) > 64<<10 {
		return errors.New("previous Codex package metadata is unavailable")
	}
	var metadata struct {
		Layout     int    `json:"layoutVersion"`
		Version    string `json:"version"`
		Target     string `json:"target"`
		Variant    string `json:"variant"`
		Entrypoint string `json:"entrypoint"`
	}
	if json.Unmarshal(data, &metadata) != nil || metadata.Layout != 1 || metadata.Variant != "codex" || metadata.Entrypoint != "bin/codex" || metadata.Version != d.Version {
		return errors.New("previous Codex package identity changed")
	}
	if _, err := ParseVersion(metadata.Version); err != nil {
		return errors.New("previous Codex package version is invalid")
	}
	switch metadata.Target {
	case "x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl", "x86_64-apple-darwin", "aarch64-apple-darwin":
	default:
		return errors.New("previous Codex package target is invalid")
	}
	if filepath.Base(snapshot.ReleaseDir) != metadata.Version+"-"+metadata.Target {
		return errors.New("previous Codex package path does not match its metadata")
	}
	if snapshot.CandidateReleaseDir != "" {
		version := strings.TrimSuffix(filepath.Base(snapshot.CandidateReleaseDir), "-"+metadata.Target)
		if filepath.Dir(snapshot.CandidateReleaseDir) != releases || version == filepath.Base(snapshot.CandidateReleaseDir) {
			return errors.New("invalid candidate Codex recovery path")
		}
		if _, err := ParseVersion(version); err != nil {
			return errors.New("invalid candidate Codex recovery version")
		}
	}
	return nil
}
func validateCodexCurrentRelease(snapshot codexRuntimeSnapshot) error {
	d := snapshot.Distribution
	current := filepath.Join(d.Home, "packages", "standalone", "current")
	target, err := os.Readlink(current)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("Codex current release is no longer a standalone symlink")
	}
	if err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		target = filepath.Clean(target)
		if target != snapshot.ReleaseDir && (snapshot.CandidateReleaseDir == "" || target != snapshot.CandidateReleaseDir) {
			return errors.New("Codex current release changed after its update checkpoint; automatic rollback refused")
		}
	}
	launcherTarget := filepath.Join(current, "bin", "codex")
	if target, err := os.Readlink(d.Binary); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(d.InstallDir, target)
		}
		if filepath.Clean(target) != launcherTarget {
			return errors.New("Codex launcher changed; automatic rollback refused")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("Codex launcher is no longer a standalone symlink")
	}
	return nil
}
func restoreCodexRuntimeSnapshot(snapshot codexRuntimeSnapshot) error {
	if err := validateCodexSnapshot(snapshot); err != nil {
		return err
	}
	if err := validateCodexCurrentRelease(snapshot); err != nil {
		return err
	}
	d := snapshot.Distribution
	standalone := filepath.Join(d.Home, "packages", "standalone")
	current := filepath.Join(standalone, "current")
	// A partial official installation can remove current or leave a broken link.
	// Never replace a directory, regular file, or a launcher redirected elsewhere.
	if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return errors.New("Codex current release was replaced by a non-symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect Codex current release")
	}
	launcherTarget := filepath.Join(current, "bin", "codex")
	if target, err := os.Readlink(d.Binary); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(d.InstallDir, target)
		}
		if filepath.Clean(target) != launcherTarget {
			return errors.New("Codex launcher changed; automatic rollback refused")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("Codex launcher is no longer a standalone symlink")
	}
	if err := atomicCodexSymlink(snapshot.ReleaseDir, current); err != nil {
		return err
	}
	if _, err := os.Lstat(d.Binary); errors.Is(err, os.ErrNotExist) {
		return atomicCodexSymlink(launcherTarget, d.Binary)
	}
	return nil
}
func atomicCodexSymlink(target, path string) error {
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(path), ".codex-rollback-"+suffix)
	if err = os.Symlink(target, temp); err != nil {
		return errors.New("cannot prepare Codex rollback symlink")
	}
	defer os.Remove(temp)
	if err = os.Rename(temp, path); err != nil {
		return errors.New("cannot restore Codex release symlink")
	}
	return SyncDir(filepath.Dir(path))
}
func clearCodexRecovery(l *Layout) error {
	if err := os.Remove(codexRecoveryPath(l)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return SyncDir(filepath.Dir(codexRecoveryPath(l)))
}
func (m *Manager) rejectCodexRecoveryVersions(l *Layout, journal *codexRecovery) error {
	state, err := loadCodexUpdateState(l)
	if err != nil {
		return err
	}
	for _, version := range journal.RejectedVersions {
		if !slices.Contains(state.RejectedVersions, version) {
			state.RejectedVersions = append(state.RejectedVersions, version)
		}
	}
	if len(state.RejectedVersions) > 64 {
		state.RejectedVersions = state.RejectedVersions[len(state.RejectedVersions)-64:]
	}
	state.RestartPending = journal.Restart
	if version, err := m.installedCodexWorkerVersion(context.Background(), l); err == nil && state.RejectedByWorkerVersion == "" {
		state.RejectedByWorkerVersion = version
	}
	return WriteJSON(codexUpdateStatePath(l), state)
}

// Recovery never stops a worker that might have accepted new work without a
// newly verified update lease. Failure to obtain one leaves this journal intact.
func (m *Manager) recoverCodexRuntime(ctx context.Context, l *Layout, journal *codexRecovery) error {
	if journal.Phase == "committed" {
		return m.finishCodexRecovery(l)
	}
	if journal.Phase != "prepared" {
		journal.Phase = "rollback"
		if err := writeCodexRecovery(l, journal); err != nil {
			return err
		}
		if err := m.rejectCodexRecoveryVersions(l, journal); err != nil {
			return err
		}
	}
	for _, snapshot := range journal.Snapshots {
		if err := validateCodexSnapshot(snapshot); err != nil {
			return err
		}
		if err := validateCodexCurrentRelease(snapshot); err != nil {
			return err
		}
	}
	running, err := m.workerRunning(ctx, l)
	if err != nil {
		return err
	}
	if running && journal.Phase == "prepared" {
		// No installer or candidate start was reached. Leave the original worker
		// alone, provided its launchers still resolve to the captured releases.
		for _, snapshot := range journal.Snapshots {
			actual, err := filepath.EvalSymlinks(snapshot.Distribution.Binary)
			if err != nil || actual != filepath.Join(snapshot.ReleaseDir, "bin", "codex") {
				return errors.New("Codex installation changed before interrupted maintenance")
			}
		}
		return m.finishCodexRecovery(l)
	}
	if running {
		if !journal.Restart {
			journal.Restart = true
			if err := writeCodexRecovery(l, journal); err != nil {
				return err
			}
		}
		lease, err := m.workerPrepare(ctx, l)
		if err != nil {
			return errors.Join(errors.New("Codex rollback is pending until the restarted worker can safely stop"), err)
		}
		stopped := false
		defer func() {
			if !stopped {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				_ = m.workerAbort(cleanup, l, lease.Token)
			}
		}()
		if lease.ExpiresAt.Sub(m.updateNow()) < 45*time.Second {
			return &BusyError{Reason: "Codex rollback reservation expired; retry later"}
		}
		if err = m.Service(ctx, l, "stop"); err != nil {
			return err
		}
		if live, err := m.workerRunning(ctx, l); err != nil || live {
			return errors.Join(err, errors.New("Codex rollback could not confirm that the worker stopped"))
		}
		stopped = true
	}
	for _, snapshot := range journal.Snapshots {
		if err := restoreCodexRuntimeSnapshot(snapshot); err != nil {
			return err
		}
	}
	expected := map[string]string{}
	for _, snapshot := range journal.Snapshots {
		current, err := m.currentCodexDistribution(ctx, &snapshot.Distribution)
		if err != nil || current.Version != snapshot.Distribution.Version {
			return errors.Join(err, errors.New("restored Codex release could not be verified"))
		}
		for profile, autostart := range snapshot.Profiles {
			if autostart {
				expected[profile] = snapshot.Distribution.Version
			}
		}
	}
	if journal.Restart {
		after := m.updateNow()
		if err = m.Service(ctx, l, "start"); err != nil {
			return errors.New("Codex rollback could not restart the worker; recovery remains pending")
		}
		if err = m.WorkerReady(ctx, l, after, ""); err != nil {
			return errors.Join(errors.New("Codex rollback worker readiness failed; recovery remains pending"), err)
		}
		if err = m.codexRuntimeReady(ctx, l, after, expected); err != nil {
			return errors.Join(errors.New("previous Codex runtimes did not recover; recovery remains pending"), err)
		}
	}
	return m.finishCodexRecovery(l)
}
func (m *Manager) finishCodexRecovery(l *Layout) error {
	state, err := loadCodexUpdateState(l)
	if err != nil {
		return err
	}
	state.RestartPending = false
	if err = WriteJSON(codexUpdateStatePath(l), state); err != nil {
		return err
	}
	return clearCodexRecovery(l)
}

func codexPreflightEnvironment(home string) []string {
	env := []string{"HOME=" + home, "CODEX_HOME=" + home}
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}
func (m *Manager) installedCodexWorkerVersion(ctx context.Context, l *Layout) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := m.command(ctx, l.Binary, "version")
	if err != nil {
		return "", errors.New("cannot verify worker version for Codex retry")
	}
	version := strings.TrimSpace(string(output))
	if _, err := ParseVersion(version); err != nil {
		return "", errors.New("invalid worker version for Codex retry")
	}
	return version, nil
}

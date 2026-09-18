package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const codexLatestURL = "https://releases.openai.com/codex/channels/latest"
const codexLatestFallbackURL = "https://api.github.com/repos/openai/codex/releases/latest"
const codexMetadataLimit = 2 * 1024 * 1024
const codexUpdateTimeout = 10 * time.Minute

// ErrCodexDistributionUnsupported distinguishes an installation that should
// remain under its existing package manager or manual version pin.
var ErrCodexDistributionUnsupported = errors.New("automatic Codex runtime updates require a supported, user-owned standalone installation")

type codexDistribution struct {
	Binary, Home, InstallDir, Version string
}

func parseCodexVersion(output string) (string, error) {
	value := strings.TrimSpace(output)
	value = strings.TrimPrefix(value, "codex-cli ")
	value = strings.TrimPrefix(value, "codex ")
	if _, err := ParseVersion(value); err != nil {
		return "", errors.New("Codex runtime version must be a stable MAJOR.MINOR.PATCH version")
	}
	return strings.TrimPrefix(value, "v"), nil
}

func codexMetadataVersion(data []byte) (string, error) {
	var metadata struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil || metadata.Draft || metadata.Prerelease || !strings.HasPrefix(metadata.Tag, "rust-v") {
		return "", errors.New("invalid stable Codex release metadata")
	}
	version, err := parseCodexVersion(strings.TrimPrefix(metadata.Tag, "rust-v"))
	if err != nil {
		return "", errors.New("invalid stable Codex release metadata")
	}
	return version, nil
}

// The standalone installer uses the same public stable channel, with GitHub
// Releases as a fallback. No account credentials are needed by either endpoint.
func (m *Manager) fetchCodexLatest(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	data, err := m.fetchCodexMetadata(ctx)
	if err == nil {
		if version, parseErr := codexMetadataVersion(data); parseErr == nil {
			return version, nil
		}
	}
	if ctx.Err() != nil {
		return "", errors.New("Codex release metadata check was cancelled or timed out")
	}
	data, err = m.Download(ctx, codexLatestFallbackURL, codexMetadataLimit)
	if err != nil {
		// A custom HTTP transport can return errors containing signed URLs or
		// credentials. Never include transport errors or response bodies here.
		return "", errors.New("could not check the latest stable Codex runtime version")
	}
	return codexMetadataVersion(data)
}

func (m *Manager) fetchCodexMetadata(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexLatestURL, nil)
	if err != nil {
		return nil, errors.New("could not construct the Codex release metadata request")
	}
	req.Header.Set("User-Agent", "codex-telegramgw-release-manager")
	req.Header.Set("Accept", "application/json")
	client := *m.HTTP
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		u := req.URL
		if len(via) >= 5 || u.Scheme != "https" || u.Hostname() != "releases.openai.com" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
			return errors.New("rejected an unexpected Codex release metadata redirect")
		}
		return nil
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Codex release metadata request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Codex release metadata request failed (HTTP %d)", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, codexMetadataLimit+1))
	if err != nil || len(data) > codexMetadataLimit || response.ContentLength > int64(len(data)) {
		return nil, errors.New("could not read Codex release metadata within its size limit")
	}
	return data, nil
}

func codexOwnedPath(path string, directory bool) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm()&0002 != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

// Only the official standalone launcher is changed automatically. A direct path
// into a release is a version pin; npm, Homebrew, and copied binaries retain
// their existing installation ownership and are left alone.
func (m *Manager) inspectCodexDistribution(ctx context.Context, executable string) (*codexDistribution, error) {
	binary, err := exec.LookPath(executable)
	if err != nil {
		return nil, ErrCodexDistributionUnsupported
	}
	binary, err = filepath.Abs(binary)
	if err != nil || filepath.Base(binary) != "codex" {
		return nil, ErrCodexDistributionUnsupported
	}
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil || filepath.Base(resolved) != "codex" || filepath.Base(filepath.Dir(resolved)) != "bin" {
		return nil, ErrCodexDistributionUnsupported
	}
	releaseDir := filepath.Dir(filepath.Dir(resolved))
	releasesDir := filepath.Dir(releaseDir)
	standaloneDir := filepath.Dir(releasesDir)
	packagesDir := filepath.Dir(standaloneDir)
	if filepath.Base(releasesDir) != "releases" || filepath.Base(standaloneDir) != "standalone" || filepath.Base(packagesDir) != "packages" {
		return nil, ErrCodexDistributionUnsupported
	}
	home := filepath.Dir(packagesDir)
	installDir := filepath.Dir(binary)
	for _, dir := range []string{home, packagesDir, standaloneDir, releasesDir, releaseDir, filepath.Dir(resolved), installDir} {
		if !codexOwnedPath(dir, true) {
			return nil, ErrCodexDistributionUnsupported
		}
	}
	current := filepath.Join(standaloneDir, "current")
	target, err := os.Readlink(binary)
	if err != nil {
		return nil, ErrCodexDistributionUnsupported
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(installDir, target)
	}
	if filepath.Clean(target) != filepath.Join(current, "bin", "codex") {
		return nil, ErrCodexDistributionUnsupported
	}
	currentTarget, err := filepath.EvalSymlinks(current)
	if err != nil || currentTarget != releaseDir {
		return nil, ErrCodexDistributionUnsupported
	}
	metadataPath := filepath.Join(releaseDir, "codex-package.json")
	if !codexOwnedPath(metadataPath, false) || !codexOwnedPath(resolved, false) {
		return nil, ErrCodexDistributionUnsupported
	}
	metadataFile, err := os.Open(metadataPath)
	if err != nil {
		return nil, ErrCodexDistributionUnsupported
	}
	data, err := io.ReadAll(io.LimitReader(metadataFile, 64*1024+1))
	metadataFile.Close()
	if err != nil || len(data) > 64*1024 {
		return nil, ErrCodexDistributionUnsupported
	}
	var metadata struct {
		LayoutVersion int    `json:"layoutVersion"`
		Version       string `json:"version"`
		Target        string `json:"target"`
		Variant       string `json:"variant"`
		Entrypoint    string `json:"entrypoint"`
	}
	if err = json.Unmarshal(data, &metadata); err != nil || metadata.LayoutVersion != 1 || metadata.Variant != "codex" || metadata.Entrypoint != "bin/codex" {
		return nil, ErrCodexDistributionUnsupported
	}
	switch metadata.Target {
	case "x86_64-unknown-linux-musl", "aarch64-unknown-linux-musl", "x86_64-apple-darwin", "aarch64-apple-darwin":
	default:
		return nil, ErrCodexDistributionUnsupported
	}
	version, err := parseCodexVersion(metadata.Version)
	if err != nil || metadata.Version != version || filepath.Base(releaseDir) != version+"-"+metadata.Target {
		return nil, ErrCodexDistributionUnsupported
	}
	output, err := m.command(ctx, binary, "--version")
	if err != nil {
		return nil, errors.New("could not inspect the installed Codex runtime version")
	}
	actual, err := parseCodexVersion(string(output))
	if err != nil || actual != version {
		return nil, errors.New("installed Codex runtime version does not match its package metadata")
	}
	return &codexDistribution{Binary: binary, Home: home, InstallDir: installDir, Version: version}, nil
}

func (m *Manager) installCodexRuntime(ctx context.Context, distribution *codexDistribution) error {
	if distribution == nil {
		return ErrCodexDistributionUnsupported
	}
	run := m.CodexRun
	if run == nil {
		run = RunCodexUpdate
	}
	// Native Codex selects its installer from the executable's package layout.
	// Remove package-manager overrides and older native updater guard variables;
	// neither should redirect this controlled update into another installation.
	args := []string{"env"}
	for _, key := range []string{"CODEX_MANAGED_BY_NPM", "CODEX_MANAGED_BY_BUN", "CODEX_MANAGED_BY_PNPM", "CODEX_MANAGED_BY_VITE_PLUS", "CODEX_INSTALL_IF_LATEST", "CODEX_UPDATE_FROM_RELEASE"} {
		args = append(args, "-u", key)
	}
	args = append(args,
		"CODEX_HOME="+distribution.Home,
		"CODEX_INSTALL_DIR="+distribution.InstallDir,
		"CODEX_RELEASE=latest",
		"CODEX_NON_INTERACTIVE=1",
		distribution.Binary, "update",
	)
	result, err := run(ctx, args...)
	if err != nil {
		return errors.New("Codex runtime update could not complete")
	}
	if result.ExitCode != 0 {
		return errors.New("native Codex runtime updater failed")
	}
	return nil
}

// RunCodexUpdate gives the official package installer sufficient download time.
// Unlike a single-process timeout, cancellation also stops the shell and its
// downloader before the caller can restart the app server. Installer output is
// intentionally discarded because it can include local paths and signed URLs.
func RunCodexUpdate(ctx context.Context, args ...string) (CommandResult, error) {
	if len(args) == 0 {
		return CommandResult{}, errors.New("missing Codex update command")
	}
	ctx, cancel := context.WithTimeout(ctx, codexUpdateTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Dir = string(filepath.Separator)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 5 * time.Second
	err := command.Run()
	if ctx.Err() != nil {
		return CommandResult{}, errors.New("Codex runtime update was cancelled or timed out")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return CommandResult{ExitCode: exit.ExitCode()}, nil
	}
	if err != nil {
		return CommandResult{}, errors.New("could not execute the native Codex runtime updater")
	}
	return CommandResult{}, nil
}

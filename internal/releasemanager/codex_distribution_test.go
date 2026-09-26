package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseCodexVersionAcceptsStableVersionsOnly(t *testing.T) {
	for _, value := range []string{"0.155.0", "codex-cli 0.155.0\n", "codex 0.155.0", "v0.155.0"} {
		version, err := parseCodexVersion(value)
		if err != nil || version != "0.155.0" {
			t.Fatalf("parse %q = %q, %v", value, version, err)
		}
	}
	for _, value := range []string{"", "0.155.0-alpha.1", "codex-cli 0.155.0-beta.2", "codex-cli 0.155.0\nprivate output", "rust-v0.155.0", "0.155.0+local", "0.0155.0"} {
		if _, err := parseCodexVersion(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestCodexLatestUsesOfficialStableChannel(t *testing.T) {
	m := New(nil)
	calls := 0
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != codexLatestURL || r.Header.Get("Authorization") != "" || r.Header.Get("User-Agent") == "" {
			t.Fatalf("unexpected metadata request: %s", r.URL)
		}
		return coreResponse(r, []byte(`{"tag_name":"rust-v0.155.0","assets":[]}`)), nil
	})
	version, err := m.fetchCodexLatest(t.Context())
	if err != nil || version != "0.155.0" || calls != 1 {
		t.Fatalf("latest = %q, %v; requests = %d", version, err, calls)
	}
}

func TestCodexLatestFallsBackWithoutLeakingMetadataFailures(t *testing.T) {
	for _, kind := range []string{"server", "malformed", "prerelease", "oversized", "redirect", "transport"} {
		t.Run(kind, func(t *testing.T) {
			m := New(nil)
			calls := 0
			m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() == codexLatestFallbackURL {
					return coreResponse(r, []byte(`{"tag_name":"rust-v0.155.0","draft":false,"prerelease":false}`)), nil
				}
				if r.URL.String() != codexLatestURL {
					t.Fatalf("unsafe redirect followed: %s", r.URL)
				}
				rsp := coreResponse(r, []byte("private metadata"))
				switch kind {
				case "server":
					rsp.StatusCode = http.StatusForbidden
				case "malformed":
				case "prerelease":
					rsp = coreResponse(r, []byte(`{"tag_name":"rust-v0.155.0","prerelease":true}`))
				case "oversized":
					rsp = coreResponse(r, []byte(strings.Repeat("x", codexMetadataLimit+1)))
				case "redirect":
					rsp.StatusCode = http.StatusFound
					rsp.Header.Set("Location", "https://private.example/secret-token")
				case "transport":
					return nil, errors.New("private transport credentials")
				}
				return rsp, nil
			})
			version, err := m.fetchCodexLatest(t.Context())
			if err != nil || version != "0.155.0" || calls != 2 {
				t.Fatalf("latest = %q, %v; requests = %d", version, err, calls)
			}
		})
	}
}

func TestCodexLatestRejectsInvalidFallbackAndSanitizesErrors(t *testing.T) {
	for _, fallback := range []string{`{"tag_name":"rust-v0.156.0-alpha.1"}`, `{"tag_name":"rust-v0.155.0","draft":true}`, "transport"} {
		m := New(nil)
		m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() == codexLatestURL || fallback == "transport" {
				return nil, errors.New("private.example/secret-token")
			}
			return coreResponse(r, []byte(fallback)), nil
		})
		version, err := m.fetchCodexLatest(t.Context())
		if err == nil || version != "" || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "private.example") {
			t.Fatalf("latest = %q, %v", version, err)
		}
	}
}

func codexDistributionFixture(t *testing.T) (*Manager, string, string, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "codex-home")
	installDir := filepath.Join(root, "bin")
	releaseDir := filepath.Join(home, "packages", "standalone", "releases", "0.155.0-aarch64-unknown-linux-musl")
	for _, dir := range []string{installDir, filepath.Join(releaseDir, "bin")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	metadata := `{"layoutVersion":1,"version":"0.155.0","target":"aarch64-unknown-linux-musl","variant":"codex","entrypoint":"bin/codex"}`
	if err := os.WriteFile(filepath.Join(releaseDir, "codex-package.json"), []byte(metadata), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseDir, "bin", "codex"), []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(home, "packages", "standalone", "current")
	if err := os.Symlink(releaseDir, current); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(installDir, "codex")
	if err := os.Symlink(filepath.Join(current, "bin", "codex"), binary); err != nil {
		t.Fatal(err)
	}
	m := New(nil)
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if !slices.Equal(args, []string{binary, "--version"}) {
			t.Fatalf("unexpected inspection command: %v", args)
		}
		return CommandResult{Output: []byte("codex-cli 0.155.0\n")}, nil
	}
	return m, binary, home, releaseDir
}

func TestCodexDistributionInspectsStableStandaloneLauncher(t *testing.T) {
	m, binary, home, release := codexDistributionFixture(t)
	// The official installer follows the user's umask; group-writable owned
	// directories and files do not make it a different installation.
	for _, path := range []string{home, release, filepath.Join(release, "bin"), filepath.Join(release, "bin", "codex"), filepath.Join(release, "codex-package.json")} {
		mode := os.FileMode(0775)
		if filepath.Base(path) == "codex-package.json" {
			mode = 0664
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	distribution, err := m.inspectCodexDistribution(t.Context(), binary)
	if err != nil {
		t.Fatal(err)
	}
	if distribution.Binary != binary || distribution.Home != home || distribution.InstallDir != filepath.Dir(binary) || distribution.Version != "0.155.0" {
		t.Fatalf("unexpected distribution: %+v", distribution)
	}
}

func TestCodexDistributionLeavesPinnedAndUnsupportedInstallationsUntouched(t *testing.T) {
	for _, kind := range []string{"pinned-path", "pinned-link", "plain-binary", "public-directory", "metadata-link", "bad-layout", "prerelease", "oversized-metadata"} {
		t.Run(kind, func(t *testing.T) {
			m, binary, home, releaseDir := codexDistributionFixture(t)
			metadataPath := filepath.Join(releaseDir, "codex-package.json")
			switch kind {
			case "pinned-path":
				binary = filepath.Join(releaseDir, "bin", "codex")
			case "pinned-link":
				if err := os.Remove(binary); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(releaseDir, "bin", "codex"), binary); err != nil {
					t.Fatal(err)
				}
			case "plain-binary":
				if err := os.Remove(binary); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(binary, []byte("fixture"), 0755); err != nil {
					t.Fatal(err)
				}
			case "public-directory":
				if err := os.Chmod(home, 0777); err != nil {
					t.Fatal(err)
				}
			case "metadata-link":
				if err := os.Rename(metadataPath, metadataPath+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(metadataPath+".old", metadataPath); err != nil {
					t.Fatal(err)
				}
			case "bad-layout", "prerelease", "oversized-metadata":
				data, err := os.ReadFile(metadataPath)
				if err != nil {
					t.Fatal(err)
				}
				value := string(data)
				if kind == "bad-layout" {
					value = strings.ReplaceAll(value, `"layoutVersion":1`, `"layoutVersion":2`)
				} else if kind == "prerelease" {
					value = strings.ReplaceAll(value, `"0.155.0"`, `"0.155.0-alpha.1"`)
				} else {
					value = strings.Repeat("x", 64*1024+1)
				}
				if err := os.WriteFile(metadataPath, []byte(value), 0644); err != nil {
					t.Fatal(err)
				}
			}
			m.Run = func(context.Context, ...string) (CommandResult, error) {
				t.Fatal("unsupported installation was executed")
				return CommandResult{}, nil
			}
			distribution, err := m.inspectCodexDistribution(t.Context(), binary)
			if !errors.Is(err, ErrCodexDistributionUnsupported) || distribution != nil {
				t.Fatalf("inspect %s = %+v, %v", kind, distribution, err)
			}
		})
	}
}

func TestCodexDistributionRejectsVersionMismatchWithoutLeakingOutput(t *testing.T) {
	m, binary, _, _ := codexDistributionFixture(t)
	m.Run = func(context.Context, ...string) (CommandResult, error) {
		return CommandResult{Output: []byte("private path secret-token")}, nil
	}
	_, err := m.inspectCodexDistribution(t.Context(), binary)
	if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), binary) {
		t.Fatalf("inspect = %v", err)
	}
}

func TestCodexRuntimeInstallerUsesDetectedInstallationAndNoninteractiveMode(t *testing.T) {
	m, binary, _, _ := codexDistributionFixture(t)
	distribution, err := m.inspectCodexDistribution(t.Context(), binary)
	if err != nil {
		t.Fatal(err)
	}
	m.CodexRun = func(_ context.Context, args ...string) (CommandResult, error) {
		for _, arg := range []string{"CODEX_HOME=" + distribution.Home, "CODEX_INSTALL_DIR=" + distribution.InstallDir, "CODEX_RELEASE=latest", "CODEX_NON_INTERACTIVE=1"} {
			if !slices.Contains(args, arg) {
				t.Errorf("installer omitted %q", arg)
			}
		}
		for _, key := range []string{"CODEX_MANAGED_BY_NPM", "CODEX_MANAGED_BY_BUN", "CODEX_MANAGED_BY_PNPM", "CODEX_MANAGED_BY_VITE_PLUS", "CODEX_INSTALL_IF_LATEST", "CODEX_UPDATE_FROM_RELEASE"} {
			index := slices.Index(args, key)
			if index < 1 || args[index-1] != "-u" {
				t.Errorf("installer did not remove inherited %s", key)
			}
		}
		if args[0] != "env" || !slices.Equal(args[len(args)-2:], []string{distribution.Binary, "update"}) {
			t.Fatalf("wrong native updater command: %v", args)
		}
		return CommandResult{}, nil
	}
	if err := m.installCodexRuntime(t.Context(), distribution); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRuntimeInstallerSanitizesCommandFailures(t *testing.T) {
	for _, failed := range []bool{false, true} {
		m := New(nil)
		m.CodexRun = func(context.Context, ...string) (CommandResult, error) {
			if failed {
				return CommandResult{}, fmt.Errorf("private.example/secret-token")
			}
			return CommandResult{ExitCode: 1, Output: []byte("private.example/secret-token")}, nil
		}
		err := m.installCodexRuntime(t.Context(), &codexDistribution{})
		if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "private.example") {
			t.Fatalf("install = %v", err)
		}
	}
}

func TestRunCodexUpdateDiscardsOutputAndPreservesExitStatus(t *testing.T) {
	result, err := RunCodexUpdate(t.Context(), "sh", "-c", "printf secret-token; printf secret-token >&2; exit 7")
	if err != nil || result.ExitCode != 7 || len(result.Output) != 0 {
		t.Fatalf("run = %+v, %v", result, err)
	}
}

func TestRunCodexUpdateCancellationKillsInstallerDescendants(t *testing.T) {
	root := t.TempDir()
	ready, marker := filepath.Join(root, "ready"), filepath.Join(root, "survived")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := RunCodexUpdate(ctx, "sh", "-c", `sh -c 'printf ready > "$1"; sleep 0.3; printf survived > "$2"' sh "$1" "$2" & wait`, "sh", ready, marker)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !FileExists(ready) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !FileExists(ready) {
		t.Fatal("installer descendant did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled update reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled update did not finish")
	}
	time.Sleep(400 * time.Millisecond)
	if FileExists(marker) {
		t.Fatal("installer descendant continued modifying files after cancellation")
	}
}

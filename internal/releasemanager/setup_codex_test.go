package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func setupCodexFixture(t *testing.T, installed bool) (*Manager, *Layout, *bytes.Buffer) {
	t.Helper()
	l, err := newLayout("worker", "linux", "arm64", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if installed {
		if err := AtomicWrite(filepath.Join(l.Bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0755, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", t.TempDir()) // Even an existing binary need not be in PATH.
	var output bytes.Buffer
	m := New(&output)
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected request: %s", r.URL)
		return nil, nil
	})
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if !reflect.DeepEqual(args, []string{filepath.Join(l.Bin, "codex"), "login", "status"}) {
			t.Fatalf("unexpected command: %v", args)
		}
		return CommandResult{}, nil
	}
	return m, l, &output
}

func TestSetupCodexUsesExistingLoginAndOffPathBinary(t *testing.T) {
	m, l, _ := setupCodexFixture(t, true)
	prompt := &scriptedWorkerSetup{t: t}
	got, err := m.setupWorkerCodex(context.Background(), prompt, l)
	if err != nil || got != filepath.Join(l.Bin, "codex") {
		t.Fatalf("binary=%q err=%v", got, err)
	}
}

func TestSetupCodexInstallsOfficialStandaloneAndCleansStage(t *testing.T) {
	m, l, _ := setupCodexFixture(t, false)
	const installer = "#!/bin/sh\n# official installer fixture\n"
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != codexSetupInstallerURL {
			t.Fatal("unexpected installer source")
		}
		return coreResponse(r, []byte(installer)), nil
	})
	var staged string
	m.CodexRun = func(_ context.Context, args ...string) (CommandResult, error) {
		if args[0] != "env" || args[len(args)-2] != "sh" {
			t.Fatalf("unexpected installer invocation: %v", args)
		}
		for _, value := range []string{"CODEX_INSTALL_DIR=" + l.Bin, "CODEX_RELEASE=latest", "CODEX_NON_INTERACTIVE=1"} {
			if !strings.Contains(strings.Join(args, "\n"), value) {
				t.Fatalf("missing option %s", value)
			}
		}
		staged = args[len(args)-1]
		data, err := os.ReadFile(staged)
		info, statErr := os.Stat(staged)
		if err != nil || statErr != nil || string(data) != installer || info.Mode().Perm() != 0600 {
			t.Fatal("installer was not staged privately and completely")
		}
		return CommandResult{}, AtomicWrite(filepath.Join(l.Bin, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0755, nil)
	}
	got, err := m.setupWorkerCodex(context.Background(), &scriptedWorkerSetup{t: t, answers: []string{"yes"}}, l)
	if err != nil || got != filepath.Join(l.Bin, "codex") || staged == "" || FileExists(staged) {
		t.Fatalf("binary=%q stage=%q err=%v", got, staged, err)
	}
}

func TestSetupCodexDeclinedInstallationDoesNotDownload(t *testing.T) {
	m, l, _ := setupCodexFixture(t, false)
	_, err := m.setupWorkerCodex(context.Background(), &scriptedWorkerSetup{t: t, answers: []string{"no"}}, l)
	if err == nil || FileExists(l.Bin) {
		t.Fatal("declined installation mutated the machine")
	}
}

func TestSetupCodexLoginRetriesAndVerifiesSavedCredentials(t *testing.T) {
	m, l, output := setupCodexFixture(t, true)
	loggedIn, attempts := false, 0
	m.Run = func(_ context.Context, _ ...string) (CommandResult, error) {
		if loggedIn {
			return CommandResult{}, nil
		}
		return CommandResult{ExitCode: 1, Output: []byte("private diagnostic")}, nil
	}
	m.SetupRun = func(_ context.Context, args ...string) (CommandResult, error) {
		attempts++
		want := []string{filepath.Join(l.Bin, "codex"), "login"}
		if attempts == 1 {
			want = append(want, "--device-auth")
		}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("command=%v", args)
		}
		if attempts == 1 {
			return CommandResult{ExitCode: 1}, errors.New("private diagnostic")
		}
		loggedIn = true
		return CommandResult{}, nil
	}
	_, err := m.setupWorkerCodex(context.Background(), &scriptedWorkerSetup{t: t, answers: []string{"invalid", "device", "browser"}}, l)
	if err != nil || attempts != 2 || strings.Contains(output.String(), "private diagnostic") {
		t.Fatal("login retry/verification failed", err, attempts)
	}
}

func TestSetupCodexLaterDoesNotClaimWorkerIsReady(t *testing.T) {
	m, l, output := setupCodexFixture(t, true)
	m.Run = func(context.Context, ...string) (CommandResult, error) { return CommandResult{ExitCode: 1}, nil }
	_, err := m.setupWorkerCodex(context.Background(), &scriptedWorkerSetup{t: t, answers: []string{"later"}}, l)
	if err == nil || !strings.Contains(err.Error(), "paused") || !strings.Contains(output.String(), "rerun the same installer") {
		t.Fatal("missing actionable deferred-login state", err)
	}
}

func TestSetupCodexRejectsBadDownloadBeforeExecution(t *testing.T) {
	for _, kind := range []string{"html", "truncated", "redirect", "http-error", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			m, l, _ := setupCodexFixture(t, false)
			m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != codexSetupInstallerURL {
					t.Fatal("followed unsafe redirect")
				}
				response := coreResponse(r, []byte("#!/bin/sh\nexit 0\n"))
				switch kind {
				case "html":
					response = coreResponse(r, []byte("<html>error</html>"))
				case "truncated":
					response.ContentLength = 9999
				case "redirect":
					response.StatusCode = http.StatusFound
					response.Header.Set("Location", "http://chatgpt.com/private-token")
				case "http-error":
					response.StatusCode = http.StatusServiceUnavailable
				case "oversize":
					response = coreResponse(r, []byte("#!/bin/sh\n"+strings.Repeat("x", 1024*1024)))
				}
				return response, nil
			})
			m.CodexRun = func(context.Context, ...string) (CommandResult, error) {
				t.Fatal("executed rejected download")
				return CommandResult{}, nil
			}
			err := m.installSetupCodex(context.Background(), l)
			if err == nil || strings.Contains(err.Error(), "private-token") || FileExists(l.Bin) {
				t.Fatal("unsafe installer result", err)
			}
		})
	}
}

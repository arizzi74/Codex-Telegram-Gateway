package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestSetupWorkerEnrollmentUsesVisibleURLAndLocalServiceAccess(t *testing.T) {
	for _, tc := range []struct {
		name, answer, access, enrollmentAccess string
	}{
		{"default restricted", "", workerServiceRestricted, workerServiceFull},
		{"explicit restricted", "yes", workerServiceRestricted, workerServiceFull},
		{"full", "no", workerServiceFull, workerServiceRestricted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, l, cwd := setupFixture(t)
			var out bytes.Buffer
			m.Out = &out
			value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, body protocol.RedeemWorkerEnrollmentRequest) {
				if body.Name != `Worker "one"` {
					t.Error("worker name changed")
				}
				writeSetupEnrollmentResponse(w, r, tc.enrollmentAccess)
			})
			prompt := &scriptedWorkerSetup{t: t, answers: []string{`Worker "one"`, value, tc.answer}}
			var staged string
			err := m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
				staged = filepath.Dir(opts.Config)
				if opts.Action != "install" || opts.Component != "worker" || !opts.AutoUpdate || !opts.WorkerSetupRecovery || opts.WorkerServiceAccess != tc.access || opts.Version != "v0.5.1" {
					t.Fatalf("incorrect install options: %+v", opts)
				}
				recovery, err := loadWorkerSetupRecovery(l)
				if err != nil || recovery == nil || recovery.ServiceAccess != tc.access {
					t.Fatal("local service access choice was not saved durably", err)
				}
				cfg, err := config.LoadWorker(opts.Config)
				if err != nil {
					t.Fatal(err)
				}
				canonicalHome, _ := filepath.EvalSymlinks(l.Home)
				if cfg.Name != `Worker "one"` || cfg.StateFile != filepath.Join(l.Home, ".local/state/codex-worker/worker.db") || !reflect.DeepEqual(cfg.AllowedWorkspaceRoots, []string{canonicalHome}) || len(cfg.Runtimes) != 1 || cfg.Runtimes[0].WorkingDirectory != canonicalHome || !cfg.Runtimes[0].Autostart || !filepath.IsAbs(cfg.Runtimes[0].CodexBinary) {
					t.Fatal("incorrect automatic worker configuration")
				}
				for _, path := range []string{staged, opts.Config, cfg.TokenFile, workerSetupRecoveryPath(l)} {
					info, err := os.Lstat(path)
					if err != nil || info.Mode().Perm()&0077 != 0 {
						t.Fatal("worker recovery files are not private", err)
					}
				}
				if data, _ := os.ReadFile(cfg.TokenFile); string(data) != testWorkerEnrollmentToken+"\n" {
					t.Fatal("worker token changed")
				}
				project := filepath.Join(l.Home, "created-after-setup", "nested")
				if err := os.MkdirAll(project, 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := auth.CanonicalWorkspace(project, cfg.AllowedWorkspaceRoots); err != nil {
					t.Fatal("future home folders are unavailable", err)
				}
				if _, err := auth.CanonicalWorkspace(cwd, cfg.AllowedWorkspaceRoots); err == nil {
					t.Fatal("outside folder was implicitly allowed")
				}
				return nil
			})
			if err != nil || !prompt.closed || !reflect.DeepEqual(prompt.secrets, []bool{false, false, false}) || len(prompt.answers) != 0 || staged == "" || FileExists(staged) {
				t.Fatal("guided worker setup failed", err)
			}
			if prompt.labels[1] != "Enrollment URL" || strings.Contains(out.String(), "hidden") || !strings.Contains(out.String(), "pasted URL is visible") {
				t.Fatal("enrollment URL input is not described as visible")
			}
			if strings.Contains(out.String(), testWorkerEnrollmentToken) {
				t.Fatal("setup output leaked the worker authentication token")
			}
		})
	}
}

func TestSetupWorkerMacOSEnrollmentSkipsServiceAccessQuestion(t *testing.T) {
	m, linux, cwd := setupFixture(t)
	l, err := newLayout("worker", "darwin", "arm64", linux.Home)
	if err != nil {
		t.Fatal(err)
	}
	oldRun := m.Run
	m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
		if len(args) >= 2 && args[0] == "launchctl" && args[1] == "print" {
			return CommandResult{}, nil
		}
		return oldRun(ctx, args...)
	}
	value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, _ protocol.RedeemWorkerEnrollmentRequest) {
		writeSetupEnrollmentResponse(w, r, workerServiceFull)
	})
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"", value}}
	err = m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
		if opts.WorkerServiceAccess != "" {
			t.Fatal("macOS installation received a Linux service profile")
		}
		recovery, err := loadWorkerSetupRecovery(l)
		if err != nil || recovery == nil || recovery.ServiceAccess != workerServiceFull {
			t.Fatal("macOS enrollment recovery changed", err)
		}
		return nil
	})
	if err != nil || !prompt.closed || !reflect.DeepEqual(prompt.labels, []string{"Worker name", "Enrollment URL"}) || !reflect.DeepEqual(prompt.secrets, []bool{false, false}) {
		t.Fatal("macOS setup asked a Linux service access question", err, prompt.labels)
	}
}

func TestSetupWorkerServiceAccessCancellationDoesNotRedeem(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"cancelled choice", context.Canceled},
		{"closed choice", io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, l, cwd := setupFixture(t)
			oldRun := m.Run
			m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
				if filepath.Base(args[0]) == "codex" {
					t.Fatal("cancelled service choice proceeded to Codex setup")
				}
				return oldRun(ctx, args...)
			}
			m.HTTP.Transport = coreRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Fatal("cancelled service choice consumed enrollment URL")
				return nil, nil
			})
			prompt := &scriptedWorkerSetup{t: t, answers: []string{"", "https://gateway.example/tgw/enroll/#" + testWorkerEnrollmentCode}, endErr: tc.err}
			err := m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(context.Context, options) error {
				t.Fatal("cancelled service choice reached installation")
				return nil
			})
			if !errors.Is(err, tc.err) || !prompt.closed || FileExists(workerSetupRecoveryPath(l)) || FileExists(l.Config) {
				t.Fatal("cancelled service choice left an enrolled worker", err)
			}
		})
	}
}

func TestSetupWorkerKeepsRecoveryCredentialsAcrossFailureAndRerun(t *testing.T) {
	for _, tc := range []struct {
		name, answer, access, enrollmentAccess string
		installErr                             error
	}{
		{"restricted after failure", "yes", workerServiceRestricted, workerServiceFull, errors.New("installation failed")},
		{"full after cancellation", "no", workerServiceFull, workerServiceRestricted, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, l, cwd := setupFixture(t)
			calls := 0
			value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, _ protocol.RedeemWorkerEnrollmentRequest) {
				calls++
				writeSetupEnrollmentResponse(w, r, tc.enrollmentAccess)
			})
			prompt := &scriptedWorkerSetup{t: t, answers: []string{"", value, tc.answer}}
			var first config.WorkerConfig
			err := m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
				if opts.WorkerServiceAccess != tc.access {
					t.Fatal("initial install ignored local service access choice")
				}
				first, _ = config.LoadWorker(opts.Config)
				return tc.installErr
			})
			if !errors.Is(err, tc.installErr) || calls != 1 || !FileExists(workerSetupRecoveryPath(l)) || first.Name != "my-worker" {
				t.Fatal("failed setup lost enrollment credentials", err)
			}
			recovery, err := loadWorkerSetupRecovery(l)
			if err != nil || recovery == nil || recovery.ServiceAccess != tc.access {
				t.Fatal("failed setup lost the local service access choice", err)
			}
			m.HTTP.Transport = coreRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Fatal("recovered installation attempted new enrollment")
				return nil, nil
			})
			err = m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) {
				t.Fatal("recovery reopened setup prompts")
				return nil, nil
			}, func(_ context.Context, opts options) error {
				second, err := config.LoadWorker(opts.Config)
				if err != nil || !reflect.DeepEqual(first, second) || opts.WorkerServiceAccess != tc.access || !opts.WorkerSetupRecovery {
					t.Fatal("saved worker identity or settings changed", err)
				}
				return nil
			})
			if err != nil || FileExists(workerSetupRecoveryDirectory(l)) {
				t.Fatal("recovered installation did not finish", err)
			}
		})
	}
}

func TestSetupWorkerExpiredURLRequiresFreshURLAndKeepsName(t *testing.T) {
	m, l, cwd := setupFixture(t)
	calls := 0
	value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, body protocol.RedeemWorkerEnrollmentRequest) {
		calls++
		if body.Name != "Worker one" {
			t.Error("name was not retained")
		}
		if body.Code == testWorkerEnrollmentCode {
			w.WriteHeader(http.StatusGone)
			return
		}
		writeSetupEnrollmentResponse(w, r, workerServiceRestricted)
	})
	fresh := strings.TrimSuffix(value, testWorkerEnrollmentCode) + "ABCDEFGH2346"
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"Worker one", value, "no", value, fresh}}
	err := m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(_ context.Context, opts options) error {
		if opts.WorkerServiceAccess != workerServiceFull {
			t.Fatal("fresh URL reset the local service access choice")
		}
		return nil
	})
	if err != nil || calls != 2 || !reflect.DeepEqual(prompt.secrets, []bool{false, false, false, false, false}) || len(prompt.answers) != 0 {
		t.Fatal("expired URL did not safely recover", err, calls)
	}
	if prompt.labels[1] != "Enrollment URL" || prompt.labels[3] != "Fresh enrollment URL" || prompt.labels[4] != "Fresh enrollment URL" {
		t.Fatal("retry URL prompts were not visible", prompt.labels)
	}
}

func TestSetupWorkerRejectsInvalidNameAndURLWithoutEchoingValues(t *testing.T) {
	m, l, cwd := setupFixture(t)
	var out bytes.Buffer
	m.Out = &out
	value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, _ protocol.RedeemWorkerEnrollmentRequest) {
		writeSetupEnrollmentResponse(w, r, workerServiceRestricted)
	})
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"private-name\nsecond", "Worker one", "https://secret@gateway.example/tgw/enroll/#" + testWorkerEnrollmentCode, value, ""}}
	err := m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(context.Context, options) error { return nil })
	if err != nil || !reflect.DeepEqual(prompt.secrets, []bool{false, false, false, false, false}) || strings.Contains(out.String(), "private-name") || strings.Contains(out.String(), "secret@gateway") {
		t.Fatal("invalid input leaked or did not recover", err)
	}
}

func TestSetupWorkerCancelsBeforeRedeemAndDoesNotEnrollAfterAuthFailure(t *testing.T) {
	for _, kind := range []string{"cancel", "auth-failure", "service-preflight"} {
		t.Run(kind, func(t *testing.T) {
			m, l, cwd := setupFixture(t)
			var out bytes.Buffer
			m.Out = &out
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			oldRun := m.Run
			m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
				if kind == "service-preflight" && args[0] == "systemctl" {
					return CommandResult{ExitCode: 1}, nil
				}
				if len(args) == 3 && args[1] == "login" && args[2] == "status" {
					if kind == "cancel" {
						cancel()
					} else {
						return CommandResult{ExitCode: 1}, nil
					}
				}
				return oldRun(ctx, args...)
			}
			m.SetupRun = func(context.Context, ...string) (CommandResult, error) { return CommandResult{ExitCode: 1}, nil }
			m.HTTP.Transport = coreRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Fatal("setup consumed enrollment before prerequisites completed")
				return nil, nil
			})
			prompt := &scriptedWorkerSetup{t: t, answers: []string{"", "https://gateway.example/tgw/enroll/#" + testWorkerEnrollmentCode, ""}}
			err := m.setupWorker(ctx, l, cwd, func() (workerSetupPrompt, error) {
				if kind == "service-preflight" {
					t.Fatal("service preflight happened after inputs opened")
				}
				return prompt, nil
			}, func(context.Context, options) error {
				t.Fatal("incomplete prerequisites reached installation")
				return nil
			})
			if err == nil || FileExists(workerSetupRecoveryPath(l)) || FileExists(l.Config) || strings.Contains(out.String(), testWorkerEnrollmentCode) {
				t.Fatal("incomplete setup left an enrolled worker", err)
			}
			if kind == "auth-failure" && !strings.Contains(out.String(), "login (or login --with-api-key)") {
				t.Fatal("manual login recovery instructions missing")
			}
		})
	}
}

func TestSetupWorkerLockPreventsConcurrentEnrollmentAndPreservesJournal(t *testing.T) {
	m, l, cwd := setupFixture(t)
	if err := prepareWorkerSetupRecovery(l); err != nil {
		t.Fatal(err)
	}
	before := []byte("private existing recovery journal")
	if err := os.WriteFile(workerSetupRecoveryPath(l), before, 0600); err != nil {
		t.Fatal(err)
	}
	unlock, err := Lock(filepath.Join(filepath.Dir(l.Lock), "setup.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	err = m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) {
		t.Fatal("concurrent setup opened prompts")
		return nil, nil
	}, func(context.Context, options) error {
		t.Fatal("concurrent setup reached installation")
		return nil
	})
	var busy *BusyError
	after, readErr := os.ReadFile(workerSetupRecoveryPath(l))
	if !errors.As(err, &busy) || readErr != nil || !bytes.Equal(before, after) {
		t.Fatal("concurrent setup bypassed lock or changed saved credentials", err)
	}
}

func TestSetupWorkerMalformedEnrollmentStopsWithoutPromptingOrInstall(t *testing.T) {
	m, l, cwd := setupFixture(t)
	calls := 0
	value := setupEnrollmentServer(t, m, func(w http.ResponseWriter, r *http.Request, _ protocol.RedeemWorkerEnrollmentRequest) {
		calls++
		w.Write([]byte(`{"private":"token"}`))
	})
	prompt := &scriptedWorkerSetup{t: t, answers: []string{"", value, ""}, endErr: io.EOF}
	err := m.setupWorker(t.Context(), l, cwd, func() (workerSetupPrompt, error) { return prompt, nil }, func(context.Context, options) error {
		t.Fatal("malformed credentials reached install")
		return nil
	})
	if err == nil || errors.Is(err, io.EOF) || calls != 1 || len(prompt.secrets) != 3 || FileExists(workerSetupRecoveryPath(l)) {
		t.Fatal("malformed enrollment was retried or persisted", err)
	}
}

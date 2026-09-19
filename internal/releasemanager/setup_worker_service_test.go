package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestPrepareWorkerServiceChecksUserSession(t *testing.T) {
	for _, system := range []string{"linux", "darwin"} {
		for _, available := range []bool{false, true} {
			t.Run(system+"/"+strconv.FormatBool(available), func(t *testing.T) {
				l := platformWorker(t, system)
				var calls [][]string
				m := New(nil)
				m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
					calls = append(calls, args)
					if !available {
						return CommandResult{ExitCode: 1}, nil
					}
					return CommandResult{}, nil
				}
				err := m.prepareWorkerSetupService(context.Background(), l)
				if (err == nil) != available {
					t.Fatalf("available=%t err=%v", available, err)
				}
				want := []string{"systemctl", "--user", "show-environment"}
				if system == "darwin" {
					want = []string{"launchctl", "print", "gui/" + strconv.Itoa(os.Getuid())}
				}
				if !reflect.DeepEqual(calls, [][]string{want}) {
					t.Fatalf("preflight mutated service state: %v", calls)
				}
				if err != nil && !strings.Contains(err.Error(), map[string]string{"linux": "sign in directly", "darwin": "logged-in desktop"}[system]) {
					t.Fatalf("missing actionable session guidance: %v", err)
				}
			})
		}
	}
}

func TestPrepareWorkerServiceDoesNotRunAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := New(nil)
	m.Run = func(context.Context, ...string) (CommandResult, error) {
		t.Fatal("cancelled setup ran a command")
		return CommandResult{}, nil
	}
	if err := m.prepareWorkerSetupService(ctx, platformWorker(t, "linux")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestFinishWorkerServiceVerifiesBackgroundStartup(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		already, self, sudo, verified bool
		answers                       []string
		wantPersistent, wantSudo      bool
	}{
		{name: "already enabled", already: true, wantPersistent: true},
		{name: "user authorized", self: true, verified: true, wantPersistent: true},
		{name: "sudo authorized", sudo: true, verified: true, answers: []string{"yes"}, wantPersistent: true, wantSudo: true},
		{name: "sudo declined", answers: []string{"no"}},
		{name: "sudo failed", answers: []string{"yes"}, wantSudo: true},
		{name: "cannot verify sudo", sudo: true, answers: []string{"yes"}, wantSudo: true},
		{name: "cannot verify user", self: true, answers: []string{"no"}},
		{name: "reprompt", answers: []string{"invalid", "n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := platformWorker(t, "linux")
			t.Setenv("PATH", "/usr/bin")
			var out bytes.Buffer
			m := New(&out)
			uid := strconv.Itoa(os.Getuid())
			enabled := tc.already
			var calls [][]string
			m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
				calls = append(calls, args)
				switch {
				case reflect.DeepEqual(args, []string{"loginctl", "show-user", uid, "--property=Linger", "--value"}):
					if enabled {
						return CommandResult{Output: []byte("yes\n")}, nil
					}
					return CommandResult{Output: []byte("no\n")}, nil
				case reflect.DeepEqual(args, []string{"loginctl", "--no-ask-password", "enable-linger", uid}):
					if tc.self {
						enabled = tc.verified
						return CommandResult{}, nil
					}
					return CommandResult{ExitCode: 1}, nil
				default:
					t.Fatalf("unexpected command: %v", args)
					return CommandResult{}, nil
				}
			}
			sudoCalled := false
			m.SetupRun = func(_ context.Context, args ...string) (CommandResult, error) {
				sudoCalled = true
				if !reflect.DeepEqual(args, []string{"sudo", "loginctl", "enable-linger", uid}) {
					t.Fatalf("unexpected elevated command: %v", args)
				}
				if tc.sudo {
					enabled = tc.verified
					return CommandResult{}, nil
				}
				return CommandResult{ExitCode: 1}, nil
			}
			prompt := &scriptedWorkerSetup{t: t, answers: tc.answers}
			if err := m.finishWorkerSetup(context.Background(), l, prompt); err != nil {
				t.Fatal(err)
			}
			if sudoCalled != tc.wantSudo {
				t.Fatalf("sudo called=%t wanted=%t", sudoCalled, tc.wantSudo)
			}
			if got := strings.Contains(out.String(), "start automatically at boot"); got != tc.wantPersistent {
				t.Fatalf("incorrect background startup promise: %s", out.String())
			}
			if !tc.wantPersistent && (!strings.Contains(out.String(), "may stop when you log out") || !strings.Contains(out.String(), "sudo loginctl enable-linger "+uid)) {
				t.Fatalf("missing background startup recovery: %s", out.String())
			}
			if tc.already && len(calls) != 1 {
				t.Fatalf("changed existing linger setting: %v", calls)
			}
			if !strings.Contains(out.String(), setupCommandQuote(l.Binary)+" doctor") || !strings.Contains(out.String(), `export PATH="$HOME/.local/bin:$PATH"`) {
				t.Fatalf("missing immediately usable commands: %s", out.String())
			}
		})
	}
}

func TestFinishWorkerServiceMacOSUsesLoginSession(t *testing.T) {
	l := platformWorker(t, "darwin")
	t.Setenv("PATH", l.Bin+":/usr/bin")
	var out bytes.Buffer
	m := New(&out)
	m.Run = func(context.Context, ...string) (CommandResult, error) {
		t.Fatal("macOS finish tried to enable Linux lingering")
		return CommandResult{}, nil
	}
	if err := m.finishWorkerSetup(context.Background(), l, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "while that account remains logged in") || strings.Contains(out.String(), "export PATH") {
		t.Fatalf("incorrect macOS finish guidance: %s", out.String())
	}
}

func TestWorkerSetupCommandsQuotePathsSafely(t *testing.T) {
	l := platformWorker(t, "linux")
	l.Binary = filepath.Join(l.Home, "user's$(false)", "codex-worker")
	var out bytes.Buffer
	m := New(&out)
	m.printWorkerSetupCommands(l)
	if !strings.Contains(out.String(), "user'\"'\"'s$(false)") || !strings.Contains(out.String(), "' attach --latest") {
		t.Fatalf("command path was not shell quoted: %s", out.String())
	}
}

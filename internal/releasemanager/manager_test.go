package releasemanager

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSetupDefaultsFollowUserPrivileges(t *testing.T) {
	for _, test := range []struct {
		args []string
		uid  int
		want string
	}{
		{nil, 0, "gateway"},
		{nil, 1000, "worker"},
		{[]string{"worker"}, 0, "worker"},
		{[]string{"gateway"}, 1000, "gateway"},
		{[]string{"other"}, 0, ""},
		{[]string{"gateway", "--config", "private.json"}, 0, ""},
	} {
		got, err := setupComponent(test.args, test.uid)
		if got != test.want || (err != nil) != (test.want == "") {
			t.Fatalf("setupComponent(%v, %d) = %q, %v", test.args, test.uid, got, err)
		}
	}
}

func coreWorkerHome(t *testing.T) (*Manager, *Layout) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("worker CLI intentionally rejects root")
	}
	t.Setenv("HOME", t.TempDir())
	l, err := NewLayout("worker")
	if err != nil {
		t.Fatal(err)
	}
	if err = AtomicWrite(l.Binary, []byte("worker"), 0755, nil); err != nil {
		t.Fatal(err)
	}
	if err = WriteJSON(l.Config, map[string]any{"worker_id": "example-worker"}); err != nil {
		t.Fatal(err)
	}
	if err = AtomicWrite(l.LegacyManager, []byte("old Python manager"), 0755, nil); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(l.LegacyManager, l.Command); err != nil {
		t.Fatal(err)
	}
	m := New(nil)
	m.Self = filepath.Join(l.Home, "new-manager")
	if err = AtomicWrite(m.Self, []byte("native executable"), 0755, nil); err != nil {
		t.Fatal(err)
	}
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if len(args) != 2 || args[0] != l.Binary || args[1] != "version" {
			t.Fatalf("unexpected service mutation: %v", args)
		}
		return CommandResult{Output: []byte("1.2.3\n")}, nil
	}
	return m, l
}

func TestAdoptPreservesPendingReadinessAndRepository(t *testing.T) {
	m, l := coreWorkerHome(t)
	if err := WriteJSON(l.State, map[string]any{"component": "worker", "repo": "example/releases", "version": "v1.2.3", "pending": true}); err != nil {
		t.Fatal(err)
	}
	if err := m.execute(context.Background(), options{Action: "adopt", Component: "worker"}); err != nil {
		t.Fatal(err)
	}
	state, err := SavedSettings(l)
	if err != nil {
		t.Fatal(err)
	}
	if state["pending"] != true || state["repo"] != "example/releases" {
		t.Fatal(state)
	}
	target, err := os.Readlink(l.Command)
	if err != nil || target != l.Manager {
		t.Fatal(target, err)
	}
	data, _ := os.ReadFile(l.Manager)
	if string(data) != "native executable" {
		t.Fatal("manager not migrated")
	}
}

func TestAdoptRejectsBadSettingsBeforeManagerReplacement(t *testing.T) {
	for _, state := range []map[string]any{{"component": "gateway", "repo": DefaultRepo}, {"component": "worker", "repo": "https://evil.example/repo"}} {
		t.Run(state["repo"].(string), func(t *testing.T) {
			m, l := coreWorkerHome(t)
			if err := WriteJSON(l.State, state); err != nil {
				t.Fatal(err)
			}
			if err := m.execute(context.Background(), options{Action: "adopt", Component: "worker"}); err == nil {
				t.Fatal("accepted invalid state")
			}
			target, _ := os.Readlink(l.Command)
			if target != l.LegacyManager || FileExists(l.Manager) {
				t.Fatal("changed manager before validating state")
			}
		})
	}
}

func TestAutomaticUpdatesRequireInstalledSettings(t *testing.T) {
	m, l := coreWorkerHome(t)
	if err := m.execute(context.Background(), options{Action: "auto-update", Component: "worker", Setting: "enable"}); err == nil {
		t.Fatal("enabled updates without installed settings")
	}
	if FileExists(filepath.Join(l.UnitDir, "codex-worker-update.timer")) {
		t.Fatal("created timer without installation")
	}
}

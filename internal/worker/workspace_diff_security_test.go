package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestWorkspaceDiffDisablesExecutableGitConfiguration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git unavailable")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = repo, workspaceGitEnvironment()
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	write := func(name, text string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "--quiet")
	write("tracked.txt", "staged value\n", 0o600)
	git("add", "tracked.txt")
	write("tracked.txt", "unstaged value\n", 0o600)
	write("untracked.txt", "untracked\n", 0o600)
	write(".gitattributes", "*.txt filter=hostile diff=hostile\n", 0o600)
	write("hook", "#!/bin/sh\necho invoked > \"$(dirname \"$0\")/hook-ran\"\ncat\n", 0o700)
	hook := filepath.Join(repo, "hook")
	for _, key := range []string{"core.fsmonitor", "diff.external", "diff.hostile.command", "diff.hostile.textconv", "filter.hostile.clean", "filter.hostile.smudge", "filter.hostile.process"} {
		git("config", key, hook)
	}
	git("config", "filter.hostile.required", "true")
	// Host environment is also untrusted input to a read-only workspace diff.
	t.Setenv("GIT_DIR", filepath.Join(repo, "missing-repo"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(repo, "wrong-index"))
	t.Setenv("GIT_EXTERNAL_DIFF", hook)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", hook)
	actor := &sessionActor{agent: &Agent{cfg: config.WorkerConfig{AllowedWorkspaceRoots: []string{repo}}}, session: protocol.Session{CWD: repo}}
	text, err := actor.workspaceDiff(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Staged changes:", "+staged value", "Unstaged changes:", "+unstaged value", "Untracked files:", "untracked.txt"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %q in diff: %s", expected, text)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "hook-ran")); !os.IsNotExist(err) {
		t.Fatalf("Git executed hostile workspace code: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "wrong-index")); !os.IsNotExist(err) {
		t.Fatalf("Git obeyed inherited index override: %v", err)
	}
}

func TestWorkspaceDiffCleanRepositoryWithoutFilters(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("Git unavailable")
	}
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", repo)
	cmd.Env = workspaceGitEnvironment()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, output)
	}
	actor := &sessionActor{agent: &Agent{cfg: config.WorkerConfig{AllowedWorkspaceRoots: []string{repo}}}, session: protocol.Session{CWD: repo}}
	text, err := actor.workspaceDiff(context.Background())
	if err != nil || text != "Working tree is clean." {
		t.Fatalf("clean diff = %q, %v", text, err)
	}
}

package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicReplacementPreservesModeAndRunningInode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "program")
	if e := os.WriteFile(p, []byte("old"), 0751); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(p, 0751); e != nil {
		t.Fatal(e)
	}
	f, e := os.Open(p)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if e = AtomicWrite(p, []byte("new"), 0600, nil); e != nil {
		t.Fatal(e)
	}
	old, _ := io.ReadAll(f)
	current, _ := os.ReadFile(p)
	info, _ := os.Stat(p)
	if string(old) != "old" || string(current) != "new" || info.Mode().Perm() != 0751 {
		t.Fatal("replacement changed open inode or mode")
	}
}

func TestAtomicReplacementAndLockRefuseLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte("original"), 0600)
	link := filepath.Join(dir, "link")
	os.Symlink(target, link)
	if AtomicWrite(link, []byte("new"), 0600, nil) == nil {
		t.Fatal("followed symlink")
	}
	if unlock, e := Lock(link); e == nil {
		unlock()
		t.Fatal("followed lock symlink")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "original" {
		t.Fatal("modified target")
	}
}

func TestConcurrentUpdaterIsBusy(t *testing.T) {
	p := filepath.Join(t.TempDir(), "update.lock")
	unlock, e := Lock(p)
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	second, e := Lock(p)
	if e == nil {
		second()
		t.Fatal("acquired second lock")
	}
	var busy *BusyError
	if !errors.As(e, &busy) {
		t.Fatal(e)
	}
}

func TestCLIHelpAndVersionRequireNoServiceOrNetwork(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"version"}, {"install", "worker", "--help"}} {
		var stdout, stderr bytes.Buffer
		if code := Execute(context.Background(), args, &stdout, &stderr); code != 0 || stdout.Len() == 0 || stderr.Len() != 0 {
			t.Fatalf("%v: %d %s", args, code, stderr.String())
		}
	}
}

func TestCLIParsesExistingInstallAndUpdateCommands(t *testing.T) {
	for _, args := range [][]string{
		{"install", "worker", "--config", "./worker.json", "--auto-update"},
		{"install", "gateway", "--config", "./gateway.json", "--secrets-env", "./secrets.env", "--version=v1.2.3"},
		{"adopt", "worker", "--auto-update"},
		{"update", "gateway", "--check"},
		{"auto-update", "disable", "worker"},
	} {
		if _, e := parseOptions(args); e != nil {
			t.Fatal(args, e)
		}
	}
	for _, args := range [][]string{{"install", "worker"}, {"install", "worker", "--config", "x", "extra"}, {"auto-update", "maybe", "worker"}, {"update", "gateway", "--version=v1.0.0-rc1"}, {"adopt", "worker", "--version=v1.0.0"}, {"update", "unknown"}} {
		if _, e := parseOptions(args); e == nil {
			t.Fatal("accepted", args)
		}
	}
}

func TestCommandErrorsDoNotExposeOutputOrArguments(t *testing.T) {
	m := New(nil)
	m.Run = func(context.Context, ...string) (CommandResult, error) {
		return CommandResult{Output: []byte("private-value"), ExitCode: 1}, nil
	}
	_, e := m.command(context.Background(), "/bin/example", "--token", "private-value")
	if e == nil || bytes.Contains([]byte(e.Error()), []byte("private-value")) {
		t.Fatal(e)
	}
}

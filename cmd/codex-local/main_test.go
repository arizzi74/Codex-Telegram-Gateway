package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func TestAttachCommandUsesRemoteResume(t *testing.T) {
	if got, want := attachCommand("/tmp/private.sock", "thr_1"), []string{"--remote", "unix:///tmp/private.sock", "resume", "thr_1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attach args = %#v, want %#v", got, want)
	}
	if got, want := attachCommand("/tmp/private.sock", ""), []string{"--remote", "unix:///tmp/private.sock", "resume"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("picker args = %#v, want %#v", got, want)
	}
}

func TestAttachLaunchesCodexInCurrentDirectory(t *testing.T) {
	binDir := t.TempDir()
	// An actual child process records the arguments and working directory seen
	// by Codex, without connecting to a worker or starting an app server.
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte("#!/bin/sh\nprintf '%s\\n' \"$PWD\" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "existing.sock")
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"latest", []string{"--socket", socket, "--latest"}, []string{"--cd", cwd, "resume", "--last", "--include-non-interactive"}},
		{"latest before socket", []string{"--latest", "--socket", socket}, []string{"--cd", cwd, "resume", "--last", "--include-non-interactive"}},
		{"picker", []string{"--socket", socket}, []string{"resume"}},
		{"explicit thread", []string{"--socket", socket, "thr_1"}, []string{"resume", "thr_1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(append([]string{"attach"}, tc.args...), &stdout, &stderr); err != nil {
				t.Fatalf("attach: %v; stderr: %s", err, stderr.String())
			}
			want := append([]string{cwd, "--remote", "unix://" + socket}, tc.want...)
			if got := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n"); !reflect.DeepEqual(got, want) {
				t.Fatalf("Codex child received %#v, want %#v", got, want)
			}
		})
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing socket", []string{"--latest"}},
		{"relative socket", []string{"--socket", "relative.sock", "--latest"}},
		{"latest and thread", []string{"--socket", socket, "--latest", "thr_1"}},
		{"latest after thread", []string{"--socket", socket, "thr_1", "--latest"}},
		{"extra thread", []string{"--socket", socket, "thr_1", "thr_2"}},
		{"unknown flag", []string{"--socket", socket, "--unknown"}},
		{"unknown flag after thread", []string{"--socket", socket, "thr_1", "--unknown"}},
		{"flag after delimiter", []string{"--socket", socket, "--", "--unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(append([]string{"attach"}, tc.args...), &stdout, &stderr); err == nil {
				t.Fatal("invalid attach arguments succeeded")
			}
			if stdout.Len() != 0 {
				t.Fatalf("invalid attach arguments launched Codex: %s", stdout.String())
			}
		})
	}
}

type localSessions struct {
	latest                     codexadapter.Thread
	found                      bool
	resumeErr                  error
	lookedUp, resumed, created string
}

func (s *localSessions) LatestThread(_ context.Context, cwd string) (codexadapter.Thread, bool, error) {
	s.lookedUp = cwd
	return s.latest, s.found, nil
}
func (s *localSessions) ResumeThread(_ context.Context, id string, _ codexadapter.ThreadOptions) (codexadapter.Thread, error) {
	s.resumed = id
	return s.latest, s.resumeErr
}
func (s *localSessions) StartThread(_ context.Context, options codexadapter.ThreadOptions) (codexadapter.Thread, error) {
	s.created = options.CWD
	return codexadapter.Thread{ID: "new"}, nil
}

func TestResumeLatestPreservesDirectoryAndDoesNotSkipLockedThread(t *testing.T) {
	sessions := &localSessions{latest: codexadapter.Thread{ID: "latest"}, found: true}
	thread, err := resumeLatest(context.Background(), sessions, "/work/project")
	if err != nil || thread.ID != "latest" || sessions.lookedUp != "/work/project" || sessions.resumed != "latest" || sessions.created != "" {
		t.Fatal("latest directory session was not resumed", thread, err, sessions)
	}
	sessions.resumeErr = errors.New("already has an active writer")
	if _, err := resumeLatest(context.Background(), sessions, "/work/project"); err == nil || !strings.Contains(err.Error(), "close it there") || sessions.created != "" {
		t.Fatal("locked latest thread should produce guidance without creating another session", err)
	}
	sessions = &localSessions{}
	thread, err = resumeLatest(context.Background(), sessions, "/work/empty")
	if err != nil || thread.ID != "" || sessions.created != "" || sessions.resumed != "" {
		t.Fatal("empty directory should let the terminal create its own initial session", thread, err, sessions)
	}
}

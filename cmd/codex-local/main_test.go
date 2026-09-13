package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func TestAttachCommandUsesRemoteResume(t *testing.T) {
	if got, want := attachCommand("/tmp/private.sock", "thr_1"), []string{"--remote", "unix:///tmp/private.sock", "resume", "thr_1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attach args = %#v, want %#v", got, want)
	}
	if got, want := attachCommand("/tmp/private.sock", "--last"), []string{"--remote", "unix:///tmp/private.sock", "resume", "--last"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("last args = %#v, want %#v", got, want)
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

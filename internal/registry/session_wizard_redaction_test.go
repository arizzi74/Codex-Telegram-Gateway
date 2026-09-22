package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
)

func TestSessionWizardAcceptsRedactedNameWhileCheckingCreatedWorkspace(t *testing.T) {
	env := newEventTestEnv(t)
	ctx := context.Background()
	browse := wizardBrowseReadyTest(t, env)
	creating := wizardClickTest(t, env, 3, wizardCallbackTest(t, env, browse, "wizard_create", nil))
	session := protocol.Session{ID: uuid.NewString(), ThreadID: "new-thread", Name: "[REDACTED]", CWD: "/home/person/CODEX/wrong-folder", WorkerID: env.worker.String(), RuntimeID: env.runtime.String(), State: "idle", Loaded: true}
	bad := wizardResultEvent(t, env, creating, "command_completed", protocol.Result{Session: &session})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, bad); !errors.Is(err, ErrEventTarget) {
		t.Fatalf("incorrect workspace accepted: %v", err)
	}
	session.CWD = "/home/person/CODEX/My_project"
	good := wizardResultEvent(t, env, creating, "command_completed", protocol.Result{Session: &session})
	if err := env.store.IngestEvent(ctx, env.worker, env.connection, good); err != nil {
		t.Fatal(err)
	}
	done := wizardLastResponse(t, env)
	if done.View != "selected" || done.SessionID != session.ID {
		t.Fatalf("redacted creation failed: %#v", done)
	}
}

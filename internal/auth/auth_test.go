package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerTokenHashAndVerification(t *testing.T) {
	token, err := GenerateWorkerToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "cwk_") || len(token) != len("cwk_")+64 {
		t.Fatalf("unexpected token format")
	}
	hash := HashWorkerToken(token)
	if !VerifyWorkerToken(token, hash) {
		t.Fatal("generated token did not verify")
	}
	if VerifyWorkerToken(token+"x", hash) || VerifyWorkerToken(token, hash[:31]) {
		t.Fatal("invalid token/hash authorized")
	}
}

func TestWebhookAndTelegramAuthorization(t *testing.T) {
	if !ValidateWebhookSecret("secret", "secret") || ValidateWebhookSecret("secret", "other") || ValidateWebhookSecret("", "") {
		t.Fatal("unexpected webhook secret result")
	}
	if !AuthorizedTelegramUser(42, []int64{42}) {
		t.Fatal("allowed ID denied")
	}
	if AuthorizedTelegramUser(43, []int64{42}) || AuthorizedTelegramUser(0, []int64{0}) {
		t.Fatal("unauthorized numeric ID allowed")
	}
}

func TestCanonicalWorkspaceRejectsTraversalAndSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	inside := filepath.Join(root, "project")
	outside := filepath.Join(base, "outside")
	for _, path := range []string{inside, outside} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := CanonicalWorkspace(filepath.Join(inside, "..", "project"), []string{root}); err != nil || got != inside {
		t.Fatalf("inside traversal canonicalization: %q, %v", got, err)
	}
	if _, err := CanonicalWorkspace(filepath.Join(root, "..", "outside"), []string{root}); err == nil {
		t.Fatal("parent traversal escaped root")
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalWorkspace(link, []string{root}); err == nil {
		t.Fatal("symlink escape authorized")
	}
}

func TestRedactor(t *testing.T) {
	r, err := NewRedactor([]string{`cwk_[a-z0-9]+`, `token=[^ ]+`}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Redact("cwk_abc token=shh okay"); got != "[REDACTED] [REDACTED] okay" {
		t.Fatalf("got %q", got)
	}
}

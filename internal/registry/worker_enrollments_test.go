package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
)

func enrollmentInput(code string) RedeemWorkerEnrollmentInput {
	return RedeemWorkerEnrollmentInput{Code: code, Name: "Test worker", OS: "linux", Arch: "arm64"}
}

func enrollmentCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	var count int
	if err := s.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestWorkerEnrollmentStoresHashesAndCreatesWorkerOnlyOnRedemption(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	e, code, err := s.CreateWorkerEnrollment(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID == uuid.Nil || e.ServiceAccess != "restricted" || e.ExpiresAt.Sub(e.CreatedAt) != 10*time.Minute || len(code) != 12 {
		t.Fatalf("incomplete enrollment metadata: %+v", e)
	}
	for _, symbol := range code {
		if !strings.ContainsRune(workerEnrollmentCodeAlphabet, symbol) {
			t.Fatal("generated code outside canonical alphabet")
		}
	}
	if enrollmentCount(t, s, "workers") != 0 || enrollmentCount(t, s, "worker_event_watermarks") != 0 {
		t.Fatal("creating an enrollment created a worker")
	}
	var codeHash []byte
	if err = s.pool.QueryRow(ctx, `SELECT code_hash FROM worker_enrollments WHERE enrollment_id=$1`, e.ID).Scan(&codeHash); err != nil {
		t.Fatal(err)
	}
	expectedCodeHash := sha256.Sum256([]byte(code))
	if !bytes.Equal(codeHash, expectedCodeHash[:]) || bytes.Contains(codeHash, []byte(code)) {
		t.Fatal("code was not persisted as its hash")
	}
	result, err := s.RedeemWorkerEnrollment(ctx, enrollmentInput(strings.ToLower(code)))
	if err != nil {
		t.Fatal(err)
	}
	if result.WorkerID == uuid.Nil || result.ServiceAccess != "restricted" || !strings.HasPrefix(result.Token, "cwk_") || len(result.Token) != 68 {
		t.Fatal("redemption did not return complete credentials")
	}
	worker, err := s.AuthenticateWorker(ctx, result.Token)
	if err != nil || worker.ID != result.WorkerID || worker.Name != "Test worker" || worker.OS != "linux" || worker.Arch != "arm64" {
		t.Fatalf("worker enrollment mismatch: %+v %v", worker, err)
	}
	var tokenHash []byte
	if err = s.pool.QueryRow(ctx, `SELECT auth_token_hash FROM workers WHERE worker_id=$1`, result.WorkerID).Scan(&tokenHash); err != nil || !bytes.Equal(tokenHash, auth.HashWorkerToken(result.Token)) {
		t.Fatal("worker token was not persisted as its hash")
	}
	var consumed *time.Time
	var owner uuid.UUID
	if err = s.pool.QueryRow(ctx, `SELECT consumed_at, worker_id FROM worker_enrollments WHERE enrollment_id=$1`, e.ID).Scan(&consumed, &owner); err != nil || consumed == nil || owner != worker.ID {
		t.Fatal("enrollment and worker were not bound atomically")
	}
	if watermark, err := s.EventWatermark(ctx, worker.ID); err != nil || watermark != 0 {
		t.Fatalf("watermark = %d, %v", watermark, err)
	}
	if _, err = s.RedeemWorkerEnrollment(ctx, enrollmentInput(code)); !errors.Is(err, ErrWorkerEnrollmentGone) {
		t.Fatalf("used enrollment accepted: %v", err)
	}
	if enrollmentCount(t, s, "workers") != 1 || enrollmentCount(t, s, "worker_event_watermarks") != 1 {
		t.Fatal("replay created additional state")
	}
}

func TestWorkerEnrollmentExpiryBoundaryAndInvalidCodesDoNotGenerateTokens(t *testing.T) {
	s := integrationStore(t)
	now := time.Date(2026, time.October, 2, 10, 0, 0, 123456789, time.UTC)
	e, code, err := s.createWorkerEnrollment(t.Context(), "full", now)
	if err != nil {
		t.Fatal(err)
	}
	generated := 0
	generator := func() (string, error) { generated++; return "cwk_boundary_token", nil }
	for _, at := range []time.Time{e.ExpiresAt, e.ExpiresAt.Add(time.Nanosecond)} {
		if _, err = s.redeemWorkerEnrollment(t.Context(), enrollmentInput(code), func() time.Time { return at }, generator); !errors.Is(err, ErrWorkerEnrollmentGone) {
			t.Fatalf("expiry boundary accepted at %s: %v", at, err)
		}
	}
	if _, err = s.redeemWorkerEnrollment(t.Context(), enrollmentInput("AAAAAAAAAAAA"), func() time.Time { return now }, generator); !errors.Is(err, ErrWorkerEnrollmentGone) {
		t.Fatalf("unknown enrollment result: %v", err)
	}
	if generated != 0 || enrollmentCount(t, s, "workers") != 0 {
		t.Fatal("invalid capability generated credentials")
	}
	result, err := s.redeemWorkerEnrollment(t.Context(), enrollmentInput(code), func() time.Time { return e.ExpiresAt.Add(-time.Nanosecond) }, generator)
	if err != nil || generated != 1 || result.ServiceAccess != "full" {
		t.Fatalf("last valid instant rejected: %v", err)
	}
	if _, err = s.redeemWorkerEnrollment(t.Context(), enrollmentInput(code), func() time.Time { return now }, generator); !errors.Is(err, ErrWorkerEnrollmentGone) || generated != 1 {
		t.Fatal("used code generated more credentials")
	}
	revoked, revokedCode, err := s.createWorkerEnrollment(t.Context(), "restricted", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeWorkerEnrollment(t.Context(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.redeemWorkerEnrollment(t.Context(), enrollmentInput(revokedCode), func() time.Time { return now }, generator); !errors.Is(err, ErrWorkerEnrollmentGone) || generated != 1 {
		t.Fatal("revoked code generated credentials")
	}
}

func TestWorkerEnrollmentConcurrentRedemptionAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	first, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err = first.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_, code, err := first.CreateWorkerEnrollment(t.Context(), "full")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 24)
	var wait sync.WaitGroup
	for i := range 24 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			store := first
			if i%2 == 1 {
				store = second
			}
			_, err := store.RedeemWorkerEnrollment(context.Background(), enrollmentInput(code))
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	success, gone := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrWorkerEnrollmentGone):
			gone++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || gone != 23 || enrollmentCount(t, first, "workers") != 1 || enrollmentCount(t, first, "worker_event_watermarks") != 1 {
		t.Fatalf("concurrent redemption: success=%d unavailable=%d", success, gone)
	}
}

func TestWorkerEnrollmentSurvivesRestartAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, unused, err := s.CreateWorkerEnrollment(t.Context(), "full")
	if err != nil {
		t.Fatal(err)
	}
	revoked, revokedCode, err := s.CreateWorkerEnrollment(t.Context(), "restricted")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeWorkerEnrollment(t.Context(), revoked.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeWorkerEnrollment(t.Context(), revoked.ID); err != nil {
		t.Fatal("repeated unused-link revocation failed")
	}
	used, usedCode, err := s.CreateWorkerEnrollment(t.Context(), "restricted")
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := s.RedeemWorkerEnrollment(t.Context(), enrollmentInput(usedCode))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{revokedCode, usedCode} {
		if _, err = s.RedeemWorkerEnrollment(t.Context(), enrollmentInput(code)); !errors.Is(err, ErrWorkerEnrollmentGone) {
			t.Fatalf("terminal code became available after restart: %v", err)
		}
	}
	if _, err = s.RedeemWorkerEnrollment(t.Context(), enrollmentInput(unused)); err != nil {
		t.Fatalf("unused link lost after restart: %v", err)
	}
	if err = s.RevokeWorkerEnrollment(t.Context(), used.ID); !errors.Is(err, ErrWorkerEnrollmentGone) {
		t.Fatal("revoked a consumed enrollment")
	}
	if _, err = s.AuthenticateWorker(t.Context(), credentials.Token); err != nil {
		t.Fatal("revoking a used link affected the enrolled worker")
	}
}

func TestWorkerEnrollmentFailureRollsBackEveryWrite(t *testing.T) {
	t.Run("token generation", func(t *testing.T) {
		s := integrationStore(t)
		_, code, err := s.CreateWorkerEnrollment(t.Context(), "restricted")
		if err != nil {
			t.Fatal(err)
		}
		result, err := s.redeemWorkerEnrollment(t.Context(), enrollmentInput(code), time.Now, func() (string, error) {
			return "", errors.New("private-entropy-error")
		})
		if err == nil || strings.Contains(err.Error(), "private-entropy-error") || result.Token != "" || result.WorkerID != uuid.Nil || enrollmentCount(t, s, "workers") != 0 {
			t.Fatal("token generation failure leaked credentials or changed the registry")
		}
		if _, err = s.RedeemWorkerEnrollment(t.Context(), enrollmentInput(code)); err != nil {
			t.Fatal("token generation failure consumed the enrollment")
		}
	})
	for _, stage := range []string{"worker", "watermark", "consume"} {
		t.Run(stage, func(t *testing.T) {
			s := integrationStore(t)
			_, code, err := s.CreateWorkerEnrollment(t.Context(), "restricted")
			if err != nil {
				t.Fatal(err)
			}
			trigger := map[string]string{
				"worker":    "BEFORE INSERT ON workers",
				"watermark": "BEFORE INSERT ON worker_event_watermarks",
				"consume":   "BEFORE UPDATE ON worker_enrollments",
			}[stage]
			if _, err = s.pool.Exec(t.Context(), "CREATE TRIGGER enrollment_test_failure "+trigger+" BEGIN SELECT RAISE(ABORT,'test failure'); END"); err != nil {
				t.Fatal(err)
			}
			result, err := s.RedeemWorkerEnrollment(t.Context(), enrollmentInput(code))
			if err == nil || result.Token != "" || result.WorkerID != uuid.Nil {
				t.Fatal("failed transaction returned credentials")
			}
			var consumed *time.Time
			var owner *uuid.UUID
			if err = s.pool.QueryRow(t.Context(), `SELECT consumed_at, worker_id FROM worker_enrollments`).Scan(&consumed, &owner); err != nil || consumed != nil || owner != nil {
				t.Fatal("failed transaction consumed enrollment")
			}
			if enrollmentCount(t, s, "workers") != 0 || enrollmentCount(t, s, "worker_event_watermarks") != 0 {
				t.Fatal("failed transaction retained worker state")
			}
			if _, err = s.pool.Exec(t.Context(), "DROP TRIGGER enrollment_test_failure"); err != nil {
				t.Fatal(err)
			}
			if _, err = s.RedeemWorkerEnrollment(t.Context(), enrollmentInput(code)); err != nil {
				t.Fatalf("failed transaction prevented retry: %v", err)
			}
		})
	}
}

func TestWorkerEnrollmentMalformedInputsDoNotConsume(t *testing.T) {
	s := integrationStore(t)
	_, code, err := s.CreateWorkerEnrollment(t.Context(), "restricted")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "  ", "line\nbreak", "line\rbreak", "line\u2028break", "nul\x00", string([]byte{0xff}), strings.Repeat("é", 161)} {
		in := enrollmentInput(code)
		in.Name = name
		if _, err = s.RedeemWorkerEnrollment(t.Context(), in); !errors.Is(err, ErrWorkerEnrollmentInput) {
			t.Fatalf("invalid name accepted: %v", err)
		}
	}
	for _, in := range []RedeemWorkerEnrollmentInput{
		{Code: "invalid", Name: "Worker", OS: "linux", Arch: "amd64"},
		{Code: code, Name: "Worker", OS: "windows", Arch: "amd64"},
		{Code: code, Name: "Worker", OS: "linux", Arch: "386"},
	} {
		if _, err = s.RedeemWorkerEnrollment(t.Context(), in); !errors.Is(err, ErrWorkerEnrollmentInput) {
			t.Fatalf("invalid input accepted: %v", err)
		}
	}
	in := enrollmentInput(code)
	in.Name = strings.Repeat("é", 160)
	if _, err = s.RedeemWorkerEnrollment(t.Context(), in); err != nil {
		t.Fatalf("valid UTF-8 name rejected: %v", err)
	}
}

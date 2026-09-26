package releasemanager

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequestedWorkerUpdateRetriesIncompletePackageDownloads(t *testing.T) {
	for _, failure := range []string{"local download", "local binary validation"} {
		t.Run(failure, func(t *testing.T) {
			f := newCodexWiringFixture(t)
			f.projectTag = "v1.2.4"
			workerAsset := "codex-worker-" + f.l.System + "-" + f.l.Architecture + ".tar.gz"
			localAsset := "codex-local-" + f.l.System + "-" + f.l.Architecture + ".tar.gz"
			workerDownloads, localDownloads := 0, 0
			transport := f.m.HTTP.Transport
			f.m.HTTP.Transport = coreRoundTripper(func(request *http.Request) (*http.Response, error) {
				switch filepath.Base(request.URL.Path) {
				case workerAsset:
					workerDownloads++
				case localAsset:
					localDownloads++
					if failure == "local download" && localDownloads == 1 {
						response := coreResponse(request, []byte("temporary asset failure"))
						// A failed download attempt reaches the request-level retry
						// without introducing a real network backoff in this test.
						response.StatusCode = http.StatusForbidden
						return response, nil
					}
				}
				return transport.RoundTrip(request)
			})
			var failedExtraction string
			run := f.m.Run
			f.m.Run = func(ctx context.Context, args ...string) (CommandResult, error) {
				if failure == "local binary validation" && len(args) == 2 && args[1] == "version" &&
					filepath.Base(args[0]) == "codex-telegramgw" && filepath.Base(filepath.Dir(args[0])) == "codex-local" && failedExtraction == "" {
					failedExtraction = filepath.Dir(args[0])
					return CommandResult{ExitCode: 1}, nil
				}
				return run(ctx, args...)
			}

			plan := &requestedWorkerPlan{stage: t.TempDir()}
			_, err := f.m.requestedWorkerStep(t.Context(), f.l, plan)
			var busy *BusyError
			if err == nil || errors.As(err, &busy) || f.prepares != 0 || workerDownloads != 1 || localDownloads != 1 {
				t.Fatalf("first package failure was not isolated: err=%v prepares=%d worker=%d local=%d", err, f.prepares, workerDownloads, localDownloads)
			}
			workerPackage := plan.packages["worker"]
			if workerPackage == "" || plan.packages["local"] != "" {
				t.Fatalf("successful worker package was not retained independently: %v", plan.packages)
			}
			// The retry must complete both package validations and reach the
			// worker's idle fence. This fixture keeps a turn active throughout.
			_, err = f.m.requestedWorkerStep(t.Context(), f.l, plan)
			if !errors.As(err, &busy) || f.prepares != 1 || workerDownloads != 1 || localDownloads != 2 || plan.packages["worker"] != workerPackage || plan.packages["local"] == "" {
				t.Fatalf("retry did not recover without redownloading the worker: err=%v prepares=%d worker=%d local=%d packages=%v", err, f.prepares, workerDownloads, localDownloads, plan.packages)
			}
			if failure == "local binary validation" && (failedExtraction == "" || failedExtraction == plan.packages["local"]) {
				t.Fatal("retry reused a partially validated extraction directory")
			}
			_, err = f.m.requestedWorkerStep(t.Context(), f.l, plan)
			if !errors.As(err, &busy) || f.prepares != 2 || workerDownloads != 1 || localDownloads != 2 || f.checks != 1 {
				t.Fatalf("busy polling redownloaded packages or repeated the runtime check: err=%v prepares=%d worker=%d local=%d runtime=%d", err, f.prepares, workerDownloads, localDownloads, f.checks)
			}
			if installed := updateRead(t, f.l.Binary); installed != "worker" || FileExists(f.l.Backups) || !strings.Contains(busy.Reason, "active turn") {
				t.Fatalf("package recovery changed the busy installation: binary=%q backups=%t reason=%q", installed, FileExists(f.l.Backups), busy.Reason)
			}
		})
	}
}

package releasemanager

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type codexWiringFixture struct {
	m                        *Manager
	l                        *Layout
	projectTag, runtimeTag   string
	projectFailure           bool
	checks, prepares, assets int
}

func codexWiringArchive(t *testing.T) []byte {
	t.Helper()
	files := map[string][]byte{"codex-worker": []byte("next worker"), "codex-local": []byte("next helper"), "codex-telegramgw": []byte("next manager")}
	manifest := ""
	for _, name := range []string{"codex-worker", "codex-local", "codex-telegramgw"} {
		manifest += coreHash(files[name]) + "  " + name + "\n"
	}
	files["SHA256SUMS"] = []byte(manifest)
	var archive bytes.Buffer
	zipped := gzip.NewWriter(&archive)
	writer := tar.NewWriter(zipped)
	for _, name := range []string{"codex-worker", "codex-local", "codex-telegramgw", "SHA256SUMS"} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func newCodexWiringFixture(t *testing.T) *codexWiringFixture {
	t.Helper()
	m, l := coreWorkerHome(t)
	// This fixture exercises update scheduling; cryptographic verification has
	// separate signed-bundle tests in releaseauth and fail-closed download tests.
	m.verifyManifest = func(string, string, []byte, []byte) error { return nil }
	_, binary, _, _ := codexDistributionFixture(t)
	f := &codexWiringFixture{m: m, l: l, projectTag: "v1.2.3", runtimeTag: "0.155.0"}
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	if err := SaveSettings(l, DefaultRepo, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(l.Config, map[string]any{"worker_id": "example-worker", "runtimes": []any{
		map[string]any{"id": "main", "codex_binary": binary, "autostart": true},
	}}); err != nil {
		t.Fatal(err)
	}
	archive := codexWiringArchive(t)
	workerAsset := "codex-worker-" + l.System + "-" + l.Architecture + ".tar.gz"
	localAsset := "codex-local-" + l.System + "-" + l.Architecture + ".tar.gz"
	m.HTTP.Transport = coreRoundTripper(func(request *http.Request) (*http.Response, error) {
		address := request.URL.String()
		prefix := "https://github.com/" + DefaultRepo + "/releases/download/" + f.projectTag + "/"
		switch address {
		case codexLatestURL:
			f.checks++
			// The deferred runtime work must finish before execute releases the
			// shared installation lock, including when project updates fail.
			unlock, err := Lock(l.Lock)
			if err == nil {
				unlock()
				t.Fatal("runtime discovery ran outside the worker update lock")
			}
			return coreResponse(request, []byte(`{"tag_name":"rust-v`+f.runtimeTag+`"}`)), nil
		case "https://api.github.com/repos/" + DefaultRepo + "/releases/latest":
			if f.projectFailure {
				response := coreResponse(request, []byte("private project release error"))
				response.StatusCode = http.StatusForbidden
				return response, nil
			}
			assets := make([]map[string]string, 0, 3)
			for _, name := range []string{"SHA256SUMS", "SHA256SUMS.sigstore.json", workerAsset, localAsset} {
				assets = append(assets, map[string]string{"name": name, "browser_download_url": prefix + name})
			}
			metadata, _ := json.Marshal(map[string]any{"tag_name": f.projectTag, "draft": false, "prerelease": false, "assets": assets})
			return coreResponse(request, metadata), nil
		case prefix + "SHA256SUMS":
			return coreResponse(request, []byte(coreHash(archive)+"  "+workerAsset+"\n"+coreHash(archive)+"  "+localAsset+"\n")), nil
		case prefix + "SHA256SUMS.sigstore.json":
			return coreResponse(request, []byte("fixture bundle")), nil
		case prefix + workerAsset, prefix + localAsset:
			f.assets++
			return coreResponse(request, archive), nil
		default:
			t.Fatalf("unexpected network request: %s", request.URL)
			return nil, errors.New("unexpected request")
		}
	})
	m.CodexRun = func(context.Context, ...string) (CommandResult, error) {
		t.Fatal("check-only or busy worker invoked a native installer")
		return CommandResult{}, nil
	}
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		switch {
		case reflect.DeepEqual(args, []string{binary, "--version"}):
			return CommandResult{Output: []byte("codex-cli 0.155.0")}, nil
		case reflect.DeepEqual(args, []string{l.Binary, "version"}):
			return CommandResult{Output: []byte("1.2.3")}, nil
		case len(args) == 2 && args[1] == "version" && (filepath.Base(args[0]) == "codex-worker" || filepath.Base(args[0]) == "codex-telegramgw"):
			return CommandResult{Output: []byte(strings.TrimPrefix(f.projectTag, "v"))}, nil
		case reflect.DeepEqual(args, []string{"systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "-p", "ActiveState"}):
			return CommandResult{Output: []byte("MainPID=123\nActiveState=active\n")}, nil
		case len(args) == 3 && args[0] == "launchctl" && args[1] == "print":
			return CommandResult{Output: []byte("pid = 123\n")}, nil
		case reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "status"}):
			data, _ := json.Marshal(map[string]any{"worker_id": "example-worker", "pid": 123, "updated_at": now, "runtimes": []any{
				map[string]any{"profile_id": "main", "codex_version": "codex-cli 0.155.0", "state": "running"},
			}})
			return CommandResult{Output: data}, nil
		case reflect.DeepEqual(args, []string{l.Binary, "--config", l.Config, "update", "prepare"}):
			f.prepares++
			return CommandResult{ExitCode: 75, Output: []byte(`{"error":"worker update: a session has an active turn or pending response"}`)}, nil
		default:
			t.Fatalf("unexpected command or service mutation: %v", args)
			return CommandResult{}, nil
		}
	}
	return f
}

func TestWorkerUpdateChecksCodexEvenWhenWorkerIsAlreadyCurrent(t *testing.T) {
	f := newCodexWiringFixture(t)
	for range 2 {
		if err := f.m.execute(t.Context(), options{Action: "update", Component: "worker"}); err != nil {
			t.Fatal(err)
		}
	}
	state, err := loadCodexUpdateState(f.l)
	if err != nil || f.checks != 1 || f.prepares != 0 || f.assets != 0 || state.LatestVersion != "0.155.0" || state.CheckFailed {
		t.Fatalf("current worker runtime checks: checks=%d prepares=%d assets=%d state=%+v err=%v", f.checks, f.prepares, f.assets, state, err)
	}
}

func TestWorkerProjectReleaseFailureDoesNotStarveDailyCodexCheck(t *testing.T) {
	f := newCodexWiringFixture(t)
	f.projectFailure, f.runtimeTag = true, "0.156.0"
	err := f.m.execute(t.Context(), options{Action: "update", Component: "worker"})
	var busy *BusyError
	if err == nil || errors.As(err, &busy) || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("project failure was lost or converted into a busy deferral: %v", err)
	}
	state, stateErr := loadCodexUpdateState(f.l)
	if stateErr != nil || f.checks != 1 || f.prepares != 1 || state.LatestVersion != "0.156.0" || state.CheckFailed {
		t.Fatalf("project outage starved Codex check: checks=%d prepares=%d state=%+v err=%v", f.checks, f.prepares, state, stateErr)
	}
}

func TestWorkerCheckOnlyIncludesCodexWithoutInstallingOrWritingState(t *testing.T) {
	f := newCodexWiringFixture(t)
	f.projectTag, f.runtimeTag = "v1.2.4", "0.156.0"
	before, err := os.ReadFile(f.l.State)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.execute(t.Context(), options{Action: "update", Component: "worker", Check: true}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(f.l.State)
	if err != nil || !bytes.Equal(before, after) || FileExists(codexUpdateStatePath(f.l)) || f.checks != 1 || f.prepares != 0 || f.assets != 0 {
		t.Fatalf("check-only mutated state or installed: checks=%d prepares=%d assets=%d err=%v", f.checks, f.prepares, f.assets, err)
	}
}

func TestWorkerReleaseBusyDeferralStillRecordsDailyCodexDiscovery(t *testing.T) {
	f := newCodexWiringFixture(t)
	f.projectTag, f.runtimeTag = "v1.2.4", "0.156.0"
	err := f.m.execute(t.Context(), options{Action: "update", Component: "worker"})
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("worker busy deferral was lost: %v", err)
	}
	state, stateErr := loadCodexUpdateState(f.l)
	if stateErr != nil || f.checks != 1 || f.prepares != 2 || f.assets != 2 || state.LatestVersion != "0.156.0" {
		t.Fatalf("busy worker starved Codex check: checks=%d prepares=%d assets=%d state=%+v err=%v", f.checks, f.prepares, f.assets, state, stateErr)
	}
	if got := updateRead(t, f.l.Binary); got != "worker" {
		t.Fatal(fmt.Sprintf("busy worker binary was replaced: %q", got))
	}
}

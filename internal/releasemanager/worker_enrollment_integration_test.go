package releasemanager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/admin"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

// Exercise the actual installer/gateway wire contract against HTTPS and an
// isolated SQLite file. No installed service or real enrollment is touched.
func TestWorkerEnrollmentInstallerGatewayIntegration(t *testing.T) {
	for _, access := range []string{"restricted", "full"} {
		t.Run(access, func(t *testing.T) {
			ctx := context.Background()
			store, err := registry.Open(ctx, filepath.Join(t.TempDir(), "gateway.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(store.Close)
			if err := store.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(nil)
			t.Cleanup(server.Close)
			address := server.Listener.Addr().String()
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				t.Fatal(err)
			}
			origin := "https://example.com:" + port
			console, err := admin.New(store, admin.Config{Origin: origin})
			if err != nil {
				t.Fatal(err)
			}
			server.Config.Handler = console
			server.StartTLS()
			client := server.Client()
			transport := client.Transport.(*http.Transport).Clone()
			transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			client.Transport = transport
			t.Cleanup(transport.CloseIdleConnections)
			_, code, err := store.CreateWorkerEnrollment(ctx, access)
			if err != nil {
				t.Fatal(err)
			}
			link := origin + protocol.WorkerEnrollmentPath + "#" + code
			response, err := client.Get(link)
			if err != nil {
				t.Fatal("enrollment landing page request failed")
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" || bytes.Contains(body, []byte(code)) {
				t.Fatal("landing page must be safe to open without consuming or exposing the enrollment code")
			}
			before, err := store.ListWorkers(ctx)
			if err != nil || len(before) != 0 {
				t.Fatal("creating/opening a link must not create a worker")
			}
			parsed, err := parseWorkerEnrollmentURL(link)
			if err != nil {
				t.Fatal("installer rejected a gateway-generated link")
			}
			var output bytes.Buffer
			manager := New(&output)
			manager.HTTP = client
			layout := &Layout{System: "linux", Architecture: "arm64"}
			result, err := manager.redeemWorkerEnrollment(ctx, parsed, "Development 日本", layout)
			if err != nil {
				t.Fatalf("installer redemption failed: %v", err)
			}
			if result.ServiceAccess != access || result.GatewayURL != strings.Replace(origin, "https://", "wss://", 1)+"/tgw/api/v1/workers/connect" {
				t.Fatal("installer did not receive the configured gateway origin and service access")
			}
			worker, err := store.AuthenticateWorker(ctx, result.Token)
			if err != nil || worker.ID.String() != result.WorkerID || worker.Name != "Development 日本" || worker.OS != "linux" || worker.Arch != "arm64" {
				t.Fatal("redeemed credentials did not authenticate the requested worker")
			}
			if _, err := manager.redeemWorkerEnrollment(ctx, parsed, "Second worker", layout); err == nil {
				t.Fatal("installer reused a single-use enrollment")
			} else {
				var rejected *workerEnrollmentRejected
				if !errors.As(err, &rejected) {
					t.Fatalf("consumed link did not produce a definite rejection: %v", err)
				}
			}
			after, err := store.ListWorkers(ctx)
			if err != nil || len(after) != 1 {
				t.Fatal("redemption did not create exactly one worker")
			}
			if strings.Contains(output.String(), code) || strings.Contains(output.String(), result.Token) {
				t.Fatal("installer output exposed enrollment credentials")
			}
		})
	}
}

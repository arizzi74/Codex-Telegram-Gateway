package releasemanager

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestReleaseProvenanceFailsClosedBeforeAnyPackageDownload(t *testing.T) {
	for _, mode := range []string{"missing", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			m := New(nil) // Use the actual compiled-in verifier.
			prefix := "https://github.com/example/project/releases/download/v1.2.3/"
			assets := []map[string]string{{"name": "SHA256SUMS", "browser_download_url": prefix + "SHA256SUMS"}}
			if mode != "missing" {
				assets = append(assets, map[string]string{"name": "SHA256SUMS.sigstore.json", "browser_download_url": prefix + "SHA256SUMS.sigstore.json"})
			}
			m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				var data []byte
				switch {
				case r.URL.Hostname() == "api.github.com":
					data, _ = json.Marshal(map[string]any{"tag_name": "v1.2.3", "draft": false, "prerelease": false, "assets": assets})
				case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
					data = []byte(coreHash(nil) + "  codex-worker-linux-amd64.tar.gz\n")
				case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.sigstore.json"):
					data = []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)
				default:
					t.Fatalf("downloaded a package before authenticating manifest: %s", r.URL.Path)
				}
				return coreResponse(r, data), nil
			})
			if _, err := m.FetchRelease(t.Context(), "example/project", "v1.2.3"); err == nil {
				t.Fatal("accepted a release without valid provenance")
			}
		})
	}
}

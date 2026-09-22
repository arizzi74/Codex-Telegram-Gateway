package releasemanager

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func coreHash(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }

type coreRoundTripper func(*http.Request) (*http.Response, error)

func (f coreRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func coreResponse(r *http.Request, data []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), Request: r}
}

func TestStableVersionsAndRepositoryNames(t *testing.T) {
	for _, v := range []string{"v1.2.3", "1.2.3", "0.0.0", "v10.20.30\n"} {
		if _, e := ParseVersion(v); e != nil {
			t.Fatal(v, e)
		}
	}
	for _, v := range []string{"dev", "v01.2.3", "v1.2", "v1.2.3-rc1", "1.2.3+build", "v18446744073709551616.0.0"} {
		if _, e := ParseVersion(v); e == nil {
			t.Fatalf("accepted %q", v)
		}
	}
	for _, repo := range []string{"../repo", "owner/..", "owner/repo/extra", "https://github.com/o/r", "owner/repo?x", "owner/repo\n"} {
		if ValidateRepo(repo) == nil {
			t.Fatalf("accepted %q", repo)
		}
	}
	if n, e := CompareVersions("v1.10.0", "1.9.10"); e != nil || n != 1 {
		t.Fatal(n, e)
	}
}

func TestManifestValidatesEveryPathAndDuplicate(t *testing.T) {
	digest := coreHash(nil)
	hashes, e := Manifest([]byte(digest+"  ./program\n"), false)
	if e != nil || hashes["program"] != digest {
		t.Fatal(hashes, e)
	}
	for _, name := range []string{"../evil", "/evil", "scripts/../evil", "scripts//evil", "scripts\\evil", ".", "a b"} {
		if _, e := Manifest([]byte(digest+"  "+name+"\n"), true); e == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if _, e = Manifest([]byte(digest+"  program\n"+digest+"  ./program\n"), false); e == nil {
		t.Fatal("accepted duplicate")
	}
	if _, e = Manifest([]byte(digest+"  scripts/file\n"), false); e == nil {
		t.Fatal("accepted nested outer path")
	}
	if _, e = Manifest([]byte(digest+"  scripts/file\n"), true); e != nil {
		t.Fatal(e)
	}
}

func coreArchive(t *testing.T, root string, extra *tar.Header, corrupt bool) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string][]byte{"codex-worker": []byte("worker"), "codex-telegramgw": []byte("native manager")}
	manifest := ""
	for _, name := range []string{"codex-worker", "codex-telegramgw"} {
		manifest += coreHash(files[name]) + "  " + name + "\n"
	}
	if corrupt {
		files["codex-worker"] = []byte("tampered")
	}
	files["SHA256SUMS"] = []byte(manifest)
	for _, name := range []string{"codex-worker", "codex-telegramgw", "SHA256SUMS"} {
		data := files[name]
		if e := tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0755, Size: int64(len(data)), Typeflag: tar.TypeReg}); e != nil {
			t.Fatal(e)
		}
		if _, e := tw.Write(data); e != nil {
			t.Fatal(e)
		}
	}
	if extra != nil {
		if e := tw.WriteHeader(extra); e != nil {
			t.Fatal(e)
		}
	}
	if e := tw.Close(); e != nil {
		t.Fatal(e)
	}
	if e := gz.Close(); e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(root, "archive.tar.gz")
	if e := os.WriteFile(p, buf.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestArchiveExtractsVerifiedNativePrograms(t *testing.T) {
	root := t.TempDir()
	archive := coreArchive(t, root, nil, false)
	stage := filepath.Join(root, "stage")
	os.Mkdir(stage, 0700)
	if e := UnpackVerified(archive, stage, "codex-worker"); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"codex-worker", "codex-telegramgw"} {
		info, e := os.Stat(filepath.Join(stage, name))
		if e != nil || info.Mode().Perm()&0100 == 0 {
			t.Fatalf("program not executable: %s %v", name, e)
		}
	}
}

func TestArchiveRejectsUnsafeMembersAndTampering(t *testing.T) {
	tests := []struct {
		name    string
		header  *tar.Header
		corrupt bool
	}{
		{"traversal", &tar.Header{Name: "../outside", Typeflag: tar.TypeReg}, false},
		{"absolute", &tar.Header{Name: "/outside", Typeflag: tar.TypeReg}, false},
		{"symlink", &tar.Header{Name: "alias", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, false},
		{"hardlink", &tar.Header{Name: "alias", Typeflag: tar.TypeLink, Linkname: "codex-worker"}, false},
		{"duplicate", &tar.Header{Name: "./codex-worker", Typeflag: tar.TypeReg}, false},
		{"fifo", &tar.Header{Name: "pipe", Typeflag: tar.TypeFifo}, false},
		{"checksum", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			archive := coreArchive(t, root, tc.header, tc.corrupt)
			stage := filepath.Join(root, "stage")
			os.Mkdir(stage, 0700)
			if e := UnpackVerified(archive, stage, "codex-worker"); e == nil {
				t.Fatal("accepted unsafe archive")
			}
			if FileExists(filepath.Join(root, "outside")) {
				t.Fatal("escaped extraction root")
			}
		})
	}
}

func TestArchiveRejectsTruncatedGzipTrailer(t *testing.T) {
	root := t.TempDir()
	archive := coreArchive(t, root, nil, false)
	data, _ := os.ReadFile(archive)
	os.WriteFile(archive, data[:len(data)-5], 0600)
	stage := filepath.Join(root, "stage")
	os.Mkdir(stage, 0700)
	if e := UnpackVerified(archive, stage, "codex-worker"); e == nil {
		t.Fatal("accepted truncated gzip")
	}
}

func TestReleaseMetadataPinnedAndScoped(t *testing.T) {
	for _, tc := range []string{"valid", "draft", "prerelease", "wrong-pin", "wrong-repository", "duplicate-asset", "missing-flags"} {
		t.Run(tc, func(t *testing.T) {
			metadata := map[string]any{"tag_name": "v1.2.3", "draft": false, "prerelease": false}
			asset := map[string]any{"name": "SHA256SUMS", "browser_download_url": "https://github.com/" + DefaultRepo + "/releases/download/v1.2.3/SHA256SUMS"}
			assets := []map[string]any{asset}
			assets = append(assets, map[string]any{"name": "SHA256SUMS.sigstore.json", "browser_download_url": "https://github.com/" + DefaultRepo + "/releases/download/v1.2.3/SHA256SUMS.sigstore.json"})
			pin := "v1.2.3"
			switch tc {
			case "draft":
				metadata["draft"] = true
			case "prerelease":
				metadata["prerelease"] = true
			case "wrong-pin":
				pin = "v2.0.0"
			case "wrong-repository":
				asset["browser_download_url"] = "https://github.com/attacker/repo/releases/download/v1.2.3/SHA256SUMS"
			case "duplicate-asset":
				assets = append(assets, asset)
			case "missing-flags":
				delete(metadata, "draft")
			}
			metadata["assets"] = assets
			encoded, _ := json.Marshal(metadata)
			m := New(nil)
			m.verifyManifest = func(repo, tag string, manifest, bundle []byte) error {
				if repo != DefaultRepo || tag != "v1.2.3" || len(manifest) == 0 || len(bundle) == 0 {
					t.Fatal("incorrect provenance verifier inputs")
				}
				return nil
			}
			calls := 0
			m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				data := encoded
				if r.URL.Hostname() == "github.com" {
					data = []byte(coreHash(nil) + "  codex-worker-linux-amd64.tar.gz\n")
				}
				return coreResponse(r, data), nil
			})
			release, e := m.FetchRelease(context.Background(), DefaultRepo, pin)
			if tc == "valid" {
				if e != nil || release.Tag != "v1.2.3" || calls != 3 {
					t.Fatal(release, e, calls)
				}
			} else if e == nil {
				t.Fatal("accepted invalid metadata")
			}
		})
	}
}

func TestDownloadLimitsAndHostPolicy(t *testing.T) {
	m := New(nil)
	called := false
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
		called = true
		return coreResponse(r, []byte("oversized")), nil
	})
	for _, u := range []string{"http://github.com/x", "https://evil.example/x", "https://token@github.com/x", "https://github.com:444/x", "https://github.com/x#frag"} {
		if _, e := m.Download(context.Background(), u, 100); e == nil {
			t.Fatalf("accepted %q", u)
		}
	}
	if called {
		t.Fatal("contacted rejected host")
	}
	if _, e := m.Download(context.Background(), "https://github.com/x", 3); e == nil {
		t.Fatal("ignored size limit")
	}
	for _, u := range []string{"https://release-assets.githubusercontent.com/x?token=example", "https://objects.githubusercontent.com/x"} {
		if e := githubURL(u, true); e != nil {
			t.Fatal(e)
		}
	}
	for _, u := range []string{"http://github.com/x", "https://evil.example/x", "https://token@github.com/x"} {
		if githubURL(u, true) == nil {
			t.Fatal("unsafe redirect accepted")
		}
	}
}

func TestPackageVerifiesChecksumBeforeExecuting(t *testing.T) {
	root := t.TempDir()
	archive := coreArchive(t, root, nil, false)
	data, _ := os.ReadFile(archive)
	m := New(nil)
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) { return coreResponse(r, data), nil })
	commands := 0
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		commands++
		return CommandResult{Output: []byte("1.2.3\n")}, nil
	}
	l := &Layout{System: "linux", Architecture: "amd64"}
	name := "codex-worker-linux-amd64.tar.gz"
	r := &Release{Tag: "v1.2.3", Assets: map[string]string{name: "https://github.com/" + DefaultRepo + "/releases/download/v1.2.3/" + name}, Hashes: map[string]string{name: strings.Repeat("0", 64)}}
	if _, e := m.Package(context.Background(), r, l, "codex-worker", root); e == nil {
		t.Fatal("accepted invalid outer checksum")
	}
	if commands != 0 {
		t.Fatal("executed unverified payload")
	}
	r.Hashes[name] = coreHash(data)
	if _, e := m.Package(context.Background(), r, l, "codex-worker", root); e != nil {
		t.Fatal(e)
	}
	if commands != 2 {
		t.Fatal("did not verify both program versions")
	}
}

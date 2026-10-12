package speech

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iaia/telegramgw/internal/buildinfo"
)

type speechArchiveEntry struct {
	name string
	kind byte
	mode int64
	body string
	link string
}

func TestSpeechInventoryPreservesModelFilenamesWithSpaces(t *testing.T) {
	dir := privateTestDir(t)
	name := "Mr serious"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("public voice fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest, err := speechInventory(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifySpeechInventory(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseSpeechInventory(manifest)
	if err != nil || parsed[name] == "" {
		t.Fatalf("spaced model filename = %v, %v", parsed, err)
	}
	for _, malformed := range []string{
		strings.Repeat("a", 64) + " ../escape\n",
		strings.Repeat("a", 64) + "  ../escape\n",
		strings.Repeat("a", 64) + "  /escape\n",
		strings.Repeat("g", 64) + "  name\n",
		string(manifest) + string(manifest),
	} {
		if _, err := parseSpeechInventory([]byte(malformed)); err == nil {
			t.Fatalf("unsafe inventory accepted: %q", malformed)
		}
	}
}

func speechTestArchive(t *testing.T, entries []speechArchiveEntry) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	for _, entry := range entries {
		kind := entry.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		mode := entry.mode
		if mode == 0 {
			mode = 0644
		}
		header := &tar.Header{Name: entry.name, Typeflag: kind, Mode: mode, Linkname: entry.link}
		if kind == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := io.WriteString(writer, entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestNativeSpeechArchiveExtractsPrivateRegularFiles(t *testing.T) {
	dir := privateTestDir(t)
	data := speechTestArchive(t, []speechArchiveEntry{
		{name: "lib/", kind: tar.TypeDir, mode: 0777},
		{name: "codex-piper", mode: 0777, body: "public helper fixture"},
		{name: "lib/library.so", mode: 0777, body: "public library fixture"},
	})
	if err := unpackSpeech(t.Context(), bytes.NewReader(data), dir, "codex-piper", "", 100); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"codex-piper": 0700, "lib": 0700, "lib/library.so": 0600} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s permissions = %v, %v", name, info, err)
		}
	}
	model := privateTestDir(t)
	data = speechTestArchive(t, []speechArchiveEntry{{name: piperModelDir + "/model.onnx", body: "public model fixture"}})
	if err := unpackSpeech(t.Context(), bytes.NewReader(data), model, "", piperModelDir, 100); err != nil {
		t.Fatal(err)
	}
}

func TestNativeSpeechArchiveRejectsUnsafeEntries(t *testing.T) {
	cases := map[string][]speechArchiveEntry{
		"parent traversal":    {{name: "../escape", body: "escape"}},
		"absolute path":       {{name: "/escape", body: "escape"}},
		"normalized alias":    {{name: "models/../escape", body: "escape"}},
		"repeated separator":  {{name: "models//voice", body: "escape"}},
		"backslash":           {{name: "models\\voice", body: "escape"}},
		"newline":             {{name: "models/voice\n", body: "escape"}},
		"symlink":             {{name: "link", kind: tar.TypeSymlink, link: "../outside"}},
		"hardlink":            {{name: "link", kind: tar.TypeLink, link: "../outside"}},
		"fifo":                {{name: "pipe", kind: tar.TypeFifo}},
		"device":              {{name: "device", kind: tar.TypeChar}},
		"setuid":              {{name: "helper", mode: 04755, body: "escape"}},
		"setgid":              {{name: "helper", mode: 02755, body: "escape"}},
		"duplicate file":      {{name: "model", body: "first"}, {name: "model", body: "second"}},
		"duplicate directory": {{name: "models/", kind: tar.TypeDir}, {name: "models/", kind: tar.TypeDir}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			dir := privateTestDir(t)
			data := speechTestArchive(t, entries)
			if err := unpackSpeech(t.Context(), bytes.NewReader(data), dir, "helper", "", 100); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	t.Run("model prefix", func(t *testing.T) {
		data := speechTestArchive(t, []speechArchiveEntry{{name: "other/model.onnx", body: "wrong model"}})
		if err := unpackSpeech(t.Context(), bytes.NewReader(data), privateTestDir(t), "", piperModelDir, 100); err == nil {
			t.Fatal("unrelated model path accepted")
		}
	})
	t.Run("preexisting directory symlink", func(t *testing.T) {
		dir, outside := privateTestDir(t), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
			t.Fatal(err)
		}
		data := speechTestArchive(t, []speechArchiveEntry{{name: "linked/escape", body: "escape"}})
		if err := unpackSpeech(t.Context(), bytes.NewReader(data), dir, "", "", 100); err == nil {
			t.Fatal("archive escaped through existing symlink")
		}
		if _, err := os.Stat(filepath.Join(outside, "escape")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("archive wrote outside its private root")
		}
	})
}

func TestNativeSpeechArchiveLimitsCancellationAndTrailer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  []byte
		limit int64
	}{
		{"single file limit", speechTestArchive(t, []speechArchiveEntry{{name: "large", body: "12345"}}), 4},
		{"cumulative size limit", speechTestArchive(t, []speechArchiveEntry{{name: "one", body: "123"}, {name: "two", body: "456"}}), 5},
		{"unexpected trailer", append(speechTestArchive(t, []speechArchiveEntry{{name: "model", body: "data"}}), []byte("unexpected")...), 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := unpackSpeech(t.Context(), bytes.NewReader(tc.data), privateTestDir(t), "", "", tc.limit); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
	t.Run("entry count", func(t *testing.T) {
		entries := make([]speechArchiveEntry, 10001)
		for i := range entries {
			entries[i] = speechArchiveEntry{name: fmt.Sprintf("dirs/d%d/", i), kind: tar.TypeDir}
		}
		data := speechTestArchive(t, entries)
		if err := unpackSpeech(t.Context(), bytes.NewReader(data), privateTestDir(t), "", "", 100); err == nil {
			t.Fatal("unbounded archive entry count accepted")
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		data := speechTestArchive(t, []speechArchiveEntry{{name: "model", body: "data"}})
		if err := unpackSpeech(ctx, bytes.NewReader(data), privateTestDir(t), "", "", 100); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation = %v", err)
		}
	})
	t.Run("gzip checksum", func(t *testing.T) {
		var compressed bytes.Buffer
		z := gzip.NewWriter(&compressed)
		_, _ = z.Write(speechTestArchive(t, []speechArchiveEntry{{name: "model", body: "data"}}))
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		data := compressed.Bytes()
		data[len(data)-8] ^= 1
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		if err := unpackSpeech(t.Context(), reader, privateTestDir(t), "", "", 100); err == nil {
			t.Fatal("bad compression checksum accepted")
		}
	})
}

func TestSpeechInventoryDetectsTamperingAndUnsafeFiles(t *testing.T) {
	for _, change := range []string{"content", "extra file", "missing file", "symlink", "manifest symlink"} {
		t.Run(change, func(t *testing.T) {
			dir := privateTestDir(t)
			file := filepath.Join(dir, "model")
			if err := os.WriteFile(file, []byte("public model fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			manifest, err := speechInventory(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(dir, "SHA256SUMS")
			if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifySpeechInventory(t.Context(), dir); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "content":
				err = os.WriteFile(file, []byte("modified model data!"), 0600)
			case "extra file":
				err = os.WriteFile(filepath.Join(dir, "extra"), []byte("extra"), 0600)
			case "missing file":
				err = os.Remove(file)
			case "symlink":
				err = os.Remove(file)
				if err == nil {
					err = os.Symlink(manifestPath, file)
				}
			case "manifest symlink":
				outside := filepath.Join(t.TempDir(), "manifest")
				if err = os.WriteFile(outside, manifest, 0600); err == nil {
					err = os.Remove(manifestPath)
				}
				if err == nil {
					err = os.Symlink(outside, manifestPath)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := verifySpeechInventory(t.Context(), dir); err == nil {
				t.Fatal("modified or unsafe inventory accepted")
			}
		})
	}
}

func TestNativeSpeechModelPinsAndInstalledRuntimeSelection(t *testing.T) {
	for engine, want := range map[string]int{"piper": 1, "qwen3": 2} {
		models := nativeModels(engine)
		if nativeProgram(engine) == "" || len(models) != want {
			t.Fatalf("invalid %s native inventory", engine)
		}
		for _, spec := range models {
			digest, err := hex.DecodeString(spec.sha256)
			if err != nil || len(digest) != 32 || spec.size <= 0 || !safeSpeechPath(spec.name) || !strings.HasPrefix(spec.url(), "https://") {
				t.Fatalf("unpinned native model: %+v", spec)
			}
			if engine == "qwen3" && !strings.Contains(spec.url(), "/resolve/ca27d74bc954b73dadab5b71ca265d87fc861a7c/") {
				t.Fatal("Qwen model is not pinned to the reviewed immutable revision")
			}
		}
	}
	if nativeProgram("../piper") != "" {
		t.Fatal("arbitrary helper accepted")
	}
	if nativeModels("../piper") != nil {
		t.Fatal("arbitrary engine selected model downloads")
	}
	dir := privateTestDir(t)
	for _, version := range []string{"v1.0.0", "v2.0.0", "v3.0.0"} {
		runtime := filepath.Join(dir, "runtime", version)
		if err := os.MkdirAll(runtime, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtime, "codex-piper"), []byte(version), 0700); err != nil {
			t.Fatal(err)
		}
		manifest, err := speechInventory(t.Context(), runtime)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtime, "SHA256SUMS"), manifest, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime", "v3.0.0", "codex-piper"), []byte("tampered"), 0700); err != nil {
		t.Fatal(err)
	}
	selected, err := installedSpeechRuntime(t.Context(), dir, "piper")
	if err != nil || filepath.Base(selected) != "v2.0.0" {
		t.Fatalf("latest verified runtime = %q, %v", selected, err)
	}
	if err := os.Chmod(filepath.Join(dir, "runtime", "v2.0.0", "codex-piper"), 0600); err != nil {
		t.Fatal(err)
	}
	selected, err = installedSpeechRuntime(t.Context(), dir, "piper")
	if err != nil || filepath.Base(selected) != "v1.0.0" {
		t.Fatalf("nonexecutable runtime was selected: %q, %v", selected, err)
	}
}

func TestNativeRuntimePreparationRejectsSymlinkBeforeTouchingExternalTarget(t *testing.T) {
	previousVersion := buildinfo.Version
	buildinfo.Version = "v1.2.3"
	t.Cleanup(func() { buildinfo.Version = previousVersion })
	s := nativeTestService(t)
	dir := filepath.Join(s.dir, "piper")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := privateTestDir(t)
	if err := os.Symlink(outside, filepath.Join(dir, "runtime")); err != nil {
		t.Fatal(err)
	}
	var requests int
	s.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("fixture must reject storage before downloading")
	})}
	if err := s.prepareRuntime(t.Context(), dir, "piper", nil); err == nil {
		t.Fatal("runtime directory symlink was accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 || requests != 0 {
		t.Fatalf("unsafe runtime path touched external storage or downloaded: entries=%v, requests=%d, err=%v", entries, requests, err)
	}
}

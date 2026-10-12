package distribution

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNativeSpeechArchiveIntegration(t *testing.T) {
	dir := os.Getenv("TELEGRAMGW_SPEECH_ARCHIVE_DIR")
	if dir == "" {
		t.Skip("set TELEGRAMGW_SPEECH_ARCHIVE_DIR for native release archive checks")
	}
	target := Target{runtime.GOOS, runtime.GOARCH}
	for _, helper := range SpeechComponents() {
		if err := verifySpeechArchive(filepath.Join(dir, helper+"-"+target.Suffix()+".tar.gz"), helper, target); err != nil {
			t.Fatal(err)
		}
	}
}

func speechFixture(t *testing.T, dist, helper string, target Target) string {
	t.Helper()
	if err := os.MkdirAll(dist, 0755); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dist, helper+"-"+target.Suffix()+".tar.gz")
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	z := gzip.NewWriter(f)
	w := tar.NewWriter(z)
	files := map[string][]byte{helper: nativeFixture(target), "NOTICE.md": []byte("Pinned native runtime and model attribution.\n")}
	if helper == "codex-qwen-tts" {
		files["lib/codex-qwen-tts-optimized"] = nativeFixture(target)
	}
	files["SHA256SUMS"] = manifest(files)
	for name, data := range files {
		mode := int64(0644)
		if name == helper || name == "lib/codex-qwen-tts-optimized" {
			mode = 0755
		}
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: mode, Typeflag: tar.TypeReg, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	for _, err := range []error{w.Close(), z.Close(), f.Close()} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return filename
}

func TestSpeechBundleRequiresCompleteManifestAndCorrectArchitecture(t *testing.T) {
	target := Target{"linux", "arm64"}
	filename := speechFixture(t, t.TempDir(), "codex-piper", target)
	if err := verifySpeechArchive(filename, "codex-piper", target); err != nil {
		t.Fatal(err)
	}
	if err := verifySpeechArchive(filename, "codex-piper", Target{"linux", "amd64"}); err == nil {
		t.Fatal("wrong speech architecture accepted")
	}
	rewriteArchive(t, filename, func(name string, f *file) (string, *tar.Header) {
		if name == "NOTICE.md" {
			f.data = []byte("Changed attribution")
		}
		return name, nil
	}, false)
	if err := verifySpeechArchive(filename, "codex-piper", target); err == nil {
		t.Fatal("changed speech bundle accepted")
	}
}

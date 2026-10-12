package speech

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeCPUVariantRequiresInstalledExecutable(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "codex-qwen-tts")
	if got := nativeCommandPath(dir, "qwen3"); got != base {
		t.Fatalf("missing optimized variant = %s", got)
	}
	if err := os.Mkdir(filepath.Join(dir, "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	optimized := filepath.Join(dir, "lib", "codex-qwen-tts-optimized")
	if err := os.WriteFile(optimized, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := nativeCommandPath(dir, "qwen3"); got != base {
		t.Fatal("non-executable optimized helper selected")
	}
	if err := os.Chmod(optimized, 0700); err != nil {
		t.Fatal(err)
	}
	want := base
	if optimizedSpeechCPU() {
		want = optimized
	}
	if got := nativeCommandPath(dir, "qwen3"); got != want {
		t.Fatalf("CPU selection = %s; want %s", got, want)
	}
	if got := nativeCommandPath(dir, "piper"); got != filepath.Join(dir, "codex-piper") {
		t.Fatal("Piper selected a Qwen variant")
	}
	if err := os.Remove(optimized); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, optimized); err != nil {
		t.Fatal(err)
	}
	if got := nativeCommandPath(dir, "qwen3"); got != base {
		t.Fatal("symlink optimized helper selected")
	}
}

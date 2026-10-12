package speech

import (
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/cpu"
)

func optimizedSpeechCPU() bool {
	switch runtime.GOARCH {
	case "arm64":
		return cpu.ARM64.HasASIMDDP && cpu.ARM64.HasASIMDHP && cpu.ARM64.HasFPHP
	case "amd64":
		return cpu.X86.HasAVX && cpu.X86.HasAVX2 && cpu.X86.HasFMA && cpu.X86.HasBMI2 && cpu.X86.HasSSE42 && hasF16C()
	}
	return false
}

func nativeCommandPath(runtimeDir, engine string) string {
	if engine == "qwen3" && optimizedSpeechCPU() {
		optimized := filepath.Join(runtimeDir, "lib", "codex-qwen-tts-optimized")
		if info, err := os.Lstat(optimized); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0100 != 0 {
			return optimized
		}
	}
	return filepath.Join(runtimeDir, nativeProgram(engine))
}

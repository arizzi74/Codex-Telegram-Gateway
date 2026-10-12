package distribution

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"path"
	"strings"
)

func verifySpeechArchive(filename, binary string, target Target) error {
	files, err := readArchive(filename)
	if err != nil {
		return fmt.Errorf("%s: %w", filename, err)
	}
	checksums, err := ParseManifest(files["SHA256SUMS"].data, true)
	if err != nil || len(checksums) != len(files)-1 {
		return errors.New("speech archive must checksum every packaged file")
	}
	for name, entry := range files {
		if name == "SHA256SUMS" {
			continue
		}
		if checksums[name] != digest(entry.data) {
			return fmt.Errorf("speech archive checksum mismatch: %s", name)
		}
		if name != binary && name != "NOTICE.md" && name != "source.env" && !strings.HasPrefix(name, "licenses/") && !strings.HasPrefix(name, "lib/") {
			return fmt.Errorf("unexpected speech archive member: %s", name)
		}
	}
	program, ok := files[binary]
	if !ok || program.mode&0111 == 0 || len(files["NOTICE.md"].data) == 0 {
		return errors.New("speech archive is missing its executable or notices")
	}
	if binary == "codex-qwen-tts" {
		if err := verifyNative(program.data, target); err != nil {
			return err
		}
		optimized, exists := files["lib/codex-qwen-tts-optimized"]
		if !exists || optimized.mode&0111 == 0 {
			return errors.New("Qwen bundle is missing its optimized CPU helper")
		}
		return verifyNative(optimized.data, target)
	}
	for name, entry := range files {
		if name != binary && !strings.HasPrefix(name, "lib/") {
			continue
		}
		if err := verifySpeechObject(entry.data, target, name == binary, files); err != nil {
			return fmt.Errorf("%s: %s: %w", filename, name, err)
		}
	}
	return nil
}

// Piper ships the pinned ONNX/espeak runtime beside its helper. Core release
// executables retain their stricter static-only Linux verification policy.
func verifySpeechObject(data []byte, target Target, executable bool, files map[string]file) error {
	if target.OS == "linux" {
		program, err := elf.NewFile(bytes.NewReader(data))
		if err != nil {
			return err
		}
		defer program.Close()
		expected := elf.EM_X86_64
		if target.Arch == "arm64" {
			expected = elf.EM_AARCH64
		}
		if program.Machine != expected || program.Class != elf.ELFCLASS64 || program.Data != elf.ELFDATA2LSB || (program.Type != elf.ET_EXEC && program.Type != elf.ET_DYN) {
			return errors.New("incorrect native speech architecture or type")
		}
		libraries, err := program.ImportedLibraries()
		if err != nil {
			return err
		}
		for _, library := range libraries {
			switch library {
			case "libc.so.6", "libm.so.6", "libdl.so.2", "libpthread.so.0", "librt.so.1", "libstdc++.so.6", "libgcc_s.so.1", "ld-linux-aarch64.so.1", "ld-linux-x86-64.so.2":
				continue
			}
			if path.Base(library) != library || len(files["lib/"+library].data) == 0 {
				return errors.New("unbundled speech library dependency")
			}
		}
		return nil
	}
	program, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer program.Close()
	expected := macho.CpuAmd64
	if target.Arch == "arm64" {
		expected = macho.CpuArm64
	}
	if program.Cpu != expected || (executable && program.Type != macho.TypeExec) || (!executable && program.Type != macho.TypeDylib) {
		return errors.New("incorrect native speech architecture or type")
	}
	libraries, err := program.ImportedLibraries()
	if err != nil {
		return err
	}
	for _, library := range libraries {
		if strings.HasPrefix(library, "/usr/lib/") || strings.HasPrefix(library, "/System/Library/Frameworks/") {
			continue
		}
		if (!strings.HasPrefix(library, "@rpath/") && !strings.HasPrefix(library, "@loader_path/") && !strings.HasPrefix(library, "@executable_path/")) || len(files["lib/"+path.Base(library)].data) == 0 {
			return errors.New("unbundled speech library dependency")
		}
	}
	return nil
}

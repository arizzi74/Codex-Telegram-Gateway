package distribution

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0644); err != nil {
		t.Fatal(err)
	}
}

// Small executable-format fixtures exercise validation without cross compiling
// test programs or relying on the test runner's own dynamic-link settings.
func nativeFixture(target Target) []byte {
	var buffer bytes.Buffer
	if target.OS == "linux" {
		header := elf.Header64{Type: uint16(elf.ET_EXEC), Machine: uint16(elf.EM_X86_64), Version: 1, Ehsize: 64}
		copy(header.Ident[:], []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), 1})
		if target.Arch == "arm64" {
			header.Machine = uint16(elf.EM_AARCH64)
		}
		_ = binary.Write(&buffer, binary.LittleEndian, header)
	} else {
		header := macho.FileHeader{Magic: macho.Magic64, Cpu: macho.CpuAmd64, Type: macho.TypeExec}
		if target.Arch == "arm64" {
			header.Cpu = macho.CpuArm64
		}
		_ = binary.Write(&buffer, binary.LittleEndian, header)
		_ = binary.Write(&buffer, binary.LittleEndian, uint32(0))
	}
	return buffer.Bytes()
}

func releaseFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	for _, source := range []string{"README.md", "LICENSE", "examples/worker.json", "deploy/systemd/codex-worker.service", "migrations/001_registry.sql", "scripts/install.sh"} {
		writeTestFile(t, filepath.Join(root, source), []byte("source for "+source+"\n"))
	}
	for _, target := range Targets() {
		writeTestFile(t, filepath.Join(dist, target.ManagerAsset()), nativeFixture(target))
		for _, component := range target.Components() {
			writeTestFile(t, filepath.Join(dist, component+"-"+target.Suffix()), nativeFixture(target))
			if err := Package(root, dist, component, target); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeTestFile(t, filepath.Join(dist, "install.sh"), []byte("source for scripts/install.sh\n"))
	if err := WriteManifest(dist); err != nil {
		t.Fatal(err)
	}
	return root, dist
}

func TestManifestRejectsAliasesDuplicatesAndUnsafePaths(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for _, contents := range []string{
		"", "bad\n", hash + "  ../outside\n", hash + "  /outside\n",
		hash + "  nested/../outside\n", hash + "  a//b\n", hash + "  a\\b\n",
		hash + "  ./same\n" + hash + "  same\n", hash + "  a\n\n",
	} {
		t.Run(contents, func(t *testing.T) {
			if _, err := ParseManifest([]byte(contents), true); err == nil {
				t.Fatal("accepted malformed manifest")
			}
		})
	}
	if _, err := ParseManifest([]byte(hash+"  nested/file\n"), false); err == nil {
		t.Fatal("accepted nested release asset")
	}
	parsed, err := ParseManifest([]byte(strings.ToUpper(hash)+" *./nested/file\n"), true)
	if err != nil || parsed["nested/file"] != hash {
		t.Fatalf("valid sha256sum entry rejected: %v %v", parsed, err)
	}
}

func TestReleasePreservesContentsAndNativeMigrationManager(t *testing.T) {
	root, dist := releaseFixture(t)
	if err := Verify(root, dist); err != nil {
		t.Fatal(err)
	}
	files, err := readArchive(filepath.Join(dist, "codex-worker-linux-amd64.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex-worker", ManagerName, LegacyManagerName} {
		if files[name].mode != 0755 {
			t.Fatalf("mode for %s = %o", name, files[name].mode)
		}
	}
	if !bytes.Equal(files[ManagerName].data, files[LegacyManagerName].data) {
		t.Fatal("migration manager differs")
	}
	for name := range files {
		if strings.HasSuffix(name, ".py") && name != LegacyManagerName {
			t.Fatalf("Python source shipped: %s", name)
		}
	}
	checksums, err := os.ReadFile(filepath.Join(dist, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	hashes, err := ParseManifest(checksums, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 15 {
		t.Fatalf("expected fifteen checksummed assets plus manifest, got %d", len(hashes))
	}
}

func rewriteArchive(t *testing.T, filename string, change func(string, *file) (string, *tar.Header), extra bool) {
	t.Helper()
	files, err := readArchive(filename)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	zipper := gzip.NewWriter(output)
	archive := tar.NewWriter(zipper)
	for name, contents := range files {
		newName, override := change(name, &contents)
		header := &tar.Header{Name: newName, Typeflag: tar.TypeReg, Mode: contents.mode, Size: int64(len(contents.data)), Uname: "root", Gname: "root"}
		if override != nil {
			header = override
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := archive.Write(contents.data); err != nil {
				t.Fatal(err)
			}
		}
		if extra && name == "README.md" {
			if err := archive.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if _, err := archive.Write(contents.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipper.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerificationRejectsTamperingEvenWithFreshOuterChecksums(t *testing.T) {
	for _, scenario := range []string{"unsafe path", "symlink", "hardlink", "duplicate", "owner uid", "owner gid", "owner name", "owner group", "special mode", "nonexecutable", "inner digest", "stale manager", "stale template", "python source"} {
		t.Run(scenario, func(t *testing.T) {
			root, dist := releaseFixture(t)
			rewriteArchive(t, filepath.Join(dist, "codex-worker-linux-amd64.tar.gz"), func(name string, contents *file) (string, *tar.Header) {
				if name != "README.md" && scenario != "nonexecutable" && scenario != "inner digest" && scenario != "stale manager" {
					return name, nil
				}
				switch scenario {
				case "unsafe path":
					return "../outside", nil
				case "python source":
					return "scripts/unexpected.py", nil
				case "symlink":
					return name, &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: "/outside"}
				case "hardlink":
					return name, &tar.Header{Name: name, Typeflag: tar.TypeLink, Linkname: "/outside"}
				case "owner uid", "owner gid", "owner name", "owner group":
					header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: contents.mode, Size: int64(len(contents.data))}
					switch scenario {
					case "owner uid":
						header.Uid = 1234
					case "owner gid":
						header.Gid = 2345
					case "owner name":
						header.Uname = "private-builder"
					case "owner group":
						header.Gname = "private-team"
					}
					return name, header
				case "special mode":
					contents.mode |= 04000
				case "nonexecutable":
					if name == ManagerName {
						contents.mode = 0644
					}
				case "inner digest":
					if name == "codex-worker" {
						contents.data = append(contents.data, 'x')
					}
				case "stale manager":
					if name == LegacyManagerName {
						contents.data = []byte("#!/usr/bin/python3\n")
					}
				case "stale template":
					contents.data = []byte("old template")
				}
				return name, nil
			}, scenario == "duplicate")
			if err := WriteManifest(dist); err != nil {
				t.Fatal(err)
			}
			if err := Verify(root, dist); err == nil {
				t.Fatal("accepted modified archive")
			}
		})
	}
}

func TestVerificationRejectsMissingAssetCorruptionAndWrongPlatform(t *testing.T) {
	for _, scenario := range []string{"missing", "corrupted", "wrong platform", "python manager", "installer", "extra archive"} {
		t.Run(scenario, func(t *testing.T) {
			root, dist := releaseFixture(t)
			name := filepath.Join(dist, "codex-telegramgw-linux-amd64")
			switch scenario {
			case "missing":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "corrupted":
				writeTestFile(t, name, []byte("bad"))
			case "wrong platform":
				writeTestFile(t, name, nativeFixture(Target{"linux", "arm64"}))
				if err := WriteManifest(dist); err != nil {
					t.Fatal(err)
				}
			case "python manager":
				writeTestFile(t, name, []byte("#!/usr/bin/python3\n"))
				if err := WriteManifest(dist); err != nil {
					t.Fatal(err)
				}
			case "installer":
				writeTestFile(t, filepath.Join(dist, "install.sh"), []byte("stale installer"))
				if err := WriteManifest(dist); err != nil {
					t.Fatal(err)
				}
			case "extra archive":
				writeTestFile(t, filepath.Join(dist, "unexpected.tar.gz"), []byte("extra"))
			}
			if err := Verify(root, dist); err == nil {
				t.Fatal("accepted bad distribution")
			}
		})
	}
}

func TestNativeValidationRejectsDynamicLoader(t *testing.T) {
	target := Target{"linux", "amd64"}
	data := nativeFixture(target)
	var header elf.Header64
	if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &header); err != nil {
		t.Fatal(err)
	}
	header.Phoff = uint64(len(data))
	header.Phnum = 1
	header.Phentsize = 56
	var buffer bytes.Buffer
	_ = binary.Write(&buffer, binary.LittleEndian, header)
	_ = binary.Write(&buffer, binary.LittleEndian, elf.Prog64{Type: uint32(elf.PT_INTERP)})
	if err := verifyNative(buffer.Bytes(), target); err == nil || !strings.Contains(err.Error(), "dynamic loader") {
		t.Fatalf("dynamic executable: %v", err)
	}
}

// Package distribution builds and verifies the files published in a release.
// It is independent of the installed updater so the build host only needs Go.
package distribution

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	ManagerName       = "codex-telegramgw"
	LegacyManagerName = "scripts/release-manager.py"
	maxUnpacked       = 256 << 20
	maxMembers        = 10000
)

type Target struct{ OS, Arch string }

func Targets() []Target {
	return []Target{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}}
}

func (t Target) Suffix() string       { return t.OS + "-" + t.Arch }
func (t Target) ManagerAsset() string { return ManagerName + "-" + t.Suffix() }
func (t Target) Components() []string {
	if t.OS == "linux" {
		return []string{"codex-gateway", "codex-worker", "codex-local"}
	}
	return []string{"codex-worker", "codex-local"}
}

func Assets() []string {
	names := []string{"install.sh"}
	for _, target := range Targets() {
		names = append(names, target.ManagerAsset())
		for _, binary := range target.Components() {
			names = append(names, binary+"-"+target.Suffix()+".tar.gz")
		}
	}
	slices.Sort(names)
	return names
}

var manifestLine = regexp.MustCompile(`^([0-9a-fA-F]{64}) [ *](?:\./)?([A-Za-z0-9_./-]+)$`)

// ParseManifest accepts sha256sum and shasum output, rejects aliases and duplicates.
func ParseManifest(contents []byte, nested bool) (map[string]string, error) {
	if len(contents) > 1<<20 {
		return nil, errors.New("checksum manifest exceeds size limit")
	}
	entries := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n") {
		match := manifestLine.FindStringSubmatch(line)
		if match == nil {
			return nil, errors.New("invalid checksum manifest entry")
		}
		name := match[2]
		if !safeName(name) || (!nested && strings.Contains(name, "/")) {
			return nil, errors.New("unsafe checksum manifest name")
		}
		if _, exists := entries[name]; exists {
			return nil, errors.New("duplicate checksum manifest name")
		}
		entries[name] = strings.ToLower(match[1])
	}
	return entries, nil
}

func safeName(name string) bool {
	if name == "" || name == "." || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func manifest(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	var output strings.Builder
	for _, name := range names {
		fmt.Fprintf(&output, "%s  %s\n", digest(files[name]), name)
	}
	return []byte(output.String())
}

// WriteManifest hashes precisely the public assets, excluding intermediate builds.
func WriteManifest(dist string) error {
	files := make(map[string][]byte)
	for _, name := range Assets() {
		data, err := os.ReadFile(filepath.Join(dist, name))
		if err != nil {
			return err
		}
		files[name] = data
	}
	return os.WriteFile(filepath.Join(dist, "SHA256SUMS"), manifest(files), 0644)
}

type file struct {
	data []byte
	mode int64
}

func packagedSources(root string) (map[string]file, error) {
	files := make(map[string]file)
	for _, name := range []string{"README.md", "LICENSE", "examples", "deploy", "migrations"} {
		err := filepath.WalkDir(filepath.Join(root, name), func(filename string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("non-regular package source: %s", filename)
			}
			relative, err := filepath.Rel(root, filename)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(relative)] = file{data, 0644}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

func validTarget(binary string, target Target) bool {
	return slices.Contains(Targets(), target) && slices.Contains(target.Components(), binary)
}

// Package includes the native manager under both canonical and migration names.
// The legacy name lets an already-installed Python updater replace itself with Go.
func Package(root, dist, binary string, target Target) error {
	if !validTarget(binary, target) {
		return errors.New("unsupported component or platform")
	}
	files, err := packagedSources(root)
	if err != nil {
		return err
	}
	program, err := os.ReadFile(filepath.Join(dist, binary+"-"+target.Suffix()))
	if err != nil {
		return err
	}
	manager, err := os.ReadFile(filepath.Join(dist, target.ManagerAsset()))
	if err != nil {
		return err
	}
	if err := verifyNative(program, target); err != nil {
		return fmt.Errorf("%s: %w", binary, err)
	}
	if err := verifyNative(manager, target); err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	files[binary] = file{program, 0755}
	files[ManagerName] = file{manager, 0755}
	files[LegacyManagerName] = file{manager, 0755}
	files["SHA256SUMS"] = file{manifest(map[string][]byte{binary: program, ManagerName: manager, LegacyManagerName: manager}), 0644}
	output, err := os.Create(filepath.Join(dist, binary+"-"+target.Suffix()+".tar.gz"))
	if err != nil {
		return err
	}
	zipper := gzip.NewWriter(output)
	archive := tar.NewWriter(zipper)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	var writeErr error
	for _, name := range names {
		entry := files[name]
		header := &tar.Header{Name: name, Mode: entry.mode, Size: int64(len(entry.data)), Typeflag: tar.TypeReg, Uid: 0, Gid: 0, Uname: "root", Gname: "root", ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if writeErr = archive.WriteHeader(header); writeErr != nil {
			break
		}
		if _, writeErr = archive.Write(entry.data); writeErr != nil {
			break
		}
	}
	return errors.Join(writeErr, archive.Close(), zipper.Close(), output.Close())
}

// verifyNative checks CPU/OS and rejects dynamically linked Linux executables.
// macOS native executables still use Apple's system library, as normal Go does.
func verifyNative(data []byte, target Target) error {
	reader := bytes.NewReader(data)
	if target.OS == "linux" {
		program, err := elf.NewFile(reader)
		if err != nil {
			return fmt.Errorf("invalid ELF executable: %w", err)
		}
		defer program.Close()
		expected := elf.EM_X86_64
		if target.Arch == "arm64" {
			expected = elf.EM_AARCH64
		}
		if program.Machine != expected || program.Class != elf.ELFCLASS64 || program.Data != elf.ELFDATA2LSB || (program.Type != elf.ET_EXEC && program.Type != elf.ET_DYN) {
			return errors.New("incorrect Linux executable architecture or type")
		}
		for _, segment := range program.Progs {
			if segment.Type == elf.PT_INTERP {
				return errors.New("Linux executable requires a dynamic loader")
			}
		}
		libraries, err := program.ImportedLibraries()
		if err != nil {
			return err
		}
		if len(libraries) != 0 {
			return errors.New("Linux executable has shared-library dependencies")
		}
		return nil
	}
	program, err := macho.NewFile(reader)
	if err != nil {
		return fmt.Errorf("invalid Mach-O executable: %w", err)
	}
	defer program.Close()
	expected := macho.CpuAmd64
	if target.Arch == "arm64" {
		expected = macho.CpuArm64
	}
	if program.Cpu != expected || program.Type != macho.TypeExec {
		return errors.New("incorrect macOS executable architecture or type")
	}
	return nil
}

func readArchive(filename string) (map[string]file, error) {
	input, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	zipper, err := gzip.NewReader(input)
	if err != nil {
		return nil, err
	}
	defer zipper.Close()
	archive := tar.NewReader(zipper)
	files := make(map[string]file)
	seen := make(map[string]bool)
	var total int64
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Uid != 0 || header.Gid != 0 || (header.Uname != "" && header.Uname != "root") || (header.Gname != "" && header.Gname != "root") {
			return nil, errors.New("archive contains build-account ownership metadata")
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !safeName(name) || seen[name] || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir) {
			return nil, fmt.Errorf("unsafe or duplicate archive member: %s", header.Name)
		}
		seen[name] = true
		if len(seen) > maxMembers || header.Size < 0 || header.Size > maxUnpacked-total {
			return nil, errors.New("archive exceeds unpacked size or member limit")
		}
		total += header.Size
		if header.Mode&07000 != 0 {
			return nil, errors.New("archive contains special permission bits")
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		data, err := io.ReadAll(archive)
		if err != nil {
			return nil, err
		}
		files[name] = file{data, header.Mode}
	}
	// Force gzip checksum verification even when tar ends before the gzip stream.
	// Only bounded zero padding may follow the end of the tar archive.
	trailing, err := io.ReadAll(io.LimitReader(zipper, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(trailing) > 1<<20 || bytes.Count(trailing, []byte{0}) != len(trailing) {
		return nil, errors.New("unexpected data after archive end")
	}
	return files, nil
}

// Verify checks all platform assets, ownership, executable formats, both levels
// of checksums, source templates and the exact native migration-manager bytes.
func Verify(root, dist string) error {
	data, err := os.ReadFile(filepath.Join(dist, "SHA256SUMS"))
	if err != nil {
		return err
	}
	hashes, err := ParseManifest(data, false)
	if err != nil {
		return err
	}
	assets := Assets()
	if len(hashes) != len(assets) {
		return errors.New("release manifest must contain exactly ten archives, four managers and installer")
	}
	for _, name := range assets {
		expected, ok := hashes[name]
		if !ok {
			return fmt.Errorf("release manifest is missing %s", name)
		}
		data, err := os.ReadFile(filepath.Join(dist, name))
		if err != nil {
			return err
		}
		if digest(data) != expected {
			return fmt.Errorf("release checksum mismatch: %s", name)
		}
	}
	archives, err := filepath.Glob(filepath.Join(dist, "*.tar.gz"))
	if err != nil {
		return err
	}
	if len(archives) != 10 {
		return errors.New("unexpected or missing platform archive")
	}
	sources, err := packagedSources(root)
	if err != nil {
		return err
	}
	bootstrap, err := os.ReadFile(filepath.Join(root, "scripts/install.sh"))
	if err != nil {
		return err
	}
	installedBootstrap, err := os.ReadFile(filepath.Join(dist, "install.sh"))
	if err != nil {
		return err
	}
	if !bytes.Equal(bootstrap, installedBootstrap) {
		return errors.New("release installer differs from source")
	}
	for _, target := range Targets() {
		manager, err := os.ReadFile(filepath.Join(dist, target.ManagerAsset()))
		if err != nil {
			return err
		}
		if err := verifyNative(manager, target); err != nil {
			return fmt.Errorf("%s: %w", target.ManagerAsset(), err)
		}
		for _, binary := range target.Components() {
			name := binary + "-" + target.Suffix() + ".tar.gz"
			files, err := readArchive(filepath.Join(dist, name))
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if len(files) != len(sources)+4 {
				return fmt.Errorf("unexpected archive contents: %s", name)
			}
			for source, expected := range sources {
				actual, ok := files[source]
				if !ok || !bytes.Equal(actual.data, expected.data) {
					return fmt.Errorf("archive file differs from source: %s: %s", name, source)
				}
			}
			inner, err := ParseManifest(files["SHA256SUMS"].data, true)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if len(inner) != 3 {
				return fmt.Errorf("unexpected inner checksum manifest: %s", name)
			}
			for _, executable := range []string{binary, ManagerName, LegacyManagerName} {
				actual, ok := files[executable]
				if !ok || digest(actual.data) != inner[executable] {
					return fmt.Errorf("archive checksum mismatch: %s: %s", name, executable)
				}
				if actual.mode&0111 == 0 {
					return fmt.Errorf("archive binary is not executable: %s: %s", name, executable)
				}
			}
			if err := verifyNative(files[binary].data, target); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			for _, executable := range []string{ManagerName, LegacyManagerName} {
				if !bytes.Equal(files[executable].data, manager) {
					return fmt.Errorf("archive manager differs from platform manager: %s: %s", name, executable)
				}
			}
		}
	}
	return nil
}

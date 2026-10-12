package speech

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/audio"
	"github.com/iaia/telegramgw/internal/buildinfo"
	"github.com/iaia/telegramgw/internal/releasemanager"
)

const piperModelDir = "vits-piper-it_IT-miro-high"

func nativeProgram(engine string) string {
	switch engine {
	case "piper":
		return "codex-piper"
	case "qwen3":
		return "codex-qwen-tts"
	}
	return ""
}

func nativeModels(engine string) []asset {
	if engine == "piper" {
		return []asset{{"piper", "miro.tar.bz2", "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-piper-it_IT-miro-high.tar.bz2", 67162890, "4612a986f49680b571b8fe0b03f0b292b34c843178fd6e07826bf52de10157c4"}}
	}
	if engine != "qwen3" {
		return nil
	}
	const base = "https://huggingface.co/ggml-org/Qwen3-TTS-12Hz-1.7B-Base-GGUF/resolve/ca27d74bc954b73dadab5b71ca265d87fc861a7c/"
	return []asset{
		{"qwen3", "Qwen3-TTS-12Hz-1.7B-Base-Q4_K_M.gguf", base + "Qwen3-TTS-12Hz-1.7B-Base-Q4_K_M.gguf", 1035965280, "8d18c94acb2addd042f97da63c98be144eafa76d0d9495177eab65130cf85129"},
		{"qwen3", "mmproj-Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf", base + "mmproj-Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf", 446422912, "6fd65188839bcd6ecc91b277ad471e22a0edfada4699a0fe82f1165c18cfcce2"},
	}
}

func safeSpeechPath(name string) bool {
	return name != "" && name != "." && path.Clean(name) == name && !path.IsAbs(name) && name != ".." && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\\\x00\r\n")
}

// unpackSpeech extracts only regular files/directories into a new private root.
// It rejects links, aliases, duplicates and oversized archives before startup.
func unpackSpeech(ctx context.Context, input io.Reader, target, executable, prefix string, limit int64) error {
	root, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()
	reader := tar.NewReader(contextReader{ctx, input})
	seen := make(map[string]bool)
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !safeSpeechPath(name) || seen[name] || (prefix != "" && name != prefix && !strings.HasPrefix(name, prefix+"/")) || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir) || header.Mode&07000 != 0 {
			return errors.New("speech: unsafe native archive entry")
		}
		seen[name] = true
		if len(seen) > 10000 || header.Size < 0 || header.Size > limit-total {
			return errors.New("speech: native archive exceeds limits")
		}
		total += header.Size
		if header.Typeflag == tar.TypeDir {
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
			continue
		}
		if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if name == executable || (executable == "codex-qwen-tts" && name == "lib/codex-qwen-tts-optimized") {
			mode = 0700
		}
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(file, reader, header.Size)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return err
		}
	}
	// Read the compression trailer, including its checksum, with a bounded tail.
	tail, err := io.ReadAll(io.LimitReader(contextReader{ctx, input}, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(tail) > 1<<20 || bytes.Count(tail, []byte{0}) != len(tail) {
		return errors.New("speech: unexpected archive trailer")
	}
	return nil
}

func speechInventory(ctx context.Context, dir string) ([]byte, error) {
	var lines []string
	err := filepath.WalkDir(dir, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("speech: unsafe installed asset")
		}
		name, err := filepath.Rel(dir, filename)
		if err != nil {
			return err
		}
		name = filepath.ToSlash(name)
		if name == "SHA256SUMS" {
			return nil
		}
		file, err := os.Open(filename)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, contextReader{ctx, file})
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		lines = append(lines, hex.EncodeToString(h.Sum(nil))+"  "+name+"\n")
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(lines)
	return []byte(strings.Join(lines, "")), nil
}

func verifySpeechInventory(ctx context.Context, dir string) error {
	if err := privateDirectory(dir); err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	expected, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	file.Close()
	if err != nil || len(expected) > 1<<20 {
		return errors.New("speech: invalid installed manifest")
	}
	if _, err := parseSpeechInventory(expected); err != nil {
		return err
	}
	actual, err := speechInventory(ctx, dir)
	if err != nil {
		return err
	}
	// Ignore ordering, while requiring exactly the manifest's files and hashes.
	want, _ := parseSpeechInventory(expected)
	got, err := parseSpeechInventory(actual)
	if err != nil || len(want) != len(got) {
		return errors.New("speech: installed asset set changed")
	}
	for name, digest := range want {
		if got[name] != digest {
			return errors.New("speech: installed asset checksum changed")
		}
	}
	return nil
}

// Model inventories include legitimate eSpeak filenames with spaces. Unlike
// public release asset names, these use the exact sha256sum two-space boundary.
func parseSpeechInventory(data []byte) (map[string]string, error) {
	if len(data) == 0 || len(data) > 1<<20 {
		return nil, errors.New("speech: invalid installed manifest")
	}
	result := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if len(line) < 67 || line[64:66] != "  " {
			return nil, errors.New("speech: invalid installed manifest")
		}
		digest, err := hex.DecodeString(line[:64])
		name := line[66:]
		if err != nil || len(digest) != sha256.Size || !safeSpeechPath(name) || result[name] != "" || len(result) >= 10000 {
			return nil, errors.New("speech: invalid installed manifest")
		}
		result[name] = strings.ToLower(line[:64])
	}
	return result, nil
}

func runtimeDirectory(dir string) string {
	return filepath.Join(dir, "runtime", "v"+strings.TrimPrefix(buildinfo.Version, "v"))
}

func (s *Service) prepareRuntime(ctx context.Context, dir, engine string, progress func(audio.Operation)) error {
	if _, err := releasemanager.ParseVersion(buildinfo.Version); err != nil {
		return errors.New("speech: native engines require a published gateway release")
	}
	target := runtimeDirectory(dir)
	program := nativeProgram(engine)
	// Check the parent before verification can create a version directory.
	if err := privateDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	if verifySpeechInventory(ctx, target) == nil {
		if info, err := os.Lstat(filepath.Join(target, program)); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0100 != 0 {
			return nil
		}
	}
	if progress != nil {
		progress(audio.Operation{State: "downloading", Model: engine, Message: "Downloading and verifying the selected native speech engine."})
	}
	manager := releasemanager.New(io.Discard)
	manager.HTTP = s.http
	release, err := manager.FetchRelease(ctx, releasemanager.DefaultRepo, buildinfo.Version)
	if err != nil {
		return errors.New("speech: signed speech engine release is unavailable")
	}
	name := program + "-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	if release.Assets[name] == "" || release.Hashes[name] == "" {
		return errors.New("speech: native speech engine is unavailable for this platform")
	}
	data, err := manager.Download(ctx, release.Assets[name], 128<<20)
	if err != nil {
		return errors.New("speech: native speech engine download failed")
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != release.Hashes[name] {
		return errors.New("speech: native speech engine checksum mismatch")
	}
	if err := privateDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	z, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer z.Close()
	if err := unpackSpeech(ctx, z, stage, program, "", 256<<20); err != nil {
		return err
	}
	if err := verifySpeechInventory(ctx, stage); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(stage, program))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0100 == 0 {
		return errors.New("speech: engine bundle is missing its executable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A failed download never replaces a working runtime. An invalid existing
	// directory is replaced only after the signed staged bundle is complete.
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return os.Rename(stage, target)
}

func (s *Service) prepareNativeModels(ctx context.Context, dir, engine string, progress func(audio.Operation)) error {
	if engine == "piper" && verifySpeechInventory(ctx, filepath.Join(dir, piperModelDir)) == nil {
		return nil
	}
	for _, spec := range nativeModels(engine) {
		target := filepath.Join(dir, spec.name)
		if verifyAsset(ctx, target, spec) != nil {
			op := audio.Operation{State: "downloading", Model: engine, TotalBytes: spec.size, Message: "Downloading and verifying the selected speech model."}
			if progress != nil {
				progress(op)
			}
			lastReport := time.Now()
			if err := downloadAsset(ctx, s.http, target, spec, func(n int64) {
				op.DownloadedBytes += n
				if progress != nil && time.Since(lastReport) >= 200*time.Millisecond {
					progress(op)
					lastReport = time.Now()
				}
			}); err != nil {
				return err
			}
			if progress != nil {
				progress(op)
			}
		}
		if engine != "piper" {
			continue
		}
		stage, err := os.MkdirTemp(dir, ".model-")
		if err != nil {
			return err
		}
		err = func() error {
			defer os.RemoveAll(stage)
			file, err := os.Open(target)
			if err != nil {
				return err
			}
			defer file.Close()
			if err := unpackSpeech(ctx, bzip2.NewReader(file), stage, "", piperModelDir, 90<<20); err != nil {
				return err
			}
			model := filepath.Join(stage, piperModelDir)
			manifest, err := speechInventory(ctx, model)
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(model, "SHA256SUMS"), manifest, 0600); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.RemoveAll(filepath.Join(dir, piperModelDir)); err != nil {
				return err
			}
			return os.Rename(model, filepath.Join(dir, piperModelDir))
		}()
		if err != nil {
			return err
		}
		if err := os.Remove(target); err != nil {
			return err
		}
	}
	return nil
}

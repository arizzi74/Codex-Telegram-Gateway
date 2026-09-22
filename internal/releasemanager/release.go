package releasemanager

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/iaia/telegramgw/internal/releaseauth"
)

const MaxArchive = 128 * 1024 * 1024
const MaxUnpacked = 512 * 1024 * 1024

var versionRE = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var hashLineRE = regexp.MustCompile(`^([0-9a-fA-F]{64}) [ *](?:\./)?([^\s]+)$`)

func ParseVersion(value string) ([3]uint64, error) {
	var result [3]uint64
	match := versionRE.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return result, errors.New("release version must be a stable vMAJOR.MINOR.PATCH tag")
	}
	for i := range result {
		n, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return result, errors.New("release version number is too large")
		}
		result[i] = n
	}
	return result, nil
}

func CompareVersions(a, b string) (int, error) {
	x, err := ParseVersion(a)
	if err != nil {
		return 0, err
	}
	y, err := ParseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := range x {
		if x[i] < y[i] {
			return -1, nil
		}
		if x[i] > y[i] {
			return 1, nil
		}
	}
	return 0, nil
}

func ValidateRepo(repo string) error {
	if !repoRE.MatchString(repo) {
		return errors.New("repository must be OWNER/REPOSITORY")
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return errors.New("invalid repository name")
		}
	}
	return nil
}

func githubURL(raw string, redirect bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return errors.New("release downloads require HTTPS on GitHub")
	}
	switch u.Hostname() {
	case "github.com", "api.github.com":
		return nil
	case "release-assets.githubusercontent.com", "objects.githubusercontent.com", "github-releases.githubusercontent.com":
		if redirect {
			return nil
		}
	}
	return errors.New("rejected an unexpected release download host")
}

func Manifest(data []byte, nested bool) (map[string]string, error) {
	result := make(map[string]string)
	text := strings.TrimSuffix(string(data), "\n")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		match := hashLineRE.FindStringSubmatch(line)
		if match == nil {
			return nil, errors.New("invalid release checksum manifest")
		}
		name := match[2]
		if !safeArchivePath(name) || (!nested && strings.Contains(name, "/")) || result[name] != "" {
			return nil, errors.New("invalid release checksum manifest")
		}
		result[name] = strings.ToLower(match[1])
	}
	if len(result) == 0 {
		return nil, errors.New("empty release checksum manifest")
	}
	return result, nil
}

func VerifyFile(file, digest string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return errors.New("release checksum verification failed")
	}
	return nil
}

func safeArchivePath(name string) bool {
	return name != "" && name != "." && !strings.HasPrefix(name, "/") && !strings.ContainsAny(name, "\\\x00\r\n") && path.Clean(name) == name && !strings.HasPrefix(name, "../") && name != ".."
}

func UnpackVerified(archive, target, binary string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer z.Close()
	root, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()
	reader := tar.NewReader(z)
	seen := map[string]bool{}
	var total int64
	for {
		h, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if (name == "" || name == ".") && h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if !safeArchivePath(name) || seen[name] || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) {
			return errors.New("unsafe entry in release archive")
		}
		seen[name] = true
		if len(seen) > 10000 {
			return errors.New("release archive has too many files")
		}
		if h.Size < 0 || h.Size > MaxUnpacked-total {
			return errors.New("release archive exceeds unpacked size limit")
		}
		total += h.Size
		if h.Typeflag == tar.TypeDir {
			if err = root.MkdirAll(name, 0700); err != nil {
				return err
			}
			continue
		}
		if err = root.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		mode := os.FileMode(0600)
		if name == binary || name == "codex-telegramgw" || name == "scripts/release-manager.py" {
			mode = 0700
		}
		output, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(output, reader, h.Size)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	// Consume the gzip trailer too, so its CRC/truncation errors cannot be lost.
	if _, err = io.Copy(io.Discard, z); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(target, "SHA256SUMS"))
	if err != nil {
		return err
	}
	hashes, err := Manifest(data, true)
	if err != nil {
		return err
	}
	for _, name := range []string{binary, "codex-telegramgw"} {
		hash, ok := hashes[name]
		if !ok {
			return errors.New("release archive is missing its verified program or updater")
		}
		if err = VerifyFile(filepath.Join(target, name), hash); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) FetchRelease(ctx context.Context, repo, version string) (*Release, error) {
	if err := ValidateRepo(repo); err != nil {
		return nil, err
	}
	suffix := "latest"
	if version != "" {
		if _, err := ParseVersion(version); err != nil {
			return nil, err
		}
		suffix = "tags/v" + strings.TrimPrefix(strings.TrimSpace(version), "v")
	}
	data, err := m.Download(ctx, "https://api.github.com/repos/"+repo+"/releases/"+suffix, 4*1024*1024)
	if err != nil {
		return nil, err
	}
	var metadata struct {
		Tag        string `json:"tag_name"`
		Draft      *bool  `json:"draft"`
		Prerelease *bool  `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err = json.Unmarshal(data, &metadata); err != nil {
		return nil, errors.New("invalid GitHub release metadata")
	}
	if metadata.Draft == nil || metadata.Prerelease == nil || *metadata.Draft || *metadata.Prerelease {
		return nil, errors.New("only published stable releases can be installed")
	}
	if _, err = ParseVersion(metadata.Tag); err != nil || !strings.HasPrefix(metadata.Tag, "v") || strings.TrimSpace(metadata.Tag) != metadata.Tag {
		return nil, errors.New("GitHub did not return a stable release tag")
	}
	if version != "" {
		comparison, e := CompareVersions(metadata.Tag, version)
		if e != nil || comparison != 0 {
			return nil, errors.New("GitHub returned the wrong release version")
		}
	}
	release := &Release{Repo: repo, Tag: metadata.Tag, Assets: map[string]string{}}
	prefix := "https://github.com/" + repo + "/releases/download/" + metadata.Tag + "/"
	for _, asset := range metadata.Assets {
		if !safeArchivePath(asset.Name) || strings.Contains(asset.Name, "/") || release.Assets[asset.Name] != "" {
			return nil, errors.New("invalid or duplicate release asset")
		}
		if asset.URL != prefix+url.PathEscape(asset.Name) {
			return nil, errors.New("release asset URL does not match the selected repository and tag")
		}
		release.Assets[asset.Name] = asset.URL
	}
	if release.Assets["SHA256SUMS"] == "" {
		return nil, errors.New("release is missing SHA256SUMS")
	}
	data, err = m.Download(ctx, release.Assets["SHA256SUMS"], 1024*1024)
	if err != nil {
		return nil, err
	}
	release.Hashes, err = Manifest(data, false)
	if err != nil {
		return nil, err
	}
	if release.Assets[releaseauth.BundleName] == "" {
		return nil, errors.New("release is missing signed build provenance; unsigned legacy releases cannot be installed by this updater")
	}
	attestation, err := m.Download(ctx, release.Assets[releaseauth.BundleName], 4*1024*1024)
	if err != nil {
		return nil, err
	}
	verify := releaseauth.Verify
	if m.verifyManifest != nil {
		verify = m.verifyManifest
	}
	if err = verify(repo, release.Tag, data, attestation); err != nil {
		return nil, err
	}
	return release, nil
}

func (m *Manager) Package(ctx context.Context, release *Release, l *Layout, binary, stage string) (string, error) {
	name := binary + "-" + l.System + "-" + l.Architecture + ".tar.gz"
	if release.Assets[name] == "" || release.Hashes[name] == "" {
		return "", errors.New("release does not contain this platform: " + name)
	}
	data, err := m.Download(ctx, release.Assets[name], MaxArchive)
	if err != nil {
		return "", err
	}
	archive := filepath.Join(stage, name)
	if err = os.WriteFile(archive, data, 0600); err != nil {
		return "", err
	}
	if err = VerifyFile(archive, release.Hashes[name]); err != nil {
		return "", err
	}
	destination := filepath.Join(stage, binary)
	if err = os.Mkdir(destination, 0700); err != nil {
		return "", err
	}
	if err = UnpackVerified(archive, destination, binary); err != nil {
		return "", err
	}
	for _, program := range []string{binary, "codex-telegramgw"} {
		if program == "codex-local" {
			continue
		}
		actual, e := m.command(ctx, filepath.Join(destination, program), "version")
		if e != nil {
			return "", e
		}
		comparison, e := CompareVersions(string(actual), release.Tag)
		if e != nil || comparison != 0 {
			return "", errors.New("binary version does not match its release tag")
		}
	}
	return destination, nil
}

package distribution

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type bootstrapFixture struct {
	root, bin, manager, checksum, script string
	environment                          []string
}

func bootstrapTestFixture(t *testing.T) bootstrapFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bootstrap")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"uname", "mktemp", "chmod", "rm", "sha256sum"} {
		source, err := exec.LookPath(name)
		if err != nil {
			if name == "sha256sum" {
				name = "shasum"
				source, err = exec.LookPath(name)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(source, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	// There is deliberately no Python (or Go) anywhere in this PATH.
	if err := os.Remove(filepath.Join(bin, "uname")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(bin, "uname"), []byte("#!/bin/sh\ncase \"$1\" in -s) printf '%s\\n' \"${TEST_OS:-Linux}\" ;; -m) printf '%s\\n' \"${TEST_ARCH:-x86_64}\" ;; esac\n"))
	if err := os.Chmod(filepath.Join(bin, "uname"), 0755); err != nil {
		t.Fatal(err)
	}
	manager := []byte("#!/bin/sh\nprintf '%s\\n' \"$0\" \"$CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE\" \"$@\" > \"$TEST_RECORD\"\nexit 17\n")
	managerPath := filepath.Join(root, "manager")
	writeTestFile(t, managerPath, manager)
	manifestPath := filepath.Join(root, "manifest")
	writeTestFile(t, manifestPath, manifest(map[string][]byte{"codex-telegramgw-linux-amd64": manager}))
	// Fake curl checks transport flags, records exact URLs and returns fixture bytes.
	curl := `#!/bin/sh
set -eu
output= writeout= proto= redirect= tls=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --write-out) writeout=$2; shift 2 ;;
    --proto) proto=$2; shift 2 ;;
    --proto-redir) redirect=$2; shift 2 ;;
    --tlsv1.2) tls=yes; shift ;;
    --connect-timeout|--max-time|--retry|--max-filesize) shift 2 ;;
    --fail|--silent|--show-error|--location) shift ;;
    https://*) url=$1; shift ;;
    *) exit 99 ;;
  esac
done
[ "$proto" = '=https' ] && [ "$redirect" = '=https' ] && [ "$tls" = yes ] || exit 98
printf '%s\n' "$url" >> "$TEST_REQUESTS"
case "$url" in
  */releases/latest) printf '%s' "$TEST_RELEASE_URL" ;;
  */SHA256SUMS) /bin/cp "$TEST_MANIFEST" "$output" ;;
  */codex-telegramgw-*) /bin/cp "$TEST_MANAGER" "$output" ;;
  *) exit 97 ;;
esac
`
	writeTestFile(t, filepath.Join(bin, "curl"), []byte(curl))
	if err := os.Chmod(filepath.Join(bin, "curl"), 0755); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	return bootstrapFixture{root, bin, managerPath, manifestPath, script, []string{
		"PATH=" + bin, "TMPDIR=" + root, "TEST_RECORD=" + filepath.Join(root, "record"), "TEST_REQUESTS=" + filepath.Join(root, "requests"),
		"TEST_MANIFEST=" + manifestPath, "TEST_MANAGER=" + managerPath,
		"TEST_RELEASE_URL=https://github.com/arizzi74/Codex-Telegram-Gateway/releases/tag/v1.2.3",
	}}
}

func (fixture bootstrapFixture) run(arguments ...string) ([]byte, error) {
	command := exec.Command("/bin/sh", append([]string{fixture.script}, arguments...)...)
	command.Env = fixture.environment
	return command.CombinedOutput()
}

func TestBootstrapWorksWithoutPythonAndPreservesArguments(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	output, err := fixture.run("install", "worker", "--config", "/tmp/a config.json", "--auto-update")
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("manager status was not propagated: %v %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(fixture.root, "record"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if strings.Join(lines[1:], "\n") != "v1.2.3\ninstall\nworker\n--config\n/tmp/a config.json\n--auto-update" {
		t.Fatalf("arguments changed: %q", lines)
	}
	if _, err := os.Stat(lines[0]); !os.IsNotExist(err) {
		t.Fatalf("temporary manager not removed: %v", err)
	}
	requests, err := os.ReadFile(filepath.Join(fixture.root, "requests"))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/arizzi74/Codex-Telegram-Gateway/releases/latest\nhttps://github.com/arizzi74/Codex-Telegram-Gateway/releases/download/v1.2.3/SHA256SUMS\nhttps://github.com/arizzi74/Codex-Telegram-Gateway/releases/download/v1.2.3/codex-telegramgw-linux-amd64\n"
	if string(requests) != want {
		t.Fatalf("wrong release requests: %s", requests)
	}
}

func TestBootstrapWithoutArgumentsStartsGuidedSetup(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	// Simulate curl piping the bootstrap to sh: no filename or shell arguments.
	script, err := os.ReadFile(fixture.script)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh")
	command.Env = fixture.environment
	command.Stdin = strings.NewReader(string(script))
	output, err := command.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("manager status was not propagated: %v %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(fixture.root, "record"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if strings.Join(lines[1:], "\n") != "v1.2.3\nsetup" {
		t.Fatalf("unexpected default invocation: %q", lines)
	}
	if _, err := os.Stat(lines[0]); !os.IsNotExist(err) {
		t.Fatalf("temporary manager not removed: %v", err)
	}
}

func TestBootstrapPinnedVersionAndRepository(t *testing.T) {
	for _, args := range [][]string{
		{"install", "worker", "--version", "v2.3.4", "--repo", "owner/repository"},
		{"install", "worker", "--version=v2.3.4", "--repo=owner/repository"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			output, err := fixture.run(args...)
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
				t.Fatalf("manager failed: %v %s", err, output)
			}
			requests, err := os.ReadFile(filepath.Join(fixture.root, "requests"))
			if err != nil {
				t.Fatal(err)
			}
			for _, request := range strings.Fields(string(requests)) {
				if !strings.HasPrefix(request, "https://github.com/owner/repository/releases/download/v2.3.4/") {
					t.Fatalf("wrong repository or version: %s", request)
				}
			}
		})
	}
}

func TestBootstrapRejectsInvalidSelectionBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"--version", "v1.2.3-rc1"}, {"--version", "../bad"}, {"--version"}, {"--version="},
		{"--version=v01.2.3"}, {"--version=v1.2"}, {"--version=v1.2.3.4"}, {"--version=v1.2.3", "--version=v2.0.0"},
		{"--repo"}, {"--repo="}, {"--repo=owner/../repository"}, {"--repo=../repository"}, {"--repo=owner/repository", "--repo=another/repository"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			if output, err := fixture.run(args...); err == nil {
				t.Fatalf("accepted invalid options: %s", output)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "requests")); !os.IsNotExist(err) {
				t.Fatal("invalid options caused a download")
			}
		})
	}
}

func TestBootstrapRejectsUnexpectedLatestRedirect(t *testing.T) {
	for _, url := range []string{
		"https://github.com/arizzi74/Codex-Telegram-Gateway/releases/tag/v1.2.3-rc1",
		"http://github.com/arizzi74/Codex-Telegram-Gateway/releases/tag/v1.2.3",
		"https://github.com/attacker/repo/releases/tag/v1.2.3",
		"https://github.com/arizzi74/Codex-Telegram-Gateway/releases/tag/extra/v1.2.3",
	} {
		t.Run(url, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			fixture.environment[len(fixture.environment)-1] = "TEST_RELEASE_URL=" + url
			if output, err := fixture.run("install", "worker"); err == nil {
				t.Fatalf("accepted unexpected redirect: %s", output)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
				t.Fatal("executed manager after unexpected redirect")
			}
		})
	}
}

func TestBootstrapRejectsCorruptionAndMalformedManifests(t *testing.T) {
	for _, scenario := range []string{"corrupt manager", "duplicate", "alias duplicate", "unsafe name", "missing", "bad hash", "malformed", "empty"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			data, err := os.ReadFile(fixture.checksum)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "corrupt manager":
				writeTestFile(t, fixture.manager, []byte("malicious code"))
			case "duplicate":
				data = append(data, data...)
			case "alias duplicate":
				data = append(data, []byte(strings.Replace(string(data), "  codex-", "  ./codex-", 1))...)
			case "unsafe name":
				data = []byte(strings.Replace(string(data), "  codex-", "  ../codex-", 1))
			case "missing":
				data = []byte(strings.Replace(string(data), "codex-telegramgw-linux-amd64", "other", 1))
			case "bad hash":
				data[0] = 'z'
			case "malformed":
				data = []byte("invalid\n")
			case "empty":
				data = nil
			}
			writeTestFile(t, fixture.checksum, data)
			if output, err := fixture.run("install", "worker"); err == nil {
				t.Fatalf("accepted tampered release: %s", output)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
				t.Fatal("executed unverified manager")
			}
		})
	}
}

func TestBootstrapSelectsEverySupportedPlatform(t *testing.T) {
	for _, target := range Targets() {
		t.Run(target.Suffix(), func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			osName, arch := "Linux", "x86_64"
			if target.OS == "darwin" {
				osName = "Darwin"
			}
			if target.Arch == "arm64" {
				arch = "arm64"
			}
			fixture.environment = append(fixture.environment, "TEST_OS="+osName, "TEST_ARCH="+arch)
			data, err := os.ReadFile(fixture.manager)
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, fixture.checksum, []byte(fmt.Sprintf("%s  %s\n", digest(data), target.ManagerAsset())))
			output, err := fixture.run("install", "worker")
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
				t.Fatalf("platform selection failed: %v %s", err, output)
			}
		})
	}
}

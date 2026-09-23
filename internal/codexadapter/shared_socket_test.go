package codexadapter

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type socketFixture struct{ root, alias, directory, target string }

func newSocketFixture(t *testing.T) socketFixture {
	t.Helper()
	// A 64-character protected filename must fit sockaddr_un even with verbose
	// Go test names, so avoid using t.TempDir's test-name prefix for socket paths.
	root, err := os.MkdirTemp("/tmp", "tg-s-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f := socketFixture{root: root, alias: filepath.Join(root, "app", "server.sock"), directory: filepath.Join(root, "daemon")}
	for _, dir := range []string{filepath.Dir(f.alias), f.directory} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.target, err = protectedSocketPath(f.alias, f.directory)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func listenFixtureSocket(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return listener
}

func linkFixtureSocket(t *testing.T, f socketFixture) {
	t.Helper()
	if err := os.Symlink(f.target, f.alias); err != nil {
		t.Fatal(err)
	}
}

func TestSharedSocketSupportsLegacyAndProtectedEndpoints(t *testing.T) {
	for _, protected := range []bool{false, true} {
		t.Run(fmt.Sprint(protected), func(t *testing.T) {
			f := newSocketFixture(t)
			endpoint := f.alias
			if protected {
				endpoint = f.target
				linkFixtureSocket(t, f)
			}
			listener := listenFixtureSocket(t, endpoint)
			socket, err := inspectSharedSocket(f.alias, f.directory)
			if err != nil || socket.endpoint != endpoint {
				t.Fatalf("validated endpoint = %+v, %v", socket, err)
			}
			listener.Close()
			if err := socket.remove(); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{f.alias, endpoint} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("owned endpoint survived cleanup: %s, %v", path, err)
				}
			}
		})
	}
}

func TestSharedSocketRejectsUntrustedAliasesWithoutChangingPermissions(t *testing.T) {
	cases := []string{"other_socket", "relative_alias", "file_target", "second_symlink", "symlink_directory", "public_directory", "public_socket", "public_alias_parent", "symlink_alias_parent"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSocketFixture(t)
			other := filepath.Join(f.root, "other.sock")
			listenFixtureSocket(t, other)
			switch name {
			case "other_socket":
				f.target = other
			case "relative_alias":
				listenFixtureSocket(t, f.target)
				f.target, _ = filepath.Rel(filepath.Dir(f.alias), f.target)
			case "file_target":
				if err := os.WriteFile(f.target, []byte("not a socket"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "second_symlink":
				if err := os.Symlink(other, f.target); err != nil {
					t.Fatal(err)
				}
			default:
				listenFixtureSocket(t, f.target)
			}
			linkFixtureSocket(t, f)
			switch name {
			case "symlink_directory":
				if err := os.Rename(f.directory, f.directory+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.directory+"-real", f.directory); err != nil {
					t.Fatal(err)
				}
			case "public_directory":
				if err := os.Chmod(f.directory, 0o755); err != nil {
					t.Fatal(err)
				}
			case "public_socket":
				if err := os.Chmod(f.target, 0o666); err != nil {
					t.Fatal(err)
				}
			case "public_alias_parent":
				if err := os.Chmod(filepath.Dir(f.alias), 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink_alias_parent":
				parent := filepath.Dir(f.alias)
				if err := os.Rename(parent, parent+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+"-real", parent); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(f.alias)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := inspectSharedSocket(f.alias, f.directory); err == nil {
				t.Fatal("accepted untrusted listener")
			}
			after, err := os.Stat(f.alias)
			if err != nil || before.Mode() != after.Mode() {
				t.Fatalf("validation modified the target permissions: %v, %v", after, err)
			}
		})
	}
}

func TestSharedSocketProtectedPathUsesCanonicalParent(t *testing.T) {
	f := newSocketFixture(t)
	aliasParent := filepath.Join(f.root, "parent-alias")
	if err := os.Symlink(filepath.Dir(f.alias), aliasParent); err != nil {
		t.Fatal(err)
	}
	got, err := protectedSocketPath(filepath.Join(aliasParent, filepath.Base(f.alias)), f.directory)
	parent, canonicalErr := filepath.EvalSymlinks(filepath.Dir(f.alias))
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	hash := sha256.Sum256([]byte(filepath.Join(parent, filepath.Base(f.alias))))
	want := filepath.Join(f.directory, fmt.Sprintf("%x", hash))
	if err != nil || got != want || got != f.target {
		t.Fatalf("protected path = %q, %v; want %q", got, err, want)
	}
	t.Setenv("TMPDIR", f.root)
	directory, err := sharedDaemonSocketDirectory()
	root, canonicalErr := filepath.EvalSymlinks("/tmp")
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	if err != nil || directory != filepath.Join(root, fmt.Sprintf("codex-daemon-%d", os.Geteuid())) {
		t.Fatalf("protected root followed TMPDIR: %q, %v", directory, err)
	}
}

func TestSharedSocketWaitsForDanglingProtectedAlias(t *testing.T) {
	f := newSocketFixture(t)
	linkFixtureSocket(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	wait := &commandWait{done: make(chan struct{})}
	type result struct {
		socket *sharedSocket
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		socket, err := waitForSocketInDirectory(ctx, f.alias, f.directory, wait)
		resultCh <- result{socket, err}
	}()
	select {
	case got := <-resultCh:
		t.Fatalf("dangling alias ended startup early: %v", got.err)
	case <-time.After(30 * time.Millisecond):
	}
	listenFixtureSocket(t, f.target)
	got := <-resultCh
	if got.err != nil || got.socket.endpoint != f.target {
		t.Fatalf("did not accept ready protected socket: %+v", got)
	}
}

func TestSharedSocketReadinessEndsWithChildOrContext(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprint(stopped), func(t *testing.T) {
			f := newSocketFixture(t)
			linkFixtureSocket(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			wait := &commandWait{done: make(chan struct{})}
			want := context.DeadlineExceeded
			if stopped {
				close(wait.done)
				want = ErrClosed
			}
			if _, err := waitForSocketInDirectory(ctx, f.alias, f.directory, wait); !errors.Is(err, want) {
				t.Fatalf("startup error = %v; want %v", err, want)
			}
		})
	}
}

func TestSharedSocketWaitsForServerToRestrictLegacyPermissions(t *testing.T) {
	f := newSocketFixture(t)
	listenFixtureSocket(t, f.alias)
	if err := os.Chmod(f.alias, 0o666); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := waitForSocketInDirectory(ctx, f.alias, f.directory, &commandWait{done: make(chan struct{})})
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("accepted socket before server restricted it: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	info, err := os.Stat(f.alias)
	if err != nil || info.Mode().Perm() != 0o666 {
		t.Fatalf("startup changed socket permissions: %v, %v", info, err)
	}
	if err := os.Chmod(f.alias, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("private socket did not become ready: %v", err)
	}
}

func TestSharedSocketCleanupPreservesReplacements(t *testing.T) {
	for _, replace := range []string{"alias", "endpoint", "directory"} {
		t.Run(replace, func(t *testing.T) {
			f := newSocketFixture(t)
			listenFixtureSocket(t, f.target)
			linkFixtureSocket(t, f)
			socket, err := inspectSharedSocket(f.alias, f.directory)
			if err != nil {
				t.Fatal(err)
			}
			replacement := f.alias
			if replace == "endpoint" {
				replacement = f.target
			}
			if replace == "directory" {
				replacement = f.directory
			}
			// Rename retains the original inode and prevents immediate inode
			// reuse from making this test's replacement look unchanged.
			if err := os.Rename(replacement, replacement+".original"); err != nil {
				t.Fatal(err)
			}
			if replace == "directory" {
				if err := os.Mkdir(replacement, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(replacement, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := socket.remove(); err == nil {
				t.Fatal("did not report replaced listener")
			}
			if _, err := os.Lstat(replacement); err != nil {
				t.Fatalf("removed replacement: %v", err)
			}
		})
	}
}

type socketTestFileInfo struct {
	os.FileInfo
	owner uint32
}

func (i socketTestFileInfo) Sys() any { return &syscall.Stat_t{Uid: i.owner} }

func TestSharedSocketOwnershipRequiresEffectiveUser(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if socketOwnedByCurrentUser(socketTestFileInfo{info, uint32(os.Geteuid()) + 1}) {
		t.Fatal("accepted another user's socket metadata")
	}
	if !socketOwnedByCurrentUser(socketTestFileInfo{info, uint32(os.Geteuid())}) {
		t.Fatal("rejected current user's socket metadata")
	}
}

func TestStartUsesExplicitEnvironment(t *testing.T) {
	dir := t.TempDir()
	output, script := filepath.Join(dir, "environment"), filepath.Join(dir, "server")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$TG_ADAPTER_TEST_VALUE\" > \"$TG_ADAPTER_TEST_OUTPUT\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Start(ctx, Config{Command: script, Env: []string{"TG_ADAPTER_TEST_VALUE=isolated", "TG_ADAPTER_TEST_OUTPUT=" + output}})
	if err == nil {
		t.Fatal("expected empty test server to fail initialization")
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "isolated" {
		t.Fatalf("child environment was not applied: %q, %v", data, err)
	}
}

// This subprocess acts only as a listener. A shell wrapper makes the separate
// proxy invocation fail, exercising StartShared's real child cleanup path.
func TestSharedFailedProxyHelper(t *testing.T) {
	if os.Getenv("TG_ADAPTER_LISTENER_HELPER") != "1" {
		return
	}
	var path string
	for index, arg := range os.Args {
		if arg == "--listen" && index+1 < len(os.Args) {
			path = strings.TrimPrefix(os.Args[index+1], "unix://")
		}
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(os.Getenv("TG_ADAPTER_SERVER_OUTPUT"), []byte(os.Getenv("TG_ADAPTER_TEST_VALUE")+":"+strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestSharedFailedProxyReapsServerAndRemovesOwnedSocket(t *testing.T) {
	f := newSocketFixture(t)
	script := filepath.Join(f.root, "codex")
	serverOutput, proxyOutput := filepath.Join(f.root, "server-env"), filepath.Join(f.root, "proxy-env")
	code := `#!/bin/sh
if [ "$2" = proxy ]; then
  printf '%s' "$TG_ADAPTER_TEST_VALUE" > "$TG_ADAPTER_PROXY_OUTPUT"
  exit 1
fi
exec "$TG_ADAPTER_TEST_EXECUTABLE" -test.run='^TestSharedFailedProxyHelper$' -- "$@"
`
	if err := os.WriteFile(script, []byte(code), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env := append(os.Environ(), "TG_ADAPTER_LISTENER_HELPER=1", "TG_ADAPTER_TEST_VALUE=isolated", "TG_ADAPTER_TEST_EXECUTABLE="+os.Args[0], "TG_ADAPTER_SERVER_OUTPUT="+serverOutput, "TG_ADAPTER_PROXY_OUTPUT="+proxyOutput)
	if _, err := StartShared(ctx, Config{Command: script, Env: env}, f.alias); err == nil {
		t.Fatal("expected proxy startup to fail")
	}
	if _, err := os.Lstat(f.alias); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed startup retained its socket: %v", err)
	}
	data, err := os.ReadFile(proxyOutput)
	if err != nil || string(data) != "isolated" {
		t.Fatalf("explicit environment did not reach proxy: %q, %v", data, err)
	}
	data, err = os.ReadFile(serverOutput)
	value, pidText, found := strings.Cut(string(data), ":")
	pid, parseErr := strconv.Atoi(pidText)
	if err != nil || !found || value != "isolated" || parseErr != nil || pid <= 0 {
		t.Fatalf("explicit environment did not reach server: %q, %v", data, err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("failed startup left its server alive or unreaped: %v", err)
	}
}

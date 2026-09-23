package codexadapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SharedRuntime owns an attachable local app-server and its private byte
// proxy. SocketPath is the private advertised endpoint; newer Codex releases
// publish it as a validated alias to their protected physical socket. The
// adapter uses JSONL over a WebSocket bridge carried through proxy stdin/stdout.
//
// The owner must be closed. Closing it stops and reaps both processes. It
// never attaches to or terminates a pre-existing app-server.
type SharedRuntime struct {
	*Client

	SocketPath string
	server     *exec.Cmd
	proxy      *exec.Cmd
	serverWait *commandWait
	proxyWait  *commandWait
	cleanup    func() error
}

// StartShared starts an owned app-server on a private Unix socket, then starts
// the supported stdio byte proxy and bridges JSONL to WebSocket frames. It is
// intended for a worker profile that allows a local interactive Codex TUI
// attachment. No network listener is created.
func StartShared(ctx context.Context, config Config, socketPath string) (*SharedRuntime, error) {
	config = config.normalized()
	startupCtx, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStartup()
	socketPath, err := prepareSocketPath(socketPath)
	if err != nil {
		return nil, err
	}

	serverArgs, err := sharedServerArgs(config.Args, socketPath)
	if err != nil {
		return nil, err
	}
	server := exec.Command(config.Command, serverArgs...)
	server.Dir = config.WorkingDirectory
	server.Env = config.Env
	server.Stderr = config.Stderr
	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("start shared codex app-server: %w", err)
	}
	serverWait := waitCommand(server)
	socket, err := waitForSocket(startupCtx, socketPath, serverWait)
	if err != nil {
		_ = stopCommand(server, serverWait)
		return nil, err
	}
	// From here every failure owns this validated endpoint. Stop the child
	// before removing its captured socket, including the physical socket behind
	// the rendezvous alias used by newer Codex releases.
	started := false
	defer func() {
		if !started {
			if err := stopCommand(server, serverWait); err == nil {
				_ = socket.remove()
			}
		}
	}()

	// Connect to the validated physical endpoint. Replacing the advertised
	// alias later must not redirect this adapter or the worker's attach proxy.
	proxy := exec.Command(config.Command, "app-server", "proxy", "--sock", socket.endpoint)
	proxy.Dir = config.WorkingDirectory
	proxy.Env = config.Env
	proxy.Stderr = config.Stderr
	in, err := proxy.StdinPipe()
	if err != nil {
		_ = stopCommand(server, serverWait)
		return nil, fmt.Errorf("open shared proxy stdin: %w", err)
	}
	out, err := proxy.StdoutPipe()
	if err != nil {
		_ = in.Close()
		_ = stopCommand(server, serverWait)
		return nil, fmt.Errorf("open shared proxy stdout: %w", err)
	}
	if err := proxy.Start(); err != nil {
		_ = in.Close()
		_ = stopCommand(server, serverWait)
		return nil, fmt.Errorf("start shared app-server proxy: %w", err)
	}
	proxyWait := waitCommand(proxy)
	ws, err := dialProxyWebSocket(startupCtx, in, out)
	if err != nil {
		_ = stopCommand(proxy, proxyWait)
		_ = stopCommand(server, serverWait)
		return nil, err
	}
	bridge := newJSONLWSBridge(ws)
	var cleanupOnce sync.Once
	var cleanupErr error
	cleanup := func() error {
		cleanupOnce.Do(func() {
			_ = bridge.Close()
			_ = in.Close()
			_ = out.Close()
			if err := stopCommand(proxy, proxyWait); err != nil && !errors.Is(err, os.ErrProcessDone) {
				cleanupErr = err
			}
			if err := stopCommand(server, serverWait); err != nil {
				if cleanupErr == nil {
					cleanupErr = err
				}
			} else if err := socket.remove(); err != nil && cleanupErr == nil {
				cleanupErr = err
			}
		})
		return cleanupErr
	}
	transport := bridge.transport(cleanup)
	client := New(transport, config)
	client.mu.Lock()
	client.pid = server.Process.Pid
	client.localSocket = socket.endpoint
	client.mu.Unlock()
	runtime := &SharedRuntime{Client: client, SocketPath: socketPath, server: server, proxy: proxy, serverWait: serverWait, proxyWait: proxyWait, cleanup: cleanup}
	go func() {
		err := serverWait.result()
		if err == nil {
			err = ErrClosed
		}
		client.fail(fmt.Errorf("shared codex app-server exited: %w", err))
	}()
	if err := client.Initialize(startupCtx); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	started = true
	return runtime, nil
}

// PID is the owned app-server PID. ProxyPID identifies the private JSONL
// transport child for diagnostics.
func (r *SharedRuntime) PID() int {
	if r == nil || r.server == nil || r.server.Process == nil {
		return 0
	}
	return r.server.Process.Pid
}

func (r *SharedRuntime) ProxyPID() int {
	if r == nil || r.proxy == nil || r.proxy.Process == nil {
		return 0
	}
	return r.proxy.Process.Pid
}

// Close stops both owned children, waits for their reaping goroutines, and
// removes only the captured endpoint and any validated rendezvous alias this
// runtime created. Parent directories can be shared and are left in place.
func (r *SharedRuntime) Close() error {
	if r == nil {
		return nil
	}
	if r.Client != nil {
		_ = r.Client.Close()
	}
	if r.cleanup != nil {
		return r.cleanup()
	}
	return nil
}

type commandWait struct {
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func waitCommand(command *exec.Cmd) *commandWait {
	wait := &commandWait{done: make(chan struct{})}
	go func() {
		err := command.Wait()
		wait.mu.Lock()
		wait.err = err
		wait.mu.Unlock()
		close(wait.done)
	}()
	return wait
}

func (w *commandWait) result() error {
	if w == nil {
		return nil
	}
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func stopCommand(command *exec.Cmd, done *commandWait) error {
	if command == nil || command.Process == nil {
		return nil
	}
	err := command.Process.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	if done != nil {
		select {
		case <-done.done:
		case <-time.After(5 * time.Second):
			return errors.New("timed out waiting for codex child to exit")
		}
	}
	return nil
}

func prepareSocketPath(socketPath string) (string, error) {
	if socketPath == "" {
		return "", errors.New("shared app-server socket path is required")
	}
	if !filepath.IsAbs(socketPath) {
		return "", errors.New("shared app-server socket path must be absolute")
	}
	socketPath = filepath.Clean(socketPath)
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create private socket directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !socketOwnedByCurrentUser(info) {
		return "", errors.New("shared app-server socket directory must be private (0700)")
	}
	if _, err := os.Lstat(socketPath); err == nil {
		return "", errors.New("refusing to adopt an existing app-server socket")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect shared app-server socket: %w", err)
	}
	return socketPath, nil
}

func sharedServerArgs(args []string, socketPath string) ([]string, error) {
	if len(args) == 0 || args[0] != "app-server" {
		return nil, errors.New("shared runtime requires Config.Args to start with app-server")
	}
	result := make([]string, 0, len(args)+2)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--listen" {
			index++
			if index >= len(args) {
				return nil, errors.New("--listen requires a value")
			}
			continue
		}
		if strings.HasPrefix(arg, "--listen=") {
			continue
		}
		result = append(result, arg)
	}
	return append(result, "--listen", "unix://"+socketPath), nil
}

func waitForSocket(ctx context.Context, socketPath string, serverWait *commandWait) (*sharedSocket, error) {
	directory, err := sharedDaemonSocketDirectory()
	if err != nil {
		return nil, err
	}
	return waitForSocketInDirectory(ctx, socketPath, directory, serverWait)
}

func waitForSocketInDirectory(ctx context.Context, socketPath, directory string, serverWait *commandWait) (*sharedSocket, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-serverWait.done:
			return nil, socketServerExited(serverWait)
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		socket, err := inspectSharedSocket(socketPath, directory)
		if err == nil {
			return socket, nil
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errSocketPermissionsPending) {
			return nil, fmt.Errorf("inspect app-server socket: %w", err)
		}
		select {
		case <-serverWait.done:
			return nil, socketServerExited(serverWait)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func socketServerExited(serverWait *commandWait) error {
	err := serverWait.result()
	if err == nil {
		err = ErrClosed
	}
	return fmt.Errorf("shared codex app-server exited before listening: %w", err)
}

var _ io.Closer = (*SharedRuntime)(nil)

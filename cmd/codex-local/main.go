// codex-local owns a short-lived local attachable Codex runtime. It does not
// expose a network listener and does not adopt existing Codex processes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/iaia/telegramgw/internal/codexadapter"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "codex-local:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usage(stdout)
	}
	switch args[0] {
	case "attach":
		return attach(args[1:], stdout, stderr)
	case "start":
		return start(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		return usage(stdout)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage(out io.Writer) error {
	_, err := fmt.Fprint(out, `Usage:
  codex-local attach --socket PATH [THREAD]
  codex-local start

attach opens the local Codex TUI against an existing private Unix socket.
start creates an owned private app-server and JSONL proxy for the current
directory, resumes the latest session in this directory (or creates one), launches the
interactive CLI on the same thread, then stops both children when the
interactive CLI exits. The runtime lasts only for that CLI process.
`)
	return err
}

func attach(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("attach", flag.ContinueOnError)
	flags.SetOutput(stderr)
	socket := flags.String("socket", "", "private app-server Unix socket")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *socket == "" {
		return errors.New("attach requires --socket")
	}
	if !filepath.IsAbs(*socket) {
		return errors.New("socket path must be absolute")
	}
	remaining := flags.Args()
	if len(remaining) > 1 {
		return errors.New("attach accepts at most one thread id")
	}
	return runCodex(context.Background(), attachCommand(*socket, first(remaining)), "", stdout, stderr)
}

func start(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("start", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("start accepts no arguments")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("read current directory: %w", err)
	}
	dir, err := os.MkdirTemp("", "codex-local-")
	if err != nil {
		return fmt.Errorf("create private runtime directory: %w", err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "app-server.sock")
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	// The foreground TUI receives Ctrl-C itself; keep the owning helper alive
	// until it exits so its private children are always reaped.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	runtime, err := codexadapter.StartShared(ctx, codexadapter.Config{WorkingDirectory: cwd, Stderr: stderr, ClientInfo: codexadapter.ClientInfo{Name: "codex-local", Title: "codex-local"}}, socket)
	if err != nil {
		return err
	}
	defer runtime.Close()
	thread, err := resumeLatest(ctx, runtime.Client, cwd)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Local Codex runtime PID %d; socket %s\n", runtime.PID(), runtime.SocketPath)
	args = []string{"--remote", "unix://" + socket}
	if thread.ID != "" {
		args = attachCommand(socket, thread.ID)
	}
	return runCodex(ctx, args, cwd, stdout, stderr)
}

type sessionClient interface {
	LatestThread(context.Context, string) (codexadapter.Thread, bool, error)
	ResumeThread(context.Context, string, codexadapter.ThreadOptions) (codexadapter.Thread, error)
}

func resumeLatest(ctx context.Context, client sessionClient, cwd string) (codexadapter.Thread, error) {
	thread, found, err := client.LatestThread(ctx, cwd)
	if err != nil {
		return codexadapter.Thread{}, fmt.Errorf("find latest session in current directory: %w", err)
	}
	if !found {
		// Empty threads have no persisted rollout yet. Let the terminal create
		// its initial thread instead of trying to resume an unpersisted ID.
		return codexadapter.Thread{}, nil
	}
	thread, err = client.ResumeThread(ctx, thread.ID, codexadapter.ThreadOptions{CWD: cwd})
	if err != nil {
		return codexadapter.Thread{}, fmt.Errorf("resume latest session: %w; if it is open in another app, close it there or use codex-worker attach for a worker-owned session", err)
	}
	return thread, nil
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func attachCommand(socket, thread string) []string {
	command := []string{"--remote", "unix://" + socket, "resume"}
	if thread != "" {
		command = append(command, thread)
	}
	return command
}

func runCodex(ctx context.Context, args []string, cwd string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, "codex", args...)
	command.Dir = cwd
	command.Stdin = os.Stdin
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("run codex: %w", err)
	}
	return nil
}

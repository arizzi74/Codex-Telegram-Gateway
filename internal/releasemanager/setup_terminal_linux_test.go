package releasemanager

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func setupTestTerminal(t *testing.T) (*os.File, *workerSetupTerminal) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Skip("pseudo terminals are not available")
	}
	t.Cleanup(func() { master.Close() })
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	state, err := workerTerminalState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	state.Lflag |= unix.ECHO | unix.ICANON
	if err := setWorkerTerminalState(int(slave.Fd()), state); err != nil {
		t.Fatal(err)
	}
	return master, &workerSetupTerminal{file: slave}
}

func TestSetupTerminalHidesTokenAndRestoresEcho(t *testing.T) {
	for _, cancelInput := range []bool{false, true} {
		t.Run(map[bool]string{false: "answer", true: "cancel"}[cancelInput], func(t *testing.T) {
			master, terminal := setupTestTerminal(t)
			before, err := workerTerminalState(int(terminal.file.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				answer string
				err    error
			}
			done := make(chan result, 1)
			go func() {
				answer, err := terminal.Ask(ctx, "Enrollment token", "", true)
				done <- result{answer, err}
			}()
			poll := []unix.PollFd{{Fd: int32(master.Fd()), Events: unix.POLLIN}}
			if ready, err := unix.Poll(poll, 2000); err != nil || ready == 0 {
				t.Fatal("secret prompt did not appear", err)
			}
			state, err := workerTerminalState(int(terminal.file.Fd()))
			if err != nil || state.Lflag&unix.ECHO != 0 {
				t.Fatal("echo was not disabled before displaying the token prompt", err)
			}
			const token = "test-secret-never-echo"
			if cancelInput {
				if _, err := unix.Write(int(master.Fd()), []byte(token)); err != nil {
					t.Fatal(err)
				}
				cancel()
			} else if _, err := unix.Write(int(master.Fd()), []byte(token+"\n")); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-done:
				if cancelInput {
					if !errors.Is(result.err, context.Canceled) || result.answer != "" {
						t.Fatal("cancelled prompt did not stop cleanly", result.err)
					}
				} else if result.err != nil || result.answer != token {
					t.Fatal("secret prompt did not read the token", result.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("terminal prompt did not finish")
			}
			after, err := workerTerminalState(int(terminal.file.Fd()))
			if err != nil || *before != *after {
				t.Fatal("terminal settings were not restored", err)
			}
			if cancelInput {
				// After cancellation, a newline must not expose the partially typed
				// secret to the next program that reads this terminal.
				if _, err := unix.Write(int(master.Fd()), []byte("\n")); err != nil {
					t.Fatal(err)
				}
				input := []unix.PollFd{{Fd: int32(terminal.file.Fd()), Events: unix.POLLIN}}
				if ready, err := unix.Poll(input, 1000); err != nil || ready == 0 {
					t.Fatal("terminal input unavailable after cancellation", err)
				}
				buffer := make([]byte, 128)
				n, err := unix.Read(int(terminal.file.Fd()), buffer)
				if err != nil || string(buffer[:n]) != "\n" {
					t.Fatal("partial enrollment token was left in terminal input", err)
				}
			}
			var output strings.Builder
			for {
				ready, err := unix.Poll(poll, 20)
				if err != nil {
					t.Fatal(err)
				}
				if ready == 0 {
					break
				}
				buffer := make([]byte, 1024)
				n, err := unix.Read(int(master.Fd()), buffer)
				if err != nil {
					t.Fatal(err)
				}
				output.Write(buffer[:n])
			}
			if strings.Contains(output.String(), token) {
				t.Fatal("terminal echoed the enrollment token")
			}
		})
	}
}

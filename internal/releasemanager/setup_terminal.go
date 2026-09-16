package releasemanager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

type workerSetupTerminal struct{ file *os.File }

func openWorkerSetupTerminal() (workerSetupPrompt, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if _, err := workerTerminalState(int(f.Fd())); err != nil {
		f.Close()
		return nil, err
	}
	return &workerSetupTerminal{file: f}, nil
}

func (t *workerSetupTerminal) Close() error { return t.file.Close() }

func (t *workerSetupTerminal) Ask(ctx context.Context, label, fallback string, secret bool) (answer string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fd := int(t.file.Fd())
	if secret {
		state, err := workerTerminalState(fd)
		if err != nil {
			return "", errors.New("cannot hide the enrollment token on this terminal")
		}
		hidden := *state
		hidden.Lflag &^= unix.ECHO | unix.ECHONL
		if err := setWorkerTerminalState(fd, &hidden); err != nil {
			return "", errors.New("cannot hide the enrollment token on this terminal")
		}
		defer func() {
			// Discard any unfinished secret input before returning to the shell.
			restoreErr := restoreWorkerTerminalState(fd, state)
			fmt.Fprintln(t.file)
			if restoreErr != nil && err == nil {
				answer, err = "", errors.New("could not restore terminal settings")
			}
		}()
	}
	if fallback != "" {
		_, err = fmt.Fprintf(t.file, "%s [%s]: ", label, fallback)
	} else {
		_, err = fmt.Fprintf(t.file, "%s: ", label)
	}
	if err != nil {
		return "", errors.New("could not write the setup prompt")
	}
	var line strings.Builder
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ready, err := unix.Poll(poll, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil || poll[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return "", errors.New("setup terminal is no longer available")
		}
		if ready == 0 {
			continue
		}
		var character [1]byte
		n, err := unix.Read(fd, character[:])
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if err != nil || n == 0 {
			return "", errors.New("setup input ended before completion")
		}
		if character[0] == '\n' {
			value := strings.TrimSuffix(line.String(), "\r")
			if value == "" {
				value = fallback
			}
			return value, nil
		}
		if line.Len() >= 16384 {
			return "", errors.New("setup input exceeds the size limit")
		}
		line.WriteByte(character[0])
	}
}

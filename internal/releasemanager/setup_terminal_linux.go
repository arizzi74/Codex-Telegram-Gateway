package releasemanager

import "golang.org/x/sys/unix"

func workerTerminalState(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TCGETS)
}

func setWorkerTerminalState(fd int, state *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TCSETS, state)
}

func restoreWorkerTerminalState(fd int, state *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TCSETSF, state)
}

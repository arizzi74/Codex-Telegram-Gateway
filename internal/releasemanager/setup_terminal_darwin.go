package releasemanager

import "golang.org/x/sys/unix"

func workerTerminalState(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TIOCGETA)
}

func setWorkerTerminalState(fd int, state *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, state)
}

func restoreWorkerTerminalState(fd int, state *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TIOCSETAF, state)
}

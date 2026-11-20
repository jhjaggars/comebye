//go:build linux

package signin

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openPTY opens a pseudo-terminal pair. The master is read and written by
// comebye; the slave becomes the child's standard streams and controlling
// terminal.
func openPTY() (master, slave *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	m := os.NewFile(uintptr(fd), "/dev/ptmx")
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("pty number: %w", err)
	}
	return finishPTY(m, fmt.Sprintf("/dev/pts/%d", n))
}

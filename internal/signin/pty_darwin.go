//go:build darwin

package signin

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func openPTY() (master, slave *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	m := os.NewFile(uintptr(fd), "/dev/ptmx")
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGRANT), 0); e != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("grant pty: %w", e)
	}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYUNLK), 0); e != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", e)
	}
	var name [128]byte
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("pty name: %w", e)
	}
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	return finishPTY(m, string(name[:n]))
}

//go:build linux || darwin

package signin

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// finishPTY opens the slave by name and gives the terminal a wide window, so
// a long URL is not wrapped by the child.
func finishPTY(m *os.File, name string) (*os.File, *os.File, error) {
	s, err := os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	_ = unix.IoctlSetWinsize(int(s.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 50, Col: 4000})
	return m, s, nil
}

// start runs cmd on the slave as its own session, with the slave as its
// controlling terminal: the master closing hangs it up, and its process
// group id is its pid.
func start(cmd *exec.Cmd, slave *os.File) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	return cmd.Start()
}

// killGroup ends the child's whole process group.
func killGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

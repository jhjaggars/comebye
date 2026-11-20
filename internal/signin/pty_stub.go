//go:build !linux && !darwin

package signin

import (
	"os"
	"os/exec"
)

func finishPTY(m *os.File, _ string) (*os.File, *os.File, error) { return m, m, nil }
func start(cmd *exec.Cmd, _ *os.File) error                      { return cmd.Start() }
func killGroup(int)                                              {}

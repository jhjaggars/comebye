package gitfetch

import "os/exec"

func execLook(s string) (string, error)          { return exec.LookPath(s) }
func execCmd(name string, a ...string) *exec.Cmd { return exec.Command(name, a...) }

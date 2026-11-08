package upgrade

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Restart modes. systemd restarts the user unit through systemctl (the
// default on a host that has the unit). exit sends the running server
// SIGTERM, which makes it exit 0, and relies on a supervisor that starts it
// again (the init loop of a sandbox host, which has no systemd). auto picks
// systemd when the unit exists and exit otherwise.
const (
	RestartAuto    = "auto"
	RestartSystemd = "systemd"
	RestartExit    = "exit"
)

// resolveMode settles o.Restart to RestartSystemd or RestartExit.
func resolveMode(o *Options) error {
	switch o.Restart {
	case "", RestartAuto:
		if _, err := exec.LookPath(o.Systemctl); err != nil {
			o.Restart = RestartExit
			return nil
		}
		cmd := exec.Command(o.Systemctl, "--user", "cat", o.Service)
		cmd.Env = o.Env
		if err := cmd.Run(); err != nil {
			o.Restart = RestartExit
			return nil
		}
		o.Restart = RestartSystemd
	case RestartSystemd, RestartExit:
	default:
		return fmt.Errorf("unknown restart mode %q (want auto, systemd or exit)", o.Restart)
	}
	return nil
}

func (o *Options) exitMode() bool { return o.Restart == RestartExit }

// restartCmdHint is the manual restart named in messages.
func restartCmdHint(o *Options) string {
	if o.exitMode() {
		return "send the comebye server SIGTERM (its supervisor starts it again)"
	}
	return "systemctl --user restart " + o.Service
}

// restartServer restarts the server in the chosen mode.
func restartServer(o *Options) error {
	if !o.exitMode() {
		return systemctl(o, "restart", o.Service)
	}
	pids, err := serverPIDs(o)
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		// Between a crash and the supervisor's restart, or never started:
		// the supervisor (or the operator) starts the installed binary.
		o.printf("no running comebye server found: it starts on the installed binary when the supervisor next starts it\n")
		return nil
	}
	for _, pid := range pids {
		o.printf("stopping the comebye server (pid %d) with SIGTERM; its supervisor restarts it\n", pid)
		if err := o.signal(pid); err != nil {
			return fmt.Errorf("signal pid %d: %w", pid, err)
		}
	}
	// Wait for them to go: until then the old build still answers.
	for i := 0; ; i++ {
		alive := false
		for _, pid := range pids {
			if o.isAlive(pid) {
				alive = true
			}
		}
		if !alive {
			return nil
		}
		if i >= o.SmokeTries {
			return fmt.Errorf("the comebye server (pids %v) did not exit after SIGTERM", pids)
		}
		o.Sleep(o.SmokeWait)
	}
}

// waitUp waits for the restarted service: active under systemd; in exit mode
// the supervisor's restart, seen as the local server answering again (it
// takes a few seconds: the supervisor waits before it starts the server).
func waitUp(o *Options) error {
	if !o.exitMode() {
		return waitActive(o)
	}
	tries := max(o.SmokeTries, 30)
	for i := 0; i < tries; i++ {
		if indexTag(o) != "" {
			return nil
		}
		o.Sleep(o.SmokeWait)
	}
	return fmt.Errorf("the local server at %s did not come back after it was stopped: is a supervisor starting it again? (start it by hand, or restore %s)", o.LocalURL, o.Bin+".prev")
}

func (o *Options) signal(pid int) error {
	if o.Kill != nil {
		return o.Kill(pid)
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}

func (o *Options) isAlive(pid int) bool {
	if o.Alive != nil {
		return o.Alive(pid)
	}
	return syscall.Kill(pid, 0) == nil && !zombie(pid)
}

// zombie reports a process that has exited but is not yet reaped (it
// still answers signal 0).
func zombie(pid int) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	return i >= 0 && strings.HasPrefix(strings.TrimSpace(string(b[i+1:])), "Z")
}

// serverPIDs is the server to stop: --pid when given, else the comebye
// server processes of this user (see findServers).
func serverPIDs(o *Options) ([]int, error) {
	if o.PID > 0 {
		return []int{o.PID}, nil
	}
	root := o.ProcRoot
	if root == "" {
		root = "/proc"
	}
	procs, err := findServers(root, os.Getpid(), os.Getuid())
	if err != nil {
		return nil, err
	}
	// Several servers (a shared host, another checkout's dev server): the
	// ones running this install's binary are the ones to restart.
	if len(procs) > 1 {
		var mine []serverProc
		for _, p := range procs {
			if p.exe != "" && p.exe == cleanExe(o.Bin) {
				mine = append(mine, p)
			}
		}
		if len(mine) > 0 {
			procs = mine
		}
	}
	var pids []int
	for _, p := range procs {
		pids = append(pids, p.pid)
	}
	if len(pids) > 1 {
		return nil, fmt.Errorf("%d comebye servers run as this user (pids %v): pass --pid for the one to restart", len(pids), pids)
	}
	return pids, nil
}

type serverProc struct {
	pid int
	exe string // the executable, "" when unreadable
}

// cleanExe resolves symlinks of an installed binary path.
func cleanExe(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// findServers lists the processes under procRoot that run the comebye
// server: the command is comebye with no subcommand (no arguments, or only
// flags), owned by uid, and not self.
func findServers(procRoot string, self, uid int) ([]serverProc, error) {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	var out []serverProc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		fi, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if filepath.Base(args[0]) != "comebye" {
			continue
		}
		if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
			continue // a CLI subcommand (comebye agents ...)
		}
		if slices.ContainsFunc(args[1:], func(a string) bool {
			return a == "-version" || a == "--version" || a == "-h" || a == "-help" || a == "--help"
		}) {
			continue // asks and exits, not a server
		}
		exe, _ := os.Readlink(filepath.Join(dir, "exe"))
		out = append(out, serverProc{pid, strings.TrimSuffix(exe, " (deleted)")})
	}
	return out, nil
}

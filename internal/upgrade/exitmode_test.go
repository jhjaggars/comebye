package upgrade

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// exitEnv adds a fake supervised server to env: Kill "stops" it, and the
// local test server (always up) plays the restarted one.
type exitEnv struct {
	*env
	mu     sync.Mutex
	killed []int
}

func newExitEnv(t *testing.T, installed string) *exitEnv {
	return &exitEnv{env: newEnv(t, installed)}
}

func (x *exitEnv) opts() Options {
	o := x.env.opts()
	o.Restart = RestartExit
	o.PID = 4242
	o.Kill = func(pid int) error {
		x.mu.Lock()
		defer x.mu.Unlock()
		x.killed = append(x.killed, pid)
		return nil
	}
	o.Alive = func(pid int) bool { return false }
	return o
}

func (x *exitEnv) systemctlCalls() string {
	b, _ := os.ReadFile(filepath.Join(x.state, "systemctl.log"))
	return string(b)
}

func TestExitModeUpgrade(t *testing.T) {
	x := newExitEnv(t, "v0.1.0")
	if err := Run(x.opts()); err != nil {
		t.Fatalf("%v\n%s\n%s", err, x.out.String(), x.err.String())
	}
	if got := x.version(x.bin); got != "comebye v0.2.0" {
		t.Fatalf("installed: %s", got)
	}
	if got := x.version(x.bin + ".prev"); got != "comebye v0.1.0" {
		t.Fatalf(".prev: %s", got)
	}
	if len(x.killed) != 1 || x.killed[0] != 4242 {
		t.Fatalf("signalled %v, want [4242]", x.killed)
	}
	if c := x.systemctlCalls(); c != "" {
		t.Fatalf("systemctl was used in exit mode:\n%s", c)
	}
	for _, want := range []string{"restart mode: exit", "checksum ok", "SIGTERM", "smoke: the local server answers again", "smoke: whoami ok", "no journal check", "upgraded v0.1.0 -> v0.2.0", "send the comebye server SIGTERM"} {
		if !strings.Contains(x.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, x.out.String())
		}
	}
}

func TestExitModeSmokeFailureRollsBack(t *testing.T) {
	x := newExitEnv(t, "v0.1.0")
	x.set("fail_ledger", "boom")
	err := Run(x.opts())
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("err = %v\n%s", err, x.err.String())
	}
	if got := x.version(x.bin); got != "comebye v0.1.0" {
		t.Fatalf("after rollback: %s", got)
	}
	if len(x.killed) != 2 {
		t.Fatalf("signalled %v, want two (install, restore)", x.killed)
	}
	if x.systemctlCalls() != "" {
		t.Fatal("systemctl was used in exit mode")
	}
}

func TestExitModeNothingRestartsTheServer(t *testing.T) {
	x := newExitEnv(t, "v0.1.0")
	x.local.Close() // the supervisor never brings the server back
	err := Run(x.opts())
	if !errors.Is(err, ErrRolledBack) && (err == nil || !strings.Contains(err.Error(), "supervisor")) {
		t.Fatalf("err = %v", err)
	}
	if x.version(x.bin) != "comebye v0.1.0" {
		t.Fatal("the old binary was not restored")
	}
}

func TestExitModeServerDoesNotExit(t *testing.T) {
	x := newExitEnv(t, "v0.1.0")
	o := x.opts()
	o.Alive = func(int) bool { return true }
	err := Run(o)
	if err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("err = %v", err)
	}
}

func TestExitModeRollbackCommand(t *testing.T) {
	x := newExitEnv(t, "v0.1.0")
	if err := Run(x.opts()); err != nil {
		t.Fatal(err)
	}
	o := x.opts()
	o.Rollback = true
	if err := Run(o); err != nil {
		t.Fatalf("%v\n%s", err, x.err.String())
	}
	if x.version(x.bin) != "comebye v0.1.0" || len(x.killed) != 2 {
		t.Fatalf("version %s, signalled %v", x.version(x.bin), x.killed)
	}
}

func TestExitModeDryRunChangesNothing(t *testing.T) {
	x := newExitEnv(t, "v0.1.0")
	o := x.opts()
	o.DryRun = true
	if err := Run(o); err != nil {
		t.Fatal(err)
	}
	if len(x.killed) != 0 || x.version(x.bin) != "comebye v0.1.0" {
		t.Fatal("dry run changed things")
	}
}

// Auto mode: the systemd path when the unit exists (the fake systemctl
// answers `cat`), exit when systemctl is missing or has no such unit.
func TestAutoModePicksTheRestart(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	o := e.opts()
	if err := resolveMode(&o); err != nil || o.Restart != RestartSystemd {
		t.Fatalf("with the unit: %q, %v", o.Restart, err)
	}
	o = e.opts()
	o.Systemctl = filepath.Join(e.state, "no-such-systemctl")
	if err := resolveMode(&o); err != nil || o.Restart != RestartExit {
		t.Fatalf("no systemctl: %q, %v", o.Restart, err)
	}
	failing := filepath.Join(e.state, "tools", "systemctl-nounit")
	os.WriteFile(failing, []byte("#!/bin/sh\n[ \"$2\" = cat ] && exit 1\nexit 0\n"), 0o755)
	o = e.opts()
	o.Systemctl = failing
	if err := resolveMode(&o); err != nil || o.Restart != RestartExit {
		t.Fatalf("no unit: %q, %v", o.Restart, err)
	}
	o.Restart = "bogus"
	if err := resolveMode(&o); err == nil {
		t.Fatal("bogus mode accepted")
	}
}

// systemd mode keeps its behaviour when asked for explicitly.
func TestSystemdModeExplicit(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	o := e.opts()
	o.Restart = RestartSystemd
	o.Kill = func(int) error { t.Fatal("signalled in systemd mode"); return nil }
	if err := Run(o); err != nil {
		t.Fatalf("%v\n%s", err, e.err.String())
	}
	if e.restarts() != 1 || !strings.Contains(e.out.String(), "smoke: comebye is active") {
		t.Fatalf("restarts %d:\n%s", e.restarts(), e.out.String())
	}
}

func fakeProc(t *testing.T, procs map[int][]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, args := range procs {
		d := filepath.Join(root, fmt.Sprint(pid))
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0o644)
	}
	os.MkdirAll(filepath.Join(root, "self"), 0o755)
	return root
}

func TestFindServers(t *testing.T) {
	root := fakeProc(t, map[int][]string{
		10: {"/home/u/.local/bin/comebye", "-addr", "127.0.0.1:8741"}, // the server
		11: {"comebye"},                                               // the server, no flags
		12: {"comebye", "agents", "--json"},                           // a CLI call
		13: {"/home/u/.local/bin/comebye", "-version"},                // asks and exits
		14: {"vim", "comebye"},                                        // not comebye
		15: {"comebye", "upgrade"},                                    // self-like
		16: {"/usr/local/bin/comebye", "-ts-socket", "/x"},            // the image's copy
	})
	got, err := findServers(root, 15, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]bool{10: true, 11: true, 16: true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected pid %d", p)
		}
	}
	// A different uid owns none of them.
	if got, _ := findServers(root, 0, os.Getuid()+1); len(got) != 0 {
		t.Fatalf("other uid: %v", got)
	}
}

func TestSeveralServersNeedPID(t *testing.T) {
	root := fakeProc(t, map[int][]string{10: {"comebye"}, 11: {"comebye", "-addr", ":1"}})
	o := Options{ProcRoot: root}
	if _, err := serverPIDs(&o); err == nil || !strings.Contains(err.Error(), "--pid") {
		t.Fatalf("err = %v", err)
	}
	o.PID = 11
	if pids, err := serverPIDs(&o); err != nil || len(pids) != 1 || pids[0] != 11 {
		t.Fatalf("%v %v", pids, err)
	}
}

func TestNoServerRunning(t *testing.T) {
	x := newExitEnv(t, "v0.1.0")
	o := x.opts()
	o.PID = 0
	o.ProcRoot = fakeProc(t, nil)
	if err := Run(o); err != nil {
		t.Fatalf("%v\n%s\n%s", err, x.out.String(), x.err.String())
	}
	if len(x.killed) != 0 || !strings.Contains(x.out.String(), "no running comebye server found") {
		t.Fatalf("killed %v\n%s", x.killed, x.out.String())
	}
}

// The real signal path: SIGTERM ends a process, and a zombie does not count as alive.
func TestRealSIGTERM(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	o := Options{Restart: RestartExit, PID: cmd.Process.Pid, SmokeTries: 5, SmokeWait: 50 * time.Millisecond, Out: io.Discard}
	o.defaults()
	if !o.isAlive(cmd.Process.Pid) {
		t.Fatal("not alive before")
	}
	if err := restartServer(&o); err != nil { // exits as soon as the child is a zombie
		t.Fatal(err)
	}
	if o.isAlive(cmd.Process.Pid) {
		t.Fatal("alive after SIGTERM")
	}
}

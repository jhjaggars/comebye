package idle

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

type rig struct {
	t      *testing.T
	now    time.Time
	epoch  string
	agents []herdr.AgentInfo
	err    error
	proc   string
	stamp  string
}

func newRig(t *testing.T) *rig {
	return &rig{t: t, now: time.Unix(1_700_000_000, 0), epoch: "boot1:100", proc: t.TempDir(), stamp: filepath.Join(t.TempDir(), "state", "idle-stamp")}
}

func (r *rig) ssh(pids ...string) {
	for i, c := range pids {
		d := filepath.Join(r.proc, fmt.Sprint(900+i))
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "cmdline"), []byte(c+"\x00"), 0o644)
	}
}

func (r *rig) check(minutes int, noTouch bool) Result {
	r.t.Helper()
	res, err := Check(context.Background(), Options{
		Minutes: minutes, Stamp: r.stamp, NoTouch: noTouch, ProcRoot: r.proc,
		Now:        func() time.Time { return r.now },
		Epoch:      func() string { return r.epoch },
		ListAgents: func(context.Context) ([]herdr.AgentInfo, error) { return r.agents, r.err },
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func TestIdleAfterTheLimit(t *testing.T) {
	r := newRig(t)
	r.agents = []herdr.AgentInfo{{Status: herdr.StatusIdle}, {Status: herdr.StatusBlocked}, {Status: herdr.StatusDone}}
	if res := r.check(30, false); res.Idle || res.IdleMinutes != 0 || len(res.Reasons) != 0 {
		t.Fatalf("first poll starts the clock: %+v", res)
	}
	r.now = r.now.Add(29 * time.Minute)
	if res := r.check(30, false); res.Idle || res.IdleMinutes != 29 {
		t.Fatalf("29 min: %+v", res)
	}
	r.now = r.now.Add(time.Minute)
	res := r.check(30, false)
	if !res.Idle || res.IdleMinutes != 30 || !strings.Contains(res.Summary(), "idle for 30 min") {
		t.Fatalf("30 min: %+v / %s", res, res.Summary())
	}
}

func TestWorkingAgentRenewsTheClock(t *testing.T) {
	r := newRig(t)
	r.check(10, false)
	r.now = r.now.Add(20 * time.Minute)
	r.agents = []herdr.AgentInfo{{Status: herdr.StatusWorking}, {Status: herdr.StatusIdle}}
	res := r.check(10, false)
	if res.Idle || res.AgentsWorking != 1 || !strings.Contains(res.Summary(), "1 herdr agent(s) working") {
		t.Fatalf("%+v", res)
	}
	r.agents = nil
	r.now = r.now.Add(9 * time.Minute)
	if res := r.check(10, false); res.Idle {
		t.Fatalf("the clock restarted at the last working poll: %+v", res)
	}
	r.now = r.now.Add(time.Minute)
	if res := r.check(10, false); !res.Idle {
		t.Fatalf("%+v", res)
	}
}

func TestInteractiveSSHCountsBareSSHDoesNot(t *testing.T) {
	r := newRig(t)
	r.ssh("sshd: alice@notty", "sshd: alice [priv]", "sshd: bob@pts/3")
	res := r.check(0, false)
	if res.Idle || res.SSHSessions != 1 {
		t.Fatalf("%+v", res)
	}
	r2 := newRig(t)
	r2.ssh("sshd: alice@notty", "sshd: alice [priv]")
	if res := r2.check(0, false); !res.Idle || res.SSHSessions != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestHerdrStates(t *testing.T) {
	// not running (no socket, refused): not activity
	for _, err := range []error{
		fmt.Errorf("dial herdr socket: %w", &net.OpError{Op: "dial", Err: os.ErrNotExist}),
		fmt.Errorf("dial herdr socket: %w", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}),
	} {
		r := newRig(t)
		r.err = err
		if res := r.check(0, false); !res.Idle || res.HerdrState != "not_running" {
			t.Fatalf("%v: %+v", err, res)
		}
	}
	// there and not answering: a guess is not a reason to stop
	r := newRig(t)
	r.err = errors.New("read response: i/o timeout")
	res := r.check(0, false)
	if res.Idle || res.HerdrState != "no_answer" || !strings.Contains(res.Summary(), "did not answer") {
		t.Fatalf("%+v", res)
	}
}

func TestNewBootStartsANewClock(t *testing.T) {
	r := newRig(t)
	r.check(60, false)
	r.now = r.now.Add(10 * time.Hour) // stopped, started again
	r.epoch = "boot1:200"
	if res := r.check(60, false); res.Idle || res.IdleMinutes != 0 {
		t.Fatalf("a restarted host counted the time it was off: %+v", res)
	}
	r.now = r.now.Add(60 * time.Minute)
	if res := r.check(60, false); !res.Idle {
		t.Fatalf("%+v", res)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	r := newRig(t)
	r.check(5, true)
	if _, err := os.Stat(r.stamp); err == nil {
		t.Fatal("dry run wrote the stamp")
	}
	r.check(5, false)
	b, _ := os.ReadFile(r.stamp)
	r.now = r.now.Add(time.Hour)
	r.agents = []herdr.AgentInfo{{Status: herdr.StatusWorking}}
	r.check(5, true)
	if b2, _ := os.ReadFile(r.stamp); string(b) != string(b2) {
		t.Fatal("dry run renewed the stamp")
	}
}

func TestBootEpoch(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sys/kernel/random"), 0o755)
	os.MkdirAll(filepath.Join(root, "1"), 0o755)
	os.WriteFile(filepath.Join(root, "sys/kernel/random/boot_id"), []byte("abc-123\n"), 0o644)
	os.WriteFile(filepath.Join(root, "1/stat"), []byte("1 (my (init)) S 0 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10 18446744073709551615"), 0o644)
	if got := bootEpoch(root); got != "abc-123:987654" {
		t.Fatalf("epoch = %q", got)
	}
}

// Against a real herdr-protocol socket: a working agent is activity.
func TestOverASocket(t *testing.T) {
	dir, _ := os.MkdirTemp("", "idl")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 4096)
			n, _ := c.Read(buf)
			id := ""
			if i := strings.Index(string(buf[:n]), `"id":"`); i >= 0 {
				id = strings.SplitN(string(buf[i+6:n]), `"`, 2)[0]
			}
			fmt.Fprintf(c, `{"id":%q,"result":{"type":"agent_list","agents":[{"name":"a","agent_status":"working"}]}}`+"\n", id)
			c.Close()
		}
	}()
	res, err := Check(context.Background(), Options{Minutes: 0, Stamp: filepath.Join(dir, "s"), ProcRoot: t.TempDir(), Herdr: herdr.NewClient(sock)})
	if err != nil || res.Idle || res.AgentsWorking != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	ln.Close()
	os.Remove(sock)
	res, _ = Check(context.Background(), Options{Minutes: 0, Stamp: filepath.Join(dir, "s"), ProcRoot: t.TempDir(), Herdr: herdr.NewClient(sock)})
	if !res.Idle || res.HerdrState != "not_running" {
		t.Fatalf("no socket: %+v", res)
	}
}

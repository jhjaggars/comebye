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
	r.agents = []herdr.AgentInfo{{Status: herdr.StatusIdle}, {Status: herdr.StatusUnknown}, {Status: herdr.StatusDone}}
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

// R2: an agent waiting on the user is not an idle host. Stopping it would
// throw away the session that asked the question.
func TestBlockedAgentKeepsTheHostUp(t *testing.T) {
	r := newRig(t)
	r.check(30, false)
	r.agents = []herdr.AgentInfo{{Status: herdr.StatusIdle}, {Status: herdr.StatusBlocked}}
	r.now = r.now.Add(40 * time.Minute)
	res := r.check(30, false)
	if res.Idle || !strings.Contains(res.Summary(), "1 herdr agent(s) blocked") {
		t.Fatalf("a blocked agent counted as idle: %+v / %s", res, res.Summary())
	}
	// Once it is answered (idle again) the clock runs from the last blocked poll.
	r.agents = []herdr.AgentInfo{{Status: herdr.StatusIdle}}
	r.now = r.now.Add(29 * time.Minute)
	if res := r.check(30, false); res.Idle {
		t.Fatalf("%+v", res)
	}
	r.now = r.now.Add(time.Minute)
	if res := r.check(30, false); !res.Idle {
		t.Fatalf("%+v", res)
	}
}

func TestBlockedAgentHoldsOnlyForBlockedMax(t *testing.T) {
	r := newRig(t)
	r.agents = []herdr.AgentInfo{{Status: herdr.StatusBlocked}}
	opts := func() Options {
		return Options{Minutes: 30, Stamp: r.stamp, ProcRoot: r.proc, BlockedMax: 2 * time.Hour,
			Now: func() time.Time { return r.now }, Epoch: func() string { return r.epoch },
			ListAgents: func(context.Context) ([]herdr.AgentInfo, error) { return r.agents, nil }}
	}
	run := func() Result {
		res, err := Check(context.Background(), opts())
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for i := 0; i < 24; i++ { // every 5 minutes, 0 to 1h55
		if res := run(); res.Idle {
			t.Fatalf("poll %d: %+v", i, res)
		}
		r.now = r.now.Add(5 * time.Minute)
	}
	// 2h blocked: no longer a reason; the idle clock last ran at 1h55.
	res := run()
	if res.Idle || len(res.Reasons) != 0 || res.BlockedMinutes != 120 || res.IdleMinutes != 5 {
		t.Fatalf("at 2h blocked is no longer a reason; the stamp was renewed at 1h55: %+v", res)
	}
	r.now = r.now.Add(30 * time.Minute)
	res = run()
	if !res.Idle || !strings.Contains(res.Summary(), "1 blocked for 150 min (past the limit)") {
		t.Fatalf("%+v / %s", res, res.Summary())
	}
	// A blocked spell that ends restarts the blocked clock.
	r.agents = nil
	run()
	r.agents = []herdr.AgentInfo{{Status: herdr.StatusBlocked}}
	if res := run(); res.Idle || res.BlockedMinutes != 0 {
		t.Fatalf("a new blocked spell: %+v", res)
	}
}

func TestBlockedMaxDefaultAndNoLimit(t *testing.T) {
	for _, c := range []struct {
		max      time.Duration
		after    time.Duration
		wantIdle bool
	}{
		{0, DefaultBlockedMax - time.Minute, false},
		{0, DefaultBlockedMax + 31*time.Minute, true},
		{-1, 30 * 24 * time.Hour, false},
	} {
		r := newRig(t)
		r.agents = []herdr.AgentInfo{{Status: herdr.StatusBlocked}}
		o := Options{Minutes: 30, Stamp: r.stamp, ProcRoot: r.proc, BlockedMax: c.max,
			Now: func() time.Time { return r.now }, Epoch: func() string { return r.epoch },
			ListAgents: func(context.Context) ([]herdr.AgentInfo, error) { return r.agents, nil }}
		Check(context.Background(), o)
		// The blocked clock is from the first poll; jump, poll once to pass
		// the limit, then let the idle clock run out.
		r.now = r.now.Add(c.after - 31*time.Minute)
		Check(context.Background(), o)
		r.now = r.now.Add(31 * time.Minute)
		res, _ := Check(context.Background(), o)
		if res.Idle != c.wantIdle {
			t.Errorf("max %v after %v: %+v", c.max, c.after, res)
		}
	}
}

func TestHoldsKeepTheHostUpUntilTheyExpire(t *testing.T) {
	r := newRig(t)
	holds := filepath.Join(t.TempDir(), "idle-holds")
	check := func() Result {
		res, err := Check(context.Background(), Options{Minutes: 30, Stamp: r.stamp, ProcRoot: r.proc, Holds: holds,
			Now: func() time.Time { return r.now }, Epoch: func() string { return r.epoch },
			ListAgents: func(context.Context) ([]herdr.AgentInfo, error) { return nil, nil }})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	check()
	r.now = r.now.Add(29 * time.Minute)
	// A handoff is typed into an idle pane a minute before the limit.
	if err := SetHold(holds, "prompt", r.now.Add(15*time.Minute), "a prompt for w1"); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(10 * time.Minute)
	res := check()
	if res.Idle || len(res.Holds) != 1 || !strings.Contains(res.Summary(), "a prompt for w1") {
		t.Fatalf("a hold did not keep the host up: %+v / %s", res, res.Summary())
	}
	// Expired: the clock runs from the last poll that saw the hold.
	r.now = r.now.Add(10 * time.Minute)
	if res := check(); res.Idle || len(res.Holds) != 0 || res.IdleMinutes != 10 {
		t.Fatalf("%+v", res)
	}
	r.now = r.now.Add(20 * time.Minute)
	if res := check(); !res.Idle {
		t.Fatalf("%+v", res)
	}
}

func TestSetHoldKeepsTheLaterTime(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "h")
	now := time.Unix(1_700_000_000, 0)
	SetHold(dir, "upgrade", now.Add(15*time.Minute), "upgrade to v1.2.3")
	SetHold(dir, "upgrade", now.Add(5*time.Minute), "shorter")
	hs := ActiveHolds(dir, now)
	if len(hs) != 1 || hs[0].Reason != "upgrade to v1.2.3" || !hs[0].Until.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("%+v", hs)
	}
	SetHold(dir, "prompt", now.Add(20*time.Minute), "p")
	if hs := ActiveHolds(dir, now); len(hs) != 2 || hs[0].Name != "prompt" {
		t.Fatalf("latest first: %+v", hs)
	}
	if err := SetHold(dir, "../x", now, "r"); err == nil {
		t.Fatal("a path as a hold name")
	}
	os.WriteFile(filepath.Join(dir, "junk"), []byte("not a time"), 0o644)
	ClearHold(dir, "prompt")
	ClearHold(dir, "missing")
	if hs := ActiveHolds(dir, now); len(hs) != 1 || hs[0].Name != "upgrade" {
		t.Fatalf("%+v", hs)
	}
	if hs := ActiveHolds(dir, now.Add(15*time.Minute)); len(hs) != 0 {
		t.Fatalf("an expired hold: %+v", hs)
	}
}

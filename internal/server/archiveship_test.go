package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeComebye writes a stand-in comebye binary: it appends its niceness and
// its arguments to log, then runs body.
func fakeComebye(t *testing.T, body string) (bin, logf string) {
	t.Helper()
	dir := t.TempDir()
	bin, logf = filepath.Join(dir, "comebye"), filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"nice=$(nice) $*\" >> '" + logf + "'\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logf
}

func calls(t *testing.T, logf string) []string {
	t.Helper()
	b, err := os.ReadFile(logf)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func peerForShip(t *testing.T) *Server {
	t.Helper()
	return newServer(t, Config{AllowLogins: []string{"me@example.com"}, HostName: "desk", StateDir: t.TempDir(), NoLedger: true, AgentToken: testToken}, nil, nil)
}

// The hub copies its own sessions and builds the all-hosts index; a peer
// only copies. Both run niced, at their own intervals.
func TestShipHubIndexesAndPeerOnlyShips(t *testing.T) {
	bin, logf := fakeComebye(t, "")
	hub, _, _ := coordServer(t)
	peer := peerForShip(t)
	lock := filepath.Join(t.TempDir(), "comebye-archive.lock")
	for name, s := range map[string]*Server{"hub": hub, "peer": peer} {
		sh := newShipper(s, &ArchiveShip{Bin: bin, Lock: lock})
		if sh == nil {
			t.Fatalf("%s: no ship stage", name)
		}
		os.Remove(logf)
		t0 := time.Now()
		sh.step(context.Background(), t0) // the first runs wait shipFirst
		if got := calls(t, logf); len(got) != 0 {
			t.Fatalf("%s ran at once: %v", name, got)
		}
		sh.step(context.Background(), t0.Add(shipFirst))
		got := calls(t, logf)
		want := []string{"archive run"}
		if name == "hub" {
			want = append(want, "archive index --all-hosts --if-changed")
		}
		if len(got) != len(want) {
			t.Fatalf("%s ran %v, want %v", name, got, want)
		}
		for i, w := range want {
			if !strings.HasPrefix(got[i], "nice=10 ") || !strings.Contains(got[i], w) {
				t.Errorf("%s run %d = %q, want %q niced", name, i, got[i], w)
			}
		}
		// Five minutes on, only the copy is due again; the index is hourly.
		os.Remove(logf)
		sh.step(context.Background(), time.Now().Add(shipEvery+time.Second))
		if got := calls(t, logf); len(got) != 1 || !strings.Contains(got[0], "archive run") {
			t.Fatalf("%s after 5 minutes ran %v", name, got)
		}
	}
	// A hub with no agents (hub-only) has nothing to copy: it only indexes.
	hub.cfg.HubOnly = true
	sh := newShipper(hub, &ArchiveShip{Bin: bin, Lock: lock})
	if len(sh.jobs) != 1 || sh.jobs[0].what != "index" {
		t.Fatalf("hub-only jobs = %+v", sh.jobs)
	}
	// No archive set up: no stage.
	if newShipper(peer, nil) != nil {
		t.Fatal("a ship stage with no archive")
	}
}

// A run that finds the lock held (the old timer still going) skips, and the
// next tick runs; the child holds the lock while it runs.
func TestShipRespectsTheLock(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "comebye-archive.lock")
	// The child checks, from its fd 3, that it holds the lock.
	mark := filepath.Join(dir, "not-held")
	bin, logf := fakeComebye(t, "flock -n '"+lock+"' true && touch '"+mark+"'; true")
	peer := peerForShip(t)
	sh := newShipper(peer, &ArchiveShip{Bin: bin, Lock: lock})
	sh.first = 0
	held, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	sh.step(context.Background(), time.Now())
	if got := calls(t, logf); len(got) != 0 || sh.lockHeld != 1 {
		t.Fatalf("ran under someone else's lock: %v (skipped %d)", got, sh.lockHeld)
	}
	held.Close()
	sh.step(context.Background(), time.Now())
	got := calls(t, logf)
	if len(got) != 1 || !strings.Contains(got[0], "archive run") {
		t.Fatalf("after the lock was let go: %v", got)
	}
	if _, err := exec.LookPath("flock"); err == nil {
		if _, err := os.Stat(mark); err == nil {
			t.Fatal("the child ran without holding the lock")
		}
	}
	// The lock is free again once the run is over.
	f, err := flockTry(lock)
	if err != nil {
		t.Fatalf("lock still held after the run: %v", err)
	}
	f.Close()
}

// A run past its time limit is stopped, its process group with it, and the
// stage moves on.
func TestShipStopsARunAtItsTimeLimit(t *testing.T) {
	dir := t.TempDir()
	pidf := filepath.Join(dir, "pid")
	bin, _ := fakeComebye(t, "sleep 30 & echo $! > '"+pidf+"'; wait")
	peer := peerForShip(t)
	sh := newShipper(peer, &ArchiveShip{Bin: bin, Lock: filepath.Join(dir, "lock")})
	sh.first = 0
	sh.jobs[0].limit = 300 * time.Millisecond
	t0 := time.Now()
	sh.step(context.Background(), t0)
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("the stage waited %s for a run with a 300ms limit", d)
	}
	if e := sh.jobs[0].lastErr; !strings.Contains(e, "time limit") {
		t.Fatalf("lastErr = %q", e)
	}
	b, err := os.ReadFile(pidf)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(strings.TrimSpace(string(b)), &pid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("the run's child %d is still alive", pid)
	}
	// The next run is at its own interval, not at once.
	if !sh.jobs[0].next.After(time.Now().Add(shipEvery - time.Minute)) {
		t.Fatalf("next = %s", sh.jobs[0].next)
	}
}

// The old outside schedulers are found where systemd installs them.
func TestOldArchiveTimersAreFound(t *testing.T) {
	d := t.TempDir()
	if got := oldArchiveTimers([]string{d}); len(got) != 0 {
		t.Fatalf("found %v in an empty dir", got)
	}
	os.MkdirAll(filepath.Join(d, "timers.target.wants"), 0o700)
	os.WriteFile(filepath.Join(d, "comebye-archive.timer"), nil, 0o600)
	os.Symlink("../comebye-worker-archive.timer", filepath.Join(d, "timers.target.wants", "comebye-worker-archive.timer"))
	got := oldArchiveTimers([]string{d})
	if len(got) != 2 || got[0].unit != "comebye-archive.timer" || got[1].unit != "comebye-worker-archive.timer" {
		t.Fatalf("found %+v", got)
	}
}

package hublock

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fast(o Options) Options {
	o.Heartbeat = 20 * time.Millisecond
	o.LeaseTTL = 2 * time.Second
	return o
}

func TestSecondHolderRefusedWithTheHoldersDetails(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir, fast(Options{Name: "hub", Addr: "127.0.0.1:1"}))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	_, err = Acquire(dir, fast(Options{Name: "other"}))
	var held *HeldError
	if !errors.As(err, &held) {
		t.Fatalf("want HeldError, got %v", err)
	}
	if held.Why != "flock" || held.Holder.PID != os.Getpid() || held.Holder.Name != "hub" || held.Holder.Started.IsZero() {
		t.Fatalf("holder = %+v", held)
	}
	for _, want := range []string{"in use by another comebye", "pid ", "host-name hub", "started"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
}

func TestReleaseFreesTheLock(t *testing.T) {
	dir := t.TempDir()
	l, _ := Acquire(dir, fast(Options{}))
	l.Release()
	l2, err := Acquire(dir, fast(Options{}))
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l2.Release()
	if b, _ := os.ReadFile(filepath.Join(dir, FileName)); len(b) != 0 {
		t.Errorf("a released lock leaves an empty record, got %q", b)
	}
}

func TestWaitGetsTheLockWhenTheHolderReleases(t *testing.T) {
	dir := t.TempDir()
	l, _ := Acquire(dir, fast(Options{}))
	go func() { time.Sleep(300 * time.Millisecond); l.Release() }()
	l2, err := Acquire(dir, fast(Options{Wait: 5 * time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
}

func TestWaitTimesOut(t *testing.T) {
	dir := t.TempDir()
	l, _ := Acquire(dir, fast(Options{}))
	defer l.Release()
	start := time.Now()
	_, err := Acquire(dir, fast(Options{Wait: 1200 * time.Millisecond}))
	if err == nil || time.Since(start) < time.Second {
		t.Fatalf("want a refusal after about the wait, got %v after %v", err, time.Since(start))
	}
}

func writeRecord(t *testing.T, dir string, h Holder) {
	t.Helper()
	b, _ := json.Marshal(h)
	if err := os.WriteFile(filepath.Join(dir, FileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A record left by a crashed process on this machine: the flock is free, so
// the new server starts at once.
func TestStaleLockFromThisMachineIsTakenOver(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, Holder{PID: 999999, Host: "me", Name: "hub", Started: time.Now(), Heartbeat: time.Now()})
	var logged string
	l, err := Acquire(dir, fast(Options{hostname: func() (string, error) { return "me", nil },
		Logf: func(f string, a ...any) { logged += f }}))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if !strings.Contains(logged, "did not shut down cleanly") {
		t.Errorf("no notice about the crash: %q", logged)
	}
}

// The filesystem does not carry flock between machines, but the other
// machine's heartbeat is fresh: refuse.
func TestFreshLeaseFromAnotherMachineRefuses(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, Holder{PID: 7, Host: "pod-a", Name: "hub", Started: time.Now(), Heartbeat: time.Now()})
	_, err := Acquire(dir, fast(Options{hostname: func() (string, error) { return "pod-b", nil }}))
	var held *HeldError
	if !errors.As(err, &held) || held.Why != "lease" || held.Holder.Host != "pod-a" {
		t.Fatalf("got %v", err)
	}
}

// ... and once that heartbeat is older than the lease, it is stale.
func TestExpiredLeaseFromAnotherMachineIsTakenOver(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, Holder{PID: 7, Host: "pod-a", Name: "hub", Started: time.Now().Add(-time.Hour), Heartbeat: time.Now().Add(-time.Minute)})
	var logged string
	l, err := Acquire(dir, fast(Options{hostname: func() (string, error) { return "pod-b", nil },
		Logf: func(f string, a ...any) { logged += f }}))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if !strings.Contains(logged, "stale lock") {
		t.Errorf("no notice: %q", logged)
	}
}

func TestGarbageRecordIsIgnored(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("not json"), 0o600)
	l, err := Acquire(dir, fast(Options{}))
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
}

func TestHeartbeatRefreshesTheRecord(t *testing.T) {
	dir := t.TempDir()
	l, _ := Acquire(dir, fast(Options{Name: "hub"}))
	defer l.Release()
	read := func() Holder {
		var h Holder
		b, _ := os.ReadFile(filepath.Join(dir, FileName))
		_ = json.Unmarshal(b, &h)
		return h
	}
	first := read().Heartbeat
	time.Sleep(200 * time.Millisecond)
	if !read().Heartbeat.After(first) {
		t.Error("heartbeat did not advance")
	}
	if l.Err() != nil {
		t.Errorf("Err = %v", l.Err())
	}
}

func TestBadTimings(t *testing.T) {
	if _, err := Acquire(t.TempDir(), Options{Heartbeat: time.Minute, LeaseTTL: time.Second}); err == nil {
		t.Error("heartbeat >= lease accepted")
	}
}

// A real second process: the helper holds the lock; this test process is refused.
func TestOtherProcessHoldsLock(t *testing.T) {
	if dir := os.Getenv("HUBLOCK_HELPER_DIR"); dir != "" {
		l, err := Acquire(dir, Options{Name: "child"})
		if err != nil {
			os.Exit(3)
		}
		defer l.Release()
		os.Stdout.WriteString("held\n")
		time.Sleep(time.Minute)
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOtherProcessHoldsLock$")
	cmd.Env = append(os.Environ(), "HUBLOCK_HELPER_DIR="+dir)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	buf := make([]byte, 5)
	if _, err := out.Read(buf); err != nil || string(buf) != "held\n" {
		t.Fatalf("helper did not take the lock: %q %v", buf, err)
	}
	_, err := Acquire(dir, fast(Options{}))
	var held *HeldError
	if !errors.As(err, &held) || held.Holder.PID != cmd.Process.Pid || held.Holder.Name != "child" {
		t.Fatalf("got %v", err)
	}
	// Kill it uncleanly: the kernel drops the flock; the record is stale but harmless.
	cmd.Process.Kill()
	cmd.Wait()
	l, err := Acquire(dir, fast(Options{Wait: 3 * time.Second}))
	if err != nil {
		t.Fatalf("after the holder was killed: %v", err)
	}
	l.Release()
}

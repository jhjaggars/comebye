package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errBlip = errors.New("connection reset")

// flakyBackend fails the calls a test chooses, the way a network does.
type flakyBackend struct {
	Backend
	failList func(prefix string) bool
	failGet  func(key string) bool
}

func (f *flakyBackend) List(prefix string) ([]string, error) {
	if f.failList != nil && f.failList(prefix) {
		return nil, errBlip
	}
	return f.Backend.List(prefix)
}

func (f *flakyBackend) Get(key string) ([]byte, error) {
	if f.failGet != nil && f.failGet(key) {
		return nil, errBlip
	}
	return f.Backend.Get(key)
}

func failTombs(prefix string) bool { return strings.HasPrefix(prefix, "tomb/") }

func TestFailedTombstoneListStopsTheRun(t *testing.T) {
	r := newRig(t)
	r.fixture()
	if _, err := r.a.Run(r.sources()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.a.DeleteSession("claude", "-proj", sidA); err != nil {
		t.Fatal(err)
	}
	real := r.a.B
	r.a.B = &flakyBackend{Backend: real, failList: failTombs}
	res, err := r.a.Run(r.sources())
	if !errors.Is(err, errBlip) {
		t.Fatalf("run went on without its tombstones: %+v, %v", res, err)
	}
	if keys, _ := real.List("v1/laharl/claude/"); len(keys) != 0 {
		t.Fatalf("a session deleted on request came back: %v", keys)
	}
	r.a.B = real
	if res, err := r.a.Run(r.sources()); err != nil || res.Tombstoned != 3 || res.Segments != 0 {
		t.Fatalf("after the blip: %+v, %v", res, err)
	}
}

func TestFailedTombstoneListStopsTheIndex(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	r.fixture()
	if _, err := r.a.Run(r.sources()); err != nil {
		t.Fatal(err)
	}
	ix := &Index{Path: filepath.Join(r.dir, "index.db")}
	r.a.B = &flakyBackend{Backend: r.a.B, failList: failTombs}
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{}); !errors.Is(err, errBlip) {
		t.Fatalf("Sync: %v", err)
	}
}

func TestFailedListStopsRewrap(t *testing.T) {
	for _, prefix := range []string{"notes/", "tomb/"} {
		r := newRig(t)
		r.fixture()
		if _, err := r.a.Run(r.sources()); err != nil {
			t.Fatal(err)
		}
		dst, err := NewFSBackend(filepath.Join(r.dir, "new"))
		if err != nil {
			t.Fatal(err)
		}
		r.a.B = &flakyBackend{Backend: r.a.B, failList: func(p string) bool { return strings.HasPrefix(p, prefix) }}
		if _, err := r.a.Rewrap(r.op, dst, r.a.S); !errors.Is(err, errBlip) {
			t.Fatalf("rewrap skipped %s after a failed list: %v", prefix, err)
		}
	}
}

func TestModeMarkerBlipIsNotAMissingMarker(t *testing.T) {
	b, err := NewFSBackend(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureMode(b, ModeNone); err != nil {
		t.Fatal(err)
	}
	f := &flakyBackend{Backend: b, failGet: func(string) bool { return true }}
	if _, err := ReadMode(f); !errors.Is(err, errBlip) {
		t.Fatalf("ReadMode: %v", err)
	}
	// Above all, asking for the other mode during a blip must not get past the check.
	if err := EnsureMode(f, ModeAge); !errors.Is(err, errBlip) {
		t.Fatalf("EnsureMode: %v", err)
	}
	if _, err := b.Get("nothing"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("FSBackend.Get of a missing key: %v", err)
	}
}

// raceBackend looks empty to the first Get, as if another host's Put landed
// just after it.
type raceBackend struct {
	Backend
	winner string
	seen   bool
}

func (r *raceBackend) Get(key string) ([]byte, error) {
	if !r.seen {
		r.seen = true
		return nil, os.ErrNotExist
	}
	return []byte(r.winner + "\n"), nil
}

func (r *raceBackend) List(string) ([]string, error) { return nil, nil }

func (r *raceBackend) Put(string, []byte) error { return ErrExists }

func TestEnsureModeLosingTheFirstWriteRace(t *testing.T) {
	if err := EnsureMode(&raceBackend{winner: ModeNone}, ModeNone); err != nil {
		t.Fatalf("same mode: %v", err)
	}
	if err := EnsureMode(&raceBackend{winner: ModeAge}, ModeNone); err == nil {
		t.Fatal("the other host chose another mode and this one did not notice")
	}
}

// barrierBackend holds the first segment scan until a second one arrives or
// a short wait passes, so that two unlocked writers are sure to scan together.
type barrierBackend struct {
	Backend
	calls atomic.Int32
	met   chan struct{}
}

func (b *barrierBackend) List(prefix string) ([]string, error) {
	if strings.HasPrefix(prefix, "v1/") {
		switch b.calls.Add(1) {
		case 1:
			select {
			case <-b.met:
			case <-time.After(300 * time.Millisecond):
			}
		case 2:
			close(b.met)
		}
	}
	return b.Backend.List(prefix)
}

// A run holds back an unterminated last line and a flush does not, so without
// a lock they cut different lengths from the same offset. That overlap would
// make shipFile refuse the stream for good.
func TestRunAndFlushDoNotOverlap(t *testing.T) {
	r := newPlainRig(t)
	stream := "-proj/" + sidA + ".jsonl"
	path := filepath.Join(r.claude, filepath.FromSlash(stream))
	r.write(path, claudeLine("user", "user", "first", "2026-01-01T00:00:00Z")+"\n"+"partial line, no newline", false)
	r.a.B = &barrierBackend{Backend: r.a.B, met: make(chan struct{})}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := r.a.Run(r.sources()); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := r.a.Flush(Source{Kind: "claude", Root: r.claude}, path); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()

	state, err := scan(r.a.B, r.a.Host)
	if err != nil {
		t.Fatal(err)
	}
	st := state["claude/"+stream]
	if st == nil || st.Overlap {
		t.Fatalf("overlapping segments: %+v", st)
	}
	// The stream must stay usable: more bytes can still be added.
	r.write(path, "\nmore\n", true)
	if res, err := r.a.Flush(Source{Kind: "claude", Root: r.claude}, path); err != nil || len(res.Errors) > 0 {
		t.Fatalf("stream refused after the race: %+v, %v", res, err)
	}
	want, _ := os.ReadFile(path)
	if got := restoreString(t, r, "claude", stream); got != string(want) {
		t.Fatalf("restored %q, want %q", got, want)
	}
}

func TestLockIsPerHost(t *testing.T) {
	dir := t.TempDir()
	a := &Archiver{Host: "laharl", LockDir: dir}
	unlock, err := a.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	other := &Archiver{Host: "w/1", LockDir: dir} // an odd name must not escape the directory
	done := make(chan error, 1)
	go func() {
		u, err := other.lock()
		if err == nil {
			u()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a different host's lock waited on this one")
	}
	same := &Archiver{Host: "laharl", LockDir: dir}
	got := make(chan struct{})
	go func() {
		if u, err := same.lock(); err == nil {
			u()
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("the same host's lock did not wait")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not released")
	}
}

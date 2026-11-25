package testruns

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/state"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Whole, as remote.sh writes it (a .tmp, then a rename): the watcher never sees half a record.
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

// rec is a record as tools/ci/remote.sh writes it.
func rec(id string, slot int, started, expires time.Time) string {
	return `{"v":1,"id":"` + id + `","suite":"browser","branch":"feat/x","head":"0123abcd","agent":"laharl/w1:p1","from":"laharl","slot":` +
		string(rune('0'+slot)) + `,"slots":4,"started":"` + started.UTC().Format(time.RFC3339) + `","expires":"` + expires.UTC().Format(time.RFC3339) + `"}` + "\n"
}

// List reads the records remote.sh writes, in slot order, and leaves out the
// expired ones and anything that is not a record.
func TestList(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	dir := Dir(t.TempDir())
	if got := List(dir, now); got != nil {
		t.Fatalf("no directory: %+v", got)
	}
	write(t, dir, "b.json", rec("run-b", 3, now.Add(-time.Minute), now.Add(20*time.Minute)))
	write(t, dir, "a.json", rec("run-a", 1, now.Add(-3*time.Minute), now.Add(time.Minute)))
	write(t, dir, "old.json", rec("run-old", 2, now.Add(-time.Hour), now.Add(-time.Second))) // a run that died
	write(t, dir, "x.json.tmp", rec("run-tmp", 2, now, now.Add(time.Hour)))                  // half written
	write(t, dir, "bad.json", "{not json")
	write(t, dir, "noid.json", `{"slot":1,"slots":1,"started":"2026-01-01T00:00:00Z","expires":"2099-01-01T00:00:00Z"}`)
	write(t, dir, "noexp.json", `{"id":"x","slot":1,"slots":1,"started":"2026-01-01T00:00:00Z"}`)
	write(t, dir, "big.json", `{"id":"big","pad":"`+strings.Repeat("x", maxRecord)+`"}`)
	got := List(dir, now)
	if len(got) != 2 || got[0].ID != "run-a" || got[1].ID != "run-b" {
		t.Fatalf("%+v", got)
	}
	a := got[0]
	if a.Slot != 1 || a.Slots != 4 || a.Suite != "browser" || a.Branch != "feat/x" || a.Head != "0123abcd" || a.Agent != "laharl/w1:p1" || a.From != "laharl" || !a.Started.Equal(now.Add(-3*time.Minute)) {
		t.Errorf("%+v", a)
	}
	// Past its expiry, a record is gone without anyone removing it.
	if got := List(dir, now.Add(2*time.Minute)); len(got) != 1 || got[0].ID != "run-b" {
		t.Errorf("after run-a expired: %+v", got)
	}
}

// Clip bounds the count, each field and the slot numbers.
func TestClip(t *testing.T) {
	now := time.Now()
	long := strings.Repeat("é", MaxText) // 2 bytes each: a cut mid-rune must not leave bad UTF-8
	var in []state.TestRun
	for i := 0; i < MaxRuns+10; i++ {
		in = append(in, state.TestRun{ID: long, Suite: long, Branch: long, Head: long, Agent: long, From: long, Slot: 99 - i, Slots: 1000, Started: now, Expires: now.Add(time.Minute)})
	}
	in = append(in, state.TestRun{ID: "zero-slot", Slot: -3, Slots: 0, Started: now, Expires: now.Add(time.Minute)})
	got := Clip(in, now)
	if len(got) != MaxRuns {
		t.Fatalf("%d runs", len(got))
	}
	if got[0].ID != "zero-slot" || got[0].Slot != 1 || got[0].Slots != 1 {
		t.Errorf("slot not made sane: %+v", got[0])
	}
	for _, r := range got[1:] {
		for _, s := range []string{r.ID, r.Suite, r.Branch, r.Head, r.Agent, r.From} {
			if len(s) > MaxText || !json.Valid([]byte(`"`+s+`"`)) || strings.ContainsRune(s, '�') {
				t.Fatalf("field not clipped cleanly: %d bytes", len(s))
			}
		}
		if r.Slots != MaxSlots || r.Slot > r.Slots {
			t.Fatalf("slots %d/%d", r.Slot, r.Slots)
		}
	}
	if Clip(nil, now) != nil {
		t.Error("nil in, nil out")
	}
}

// Watch says when a run starts, ends or expires, and not otherwise.
func TestWatch(t *testing.T) {
	dir := Dir(t.TempDir())
	var n atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, dir, 10*time.Millisecond, func() { n.Add(1) })
	wait := func(want int32, what string) {
		t.Helper()
		for i := 0; i < 300 && n.Load() < want; i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if got := n.Load(); got != want {
			t.Fatalf("%s: %d changes, want %d", what, got, want)
		}
	}
	time.Sleep(50 * time.Millisecond)
	wait(0, "nothing there")
	now := time.Now()
	write(t, dir, "a.json", rec("run-a", 1, now, now.Add(time.Hour)))
	wait(1, "a run started")
	write(t, dir, "a.json", rec("run-a", 1, now, now.Add(2*time.Hour))) // renewed: the same run
	time.Sleep(60 * time.Millisecond)
	wait(1, "a run renewed")
	write(t, dir, "b.json", rec("run-b", 2, now, now.Add(time.Second)))
	wait(2, "a second run")
	wait(3, "the second expired")
	os.Remove(filepath.Join(dir, "a.json"))
	wait(4, "the first ended")
}

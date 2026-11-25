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

// done is a done record as tools/ci/remote.sh writes it (the VM's, then the
// caller's temperatures added).
func done(id string, ended time.Time, rc int, temps string) string {
	return `{"v":1,"id":"` + id + `","suite":"browser","branch":"feat/x","head":"0123abcd","agent":"laharl/w1:p1","from":"laharl","slot":2,"slots":4,"started":"` +
		ended.Add(-90*time.Second).UTC().Format(time.RFC3339) + `","ended":"` + ended.UTC().Format(time.RFC3339) + `","secs":90,"rc":` + string(rune('0'+rc)) + temps + `}` + "\n"
}

// History reads the done records, newest first, at most MaxHistory, each
// saying whether its log is kept; what is not a finished record is left out.
func TestHistory(t *testing.T) {
	root := t.TempDir()
	dir := DoneDir(root)
	if got := History(dir); got != nil {
		t.Fatalf("no directory: %+v", got)
	}
	now := time.Now().Truncate(time.Second)
	for i := range 50 { // the VM keeps 50
		id := "laharl-20261124-1000" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		write(t, dir, id+".json", done(id, now.Add(time.Duration(i-60)*time.Minute), i%2, ""))
	}
	write(t, dir, "newest.json", done("newest", now, 1, `,"cpu_start":61,"nvme_start":60,"cpu_end":88,"nvme_end":971`))
	write(t, dir, "newest.log", "the suite's output\n")
	write(t, dir, "other.log", "a log with no record\n")
	write(t, dir, "live.json", rec("live", 1, now, now.Add(time.Hour))) // not finished: no end, no rc
	write(t, dir, "norc.json", `{"id":"norc","started":"2026-11-24T10:00:00Z","ended":"2026-11-24T10:01:00Z"}`)
	write(t, dir, "x.json.tmp", done("tmp", now.Add(time.Hour), 0, ""))
	// The records live beside the live ones, which List never mistakes them for.
	if got := List(Dir(root), now); len(got) != 0 {
		t.Fatalf("List saw the done dir: %+v", got)
	}
	got := History(dir)
	if len(got) != MaxHistory {
		t.Fatalf("%d runs", len(got))
	}
	n := got[0]
	if n.ID != "newest" || *n.RC != 1 || n.Secs != 90 || !n.Log || n.Slot != 2 || n.Slots != 4 || n.Agent != "laharl/w1:p1" || !n.Ended.Equal(now) {
		t.Errorf("newest: %+v", n)
	}
	if n.CPUStart == nil || *n.CPUStart != 61 || *n.NVMeStart != 60 || *n.CPUEnd != 88 || n.NVMeEnd != nil {
		t.Errorf("temperatures (971 °C is no reading): %v %v %v %v", n.CPUStart, n.NVMeStart, n.CPUEnd, n.NVMeEnd)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Log || got[i].RC == nil || !got[i].Ended.Before(got[i-1].Ended) || !got[i].Expires.IsZero() {
			t.Fatalf("run %d: %+v", i, got[i])
		}
	}
	if got[1].ID != "laharl-20261124-1000bx" {
		t.Errorf("second newest: %s", got[1].ID)
	}
}

// ClipHistory bounds what a peer reports: the text, the numbers, and the count.
func TestClipHistory(t *testing.T) {
	now := time.Now()
	rc, bad, hot := 300, -4, 5000
	var in []state.TestRun
	for i := range MaxHistory + 7 {
		in = append(in, state.TestRun{ID: "r", Suite: strings.Repeat("s", MaxText+9), Slot: 9, Slots: 4, Secs: -3, RC: &rc, CPUEnd: &hot,
			Started: now.Add(-time.Hour), Ended: now.Add(time.Duration(i) * time.Second), Expires: now})
	}
	in = append(in, state.TestRun{ID: "nort", Started: now, Ended: now}, state.TestRun{ID: "noend", Started: now, RC: &bad})
	got := ClipHistory(in)
	if len(got) != MaxHistory {
		t.Fatalf("%d", len(got))
	}
	g := got[0]
	if len(g.Suite) != MaxText || g.Slot != 4 || g.Secs != 0 || *g.RC != 255 || g.CPUEnd != nil || !g.Expires.IsZero() || !g.Ended.Equal(now.Add(time.Duration(MaxHistory+6)*time.Second)) {
		t.Errorf("%+v", g)
	}
	if &rc == g.RC {
		t.Error("the clip shares the peer's rc")
	}
}

// Log serves a run's kept output: at most MaxLog bytes (the end, from a whole
// line), only for an id remote.sh could have made, and only a regular file.
func TestLog(t *testing.T) {
	dir := DoneDir(t.TempDir())
	write(t, dir, "run-1.log", "line one\nline two\n")
	if b, err := Log(dir, "run-1"); err != nil || string(b) != "line one\nline two\n" {
		t.Fatalf("%q %v", b, err)
	}
	big := strings.Repeat("0123456789abcde\n", MaxLog/16) + "the end\n"
	write(t, dir, "run-2.log", big)
	b, err := Log(dir, "run-2")
	if err != nil || len(b) > MaxLog || !strings.HasSuffix(string(b), "the end\n") || !strings.HasPrefix(string(b), "0123456789abcde\n") {
		t.Fatalf("%d bytes, %v, starts %q", len(b), err, b[:min(20, len(b))])
	}
	if err := os.Symlink(filepath.Join(dir, "run-1.log"), filepath.Join(dir, "run-3.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "run-4.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../run-1", "run-1/../run-1", "RUN-1", "-x", "run-9", "run-4", strings.Repeat("a", 129), "run-1.log"} {
		if _, err := Log(dir, id); err != ErrNoLog {
			t.Errorf("%q: %v", id, err)
		}
	}
	// A symlink to a log is followed (os.Open), and its target is still a log in
	// the same place; the id itself can never leave the directory.
	if b, err := Log(dir, "run-3"); err != nil || string(b) != "line one\nline two\n" {
		t.Errorf("symlink: %q %v", b, err)
	}
}

// Watch notices a run finishing into the done records, and its temperatures
// and log arriving afterwards (the caller's cleanup adds them).
func TestWatchHistory(t *testing.T) {
	root := t.TempDir()
	dir, dd := Dir(root), DoneDir(root)
	var n atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, dir, 20*time.Millisecond, func() { n.Add(1) })
	wait := func(want int32) {
		t.Helper()
		for i := 0; i < 200 && n.Load() < want; i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if n.Load() < want {
			t.Fatalf("changes %d, want %d", n.Load(), want)
		}
	}
	time.Sleep(50 * time.Millisecond)
	now := time.Now()
	write(t, dd, "r1.json", done("r1", now, 0, ""))
	wait(1)
	write(t, dd, "r1.json", done("r1", now, 0, `,"cpu_start":60`))
	wait(2)
	write(t, dd, "r1.log", "out\n")
	wait(3)
}

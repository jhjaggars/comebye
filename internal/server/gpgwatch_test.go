package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/gpgunlock"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
)

// watchRig is a gpgRig whose setup is done (the passphrase blob stored) and
// whose key is locked, on a fake clock, with no old unit installed.
func newWatchRig(t *testing.T) (*gpgRig, *atomic.Int64) {
	t.Helper()
	r := newGPGRig(t)
	r.s.cfg.GPGUnitDirs = []string{t.TempDir()}
	_, ap, req := r.file(t, true)
	if code, body := r.answer(t, ap.Approval.ID, req, r.prf, r.key.Pass, true); code != 200 {
		t.Fatalf("setup = %d %s", code, body)
	}
	if err := r.host.Lock(); err != nil {
		t.Fatal(err)
	}
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	r.host.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	return r, clock
}

func (r *gpgRig) startWatch(t *testing.T) {
	t.Helper()
	r.s.cfg.GPGWatch = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.s.RunGPGWatch(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool {
		r.s.gpgMu.Lock()
		defer r.s.gpgMu.Unlock()
		return r.s.gpgWake != nil
	})
}

func (r *gpgRig) agents(status herdr.AgentStatus) {
	r.s.fleet.Hub().Broadcast(state.Event{Type: "agents", Agents: []herdr.AgentInfo{{Host: "desk", PaneID: "w2:p1", ID: "desk/w2:p1", Status: status}}})
}

func (r *gpgRig) lockedItems() []Todo {
	var out []Todo
	for _, it := range r.s.todos.list("") {
		if it.Question == "GPG is locked on desk" && it.State == TodoOpen {
			out = append(out, it)
		}
	}
	return out
}

func (r *gpgRig) keyLocked(t *testing.T) bool {
	t.Helper()
	cfg, _ := r.host.Config()
	l, err := r.key.GPG.Locked(cfg.Keygrips)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The "GPG is locked" item follows this host's agents through the server's
// own events: filed once when one becomes active while the key is locked,
// never for an idle host, and withdrawn when an event finds the key unlocked
// another way (pinentry).
func TestGPGWatchFilesAndWithdrawsOnAgentEvents(t *testing.T) {
	r, _ := newWatchRig(t)
	r.startWatch(t)
	r.agents(herdr.StatusIdle)
	time.Sleep(200 * time.Millisecond)
	if n := len(r.lockedItems()); n != 0 {
		t.Fatalf("an idle host got %d item(s)", n)
	}
	r.agents(herdr.StatusWorking)
	waitFor(t, func() bool { return len(r.lockedItems()) == 1 })
	last, ok := r.host.LastFiled()
	if !ok || last.Setup {
		t.Fatalf("record = %+v %v", last, ok)
	}
	// More events while it stands: still one item, the same request.
	r.agents(herdr.StatusBlocked)
	r.agents(herdr.StatusWorking)
	time.Sleep(200 * time.Millisecond)
	if items := r.lockedItems(); len(items) != 1 || items[0].Approval == nil || items[0].Approval.ID != last.Approval {
		t.Fatalf("items = %+v", items)
	}
	// The passphrase typed in a pinentry: the next event withdraws the item.
	cfg, _ := r.host.Config()
	for _, g := range cfg.Keygrips {
		if err := r.key.GPG.Preset(g, []byte(r.key.Pass)); err != nil {
			t.Fatal(err)
		}
	}
	r.agents(herdr.StatusIdle)
	waitFor(t, func() bool { return len(r.lockedItems()) == 0 })
	if _, ok := r.host.LastFiled(); ok {
		t.Fatal("the record outlived the withdrawal")
	}
	ap := r.s.approvals.find(last.Approval)
	if ap == nil || ap.State != apExpired || ap.Note != "withdrawn by the host" {
		t.Fatalf("approval = %+v", ap)
	}
}

// The passkey answer closes the watcher's item and clears its record, and
// the watcher files nothing more while the key is unlocked.
func TestGPGWatchStopsAtTheAnswer(t *testing.T) {
	r, _ := newWatchRig(t)
	r.startWatch(t)
	r.agents(herdr.StatusWorking)
	waitFor(t, func() bool { return len(r.lockedItems()) == 1 })
	last, _ := r.host.LastFiled()
	ap := r.s.approvals.find(last.Approval)
	if ap == nil {
		t.Fatal("no approval for the record")
	}
	code, body := r.answer(t, ap.ID, ap.Request, r.prf, "", false)
	if code != 200 {
		t.Fatalf("answer = %d %s", code, body)
	}
	if r.keyLocked(t) {
		t.Fatal("locked after the answer")
	}
	r.agents(herdr.StatusBlocked)
	r.agents(herdr.StatusWorking)
	time.Sleep(200 * time.Millisecond)
	if n := len(r.lockedItems()); n != 0 {
		t.Fatalf("%d item(s) while unlocked", n)
	}
}

// An unlock ends at 8 hours: the answer sets the end, the watcher keeps the
// key unlocked up to it and forgets it once it passed, with no help.
func TestGPGWatchEndsTheUnlockAt8h(t *testing.T) {
	r, clock := newWatchRig(t)
	r.startWatch(t)
	start := time.Unix(0, clock.Load())
	_, un, req := r.file(t, false)
	if code, body := r.answer(t, un.Approval.ID, req, r.prf, "", false); code != 200 {
		t.Fatalf("unlock = %d %s", code, body)
	}
	st, ok := r.host.Unlocked()
	if !ok || !st.ExpiresAt.Equal(start.Add(8*time.Hour)) {
		t.Fatalf("unlock ends %v, want %v", st.ExpiresAt, start.Add(8*time.Hour))
	}
	clock.Store(start.Add(8*time.Hour - time.Minute).UnixNano())
	r.s.gpgPoke()
	time.Sleep(200 * time.Millisecond)
	if r.keyLocked(t) {
		t.Fatal("locked before its 8 hours")
	}
	clock.Store(start.Add(8*time.Hour + time.Second).UnixNano())
	r.s.gpgPoke()
	waitFor(t, func() bool { return r.keyLocked(t) })
	if err := r.key.GPG.TestSign(r.key.FPR); err == nil {
		t.Fatal("signed after the 8 hours")
	}
	if _, ok := r.host.Unlocked(); ok {
		t.Fatal("the state still says unlocked")
	}
}

// The real clock with a shortened lifetime: the watcher's own timer ends
// the unlock, no event needed.
func TestGPGWatchTimerEndsAShortUnlock(t *testing.T) {
	r, _ := newWatchRig(t)
	r.host.Now = nil
	r.host.Lifetime = 2 * time.Second
	r.startWatch(t)
	_, un, req := r.file(t, false)
	if code, body := r.answer(t, un.Approval.ID, req, r.prf, "", false); code != 200 {
		t.Fatalf("unlock = %d %s", code, body)
	}
	if r.keyLocked(t) {
		t.Fatal("locked right after the unlock")
	}
	waitFor(t, func() bool { return r.keyLocked(t) })
}

// Off (the default), no loop runs: RunGPGWatch returns at once, nothing can
// wake a watcher, and an active agent on a locked host files nothing.
func TestGPGWatchOffRunsNothing(t *testing.T) {
	r, _ := newWatchRig(t)
	done := make(chan struct{})
	go func() { r.s.RunGPGWatch(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunGPGWatch kept running with -gpg-watch off")
	}
	if r.s.gpgWake != nil {
		t.Fatal("a wake channel exists with the watcher off")
	}
	r.agents(herdr.StatusWorking)
	r.s.gpgPoke()
	time.Sleep(200 * time.Millisecond)
	if n := len(r.lockedItems()); n != 0 {
		t.Fatalf("%d item(s) filed with the watcher off", n)
	}
	if _, ok := r.host.LastFiled(); ok {
		t.Fatal("a request was filed with the watcher off")
	}
}

func TestGPGWatchOldUnitWarning(t *testing.T) {
	s, _, _ := todoServer(t)
	dir := t.TempDir()
	s.cfg.GPGUnitDirs = []string{filepath.Join(dir, "none"), dir}
	if msg := s.gpgOldUnitWarning(); msg != "" {
		t.Fatalf("no unit, yet: %q", msg)
	}
	unit := filepath.Join(dir, gpgOldUnit)
	if err := os.WriteFile(unit, []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg := s.gpgOldUnitWarning()
	for _, want := range []string{unit, "systemctl --user disable --now comebye-gpg-watch.service", "-gpg-watch"} {
		if !strings.Contains(msg, want) {
			t.Errorf("warning %q lacks %q", msg, want)
		}
	}
	s.cfg.GPGWatch = true
	if msg := s.gpgOldUnitWarning(); msg == "" || strings.Contains(msg, "first turn") {
		t.Errorf("with the watcher on: %q", msg)
	}
	// A unit enabled through a symlink only (the target gone) counts too.
	_ = os.Remove(unit)
	if err := os.Symlink(filepath.Join(dir, "gone"), unit); err != nil {
		t.Fatal(err)
	}
	if s.gpgOldUnitWarning() == "" {
		t.Error("a dangling unit symlink was missed")
	}
}

// The floor: gpg-agent's max-cache-ttl. Setup's 8 hours passes; a longer
// one is a warning, since an unlock would outlive comebye being down.
func TestGPGWatchFloorWarning(t *testing.T) {
	r := newGPGRig(t)
	if msg := gpgFloorWarning(r.host); msg != "" {
		t.Fatalf("setup's config: %q", msg)
	}
	conf := filepath.Join(r.key.Home, "gpg-agent.conf")
	if err := os.WriteFile(conf, []byte("allow-preset-passphrase\nmax-cache-ttl 86400\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg := gpgFloorWarning(r.host); !strings.Contains(msg, "max-cache-ttl is 24h0m0s") || !strings.Contains(msg, "max-cache-ttl 28800") {
		t.Fatalf("a 24 h cap: %q", msg)
	}
}

// -gpg-watch's lifetime override only ever shortens the 8 hours.
func TestGPGLifetimeOnlyShortens(t *testing.T) {
	for _, c := range []struct {
		set, want time.Duration
	}{{0, 0}, {30 * time.Second, 30 * time.Second}, {9 * time.Hour, 0}, {gpgunlock.DefaultLifetime, 0}} {
		s, _, _ := todoServer(t)
		s.cfg.GPGLifetime = c.set
		if got := s.gpgHost().Lifetime; got != c.want {
			t.Errorf("GPGLifetime %v: host lifetime %v, want %v", c.set, got, c.want)
		}
	}
}

// A worker's `comebye gpg unlock` (POST /api/approvals, here or forwarded by
// a peer) wakes the watcher, a moment later, so it sees the request the
// worker recorded and keeps that item.
func TestGPGWatchWakesAfterAWorkersRequest(t *testing.T) {
	r, _ := newWatchRig(t)
	old := gpgNotedAfter
	gpgNotedAfter = 10 * time.Millisecond
	t.Cleanup(func() { gpgNotedAfter = old })
	wake := make(chan struct{}, 1)
	r.s.gpgMu.Lock()
	r.s.gpgWake = wake
	r.s.gpgMu.Unlock()
	r.file(t, false)
	select {
	case <-wake:
	case <-time.After(3 * time.Second):
		t.Fatal("no wake after a worker's request")
	}
}

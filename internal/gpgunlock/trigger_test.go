package gpgunlock

import (
	"context"
	"errors"
	"testing"
	"time"
)

// triggerRig is a set-up host (blob stored) whose key is locked, with a
// watcher whose hub is a counter. Real gpg, in a throwaway GNUPGHOME.
type triggerRig struct {
	*rig
	w         *Watcher
	filed     int
	withdrawn []string
	active    bool
	wdErr     error
	k         []byte
}

func newTriggerRig(t *testing.T, life time.Duration) *triggerRig {
	t.Helper()
	r := newRig(t, life)
	r.setup(t)
	k := newK(t)
	if _, err := r.doSetup(t, k, r.pass); err != nil {
		t.Fatal(err)
	}
	if err := r.h.Lock(); err != nil {
		t.Fatal(err)
	}
	if !r.locked(t) {
		t.Fatal("still unlocked after lock")
	}
	tr := &triggerRig{rig: r, k: k}
	tr.w = &Watcher{
		Host: r.h,
		File: func() (string, string, error) {
			tr.filed++
			id, _, err := r.h.NewRequest(testRP, false)
			return "ap_" + id, id, err
		},
		Withdraw: func(ap string) error {
			if tr.wdErr != nil {
				return tr.wdErr
			}
			tr.withdrawn = append(tr.withdrawn, ap)
			return nil
		},
		Active: func() (bool, error) { return tr.active, nil },
	}
	return tr
}

func (tr *triggerRig) check(t *testing.T) string {
	t.Helper()
	got, err := tr.w.Check()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWatchIdleHostFilesNothing(t *testing.T) {
	tr := newTriggerRig(t, 8*time.Hour)
	for i := 0; i < 3; i++ {
		if got := tr.check(t); got != "" {
			t.Fatalf("idle host: %q", got)
		}
	}
	if tr.filed != 0 {
		t.Fatalf("filed %d for an idle host", tr.filed)
	}
}

func TestWatchLockedWithActiveAgentFilesExactlyOne(t *testing.T) {
	tr := newTriggerRig(t, 8*time.Hour)
	tr.active = true
	if got := tr.check(t); got != "filed" {
		t.Fatalf("first check: %q", got)
	}
	for i := 0; i < 5; i++ { // the request still waits: no second item
		tr.now.Add(10 * time.Second)
		if got := tr.check(t); got != "" {
			t.Fatalf("check %d: %q", i, got)
		}
	}
	// A worker's `gpg unlock` sees the same waiting request and files nothing.
	if _, ok := tr.h.Waiting(); !ok {
		t.Fatal("a worker would not see the waiting request")
	}
	// The request lapsed unanswered: not asked again at once ...
	tr.now.Add(RequestTTL)
	if got := tr.check(t); got != "" {
		t.Fatalf("asked again inside the cooldown: %q", got)
	}
	// ... but after the cooldown, while an agent is active.
	tr.now.Add(Cooldown)
	if got := tr.check(t); got != "filed" {
		t.Fatalf("after cooldown: %q", got)
	}
	if tr.filed != 2 {
		t.Fatalf("filed %d, want 2", tr.filed)
	}
	// The agent went away: nothing more.
	tr.active = false
	tr.now.Add(RequestTTL + Cooldown)
	if got := tr.check(t); got != "" || tr.filed != 2 {
		t.Fatalf("idle again: %q, filed %d", got, tr.filed)
	}
}

func TestWatchWithdrawsWhenUnlockedByPinentry(t *testing.T) {
	tr := newTriggerRig(t, 8*time.Hour)
	tr.active = true
	tr.check(t)
	last, _ := tr.h.LastFiled()
	// Pinentry caches the passphrase: here, the agent is told directly.
	c, _ := tr.h.Config()
	for _, g := range c.Keygrips {
		if err := tr.g.Preset(g, []byte(tr.pass)); err != nil {
			t.Fatal(err)
		}
	}
	if tr.locked(t) {
		t.Fatal("still locked")
	}
	tr.wdErr = errors.New("hub down")
	if _, err := tr.w.Check(); err == nil {
		t.Fatal("a failed withdraw was swallowed")
	}
	if _, ok := tr.h.LastFiled(); !ok {
		t.Fatal("the record was dropped before the item was withdrawn")
	}
	tr.wdErr = nil
	if got := tr.check(t); got != "withdrawn" || len(tr.withdrawn) != 1 || tr.withdrawn[0] != last.Approval {
		t.Fatalf("got %q, withdrawn %v", got, tr.withdrawn)
	}
	if got := tr.check(t); got != "" || len(tr.withdrawn) != 1 {
		t.Fatalf("withdrew twice: %q %v", got, tr.withdrawn)
	}
}

func TestWatchPasskeyUnlockClearsTheRecord(t *testing.T) {
	tr := newTriggerRig(t, 8*time.Hour)
	tr.active = true
	tr.check(t)
	id, req, err := tr.h.NewRequest(testRP, false)
	if err != nil {
		t.Fatal(err)
	}
	// The hub closes the item itself on a good answer; the host forgets its record.
	if _, err := tr.h.Complete(id, tr.answer(t, id, req, "", tr.k, false)); err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.h.LastFiled(); ok {
		t.Fatal("the record outlived the unlock")
	}
	if got := tr.check(t); got != "" || len(tr.withdrawn) != 0 {
		t.Fatalf("withdrew after a passkey unlock: %q %v", got, tr.withdrawn)
	}
}

func TestWatchRunEnforcesTheLifetime(t *testing.T) {
	tr := newTriggerRig(t, 2*time.Second)
	tr.h.Now = nil
	tr.w.Every = 100 * time.Millisecond
	// Unlock for the (shortened) lifetime, as a passkey answer does.
	if _, err := tr.h.armLocked(mustGrips(t, tr.h)); err != nil {
		t.Fatal(err)
	}
	c, _ := tr.h.Config()
	for _, g := range c.Keygrips {
		if err := tr.g.Preset(g, []byte(tr.pass)); err != nil {
			t.Fatal(err)
		}
	}
	if tr.locked(t) {
		t.Fatal("locked right after unlock")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.w.Run(ctx, nil)
	waitFor(t, func() bool { return tr.locked(t) })
	if err := tr.g.TestSign(tr.fpr); err == nil {
		t.Fatal("signed after the lifetime")
	}
}

func mustGrips(t *testing.T, h *Host) []string {
	t.Helper()
	c, err := h.Config()
	if err != nil {
		t.Fatal(err)
	}
	return c.Keygrips
}

// A request the host has taken an answer for is spent, so a new ask is not
// held back by it (a refused answer leaves the item open for a new request).
func TestWaitingEndsWhenTheRequestIsTaken(t *testing.T) {
	tr := newTriggerRig(t, 8*time.Hour)
	tr.active = true
	tr.check(t)
	last, _ := tr.h.LastFiled()
	if _, ok := tr.h.Waiting(); !ok {
		t.Fatal("not waiting right after filing")
	}
	if _, err := tr.h.takePending(last.Request); err != nil {
		t.Fatal(err)
	}
	if _, ok := tr.h.Waiting(); ok {
		t.Fatal("still waiting on a spent request")
	}
}

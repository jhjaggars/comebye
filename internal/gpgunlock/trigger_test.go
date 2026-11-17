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
	fileErr   error
	start     time.Time
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
			if tr.fileErr != nil {
				return "", "", tr.fileErr
			}
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
	tr.start = r.h.now()
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
		tr.now.Add(5 * time.Second)
		if got := tr.check(t); got != "" {
			t.Fatalf("check %d: %q", i, got)
		}
	}
	// A worker's `gpg unlock` sees the same waiting request and files nothing.
	if _, ok := tr.h.Waiting(); !ok {
		t.Fatal("a worker would not see the waiting request")
	}
}

// The item stays across many rounds, hours of them: each time the request
// runs low it is renewed (File again, which the hub turns into the same item),
// and nothing is withdrawn while an agent is active.
func TestWatchKeepsTheItemAcrossManyRounds(t *testing.T) {
	tr := newTriggerRig(t, 8*time.Hour)
	tr.active = true
	first := tr.check(t)
	if first != "filed" {
		t.Fatalf("first: %q", first)
	}
	renewals := 0
	for i := 0; i < 4*60*2; i++ { // 4 hours of 30 s rounds
		tr.now.Add(WatchEvery)
		switch got := tr.check(t); got {
		case "refreshed":
			renewals++
		case "":
		default:
			t.Fatalf("round %d: %q", i, got)
		}
		// At every round a request the user could answer is there.
		if _, ok := tr.h.Waiting(); !ok {
			t.Fatalf("round %d: no live request", i)
		}
	}
	if len(tr.withdrawn) != 0 {
		t.Fatalf("withdrawn: %v", tr.withdrawn)
	}
	if tr.filed != 1+renewals || renewals < 100 {
		t.Fatalf("filed %d, renewals %d", tr.filed, renewals)
	}
}

// A renewal is a fresh request (a new challenge) for the item that stands, and
// the old request stays answerable until its own time.
func TestWatchRefreshesTheChallengeBeforeItRunsOut(t *testing.T) {
	tr := newTriggerRig(t, 8*time.Hour)
	tr.active = true
	tr.check(t)
	a, _ := tr.h.LastFiled()
	tr.now.Add(RequestTTL - Refresh - time.Second)
	if got := tr.check(t); got != "" {
		t.Fatalf("renewed too early: %q", got)
	}
	tr.now.Add(2 * time.Second)
	if got := tr.check(t); got != "refreshed" {
		t.Fatalf("not renewed with %s left: %q", Refresh, got)
	}
	b, _ := tr.h.LastFiled()
	if b.Request == a.Request || b.Approval == a.Approval {
		t.Fatalf("same request after a renewal: %+v %+v", a, b)
	}
	if !b.Exp.After(a.Exp) {
		t.Fatalf("expiry did not move: %v %v", a.Exp, b.Exp)
	}
	// The request the user may be answering (the old one) is still on the host.
	if _, err := tr.h.takePending(a.Request); err != nil {
		t.Fatalf("the old request was dropped by the renewal: %v", err)
	}
}

// With no agent active the item stays for the grace (renewed, so a tap still
// works), then is withdrawn, and nothing is filed again for an idle host.
func TestWatchWithdrawsAfterTheGraceWithNoActiveAgent(t *testing.T) {
	tr := newTriggerRig(t, 8*time.Hour)
	tr.active = true
	tr.check(t)
	last, _ := tr.h.LastFiled()
	tr.active = false
	for tr.now.Now().Sub(tr.start) < Grace-time.Minute {
		tr.now.Add(WatchEvery)
		if got := tr.check(t); got != "" && got != "refreshed" {
			t.Fatalf("inside the grace: %q", got)
		}
	}
	if len(tr.withdrawn) != 0 {
		t.Fatal("withdrawn inside the grace")
	}
	if _, ok := tr.h.Waiting(); !ok {
		t.Fatal("no live request inside the grace")
	}
	tr.now.Add(2 * time.Minute)
	got := tr.check(t)
	if got != "withdrawn" || len(tr.withdrawn) != 1 {
		t.Fatalf("after the grace: %q %v", got, tr.withdrawn)
	}
	if tr.withdrawn[0] == "" || last.Approval == "" {
		t.Fatal("no approval id")
	}
	filed := tr.filed
	for i := 0; i < 5; i++ {
		tr.now.Add(WatchEvery)
		if got := tr.check(t); got != "" {
			t.Fatalf("idle host after the grace: %q", got)
		}
	}
	if tr.filed != filed {
		t.Fatal("filed for an idle host")
	}
	// An agent back: one new item.
	tr.active = true
	if got := tr.check(t); got != "filed" {
		t.Fatalf("agent back: %q", got)
	}
}

// A brief idle moment does not start the grace over: the agent being seen
// again resets it.
func TestWatchBriefIdleKeepsTheItem(t *testing.T) {
	tr := newTriggerRig(t, 8*time.Hour)
	tr.active = true
	tr.check(t)
	for i := 0; i < 10; i++ {
		tr.active = false
		tr.now.Add(20 * time.Minute)
		if got := tr.check(t); got == "withdrawn" {
			t.Fatalf("withdrawn after a brief idle (round %d)", i)
		}
		tr.active = true
		tr.now.Add(WatchEvery)
		tr.check(t)
	}
}

// "Not now": the hub answers snoozed, the watcher files nothing and stays
// quiet until the time given.
func TestWatchSnoozedByNotNow(t *testing.T) {
	tr := newTriggerRig(t, 8*time.Hour)
	tr.active = true
	until := tr.now.Now().Add(10 * time.Minute)
	tr.fileErr = &SnoozedError{Until: until}
	if got := tr.check(t); got != "" {
		t.Fatalf("snoozed: %q", got)
	}
	n := tr.filed
	tr.fileErr = nil
	tr.now.Add(5 * time.Minute)
	if got := tr.check(t); got != "" || tr.filed != n {
		t.Fatalf("inside the snooze: %q, filed %d", got, tr.filed)
	}
	tr.now.Add(6 * time.Minute)
	if got := tr.check(t); got != "filed" {
		t.Fatalf("after the snooze: %q", got)
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

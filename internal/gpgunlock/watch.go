package gpgunlock

import (
	"context"
	"time"
)

// RetryEvery is how soon a round that failed (the hub did not answer, gpg
// errored) is tried again.
const RetryEvery = WatchEvery

// Run enforces the lifetime and keeps the one item until ctx ends. The
// comebye server runs it (-gpg-watch); nothing polls. It does a round at
// start (a restart after the expiry forgets at once), at each send on wake
// (the server sends when an agent of this host starts or stops being active,
// and when an unlock or a request lands), and on its own only when a round
// has something due: the unlock's expiry, the standing item's renewal or
// grace, the end of a "Not now", or a retry after an error. With nothing due
// it sleeps until the next wake. Errors go to log.
func (w *Watcher) Run(ctx context.Context, wake <-chan struct{}, log func(error)) {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		failed := false
		if _, err := w.Host.Sweep(); err != nil {
			failed = true
			if log != nil {
				log(err)
			}
		}
		if _, err := w.Check(); err != nil {
			failed = true
			if log != nil {
				log(err)
			}
		}
		now := w.Host.now()
		at, due := w.Next(now)
		if failed && (!due || at.After(now.Add(RetryEvery))) {
			at, due = now.Add(RetryEvery), true
		}
		var timer <-chan time.Time
		if due {
			d := at.Sub(now)
			if d < 50*time.Millisecond {
				d = 50 * time.Millisecond
			}
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(d)
			timer = t.C
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-timer:
		}
	}
}

// Next is when a round next has something to do with no event: the unlock's
// expiry; for a standing item, its renewal (Refresh before its request runs
// out, which also notices an unlock made by pinentry) and the end of its
// grace; and the end of a "Not now". false: nothing is due, only an event
// wakes the watcher.
func (w *Watcher) Next(now time.Time) (time.Time, bool) {
	var at time.Time
	due := false
	add := func(t time.Time) {
		if !due || t.Before(at) {
			at, due = t, true
		}
	}
	if exp, ok := w.Host.NextExpiry(); ok {
		add(exp)
	}
	if last, ok := w.Host.LastFiled(); ok && !last.Setup && w.Host.HasBlob() {
		margin := w.Refresh
		if margin <= 0 {
			margin = Refresh
		}
		add(last.Exp.Add(-margin))
		grace := w.Grace
		if grace <= 0 {
			grace = Grace
		}
		if !last.ActiveAt.IsZero() {
			add(last.ActiveAt.Add(grace))
		}
	}
	if w.snoozeUntil.After(now) {
		add(w.snoozeUntil)
	}
	return at, due
}

package gpgunlock

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The trigger side of the unlock (slice 4): who notices a locked key and asks
// for Face ID, with one item per host. Two things ask: the server's watcher
// (-gpg-watch, Watcher.Run), when an agent is active on the host and the key
// is locked, and a worker's
// `comebye gpg unlock` after a signed commit failed. Both go through the
// record below, so a request that still waits is never filed twice, and the
// watcher withdraws the item when the key is unlocked by any means (pinentry
// included).

// WatchEvery was how often the old `comebye gpg watch` daemon looked; the
// server's watcher is driven by events and uses it only as the retry after
// a failed round (RetryEvery).
//
// The item stays: the request behind it lives RequestTTL, but the watcher
// renews it (Refresh before it runs out) on the same to-do item, so the user
// finds it whenever they look. It is withdrawn when the key is unlocked, or
// when no agent has been active on the host for Grace.
const (
	WatchEvery = 30 * time.Second
	Grace      = 30 * time.Minute // how long an idle host keeps the item
	Refresh    = 45 * time.Second // renew the request when this little of it is left
)

// SnoozedError is what File returns when the hub says the user answered "Not
// now": nothing was filed, and the watcher stays quiet until Until.
type SnoozedError struct{ Until time.Time }

func (e *SnoozedError) Error() string {
	return "the user said not now, until " + e.Until.Format(time.RFC3339)
}

const fileFiled = "gpg-filed.json"

// Filed is the request this host last filed with the hub.
type Filed struct {
	Approval string    `json:"approval"`
	Request  string    `json:"request"`
	Setup    bool      `json:"setup,omitempty"`
	At       time.Time `json:"at"`
	Exp      time.Time `json:"exp"`
	// ActiveAt is when an agent was last seen active on this host while the
	// item stood: the grace runs from it.
	ActiveAt time.Time `json:"active_at,omitempty"`
}

// RecordFiled remembers a request just filed. The expiry is the request's.
func (h *Host) RecordFiled(approval, request string, setup bool) error {
	ttl := RequestTTL
	if setup {
		ttl = SetupTTL
	}
	now := h.now()
	active := now
	if prev, ok := h.LastFiled(); ok && !prev.Setup && !prev.ActiveAt.IsZero() && now.Sub(prev.ActiveAt) < Grace {
		active = prev.ActiveAt // a renewal does not extend the grace
	}
	return h.writeJSON(fileFiled, Filed{Approval: approval, Request: request, Setup: setup, At: now, Exp: now.Add(ttl), ActiveAt: active})
}

// LastFiled returns the record, if any (live or not).
func (h *Host) LastFiled() (Filed, bool) {
	var f Filed
	if h.readJSON(fileFiled, &f) != nil || f.Approval == "" {
		return f, false
	}
	return f, true
}

// Waiting returns the request that still waits for its answer, so a second
// ask does not replace it: the item the user may have open stays valid.
func (h *Host) Waiting() (Filed, bool) {
	f, ok := h.LastFiled()
	if !ok || !h.now().Before(f.Exp) || f.Setup == h.HasBlob() {
		return f, false
	}
	// A request the host has taken an answer for (a refusal included) is
	// spent: its pending file is gone, and a new one is wanted.
	if strings.ContainsAny(f.Request, "/\\.") {
		return f, false
	}
	if _, err := os.Stat(h.path(filepath.Join(dirPending, f.Request+".json"))); err != nil {
		return f, false
	}
	return f, true
}

func (h *Host) clearFiled() { _ = os.Remove(h.path(fileFiled)) }

// Watcher is the host timer: it enforces the lifetime (Host.Sweep) and keeps
// the one "GPG is locked" item honest.
type Watcher struct {
	Host *Host
	// File asks the hub for Face ID (a request in the usual way) and returns
	// the approval's id and the request's; the watcher records them.
	File func() (approval, request string, err error)
	// Withdraw takes a filed approval back (the key was unlocked).
	Withdraw func(approval string) error
	// Active says whether an agent is active on this host.
	Active func() (bool, error)

	Grace   time.Duration // Grace when zero
	Refresh time.Duration // Refresh when zero

	snoozeUntil time.Time
}

// Check does one round and says what it did: "filed", "refreshed",
// "withdrawn" or "".
//
// While the key is locked there is at most one item. It is filed when an agent
// is active and none stands; it is renewed (a fresh request on the same item,
// never a new item) before its request runs out; it is withdrawn when the key
// is unlocked, or when no agent has been active for Grace.
func (w *Watcher) Check() (string, error) {
	h := w.Host
	cfg, err := h.Config()
	if err != nil {
		if errors.Is(err, ErrNotSetUp) {
			return "", nil
		}
		return "", err
	}
	if !h.HasBlob() { // set up is not finished: the set-up item is the user's to answer
		return "", nil
	}
	locked, err := h.GPG.Locked(cfg.Keygrips)
	if err != nil {
		return "", err
	}
	last, haveLast := h.LastFiled()
	if haveLast && last.Setup {
		haveLast = false // the set-up item is not ours to keep
	}
	if !locked {
		if !haveLast {
			return "", nil
		}
		// Unlocked by the passkey (the record is cleared then) or by any other
		// means: the item has no more to ask.
		return w.withdraw(last)
	}
	now := h.now()
	active, known := false, false
	if w.Active != nil {
		a, aerr := w.Active()
		if aerr != nil {
			if !haveLast {
				return "", aerr
			}
			// An item stands: do not drop it for a failed look.
		} else {
			active, known = a, true
		}
	}
	if haveLast {
		if active {
			if now.Sub(last.ActiveAt) > time.Second {
				last.ActiveAt = now
				_ = h.writeJSON(fileFiled, last)
			}
		} else if known {
			grace := w.Grace
			if grace <= 0 {
				grace = Grace
			}
			if now.Sub(last.ActiveAt) >= grace {
				return w.withdraw(last)
			}
		}
		margin := w.Refresh
		if margin <= 0 {
			margin = Refresh
		}
		if last.Exp.Sub(now) > margin {
			return "", nil
		}
		if w.File == nil {
			return "", nil
		}
		ap, req, err := w.File()
		if err != nil {
			return "", w.noted(err)
		}
		return "refreshed", h.RecordFiled(ap, req, false)
	}
	if !active || w.File == nil || now.Before(w.snoozeUntil) { // an idle host files nothing
		return "", nil
	}
	ap, req, err := w.File()
	if err != nil {
		return "", w.noted(err)
	}
	return "filed", h.RecordFiled(ap, req, false)
}

// noted remembers a "Not now" and reports it as no error.
func (w *Watcher) noted(err error) error {
	var sn *SnoozedError
	if errors.As(err, &sn) {
		w.snoozeUntil = sn.Until
		return nil
	}
	return err
}

func (w *Watcher) withdraw(last Filed) (string, error) {
	if w.Withdraw != nil {
		if err := w.Withdraw(last.Approval); err != nil {
			return "", err // keep the record: try again next round
		}
	}
	w.Host.clearFiled()
	return "withdrawn", nil
}

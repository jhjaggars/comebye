package gpgunlock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The trigger side of the unlock (slice 4): who notices a locked key and asks
// for Face ID, with one item per host. Two things ask: `comebye gpg watch`,
// when an agent is active on the host and the key is locked, and a worker's
// `comebye gpg unlock` after a signed commit failed. Both go through the
// record below, so a request that still waits is never filed twice, and the
// watcher withdraws the item when the key is unlocked by any means (pinentry
// included).

// WatchEvery is how often the watcher looks at the agent; Cooldown is how
// long it waits after asking before it asks again for the same lock. The
// request itself waits RequestTTL, so an item left unanswered is gone and is
// asked again only that much later, while an agent is still active.
const (
	WatchEvery = 30 * time.Second
	Cooldown   = 10 * time.Minute
)

const fileFiled = "gpg-filed.json"

// Filed is the request this host last filed with the hub.
type Filed struct {
	Approval string    `json:"approval"`
	Request  string    `json:"request"`
	Setup    bool      `json:"setup,omitempty"`
	At       time.Time `json:"at"`
	Exp      time.Time `json:"exp"`
}

// RecordFiled remembers a request just filed. The expiry is the request's.
func (h *Host) RecordFiled(approval, request string, setup bool) error {
	ttl := RequestTTL
	if setup {
		ttl = SetupTTL
	}
	now := h.now()
	return h.writeJSON(fileFiled, Filed{Approval: approval, Request: request, Setup: setup, At: now, Exp: now.Add(ttl)})
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

	Every    time.Duration // WatchEvery when zero
	Cooldown time.Duration // Cooldown when zero
}

// Check does one round and says what it did: "filed", "withdrawn" or "".
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
	if !locked {
		if !haveLast {
			return "", nil
		}
		// Unlocked by the passkey (the record is cleared then) or by any other
		// means: the item has no more to ask.
		if w.Withdraw != nil {
			if err := w.Withdraw(last.Approval); err != nil {
				return "", err // keep the record: try again next round
			}
		}
		h.clearFiled()
		return "withdrawn", nil
	}
	if _, waiting := h.Waiting(); waiting {
		return "", nil
	}
	cool := w.Cooldown
	if cool <= 0 {
		cool = Cooldown
	}
	if haveLast && !last.Setup && h.now().Before(last.At.Add(cool)) {
		return "", nil
	}
	if w.Active == nil {
		return "", nil
	}
	active, err := w.Active()
	if err != nil || !active { // an idle host files nothing
		return "", err
	}
	ap, req, err := w.File()
	if err != nil {
		return "", err
	}
	return "filed", h.RecordFiled(ap, req, false)
}

// Run enforces the lifetime and the one item until ctx ends: at start (a
// restart after the expiry forgets at once), at each expiry and every
// w.Every. Errors go to log.
func (w *Watcher) Run(ctx context.Context, log func(error)) {
	every := w.Every
	if every <= 0 {
		every = WatchEvery
	}
	for {
		if _, err := w.Host.Sweep(); err != nil && log != nil {
			log(err)
		}
		if _, err := w.Check(); err != nil && log != nil {
			log(err)
		}
		wait := every
		if at, ok := w.Host.NextExpiry(); ok {
			if d := at.Sub(w.Host.now()); d < wait {
				wait = d
			}
		}
		if wait < 50*time.Millisecond {
			wait = 50 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

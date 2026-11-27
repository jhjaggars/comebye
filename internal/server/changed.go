package server

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/state"
)

// One "changed" event for the app (issue #143, slice 1).
//
// Every store the app shows tells the open apps when it is written, through
// one hook per store, so a screen fetches it again at once instead of polling:
//
//	ledger    the ledger, roles, the ledger's cached worktree checks (and so
//	          Archived, which the ledger holds)
//	todos     the to-do list
//	pages     pages and their comments
//	parked    this host's park records (a peer's are relayed by the hub)
//	releases  the rollout, the deploy record, a release's to-do item
//	usage     the token counts
//	job:<id>  a provision job (the provisioner's and the hub's copy), a
//	          sign-in, or job:wake for the hub's book of host starts
//	transcript:<agent id>  a watched agent's session file (transcriptwatch.go)
//
// The hook is the store's own write, so a new write path needs nothing more.
// changed_test.go lists every store the Server holds and fails for one with
// no kind here (or no reason why the app never shows it).

// wireChanged connects each store's write hook to Changed. New calls it once
// the stores are open; the park and comment stores open on first use and
// are wired there (parkStore, commentsStore).
func (s *Server) wireChanged() {
	if s.ledger != nil {
		s.ledger.OnSave(func() { s.Changed(state.ChangedLedger) })
	}
	if s.roles != nil {
		s.roles.onSave = func() { s.Changed(state.ChangedLedger) } // the ledger carries the roles
	}
	if s.todos != nil {
		t := s.todos
		var relSig string
		t.onSave = func() { // t.mu is held
			s.Changed(state.ChangedTodos)
			// The Releases tab shows the release's own item: tell it too
			// when a release item is what changed.
			if sig := releaseItemsSig(t.data.Items); sig != relSig {
				relSig = sig
				s.Changed(state.ChangedReleases)
			}
		}
		relSig = releaseItemsSig(t.data.Items)
	}
	if s.pages != nil {
		s.pages.onSave = func() { s.Changed(state.ChangedPages) }
	}
	if s.rollout != nil {
		s.rollout.onSave = func() { s.Changed(state.ChangedReleases) }
	}
	if s.usage != nil {
		s.usage.store.OnSave(func() { s.Changed(state.ChangedUsage) })
	}
	job := func(id string) { s.Changed(state.ChangedJobPrefix + id) }
	if s.pjobs != nil {
		s.pjobs.onSave = job
	}
	if s.hjobs != nil {
		s.hjobs.onSave = job
	}
	if s.wake != nil {
		s.wake.onSave = func() { job(wakeJobID) } // the host starts the app follows
	}
	if s.signin != nil {
		s.signin.SetOnChange(job)
	}
	if s.fleet != nil {
		s.fleet.OnPeerChanged(s.peerChanged)
	}
}

// wakeJobID is the job id of the hub's book of host starts: one kind for
// every start the book follows (GET /api/provision/wake reads them all).
const wakeJobID = "wake"

// wirePark connects the park store's write hook (parkStore opens it late).
func (s *Server) wirePark() {
	if s.parks != nil {
		s.parks.OnSave(func() { s.Changed(state.ChangedParked) })
	}
}

// peerChanged maps a kind a peer relayed onto the hub's own copies: a
// provisioner's job is the hub job that follows it. The hub reads the job
// from the provisioner at once (not when the app next asks), then tells the
// app.
func (s *Server) peerChanged(host, kind string) {
	pid, ok := strings.CutPrefix(kind, state.ChangedJobPrefix)
	if !ok || s.hjobs == nil {
		return
	}
	var ids []string
	s.hjobs.mu.Lock()
	for _, j := range s.hjobs.jobs {
		if j.Via == host && j.PeerID == pid {
			ids = append(ids, j.ID)
		}
	}
	s.hjobs.mu.Unlock()
	for _, id := range ids {
		s.refreshFromPeer(id)
	}
}

// refreshFromPeer reads a hub job from its provisioner now, once more if
// the provisioner said it changed again meanwhile, so the last change of a
// burst is never lost to the read gap.
func (s *Server) refreshFromPeer(id string) {
	h := s.hjobs
	h.mu.Lock()
	if h.refreshing == nil {
		h.refreshing = map[string]bool{}
	}
	if busy, ok := h.refreshing[id]; ok {
		if !busy {
			h.refreshing[id] = true // read again when the one in flight ends
		}
		h.mu.Unlock()
		return
	}
	h.refreshing[id] = false
	h.mu.Unlock()
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		for {
			h.mu.Lock()
			j := h.jobs[id]
			if j != nil {
				j.polled = time.Time{} // past the read gap
			}
			h.mu.Unlock()
			if j != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				s.refreshJob(ctx, j)
				cancel()
				s.Changed(state.ChangedJobPrefix + id) // also when only "not answering" changed, which is not written
			}
			h.mu.Lock()
			again := h.refreshing[id]
			if !again || j == nil {
				delete(h.refreshing, id)
				h.mu.Unlock()
				return
			}
			h.refreshing[id] = false
			h.mu.Unlock()
		}
	}()
}

// releaseItemsSig is what the release items look like, to tell a write that
// changed one from the rest.
func releaseItemsSig(items []Todo) string {
	var rel []Todo
	for _, it := range items {
		if it.Release != nil {
			rel = append(rel, it)
		}
	}
	if len(rel) == 0 {
		return ""
	}
	b, _ := json.Marshal(rel)
	return string(b)
}

// sameChecks reports whether two worktree checks would show the same.
func sameChecks(a, b worktreeChecks) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

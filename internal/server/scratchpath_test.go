package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// A session's scratch on a peer host: the hub's path probe goes through the
// fleet proxy to the peer's own /api/probe. The proxy used to drop the path
// query, so the peer saw no path and every such item stayed "can't check:
// HOST: want an absolute path", and a session whose only leftover was its
// deleted scratch was never removed.

// probePeer is a peer host that answers /api/probe with a real server's
// handler (its own validation and lookups) and remembers the queries it got.
type probePeer struct {
	mu   sync.Mutex
	seen []url.Values
	h    http.HandlerFunc
}

func (p *probePeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/probe" {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	p.seen = append(p.seen, r.URL.Query())
	p.mu.Unlock()
	p.h(w, r)
}

func (p *probePeer) queries() []url.Values {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]url.Values(nil), p.seen...)
}

// peerProbeRig is a hub ("desk") with one peer ("kube") whose probes are
// answered by the real handler, and a probe root on the peer.
func peerProbeRig(t *testing.T) (hub *Server, peer *probePeer, ps *httptest.Server, tmp string) {
	t.Helper()
	_, tmp = probeRoots(t)
	other := newServer(t, Config{ReadLines: 100, HostName: "kube", StateDir: t.TempDir()}, nil, nil)
	peer = &probePeer{h: other.handleProbe}
	ps = httptest.NewServer(peer)
	t.Cleanup(ps.Close)
	pu, _ := url.Parse(ps.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	hub = newServer(t, Config{ReadLines: 100, Fleet: fl, StateDir: t.TempDir()}, nil, nil)
	hub.probeTTL = 0
	return hub, peer, ps, tmp
}

// scratchPathItem is a path the session calls its own scratch.
func scratchPathItem(name, host string) ledger.AskItem {
	return ledger.AskItem{Type: ledger.ItemPath, Name: name, Host: host, Text: "343 MB of my scratch; deleting it is cleanup I can do if told to go ahead"}
}

func TestRecheckPathThroughAPeer(t *testing.T) {
	s, peer, ps, tmp := peerProbeRig(t)
	dir := filepath.Join(tmp, "jfetch2952")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	it := &removal{ID: "kube/wS:p9", Host: "kube", Worktree: "/home/k/.worktrees/proj/x"}
	item := scratchPathItem(dir, "kube")

	// Still there: the peer looked, with the path it was sent.
	st, now, _ := s.recheck(context.Background(), it, item)
	if st != itemOpen || !strings.Contains(now, "is still there (a directory)") {
		t.Fatalf("present = %s %q", st, now)
	}
	q := peer.queries()
	if len(q) != 1 || q[0].Get("kind") != "path" || q[0].Get("path") != dir {
		t.Fatalf("peer got %v; want kind=path path=%s", q, dir)
	}

	// Deleted: resolved.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if st, now, _ := s.recheck(context.Background(), it, item); st != itemResolved || now != "is gone" {
		t.Fatalf("deleted = %s %q", st, now)
	}

	// The peer's own refusals come back as "can't check", never resolved.
	for _, c := range []struct{ name, want string }{
		{"/etc/passwd", "kube: the hub looks only in the home and temporary directories"},
		{tmp + "/a/../b", "kube: the path may not contain .."},
	} {
		if st, now, _ := s.recheck(context.Background(), it, scratchPathItem(c.name, "kube")); st != itemUnchecked || !strings.Contains(now, c.want) {
			t.Errorf("%s = %s %q; want unchecked ~%q", c.name, st, now, c.want)
		}
	}
	// A relative path is refused on the hub: the peer is not asked.
	n := len(peer.queries())
	if st, now, _ := s.recheck(context.Background(), it, scratchPathItem("tmp/x", "kube")); st != itemUnchecked || !strings.Contains(now, "not an absolute path") {
		t.Errorf("relative = %s %q", st, now)
	}
	if len(peer.queries()) != n {
		t.Error("the peer was asked about a relative path")
	}
	// A host outside the fleet is not asked either.
	if st, now, _ := s.recheck(context.Background(), it, scratchPathItem(dir, "elsewhere")); st != itemUnchecked || !strings.Contains(now, "not in this hub's fleet") {
		t.Errorf("unknown host = %s %q", st, now)
	}
	// A peer that is down leaves it unchecked.
	ps.Close()
	if st, now, _ := s.recheck(context.Background(), it, scratchPathItem(filepath.Join(tmp, "other"), "kube")); st != itemUnchecked || !strings.Contains(now, "can't check") {
		t.Errorf("peer down = %s %q", st, now)
	}
}

// The review of a scratch-only answer on a peer: while the scratch is there it
// is only cleanup (the go-ahead covers it) and, with the re-asks spent, owed;
// once it is gone, every item has resolved, which is the sweep's "nothing
// left" (leftovers.go).
func TestPeerScratchReviewSettlesWhenItIsGone(t *testing.T) {
	s, _, _, tmp := peerProbeRig(t)
	dir := filepath.Join(tmp, "jfetch2952")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-20 * time.Minute)
	it := &removal{ID: "kube/wS:p9", Host: "kube", Worktree: "/home/k/.worktrees/proj/x",
		Mark: ledger.Mark{AgentID: "kube/wS:p9", Ask: &ledger.Ask{State: ledger.AskUnsafe, At: at, Reasks: 3, Items: []ledger.AskItem{scratchPathItem(dir, "kube")}}}}

	ar := s.askReviewOf(context.Background(), it)
	if ar.Resolved || !ar.OnlyCleanup || ar.Items[0].State != itemOpen {
		t.Fatalf("present: %+v", ar)
	}
	if left, ok := leftOnlyOwed(ar); !ok || len(left) != 1 || left[0].Name != dir {
		t.Fatalf("present: owed %v %+v", ok, left)
	}

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	ar = s.askReviewOf(context.Background(), it)
	if !ar.Resolved || ar.Items[0].State != itemResolved {
		t.Fatalf("gone: %+v", ar)
	}
	if _, ok := leftOnlyOwed(ar); ok {
		t.Fatal("gone: owed (it should go the nothing-left way)")
	}
}

// The sweep, end to end on the hub's own host, with the re-asks spent: a
// scratch path that is gone settles the answer ("nothing left"), one still
// there archives the session at once with the path on its record (the
// worktree and branch stay), and one the hub cannot check, or a path that is
// not the session's scratch, keeps it waiting.
func TestSweepWithAScratchPathAfterTheReasks(t *testing.T) {
	now := time.Now().UTC()
	fin := now.Add(-3 * time.Hour)
	setup := func(t *testing.T, items ...ledger.AskItem) (*archiveRig, ledger.Entry) {
		t.Helper()
		r := newArchiveRig(t, "feat/scratch")
		r.s.probeTTL = 0
		e := r.add(t, ledger.Entry{AgentID: "desk/w9:p1", PaneID: "w9:p1", AgentName: "research", Status: ledger.Finished, FinishedAt: &fin, FinalReport: "done", Branch: "feat/scratch", CreatedAt: now.Add(-day)})
		m := ledger.Mark{AgentID: e.AgentID, Host: "desk", PaneID: e.PaneID, Name: e.AgentName, Worktree: r.wt, By: AutoCreator, Reason: "finished", At: now.Add(-2 * time.Hour)}
		if _, err := r.s.ledger.SetMark(m); err != nil {
			t.Fatal(err)
		}
		if _, err := r.s.ledger.SetAsk(e.AgentID, m.At, ledger.Ask{State: ledger.AskUnsafe, At: now.Add(-10 * time.Minute), Reason: "left", Items: items, Reasks: 3}); err != nil {
			t.Fatal(err)
		}
		return r, e
	}
	status := func(t *testing.T, r *archiveRig, e ledger.Entry) ledger.Entry {
		t.Helper()
		got, _ := r.s.ledger.Get(e.ID)
		return got
	}

	t.Run("gone: the sweep settles it", func(t *testing.T) {
		_, tmp := probeRoots(t)
		r, e := setup(t, scratchPathItem(filepath.Join(tmp, "jfetch"), ""))
		if n, _ := r.s.leftovers(context.Background(), now, false); n != 1 {
			t.Fatalf("settled %d", n)
		}
		got := status(t, r, e)
		if got.Mark == nil || got.Mark.Ask == nil || got.Mark.Ask.State != ledger.AskSafe {
			t.Fatalf("mark = %+v", got.Mark)
		}
		if !strings.Contains(got.Mark.Ask.Reason, "is gone") {
			t.Fatalf("reason = %q", got.Mark.Ask.Reason)
		}
		// Settled, it is not archived: the proof decides its removal.
		if done := r.s.archiveStep(context.Background(), now); len(done) != 0 {
			t.Fatalf("archived: %+v", done)
		}
	})

	t.Run("still there: archived at once, the work kept", func(t *testing.T) {
		_, tmp := probeRoots(t)
		dir := filepath.Join(tmp, "jfetch")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		r, e := setup(t, scratchPathItem(dir, ""))
		if n, _ := r.s.leftovers(context.Background(), now, false); n != 0 {
			t.Fatalf("settled %d with the scratch still there", n)
		}
		done := r.s.archiveStep(context.Background(), now)
		if len(done) != 1 || done[0].Kind != doneArchived {
			t.Fatalf("done = %+v", done)
		}
		got := status(t, r, e)
		if got.Status != ledger.Archived || got.Archive == nil || len(got.Archive.Left) != 1 || got.Archive.Left[0].Name != dir {
			t.Fatalf("entry = %s %+v", got.Status, got.Archive)
		}
		if _, err := os.Stat(r.wt); err != nil {
			t.Fatalf("worktree gone: %v", err)
		}
		if out := git(t, r.repo, "branch", "--list", "feat/scratch"); !strings.Contains(out, "feat/scratch") {
			t.Fatalf("branch gone: %q", out)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("the hub touched the scratch: %v", err)
		}
	})

	for _, c := range []struct {
		name string
		item func(tmp string) ledger.AskItem
	}{
		{"can't check (outside the probe roots): waits", func(string) ledger.AskItem { return scratchPathItem("/etc/jfetch", "") }},
		{"can't check (a host outside the fleet): waits", func(tmp string) ledger.AskItem { return scratchPathItem(filepath.Join(tmp, "x"), "elsewhere") }},
		{"not its scratch, still there: waits", func(tmp string) ledger.AskItem {
			os.Mkdir(filepath.Join(tmp, "clone"), 0o755)
			return ledger.AskItem{Type: ledger.ItemPath, Name: filepath.Join(tmp, "clone"), Text: "the user's clone, keep it"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, tmp := probeRoots(t)
			r, e := setup(t, c.item(tmp))
			if n, _ := r.s.leftovers(context.Background(), now, false); n != 0 {
				t.Fatalf("settled %d", n)
			}
			if done := r.s.archiveStep(context.Background(), now); len(done) != 0 {
				t.Fatalf("archived: %+v", done)
			}
			if got := status(t, r, e); got.Status != ledger.ReadyToRemove || got.Mark.Ask.State != ledger.AskUnsafe {
				t.Fatalf("entry = %s %+v", got.Status, got.Mark.Ask)
			}
		})
	}
}

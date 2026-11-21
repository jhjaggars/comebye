package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/provision"
)

const day = 24 * time.Hour

// The archive clock and its guards (archiveBlock is the rule book).
func TestArchiveBlock(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	fin := func(branch string, idle time.Duration) ledger.Entry {
		return ledger.Entry{Status: ledger.Finished, Branch: branch, Worktree: "/wt/x", CreatedAt: now.Add(-30 * day), FinishedAt: at(idle), FinalReport: "r"}
	}
	ok := archiveFacts{knownHost: true}
	idle := &herdr.AgentInfo{Status: herdr.StatusIdle}
	marked := func(m ledger.Mark) ledger.Entry {
		e := fin("feat/x", 8*day)
		m.At = now.Add(-8 * day)
		e.Status, e.Mark = ledger.ReadyToRemove, &m
		return e
	}
	restored := fin("feat/x", 9*day)
	restored.ArchiveHistory = []ledger.ArchiveEvent{{Event: "unarchived", At: now.Add(-day)}}
	looped := fin("feat/x", 8*day)
	looped.Loop = "watch PR 4"
	running := fin("feat/x", 8*day)
	running.Status = ledger.Running
	noWT := fin("feat/x", 8*day)
	noWT.Worktree = ""
	progressed := fin("feat/x", 8*day)
	progressed.Progress = &ledger.Progress{Text: "x", At: now.Add(-2 * day)}
	for _, tc := range []struct {
		name string
		e    ledger.Entry
		f    archiveFacts
		want bool
	}{
		{"feat 7 days", fin("feat/x", 7*day+time.Minute), ok, true},
		{"feat 6 days", fin("feat/x", 6*day), ok, false},
		{"plan 3 days", fin("plan/x", 3*day+time.Minute), ok, true},
		{"smoke 3 days", fin("smoke/x", 3*day+time.Minute), ok, true},
		{"poc 2 days", fin("poc/x", 2*day), ok, false},
		{"docs 3 days is work to land", fin("docs/x", 3*day+time.Minute), ok, false},
		{"docs 7 days", fin("docs/x", 7*day+time.Minute), ok, true},
		{"a live idle agent", fin("feat/x", 8*day), archiveFacts{knownHost: true, live: idle}, true},
		{"a working agent", fin("feat/x", 8*day), archiveFacts{knownHost: true, live: &herdr.AgentInfo{Status: herdr.StatusWorking}}, false},
		{"a blocked agent", fin("feat/x", 8*day), archiveFacts{knownHost: true, live: &herdr.AgentInfo{Status: herdr.StatusBlocked}}, false},
		{"a coordinator", fin("feat/x", 8*day), archiveFacts{knownHost: true, coordinator: true}, false},
		{"a pinned park record", fin("feat/x", 8*day), archiveFacts{knownHost: true, pinned: true}, false},
		{"an open to-do", fin("feat/x", 8*day), archiveFacts{knownHost: true, openTodo: true}, false},
		{"a queued prompt", fin("feat/x", 8*day), archiveFacts{knownHost: true, queued: true}, false},
		{"a background task", fin("feat/x", 8*day), archiveFacts{knownHost: true, background: true}, false},
		{"a waiting plan", fin("feat/x", 8*day), archiveFacts{knownHost: true, planWaiting: true}, false},
		{"an unknown host", fin("feat/x", 8*day), archiveFacts{}, false},
		{"a loop worker", looped, ok, false},
		{"running", running, ok, false},
		{"no worktree (home)", noWT, ok, false},
		{"a progress note 2 days ago", progressed, ok, false},
		{"restored a day ago", restored, ok, false},
		{"marked, LEFT unsettled", marked(ledger.Mark{Ask: &ledger.Ask{State: ledger.AskUnsafe, At: now.Add(-8 * day)}}), ok, true},
		{"marked as delivered", marked(ledger.Mark{Discard: true}), ok, true},
		{"marked SAFE: the removal path", marked(ledger.Mark{Ask: &ledger.Ask{State: ledger.AskSafe, At: now.Add(-8 * day)}}), ok, false},
		{"marked, a re-ask in flight", marked(ledger.Mark{Ask: &ledger.Ask{State: ledger.AskUnsafe, At: now.Add(-8 * day), ReaskedAt: at(8 * day)}}), ok, false},
		{"marked, still asking", marked(ledger.Mark{Ask: &ledger.Ask{State: ledger.AskAsking, At: now.Add(-8 * day)}}), ok, false},
	} {
		got := archiveBlock(tc.e, tc.f, now, 7*day, 3*day)
		if (got == "") != tc.want {
			t.Errorf("%s: block = %q, want archive=%v", tc.name, got, tc.want)
		}
	}
}

// archiveRig is a hub with one Rust-shaped worktree on its own host and no live agent.
type archiveRig struct {
	s        *Server
	fh       *fakeLaunchHerdr
	repo, wt string
}

func newArchiveRig(t *testing.T, branch string) *archiveRig {
	t.Helper()
	root, repo, wtRoot := workerTree(t)
	os.WriteFile(repo+"/.gitignore", []byte("target/\nnode_modules/\n.env\ndata/\n"), 0o644)
	os.WriteFile(repo+"/Cargo.toml", []byte("[package]\nname = \"x\"\n"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "rust")
	wt := filepath.Join(wtRoot, "proj", "w")
	git(t, repo, "worktree", "add", "-q", "-b", branch, wt, "main")
	os.WriteFile(wt+"/work.txt", []byte("w\n"), 0o644)
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-q", "-m", "work not in main")
	fh, client := startFakeLaunchHerdr(t)
	m := managerWith(t, client, "desk", 0)
	s := newServer(t, Config{ReadLines: 100, HostName: "desk", StateDir: t.TempDir(), LaunchRoots: []string{root}, WorktreeRoot: wtRoot, AllowLogins: []string{"me@example.com"}, AgentToken: testToken}, client, m)
	s.askPoll, s.askWait = 10_000_000, 50_000_000
	t.Cleanup(s.asking.Wait)
	return &archiveRig{s: s, fh: fh, repo: repo, wt: wt}
}

// The CACHEDIR.TAG (44 bytes) counts with target/.
func writeN(t *testing.T, path string, n int) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (r *archiveRig) seedBuildOutput(t *testing.T) {
	writeN(t, r.wt+"/target/debug/big.rlib", 300_000)
	os.WriteFile(r.wt+"/target/CACHEDIR.TAG", []byte("Signature: 8a477f597d28d172789f06886806bc55\n"), 0o644)
	writeN(t, r.wt+"/node_modules/left-pad/index.js", 100_000)
	os.WriteFile(r.wt+"/notes.md", []byte("untracked notes\n"), 0o644)
	os.WriteFile(r.wt+"/.env", []byte("SECRET=1\n"), 0o644)
	writeN(t, r.wt+"/data/rows.csv", 50_000)
}

func (r *archiveRig) add(t *testing.T, e ledger.Entry) ledger.Entry {
	t.Helper()
	e.Host, e.Repo, e.RepoName, e.Base, e.Worktree = "desk", r.repo, "proj", "main", r.wt
	if e.Branch == "" {
		e.Branch = "poc/rust"
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().Add(-20 * day)
	}
	got, err := r.s.ledger.Add(e)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The loop archives a finished session whose agent is gone: the build output
// goes (target/ and node_modules/), untracked, ignored and data files stay,
// the bytes freed are recorded, and it leaves the board, the review and the
// sweep. A dirty tree is cleared the same way.
func TestArchiveStepClearsBuildOutputOnly(t *testing.T) {
	r := newArchiveRig(t, "poc/rust")
	r.seedBuildOutput(t)
	fin := time.Now().Add(-4 * day)
	e := r.add(t, ledger.Entry{AgentID: "desk/w9:p1", PaneID: "w9:p1", AgentName: "kv", Status: ledger.Finished, FinishedAt: &fin, FinalReport: "the poc works", ReportSource: "explicit"})
	done := r.s.archiveStep(context.Background(), time.Now())
	if len(done) != 1 || done[0].Kind != doneArchived || done[0].Freed != 400_044 {
		t.Fatalf("done = %+v", done)
	}
	got, _ := r.s.ledger.Get(e.ID)
	a := got.Archive
	if got.Status != ledger.Archived || a == nil || a.BuildFreed != 400_044 || a.BuildPending || a.By != AutoCreator || a.Unmerged != 1 || a.Tip == "" {
		t.Fatalf("entry = %s %+v", got.Status, a)
	}
	if !strings.Contains(strings.Join(a.BuildDirs, ","), "target") || !strings.Contains(strings.Join(a.BuildDirs, ","), "node_modules") {
		t.Fatalf("dirs = %v", a.BuildDirs)
	}
	for _, gone := range []string{"target", "node_modules"} {
		if _, err := os.Stat(filepath.Join(r.wt, gone)); !os.IsNotExist(err) {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, kept := range []string{"notes.md", ".env", "data/rows.csv", "work.txt", "Cargo.toml"} {
		if _, err := os.Stat(filepath.Join(r.wt, kept)); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
	if git(t, r.repo, "branch", "--list", "poc/rust") == "" {
		t.Fatal("the branch went")
	}
	// A second step does nothing more.
	if again := r.s.archiveStep(context.Background(), time.Now()); len(again) != 0 {
		t.Fatalf("archived again: %+v", again)
	}
	// Off the board's lanes, counted; out of the review and the sweep.
	w := do(r.s, "GET", "/api/board", userHdr, "")
	var b BoardView
	json.Unmarshal(w.Body.Bytes(), &b)
	if b.Loose.Archived != 1 {
		t.Fatalf("loose = %+v", b.Loose)
	}
	for _, l := range b.Lanes {
		for _, it := range l.Items {
			if it.ID == e.ID {
				t.Fatalf("in lane %s", l.Key)
			}
		}
	}
	for _, it := range r.s.marked() {
		if it.EntryID == e.ID {
			t.Fatal("in the removals review")
		}
	}
	res := sweepOf(t, r.s, `{"dry_run":true}`)
	for _, l := range [][]sweepItem{res.Ready, res.NeedsLook, res.Delivered, res.Gone} {
		for _, it := range l {
			if it.EntryID == e.ID {
				t.Fatalf("the sweep lists it: %+v", it)
			}
		}
	}
	// Any reader lists it, with the bytes freed.
	var v archivedView
	w = do(r.s, "GET", "/api/archived", agentHdr("w1:p1"), "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Count != 1 || v.BuildFreed != 400_044 || v.Archived[0].Purpose != "poc" {
		t.Fatalf("archived = %d %s", w.Code, w.Body)
	}
}

func TestArchiveBoardLineSaysWhatItFreed(t *testing.T) {
	r := newArchiveRig(t, "poc/rust")
	r.s.recordDone(time.Now(), []sweepDone{{At: time.Now(), Kind: doneArchived, Name: "kv", Freed: 18_400_000_000}})
	line, _, ok := r.s.sweepBoardLine()
	if !ok {
		r.s.sweeps.mu.Lock()
		r.s.sweeps.st.LastRunAt = time.Now()
		r.s.sweeps.mu.Unlock()
		line, _, _ = r.s.sweepBoardLine()
	}
	if !strings.Contains(line, "archived 1 session (kv), freed 18.4 GB of build output") {
		t.Fatalf("line = %q", line)
	}
	evs := r.s.events.query(time.Now().Add(-time.Hour), []string{evSweep}, 0)
	if len(evs) == 0 || !strings.Contains(evs[0].Text, "archived 1 session") {
		t.Fatalf("log = %+v", evs)
	}
}

// The host call refuses a worktree with a live agent and one outside the
// worktree root, and leaves a symlinked target/ alone.
func TestBuildoutClearHostCall(t *testing.T) {
	r := newArchiveRig(t, "feat/x")
	outside := t.TempDir()
	writeN(t, outside+"/big", 10_000)
	os.Symlink(outside, r.wt+"/target")
	w := do(r.s, "POST", "/api/buildout/clear", userHdr, `{"worktree":"`+r.wt+`"}`)
	var res buildoutClearResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || res.Freed != 0 {
		t.Fatalf("clear = %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(outside + "/big"); err != nil {
		t.Fatal("a symlinked target/ was followed")
	}
	if fi, err := os.Lstat(r.wt + "/target"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink itself went")
	}
	if w := do(r.s, "POST", "/api/buildout/clear", userHdr, `{"worktree":"`+outside+`"}`); w.Code != 403 {
		t.Fatalf("outside the root = %d %s", w.Code, w.Body)
	}
	r.fh.set(func(f *fakeLaunchHerdr) {
		f.agents = []herdr.AgentInfo{{Name: "w", PaneID: "w9:p1", WorkspaceID: "w9", Agent: "pi", Status: herdr.StatusIdle, CWD: r.wt}}
	})
	if w := do(r.s, "POST", "/api/buildout/clear", userHdr, `{"worktree":"`+r.wt+`"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "agent_live") {
		t.Fatalf("with a live agent = %d %s", w.Code, w.Body)
	}
}

// A host that does not answer: archived at once with the clearing pending; the
// next proof on a host that answers clears it.
func TestArchivePendingBuildOutput(t *testing.T) {
	r := newArchiveRig(t, "feat/far")
	fin := time.Now().Add(-8 * day)
	far, _ := r.s.ledger.Add(ledger.Entry{Host: "far", Branch: "feat/far", Worktree: "/wt/far", AgentName: "far", Status: ledger.Finished, FinishedAt: &fin})
	got, err := r.s.archiveOne(context.Background(), far, nil, "me@example.com", "by hand", false)
	if err != nil || got.Status != ledger.Archived || !got.Archive.BuildPending || got.Archive.Unpushed != -1 {
		t.Fatalf("archive = %+v %v", got.Archive, err)
	}
	// Now on a host that answers.
	r.seedBuildOutput(t)
	e := r.add(t, ledger.Entry{AgentName: "near", Status: ledger.Finished, FinishedAt: &fin})
	r.s.ledger.Archive(e.ID, ledger.Archival{By: "hub", BuildPending: true, Unmerged: -1, Unpushed: -1})
	r.s.archiveRecheckAll(context.Background(), time.Now())
	g, _ := r.s.ledger.Get(e.ID)
	if g.Status != ledger.Archived || g.Archive.BuildPending || g.Archive.BuildFreed != 400_044 || g.Archive.Unmerged != 1 || g.Archive.Unpushed != 1 {
		t.Fatalf("after the host is up = %+v", g.Archive)
	}
}

// The daily proof: work that landed since is unarchived and marked (the normal
// removal path); a new commit on the branch brings it back to the board.
func TestArchiveRecheck(t *testing.T) {
	r := newArchiveRig(t, "feat/landed")
	fin := time.Now().Add(-8 * day)
	e := r.add(t, ledger.Entry{Branch: "feat/landed", AgentID: "desk/w9:p1", AgentName: "landed", Status: ledger.Finished, FinishedAt: &fin, FinalReport: "r"})
	if _, err := r.s.archiveOne(context.Background(), e, nil, AutoCreator, "idle", true); err != nil {
		t.Fatal(err)
	}
	// Someone merged it.
	git(t, r.repo, "merge", "-q", "--no-edit", "feat/landed")
	git(t, r.repo, "update-ref", "refs/remotes/origin/main", "main")
	git(t, r.repo, "update-ref", "refs/remotes/origin/feat/landed", "feat/landed")
	r.s.ledger.UpdateArchive(e.ID, func(a *ledger.Archival) { a.CheckedAt = nil })
	r.s.archiveRecheckAll(context.Background(), time.Now())
	r.s.asking.Wait()
	g, _ := r.s.ledger.Get(e.ID)
	if g.Status != ledger.ReadyToRemove || g.Mark == nil || !strings.Contains(g.Mark.Reason, "archived, and since landed") {
		t.Fatalf("landed archived = %s %+v", g.Status, g.Mark)
	}

	// A new commit brings it back.
	r2 := newArchiveRig(t, "feat/more")
	e2 := r2.add(t, ledger.Entry{Branch: "feat/more", AgentName: "more", Status: ledger.Finished, FinishedAt: &fin})
	if _, err := r2.s.archiveOne(context.Background(), e2, nil, AutoCreator, "idle", true); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(r2.wt+"/more.txt", []byte("m\n"), 0o644)
	git(t, r2.wt, "add", ".")
	git(t, r2.wt, "commit", "-q", "-m", "more")
	r2.s.ledger.UpdateArchive(e2.ID, func(a *ledger.Archival) { a.CheckedAt = nil })
	r2.s.archiveRecheckAll(context.Background(), time.Now())
	if g, _ := r2.s.ledger.Get(e2.ID); g.Status != ledger.Finished || g.Archive != nil {
		t.Fatalf("after a new commit = %s", g.Status)
	}
}

// A live idle agent is parked first (a dirty tree is allowed), its park id
// recorded.
func TestArchiveParksTheLiveAgent(t *testing.T) {
	s, fh, wt, _ := parkSetup(t)
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.ledger = l
	os.WriteFile(wt+"/dirty.txt", []byte("uncommitted\n"), 0o644)
	fin := time.Now().Add(-8 * day)
	e, _ := l.Add(ledger.Entry{Host: "desk", RepoName: "proj", Branch: "w", Base: "main", Worktree: wt, AgentID: "desk/w9:p1", PaneID: "w9:p1", AgentName: "w", Status: ledger.Finished, FinishedAt: &fin})
	var live herdr.AgentInfo
	fh.set(func(f *fakeLaunchHerdr) { live = f.agents[0] })
	got, err := s.archiveOne(context.Background(), e, &live, AutoCreator, "idle", true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ledger.Archived || got.Archive.ParkID == "" {
		t.Fatalf("entry = %+v", got.Archive)
	}
	rec, ok := s.parkStore().Get(got.Archive.ParkID)
	if !ok || rec.Why != "archive" || !rec.Dirty {
		t.Fatalf("park record = %+v %v", rec, ok)
	}
	if _, err := os.Stat(wt + "/dirty.txt"); err != nil {
		t.Fatal("the dirty file went")
	}
	if fh.call("workspace.close") == nil && fh.call("pane.close") == nil {
		t.Fatal("the pane was not closed")
	}
}

// Archiving and restoring are the user's or a coordinator's, never an agent's
// on itself; restore returns it to the state it was archived from.
func TestArchiveAPIRoles(t *testing.T) {
	f := newSweepFix(t)
	s := f.s
	post := func(path string, h map[string]string, body string) (int, string) {
		w := do(s, "POST", path, h, body)
		return w.Code, w.Body.String()
	}
	if code, body := post("/api/archived", agentHdr("w2:p1"), `{"ref":"pushed","reason":"x"}`); code != 409 || !strings.Contains(body, `"self"`) {
		t.Fatalf("self = %d %s", code, body)
	}
	if code, body := post("/api/archived", agentHdr("w3:p1"), `{"ref":"pushed","reason":"x"}`); code != 403 || !strings.Contains(body, "coordinator_only") {
		t.Fatalf("a plain agent = %d %s", code, body)
	}
	if code, body := post("/api/archived", agentHdr("w5:p1"), `{"ref":"pushed"}`); code != 400 || !strings.Contains(body, "reason_required") {
		t.Fatalf("no reason = %d %s", code, body)
	}
	// The coordinator archives it (its agent is a pi without a session, so it
	// is gone from the fleet first).
	f.fh.set(func(fk *fakeLaunchHerdr) {
		kept := fk.agents[:0]
		for _, a := range fk.agents {
			if a.Name != "pushed" {
				kept = append(kept, a)
			}
		}
		fk.agents = kept
	})
	s.manager.Refresh()
	waitFor(t, func() bool {
		for _, a := range s.fleet.Agents() {
			if a.Name == "pushed" {
				return false
			}
		}
		return true
	})
	code, body := post("/api/archived", agentHdr("w5:p1"), `{"ref":"pushed","reason":"superseded by #90"}`)
	if code != 200 || statusOf(s, f.ids["pushed"]) != ledger.Archived {
		t.Fatalf("coordinator = %d %s", code, body)
	}
	if code, body := post("/api/archived", userHdr, `{"ref":"pushed"}`); code != 409 || !strings.Contains(body, "already_archived") {
		t.Fatalf("twice = %d %s", code, body)
	}
	if code, body := post("/api/archived/restore", agentHdr("w3:p1"), `{"ref":"pushed"}`); code != 403 {
		t.Fatalf("restore by a plain agent = %d %s", code, body)
	}
	if code, body := post("/api/archived/restore", userHdr, `{"ref":"pushed","resume":true}`); code != 200 || !strings.Contains(body, "resume_error") {
		t.Fatalf("restore = %d %s", code, body)
	}
	if st := statusOf(s, f.ids["pushed"]); st != ledger.Finished {
		t.Fatalf("after restore = %s", st)
	}
	if code, _ := post("/api/archived/restore", userHdr, `{"ref":"pushed"}`); code != 404 {
		t.Fatalf("restore of a session not archived = %d", code)
	}
}

// While an archived worktree has commits on no remote, the hub holds its
// host's destroy timer, and lifts the hold once it no longer applies.
func TestArchiveHoldsTheDestroy(t *testing.T) {
	r := newArchiveRig(t, "feat/x")
	book := provision.OpenHostBook("")
	book.Add("kv1", "pj_0123456789abcdef", provision.DefaultSpec("kv1", "t3.small"), "us-east-1", nil, provision.Estimate{})
	r.s.phosts = book
	e, _ := r.s.ledger.Add(ledger.Entry{Host: "kv1", Branch: "poc/x", Worktree: "/wt/x", AgentName: "kvpoc", Status: ledger.Finished})
	r.s.ledger.Archive(e.ID, ledger.Archival{By: "hub", Unmerged: 3, Unpushed: 2})
	r.s.holdDestroys(context.Background(), time.Now())
	h, _ := book.Get("kv1")
	if !h.Held(time.Now()) || !strings.Contains(h.HoldWhy, "kvpoc") {
		t.Fatalf("hold = %+v %q", h.HoldUntil, h.HoldWhy)
	}
	evs := r.s.events.query(time.Now().Add(-time.Hour), []string{evArchive}, 0)
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "held the destroy timer of kv1") {
		t.Fatalf("log = %+v", evs)
	}
	// Restored and pushed: the hold is lifted.
	r.s.ledger.Unarchive(e.ID, "me", "")
	r.s.holdDestroys(context.Background(), time.Now())
	if h, _ := book.Get("kv1"); h.Held(time.Now()) {
		t.Fatal("the hold was not lifted")
	}
}

package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gateRig is two hosts' sessions and a clock the test moves.
func gateRig(t *testing.T) (*rig, *Index) {
	t.Helper()
	hasSqlite(t)
	r := newRig(t)
	twoHosts(r)
	r.now = time.Now().UTC().Truncate(time.Second)
	d := filepath.Join(r.dir, "state")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return r, &Index{Path: filepath.Join(d, "index.db")}
}

func gateMeta(t *testing.T, ix *Index, k string) string {
	t.Helper()
	return ix.metaValues()[k]
}

func TestGateOnlyIndexesWhenSomethingChanged(t *testing.T) {
	r, ix := gateRig(t)
	links := []Link{{ID: "h-1", Host: "laharl", Branch: "feat/span", Status: "running"}}
	tick := func(what string, links []Link, wantRan bool) GateResult {
		t.Helper()
		r.now = r.now.Add(5 * time.Minute)
		res, err := ix.SyncIfChanged(r.a, r.op, links, GateOptions{LedgerOK: true})
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if res.Ran != wantRan {
			t.Fatalf("%s: ran=%v, want %v (%+v)", what, res.Ran, wantRan, res)
		}
		if got := gateMeta(t, ix, metaCheckedAt); got != r.now.Format(time.RFC3339) {
			t.Fatalf("%s: checked_at %s", what, got)
		}
		return res
	}
	if res := tick("a new index", links, true); res.Stats.Indexed != 4 {
		t.Fatalf("%+v", res)
	}
	built := gateMeta(t, ix, metaBuiltAt)
	threads, _ := ix.Query("SELECT count(*) AS n FROM threads;")
	// Nothing happens on a match, down to not touching the thread tables.
	if _, err := ix.run("UPDATE threads SET title='marked by the test';", false); err != nil {
		t.Fatal(err)
	}
	tick("nothing new", links, false)
	if gateMeta(t, ix, metaBuiltAt) != built {
		t.Error("a no-op changed the build time")
	}
	if rows, _ := ix.Query("SELECT count(*) AS n FROM threads WHERE title='marked by the test';"); num(rows[0], "n") != num(threads[0], "n") {
		t.Error("a no-op rebuilt the threads")
	}
	// Each kind of change in the store, from any host.
	r.hostSession("jjaggars-kubevirt", sidHub2, "/home/u/code/solo", "main", "2026-10-04T08:00:00Z", "something only the hub did", "more output")
	if res := tick("a segment on another host", links, true); res.Stats.Indexed != 1 {
		t.Errorf("%+v", res)
	}
	tick("then nothing", links, false)
	if err := r.a.forHost("jjaggars-kubevirt").SetSummary("claude", sidHub2, "a summary", "test"); err != nil {
		t.Fatal(err)
	}
	tick("a note", links, true)
	tick("then nothing", links, false)
	if _, err := r.a.forHost("laharl").DeleteSession("claude", "-p", sidLah2); err != nil {
		t.Fatal(err)
	}
	if res := tick("a delete on request", links, true); res.Stats.Removed != 1 {
		t.Errorf("%+v", res)
	}
	tick("then nothing", links, false)
	if err := r.a.forHost("w1.i-1").ClaimWorker(WorkerInfo{Name: "w1", Provider: "aws", InstanceID: "i-1"}); err != nil {
		t.Fatal(err)
	}
	tick("a host claim", links, true)
	tick("then nothing", links, false)
	// The ledger, with the store as it was.
	changed := []Link{{ID: "h-1", Host: "laharl", Branch: "feat/span", Status: "finished", FinalReport: "## Summary\ndone"}}
	tick("a ledger change", changed, true)
	tick("then nothing", changed, false)
}

// Listing failing is not "nothing changed", and leaves the check time alone, so
// the index goes stale if it lasts.
func TestGateFailsWhenTheStoreCannotBeListed(t *testing.T) {
	r, ix := gateRig(t)
	if _, err := ix.SyncIfChanged(r.a, r.op, nil, GateOptions{LedgerOK: true}); err != nil {
		t.Fatal(err)
	}
	before := gateMeta(t, ix, metaCheckedAt)
	r.now = r.now.Add(5 * time.Minute)
	a := *r.a
	a.B = failingList{r.a.B, "notes/"}
	if res, err := ix.SyncIfChanged(&a, r.op, nil, GateOptions{LedgerOK: true}); err == nil {
		t.Fatalf("%+v", res)
	}
	if got := gateMeta(t, ix, metaCheckedAt); got != before {
		t.Errorf("checked_at moved to %s", got)
	}
}

// A host that cannot be read fails the tick and is retried on the next one, even
// with nothing else new.
func TestGateRetriesAHostThatFailed(t *testing.T) {
	r, ix := gateRig(t)
	if _, err := ix.SyncIfChanged(r.a, r.op, nil, GateOptions{LedgerOK: true}); err != nil {
		t.Fatal(err)
	}
	r.hostSession("jjaggars-kubevirt", sidHub2, "/home/u/code/solo", "main", "2026-10-04T08:00:00Z", "something only the hub did", "more output")
	r.now = r.now.Add(5 * time.Minute)
	a := *r.a
	a.B = failOnce{r.a.B, "v1/jjaggars-kubevirt/"}
	if _, err := ix.SyncIfChanged(&a, r.op, nil, GateOptions{LedgerOK: true}); err == nil || !strings.Contains(err.Error(), "jjaggars-kubevirt") {
		t.Fatalf("%v", err)
	}
	r.now = r.now.Add(5 * time.Minute)
	res, err := ix.SyncIfChanged(r.a, r.op, nil, GateOptions{LedgerOK: true})
	if err != nil || !res.Ran || res.Stats.Indexed != 1 {
		t.Errorf("%v %+v", err, res)
	}
}

// failOnce fails listing a prefix directly, the way syncHost does, but not the
// whole of v1/, the way the gate does.
type failOnce struct {
	Backend
	prefix string
}

func (f failOnce) List(p string) ([]string, error) {
	if p == f.prefix {
		return nil, errors.New("blip")
	}
	return f.Backend.List(p)
}

// The ledger being down must not cost threads their ledger outcomes. New sessions
// are still indexed, and threads catch up when it is back.
func TestGateKeepsThreadsWhileTheLedgerIsDown(t *testing.T) {
	r, ix := gateRig(t)
	cwd := "/home/u/.worktrees/widgets/feat-span"
	links := []Link{{ID: "h-1", Host: "laharl", AgentID: "a", RepoName: "widgets", Branch: "feat/span", Worktree: cwd, CreatedAt: "2026-10-02T07:59:00Z", Status: "finished", FinalReport: "## Summary\ndone", ReportSource: "explicit"}}
	if _, err := ix.SyncIfChanged(r.a, r.op, links, GateOptions{LedgerOK: true}); err != nil {
		t.Fatal(err)
	}
	reports := func() int {
		t.Helper()
		rows, err := ix.Query("SELECT count(*) AS n FROM outcomes WHERE kind='report';")
		if err != nil {
			t.Fatal(err)
		}
		return num(rows[0], "n")
	}
	if reports() != 1 {
		t.Fatalf("no report to begin with")
	}
	// Down, with nothing new: only the check moves.
	r.now = r.now.Add(5 * time.Minute)
	res, err := ix.SyncIfChanged(r.a, r.op, nil, GateOptions{})
	if err != nil || res.Ran {
		t.Fatalf("%v %+v", err, res)
	}
	// Down, with a new session: it is searchable, and the outcomes stay.
	r.hostSession("hub", sidHub2, "/home/u/code/solo", "main", "2026-10-05T08:00:00Z", "a word only here: quetzal")
	r.now = r.now.Add(5 * time.Minute)
	res, err = ix.SyncIfChanged(r.a, r.op, nil, GateOptions{})
	if err != nil || !res.Ran || res.Stats.Indexed != 1 {
		t.Fatalf("%v %+v", err, res)
	}
	if hits, err := ix.Search("quetzal", Filter{}); err != nil || len(hits) != 1 {
		t.Errorf("%v %v", err, hits)
	}
	if reports() != 1 {
		t.Error("the ledger being down dropped a report")
	}
	if got := threadsOf(t, ix); len(got) == 0 {
		t.Error("no threads")
	}
	// Back: the new session gets its thread, with no other change.
	r.now = r.now.Add(5 * time.Minute)
	res, err = ix.SyncIfChanged(r.a, r.op, links, GateOptions{LedgerOK: true})
	if err != nil || !res.Ran {
		t.Fatalf("%v %+v", err, res)
	}
	found := false
	for _, ss := range threadsOf(t, ix) {
		for _, s := range ss {
			found = found || s == "hub/"+sidHub2
		}
	}
	if !found || reports() != 1 {
		t.Errorf("found=%v reports=%d", found, reports())
	}
}

func commitOutcome(t *testing.T, ix *Index, sha string) (kind, source string) {
	t.Helper()
	rows, err := ix.Query("SELECT kind, source FROM outcomes WHERE ref=" + q(sha) + ";")
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", err, rows)
	}
	return sget(rows[0], "kind"), sget(rows[0], "source")
}

// The git check runs hourly, with or without other changes. In between, a commit
// keeps its verdict, a new commit is asked about locally, and nothing is fetched.
func TestGateGitPassIsHourly(t *testing.T) {
	r, ix := gateRig(t)
	origin, clone := gitWorld(t)
	other := filepath.Join(t.TempDir(), "elsewhere")
	gitDo(t, filepath.Dir(other), "clone", "-q", origin, other)
	// A change is a file named for the subject, so that the same subject is the same
	// change: landing rebases, and git places a commit by its change, not its message.
	// (Empty commits have no change to match, and two of them made in the same second
	// with one subject are even the same commit.)
	change := func(subject string) {
		t.Helper()
		name := strings.ReplaceAll(subject, " ", "_") + ".txt"
		if err := os.WriteFile(filepath.Join(other, name), []byte(subject+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitDo(t, other, "add", name)
		gitDo(t, other, "commit", "-q", "-m", subject)
	}
	land := func(subject string) string {
		t.Helper()
		gitDo(t, other, "pull", "-q", "origin", "main")
		change(subject)
		gitDo(t, other, "push", "-q", "origin", "HEAD:main")
		return gitDo(t, other, "rev-parse", "HEAD")
	}
	branchCommit := func(subject string) string {
		t.Helper()
		gitDo(t, other, "checkout", "-q", "-B", "feat/x", "origin/main")
		change(subject)
		sha := gitDo(t, other, "rev-parse", "HEAD")
		// Workers push their branch as a backup, which is how this checkout comes to
		// have the commit: a patch-id can only be taken of a commit it holds.
		gitDo(t, other, "push", "-q", "-f", "origin", "feat/x")
		gitDo(t, other, "checkout", "-q", "main")
		return sha
	}
	cwd := filepath.Join(clone, "..", "..", ".worktrees", "widgets", "feat-x")
	opts := GateOptions{Git: true, LedgerOK: true}
	tick := func(minutes int) GateResult {
		t.Helper()
		r.now = r.now.Add(time.Duration(minutes) * time.Minute)
		res, err := ix.SyncIfChanged(r.a, r.op, nil, opts)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// Reported by a session as made on its branch, and on main by the first pass.
	first := land("the first change that landed")
	r.hostSession("hub", sidHub1, cwd, "feat/x", "2026-10-07T08:00:00Z", "one", "[feat/x "+first[:10]+"] the first change that landed\n")
	res := tick(5)
	if !res.Ran || !res.GitPass {
		t.Fatalf("the first run is a git pass: %+v", res)
	}
	if kind, _ := commitOutcome(t, ix, first[:10]); kind != "commit-landed" {
		t.Fatalf("%s", kind)
	}
	gitAt := gateMeta(t, ix, metaGitAt)

	// Later, unlanded: not on main at the last pass, then landed from elsewhere.
	unlanded := branchCommit("work on a branch for a while")
	r.hostSession("hub", sidHub2, cwd, "feat/x", "2026-10-07T09:00:00Z", "two", "[feat/x "+unlanded[:10]+"] work on a branch for a while\n")
	res = tick(5)
	if !res.Ran || res.GitPass {
		t.Fatalf("a change between passes: %+v", res)
	}
	if gateMeta(t, ix, metaGitAt) != gitAt {
		t.Error("the git time moved with no pass")
	}
	// The earlier verdict stayed, not "unchecked", though nothing was asked.
	if kind, _ := commitOutcome(t, ix, first[:10]); kind != "commit-landed" {
		t.Errorf("a verdict was lost between passes: %s", kind)
	}
	// Not on main, and no PR lookup: git cannot say it is unmerged (it could be a
	// squash with no PR), so it says it does not know.
	if kind, _ := commitOutcome(t, ix, unlanded[:10]); kind != "commit-unknown" {
		t.Errorf("a new commit is asked about locally: %s", kind)
	}
	// It lands on origin, by subject, as the land step's rebase would. No fetch and
	// no pass yet, so a change now keeps the old verdict.
	land("work on a branch for a while")
	r.hostSession("laharl", sidLah2, "/home/u/code/alone", "main", "2026-10-03T08:00:00Z", "something only laharl did", "more output")
	if res = tick(5); !res.Ran || res.GitPass {
		t.Fatalf("%+v", res)
	}
	if kind, _ := commitOutcome(t, ix, unlanded[:10]); kind != "commit-unknown" {
		t.Errorf("no pass, so no new verdict: %s", kind)
	}
	if res = tick(5); res.Ran {
		t.Errorf("%+v", res)
	}
	// The hour is up: a pass runs with nothing else changed, and fetches.
	res = tick(45)
	if !res.Ran || !res.GitPass {
		t.Fatalf("the hour is up: %+v", res)
	}
	if kind, _ := commitOutcome(t, ix, unlanded[:10]); kind != "commit-landed" {
		t.Errorf("the pass should see what landed: %s", kind)
	}
	if res = tick(5); res.Ran {
		t.Errorf("the pass is recorded: %+v", res)
	}
}

// Optimize is not on the per-tick path.
func TestGateOptimizesDaily(t *testing.T) {
	r, ix := gateRig(t)
	grow := func(extra string) {
		t.Helper()
		r.hostSession("hub", sidHub2, "/home/u/code/solo", "main", "2026-10-05T08:00:00Z", "start", extra)
	}
	tick := func(by time.Duration) GateResult {
		t.Helper()
		r.now = r.now.Add(by)
		res, err := ix.SyncIfChanged(r.a, r.op, nil, GateOptions{LedgerOK: true})
		if err != nil || !res.Ran {
			t.Fatalf("%v %+v", err, res)
		}
		return res
	}
	grow("a")
	if !tick(time.Minute).Optimized {
		t.Fatal("a new index is optimized")
	}
	at := gateMeta(t, ix, metaOptimizeAt)
	grow("ab")
	if tick(time.Hour).Optimized {
		t.Error("optimized within the day")
	}
	if gateMeta(t, ix, metaOptimizeAt) != at {
		t.Error("the time moved")
	}
	grow("abc")
	if !tick(24 * time.Hour).Optimized {
		t.Error("not optimized after a day")
	}
	if gateMeta(t, ix, metaOptimizeAt) == at {
		t.Error("the time did not move")
	}
	// A plain sync optimizes, as it always did.
	at = gateMeta(t, ix, metaOptimizeAt)
	r.now = r.now.Add(time.Minute)
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	if gateMeta(t, ix, metaOptimizeAt) == at {
		t.Error("a plain sync did not optimize")
	}
}

func TestStaleThresholds(t *testing.T) {
	r, ix := gateRig(t)
	rd := NewReader(ix.Path)
	state := func() IndexStatus {
		t.Helper()
		st, err := rd.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	at := func(ago time.Duration) string { return time.Now().UTC().Add(-ago).Format(time.RFC3339) }
	set := func(built, checked string) {
		t.Helper()
		kv := map[string]string{metaBuiltAt: built, metaCheckedAt: checked}
		if err := ix.setMeta(kv); err != nil {
			t.Fatal(err)
		}
		if checked == "" {
			if _, err := ix.run("DELETE FROM meta WHERE key='checked_at';", false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := ix.SyncIfChanged(r.a, r.op, nil, GateOptions{LedgerOK: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name           string
		built, checked time.Duration
		noCheck        bool
		want           string
	}{
		{"old content, looked at just now", 72 * time.Hour, time.Minute, false, "ok"},
		{"old content, looked at 25 minutes ago", 72 * time.Hour, 25 * time.Minute, false, "ok"},
		{"the timer stopped 35 minutes ago", 72 * time.Hour, 35 * time.Minute, false, "stale"},
		{"built just now, last look long ago", time.Minute, 72 * time.Hour, false, "ok"},
		{"both 35 minutes ago", 35 * time.Minute, 35 * time.Minute, false, "stale"},
		{"an hourly timer, an hour ago", time.Hour, 0, true, "ok"},
		{"an hourly timer, two builds missed", 3 * time.Hour, 0, true, "stale"},
	} {
		chk := at(c.checked)
		if c.noCheck {
			chk = ""
		}
		set(at(c.built), chk)
		if st := state(); st.State != c.want {
			t.Errorf("%s: %+v, want %s", c.name, st, c.want)
		}
	}
	// A plain sync (an hourly timer put back) drops the check time, so that it is
	// judged as an hourly index from then on, not stale in half an hour.
	set(at(time.Hour), at(time.Minute))
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	if st := state(); st.CheckedAt != "" {
		t.Errorf("%+v", st)
	}
}

// A file that was pulled from a snapshot by v0.5.6 needs no rebuild: the first tick
// brings it up to date and forgets where it came from.
func TestGateTakesOverAPulledIndex(t *testing.T) {
	r, ix := gateRig(t)
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.run("PRAGMA journal_mode=DELETE; INSERT OR REPLACE INTO meta VALUES('snapshot_key','20261106T202056Z.db.zst'); INSERT OR REPLACE INTO meta VALUES('checked_at','2026-11-06T20:21:00Z');", false); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(time.Minute)
	res, err := ix.SyncIfChanged(r.a, r.op, nil, GateOptions{LedgerOK: true})
	if err != nil || !res.Ran || res.Stats.Indexed != 0 || res.Stats.Unchanged != 4 {
		t.Fatalf("%v %+v", err, res)
	}
	if v := gateMeta(t, ix, "snapshot_key"); v != "" {
		t.Errorf("snapshot_key %s", v)
	}
	if v := gateMeta(t, ix, metaCheckedAt); v != res.CheckedAt {
		t.Errorf("checked_at %s", v)
	}
}

// SQLite would apply a log left beside a new file, so with a log it cannot close,
// a rebuild must not replace the index.
func TestFoldWALRefusesWhileAReaderHoldsTheLog(t *testing.T) {
	foldWait = 300 * time.Millisecond
	t.Cleanup(func() { foldWait = 4 * time.Second })
	r, ix := gateRig(t)
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	sqliteHolding(t, ix.Path, "PRAGMA wal_autocheckpoint=0; UPDATE sessions SET title='from the log';")()
	release := sqliteHolding(t, ix.Path, "BEGIN; SELECT count(*) FROM sessions;")
	if err := ix.foldWAL(); err == nil || !strings.Contains(err.Error(), "not replaced") {
		t.Fatalf("%v", err)
	}
	release()
	if err := ix.foldWAL(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ix.Path + "-wal"); err == nil {
		t.Error("a log is still there")
	}
	rows, err := ix.Query("SELECT count(*) AS n FROM sessions WHERE title='from the log';")
	if err != nil || num(rows[0], "n") == 0 {
		t.Errorf("the log's content did not reach the file: %v %v", err, rows)
	}
}

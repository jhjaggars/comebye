package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hostSession stores a Claude session as the given host would have uploaded it.
// Each extra is the text of a tool result.
func (r *rig) hostSession(host, sid, cwd, branch, start, text string, extra ...string) {
	r.t.Helper()
	line := func(role string, content any) string {
		b, err := json.Marshal(map[string]any{"type": role, "sessionId": sid, "cwd": cwd, "gitBranch": branch, "timestamp": start,
			"message": map[string]any{"role": role, "content": content}})
		if err != nil {
			r.t.Fatal(err)
		}
		return string(b) + "\n"
	}
	body := line("user", text)
	for _, x := range extra {
		body += line("user", []map[string]any{{"type": "tool_result", "content": x}})
	}
	root := filepath.Join(r.dir, "hosts", host, "claude")
	r.write(filepath.Join(root, "-p", sid+".jsonl"), body, false)
	res, err := r.a.forHost(host).RunFinal([]Source{{Kind: "claude", Root: root}})
	if err != nil || len(res.Errors) > 0 {
		r.t.Fatalf("%v %v", err, res.Errors)
	}
}

const (
	sidHub1 = "aaaaaaaa-0000-4000-8000-000000000001"
	sidLah1 = "aaaaaaaa-0000-4000-8000-000000000002"
	sidLah2 = "aaaaaaaa-0000-4000-8000-000000000003"
	sidHub2 = "aaaaaaaa-0000-4000-8000-000000000004"
)

// twoHosts stores a piece of work that moved from the hub to laharl (one branch),
// and one more session on each host.
func twoHosts(r *rig) {
	r.t.Helper()
	cwd := "/home/u/.worktrees/widgets/feat-span"
	r.hostSession("jjaggars-kubevirt", sidHub1, cwd, "feat/span", "2026-10-01T08:00:00Z", "start the span work about aardvarks")
	r.hostSession("laharl", sidLah1, cwd, "feat/span", "2026-10-02T08:00:00Z", "finish the span work about aardvarks")
	r.hostSession("laharl", sidLah2, "/home/u/code/alone", "main", "2026-10-03T08:00:00Z", "something only laharl did")
	r.hostSession("jjaggars-kubevirt", sidHub2, "/home/u/code/solo", "main", "2026-10-04T08:00:00Z", "something only the hub did")
}

func threadsOf(t *testing.T, ix *Index) map[string][]string {
	t.Helper()
	rows, err := ix.Query("SELECT thread_id, host, kind, session_id FROM thread_sessions ORDER BY thread_id, host, session_id;")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, x := range rows {
		id := sget(x, "thread_id")
		out[id] = append(out[id], sget(x, "host")+"/"+sget(x, "session_id"))
	}
	return out
}

func TestAllHostsIndexesEveryHostAndAThreadSpansThem(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	twoHosts(r)
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	st, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true})
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("%v %+v", err, st)
	}
	if st.Sessions != 4 || st.Threads != 3 || strings.Join(st.Hosts, ",") != "jjaggars-kubevirt,laharl" {
		t.Fatalf("%+v", st)
	}
	// Named after its first session: the hub's, the earlier of the two.
	span := threadID(&sess{host: "jjaggars-kubevirt", kind: "claude", id: sidHub1})
	got := threadsOf(t, ix)[span]
	if len(got) != 2 || got[0] != "jjaggars-kubevirt/"+sidHub1 || got[1] != "laharl/"+sidLah1 {
		t.Fatalf("thread %s holds %v (all: %v)", span, got, threadsOf(t, ix))
	}
	rows, err := ix.Query("SELECT host, hosts, n_sessions FROM threads WHERE id=" + q(span))
	if err != nil || len(rows) != 1 || sget(rows[0], "host") != "jjaggars-kubevirt" || sget(rows[0], "hosts") != "jjaggars-kubevirt,laharl" || num(rows[0], "n_sessions") != 2 {
		t.Fatalf("%v %v", err, rows)
	}
	// A thread's view reaches each session on its own host, and raw search joins through it.
	v, err := ix.Thread(span)
	if err != nil || len(v.Sessions) != 2 || v.Sessions[0].Host != "jjaggars-kubevirt" || v.Sessions[1].Host != "laharl" {
		t.Fatalf("%v %+v", err, v)
	}
	views, err := ix.Ask("aardvarks", Filter{}, 5)
	if err != nil || len(views) != 1 || views[0].ID != span {
		t.Fatalf("%v %+v", err, views)
	}
	// A rerun reads nothing again and names the same threads.
	before := threadsOf(t, ix)
	st, err = ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true})
	if err != nil || st.Indexed != 0 || st.Unchanged != 4 {
		t.Fatalf("%v %+v", err, st)
	}
	after := threadsOf(t, ix)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("threads changed on a rerun: %v, then %v", before, after)
	}
}

// A session id can appear on two hosts: they are two sessions.
func TestTheSameSessionIDOnTwoHostsStaysApart(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	r.hostSession("laharl", sidLah1, "/home/u/code/a", "main", "2026-10-01T08:00:00Z", "said on laharl: kiwis")
	r.hostSession("hub", sidLah1, "/home/u/code/b", "main", "2026-10-02T08:00:00Z", "said on the hub: lemons")
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	st, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true})
	if err != nil || st.Sessions != 2 || st.Threads != 2 {
		t.Fatalf("%v %+v", err, st)
	}
	for host, word := range map[string]string{"laharl": "kiwis", "hub": "lemons"} {
		hits, err := ix.Search(word, Filter{Host: host})
		if err != nil || len(hits) != 1 {
			t.Errorf("%s: %v %v", host, err, hits)
		}
	}
}

// These values come from the formula before threads went global, so a change to
// the id of a thread that never left its host fails here.
func TestSingleHostThreadIDsDoNotChange(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	r.fixture()
	if _, err := r.a.Run(r.sources()); err != nil {
		t.Fatal(err)
	}
	want := []string{"t-8c8978bb", "t-97d85974"}
	check := func(what string, ix *Index) {
		t.Helper()
		ts := threadsOf(t, ix)
		for _, id := range want {
			if len(ts[id]) != 1 || !strings.HasPrefix(ts[id][0], "laharl/") {
				t.Errorf("%s: thread %s holds %v (all: %v)", what, id, ts[id], ts)
			}
		}
	}
	alone := &Index{Path: filepath.Join(r.dir, "alone.db")}
	if _, err := alone.Sync(r.a, r.op, nil, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	check("this host only", alone)
	// Other hosts in the index do not disturb them.
	r.hostSession("hub", sidHub2, "/home/u/code/solo", "main", "2026-09-01T08:00:00Z", "an earlier session elsewhere")
	all := &Index{Path: filepath.Join(r.dir, "all.db")}
	if _, err := all.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	check("all hosts", all)
}

// Sessions that started at the same moment still give the thread one first
// session, whatever order their rows happen to be in. A session that grows gets
// its row replaced, which moves it to the end of the table.
func TestThreadIDIsSettledWhenSessionsTie(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	const cwd, start = "/home/u/.worktrees/w/feat-tie", "2026-10-01T08:00:00Z"
	r.hostSession("hub", sidHub1, cwd, "feat/tie", start, "tied")
	r.hostSession("laharl", sidLah1, cwd, "feat/tie", start, "tied")
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	want := threadID(&sess{host: "hub", kind: "claude", id: sidHub1})
	if ts := threadsOf(t, ix); len(ts) != 1 || len(ts[want]) != 2 {
		t.Fatalf("%v", ts)
	}
	r.hostSession("hub", sidHub1, cwd, "feat/tie", start, "tied", "more output")
	st, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true})
	if err != nil || st.Indexed != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	if ts := threadsOf(t, ix); len(ts) != 1 || len(ts[want]) != 2 {
		t.Errorf("%v", ts)
	}
}

func TestAllHostsRespectsTombstonesAndForgetsAnEmptiedHost(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	twoHosts(r)
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	hub := r.a.forHost("jjaggars-kubevirt")
	if _, err := hub.DeleteSession("claude", "-p", sidHub1); err != nil {
		t.Fatal(err)
	}
	st, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true})
	if err != nil || st.Removed != 1 || st.Sessions != 3 {
		t.Fatalf("%v %+v", err, st)
	}
	if hits, err := ix.Search("start", Filter{Host: "jjaggars-kubevirt"}); err != nil || len(hits) != 0 {
		t.Errorf("a deleted session is still found: %v %v", err, hits)
	}
	// The thread that spanned hosts lives on from its other session, under that session's name.
	if got := threadsOf(t, ix)[threadID(&sess{host: "laharl", kind: "claude", id: sidLah1})]; len(got) != 1 {
		t.Errorf("%v", threadsOf(t, ix))
	}
	// The last session of a host goes: it is no longer in v1/, but its rows are in the index.
	if _, err := hub.DeleteSession("claude", "-p", sidHub2); err != nil {
		t.Fatal(err)
	}
	st, err = ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true})
	if err != nil || st.Removed != 1 || st.Sessions != 2 {
		t.Fatalf("%v %+v", err, st)
	}
	rows, err := ix.List(Filter{Host: "jjaggars-kubevirt"})
	if err != nil || len(rows) != 0 {
		t.Errorf("%v %v", err, rows)
	}
}

// failingList makes one prefix's listing fail, as a network blip would.
type failingList struct {
	Backend
	prefix string
}

func (f failingList) List(p string) ([]string, error) {
	if strings.HasPrefix(p, f.prefix) {
		return nil, errors.New("blip")
	}
	return f.Backend.List(p)
}

// One host that cannot be read keeps its rows and is named, and the others go on.
func TestAllHostsSurvivesOneHostFailing(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	twoHosts(r)
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	r.hostSession("laharl", sidHub1, "/home/u/code/more", "main", "2026-10-05T08:00:00Z", "a new one")
	a := *r.a
	a.B = failingList{r.a.B, "v1/jjaggars-kubevirt/"}
	st, err := ix.Sync(&a, r.op, nil, SyncOptions{AllHosts: true})
	if err != nil || strings.Join(st.FailedHosts, ",") != "jjaggars-kubevirt" || st.Indexed != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	if rows, err := ix.List(Filter{Host: "jjaggars-kubevirt"}); err != nil || len(rows) != 2 {
		t.Errorf("its rows should stay: %v %v", err, rows)
	}
}

func TestThreadsHostFilterIsExact(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	r.hostSession("laharl", sidLah1, "/home/u/code/a", "main", "2026-10-01T08:00:00Z", "one")
	r.hostSession("laharl2", sidLah2, "/home/u/code/b", "main", "2026-10-02T08:00:00Z", "two")
	r.hostSession("w1.i-9", sidHub1, "/home/u/code/c", "main", "2026-10-03T08:00:00Z", "three")
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]int{"laharl": 1, "lah": 0, "laharl2": 1, "w1": 1, "w1.i-9": 1, "%": 0, "l_harl": 0} {
		views, err := ix.Threads(Filter{Host: host}, false)
		if err != nil || len(views) != want {
			t.Errorf("--host %s: %d threads, want %d (%v)", host, len(views), want, err)
		}
	}
}

func gitDo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitWorld makes a bare origin and a clone at $HOME/code/widgets, as this
// machine's checkout, with an initial commit on main.
func gitWorld(t *testing.T) (origin, clone string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	origin = filepath.Join(home, "origin.git")
	gitDo(t, home, "init", "-q", "--bare", "-b", "main", origin)
	clone = filepath.Join(home, "code", "widgets")
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	gitDo(t, home, "clone", "-q", origin, clone)
	gitDo(t, clone, "checkout", "-q", "-b", "main")
	gitDo(t, clone, "commit", "-q", "--allow-empty", "-m", "the first commit of all")
	gitDo(t, clone, "push", "-q", "origin", "main")
	return origin, clone
}

// A commit landed from the other host is on origin but not yet known here. The
// fetch makes it read as landed, and the session's path, which on this machine
// happens to be some other repository, is not what gets asked.
func TestGitCheckFetchesAndUsesOnlyThisHostsPaths(t *testing.T) {
	hasSqlite(t)
	origin, clone := gitWorld(t)
	other := filepath.Join(t.TempDir(), "elsewhere")
	gitDo(t, filepath.Dir(other), "clone", "-q", origin, other)
	gitDo(t, other, "commit", "-q", "--allow-empty", "-m", "landed from the hub today")
	sha := gitDo(t, other, "rev-parse", "HEAD")
	gitDo(t, other, "push", "-q", "origin", "HEAD:main")
	if _, err := exec.Command("git", "-C", clone, "cat-file", "-e", sha+"^{commit}").Output(); err == nil {
		t.Fatal("the clone already has the commit")
	}

	// The hub's session ran in a directory that here is a different repository.
	cwd := filepath.Join(filepath.Dir(clone), "..", "trap", ".worktrees", "widgets", "feat-x")
	gitDo(t, filepath.Dir(clone), "init", "-q", "-b", "main", cwd)
	gitDo(t, cwd, "commit", "-q", "--allow-empty", "-m", "an unrelated repository")

	r := newRig(t)
	r.hostSession("hub", sidHub1, cwd, "feat/x", "2026-10-01T08:00:00Z", "land it", "[feat/x "+sha[:10]+"] landed from the hub today\n 1 file changed")
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	st, err := ix.Sync(r.a, r.op, nil, SyncOptions{Git: true, AllHosts: true})
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("%v %+v", err, st)
	}
	rows, err := ix.Query("SELECT kind, source FROM outcomes WHERE kind LIKE 'commit-%';")
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", err, rows)
	}
	if sget(rows[0], "kind") != "commit-landed" || !strings.Contains(sget(rows[0], "source"), "origin/main") {
		t.Errorf("%v", rows[0])
	}
	// The same session as this host's own: its cwd exists, so it is what gets asked.
	g := &gitCtx{host: "hub", enabled: true, cache: map[string]*repoInfo{}, fetched: map[string]bool{}}
	if got := g.repoDir("hub", cwd, "widgets", nil); got != cwd {
		t.Errorf("this host's session: %s", got)
	}
	if got := g.repoDir("laharl", cwd, "widgets", &Link{Repo: cwd}); got != clone {
		t.Errorf("another host's session: %s", got)
	}
	if got := g.repoDir("laharl", cwd, "nothing-here", &Link{Repo: cwd}); got != "" {
		t.Errorf("no checkout: %s", got)
	}
}

func TestGitFetchIsOncePerRepositoryAndAFailureIsReported(t *testing.T) {
	origin, clone := gitWorld(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitDo(t, clone, "worktree", "add", "-q", "-b", "feat/wt", wt)
	var errs []string
	g := &gitCtx{host: "h", enabled: true, cache: map[string]*repoInfo{}, fetched: map[string]bool{}, errs: &errs}
	g.info(clone)
	g.info(wt)
	if len(g.fetched) != 1 || len(errs) != 0 {
		t.Errorf("fetched %v, errors %v", g.fetched, errs)
	}
	// No origin at all is not a fault.
	bare := filepath.Join(t.TempDir(), "noorigin")
	gitDo(t, filepath.Dir(bare), "init", "-q", "-b", "main", bare)
	g.info(bare)
	if len(errs) != 0 {
		t.Errorf("%v", errs)
	}
	// An origin that has gone is, and the check still answers from what it knows.
	if err := os.Rename(origin, origin+".gone"); err != nil {
		t.Fatal(err)
	}
	g = &gitCtx{host: "h", enabled: true, cache: map[string]*repoInfo{}, fetched: map[string]bool{}, errs: &errs}
	if ri := g.info(clone); !ri.ok {
		t.Errorf("%+v", ri)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "git fetch failed") {
		t.Errorf("%v", errs)
	}
}

// builtIndex indexes the two hosts into a private directory.
func builtIndex(t *testing.T, r *rig, name string) *Index {
	t.Helper()
	d := filepath.Join(r.dir, name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	ix := &Index{Path: filepath.Join(d, "index.db")}
	if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
		t.Fatal(err)
	}
	return ix
}

// changeIndex changes what a reader of the index would see.
func changeIndex(t *testing.T, ix *Index) {
	t.Helper()
	if _, err := ix.run("UPDATE sessions SET summary=coalesce(summary,'')||'x' WHERE rowid=(SELECT min(rowid) FROM sessions);", false); err != nil {
		t.Fatal(err)
	}
}

func testPublishPull(t *testing.T, r *rig) {
	t.Helper()
	twoHosts(r)
	ix := builtIndex(t, r, "builder")
	r.now = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

	hub := &Index{Path: filepath.Join(r.dir, "hub", "state", "index.db")}
	if _, err := hub.Pull(r.a, r.op, SnapshotPrefix); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("no snapshot yet: %v", err)
	}
	var keys []string
	for i := 0; i < 3; i++ {
		changeIndex(t, ix)
		res, err := ix.Publish(r.a, SnapshotPrefix)
		if err != nil || res.PruneError != "" || res.Skipped {
			t.Fatalf("%v %+v", err, res)
		}
		keys = append(keys, res.Key)
		r.now = r.now.Add(time.Hour)
	}
	stored, err := snapshots(r.a.B, SnapshotPrefix)
	if err != nil || fmt.Sprint(stored) != fmt.Sprint(keys[1:]) {
		t.Fatalf("keeps the newest two: %v %v", err, stored)
	}
	res, err := hub.Pull(r.a, r.op, SnapshotPrefix)
	if err != nil || res.Key != keys[2] || res.UpToDate || res.Sessions != 4 {
		t.Fatalf("%v %+v", err, res)
	}
	fi, err := os.Stat(hub.Path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", err, fi)
	}
	if di, err := os.Stat(filepath.Dir(hub.Path)); err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", err, di)
	}
	ents, _ := os.ReadDir(filepath.Dir(hub.Path))
	if len(ents) != 1 {
		t.Errorf("left beside the index: %v", ents)
	}
	// It reads like the original, and says where it came from.
	rows, err := hub.Query("PRAGMA journal_mode;")
	if err != nil || sget(rows[0], "journal_mode") != "delete" {
		t.Errorf("%v %v", err, rows)
	}
	if a, b := threadsOf(t, ix), threadsOf(t, hub); fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("%v, %v", a, b)
	}
	st, err := NewReader(hub.Path).Status(context.Background())
	if err != nil || st.Sessions != 4 || st.Snapshot == "" || len(st.Hosts) != 2 {
		t.Errorf("%v %+v", err, st)
	}
	// Nothing new: nothing downloaded.
	if res, err := hub.Pull(r.a, r.op, SnapshotPrefix); err != nil || !res.UpToDate {
		t.Errorf("%v %+v", err, res)
	}
	// The source index stayed as it was: not marked a snapshot, still in its own mode.
	if rows, err := ix.Query("SELECT value FROM meta WHERE key='snapshot_key';"); err != nil || len(rows) != 0 {
		t.Errorf("%v %v", err, rows)
	}
}

func TestPublishAndPullFS(t *testing.T) {
	hasSqlite(t)
	testPublishPull(t, newRig(t)) // the age mode: the object is sealed
}

func TestPublishAndPullFakeS3(t *testing.T) {
	hasSqlite(t)
	r, _ := s3Rig(t)
	testPublishPull(t, r)
}

// A snapshot in the plaintext mode is an ordinary .zst: `zstd -d` reads it.
func TestPlaintextSnapshotIsPlainZstd(t *testing.T) {
	hasSqlite(t)
	r, f := s3Rig(t)
	twoHosts(r)
	res, err := builtIndex(t, r, "b").Publish(r.a, SnapshotPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if body := f.objects[res.Key]; len(body) < 4 || string(body[:4]) != "\x28\xb5\x2f\xfd" {
		t.Errorf("not a zstd frame")
	}
}

// Anything wrong with a download leaves the file that is there.
func TestPullRefusesABadSnapshot(t *testing.T) {
	hasSqlite(t)
	r := newPlainRig(t)
	twoHosts(r)
	ix := builtIndex(t, r, "b")
	r.now = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	pub, err := ix.Publish(r.a, SnapshotPrefix)
	if err != nil {
		t.Fatal(err)
	}
	hub := &Index{Path: filepath.Join(r.dir, "h", "index.db")}
	if _, err := hub.Pull(r.a, r.op, SnapshotPrefix); err != nil {
		t.Fatal(err)
	}
	good, _ := os.ReadFile(hub.Path)
	store, _ := r.a.B.Get(pub.Key)

	next := func(body []byte) {
		t.Helper()
		r.now = r.now.Add(time.Hour)
		key := SnapshotPrefix + r.now.UTC().Format(snapshotTimeFmt) + ".db.zst"
		if err := r.a.B.Put(key, body); err != nil {
			t.Fatal(err)
		}
	}
	flipped := append([]byte{}, store...)
	flipped[len(flipped)/2] ^= 0xff
	cases := map[string][]byte{
		"not zstd":  []byte("nonsense"),
		"truncated": store[:len(store)/2],
		"flipped":   flipped,
	}
	// A valid frame holding things that are not a usable index.
	pack := func(mut string) []byte {
		t.Helper()
		d := t.TempDir()
		c := &Index{Path: filepath.Join(d, "x.db")}
		if _, err := c.run(mut, false); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(c.Path)
		return zstdEnc.EncodeAll(raw, nil)
	}
	cases["another format"] = pack("PRAGMA user_version=99; CREATE TABLE t(a);")
	cases["never finished"] = pack(schema)
	cases["not sqlite"] = zstdEnc.EncodeAll([]byte(strings.Repeat("not a database ", 1000)), nil)
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			next(body)
			if res, err := hub.Pull(r.a, r.op, SnapshotPrefix); err == nil {
				t.Fatalf("accepted: %+v", res)
			}
			now, _ := os.ReadFile(hub.Path)
			if string(now) != string(good) {
				t.Fatal("the index changed")
			}
			ents, _ := os.ReadDir(filepath.Dir(hub.Path))
			if len(ents) != 1 {
				t.Fatalf("left behind: %v", ents)
			}
		})
	}
}

func TestPullRefusesALooseDirectory(t *testing.T) {
	hasSqlite(t)
	r := newPlainRig(t)
	twoHosts(r)
	if _, err := builtIndex(t, r, "b").Publish(r.a, SnapshotPrefix); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(r.dir, "loose")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	hub := &Index{Path: filepath.Join(d, "index.db")}
	if _, err := hub.Pull(r.a, r.op, SnapshotPrefix); err == nil || !strings.Contains(err.Error(), "0700") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(hub.Path); err == nil {
		t.Fatal("wrote the index there")
	}
}

// sqliteHolding leaves a sqlite3 process with the file open. The returned
// function kills it, as a crash would, or at the end of the test.
func sqliteHolding(t *testing.T, path, sql string) (kill func()) {
	t.Helper()
	cmd := exec.Command("sqlite3", path)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	kill = func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
	t.Cleanup(kill)
	fmt.Fprintln(in, sql+" SELECT 'done';")
	buf := make([]byte, 256)
	for n := 0; !strings.Contains(string(buf[:n]), "done"); {
		m, err := out.Read(buf[n:])
		if err != nil {
			t.Fatal(err)
		}
		n += m
	}
	return kill
}

// The hub's own index is in WAL mode. SQLite would apply a log left beside the
// new file to it, so with a log it cannot close, the file must not be replaced.
func TestPullDoesNotApplyTheOldIndexesLog(t *testing.T) {
	hasSqlite(t)
	foldWait = 300 * time.Millisecond
	t.Cleanup(func() { foldWait = 4 * time.Second })
	r := newPlainRig(t)
	twoHosts(r)
	ix := builtIndex(t, r, "b")
	r.now = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	if _, err := ix.Publish(r.a, SnapshotPrefix); err != nil {
		t.Fatal(err)
	}
	r2 := newPlainRig(t)
	r2.hostSession("own", sidHub1, "/home/u/code/o", "main", "2026-10-01T08:00:00Z", "the hub's own old index")
	hub := builtIndex(t, r2, "h")
	// A writer dies, leaving frames in the log. A reader keeps them there.
	sqliteHolding(t, hub.Path, "PRAGMA wal_autocheckpoint=0; UPDATE sessions SET title='from the old log';")()
	if fi, err := os.Stat(hub.Path + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("the test needs a log with frames: %v %v", err, fi)
	}
	release := sqliteHolding(t, hub.Path, "BEGIN; SELECT count(*) FROM sessions;")
	if res, err := hub.Pull(r.a, r.op, SnapshotPrefix); err == nil || !strings.Contains(err.Error(), "not replaced") {
		t.Fatalf("%v %+v", err, res)
	}
	rows, err := hub.Query("SELECT count(*) AS n, min(title) AS title FROM sessions;")
	if err != nil || num(rows[0], "n") != 1 || sget(rows[0], "title") != "from the old log" {
		t.Fatalf("the old index should be intact: %v %v", err, rows)
	}
	// With the reader gone, the log is folded in and the new file goes in with none beside it.
	release()
	sqliteHolding(t, hub.Path, "PRAGMA wal_autocheckpoint=0; UPDATE sessions SET title='from the old log, again';")()
	res, err := hub.Pull(r.a, r.op, SnapshotPrefix)
	if err != nil || res.Sessions != 4 {
		t.Fatalf("%v %+v", err, res)
	}
	for _, s := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(hub.Path + s); err == nil {
			t.Errorf("%s still there", s)
		}
	}
	rows, err = hub.Query("SELECT count(*) AS n FROM sessions WHERE title LIKE 'from the old log%';")
	if err != nil || num(rows[0], "n") != 0 {
		t.Errorf("%v %v", err, rows)
	}
	if rows, err := hub.Query("PRAGMA integrity_check;"); err != nil || sget(rows[0], "integrity_check") != "ok" {
		t.Errorf("%v %v", err, rows)
	}
}

// A first pull over the hub's own WAL-mode index, with the server reading it.
func TestFirstPullOverABusyIndex(t *testing.T) {
	hasSqlite(t)
	r := newPlainRig(t)
	twoHosts(r)
	ix := builtIndex(t, r, "b")
	r.now = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	if _, err := ix.Publish(r.a, SnapshotPrefix); err != nil {
		t.Fatal(err)
	}
	r2 := newPlainRig(t)
	r2.hostSession("own", sidHub1, "/home/u/code/o", "main", "2026-10-01T08:00:00Z", "the hub's own old index")
	hub := builtIndex(t, r2, "h")
	rd := NewReader(hub.Path)
	rd.Timeout = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var failed atomic.Int64
	var firstErr atomic.Value
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			if _, err := rd.Recent(ctx, Filter{}, 0); err != nil && ctx.Err() == nil {
				failed.Add(1)
				firstErr.CompareAndSwap(nil, err.Error())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	res, err := hub.Pull(r.a, r.op, SnapshotPrefix)
	cancel()
	wg.Wait()
	if err != nil || res.Sessions != 4 {
		t.Fatalf("%v %+v", err, res)
	}
	if failed.Load() != 0 {
		t.Errorf("%d queries failed, the first: %v", failed.Load(), firstErr.Load())
	}
}

// The server keeps answering while the file is swapped, repeatedly, with queries
// in flight: every answer is whole, from one snapshot or the other.
func TestReaderKeepsWorkingDuringPulls(t *testing.T) {
	hasSqlite(t)
	r := newPlainRig(t)
	twoHosts(r)
	ix := builtIndex(t, r, "b")
	r.now = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	if _, err := ix.Publish(r.a, SnapshotPrefix); err != nil {
		t.Fatal(err)
	}
	hub := &Index{Path: filepath.Join(r.dir, "h", "index.db")}
	if _, err := hub.Pull(r.a, r.op, SnapshotPrefix); err != nil {
		t.Fatal(err)
	}
	rd := NewReader(hub.Path)
	rd.Timeout = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	var served, failed atomic.Int64
	var firstErr atomic.Value
	note := func(err error) {
		failed.Add(1)
		firstErr.CompareAndSwap(nil, err.Error())
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				res, err := rd.Search(ctx, "aardvarks", Filter{}, "", 0)
				if err != nil {
					if ctx.Err() == nil {
						note(err)
					}
					continue
				}
				if res.Total != 2 {
					note(fmt.Errorf("%d hits", res.Total))
				}
				rec, err := rd.Recent(ctx, Filter{}, 0)
				if err != nil {
					if ctx.Err() == nil {
						note(err)
					}
					continue
				}
				if rec.Total != 4 {
					note(fmt.Errorf("%d recent", rec.Total))
				}
				served.Add(1)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		r.now = r.now.Add(time.Hour)
		changeIndex(t, ix)
		if _, err := ix.Publish(r.a, SnapshotPrefix); err != nil {
			t.Fatal(err)
		}
		if res, err := hub.Pull(r.a, r.op, SnapshotPrefix); err != nil || res.UpToDate {
			t.Fatalf("%v %+v", err, res)
		}
	}
	cancel()
	wg.Wait()
	if failed.Load() != 0 {
		t.Errorf("%d failures during the swaps, the first: %v", failed.Load(), firstErr.Load())
	}
	if served.Load() == 0 {
		t.Error("no query ran during the swaps")
	}
}

func TestSnapshotPrefixCannotReachOtherObjects(t *testing.T) {
	hasSqlite(t)
	r := newPlainRig(t)
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	for _, p := range []string{"v1/", "v1/x/", "tomb/", "notes/", "hosts/", "index", "/index/", "a/../b/", ""} {
		if _, err := ix.Publish(r.a, p); err == nil {
			t.Errorf("prefix %q accepted", p)
		}
		if _, err := NewestSnapshot(r.a.B, p); err == nil {
			t.Errorf("listing prefix %q accepted", p)
		}
	}
}

// Only keys that look like snapshots are candidates, for reading and for pruning.
func TestPruneLeavesOtherObjectsAlone(t *testing.T) {
	hasSqlite(t)
	r := newPlainRig(t)
	twoHosts(r)
	ix := builtIndex(t, r, "b")
	r.now = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	if err := r.a.B.Put(SnapshotPrefix+"zzz-notes.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		changeIndex(t, ix)
		if _, err := ix.Publish(r.a, SnapshotPrefix); err != nil {
			t.Fatal(err)
		}
		r.now = r.now.Add(time.Hour)
	}
	keys, _ := r.a.B.List(SnapshotPrefix)
	if len(keys) != 5 { // two snapshots, two checks, and the other object
		t.Errorf("%v", keys)
	}
	if err := r.a.B.Put(SnapshotPrefix+"zzz-notes.txt", []byte("x")); !errors.Is(err, ErrExists) {
		t.Errorf("the other object went: %v", err)
	}
	if n, err := NewestSnapshot(r.a.B, SnapshotPrefix); err != nil || !strings.HasSuffix(n, "T120000Z.db.zst") && !strings.HasSuffix(n, "T130000Z.db.zst") {
		t.Errorf("%v %s", err, n)
	}
}

// storeFixture stores one stream in two segments on each of two hosts.
func storeFixture(t *testing.T, r *rig) (seg0, seg1 string) {
	t.Helper()
	for _, h := range []string{"hub", "laharl"} {
		a := r.a.forHost(h)
		k0, k1 := segKeyFor(h, "claude", "-p/s.jsonl", 0, 6), segKeyFor(h, "claude", "-p/s.jsonl", 6, 6)
		for k, off := range map[string]int64{k0: 0, k1: 6} {
			body, err := encodeSegment(a.S, h, "claude", "-p/s.jsonl", off, []byte("abcdef"))
			if err != nil {
				t.Fatal(err)
			}
			if err := a.B.Put(k, body); err != nil {
				t.Fatal(err)
			}
		}
	}
	return segKeyFor("hub", "claude", "-p/s.jsonl", 0, 6), segKeyFor("hub", "claude", "-p/s.jsonl", 6, 6)
}

func TestVerifyStore(t *testing.T) {
	for name, mk := range map[string]func(*testing.T) *rig{
		"fs-age": newRig,
		"s3":     func(t *testing.T) *rig { r, _ := s3Rig(t); return r },
	} {
		t.Run(name, func(t *testing.T) {
			r := mk(t)
			seg0, _ := storeFixture(t, r)
			c, err := r.a.VerifyStore(r.op, true)
			if err != nil || !c.OK() || c.Segments != 4 || c.Streams != 2 || c.Bytes != 24 || strings.Join(c.Hosts, ",") != "hub,laharl" {
				t.Fatalf("%v %+v", err, c)
			}
			// Without all hosts, only this one.
			c, err = r.a.VerifyStore(r.op, false)
			if err != nil || !c.OK() || c.Segments != 2 || strings.Join(c.Hosts, ",") != "laharl" {
				t.Fatalf("%v %+v", err, c)
			}
			// A segment moved to another key: its header says otherwise.
			body, _ := r.a.B.Get(seg0)
			moved := segKeyFor("hub", "claude", "-p/other.jsonl", 0, 6)
			r.a.B.Put(moved, body)
			c, _ = r.a.VerifyStore(r.op, true)
			if c.Bad != 1 || !strings.Contains(strings.Join(c.Failures, "\n"), "moved or swapped") {
				t.Errorf("%+v", c)
			}
			r.a.B.Delete(moved)
			// A key that claims another length than the segment holds.
			short, _ := encodeSegment(r.a.S, "hub", "claude", "-p/short.jsonl", 0, []byte("abc"))
			lying := segKeyFor("hub", "claude", "-p/short.jsonl", 0, 9)
			r.a.B.Put(lying, short)
			if c, _ = r.a.VerifyStore(r.op, true); c.Bad != 1 {
				t.Errorf("%+v", c)
			}
			r.a.B.Delete(lying)
			// Junk where a segment should be.
			junk := segKeyFor("hub", "claude", "-p/junk.jsonl", 0, 9)
			r.a.B.Put(junk, []byte("nonsense"))
			c, _ = r.a.VerifyStore(r.op, true)
			if c.Bad != 1 || c.OK() {
				t.Errorf("%+v", c)
			}
			r.a.B.Delete(junk)
			// A gap: the first segment of the stream is gone, then one in the middle.
			r.a.B.Delete(seg0)
			c, _ = r.a.VerifyStore(r.op, true)
			if c.Gaps != 1 || !strings.Contains(strings.Join(c.Failures, "\n"), "bytes 0 to 6 are missing") {
				t.Errorf("%+v", c)
			}
			r.a.B.Put(seg0, body)
			// An overlap, and everything after it still gets looked at.
			over := segKeyFor("hub", "claude", "-p/s.jsonl", 3, 6)
			ob, _ := encodeSegment(r.a.S, "hub", "claude", "-p/s.jsonl", 3, []byte("defabc"))
			r.a.B.Put(over, ob)
			far := segKeyFor("hub", "claude", "-p/s.jsonl", 100, 6)
			fb, _ := encodeSegment(r.a.S, "hub", "claude", "-p/s.jsonl", 100, []byte("abcdef"))
			r.a.B.Put(far, fb)
			c, _ = r.a.VerifyStore(r.op, true)
			if c.Overlaps != 2 || c.Gaps != 1 || c.Bad != 0 {
				t.Errorf("%+v", c)
			}
			r.a.B.Delete(over)
			r.a.B.Delete(far)
			// An object that is no segment at all.
			r.a.B.Put("v1/hub/claude/-p/stray.txt", []byte("x"))
			c, _ = r.a.VerifyStore(r.op, true)
			if c.Unnamed != 1 || c.OK() {
				t.Errorf("%+v", c)
			}
			r.a.B.Delete("v1/hub/claude/-p/stray.txt")
		})
	}
}

// The real segment truncated or flipped in the store is caught.
func TestVerifyStoreCatchesDamage(t *testing.T) {
	r := newPlainRig(t)
	seg0, _ := storeFixture(t, r)
	body, _ := r.a.B.Get(seg0)
	r.a.B.Delete(seg0)
	r.a.B.Put(seg0, body[:len(body)-4])
	c, err := r.a.VerifyStore(r.op, true)
	if err != nil || c.Bad != 1 {
		t.Errorf("%v %+v", err, c)
	}
}

func TestReaderRecent(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	twoHosts(r)
	ix := builtIndex(t, r, "b")
	rd := NewReader(ix.Path)
	ctx := context.Background()
	res, err := rd.Recent(ctx, Filter{Limit: 2}, 0)
	if err != nil || res.Total != 4 || len(res.Hits) != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Hits[0].ID != sidHub2 || res.Hits[1].ID != sidLah2 || res.Hits[0].Host != "jjaggars-kubevirt" || res.Hits[1].Host != "laharl" {
		t.Errorf("not newest first: %+v", res.Hits)
	}
	if res.Hits[0].Repo != "solo" || res.Hits[0].Started == "" {
		t.Errorf("%+v", res.Hits[0])
	}
	if res, err = rd.Recent(ctx, Filter{Limit: 2}, 3); err != nil || len(res.Hits) != 1 || res.Hits[0].ID != sidHub1 {
		t.Errorf("paging: %v %+v", err, res)
	}
	for f, want := range map[*Filter]int{
		{Host: "laharl"}:                   2,
		{Project: "solo"}:                  1,
		{Kind: "pi"}:                       0,
		{Since: "2026-10-03"}:              2,
		{Until: "2026-10-02"}:              1,
		{Host: "laharl", Role: "commands"}: 2,
	} {
		if res, err := rd.Recent(ctx, *f, 0); err != nil || res.Total != want || len(res.Hits) != want {
			t.Errorf("%+v: %v %+v", *f, err, res)
		}
	}
}

// Nothing changed: no upload, but the check is written. A real change uploads.
func TestPublishOnlyWhenChanged(t *testing.T) {
	hasSqlite(t)
	for name, mk := range map[string]func(*testing.T) *rig{
		"fs-age": newRig,
		"s3":     func(t *testing.T) *rig { r, _ := s3Rig(t); return r },
	} {
		t.Run(name, func(t *testing.T) {
			r := mk(t)
			twoHosts(r)
			ix := builtIndex(t, r, "b")
			r.now = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
			first, err := ix.Publish(r.a, SnapshotPrefix)
			if err != nil || first.Skipped || first.Stored == 0 {
				t.Fatalf("%v %+v", err, first)
			}
			step := func(what string, wantSkip bool) PublishResult {
				t.Helper()
				r.now = r.now.Add(time.Hour)
				res, err := ix.Publish(r.a, SnapshotPrefix)
				if err != nil || res.Skipped != wantSkip {
					t.Fatalf("%s: %v %+v", what, err, res)
				}
				c, err := LastCheck(r.a.B, SnapshotPrefix)
				if err != nil || c == nil || c.CheckedAt != r.now.UTC().Format(time.RFC3339) || c.Snapshot != res.Key {
					t.Fatalf("%s: %v %+v", what, err, c)
				}
				return res
			}
			// Rebuilding the index rewrites the thread tables and the build time
			// without changing anything a reader sees.
			if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
				t.Fatal(err)
			}
			res := step("a rerun of the index", true)
			if res.Key != first.Key || res.Stored != 0 {
				t.Errorf("%+v", res)
			}
			if keys, _ := snapshots(r.a.B, SnapshotPrefix); len(keys) != 1 {
				t.Errorf("%v", keys)
			}
			// A session that grows.
			r.hostSession("laharl", sidLah1, "/home/u/.worktrees/widgets/feat-span", "feat/span", "2026-10-02T08:00:00Z", "finish the span work about aardvarks", "more output")
			if _, err := ix.Sync(r.a, r.op, nil, SyncOptions{AllHosts: true}); err != nil {
				t.Fatal(err)
			}
			step("a session grew", false)
			step("then nothing again", true)
			// A summary, and a thread's outcome, with no session growing.
			if _, err := ix.run("UPDATE sessions SET summary='new' WHERE id="+q(sidLah2)+";", false); err != nil {
				t.Fatal(err)
			}
			step("a summary", false)
			step("then nothing again", true)
			if _, err := ix.run("INSERT INTO outcomes VALUES('t-x','doc','r','text','src','');", false); err != nil {
				t.Fatal(err)
			}
			step("an outcome", false)
			// Checks are pruned too.
			keys, _ := r.a.B.List(SnapshotPrefix)
			if len(keys) != 2*snapshotKeep {
				t.Errorf("%v", keys)
			}
			// The snapshot the last check names has gone: the content is uploaded again.
			last, _ := LastCheck(r.a.B, SnapshotPrefix)
			if err := r.a.B.Delete(last.Snapshot); err != nil {
				t.Fatal(err)
			}
			step("the snapshot went missing", false)
		})
	}
}

// A failure reading the last check is not "changed": nothing is uploaded.
func TestPublishFailsWhenTheLastCheckCannotBeRead(t *testing.T) {
	hasSqlite(t)
	r := newPlainRig(t)
	twoHosts(r)
	ix := builtIndex(t, r, "b")
	r.now = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	a := *r.a
	a.B = failingList{r.a.B, SnapshotPrefix}
	if res, err := ix.Publish(&a, SnapshotPrefix); err == nil {
		t.Fatalf("%+v", res)
	}
	if keys, _ := r.a.B.List(SnapshotPrefix); len(keys) != 0 {
		t.Errorf("%v", keys)
	}
}

// Old content with a live indexer is fresh. When the indexer stops, it goes stale.
func TestHeartbeatKeepsAnUnchangedCopyFresh(t *testing.T) {
	hasSqlite(t)
	r := newPlainRig(t)
	twoHosts(r)
	ix := builtIndex(t, r, "b")
	now := time.Now().UTC()
	// The content was built three days ago, and every check since found it the same.
	old := now.Add(-72 * time.Hour).Format(time.RFC3339)
	if _, err := ix.run("INSERT OR REPLACE INTO meta VALUES('built_at',"+q(old)+");", false); err != nil {
		t.Fatal(err)
	}
	r.now = now.Add(-72 * time.Hour)
	first, err := ix.Publish(r.a, SnapshotPrefix)
	if err != nil {
		t.Fatal(err)
	}
	hub := &Index{Path: filepath.Join(r.dir, "h", "index.db")}
	if res, err := hub.Pull(r.a, r.op, SnapshotPrefix); err != nil || res.UpToDate {
		t.Fatalf("%v %+v", err, res)
	}
	rd := NewReader(hub.Path)
	status := func() IndexStatus {
		t.Helper()
		st, err := rd.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := status(); st.State != "stale" || st.BuiltAt != old {
		t.Fatalf("no check for three days: %+v", st)
	}
	r.now = now.Add(-30 * time.Minute)
	if res, err := ix.Publish(r.a, SnapshotPrefix); err != nil || !res.Skipped || res.Key != first.Key {
		t.Fatalf("%v %+v", err, res)
	}
	res, err := hub.Pull(r.a, r.op, SnapshotPrefix)
	if err != nil || !res.UpToDate || res.Stored != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	st := status()
	if st.State != "ok" || st.BuiltAt != old || st.CheckedAt != r.now.Format(time.RFC3339) || st.Snapshot == "" {
		t.Errorf("a fresh check should keep it ok, and the content's age should stay: %+v", st)
	}
	if ents, _ := os.ReadDir(filepath.Dir(hub.Path)); len(ents) != 1 {
		t.Errorf("left beside the index: %v", ents)
	}
	// A new copy also takes the check.
	hub2 := &Index{Path: filepath.Join(r.dir, "h2", "index.db")}
	if _, err := hub2.Pull(r.a, r.op, SnapshotPrefix); err != nil {
		t.Fatal(err)
	}
	if st, _ := NewReader(hub2.Path).Status(context.Background()); st.State != "ok" {
		t.Errorf("%+v", st)
	}
	// The indexer stops: later pulls find nothing newer, and the time runs out.
	r.now = now.Add(-3 * time.Hour)
	if _, err := hub.run("INSERT OR REPLACE INTO meta VALUES('checked_at',"+q(r.now.Format(time.RFC3339))+");", false); err != nil {
		t.Fatal(err)
	}
	if st := status(); st.State != "stale" {
		t.Errorf("%+v", st)
	}
}

// The fingerprint leaves out the build time and sees every table a reader reads.
func TestFingerprint(t *testing.T) {
	hasSqlite(t)
	r := newPlainRig(t)
	twoHosts(r)
	ix := builtIndex(t, r, "b")
	base, err := ix.fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.run("INSERT OR REPLACE INTO meta VALUES('built_at','2000-01-01T00:00:00Z');", false); err != nil {
		t.Fatal(err)
	}
	if fp, _ := ix.fingerprint(); fp != base {
		t.Error("the build time changed it")
	}
	for name, sql := range map[string]string{
		"session bytes": "UPDATE sessions SET n_bytes=n_bytes+1 WHERE rowid=1;",
		"handoff link":  "UPDATE sessions SET link_sig='new' WHERE rowid=1;",
		"thread":        "UPDATE threads SET verdict='landed' WHERE rowid=1;",
		"membership":    "DELETE FROM thread_sessions WHERE rowid=1;",
		"outcome":       "INSERT INTO outcomes VALUES('t','k','r','x','s','');",
		"a session":     "DELETE FROM sessions WHERE rowid=2;",
	} {
		before, _ := ix.fingerprint()
		if _, err := ix.run(sql, false); err != nil {
			t.Fatal(err)
		}
		if fp, _ := ix.fingerprint(); fp == before {
			t.Errorf("%s did not change it", name)
		}
	}
}

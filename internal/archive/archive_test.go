package archive

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	sidA = "11111111-1111-4111-8111-111111111111"
	sidB = "22222222-2222-4222-8222-222222222222"
)

type rig struct {
	t        *testing.T
	claude   string
	pi       string
	dir      string
	a        *Archiver
	op       Opener
	now      time.Time
	identity string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	d := t.TempDir()
	ident, rcpt, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSealer(rcpt)
	if err != nil {
		t.Fatal(err)
	}
	o, err := NewOpener(ident)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewFSBackend(filepath.Join(d, "store"))
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, claude: filepath.Join(d, "claude"), pi: filepath.Join(d, "pi"), dir: d, op: o, identity: ident, now: time.Now()}
	r.a = &Archiver{Host: "laharl", B: b, S: s, MaxSegment: 64 << 10, IdleAfter: time.Minute, Now: func() time.Time { return r.now }}
	return r
}

func (r *rig) sources() []Source {
	return []Source{{Kind: "claude", Root: r.claude}, {Kind: "pi", Root: r.pi}}
}

func (r *rig) write(path string, data string, appendTo bool) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		r.t.Fatal(err)
	}
	f.WriteString(data)
	f.Close()
}

func (r *rig) old(path string) { // make a file look idle
	old := r.now.Add(-time.Hour)
	os.Chtimes(path, old, old)
}

func claudeLine(typ, role, text, ts string) string {
	return fmt.Sprintf(`{"type":%q,"sessionId":%q,"cwd":"/home/u/.worktrees/comebye/feat-x","gitBranch":"feat/x","timestamp":%q,"version":"2.1.0","message":{"role":%q,"content":%q}}`+"\n", typ, sidA, ts, role, text)
}

func (r *rig) fixture() (mainA, subA, pi string) {
	mainA = filepath.Join(r.claude, "-proj", sidA+".jsonl")
	r.write(mainA,
		`{"type":"ai-title","sessionId":"`+sidA+`","aiTitle":"Fix the kubevirt console cert"}`+"\n"+
			claudeLine("user", "user", "please fix the kubevirt console certificate rotation", "2026-10-03T10:00:00Z")+
			claudeLine("assistant", "assistant", "I will look at the cert controller", "2026-10-03T10:01:00Z")+
			`{"type":"user","sessionId":"`+sidA+`","cwd":"/home/u/.worktrees/comebye/feat-x","gitBranch":"feat/x","timestamp":"2026-10-03T10:02:00Z","message":{"role":"user","content":[{"type":"tool_result","content":"[feat/x abc1234] Rotate console cert on expiry\n 1 file changed"}]}}`+"\n", false)
	r.old(mainA)
	subA = filepath.Join(r.claude, "-proj", sidA, "subagents", "agent-a1b2.jsonl")
	r.write(subA, claudeLine("assistant", "assistant", "subagent found the rotation bug in webhook", "2026-10-03T10:01:30Z"), false)
	r.old(subA)
	r.write(filepath.Join(r.claude, "-proj", sidA, "tool-results", "x.txt"), "big output", false)
	r.write(filepath.Join(r.claude, "-proj", "memory", "MEMORY.md"), "not a session", false)
	pi = filepath.Join(r.pi, "--home-u-code-foo--", "2026-10-05T09-00-00-000Z_"+sidB+".jsonl")
	r.write(pi,
		`{"type":"session","version":3,"id":"`+sidB+`","timestamp":"2026-10-05T09:00:00Z","cwd":"/home/u/code/foo"}`+"\n"+
			`{"type":"message","timestamp":"2026-10-05T09:00:05Z","message":{"role":"user","content":[{"type":"text","text":"investigate the flaky scheduler test"}]}}`+"\n"+
			`{"type":"message","timestamp":"2026-10-05T09:00:09Z","message":{"role":"assistant","content":[{"type":"text","text":"found a race in the scheduler"},{"type":"toolCall","name":"bash","arguments":{"command":"git push origin main"}}]}}`+"\n"+
			`{"type":"message","timestamp":"2026-10-05T09:00:10Z","message":{"role":"toolResult","toolCallId":"1","content":[{"type":"text","text":"   9f8e7d6..0a1b2c3  main -> main"}]}}`+"\n", false)
	r.old(pi)
	return
}

func restoreString(t *testing.T, r *rig, kind, stream string) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := StreamTo(&b, r.a.B, r.op, "laharl", kind, stream); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestRunRestoreAndRerun(t *testing.T) {
	r := newRig(t)
	mainA, _, _ := r.fixture()
	res, err := r.a.Run(r.sources())
	if err != nil || len(res.Errors) > 0 {
		t.Fatalf("run: %v %v", err, res.Errors)
	}
	// main, subagent, tool-result, pi; MEMORY.md is not a session file.
	if res.Files != 4 || res.Updated != 4 || res.Segments != 4 {
		t.Fatalf("first run: %+v", res)
	}
	want, _ := os.ReadFile(mainA)
	if got := restoreString(t, r, "claude", "-proj/"+sidA+".jsonl"); got != string(want) {
		t.Fatal("restored bytes differ")
	}
	vr, _ := r.a.Verify(r.op, r.sources())
	if vr.Matched != 4 || vr.Mismatched != 0 || vr.Missing != 0 {
		t.Fatalf("verify: %+v", vr)
	}
	// Rerun changes nothing.
	res2, _ := r.a.Run(r.sources())
	if res2.Segments != 0 || res2.Updated != 0 {
		t.Fatalf("rerun wrote: %+v", res2)
	}
	// Append: exactly one new segment, starting where the last ended; older
	// segment objects are untouched.
	before, _ := r.a.B.List("v1/")
	r.write(mainA, claudeLine("user", "user", "second prompt", "2026-10-03T11:00:00Z"), true)
	r.old(mainA)
	res3, _ := r.a.Run(r.sources())
	if res3.Segments != 1 {
		t.Fatalf("append: %+v", res3)
	}
	after, _ := r.a.B.List("v1/")
	if len(after) != len(before)+1 {
		t.Fatalf("%d -> %d objects", len(before), len(after))
	}
	have := map[string]bool{}
	for _, k := range after {
		have[k] = true
	}
	for _, k := range before {
		if !have[k] {
			t.Fatalf("segment %s disappeared", k)
		}
	}
	want, _ = os.ReadFile(mainA)
	if got := restoreString(t, r, "claude", "-proj/"+sidA+".jsonl"); got != string(want) {
		t.Fatal("restored bytes differ after append")
	}
}

func TestSplitsLargeFilesAndHoldsBackPartialLine(t *testing.T) {
	r := newRig(t)
	p := filepath.Join(r.claude, "-proj", sidA+".jsonl")
	var sb strings.Builder
	for i := 0; i < 3000; i++ {
		sb.WriteString(claudeLine("user", "user", strings.Repeat("x", 100)+fmt.Sprint(i), "2026-10-03T10:00:00Z"))
	}
	body := sb.String()
	r.write(p, body+`{"type":"user","partial`, false) // unterminated tail, just written
	if _, err := r.a.Run(r.sources()); err != nil {
		t.Fatal(err)
	}
	got := restoreString(t, r, "claude", "-proj/"+sidA+".jsonl")
	if got != body {
		t.Fatalf("a fresh unterminated line was archived (%d bytes vs %d)", len(got), len(body))
	}
	keys, _ := r.a.B.List("v1/")
	if len(keys) < 4 {
		t.Fatalf("expected several segments, got %d", len(keys))
	}
	// Once the file is idle the tail follows, byte-exactly.
	r.old(p)
	r.a.Run(r.sources())
	if got := restoreString(t, r, "claude", "-proj/"+sidA+".jsonl"); got != body+`{"type":"user","partial` {
		t.Fatal("tail not archived once idle")
	}
	// And a final flush ships it even when fresh.
	r.write(p, "more", true)
	if _, err := r.a.Flush(Source{Kind: "claude", Root: r.claude}, p); err != nil {
		t.Fatal(err)
	}
	if got := restoreString(t, r, "claude", "-proj/"+sidA+".jsonl"); !strings.HasSuffix(got, "partialmore") {
		t.Fatal("flush did not ship the unterminated tail")
	}
}

func TestShrunkFileIsNeverRewritten(t *testing.T) {
	r := newRig(t)
	mainA, _, _ := r.fixture()
	r.a.Run(r.sources())
	keys, _ := r.a.B.List("v1/")
	r.write(mainA, "short\n", false)
	res, _ := r.a.Run(r.sources())
	if res.Shrunk != 1 || res.Segments != 0 {
		t.Fatalf("%+v", res)
	}
	after, _ := r.a.B.List("v1/")
	if len(after) != len(keys) {
		t.Fatal("objects changed")
	}
	vr, _ := r.a.Verify(r.op, r.sources())
	if vr.Mismatched != 1 {
		t.Fatalf("verify should flag the rewritten file: %+v", vr)
	}
}

func TestTamperAndWrongKey(t *testing.T) {
	r := newRig(t)
	r.fixture()
	r.a.Run(r.sources())
	keys, _ := r.a.B.List("v1/laharl/claude/-proj/" + sidA + ".jsonl/")
	if len(keys) != 1 {
		t.Fatal(keys)
	}
	ident2, _, _ := GenerateIdentity()
	o2, _ := NewOpener(ident2)
	if _, err := StreamTo(&bytes.Buffer{}, r.a.B, o2, "laharl", "claude", "-proj/"+sidA+".jsonl"); err == nil {
		t.Fatal("wrong key decrypted")
	}
	// Moving a segment to another offset fails authentication.
	body, _ := r.a.B.Get(keys[0])
	n, _ := parseSegKey(keys[0])
	n.Off = 99
	if _, err := decodeSegment(r.op, n, body); err == nil {
		t.Fatal("segment accepted at a different offset")
	}
	// A flipped bit fails.
	body[len(body)-20] ^= 1
	n, _ = parseSegKey(keys[0])
	if _, err := decodeSegment(r.op, n, body); err == nil {
		t.Fatal("tampered segment accepted")
	}
}

func TestBackendCreateOnlyAndKeys(t *testing.T) {
	b, _ := NewFSBackend(t.TempDir())
	if err := b.Put("a/b", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := b.Put("a/b", []byte("2")); !errors.Is(err, ErrExists) {
		t.Fatalf("overwrite allowed: %v", err)
	}
	got, _ := b.Get("a/b")
	if string(got) != "1" {
		t.Fatal("object changed")
	}
	for _, k := range []string{"", "/a", "a/../b", "a//b", ".tmp/x"} {
		if err := b.Put(k, nil); err == nil {
			t.Fatalf("key %q accepted", k)
		}
	}
	ks, _ := b.List("a/")
	if len(ks) != 1 || ks[0] != "a/b" {
		t.Fatal(ks)
	}
}

func TestDeleteSessionStaysDeleted(t *testing.T) {
	r := newRig(t)
	r.fixture()
	r.a.Run(r.sources())
	n, err := r.a.DeleteSession("claude", "-proj", sidA)
	if err != nil || n != 3 {
		t.Fatalf("deleted %d segments: %v", n, err)
	}
	res, _ := r.a.Run(r.sources())
	if res.Segments != 0 || res.Tombstoned != 3 {
		t.Fatalf("deleted session was archived again: %+v", res)
	}
	keys, _ := r.a.B.List("v1/laharl/claude/")
	if len(keys) != 0 {
		t.Fatalf("left %v", keys)
	}
	if keys, _ := r.a.B.List("v1/laharl/pi/"); len(keys) != 1 {
		t.Fatal("other session touched")
	}
}

func TestSessionOf(t *testing.T) {
	cases := []struct {
		kind, rel string
		slug, sid string
		ok        bool
	}{
		{"claude", "-p/" + sidA + ".jsonl", "-p", sidA, true},
		{"claude", "-p/" + sidA + "/subagents/agent-1.jsonl", "-p", sidA, true},
		{"claude", "-p/" + sidA + "/tool-results/a.txt", "-p", sidA, true},
		{"claude", "-p/memory/MEMORY.md", "", "", false},
		{"claude", "-p/sessions-index.json", "", "", false},
		{"pi", "--x--/2026_" + sidB + ".jsonl", "--x--", "2026_" + sidB, true},
		{"pi", "--x--/note.txt", "", "", false},
	}
	for _, c := range cases {
		slug, sid, ok := SessionOf(c.kind, c.rel)
		if slug != c.slug || sid != c.sid || ok != c.ok {
			t.Errorf("%s %s: %q %q %v", c.kind, c.rel, slug, sid, ok)
		}
	}
}

func hasSqlite(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("no sqlite3")
	}
}

func TestIndexSearchLinksCommitsSummaryRebuildDelete(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	r.fixture()
	r.a.Run(r.sources())
	ix := &Index{Path: filepath.Join(r.dir, "index.db")}
	links := []Link{{ID: "h-1", Host: "laharl", AgentID: "laharl/w1:p1", RepoName: "comebye", Branch: "feat/x", Worktree: "/home/u/.worktrees/comebye/feat-x", Ref: "BL-7", Brief: "Fix the console cert", CreatedAt: "2026-10-03T09:59:00Z"}}
	st, err := ix.Sync(r.a, r.op, links, SyncOptions{})
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("%v %v", err, st.Errors)
	}
	if st.Sessions != 2 || st.Indexed != 2 {
		t.Fatalf("%+v", st)
	}
	// Re-sync with nothing new indexes nothing.
	st, _ = ix.Sync(r.a, r.op, links, SyncOptions{})
	if st.Indexed != 0 || st.Unchanged != 2 {
		t.Fatalf("%+v", st)
	}
	rows, err := ix.Search("rotation webhook", Filter{})
	if err != nil || len(rows) != 1 || rows[0]["id"] != sidA {
		t.Fatalf("subagent text not found: %v %v", rows, err)
	}
	rows, _ = ix.Search("scheduler race", Filter{Project: "foo"})
	if len(rows) != 1 || !strings.HasSuffix(fmt.Sprint(rows[0]["id"]), sidB) {
		t.Fatalf("%v", rows)
	}
	if rows, _ = ix.Search("scheduler", Filter{Project: "comebye"}); len(rows) != 0 {
		t.Fatal("project filter ignored")
	}
	if rows, _ = ix.Search("console cert", Filter{Ledger: "h-1"}); len(rows) != 1 {
		t.Fatalf("handoff text/link: %v", rows)
	}
	if rows, err = ix.Search(`weird "quote' (paren`, Filter{}); err != nil {
		t.Fatalf("query with odd characters failed: %v", err)
	}
	s, cs, err := ix.Show(sidA[:8])
	if err != nil || s["branch"] != "feat/x" || s["ledger_id"] != "h-1" || s["ref"] != "BL-7" || s["repo"] != "comebye" || s["title"] != "Fix the kubevirt console cert" {
		t.Fatalf("%v %v", s, err)
	}
	if len(cs) != 1 || cs[0]["sha"] != "abc1234" || cs[0]["rel"] != "made" {
		t.Fatalf("commits: %v", cs)
	}
	by, _ := ix.ByCommit("abc1234")
	if len(by) != 1 || by[0]["id"] != sidA {
		t.Fatalf("by commit: %v", by)
	}
	if by, _ = ix.ByCommit("0a1b2c3"); len(by) != 1 || by[0]["rel"] != "pushed" {
		t.Fatalf("pushed commit: %v", by)
	}
	lst, _ := ix.List(Filter{Since: "2026-10-01", Until: "2026-10-04"})
	if len(lst) != 1 || lst[0]["id"] != sidA {
		t.Fatalf("date filter: %v", lst)
	}
	// Summary survives a full rebuild from the archive.
	if err := r.a.SetSummary("claude", sidA, "Rotated the console cert", "test"); err != nil {
		t.Fatal(err)
	}
	os.Remove(ix.Path)
	st, err = ix.Sync(r.a, r.op, links, SyncOptions{})
	if err != nil || st.Indexed != 2 {
		t.Fatalf("rebuild: %+v %v", st, err)
	}
	s, _, _ = ix.Show(sidA)
	if s["summary"] != "Rotated the console cert" {
		t.Fatalf("summary lost: %v", s["summary"])
	}
	// Deleting a session removes it from archive and index.
	if _, err := r.a.DeleteSession("claude", "-proj", sidA); err != nil {
		t.Fatal(err)
	}
	st, _ = ix.Sync(r.a, r.op, links, SyncOptions{})
	if st.Removed != 1 {
		t.Fatalf("%+v", st)
	}
	if rows, _ = ix.Search("rotation", Filter{}); len(rows) != 0 {
		t.Fatal("deleted session still searchable")
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Sessions on the same branch are one piece of work; its outcome comes from
// the ledger and git, and a descriptive question finds it.
func TestThreadsOutcomesAndAsk(t *testing.T) {
	hasSqlite(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	r := newRig(t)
	repo := filepath.Join(r.dir, "code", "widgets")
	os.MkdirAll(repo, 0o755)
	gitIn(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o644)
	gitIn(t, repo, "add", "a.txt")
	gitIn(t, repo, "commit", "-q", "-m", "Rotate the console certificate before expiry")
	landed := gitIn(t, repo, "rev-parse", "--short", "HEAD")

	mk := func(sid, ts, branch, text string, extra string) {
		cwd := repo
		if sid == "44444444-4444-4444-8444-444444444444" {
			cwd = filepath.Join(r.dir, "elsewhere")
		}
		r.write(filepath.Join(r.claude, "-w", sid+".jsonl"),
			fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"gitBranch":%q,"timestamp":%q,"message":{"role":"user","content":%q}}`+"\n", sid, cwd, branch, ts, text)+extra, false)
		r.old(filepath.Join(r.claude, "-w", sid+".jsonl"))
	}
	res := fmt.Sprintf(`{"type":"user","sessionId":"x","cwd":%q,"gitBranch":"feat/cert","timestamp":"2026-10-03T10:30:00Z","message":{"role":"user","content":[{"type":"tool_result","content":"[feat/cert %s] Rotate the console certificate before expiry\n"}]}}`+"\n", repo, landed)
	mk(sidA, "2026-10-03T10:00:00Z", "feat/cert", "work on the certificate rotation for the console", res)
	mk(sidB, "2026-10-09T10:00:00Z", "feat/cert", "follow up: we decided to go with a webhook for rotation", "")
	other := "44444444-4444-4444-8444-444444444444"
	mk(other, "2026-10-04T10:00:00Z", "feat/other", "benchmark the scheduler", "")
	r.a.Run(r.sources())
	links := []Link{{ID: "h-9", Host: "laharl", Repo: repo, RepoName: "widgets", Branch: "feat/cert", Worktree: repo, Brief: "Rotate the console certificate", CreatedAt: "2026-10-03T09:59:00Z",
		Status: "finished", FinalReport: "## Summary\nRotation now happens a week before expiry.\n\n## Changes\n- x", ReportSource: "explicit", FinishedAt: "2026-10-03T12:00:00Z"}}
	ix := &Index{Path: filepath.Join(r.dir, "i.db")}
	st, err := ix.Sync(r.a, r.op, links, SyncOptions{Git: true})
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("%v %v", err, st.Errors)
	}
	if st.Threads != 2 {
		t.Fatalf("want 2 threads (cert work, scheduler), got %d", st.Threads)
	}
	vs, err := ix.Ask("In the past we worked on rotating the console certificate and I forgot what the outcome was", Filter{}, 3)
	if err != nil || len(vs) == 0 {
		t.Fatalf("%v %v", vs, err)
	}
	v := vs[0]
	if len(v.Sessions) != 2 || v.Verdict != "landed" || !strings.Contains(v.Outcome, "on main") {
		t.Fatalf("thread: %+v", v)
	}
	var sawReport, sawDecision, sawCommit bool
	for _, o := range v.Outcomes {
		switch {
		case o.Kind == "report" && strings.Contains(o.Text, "week before expiry") && strings.Contains(o.Source, "ledger"):
			sawReport = true
		case o.Kind == "decision" && strings.Contains(o.Source, "heuristic"):
			sawDecision = true
		case o.Kind == "commit-landed" && strings.HasPrefix(o.Ref, landed) && strings.HasPrefix(o.Source, "git"):
			sawCommit = true
		}
	}
	if !sawReport || !sawDecision || !sawCommit {
		t.Fatalf("outcomes missing (report %v decision %v commit %v): %+v", sawReport, sawDecision, sawCommit, v.Outcomes)
	}
	if vs, _ = ix.Ask("scheduler benchmark", Filter{}, 3); len(vs) == 0 || vs[0].Verdict != "unknown" {
		t.Fatalf("%+v", vs)
	}
	if vs, _ = ix.Ask("console certificate", Filter{Until: "2026-10-01"}, 3); len(vs) != 0 {
		t.Fatal("date filter ignored")
	}
	if _, err := ix.Ask("what did we do in the past", Filter{}, 3); err == nil {
		t.Fatal("a question with no topic words should say so")
	}
	// A summary written later is searched and shown, and survives a rebuild.
	r.a.SetSummary("claude", sidA, "Moved certificate renewal to a webhook driven controller", "test")
	os.Remove(ix.Path)
	ix.Sync(r.a, r.op, links, SyncOptions{Git: true})
	if vs, _ = ix.Ask("webhook driven controller", Filter{}, 3); len(vs) == 0 || vs[0].Summary == "" {
		t.Fatalf("summary not used: %+v", vs)
	}
	if td, _ := ix.Threads(Filter{}, true); len(td) != 1 {
		t.Fatalf("unsummarized threads: %d", len(td))
	}
}

func TestMultipleRecipientsAndRewrap(t *testing.T) {
	r := newRig(t)
	r.fixture()
	// Old segments are written for identity A only (the rig's).
	r.a.Run(r.sources())
	oldKeys, _ := r.a.B.List("v1/laharl/claude/")

	// Identity B joins later: the recipient list becomes A+B for new segments.
	idB, rcptB, _ := GenerateIdentity()
	opB, _ := NewOpener(idB)
	rcptA, _ := RecipientsOf(r.identity)
	both, err := NewSealer(rcptA + "," + rcptB)
	if err != nil {
		t.Fatal(err)
	}
	r.a.S = both
	mainA := filepath.Join(r.claude, "-proj", sidA+".jsonl")
	r.write(mainA, claudeLine("user", "user", "later prompt", "2026-10-04T10:00:00Z"), true)
	r.old(mainA)
	r.a.Run(r.sources())
	stream := "-proj/" + sidA + ".jsonl"
	// A reads everything; B reads only the new segment.
	if got := restoreString(t, r, "claude", stream); !strings.Contains(got, "later prompt") {
		t.Fatal("A cannot read")
	}
	if _, err := StreamTo(&bytes.Buffer{}, r.a.B, opB, "laharl", "claude", stream); err == nil {
		t.Fatal("B read segments written before it was a recipient")
	}
	newKeys, _ := r.a.B.List("v1/laharl/claude/" + stream + "/")
	var newest string
	for _, k := range newKeys {
		newest = k
	}
	body, _ := r.a.B.Get(newest)
	n, _ := parseSegKey(newest)
	if _, err := decodeSegment(opB, n, body); err != nil {
		t.Fatalf("B cannot read the segment written for A+B: %v", err)
	}
	_ = oldKeys

	// Rewrap everything to B alone, into a second store; the source is unchanged.
	dstB, _ := NewFSBackend(filepath.Join(r.dir, "store2"))
	sealB, _ := NewSealer(rcptB)
	if err := r.a.SetSummary("claude", sidA, "a note", "t"); err != nil {
		t.Fatal(err)
	}
	st, err := r.a.Rewrap(r.op, dstB, sealB)
	if err != nil || st.Segments < 5 || st.Notes != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	if st2, _ := r.a.Rewrap(r.op, dstB, sealB); st2.Segments != 0 || st2.Skipped != st.Segments {
		t.Fatalf("rewrap is not resumable: %+v", st2)
	}
	moved := &Archiver{Host: "laharl", B: dstB, S: sealB}
	vr, _ := moved.Verify(opB, r.sources())
	if vr.Mismatched != 0 || vr.Matched != 4 {
		t.Fatalf("verify against the rewrapped store: %+v", vr)
	}
	if _, err := StreamTo(&bytes.Buffer{}, dstB, r.op, "laharl", "claude", stream); err == nil {
		t.Fatal("the old identity still reads the rewrapped store")
	}
	if got := restoreString(t, r, "claude", stream); !strings.Contains(got, "later prompt") {
		t.Fatal("source store changed")
	}
}

func TestSSHRecipient(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	d := t.TempDir()
	key := filepath.Join(d, "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen: %v %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	priv, _ := os.ReadFile(key)
	s, err := NewSealer(string(pub))
	if err != nil {
		t.Fatal(err)
	}
	o, err := NewOpener(string(priv))
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := s.Seal([]byte("hello"))
	got, err := o.Open(blob)
	if err != nil || string(got) != "hello" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := NewSealer("not-a-key"); err == nil {
		t.Fatal("bad recipient accepted")
	}
}

func newPlainRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.a.S = NoneSealer{}
	r.op = NoneSealer{}
	return r
}

func TestPlaintextModeRoundTrip(t *testing.T) {
	r := newPlainRig(t)
	mainA, _, _ := r.fixture()
	res, err := r.a.Run(r.sources())
	if err != nil || len(res.Errors) > 0 || res.Segments != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	keys, _ := r.a.B.List("v1/laharl/claude/-proj/" + sidA + ".jsonl/")
	body, _ := r.a.B.Get(keys[0])
	if string(body[:4]) != "CBP2" || bytes.Contains(body, []byte("age-encryption.org")) {
		t.Fatalf("not a plaintext segment: %q", body[:12])
	}
	want, _ := os.ReadFile(mainA)
	if got := restoreString(t, r, "claude", "-proj/"+sidA+".jsonl"); got != string(want) {
		t.Fatal("restored bytes differ")
	}
	vr, _ := r.a.Verify(r.op, r.sources())
	if vr.Matched != 4 || vr.Mismatched != 0 {
		t.Fatalf("%+v", vr)
	}
	// Append-only and rerun-safe, as in age mode.
	r.a.Run(r.sources())
	before, _ := r.a.B.List("v1/")
	r.write(mainA, claudeLine("user", "user", "more", "2026-10-03T11:00:00Z"), true)
	r.old(mainA)
	res, _ = r.a.Run(r.sources())
	after, _ := r.a.B.List("v1/")
	if res.Segments != 1 || len(after) != len(before)+1 {
		t.Fatalf("%+v %d %d", res, len(before), len(after))
	}
	// A tampered plaintext segment is still caught (the header must match its key).
	n, _ := parseSegKey(keys[0])
	n.Off = 5
	if _, err := decodeSegment(r.op, n, body); err == nil {
		t.Fatal("moved plaintext segment accepted")
	}
}

func TestModesAreNeverMixed(t *testing.T) {
	d := t.TempDir()
	_, rcpt, _ := GenerateIdentity()
	open := func(mode, rc string) (*Archiver, error) {
		return Config{Encryption: mode, Dir: filepath.Join(d, "store"), Recipient: rc, Host: "h"}.Open()
	}
	if _, err := open("", rcpt); err == nil || !strings.Contains(err.Error(), "explicitly") {
		t.Fatalf("a missing mode must be an error: %v", err)
	}
	if _, err := open("rot13", ""); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := open(ModeNone, rcpt); err == nil || !strings.Contains(err.Error(), "recipients are set") {
		t.Fatalf("none with recipients must be refused: %v", err)
	}
	if _, err := open(ModeAge, ""); err == nil {
		t.Fatal("age with no recipients accepted")
	}
	a, err := open(ModeNone, "")
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := ReadMode(a.B); m != ModeNone {
		t.Fatalf("marker %q", m)
	}
	if _, err := open(ModeAge, rcpt); err == nil || !strings.Contains(err.Error(), "mixing modes") {
		t.Fatalf("age onto a plaintext archive must be refused: %v", err)
	}
	if _, err := open(ModeNone, ""); err != nil {
		t.Fatalf("reopening in the same mode failed: %v", err)
	}
	// A plain segment is refused by an age reader, and the reverse.
	r := newPlainRig(t)
	r.fixture()
	r.a.Run(r.sources())
	keys, _ := r.a.B.List("v1/laharl/pi/")
	body, _ := r.a.B.Get(keys[0])
	n, _ := parseSegKey(keys[0])
	ageOp, _ := NewOpener(r.identity)
	if _, err := decodeSegment(ageOp, n, body); err == nil || !strings.Contains(err.Error(), "mixed modes are refused") {
		t.Fatalf("%v", err)
	}
	// An archive with no marker (made before it existed) is read from its first segment.
	r2 := newRig(t)
	r2.fixture()
	r2.a.Run(r2.sources())
	if m, _ := ReadMode(r2.a.B); m != ModeAge {
		t.Fatalf("inferred %q", m)
	}
	if err := EnsureMode(r2.a.B, ModeNone); err == nil {
		t.Fatal("plaintext accepted onto an age archive without a marker")
	}
}

func TestPermissionWarnings(t *testing.T) {
	d := t.TempDir()
	loose := filepath.Join(d, "loose")
	os.Mkdir(loose, 0o755)
	os.Chmod(loose, 0o755)
	if w := CheckPerms(loose, filepath.Join(d, "missing"), ""); len(w) != 1 || !strings.Contains(w[0], "0755") {
		t.Fatalf("%v", w)
	}
	os.Chmod(loose, 0o700)
	if w := CheckPerms(loose); len(w) != 0 {
		t.Fatalf("%v", w)
	}
	// A new archive directory is private, and so is the index's.
	b, _ := NewFSBackend(filepath.Join(d, "new", "store"))
	if w := CheckPerms(b.Dir); len(w) != 0 {
		t.Fatalf("new archive dir not private: %v", w)
	}
	ix := &Index{Path: filepath.Join(d, "idx", "i.db")}
	if err := ix.open(); err != nil {
		t.Skip("no sqlite3")
	}
	if w := CheckPerms(filepath.Dir(ix.Path), ix.Path); len(w) != 0 {
		t.Fatalf("index not private: %v", w)
	}
}

func TestRewrapPlainToAgeAndBack(t *testing.T) {
	r := newPlainRig(t)
	r.fixture()
	r.a.Run(r.sources())
	r.a.SetSummary("claude", sidA, "a plain note", "t")
	ident, rcpt, _ := GenerateIdentity()
	opAge, _ := NewOpener(ident)
	sealAge, _ := NewSealer(rcpt)

	dst, _ := NewFSBackend(filepath.Join(r.dir, "age-store"))
	st, err := r.a.Rewrap(r.op, dst, sealAge)
	if err != nil || st.Segments != 4 || st.Notes != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	aged := &Archiver{Host: "laharl", B: dst, S: sealAge}
	vr, _ := aged.Verify(opAge, r.sources())
	if vr.Matched != 4 || vr.Mismatched != 0 {
		t.Fatalf("age copy does not verify: %+v", vr)
	}
	keys, _ := dst.List("v1/laharl/claude/-proj/" + sidA + ".jsonl/")
	body, _ := dst.Get(keys[0])
	if string(body[:4]) != "CBA2" {
		t.Fatal("rewrapped segment is not age")
	}
	// The plain reader cannot read the age copy, with a clear refusal.
	if _, err := StreamTo(&bytes.Buffer{}, dst, NoneSealer{}, "laharl", "claude", "-proj/"+sidA+".jsonl"); err == nil || !strings.Contains(err.Error(), "mixed modes") {
		t.Fatalf("%v", err)
	}

	// And back to plaintext.
	back, _ := NewFSBackend(filepath.Join(r.dir, "plain-store"))
	st, err = aged.Rewrap(opAge, back, NoneSealer{})
	if err != nil || st.Segments != 4 || st.Notes != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	plain := &Archiver{Host: "laharl", B: back, S: NoneSealer{}}
	if vr, _ = plain.Verify(NoneSealer{}, r.sources()); vr.Matched != 4 || vr.Mismatched != 0 {
		t.Fatalf("plaintext copy does not verify: %+v", vr)
	}
}

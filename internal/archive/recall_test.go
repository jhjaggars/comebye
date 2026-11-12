package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// recallRig indexes a session with tool calls, a subagent and secrets.
type recallRig struct {
	*rig
	ix   *Index
	rd   *Reader
	path string
}

const (
	sidR = "99999999-9999-4999-8999-999999999999"
	sidP = "2026-10-03T10-00-00-000Z_77777777-7777-4777-8777-777777777777"
)

func recallLine(t *testing.T, typ, ts string, content any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": typ, "sessionId": sidR, "cwd": "/home/u/.worktrees/comebye/feat-r", "gitBranch": "feat/r", "timestamp": ts,
		"message": map[string]any{"role": typ, "content": content}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func newRecallRig(t *testing.T) *recallRig {
	t.Helper()
	hasSqlite(t)
	r := newRig(t)
	long := "start of output\n" + strings.Repeat("noise noise\n", 1000) + "kerfuffle at the very end"
	first := recallLine(t, "user", "2026-10-03T10:00:00Z", "the zebra plan, token "+fakeGH)
	body := first +
		recallLine(t, "assistant", "2026-10-03T10:00:01Z", []map[string]any{{"type": "text", "text": "**zebra** [x]\x02 fake\x03 \ue000mark\ue001"}}) +
		recallLine(t, "assistant", "2026-10-03T10:00:02Z", []map[string]any{{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]string{"command": "curl -H 'Authorization: Bearer " + fakeBearer + "' zebra.test"}}}) +
		recallLine(t, "user", "2026-10-03T10:00:03Z", []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "is_error": true, "content": long}})
	for i := 0; i < 30; i++ {
		body += recallLine(t, "assistant", "2026-10-03T10:01:00Z", []map[string]any{{"type": "text", "text": fmt.Sprintf("filler %d quokka", i)}})
	}
	main := filepath.Join(r.claude, "-proj", sidR+".jsonl")
	r.write(main, body, false)
	r.old(main)
	sub := filepath.Join(r.claude, "-proj", sidR, "subagents", "agent-sub1.jsonl")
	r.write(sub, recallLine(t, "assistant", "2026-10-03T10:02:00Z", []map[string]any{{"type": "text", "text": "subagent says wombat"}}), false)
	r.old(sub)
	pi := filepath.Join(r.pi, "--x--", sidP+".jsonl")
	r.write(pi, `{"type":"session","version":3,"id":"77777777","timestamp":"2026-09-01T10:00:00Z","cwd":"/home/u/code/other"}`+"\n"+
		`{"type":"message","timestamp":"2026-09-01T10:00:01Z","message":{"role":"user","content":[{"type":"text","text":"the zebra elsewhere"}]}}`+"\n"+
		`{"type":"message","timestamp":"2026-09-01T10:00:02Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls zebra"}}]}}`+"\n"+
		`{"type":"message","timestamp":"2026-09-01T10:00:03Z","message":{"role":"toolResult","toolCallId":"c1","isError":true,"content":[{"type":"text","text":"ls: zebra: no such file"}]}}`+"\n", false)
	r.old(pi)
	if _, err := r.a.Run(r.sources()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.dir, "recall.db")
	ix := &Index{Path: path}
	links := []Link{{ID: "h-9", Host: "laharl", AgentID: "laharl/w1:p1", RepoName: "comebye", Branch: "feat/r", Worktree: "/home/u/.worktrees/comebye/feat-r",
		Brief: "Do the zebra work with " + fakeAWS, CreatedAt: "2026-10-03T09:59:00Z", FinalReport: "## Summary\nfinished, " + fakeTS, Status: "finished"}}
	st, err := ix.Sync(r.a, r.op, links, SyncOptions{})
	if err != nil || len(st.Errors) > 0 {
		t.Fatalf("%v %+v", err, st)
	}
	return &recallRig{r, ix, NewReader(path), path}
}

// Nothing secret-looking reaches any table: not messages, not the full-text
// index, not the session's prompt or the thread's outcome.
func TestSecretsNeverReachTheIndex(t *testing.T) {
	r := newRecallRig(t)
	if _, err := r.ix.run("PRAGMA wal_checkpoint(TRUNCATE);", false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fakeGH, fakeBearer, fakeAWS, fakeTS} {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("%s... is in the index file", s[:8])
		}
	}
	for _, s := range []string{fakeGH, fakeBearer} {
		res, err := r.rd.Search(context.Background(), `"`+s+`"`, Filter{}, "", 0)
		if err != nil || res.Total != 0 {
			t.Errorf("searching for a masked value: %v %+v", err, res)
		}
	}
	rows, err := r.ix.Query("SELECT n_secret FROM sessions WHERE id=" + q(sidR))
	if err != nil || num(rows[0], "n_secret") != 3 { // the prompt, the command, the brief
		t.Errorf("%v %v", err, rows)
	}
}

func TestReaderSearch(t *testing.T) {
	r := newRecallRig(t)
	ctx := context.Background()
	res, err := r.rd.Search(ctx, "zebra", Filter{}, "", 0)
	if err != nil || res.Total != 2 || len(res.Hits) != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	var claude SearchHit
	for _, h := range res.Hits {
		if h.Kind == "claude" {
			claude = h
		}
	}
	if claude.Matches < 3 || !strings.Contains(claude.Snippet, "\x02") || claude.Secrets == 0 || claude.Ledger != "h-9" || claude.Thread == "" || claude.Verdict == "" {
		t.Errorf("%+v", claude)
	}
	// One row per session, paged by skip.
	p1, _ := r.rd.Search(ctx, "zebra", Filter{Limit: 1}, "new", 0)
	p2, _ := r.rd.Search(ctx, "zebra", Filter{Limit: 1}, "new", 1)
	if len(p1.Hits) != 1 || len(p2.Hits) != 1 || p1.Hits[0].ID == p2.Hits[0].ID || p1.Total != 2 || p1.Hits[0].Kind != "claude" {
		t.Errorf("%+v %+v", p1, p2)
	}
	// The end of long output can be found, and the role filter sees it.
	res, _ = r.rd.Search(ctx, "kerfuffle", Filter{Role: "output"}, "", 0)
	if res.Total != 1 || res.Hits[0].Role != "result" {
		t.Errorf("%+v", res)
	}
	res, _ = r.rd.Search(ctx, "kerfuffle", Filter{Role: "said"}, "", 0)
	if res.Total != 0 {
		t.Errorf("%+v", res)
	}
	res, _ = r.rd.Search(ctx, "zebra", Filter{Host: "laharl", Kind: "pi"}, "", 0)
	if res.Total != 1 || res.Hits[0].Kind != "pi" {
		t.Errorf("%+v", res)
	}
	// Text that is not valid FTS5 is searched as words, and text cannot reach SQL.
	for _, bad := range []string{`"zebra`, `zebra"`, `(zebra`, `zebra*:`, `NEAR(`, `' OR 1=1; DROP TABLE messages; --`, `.shell true`, "a\x00b"} {
		if _, err := r.rd.Search(ctx, bad, Filter{Project: "x'; DROP TABLE sessions; --"}, "", 0); err != nil {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if rows, err := r.ix.Query("SELECT count(*) AS n FROM messages"); err != nil || num(rows[0], "n") == 0 {
		t.Errorf("%v %v", err, rows)
	}
	// A stored transcript cannot fake a highlight.
	res, _ = r.rd.Search(ctx, "fake", Filter{}, "", 0)
	if res.Total != 1 || strings.Count(res.Hits[0].Snippet, "\x02") != 1 {
		t.Errorf("%+v", res)
	}
}

func TestReaderSessionPages(t *testing.T) {
	r := newRecallRig(t)
	ctx := context.Background()
	p, err := r.rd.Session(ctx, "laharl", "claude", sidR, PageOpts{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Msgs) != 5 || p.Before || !p.After || p.Msgs[0].Role != "handoff" || p.Msgs[0].Seq != -1 {
		t.Fatalf("%+v", p)
	}
	if len(p.Subs) != 1 || p.Subs[0].ID != "sub1" || p.Subs[0].Messages != 1 {
		t.Errorf("subs: %+v", p.Subs)
	}
	var tool, result *PageMsg
	for i := range p.Msgs {
		switch p.Msgs[i].Role {
		case "tool":
			tool = &p.Msgs[i]
		case "result":
			result = &p.Msgs[i]
		}
	}
	if tool == nil || result == nil || tool.ToolID != "t1" || result.ToolID != "t1" || !result.IsErr || result.FullLen == 0 || !tool.Secret {
		t.Errorf("%+v %+v", tool, result)
	}
	// Around a message, and either side of one.
	s := 15
	p, _ = r.rd.Session(ctx, "laharl", "claude", sidR, PageOpts{Limit: 6, Seq: &s})
	if len(p.Msgs) != 6 || p.Msgs[0].Seq != 12 || p.Msgs[5].Seq != 17 || !p.Before || !p.After {
		t.Errorf("around: %+v", p.Msgs)
	}
	p, _ = r.rd.Session(ctx, "laharl", "claude", sidR, PageOpts{Limit: 3, Seq: &s, Dir: "after"})
	if p.Msgs[0].Seq != 16 || !p.Before {
		t.Errorf("after: %+v", p.Msgs)
	}
	p, _ = r.rd.Session(ctx, "laharl", "claude", sidR, PageOpts{Limit: 3, Seq: &s, Dir: "before"})
	if p.Msgs[2].Seq != 14 || !p.After {
		t.Errorf("before: %+v", p.Msgs)
	}
	// A subagent is its own thread.
	p, _ = r.rd.Session(ctx, "laharl", "claude", sidR, PageOpts{Sub: "sub1"})
	if len(p.Msgs) != 1 || p.Msgs[0].Text != "subagent says wombat" {
		t.Errorf("sub: %+v", p.Msgs)
	}
	// Marks in the page, for what matched only.
	p, _ = r.rd.Session(ctx, "laharl", "claude", sidR, PageOpts{Q: "zebra", Limit: 4})
	n := 0
	for _, m := range p.Msgs {
		if m.Hit {
			n++
			if !strings.Contains(m.Text, "\x02") {
				t.Errorf("no mark: %q", m.Text)
			}
		}
	}
	if n == 0 {
		t.Error("nothing marked")
	}
	if _, err := r.rd.Session(ctx, "laharl", "claude", "nope", PageOpts{}); err != ErrNoSession {
		t.Errorf("%v", err)
	}
	hits, err := r.rd.Hits(ctx, "laharl", "claude", sidR, "wombat")
	if err != nil || len(hits) != 1 || hits[0].Sub != "sub1" {
		t.Errorf("%v %+v", err, hits)
	}
}

func TestReaderCannotWrite(t *testing.T) {
	r := newRecallRig(t)
	ctx := context.Background()
	if _, err := r.rd.query(ctx, "DELETE FROM sessions;"); err == nil {
		t.Error("a write went through")
	}
	marker := filepath.Join(r.dir, "shell-ran")
	if _, err := r.rd.query(ctx, ".shell touch "+marker+"\n"); err == nil {
		t.Error("a dot-command went through")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error(".shell ran")
	}
}

func TestReaderStatus(t *testing.T) {
	r := newRecallRig(t)
	ctx := context.Background()
	st, err := r.rd.Status(ctx)
	if err != nil || st.State != "ok" || st.Sessions != 2 || st.BuiltAt == "" || len(st.Hosts) != 1 {
		t.Errorf("%v %+v", err, st)
	}
	if _, err := r.ix.run("UPDATE meta SET value='2020-01-01T00:00:00Z' WHERE key='built_at';", false); err != nil {
		t.Fatal(err)
	}
	if st, _ := r.rd.Status(ctx); st.State != "stale" {
		t.Errorf("%+v", st)
	}
	if st, _ := NewReader(filepath.Join(r.dir, "none.db")).Status(ctx); st.State != "missing" {
		t.Errorf("%+v", st)
	}
	bad := &Reader{Path: r.path, Sqlite: filepath.Join(r.dir, "no-sqlite3"), Timeout: NewReader("").Timeout, slots: make(chan struct{}, 1)}
	if st, _ := bad.Status(ctx); st.State != "unavailable" {
		t.Errorf("%+v", st)
	}
	if _, err := NewReader(filepath.Join(r.dir, "none.db")).Facets(ctx); err != ErrNoIndex {
		t.Errorf("%v", err)
	}
	f, err := r.rd.Facets(ctx)
	if err != nil || len(f.Hosts) != 1 || len(f.Kinds) != 2 || len(f.Projects) != 2 {
		t.Errorf("%v %+v", err, f)
	}
}

// An index in the old format is replaced whole, keeps the PR cache, and stays
// readable until the new one is ready.
func TestOldIndexIsRebuiltAside(t *testing.T) {
	hasSqlite(t)
	r := newRig(t)
	r.fixture()
	if _, err := r.a.Run(r.sources()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.dir, "old.db")
	ix := &Index{Path: path}
	if err := ix.create(); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.run(`PRAGMA journal_mode=WAL;
CREATE TABLE sessions(host TEXT, kind TEXT, id TEXT);
CREATE VIRTUAL TABLE msgs USING fts5(text, host UNINDEXED);
INSERT INTO msgs VALUES('only in the old file', 'laharl');
CREATE TABLE gh_prs(repo TEXT, branch TEXT, fetched TEXT, json TEXT, PRIMARY KEY(repo,branch));
INSERT INTO gh_prs VALUES('r','b','t','[]');`, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := NewReader(path).Status(context.Background()); st.State != "outdated" {
		t.Errorf("%+v", st)
	}
	if !ix.Outdated() {
		t.Error("not seen as outdated")
	}
	if err := ix.DeleteSession("laharl", "pi", "x"); err != nil {
		t.Errorf("deleting from an old index: %v", err)
	}
	st, err := ix.Sync(r.a, r.op, nil, SyncOptions{})
	if err != nil || st.Indexed != 2 {
		t.Fatalf("%v %+v", err, st)
	}
	if v, err := ix.version(); err != nil || v != schemaVersion {
		t.Errorf("%v %v", err, v)
	}
	if rows, err := ix.Query("SELECT count(*) AS n FROM gh_prs"); err != nil || num(rows[0], "n") != 1 {
		t.Errorf("PR cache: %v %v", err, rows)
	}
	if rows, err := ix.Search("only in the old file", Filter{}); err != nil || len(rows) != 0 {
		t.Errorf("%v %v", err, rows)
	}
	for _, f := range []string{".rebuild", ".rebuild-wal", ".rebuild-shm"} {
		if _, err := os.Stat(path + f); err == nil {
			t.Errorf("%s left behind", f)
		}
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
}

// Syncing with nothing new rewrites no handoff row, so the full-text index
// does not grow with each hourly run.
func TestUnchangedLinksAreNotRewritten(t *testing.T) {
	r := newRecallRig(t)
	// A rewrite would put the link's time back in place of this mark.
	mark := func() {
		if _, err := r.ix.run("UPDATE messages SET ts='untouched' WHERE role='handoff';", false); err != nil {
			t.Fatal(err)
		}
	}
	ts := func() string {
		rows, err := r.ix.Query("SELECT ts FROM messages WHERE role='handoff';")
		if err != nil || len(rows) != 1 {
			t.Fatalf("%v %v", err, rows)
		}
		return sget(rows[0], "ts")
	}
	mark()
	links := []Link{{ID: "h-9", Host: "laharl", AgentID: "laharl/w1:p1", RepoName: "comebye", Branch: "feat/r", Worktree: "/home/u/.worktrees/comebye/feat-r",
		Brief: "Do the zebra work with " + fakeAWS, CreatedAt: "2026-10-03T09:59:00Z"}}
	if _, err := r.ix.Sync(r.a, r.op, links, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if ts() != "untouched" {
		t.Error("the handoff row was rewritten with nothing changed")
	}
	links[0].Brief = "A new brief about capybaras"
	if _, err := r.ix.Sync(r.a, r.op, links, SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if ts() == "untouched" {
		t.Error("a changed brief was not written")
	}
	for q, want := range map[string]int{"capybaras": 1, "\"zebra work\"": 0} {
		rows, err := r.ix.Search(q, Filter{})
		if err != nil || len(rows) != want {
			t.Errorf("%s: %v %d rows, want %d", q, err, len(rows), want)
		}
	}
}

// The thread pass reads each host once; what it finds per thread must not change.
func TestThreadInputsPerSession(t *testing.T) {
	r := newRecallRig(t)
	rows, err := r.ix.Query("SELECT id,verdict,keywords FROM threads ORDER BY started;")
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %v", err, rows)
	}
	for _, x := range rows {
		if sget(x, "keywords") == "" {
			t.Errorf("no keywords: %v", x)
		}
	}
	os, err := r.ix.Query("SELECT o.kind AS kind, o.text AS text FROM outcomes o JOIN thread_sessions x ON x.thread_id=o.thread_id WHERE x.session_id=" + q(sidP) + ";")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range os {
		if sget(o, "kind") == "last-message" {
			found = true
		}
	}
	if found {
		t.Errorf("a session with no assistant text has a last message: %v", os)
	}
	os, _ = r.ix.Query("SELECT o.kind AS kind, o.text AS text FROM outcomes o JOIN thread_sessions x ON x.thread_id=o.thread_id WHERE x.session_id=" + q(sidR) + " AND o.kind='report';")
	if len(os) != 1 || strings.Contains(sget(os[0], "text"), "tskey") {
		t.Errorf("report: %v", os)
	}
}

// Older sqlite3 shells write control bytes in -json output as a bare "u0002", so
// marks must not pass through the shell as control bytes. This shell behaves so.
func TestMarksSurviveAnOlderSqliteShell(t *testing.T) {
	r := newRecallRig(t)
	real, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("no sqlite3")
	}
	shim := filepath.Join(r.dir, "old-sqlite3")
	script := "#!/bin/sh\n" + real + " \"$@\" | sed 's/\\\\u00\\(0[0-9a-f]\\)/u00\\1/g'\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := NewReader(r.path)
	old.Sqlite = shim
	ctx := context.Background()
	res, err := old.Search(ctx, "wombat", Filter{}, "", 0)
	if err != nil || res.Total != 1 || !strings.Contains(res.Hits[0].Snippet, "\x02wombat\x03") {
		t.Errorf("%v %+v", err, res)
	}
	p, err := old.Session(ctx, "laharl", "claude", sidR, PageOpts{Sub: "sub1", Q: "wombat"})
	if err != nil || len(p.Msgs) != 1 || !strings.Contains(p.Msgs[0].Text, "\x02wombat\x03") {
		t.Errorf("%v %+v", err, p.Msgs)
	}
}

// Work finds the threads a question is about; Thread and ForLink lead to the sessions.
func TestReaderWorkThreadAndLinks(t *testing.T) {
	r := newRecallRig(t)
	ctx := context.Background()
	w, err := r.rd.Work(ctx, "zebra", Filter{}, 0)
	if err != nil || w.Total < 1 || len(w.Threads) < 1 {
		t.Fatalf("%v %+v", err, w)
	}
	var th ThreadRow
	for _, x := range w.Threads {
		if x.Sessions >= 1 && strings.Contains(x.Brief, "zebra") {
			th = x
		}
	}
	if th.ID == "" || th.Verdict == "" || strings.Contains(th.Brief, fakeAWS) {
		t.Fatalf("%+v", w)
	}
	// Filters apply: another agent has no such work, this one does.
	if n, _ := r.rd.Work(ctx, "zebra", Filter{Agent: "laharl/w9:p9"}, 0); n.Total != 0 || len(n.Threads) != 0 {
		t.Errorf("%+v", n)
	}
	if n, _ := r.rd.Work(ctx, "zebra", Filter{Agent: "laharl/w1:p1"}, 0); n.Total < 1 {
		t.Errorf("%+v", n)
	}
	if n, _ := r.rd.Work(ctx, "zebra", Filter{}, 99); n.Total != w.Total || len(n.Threads) != 0 {
		t.Errorf("skip past the end: %+v", n)
	}
	if n, err := r.rd.Work(ctx, "the and", Filter{}, 0); err != nil || n.Total != 0 || n.Threads == nil {
		t.Errorf("a question with no topic word: %v %+v", err, n)
	}
	v, err := r.rd.Thread(ctx, th.ID)
	if err != nil || v.ID != th.ID || len(v.Sessions) == 0 || v.Sessions[0].Handoff == "" {
		t.Fatalf("%v %+v", err, v)
	}
	if strings.Contains(fmt.Sprint(v), fakeAWS) || strings.Contains(fmt.Sprint(v), fakeTS) {
		t.Errorf("a masked value reached the thread: %+v", v)
	}
	if _, err := r.rd.Thread(ctx, "t-00000000"); err != ErrNoThread {
		t.Errorf("%v", err)
	}
	if _, err := r.rd.Thread(ctx, "t-%"); err != ErrNoThread {
		t.Errorf("a pattern in an id: %v", err)
	}
	for _, f := range []Filter{{Ledger: "h-9"}, {Agent: "laharl/w1:p1"}} {
		l, err := r.rd.ForLink(ctx, f)
		if err != nil || len(l.Sessions) != 1 || l.Sessions[0].ID != sidR || l.Thread != th.ID {
			t.Errorf("%+v: %v %+v", f, err, l)
		}
	}
	if l, err := r.rd.ForLink(ctx, Filter{Ledger: "h-nope"}); err != nil || len(l.Sessions) != 0 || l.Sessions == nil {
		t.Errorf("%v %+v", err, l)
	}
}

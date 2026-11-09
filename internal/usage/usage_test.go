package usage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 5, 10, 30, 0, 0, time.UTC)

func clock() time.Time { return t0.Add(2 * time.Hour) }

func cl(id, ts, model string, in, out, cw, cr int, extra string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"cwd":"/w/x","gitBranch":"feat/a"%s,"message":{"id":%q,"model":%q,"content":[{"type":"text","text":"SECRET"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`+"\n",
		ts, extra, id, model, in, out, cw, cr)
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "usage.json"), "box")
	if err != nil {
		t.Fatal(err)
	}
	st.SetClock(clock)
	return st
}

func scan(t *testing.T, st *Store, srcs ...Source) ScanStats {
	t.Helper()
	s, err := st.Scan(context.Background(), srcs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func session(t *testing.T, st *Store, kind, id string) *Session {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.sessions["box/"+kind+"/"+id]
	if s == nil {
		t.Fatalf("no session %s/%s", kind, id)
	}
	return s
}

func TestParseClaude(t *testing.T) {
	s, meta, ok := ParseClaude([]byte(cl("m1", "2026-11-05T10:30:00.000Z", "claude-opus-5-5", 3, 4, 5, 600, "")), false)
	if !ok || s == nil || s.ID != "m1" || s.Model != "claude-opus-5-5" {
		t.Fatalf("sample = %+v", s)
	}
	if s.Tok != (Tokens{In: 3, Out: 4, CW: 5, CR: 600, Calls: 1}) {
		t.Fatalf("tokens = %+v", s.Tok)
	}
	if meta.Cwd != "/w/x" || meta.Branch != "feat/a" {
		t.Fatalf("meta = %+v", meta)
	}
	// A sidechain reply counts as a subagent call; so does any reply in a subagent file.
	s, _, _ = ParseClaude([]byte(cl("m2", "2026-11-05T10:30:00Z", "m", 1, 1, 0, 0, `,"isSidechain":true`)), false)
	if s == nil || s.Tok.Sub != 1 || !s.Side {
		t.Fatalf("sidechain = %+v", s)
	}
	s, _, _ = ParseClaude([]byte(cl("m3", "2026-11-05T10:30:00Z", "m", 1, 1, 0, 0, "")), true)
	if s == nil || s.Tok.Sub != 1 {
		t.Fatalf("subagent file = %+v", s)
	}
	// Lines that are not replies, are not JSON, or carry a string message give nothing and do not panic.
	for _, l := range []string{`{"type":"user","message":{"content":"hi","usage":1}}`, `not json "usage"`, `{"type":"assistant","message":"usage"}`, `{"type":"summary"}`} {
		if s, _, _ := ParseClaude([]byte(l), false); s != nil {
			t.Errorf("%q gave a sample", l)
		}
	}
}

func TestParsePi(t *testing.T) {
	_, meta, _ := ParsePi([]byte(`{"type":"session","version":3,"id":"x","timestamp":"2026-06-06T19:50:36.226Z","cwd":"/home/u"}`))
	if meta.Cwd != "/home/u" {
		t.Fatalf("meta = %+v", meta)
	}
	s, _, ok := ParsePi([]byte(`{"type":"message","id":"a","timestamp":"2026-06-06T19:51:00.000Z","message":{"role":"assistant","provider":"homelab","model":"glm","usage":{"input":10,"output":2,"cacheRead":7,"cacheWrite":1,"cost":{"total":0.5}}}}`))
	if !ok || s == nil || s.Model != "homelab/glm" || s.Tok != (Tokens{In: 10, Out: 2, CW: 1, CR: 7, Calls: 1}) {
		t.Fatalf("sample = %+v", s)
	}
	if s, _, _ := ParsePi([]byte(`{"type":"message","timestamp":"2026-06-06T19:51:00Z","message":{"role":"toolResult","usage":{"input":1}}}`)); s != nil {
		t.Fatal("a tool result counted")
	}
	// Pi ids are UUIDv7: the first eight characters are shared by neighbours, so the whole tail is the id.
	a := PiSessionID("2026-08-17T23-35-16-514Z_01a01214-5122-79e4-b929-ff945fa74686.jsonl")
	b := PiSessionID("2026-08-17T23-35-17-001Z_01a01214-9999-79e4-b929-ff945fa74686.jsonl")
	if a != "01a01214-5122-79e4-b929-ff945fa74686" || a == b {
		t.Fatalf("ids %q %q", a, b)
	}
}

func TestScanDedupeAndSubagents(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "proj", "sess1.jsonl")
	// The same message id is written once per content block with the usage so far: the last wins.
	write(t, main,
		cl("m1", "2026-11-05T10:30:00Z", "claude-opus-5-5", 10, 1, 100, 1000, "")+
			cl("m1", "2026-11-05T10:30:01Z", "claude-opus-5-5", 10, 50, 100, 1000, "")+
			cl("m2", "2026-11-05T11:05:00Z", "claude-opus-5-5", 20, 5, 0, 2000, "")+
			cl("s", "2026-11-05T11:06:00Z", "<synthetic>", 5, 5, 0, 0, "")+
			`{"type":"user","message":{"content":"hello"}}`+"\n")
	write(t, filepath.Join(dir, "proj", "sess1", "subagents", "agent-a.jsonl"),
		cl("sub1", "2026-11-05T10:40:00Z", "claude-haiku-5-5", 7, 3, 0, 0, ""))
	st := newStore(t)
	scan(t, st, Source{"claude", dir})
	s := session(t, st, "claude", "sess1")
	tot := s.Total()
	want := Tokens{In: 10 + 20 + 7, Out: 50 + 5 + 3, CW: 100, CR: 3000, Calls: 3, Sub: 1}
	if tot != want {
		t.Fatalf("total = %+v want %+v", tot, want)
	}
	if len(s.Rows) != 3 { // two hours for opus (10h, 11h), one haiku
		t.Fatalf("rows = %+v", s.Rows)
	}
	if s.Cwd != "/w/x" || s.Branch != "feat/a" {
		t.Fatalf("cwd/branch = %q %q", s.Cwd, s.Branch)
	}
	// A second scan with nothing new changes nothing.
	if x := scan(t, st, Source{"claude", dir}); x.Read != 0 || session(t, st, "claude", "sess1").Total() != want {
		t.Fatalf("rescan read %d", x.Read)
	}
}

func TestIncrementalOffsetsAndTruncatedLine(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p", "s.jsonl")
	l1 := cl("a", "2026-11-05T10:00:00Z", "m", 1, 1, 0, 0, "")
	l2 := cl("b", "2026-11-05T10:01:00Z", "m", 2, 2, 0, 0, "")
	cut := len(l2) / 2
	write(t, f, l1+l2[:cut]) // the last line is still being written
	st := newStore(t)
	scan(t, st, Source{"claude", dir})
	if got := session(t, st, "claude", "s").Total(); got.In != 1 || got.Calls != 1 {
		t.Fatalf("after the partial line: %+v", got)
	}
	st.mu.Lock()
	off := st.files[f].Off
	st.mu.Unlock()
	if off != int64(len(l1)) {
		t.Fatalf("offset %d, want %d (just past the whole line)", off, len(l1))
	}
	appendTo(t, f, l2[cut:])
	appendTo(t, f, cl("b", "2026-11-05T10:01:30Z", "m", 2, 9, 0, 0, "")) // b again, grown: replaces the first b
	scan(t, st, Source{"claude", dir})
	if got := session(t, st, "claude", "s").Total(); got.In != 3 || got.Out != 1+9 || got.Calls != 2 {
		t.Fatalf("after completing: %+v", got)
	}
	// The replacement works across scans, and a whole last line without a newline is read.
	appendTo(t, f, strings.TrimSuffix(cl("c", "2026-11-05T10:02:00Z", "m", 5, 5, 0, 0, ""), "\n"))
	scan(t, st, Source{"claude", dir})
	if got := session(t, st, "claude", "s").Total(); got.In != 8 || got.Calls != 3 {
		t.Fatalf("no trailing newline: %+v", got)
	}
	// It survives a restart: offsets are saved with the counts, so nothing is counted twice.
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(st.path, "box")
	if err != nil {
		t.Fatal(err)
	}
	st2.SetClock(clock)
	scan(t, st2, Source{"claude", dir})
	if got := session(t, st2, "claude", "s").Total(); got.In != 8 || got.Calls != 3 {
		t.Fatalf("after reopen: %+v", got)
	}
}

func TestPiSessions(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "--home-u--", "2026-11-05T10-00-00-000Z_01a01214-5122-79e4-b929-ff945fa74686.jsonl"),
		`{"type":"session","version":3,"id":"x","timestamp":"2026-11-05T10:00:00.000Z","cwd":"/home/u/work"}`+"\n"+
			`{"type":"message","id":"a","timestamp":"2026-11-05T10:01:00.000Z","message":{"role":"assistant","provider":"p","model":"m","usage":{"input":100,"output":10,"cacheRead":50,"cacheWrite":0,"cost":{"total":1}}}}`+"\n"+
			`{"type":"message","id":"b","timestamp":"2026-11-05T10:02:00.000Z","message":{"role":"toolResult","content":[{"type":"text","text":"x"}]}}`+"\n")
	st := newStore(t)
	scan(t, st, Source{"pi", dir})
	s := session(t, st, "pi", "01a01214-5122-79e4-b929-ff945fa74686")
	if s.Cwd != "/home/u/work" || s.Total() != (Tokens{In: 100, Out: 10, CR: 50, Calls: 1}) || s.Rows[0].M != "p/m" {
		t.Fatalf("%+v", s)
	}
}

func TestStoresCountsAndLabelsNeverText(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "p", "s.jsonl"), cl("a", "2026-11-05T10:00:00Z", "m", 1, 1, 0, 0, ""))
	st := newStore(t)
	scan(t, st, Source{"claude", dir})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(st.path)
	if strings.Contains(string(b), "SECRET") {
		t.Fatal("transcript text reached the store")
	}
	snap, _ := os.ReadFile(st.path)
	_ = snap
	if fi, _ := os.Stat(st.path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestOldFilesAreNotRead(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p", "old.jsonl")
	write(t, f, cl("a", "2025-01-01T10:00:00Z", "m", 1, 1, 0, 0, ""))
	old := clock().Add(-SessionRetention - time.Hour)
	if err := os.Chtimes(f, old, old); err != nil {
		t.Fatal(err)
	}
	st := newStore(t)
	if x := scan(t, st, Source{"claude", dir}); x.Read != 0 {
		t.Fatalf("read %d", x.Read)
	}
}

func fixture(t *testing.T) *Store {
	st := newStore(t)
	st.mu.Lock()
	defer st.mu.Unlock()
	put := func(host, kind, id string, l Labels, last time.Time, rows ...Row) {
		s := &Session{Host: host, Kind: kind, ID: id, L: l, Rows: rows, First: last.Unix(), Last: last.Unix()}
		s.index()
		st.sessions[s.Key()] = s
	}
	h := func(d time.Time) int64 { return HourOf(d) }
	put("box", "claude", "s1", Labels{Project: "comebye", Role: "worker", Purpose: "build", Handoff: "h1", Name: "w1"}, t0,
		Row{h(t0), "claude-opus-5-5", Tokens{In: 1000, Out: 2000, CW: 3000, CR: 40000, Calls: 5, Sub: 1}},
		Row{h(t0.Add(25 * time.Hour)), "claude-opus-5-5", Tokens{In: 10, Out: 20, CW: 30, CR: 400, Calls: 1}})
	put("peer", "pi", "p1", Labels{Project: "x", Role: "home", Purpose: "ad-hoc"}, t0,
		Row{h(t0), "homelab/glm", Tokens{In: 500, Out: 500, CR: 5000, Calls: 2}})
	return st
}

func TestRunRollupMath(t *testing.T) {
	st := fixture(t)
	q := Query{From: t0.Add(-24 * time.Hour), To: t0.Add(72 * time.Hour), By: "kind", Bucket: "day", Prices: Effective(nil)}
	r, err := st.Run(q)
	if err != nil {
		t.Fatal(err)
	}
	// fresh = in+out+cw, cache reads apart.
	if r.Totals.Fresh != 6000+60+1000 || r.Totals.CR != 40000+400+5000 || r.Totals.Calls != 8 || r.Totals.Sub != 1 {
		t.Fatalf("totals %+v", r.Totals)
	}
	if len(r.Groups) != 2 || r.Groups[0].Key != "claude" || r.Groups[0].Fresh != 6060 || r.Groups[1].Key != "pi" {
		t.Fatalf("groups %+v", r.Groups)
	}
	var sum int64
	for _, v := range r.Series {
		sum += v
	}
	if sum != r.Totals.Fresh {
		t.Fatalf("series sum %d", sum)
	}
	if r.Totals.Unpriced != 1000 { // the pi tokens have no price
		t.Fatalf("unpriced %d", r.Totals.Unpriced)
	}
	// opus-5-5: 4/20/5/0.20 per Mtok.
	want := (1010*4 + 2020*20 + 3030*5 + 40400*0.20) / 1e6
	if d := r.Totals.USD - want; d > 1e-9 || d < -1e-9 {
		t.Fatalf("usd %v want %v", r.Totals.USD, want)
	}
	// Hit = CR / (CR+CW+In)
	if got, w := r.Cache.Hit, float64(45400)/float64(45400+3030+1510); got-w > 1e-9 || w-got > 1e-9 {
		t.Fatalf("hit %v want %v", got, w)
	}
	// Day buckets: the two claude hours are a day apart.
	if len(r.Buckets) < 4 || r.Groups[0].Series[1] == r.Groups[0].Series[2] && r.Groups[0].Series[1] == 0 {
		t.Fatalf("buckets %v series %v", r.Buckets, r.Groups[0].Series)
	}
	// By token: four groups, cache reads apart.
	q.By = "token"
	r, _ = st.Run(q)
	if len(r.Groups) != 4 || !r.Groups[3].Apart || r.Groups[3].Key != "cache read" || r.Groups[3].CR != 45400 {
		t.Fatalf("token groups %+v", r.Groups)
	}
	// Filter and top lists.
	q.By, q.Filter = "handoff", map[string]string{"project": "comebye"}
	r, _ = st.Run(q)
	if len(r.Groups) != 1 || r.Groups[0].Key != "h1" || r.Groups[0].Label != "w1" || len(r.TopSessions) != 1 || len(r.TopHandoffs) != 1 || r.TopHandoffs[0].Sessions != 1 {
		t.Fatalf("filtered %+v", r)
	}
	// A window excluding the second hour.
	q.Filter, q.To = nil, t0.Add(2*time.Hour)
	q.By = "model"
	r, _ = st.Run(q)
	if r.Totals.CR != 45000 {
		t.Fatalf("window %+v", r.Totals)
	}
	if _, err := st.Run(Query{From: t0, To: t0, By: "kind"}); err == nil {
		t.Fatal("empty window accepted")
	}
	if _, err := st.Run(Query{From: t0, To: t0.Add(time.Hour), By: "nonsense"}); err == nil {
		t.Fatal("bad dimension accepted")
	}
}

func TestDayBucketsFollowTheZone(t *testing.T) {
	st := newStore(t)
	late := time.Date(2026, 11, 5, 2, 0, 0, 0, time.UTC) // 21:00 on the 4th at UTC-5
	st.mu.Lock()
	s := &Session{Host: "box", Kind: "claude", ID: "z", Last: late.Unix(), First: late.Unix(), Rows: []Row{{HourOf(late), "m", Tokens{In: 1}}}}
	s.index()
	st.sessions[s.Key()] = s
	st.mu.Unlock()
	from := time.Date(2026, 11, 4, 5, 0, 0, 0, time.UTC) // local midnight on the 4th
	r, err := st.Run(Query{From: from, To: from.Add(48 * time.Hour), By: "kind", Bucket: "day", TZ: -300})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Buckets) != 2 || r.Buckets[0] != from.Unix() || r.Series[0] != 1 || r.Series[1] != 0 {
		t.Fatalf("buckets %v series %v", r.Buckets, r.Series)
	}
}

func TestRetentionRollsUpAndKeepsTotals(t *testing.T) {
	st := fixture(t)
	q := Query{From: t0.Add(-400 * 24 * time.Hour), To: t0.Add(72 * time.Hour), By: "project", Bucket: "day", Prices: Effective(nil)}
	before, _ := st.Run(q)
	// 200 days later the sessions are past retention: per-session rows go, hourly rollups stay.
	st.SetClock(func() time.Time { return clock().Add(200 * 24 * time.Hour) })
	if n := st.Prune(); n != 2 {
		t.Fatalf("pruned %d", n)
	}
	st.mu.Lock()
	left := len(st.sessions)
	st.mu.Unlock()
	after, err := st.Run(q)
	if err != nil {
		t.Fatal(err)
	}
	if left != 0 || len(after.TopSessions) != 0 {
		t.Fatalf("sessions left %d, top %d", left, len(after.TopSessions))
	}
	if after.Totals.Tokens != before.Totals.Tokens || after.Totals.USD != before.Totals.USD {
		t.Fatalf("totals changed: %+v vs %+v", after.Totals, before.Totals)
	}
	if len(after.Groups) != len(before.Groups) {
		t.Fatalf("groups %d vs %d", len(after.Groups), len(before.Groups))
	}
	// A young session is kept.
	st.SetClock(func() time.Time { return clock().Add(10 * 24 * time.Hour) })
	st2 := fixture(t)
	st2.SetClock(func() time.Time { return clock().Add(10 * 24 * time.Hour) })
	if st2.Prune() != 0 {
		t.Fatal("pruned a young session")
	}
	// Rollups survive a save and reopen.
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	re, err := Open(st.path, "box")
	if err != nil {
		t.Fatal(err)
	}
	re.SetClock(func() time.Time { return clock().Add(200 * 24 * time.Hour) })
	again, _ := re.Run(q)
	if again.Totals.Tokens != before.Totals.Tokens {
		t.Fatalf("reopened totals %+v", again.Totals)
	}
}

func TestGatherSnapshots(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "p", "s.jsonl")
	write(t, f, cl("a", "2026-11-05T10:00:00Z", "m", 1, 1, 0, 0, ""))
	peer := newStore(t)
	scan(t, peer, Source{"claude", dir})
	hub := newStore(t)
	hub.host = "hub"

	snap := peer.SnapshotSince(0, 0)
	if len(snap.Sessions) != 1 || snap.Sessions[0].L != (Labels{}) {
		t.Fatalf("snapshot %+v", snap)
	}
	if n := hub.Merge("peer1", snap); n != 1 {
		t.Fatalf("merged %d", n)
	}
	since, epoch := hub.PeerCursor("peer1")
	if since != snap.Cursor || epoch != peer.epoch {
		t.Fatalf("cursor %d %q", since, epoch)
	}
	// Nothing changed: nothing comes.
	if again := peer.SnapshotSince(since, 0); len(again.Sessions) != 0 {
		t.Fatalf("again %+v", again)
	}
	// Growth: the whole session comes again and replaces the hub's copy (no double count).
	appendTo(t, f, cl("b", "2026-11-05T10:05:00Z", "m", 2, 2, 0, 0, ""))
	scan(t, peer, Source{"claude", dir})
	hub.Merge("peer1", peer.SnapshotSince(since, 0))
	hub.mu.Lock()
	s := hub.sessions["peer1/claude/s"]
	hub.mu.Unlock()
	if s == nil || s.Total().In != 3 || s.Host != "peer1" {
		t.Fatalf("hub copy %+v", s)
	}
	// Labels the hub gave survive the next merge, and a guess does not replace a ledger label.
	hub.Relabel(func(host, home string, s *Session) Labels { return Labels{Project: "p", Src: SrcLedger} })
	appendTo(t, f, cl("c", "2026-11-05T10:06:00Z", "m", 1, 1, 0, 0, ""))
	scan(t, peer, Source{"claude", dir})
	hub.Merge("peer1", peer.SnapshotSince(0, 0))
	hub.Relabel(func(host, home string, s *Session) Labels { return Labels{Project: "guess"} })
	hub.mu.Lock()
	got := hub.sessions["peer1/claude/s"].L
	hub.mu.Unlock()
	if got.Project != "p" || got.Src != SrcLedger {
		t.Fatalf("labels %+v", got)
	}
	// Sessions already past retention are not taken (they may be rolled up).
	old := Snapshot{Epoch: "e", Sessions: []*Session{{Host: "peer1", Kind: "claude", ID: "ancient", Last: clock().Add(-SessionRetention - time.Hour).Unix()}}}
	if n := hub.Merge("peer1", old); n != 0 {
		t.Fatalf("took an expired session")
	}
}

func TestPricesOverlay(t *testing.T) {
	e := Effective(map[string]Price{"homelab/glm": {1, 2, 0, 0.1}, "claude-haiku-4-5": {}})
	if _, ok := e.Lookup("claude-haiku-4-5"); ok {
		t.Fatal("zero entry did not remove the default")
	}
	if p, ok := e.Lookup("claude-opus-4-8-20260101"); !ok || p.In != 5 {
		t.Fatalf("prefix lookup %+v %v", p, ok)
	}
	if usd, ok := e.USD("homelab/glm", Tokens{In: 1e6, Out: 1e6}); !ok || usd != 3 {
		t.Fatalf("usd %v %v", usd, ok)
	}
	if _, ok := e.USD("gpt-5.6-terra", Tokens{In: 1}); ok {
		t.Fatal("an unpriced model got a price")
	}
}

package archive

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/jhjaggars/comebye/internal/gitfetch"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A thread is one piece of work: every session (any host, any agent,
// subagents included) tied together by the same handoff, plan, backlog
// ref, or repo+branch. Recall answers per thread, so "the KubeVirt PoC" is
// one row with its outcome, not forty files.
//
// The outcome is taken from structured sources, each recorded with where
// it came from: the ledger (status, final report, discard marks, what was
// unmerged at removal), git (is the commit on main), gh (PR state), and
// the tool calls in the transcripts (docs written). Anything inferred from
// the conversation itself (a user's "go with X" line, a deploy/ branch) is
// labelled as a heuristic.

// SyncOptions say which outcome sources to consult when indexing.
type SyncOptions struct {
	Git bool // check commits against the local checkout's main
	GH  bool // refresh PR state with `gh` (network); cached between runs
	// AllHosts indexes every host in the store, not only this one and its workers.
	AllHosts bool
	// GitCached keeps the verdict each commit already has, and asks only about
	// commits with none, with no fetch. See SyncIfChanged.
	GitCached bool
	// SkipOptimize leaves out merging the full-text index.
	SkipOptimize bool
	// NoThreads leaves the thread tables as they are.
	NoThreads bool
}

// ThreadStats reports the thread pass.
type ThreadStats struct {
	Threads int
	Errors  []string
}

const threadSchema = `
DROP TABLE IF EXISTS threads;
DROP TABLE IF EXISTS thread_sessions;
DROP TABLE IF EXISTS outcomes;
DROP TABLE IF EXISTS thread_fts;
CREATE TABLE threads(id TEXT PRIMARY KEY, host TEXT, title TEXT, brief TEXT, started TEXT, ended TEXT,
  n_sessions INT, hosts TEXT, repos TEXT, branches TEXT, verdict TEXT, outcome TEXT, keywords TEXT,
  summary TEXT, lead_kind TEXT, lead_id TEXT);
CREATE TABLE thread_sessions(thread_id TEXT, host TEXT, kind TEXT, session_id TEXT);
CREATE INDEX thread_sessions_t ON thread_sessions(thread_id);
CREATE INDEX thread_sessions_s ON thread_sessions(host,kind,session_id);
CREATE TABLE outcomes(thread_id TEXT, kind TEXT, ref TEXT, text TEXT, source TEXT, at TEXT);
CREATE INDEX outcomes_t ON outcomes(thread_id);
CREATE VIRTUAL TABLE thread_fts USING fts5(title, brief, outcome, keywords, digest, decisions, thread_id UNINDEXED, tokenize='porter unicode61');
CREATE TABLE IF NOT EXISTS gh_prs(repo TEXT, branch TEXT, fetched TEXT, json TEXT, PRIMARY KEY(repo,branch));
`

type sess struct {
	key, host, kind, id, repo, branch, lbranch, ledger, plan, ref, cwd string
	started, ended, title, first, brief                                string
	nmsgs                                                              int
}

type uf map[string]string

func (u uf) find(x string) string {
	if _, ok := u[x]; !ok {
		u[x] = x
	}
	for u[x] != x {
		u[x] = u[u[x]]
		x = u[x]
	}
	return x
}
func (u uf) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u[ra] = rb
	}
}

func sget(r map[string]any, k string) string {
	switch v := r[k].(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprint(r[k])
}

var genericBranch = map[string]bool{"": true, "HEAD": true, "main": true, "master": true}

type outcome struct{ kind, ref, text, source, at string }

type thread struct {
	id         string
	ss         []*sess
	outs       []outcome
	decisions  []string
	verdict    string
	outcomeTxt string
	keywords   []string
	summary    string
	report     string
	text       strings.Builder
}

// builtThreads is the index's threads, ready to write.
type builtThreads struct {
	threads []*thread
	sums    map[string]string
}

// threadID names a thread after its first session. The thread keeps the id
// when later sessions join it from any host, unless an earlier one joins.
func threadID(first *sess) string {
	h := sha1.Sum([]byte(first.host + "|" + first.kind + "/" + first.id))
	return "t-" + hex.EncodeToString(h[:])[:8]
}

// buildThreads replaces the thread tables with the threads of every session in
// the index, on whichever host. thisHost is the host this machine is, whose
// sessions' paths can be looked at here. Everything is read first, because
// reading a thread's outcome can write the PR cache, which an open write
// transaction here would block.
func (ix *Index) buildThreads(thisHost string, links []Link, opt SyncOptions) (ThreadStats, error) {
	var st ThreadStats
	built, err := ix.gatherThreads(thisHost, links, opt, &st)
	if err != nil {
		return st, err
	}
	w, err := ix.writer()
	if err != nil {
		return st, err
	}
	// In one transaction with the drops, so a reader never finds no threads.
	w.exec("BEGIN;")
	w.exec(threadSchema)
	built.write(w, &st)
	w.exec("COMMIT;")
	return st, w.close()
}

func (ix *Index) gatherThreads(thisHost string, links []Link, opt SyncOptions, st *ThreadStats) (*builtThreads, error) {
	rows, err := ix.Query("SELECT host,kind,id,repo,branch,ledger_branch,ledger_id,plan_id,ref,cwd,started,ended,title,first_prompt,brief,n_msgs,summary FROM sessions ORDER BY started,host,kind,id;")
	if err != nil {
		return nil, err
	}
	byLedger := map[string]*Link{}
	for i := range links {
		byLedger[links[i].ID] = &links[i]
	}
	var all []*sess
	sums := map[string]string{}
	u := uf{}
	for _, r := range rows {
		s := &sess{host: sget(r, "host"), kind: sget(r, "kind"), id: sget(r, "id"), repo: sget(r, "repo"), branch: sget(r, "branch"), lbranch: sget(r, "ledger_branch"),
			ledger: sget(r, "ledger_id"), plan: sget(r, "plan_id"), ref: sget(r, "ref"), cwd: sget(r, "cwd"), started: sget(r, "started"), ended: sget(r, "ended"),
			title: sget(r, "title"), first: sget(r, "first_prompt"), brief: sget(r, "brief")}
		fmt.Sscan(sget(r, "n_msgs"), &s.nmsgs)
		s.key = sessKey(s.host, s.kind, s.id)
		all = append(all, s)
		sums[s.key] = sget(r, "summary")
		u.find(s.key)
		if s.ledger != "" {
			u.union(s.key, "ledger:"+s.ledger)
		}
		if s.plan != "" {
			u.union(s.key, "plan:"+s.plan)
		}
		if s.ref != "" {
			u.union(s.key, "ref:"+s.ref)
		}
		b := s.lbranch
		if b == "" {
			b = s.branch
		}
		if !genericBranch[b] && s.repo != "" {
			u.union(s.key, "br:"+s.repo+"@"+b)
		}
	}
	groups := map[string]*thread{}
	for _, s := range all {
		root := u.find(s.key)
		t := groups[root]
		if t == nil {
			t = &thread{}
			groups[root] = t
		}
		t.ss = append(t.ss, s)
	}
	var threads []*thread
	for _, t := range groups {
		// The rows are already in order, with ties settled by host, kind and id.
		t.id = threadID(t.ss[0])
		threads = append(threads, t)
	}
	sort.Slice(threads, func(i, j int) bool {
		if a, b := threads[i].ss[0].started, threads[j].ss[0].started; a != b {
			return a < b
		}
		return threads[i].id < threads[j].id
	})

	g := &gitCtx{host: thisHost, enabled: opt.Git, cache: map[string]*repoInfo{}, fetched: map[string]bool{}, errs: &st.Errors}
	if opt.Git && opt.GitCached {
		g.noFetch, g.known = true, ix.knownCommits()
	}
	ghc := map[string][]ghPR{}
	if opt.GH {
		g.loadGH(ix)
	}
	if prs, err := ix.cachedPRs(); err == nil {
		ghc = prs
	}

	in, err := ix.loadThreadInputs()
	if err != nil {
		return nil, err
	}

	// document frequency for keywords
	var texts []map[string]int
	for _, t := range threads {
		t.gather(in, byLedger, sums, g, ghc, opt)
		texts = append(texts, termCounts(t.text.String()))
	}
	df := map[string]int{}
	for _, tc := range texts {
		for term := range tc {
			df[term]++
		}
	}
	for i, t := range threads {
		t.keywords = topTerms(texts[i], df, len(threads), 14)
		if opt.GH {
			// fetched below; PR outcomes were added in gather from cache
		}
		t.finish()
	}
	return &builtThreads{threads: threads, sums: sums}, nil
}

func (bt *builtThreads) write(w *stmtWriter, st *ThreadStats) {
	sums := bt.sums
	for _, t := range bt.threads {
		st.Threads++
		first := t.ss[0]
		hosts, repos, branches := t.sets()
		lead := t.lead()
		w.exec(fmt.Sprintf("INSERT INTO threads VALUES(%s,%s,%s,%s,%s,%s,%d,%s,%s,%s,%s,%s,%s,%s,%s,%s);",
			q(t.id), q(first.host), q(t.title()), q(clip(t.brief(), 600)), q(first.started), q(t.end()), len(t.ss), q(hosts), q(repos), q(branches),
			q(t.verdict), q(t.outcomeTxt), q(strings.Join(t.keywords, " ")), q(t.summary), q(lead.kind), q(lead.id)))
		for _, s := range t.ss {
			w.exec(fmt.Sprintf("INSERT INTO thread_sessions VALUES(%s,%s,%s,%s);", q(t.id), q(s.host), q(s.kind), q(s.id)))
		}
		var dec []string
		for _, o := range t.outs {
			w.exec(fmt.Sprintf("INSERT INTO outcomes VALUES(%s,%s,%s,%s,%s,%s);", q(t.id), q(o.kind), q(o.ref), q(maskPlain(o.text)), q(o.source), q(o.at)))
		}
		dec = append(dec, t.decisions...)
		w.exec(fmt.Sprintf("INSERT INTO thread_fts(title,brief,outcome,keywords,digest,decisions,thread_id) VALUES(%s,%s,%s,%s,%s,%s,%s);",
			q(t.title()), q(t.brief()), q(t.outcomeTxt+" "+t.report), q(strings.Join(t.keywords, " ")), q(t.digest(sums)), q(strings.Join(dec, "\n")), q(t.id)))
	}
}

func (t *thread) end() string {
	e := ""
	for _, s := range t.ss {
		if s.ended > e {
			e = s.ended
		}
	}
	return e
}

func (t *thread) lead() *sess {
	for _, s := range t.ss {
		if s.ledger != "" {
			return s
		}
	}
	best := t.ss[0]
	for _, s := range t.ss {
		if s.nmsgs > best.nmsgs {
			best = s
		}
	}
	return best
}

func (t *thread) brief() string {
	for _, s := range t.ss {
		if s.brief != "" {
			return s.brief
		}
	}
	return ""
}

func (t *thread) title() string {
	// Claude's own session title is the best label; then the branch the
	// work was done on; then the first prompt; the handoff brief last
	// (it usually starts with boilerplate).
	if l := t.lead(); l.kind == "claude" && l.title != "" && l.ledger != "" {
		return clip(l.title, 110)
	}
	for i := len(t.ss) - 1; i >= 0; i-- {
		if s := t.ss[i]; s.kind == "claude" && s.title != "" {
			return clip(s.title, 110)
		}
	}
	for _, s := range t.ss {
		b := s.lbranch
		if b == "" {
			b = s.branch
		}
		if !genericBranch[b] {
			return clip(s.repo+" "+b, 110)
		}
	}
	l := t.lead()
	if l.first != "" {
		return clip(l.first, 110)
	}
	return clip(strings.TrimSpace(strings.SplitN(strings.TrimPrefix(strings.TrimSpace(t.brief()), "#"), "\n", 2)[0]), 110)
}

func (t *thread) sets() (hosts, repos, branches string) {
	h, r, b := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, s := range t.ss {
		h[s.host] = true
		if s.repo != "" {
			r[s.repo] = true
		}
		br := s.lbranch
		if br == "" {
			br = s.branch
		}
		if br != "" {
			b[br] = true
		}
	}
	join := func(m map[string]bool) string {
		var l []string
		for k := range m {
			l = append(l, k)
		}
		sort.Strings(l)
		return strings.Join(l, ",")
	}
	return join(h), join(r), join(b)
}

func (t *thread) digest(sums map[string]string) string {
	var sb strings.Builder
	sb.WriteString(t.summary + "\n")
	for _, s := range t.ss {
		sb.WriteString(s.title + " " + s.first + "\n")
		sb.WriteString(sums[s.key] + "\n")
	}
	return clip(sb.String(), 8000)
}

// sessKey names a session in the thread pass. The host is part of it: a session
// id is only unique within a host.
func sessKey(host, kind, id string) string { return host + "/" + kind + "/" + id }

// threadInputs is what gather reads from the messages and commits of every
// session, by sessKey. It is read once: a query per thread is what made the
// hub's build take nine minutes (threads x queries x messages).
type threadInputs struct {
	commits, docs, users, prose map[string][]map[string]any
	last                        map[string]string
}

// proseBytes is how much of a session's prose can reach the keywords: termCounts
// stops at 60000 bytes, so later rows of a session cannot matter.
const proseBytes = 60000

func (ix *Index) loadThreadInputs() (*threadInputs, error) {
	in := &threadInputs{last: map[string]string{}}
	load := func(sql string) (map[string][]map[string]any, error) {
		rows, err := ix.Query(sql)
		if err != nil {
			return nil, err
		}
		out := map[string][]map[string]any{}
		for _, r := range rows {
			k := sessKey(sget(r, "host"), sget(r, "kind"), sget(r, "sid"))
			out[k] = append(out[k], r)
		}
		return out, nil
	}
	var err error
	if in.commits, err = load("SELECT c.host AS host, c.kind AS kind, c.session_id AS sid, c.ref AS sha, c.rel, c.branch, c.title AS subject, s.cwd, s.repo, s.ledger_id FROM produced c JOIN sessions s ON s.host=c.host AND s.kind=c.kind AND s.id=c.session_id WHERE c.type='commit' AND c.rel IN ('made','pushed') ORDER BY c.rowid;"); err != nil {
		return nil, err
	}
	if in.docs, err = load("SELECT host, kind, session_id AS sid, path FROM artifacts;"); err != nil {
		return nil, err
	}
	if in.users, err = load("SELECT host, kind, session_id AS sid, ts, text FROM messages WHERE role='user' AND sub='' AND length(text) < 700 ORDER BY ts;"); err != nil {
		return nil, err
	}
	if in.prose, err = load(fmt.Sprintf(`SELECT host, kind, sid, seq, text FROM (
  SELECT host, kind, session_id AS sid, seq, substr(text,1,1200) AS text,
    row_number() OVER w AS n, sum(min(length(text),1200)) OVER w AS upto
  FROM messages WHERE role IN ('user','assistant') AND sub='' AND seq>=0
  WINDOW w AS (PARTITION BY host, kind, session_id ORDER BY seq))
WHERE n <= 400 AND upto - 1200 <= %d ORDER BY seq;`, proseBytes)); err != nil {
		return nil, err
	}
	rows, err := ix.Query("SELECT host, kind, session_id AS sid, substr(text,1,600) AS text, max(seq) AS seq FROM messages WHERE role='assistant' AND sub='' AND seq>=0 GROUP BY host, kind, session_id;")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		in.last[sessKey(sget(r, "host"), sget(r, "kind"), sget(r, "sid"))] = sget(r, "text")
	}
	return in, nil
}

// of returns one table's rows for the thread's sessions.
func (t *thread) of(table map[string][]map[string]any) []map[string]any {
	var out []map[string]any
	for _, s := range t.ss {
		out = append(out, table[s.key]...)
	}
	return out
}

// gather collects the outcome evidence for a thread.
func (t *thread) gather(in *threadInputs, byLedger map[string]*Link, sums map[string]string, g *gitCtx, prs map[string][]ghPR, opt SyncOptions) {
	seen := map[string]bool{}
	// Ledger: what the hub recorded.
	for _, s := range t.ss {
		if s.ledger == "" || seen[s.ledger] {
			continue
		}
		seen[s.ledger] = true
		l := byLedger[s.ledger]
		if l == nil {
			continue
		}
		at := l.FinishedAt
		switch {
		case l.Mark != nil && l.Mark.Discard:
			t.outs = append(t.outs, outcome{"abandoned", s.ledger, "marked for discard: " + clip(l.Mark.Reason, 120), "ledger mark", l.RemovedAt})
		case l.Removal != nil && l.Removal.Discarded:
			t.outs = append(t.outs, outcome{"abandoned", s.ledger, "worktree discarded at removal", "ledger removal", l.RemovedAt})
		}
		t.outs = append(t.outs, outcome{"handoff", s.ledger, "handoff " + s.ledger + " " + l.Status, "ledger", at})
		if l.Removal != nil {
			t.outs = append(t.outs, outcome{"unmerged", s.ledger, fmt.Sprintf("%d commits not merged into base when the worker was removed", l.Removal.Checks.Unmerged), "ledger removal check", l.RemovedAt})
		} else if l.Checks != nil && l.Status != "running" {
			t.outs = append(t.outs, outcome{"unmerged", s.ledger, fmt.Sprintf("%d commits not merged into base (last check)", l.Checks.Unmerged), "ledger check", ""})
		}
		if l.FinalReport != "" {
			r := reportSummary(l.FinalReport)
			src := "ledger report"
			if l.ReportSource == "explicit" {
				src = "ledger report (worker's own `comebye report`)"
			} else {
				src = "ledger report (hub took the last reply)"
			}
			t.outs = append(t.outs, outcome{"report", s.ledger, r, src, at})
			if t.report == "" || at > "" {
				t.report = r
			}
		}
	}
	// Commits and their state on main.
	done := map[string]bool{}
	for _, r := range t.of(in.commits) {
		sha := sget(r, "sha")
		if done[sha] {
			continue
		}
		done[sha] = true
		var link *Link
		if l := sget(r, "ledger_id"); l != "" {
			link = byLedger[l]
		}
		state, how := g.landed(sget(r, "host"), sget(r, "cwd"), sget(r, "repo"), link, sha, sget(r, "subject"))
		txt := fmt.Sprintf("%s %s", sha[:min(8, len(sha))], clip(sget(r, "subject"), 80))
		t.outs = append(t.outs, outcome{"commit-" + state, sha, txt, how, ""})
	}
	// Pull requests (cached gh results).
	for _, s := range t.ss {
		b := s.lbranch
		if b == "" {
			b = s.branch
		}
		if genericBranch[b] {
			continue
		}
		repoDir := g.repoDir(s.host, s.cwd, s.repo, byLedger[s.ledger])
		for _, pr := range prs[repoDir+"\x00"+b] {
			if seen["pr"+fmt.Sprint(pr.Number)+repoDir] {
				continue
			}
			seen["pr"+fmt.Sprint(pr.Number)+repoDir] = true
			t.outs = append(t.outs, outcome{"pr-" + strings.ToLower(pr.State), fmt.Sprint(pr.Number), fmt.Sprintf("PR #%d %s: %s", pr.Number, strings.ToLower(pr.State), clip(pr.Title, 80)), "gh " + pr.URL, pr.MergedAt})
		}
		if strings.HasPrefix(b, "deploy/") {
			t.outs = append(t.outs, outcome{"deploy", b, "a deploy branch: " + b, "branch name (heuristic)", s.started})
		}
	}
	// Docs written.
	docs := map[string]bool{}
	for _, r := range t.of(in.docs) {
		if p := sget(r, "path"); !docs[p] {
			docs[p] = true
			t.outs = append(t.outs, outcome{"doc", p, shortPath(p), "tool call in transcript", ""})
		}
	}
	// Decisions: the user's own lines that sound like one (a heuristic).
	urows := t.of(in.users)
	sort.SliceStable(urows, func(i, j int) bool { return sget(urows[i], "ts") < sget(urows[j], "ts") })
	for _, r := range urows {
		txt := sget(r, "text")
		if decisionRE.MatchString(txt) && !strings.HasPrefix(txt, "You are a worker agent") && len(t.decisions) < 5 {
			t.decisions = append(t.decisions, clip(strings.Join(strings.Fields(txt), " "), 240))
			t.outs = append(t.outs, outcome{"decision", sget(r, "sid"), clip(strings.Join(strings.Fields(txt), " "), 240), "user message (keyword heuristic)", sget(r, "ts")})
		}
	}
	// No structured report: show how the last session ended, labelled as
	// the agent's own words.
	if t.report == "" {
		last := t.ss[len(t.ss)-1]
		if txt, ok := in.last[last.key]; ok {
			t.outs = append(t.outs, outcome{"last-message", last.id, clip(strings.Join(strings.Fields(txt), " "), 320), "the agent's last reply (its own claim, not verified)", last.ended})
		}
	}
	// Text for keywords: titles, briefs, user and assistant prose.
	for _, s := range t.ss {
		t.text.WriteString(s.title + " " + s.first + " " + s.brief + " " + sums[s.key] + "\n")
	}
	trows := t.of(in.prose)
	if len(t.ss) > 1 {
		sort.SliceStable(trows, func(i, j int) bool { return seqOf(trows[i]) < seqOf(trows[j]) })
	}
	if len(trows) > 400 {
		trows = trows[:400]
	}
	for _, r := range trows {
		t.text.WriteString(sget(r, "text") + "\n")
	}
	for _, o := range t.outs {
		if o.kind == "report" || o.kind == "last-message" || strings.HasPrefix(o.kind, "commit-") || o.kind == "doc" {
			t.text.WriteString(o.text + " ")
		}
	}
	for _, s := range t.ss {
		if sums[s.key] != "" && t.summary == "" {
			t.summary = sums[s.key]
		}
	}
}

func seqOf(r map[string]any) float64 {
	n, _ := r["seq"].(float64)
	return n
}

var decisionRE = regexp.MustCompile(`(?i)\b(decided|decision|let'?s go with|go with|we'?ll go|approved?|lgtm|ship it|abandon|scrap|drop (it|this)|don'?t (do|bother)|instead of|rather than|the plan is)\b`)

func shortPath(p string) string {
	if i := strings.LastIndex(p, "/docs/"); i >= 0 {
		return p[i+1:]
	}
	return filepath.Base(p)
}

// reportSummary is the "Summary" paragraph of a worker's report.
func reportSummary(rep string) string {
	rep = strings.TrimSpace(rep)
	if i := strings.Index(rep, "## Summary"); i >= 0 {
		rep = rep[i+len("## Summary"):]
	}
	if i := strings.Index(rep, "\n## "); i >= 0 {
		rep = rep[:i]
	}
	return clip(strings.Join(strings.Fields(rep), " "), 420)
}

func (t *thread) hasReport() bool {
	for _, o := range t.outs {
		if o.kind == "report" {
			return true
		}
	}
	return false
}

// finish decides the verdict and the one-line outcome.
func (t *thread) finish() {
	has := func(prefix string) []outcome {
		var o []outcome
		for _, x := range t.outs {
			if strings.HasPrefix(x.kind, prefix) {
				o = append(o, x)
			}
		}
		return o
	}
	landed, notIn, unknown := 0, 0, 0
	for _, c := range has("commit-") {
		switch c.kind {
		case "commit-landed":
			landed++
		case "commit-notinmain":
			notIn++
		default:
			unknown++
		}
	}
	var mergedPR, openPR, closedPR []outcome
	for _, p := range has("pr-") {
		switch p.kind {
		case "pr-merged":
			mergedPR = append(mergedPR, p)
		case "pr-open":
			openPR = append(openPR, p)
		default:
			closedPR = append(closedPR, p)
		}
	}
	abandoned := len(has("abandoned")) > 0
	var parts []string
	switch {
	case len(mergedPR) > 0 || landed > 0:
		t.verdict = "landed"
		if landed > 0 {
			parts = append(parts, fmt.Sprintf("%d of %d commits are on main", landed, landed+notIn+unknown))
		}
		if len(mergedPR) > 0 {
			parts = append(parts, fmt.Sprintf("%d PR merged", len(mergedPR)))
		}
	case abandoned:
		t.verdict = "abandoned"
		parts = append(parts, has("abandoned")[0].text)
	case len(openPR) > 0:
		t.verdict = "open"
		parts = append(parts, "PR still open")
	case len(closedPR) > 0:
		t.verdict = "abandoned"
		parts = append(parts, "PR closed unmerged")
	case notIn > 0:
		t.verdict = "unlanded"
		parts = append(parts, fmt.Sprintf("%d commits not on main", notIn))
	case t.hasReport():
		t.verdict = "reported"
		parts = append(parts, "the worker finished and reported; no commit or PR found")
	default:
		t.verdict = "unknown"
		parts = append(parts, "no landed commit or PR found")
	}
	if len(has("deploy")) > 0 {
		parts = append(parts, "deploy branch")
	}
	for _, h := range has("handoff") {
		if strings.HasSuffix(h.text, "failed") {
			parts = append(parts, "a handoff failed")
			if t.verdict == "unknown" {
				t.verdict = "failed"
			}
			break
		}
	}
	t.outcomeTxt = strings.Join(parts, "; ")
}

// ----- git -----

type repoInfo struct {
	dir, base string
	ok        bool
}

type gitCtx struct {
	host    string // the host this machine is
	enabled bool
	cache   map[string]*repoInfo
	fetched map[string]bool // by the repository's common git directory
	errs    *[]string
	noFetch bool
	known   map[string]knownCommit // verdicts to keep, by full sha
}

// knownCommit is a verdict from an earlier git pass.
type knownCommit struct{ state, how string }

// knownCommits reads the verdicts in the current outcomes. An index with no
// thread tables yet has none.
func (ix *Index) knownCommits() map[string]knownCommit {
	out := map[string]knownCommit{}
	rows, err := ix.Query("SELECT ref, kind, source FROM outcomes WHERE kind IN ('commit-landed','commit-notinmain');")
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[sget(r, "ref")] = knownCommit{strings.TrimPrefix(sget(r, "kind"), "commit-"), sget(r, "source")}
	}
	return out
}

func run(dir string, timeout time.Duration, name string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return strings.TrimSpace(string(out)), code, err
}

// repoDir finds a checkout on this machine to ask git about: for a session of
// this host, the ledger's primary checkout or the session's cwd if it still
// exists; otherwise ~/code/<repo>. The ledger's path and the cwd of another
// host's session name directories on that host. The same path here could be
// something else entirely.
func (g *gitCtx) repoDir(host, cwd, repo string, l *Link) string {
	cands := []string{}
	if host == g.host {
		if l != nil && l.Repo != "" {
			cands = append(cands, l.Repo)
		}
		if cwd != "" {
			cands = append(cands, cwd)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && repo != "" {
		cands = append(cands, filepath.Join(home, "code", repo))
	}
	for _, c := range cands {
		if fi, err := os.Stat(filepath.Join(c, ".git")); err == nil {
			_ = fi
			return c
		}
	}
	return ""
}

func (g *gitCtx) info(dir string) *repoInfo {
	if ri, ok := g.cache[dir]; ok {
		return ri
	}
	ri := &repoInfo{dir: dir}
	g.cache[dir] = ri
	if !g.noFetch {
		g.fetch(dir)
	}
	for _, ref := range []string{"origin/main", "main", "origin/master", "master"} {
		if _, code, _ := run(dir, 10*time.Second, "git", "rev-parse", "--verify", "-q", ref+"^{commit}"); code == 0 {
			ri.base, ri.ok = ref, true
			break
		}
	}
	return ri
}

// fetch brings a repository's origin up to date, once per repository however
// many worktrees it has, so that work landed from another host does not read as
// not landed. A failure leaves the last-known origin/main and is reported.
func (g *gitCtx) fetch(dir string) {
	common, code, _ := run(dir, 10*time.Second, "git", "rev-parse", "--git-common-dir")
	if code != 0 {
		return
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	if g.fetched[common] {
		return
	}
	g.fetched[common] = true
	// Under the repository's fetch lock, which the server's merge checks take too.
	res := gitfetch.Once(context.Background(), dir, gitFetchWait, gitFetchTimeout)
	switch {
	case res.Busy && g.errs != nil:
		*g.errs = append(*g.errs, "git fetch skipped in "+filepath.Base(dir)+": another fetch held the repository; its commits are checked against the last-known origin")
	case res.Failed() && g.errs != nil:
		*g.errs = append(*g.errs, "git fetch failed in "+filepath.Base(dir)+" ("+res.Reason+"): its commits are checked against the last-known origin")
	}
}

// gitFetchTimeout limits one `git fetch origin`.
const gitFetchTimeout = gitfetch.DefaultTimeout

// gitFetchWait is how long the git pass waits for another fetch of the same
// repository (the server's merge checks) to finish.
var gitFetchWait = gitFetchTimeout

// landed says whether a commit is on the default branch of a local checkout:
// "landed" (an ancestor, or a commit on main with the same subject: the
// land step rebases, which changes SHAs), "notinmain", or "unchecked".
func (g *gitCtx) landed(host, cwd, repo string, l *Link, sha, subject string) (state, how string) {
	if !g.enabled {
		return "unchecked", "git not consulted"
	}
	if k, ok := g.known[sha]; ok {
		return k.state, k.how
	}
	dir := g.repoDir(host, cwd, repo, l)
	if dir == "" {
		return "unchecked", "no local checkout"
	}
	ri := g.info(dir)
	if !ri.ok {
		return "unchecked", "no main branch in " + filepath.Base(dir)
	}
	if _, code, _ := run(dir, 10*time.Second, "git", "cat-file", "-e", sha+"^{commit}"); code == 0 {
		if _, code, _ := run(dir, 10*time.Second, "git", "merge-base", "--is-ancestor", sha, ri.base); code == 0 {
			return "landed", "git: ancestor of " + ri.base
		}
	}
	if len(subject) >= 12 {
		if out, _, _ := run(dir, 20*time.Second, "git", "log", ri.base, "--fixed-strings", "--grep="+subject, "--format=%h", "-n1"); out != "" {
			return "landed", "git: a commit on " + ri.base + " with the same subject (" + out + ")"
		}
	}
	return "notinmain", "git: not on " + ri.base + " in " + filepath.Base(dir)
}

// ----- gh -----

type ghPR struct {
	Number   int    `json:"number"`
	State    string `json:"state"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	MergedAt string `json:"mergedAt"`
}

func (ix *Index) cachedPRs() (map[string][]ghPR, error) {
	rows, err := ix.Query("SELECT repo,branch,json FROM gh_prs;")
	if err != nil {
		return nil, err
	}
	out := map[string][]ghPR{}
	for _, r := range rows {
		var prs []ghPR
		if json.Unmarshal([]byte(sget(r, "json")), &prs) == nil {
			out[sget(r, "repo")+"\x00"+sget(r, "branch")] = prs
		}
	}
	return out, nil
}

// loadGH asks GitHub (through `gh`, with the user's own login) which PRs
// exist for each repo+branch the index knows, and caches the answers in the
// index. It sends branch names and repo directories' remotes only.
func (g *gitCtx) loadGH(ix *Index) {
	rows, err := ix.Query("SELECT DISTINCT host, repo, CASE WHEN ledger_branch<>'' THEN ledger_branch ELSE branch END AS br, cwd, ledger_id FROM sessions;")
	if err != nil {
		return
	}
	type key struct{ dir, br string }
	seen := map[key]bool{}
	w, err := ix.writer()
	if err != nil {
		return
	}
	w.exec(threadSchema0)
	w.exec("BEGIN;")
	for _, r := range rows {
		br := sget(r, "br")
		if genericBranch[br] {
			continue
		}
		dir := g.repoDir(sget(r, "host"), sget(r, "cwd"), sget(r, "repo"), nil)
		if dir == "" || seen[key{dir, br}] {
			continue
		}
		seen[key{dir, br}] = true
		out, code, _ := run(dir, 30*time.Second, "gh", "pr", "list", "--state", "all", "--head", br, "--limit", "5", "--json", "number,state,title,url,mergedAt")
		if code != 0 {
			continue
		}
		w.exec(fmt.Sprintf("INSERT OR REPLACE INTO gh_prs VALUES(%s,%s,%s,%s);", q(dir), q(br), q(time.Now().UTC().Format(time.RFC3339)), q(out)))
	}
	w.exec("COMMIT;")
	_ = w.close()
}

const threadSchema0 = `CREATE TABLE IF NOT EXISTS gh_prs(repo TEXT, branch TEXT, fetched TEXT, json TEXT, PRIMARY KEY(repo,branch));`

// ----- keywords -----

var tokRE = regexp.MustCompile(`[a-z][a-z0-9_-]{2,}`)

var stop = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`the and for that this with from have has had are was were been being not but you your will would should could can may might must into out over under then than them they their there these those what when where which who why how all any some more most other such only own same too very just also about after before again against between both each few here once while during through above below off onto upon via per use used using uses make made need needs want wants like get got let see look think know file files code run running runs new one two now still already yet first last next work works working task tasks thing things way ways does did done doing going gone able said say says see seen sure yes okay please thanks thank able bash tool tools result results output line lines name names path text type true false null none error errors test tests command commands agent agents session sessions user assistant comebye claude pi` + ``) {
		m[w] = true
	}
	return m
}()

func termCounts(text string) map[string]int {
	tc := map[string]int{}
	text = strings.ToLower(text)
	if len(text) > 60000 {
		text = text[:60000]
	}
	for _, w := range tokRE.FindAllString(text, -1) {
		if stop[w] || len(w) > 30 {
			continue
		}
		tc[w]++
	}
	return tc
}

func topTerms(tc map[string]int, df map[string]int, n, k int) []string {
	type ts struct {
		t string
		s float64
	}
	var l []ts
	for t, c := range tc {
		if c < 2 {
			continue
		}
		idf := math.Log(float64(n+1) / float64(df[t]))
		l = append(l, ts{t, (1 + math.Log(float64(c))) * idf})
	}
	sort.Slice(l, func(i, j int) bool {
		if l[i].s != l[j].s {
			return l[i].s > l[j].s
		}
		return l[i].t < l[j].t
	})
	var out []string
	for i := 0; i < len(l) && i < k; i++ {
		out = append(out, l[i].t)
	}
	return out
}

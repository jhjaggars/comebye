package archive

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

func (ix *Index) buildThreads(host string, links []Link, opt SyncOptions) (ThreadStats, error) {
	var st ThreadStats
	rows, err := ix.Query("SELECT kind,id,repo,branch,ledger_branch,ledger_id,plan_id,ref,cwd,started,ended,title,first_prompt,brief,n_msgs,summary FROM sessions WHERE host=" + q(host) + " ORDER BY started;")
	if err != nil {
		return st, err
	}
	byLedger := map[string]*Link{}
	for i := range links {
		byLedger[links[i].ID] = &links[i]
	}
	var all []*sess
	sums := map[string]string{}
	u := uf{}
	for _, r := range rows {
		s := &sess{host: host, kind: sget(r, "kind"), id: sget(r, "id"), repo: sget(r, "repo"), branch: sget(r, "branch"), lbranch: sget(r, "ledger_branch"),
			ledger: sget(r, "ledger_id"), plan: sget(r, "plan_id"), ref: sget(r, "ref"), cwd: sget(r, "cwd"), started: sget(r, "started"), ended: sget(r, "ended"),
			title: sget(r, "title"), first: sget(r, "first_prompt"), brief: sget(r, "brief")}
		fmt.Sscan(sget(r, "n_msgs"), &s.nmsgs)
		s.key = s.kind + "/" + s.id
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
		sort.Slice(t.ss, func(i, j int) bool { return t.ss[i].started < t.ss[j].started })
		h := sha1.Sum([]byte(host + "|" + t.ss[0].key))
		t.id = "t-" + hex.EncodeToString(h[:])[:8]
		threads = append(threads, t)
	}
	sort.Slice(threads, func(i, j int) bool { return threads[i].ss[0].started < threads[j].ss[0].started })

	g := &gitCtx{host: host, enabled: opt.Git, cache: map[string]*repoInfo{}}
	ghc := map[string][]ghPR{}
	if opt.GH {
		g.loadGH(ix)
	}
	if prs, err := ix.cachedPRs(); err == nil {
		ghc = prs
	}

	w, err := ix.writerNoSchema(threadSchema)
	if err != nil {
		return st, err
	}
	w.exec("BEGIN;")
	// document frequency for keywords
	var texts []map[string]int
	for _, t := range threads {
		t.gather(ix, byLedger, sums, g, ghc, opt)
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
		st.Threads++
		first := t.ss[0]
		hosts, repos, branches := t.sets()
		lead := t.lead()
		w.exec(fmt.Sprintf("INSERT INTO threads VALUES(%s,%s,%s,%s,%s,%s,%d,%s,%s,%s,%s,%s,%s,%s,%s,%s);",
			q(t.id), q(host), q(t.title()), q(clip(t.brief(), 600)), q(first.started), q(t.end()), len(t.ss), q(hosts), q(repos), q(branches),
			q(t.verdict), q(t.outcomeTxt), q(strings.Join(t.keywords, " ")), q(t.summary), q(lead.kind), q(lead.id)))
		for _, s := range t.ss {
			w.exec(fmt.Sprintf("INSERT INTO thread_sessions VALUES(%s,%s,%s,%s);", q(t.id), q(host), q(s.kind), q(s.id)))
		}
		var dec []string
		for _, o := range t.outs {
			w.exec(fmt.Sprintf("INSERT INTO outcomes VALUES(%s,%s,%s,%s,%s,%s);", q(t.id), q(o.kind), q(o.ref), q(o.text), q(o.source), q(o.at)))
		}
		dec = append(dec, t.decisions...)
		w.exec(fmt.Sprintf("INSERT INTO thread_fts(title,brief,outcome,keywords,digest,decisions,thread_id) VALUES(%s,%s,%s,%s,%s,%s,%s);",
			q(t.title()), q(t.brief()), q(t.outcomeTxt+" "+t.report), q(strings.Join(t.keywords, " ")), q(t.digest(sums)), q(strings.Join(dec, "\n")), q(t.id)))
	}
	w.exec("COMMIT;")
	return st, w.close()
}

func (ix *Index) writerNoSchema(pre string) (*stmtWriter, error) {
	w, err := ix.writer()
	if err != nil {
		return nil, err
	}
	w.exec(pre)
	return w, nil
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

// gather collects the outcome evidence for a thread.
func (t *thread) gather(ix *Index, byLedger map[string]*Link, sums map[string]string, g *gitCtx, prs map[string][]ghPR, opt SyncOptions) {
	seen := map[string]bool{}
	var ids []string
	for _, s := range t.ss {
		ids = append(ids, q(s.id))
	}
	in := strings.Join(ids, ",")
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
	rows, _ := ix.Query(fmt.Sprintf("SELECT c.sha, c.rel, c.branch, c.subject, s.cwd, s.repo, s.ledger_id FROM commits c JOIN sessions s ON s.host=c.host AND s.kind=c.kind AND s.id=c.session_id WHERE c.host=%s AND c.session_id IN (%s) AND c.rel IN ('made','pushed');", q(g.host), in))
	done := map[string]bool{}
	for _, r := range rows {
		sha := sget(r, "sha")
		if done[sha] {
			continue
		}
		done[sha] = true
		var link *Link
		if l := sget(r, "ledger_id"); l != "" {
			link = byLedger[l]
		}
		state, how := g.landed(sget(r, "cwd"), sget(r, "repo"), link, sha, sget(r, "subject"))
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
		repoDir := g.repoDir(s.cwd, s.repo, byLedger[s.ledger])
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
	drows, _ := ix.Query(fmt.Sprintf("SELECT DISTINCT path FROM artifacts WHERE host=%s AND session_id IN (%s);", q(g.host), in))
	for _, r := range drows {
		t.outs = append(t.outs, outcome{"doc", sget(r, "path"), shortPath(sget(r, "path")), "tool call in transcript", ""})
	}
	// Decisions: the user's own lines that sound like one (a heuristic).
	urows, _ := ix.Query(fmt.Sprintf("SELECT session_id, ts, text FROM msgs WHERE host=%s AND session_id IN (%s) AND role='user' AND sub='' AND length(text) < 700 ORDER BY ts;", q(g.host), in))
	for _, r := range urows {
		txt := sget(r, "text")
		if decisionRE.MatchString(txt) && !strings.HasPrefix(txt, "You are a worker agent") && len(t.decisions) < 5 {
			t.decisions = append(t.decisions, clip(strings.Join(strings.Fields(txt), " "), 240))
			t.outs = append(t.outs, outcome{"decision", sget(r, "session_id"), clip(strings.Join(strings.Fields(txt), " "), 240), "user message (keyword heuristic)", sget(r, "ts")})
		}
	}
	// No structured report: show how the last session ended, labelled as
	// the agent's own words.
	if t.report == "" {
		last := t.ss[len(t.ss)-1]
		lrows, _ := ix.Query(fmt.Sprintf("SELECT substr(text,1,600) AS text FROM msgs WHERE host=%s AND kind=%s AND session_id=%s AND role='assistant' AND sub='' AND seq>=0 ORDER BY seq DESC LIMIT 1;", q(g.host), q(last.kind), q(last.id)))
		if len(lrows) > 0 {
			t.outs = append(t.outs, outcome{"last-message", last.id, clip(strings.Join(strings.Fields(sget(lrows[0], "text")), " "), 320), "the agent's last reply (its own claim, not verified)", last.ended})
		}
	}
	// Text for keywords: titles, briefs, user and assistant prose.
	for _, s := range t.ss {
		t.text.WriteString(s.title + " " + s.first + " " + s.brief + " " + sums[s.key] + "\n")
	}
	trows, _ := ix.Query(fmt.Sprintf("SELECT substr(text,1,1200) AS text FROM msgs WHERE host=%s AND session_id IN (%s) AND role IN ('user','assistant') AND sub='' AND seq>=0 ORDER BY seq LIMIT 400;", q(g.host), in))
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
	host    string
	enabled bool
	cache   map[string]*repoInfo
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

// repoDir finds a checkout on this host to ask git about: the ledger's
// primary checkout, the session's cwd if it still exists, or ~/code/<repo>.
func (g *gitCtx) repoDir(cwd, repo string, l *Link) string {
	cands := []string{}
	if l != nil && l.Repo != "" {
		cands = append(cands, l.Repo)
	}
	if cwd != "" {
		cands = append(cands, cwd)
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
	for _, ref := range []string{"origin/main", "main", "origin/master", "master"} {
		if _, code, _ := run(dir, 10*time.Second, "git", "rev-parse", "--verify", "-q", ref+"^{commit}"); code == 0 {
			ri.base, ri.ok = ref, true
			break
		}
	}
	return ri
}

// landed says whether a commit is on the default branch of a local checkout:
// "landed" (an ancestor, or a commit on main with the same subject: the
// land step rebases, which changes SHAs), "notinmain", or "unchecked".
func (g *gitCtx) landed(cwd, repo string, l *Link, sha, subject string) (state, how string) {
	if !g.enabled {
		return "unchecked", "git not consulted"
	}
	dir := g.repoDir(cwd, repo, l)
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
	rows, err := ix.Query("SELECT DISTINCT repo, CASE WHEN ledger_branch<>'' THEN ledger_branch ELSE branch END AS br, cwd, ledger_id FROM sessions WHERE host=" + q(g.host) + ";")
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
		dir := g.repoDir(sget(r, "cwd"), sget(r, "repo"), nil)
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

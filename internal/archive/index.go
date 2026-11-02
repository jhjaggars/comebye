package archive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The index is a SQLite database (FTS5) built from the archive. It can
// always be deleted and rebuilt: Sync recreates every row from the stored
// segments. It holds plaintext (that is what makes it searchable), so it
// is created mode 0600 and belongs on the host that holds the identity.
//
// The repo takes no module dependencies, so SQLite is driven through the
// `sqlite3` command-line tool (3.9+ with FTS5), not a Go driver. A Go
// driver (modernc.org/sqlite) is the alternative if dependencies are OK.

// Index is a SQLite file.
type Index struct {
	Path   string
	Sqlite string // the sqlite3 binary; "" means "sqlite3"
}

func (ix *Index) bin() string {
	if ix.Sqlite != "" {
		return ix.Sqlite
	}
	return "sqlite3"
}

const schema = `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS sessions(
  host TEXT, kind TEXT, id TEXT, slug TEXT, cwd TEXT, repo TEXT, branch TEXT,
  title TEXT, first_prompt TEXT, version TEXT, started TEXT, ended TEXT,
  n_msgs INT, n_sub INT, n_bytes INT, n_segs INT, n_streams INT,
  ledger_id TEXT, ledger_agent TEXT, ledger_branch TEXT, plan_id TEXT, ref TEXT, brief TEXT,
  summary TEXT, summary_at TEXT,
  PRIMARY KEY(host,kind,id));
CREATE INDEX IF NOT EXISTS sessions_started ON sessions(started);
CREATE TABLE IF NOT EXISTS artifacts(host TEXT, kind TEXT, session_id TEXT, path TEXT);
CREATE TABLE IF NOT EXISTS commits(
  host TEXT, kind TEXT, session_id TEXT, sha TEXT, rel TEXT, branch TEXT, subject TEXT);
CREATE INDEX IF NOT EXISTS commits_sha ON commits(sha);
CREATE INDEX IF NOT EXISTS commits_session ON commits(host,kind,session_id);
CREATE VIRTUAL TABLE IF NOT EXISTS msgs USING fts5(
  text, host UNINDEXED, kind UNINDEXED, session_id UNINDEXED, seq UNINDEXED,
  role UNINDEXED, sub UNINDEXED, ts UNINDEXED, tokenize='porter unicode61');
`

func q(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func qn(n int64) string { return strconv.FormatInt(n, 10) }

// exec runs SQL (stdin) and returns the rows as JSON when json is set.
func (ix *Index) run(sql string, asJSON bool) ([]byte, error) {
	args := []string{"-bail", "-cmd", ".timeout 10000"}
	if asJSON {
		args = append(args, "-json")
	}
	args = append(args, ix.Path)
	cmd := exec.Command(ix.bin(), args...)
	cmd.Stdin = strings.NewReader(sql)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sqlite3: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Query runs a SELECT and returns rows keyed by column.
func (ix *Index) Query(sql string) ([]map[string]any, error) {
	out, err := ix.run(sql, true)
	if err != nil {
		return nil, err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// open creates the file (mode 0600) and the schema.
func (ix *Index) open() error {
	if err := os.MkdirAll(filepath.Dir(ix.Path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(ix.Path); os.IsNotExist(err) {
		f, err := os.OpenFile(ix.Path, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		f.Close()
	}
	_, err := ix.run(schema, false)
	return err
}

// stmtWriter streams statements to one sqlite3 process.
type stmtWriter struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	errb *bytes.Buffer
}

func (ix *Index) writer() (*stmtWriter, error) {
	cmd := exec.Command(ix.bin(), "-bail", "-cmd", ".timeout 10000", ix.Path)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	w := &stmtWriter{cmd: cmd, in: in, errb: &bytes.Buffer{}}
	cmd.Stderr = w.errb
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *stmtWriter) exec(sql string) { _, _ = io.WriteString(w.in, sql+"\n") }

func (w *stmtWriter) close() error {
	w.in.Close()
	if err := w.cmd.Wait(); err != nil {
		return fmt.Errorf("sqlite3: %v: %s", err, strings.TrimSpace(w.errb.String()))
	}
	return nil
}

// Link ties a session to a Comebye handoff. It is built from
// `comebye ledger --json` (the "entries" array).
type Link struct {
	ID         string   `json:"id"`
	Host       string   `json:"host"`
	AgentID    string   `json:"agent_id"`
	RepoName   string   `json:"repo_name"`
	Branch     string   `json:"branch"`
	PrevBranch []string `json:"previous_branches"`
	Worktree   string   `json:"worktree"`
	PlanID     string   `json:"plan_id"`
	Ref        string   `json:"ref"` // backlog item, once handoffs carry one
	Brief      string   `json:"brief"`
	CreatedAt  string   `json:"created_at"`
	// What the ledger says happened (structured, not the agent's claim).
	Repo         string `json:"repo"` // the primary checkout on Host
	Status       string `json:"status"`
	FinalReport  string `json:"final_report"`
	ReportSource string `json:"report_source"`
	FinishedAt   string `json:"finished_at"`
	RemovedAt    string `json:"removed_at"`
	Mark         *struct {
		Discard bool   `json:"discard"`
		Reason  string `json:"reason"`
	} `json:"mark"`
	Removal *struct {
		Discarded     bool   `json:"discarded"`
		BranchDeleted string `json:"branch_deleted"`
		Checks        struct {
			Unmerged int `json:"unmerged_commits"`
		} `json:"checks"`
	} `json:"removal"`
	Checks *struct {
		Unmerged int `json:"unmerged_commits"`
	} `json:"checks"`
}

// LoadLinks reads the output of `comebye ledger --json`.
func LoadLinks(r io.Reader) ([]Link, error) {
	var d struct {
		Entries []Link `json:"entries"`
	}
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, err
	}
	return d.Entries, nil
}

// bestLink picks the handoff a session ran for: same host, and its cwd is
// the handoff's worktree (or inside it); of several (a worktree reused by
// a follow-up), the one created closest to the session's start.
func bestLink(links []Link, host, cwd, start string) *Link {
	var best *Link
	var bestD time.Duration
	st, _ := time.Parse(time.RFC3339, start)
	for i := range links {
		l := &links[i]
		if l.Host != host || l.Worktree == "" {
			continue
		}
		if cwd != l.Worktree && !strings.HasPrefix(cwd, strings.TrimSuffix(l.Worktree, "/")+"/") {
			continue
		}
		ct, _ := time.Parse(time.RFC3339, l.CreatedAt)
		d := st.Sub(ct)
		if d < 0 {
			d = -d
		}
		if best == nil || d < bestD {
			best, bestD = l, d
		}
	}
	return best
}

type group struct {
	Kind, Slug, SID string
	Main            string   // stream of the main file
	Subs            []string // .jsonl streams of subagents and workflows
	Bytes           int64
	Segs            int
	Streams         int
}

func (a *Archiver) groups(state map[string]*streamState) map[string]*group {
	gs := map[string]*group{}
	for id, st := range state {
		kind, stream, _ := strings.Cut(id, "/")
		slug, sid, ok := SessionOf(kind, stream)
		if !ok {
			continue
		}
		key := kind + "/" + slug + "/" + sid
		g := gs[key]
		if g == nil {
			g = &group{Kind: kind, Slug: slug, SID: sid}
			gs[key] = g
		}
		g.Bytes += st.Contiguous
		g.Segs += len(st.Segs)
		g.Streams++
		switch {
		case stream == slug+"/"+sid+".jsonl":
			g.Main = stream
		case strings.HasSuffix(stream, ".jsonl"):
			g.Subs = append(g.Subs, stream)
		}
	}
	for _, g := range gs {
		sort.Strings(g.Subs)
	}
	return gs
}

// SyncStats reports Sync.
type SyncStats struct {
	Sessions  int      `json:"sessions"`
	Indexed   int      `json:"indexed"` // parsed this run
	Unchanged int      `json:"unchanged"`
	Removed   int      `json:"removed"`
	Messages  int      `json:"messages"`
	Threads   int      `json:"threads"`
	Errors    []string `json:"errors,omitempty"`
}

// Sync brings the index up to date with the archive: sessions whose stored
// bytes changed are re-read (decrypted, parsed) and their rows replaced;
// sessions deleted on request are dropped; handoff links and summaries are
// re-applied. A fresh index file is a full rebuild.
func (ix *Index) Sync(a *Archiver, o Opener, links []Link, opt SyncOptions) (SyncStats, error) {
	var ss SyncStats
	if err := ix.open(); err != nil {
		return ss, err
	}
	state, err := scan(a.B, a.Host)
	if err != nil {
		return ss, err
	}
	gs := a.groups(state)
	rows, err := ix.Query("SELECT kind,id,n_bytes FROM sessions WHERE host=" + q(a.Host) + ";")
	if err != nil {
		return ss, err
	}
	have := map[string]int64{}
	for _, r := range rows {
		n, _ := r["n_bytes"].(float64)
		have[fmt.Sprint(r["kind"])+"/"+fmt.Sprint(r["id"])] = int64(n)
	}
	w, err := ix.writer()
	if err != nil {
		return ss, err
	}
	w.exec("BEGIN;")
	keys := make([]string, 0, len(gs))
	for k := range gs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	live := map[string]bool{}
	for _, k := range keys {
		g := gs[k]
		if a.Tombstoned(g.Kind, g.Slug, g.SID) {
			continue
		}
		live[g.Kind+"/"+g.SID] = true
		ss.Sessions++
		if n, ok := have[g.Kind+"/"+g.SID]; ok && n == g.Bytes {
			ss.Unchanged++
			continue
		}
		if g.Main == "" {
			continue // only subagent files so far
		}
		p, err := a.parseGroup(o, g)
		if err != nil {
			ss.Errors = append(ss.Errors, g.Kind+"/"+g.SID+": "+err.Error())
			continue
		}
		where := "host=" + q(a.Host) + " AND kind=" + q(g.Kind)
		w.exec("DELETE FROM sessions WHERE " + where + " AND id=" + q(g.SID) + ";")
		w.exec("DELETE FROM commits WHERE " + where + " AND session_id=" + q(g.SID) + ";")
		w.exec("DELETE FROM artifacts WHERE " + where + " AND session_id=" + q(g.SID) + ";")
		w.exec("DELETE FROM msgs WHERE host=" + q(a.Host) + " AND kind=" + q(g.Kind) + " AND session_id=" + q(g.SID) + ";")
		nsub := 0
		for _, m := range p.Msgs {
			if m.Sub != "" {
				nsub++
			}
		}
		repo := RepoFromCwd(p.Cwd)
		w.exec(fmt.Sprintf("INSERT INTO sessions(host,kind,id,slug,cwd,repo,branch,title,first_prompt,version,started,ended,n_msgs,n_sub,n_bytes,n_segs,n_streams) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%d,%d,%d,%d,%d);",
			q(a.Host), q(g.Kind), q(g.SID), q(g.Slug), q(p.Cwd), q(repo), q(p.Branch), q(p.Title), q(p.FirstPrompt), q(p.Version),
			q(p.Start), q(p.End), len(p.Msgs)-nsub, nsub, g.Bytes, g.Segs, g.Streams))
		for _, c := range p.Commits {
			w.exec(fmt.Sprintf("INSERT INTO commits VALUES(%s,%s,%s,%s,%s,%s,%s);", q(a.Host), q(g.Kind), q(g.SID), q(c.SHA), q(c.Rel), q(c.Branch), q(c.Subject)))
		}
		for _, d := range p.Docs {
			w.exec(fmt.Sprintf("INSERT INTO artifacts VALUES(%s,%s,%s,%s);", q(a.Host), q(g.Kind), q(g.SID), q(d)))
		}
		for _, m := range p.Msgs {
			w.exec(fmt.Sprintf("INSERT INTO msgs(text,host,kind,session_id,seq,role,sub,ts) VALUES(%s,%s,%s,%s,%d,%s,%s,%s);",
				q(m.Text), q(a.Host), q(g.Kind), q(g.SID), m.Seq, q(m.Role), q(m.Sub), q(m.TS)))
		}
		ss.Indexed++
		ss.Messages += len(p.Msgs)
	}
	for _, r := range rows { // gone from the archive: deleted on request
		id := fmt.Sprint(r["kind"]) + "/" + fmt.Sprint(r["id"])
		if !live[id] {
			kind, sid := fmt.Sprint(r["kind"]), fmt.Sprint(r["id"])
			w.exec("DELETE FROM sessions WHERE host=" + q(a.Host) + " AND kind=" + q(kind) + " AND id=" + q(sid) + ";")
			w.exec("DELETE FROM commits WHERE host=" + q(a.Host) + " AND kind=" + q(kind) + " AND session_id=" + q(sid) + ";")
			w.exec("DELETE FROM artifacts WHERE host=" + q(a.Host) + " AND kind=" + q(kind) + " AND session_id=" + q(sid) + ";")
			w.exec("DELETE FROM msgs WHERE host=" + q(a.Host) + " AND kind=" + q(kind) + " AND session_id=" + q(sid) + ";")
			ss.Removed++
		}
	}
	w.exec("COMMIT;")
	if err := w.close(); err != nil {
		return ss, err
	}
	if err := ix.applyLinks(a.Host, links); err != nil {
		return ss, err
	}
	if err := ix.applySummaries(a, o); err != nil {
		return ss, err
	}
	ts, err := ix.buildThreads(a.Host, links, opt)
	ss.Threads, ss.Errors = ts.Threads, append(ss.Errors, ts.Errors...)
	return ss, err
}

// parseGroup restores a session's files from the archive and parses them.
func (a *Archiver) parseGroup(o Opener, g *group) (*Parsed, error) {
	var main bytes.Buffer
	if _, err := StreamTo(&main, a.B, o, a.Host, g.Kind, g.Main); err != nil {
		return nil, err
	}
	p := &Parsed{}
	switch g.Kind {
	case "claude":
		if err := p.ParseClaude(&main, ""); err != nil {
			return nil, err
		}
		for _, s := range g.Subs {
			var b bytes.Buffer
			if _, err := StreamTo(&b, a.B, o, a.Host, g.Kind, s); err != nil {
				continue // a subagent stream with a gap must not hide the session
			}
			id := strings.TrimSuffix(filepath.Base(s), ".jsonl")
			if err := p.ParseClaude(&b, strings.TrimPrefix(id, "agent-")); err != nil {
				continue
			}
		}
	case "pi":
		if err := p.ParsePi(&main); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("no parser for kind %q", g.Kind)
	}
	if p.Title == "" {
		p.Title = clip(p.FirstPrompt, 80)
	}
	return p, nil
}

func (ix *Index) applyLinks(host string, links []Link) error {
	rows, err := ix.Query("SELECT kind,id,cwd,started FROM sessions WHERE host=" + q(host) + ";")
	if err != nil || len(links) == 0 {
		return err
	}
	w, err := ix.writer()
	if err != nil {
		return err
	}
	w.exec("BEGIN;")
	for _, r := range rows {
		kind, id, cwd, start := fmt.Sprint(r["kind"]), fmt.Sprint(r["id"]), fmt.Sprint(r["cwd"]), fmt.Sprint(r["started"])
		where := " WHERE host=" + q(host) + " AND kind=" + q(kind) + " AND id=" + q(id) + ";"
		l := bestLink(links, host, cwd, start)
		if l == nil {
			continue
		}
		w.exec(fmt.Sprintf("UPDATE sessions SET ledger_id=%s, ledger_agent=%s, ledger_branch=%s, plan_id=%s, ref=%s, brief=%s, repo=CASE WHEN %s<>'' THEN %s ELSE repo END%s",
			q(l.ID), q(l.AgentID), q(l.Branch), q(l.PlanID), q(l.Ref), q(clip(l.Brief, 600)), q(l.RepoName), q(l.RepoName), where))
		w.exec("DELETE FROM msgs WHERE host=" + q(host) + " AND kind=" + q(kind) + " AND session_id=" + q(id) + " AND role='handoff';")
		w.exec(fmt.Sprintf("INSERT INTO msgs(text,host,kind,session_id,seq,role,sub,ts) VALUES(%s,%s,%s,%s,-1,'handoff','',%s);",
			q(clip(l.Brief, 4000)), q(host), q(kind), q(id), q(l.CreatedAt)))
	}
	w.exec("COMMIT;")
	return w.close()
}

func noteHeader(key string) string { return "cba2-note|" + key + "\n" }

// Summaries are stored in the archive as small sealed objects, so they
// survive an index rebuild: notes/<host>/<kind>/<sid>/<time>.json, the
// latest wins.

// SetSummary stores a short summary of a session (written by an agent or a
// person; the archive does not call a model).
func (a *Archiver) SetSummary(kind, sid, text, by string) error {
	now := a.now().UTC()
	body, _ := json.Marshal(map[string]string{"summary": text, "by": by, "at": now.Format(time.RFC3339)})
	key := fmt.Sprintf("notes/%s/%s/%s/%s.json", a.Host, kind, sid, now.Format("20060102T150405.000000000Z"))
	sealed, err := a.S.Seal(append([]byte(noteHeader(key)), body...))
	if err != nil {
		return err
	}
	return a.B.Put(key, sealed)
}

func (ix *Index) applySummaries(a *Archiver, o Opener) error {
	keys, err := a.B.List("notes/" + a.Host + "/")
	if err != nil || len(keys) == 0 {
		return err
	}
	latest := map[string]string{} // kind/sid -> key (keys sort by time)
	for _, k := range keys {
		p := strings.Split(k, "/")
		if len(p) == 5 {
			latest[p[2]+"/"+p[3]] = k
		}
	}
	w, err := ix.writer()
	if err != nil {
		return err
	}
	w.exec("BEGIN;")
	for ks, k := range latest {
		blob, err := a.B.Get(k)
		if err != nil {
			continue
		}
		plain, err := o.Open(blob)
		if err != nil || !bytes.HasPrefix(plain, []byte(noteHeader(k))) {
			continue
		}
		plain = plain[len(noteHeader(k)):]
		var n struct{ Summary, At string }
		var m map[string]string
		if json.Unmarshal(plain, &m) != nil {
			continue
		}
		n.Summary, n.At = m["summary"], m["at"]
		kind, sid, _ := strings.Cut(ks, "/")
		w.exec(fmt.Sprintf("UPDATE sessions SET summary=%s, summary_at=%s WHERE host=%s AND kind=%s AND id=%s;", q(n.Summary), q(n.At), q(a.Host), q(kind), q(sid)))
	}
	w.exec("COMMIT;")
	return w.close()
}

// Filter narrows a listing or a search.
type Filter struct {
	Since, Until string // dates or times, compared with the session's span
	Host, Kind   string
	Project      string // matches repo, cwd, branch, or the handoff's branch
	Branch       string
	Ledger       string // handoff id
	Limit        int
}

func (f Filter) where() string {
	c := []string{"1=1"}
	if f.Since != "" {
		c = append(c, "s.ended >= "+q(f.Since))
	}
	if f.Until != "" {
		c = append(c, "s.started < "+q(f.Until))
	}
	if f.Host != "" {
		c = append(c, "s.host = "+q(f.Host))
	}
	if f.Kind != "" {
		c = append(c, "s.kind = "+q(f.Kind))
	}
	if f.Project != "" {
		p := q("%" + f.Project + "%")
		c = append(c, fmt.Sprintf("(s.repo LIKE %s OR s.cwd LIKE %s OR s.branch LIKE %s OR s.ledger_branch LIKE %s OR s.title LIKE %s)", p, p, p, p, p))
	}
	if f.Branch != "" {
		c = append(c, fmt.Sprintf("(s.branch = %s OR s.ledger_branch = %s)", q(f.Branch), q(f.Branch)))
	}
	if f.Ledger != "" {
		c = append(c, "s.ledger_id = "+q(f.Ledger))
	}
	return strings.Join(c, " AND ")
}

func (f Filter) limit(def int) int {
	if f.Limit > 0 {
		return f.Limit
	}
	return def
}

const sessionCols = "s.host, s.kind, s.id, s.title, s.repo, s.branch, s.started, s.ended, s.n_msgs, s.n_sub, s.ledger_id, s.plan_id, s.ref, s.summary"

// List returns sessions, newest first.
func (ix *Index) List(f Filter) ([]map[string]any, error) {
	return ix.Query(fmt.Sprintf("SELECT %s FROM sessions s WHERE %s ORDER BY s.started DESC LIMIT %d;", sessionCols, f.where(), f.limit(30)))
}

// FTSQuery makes a safe FTS5 query from free text: every word must match,
// unless the text already uses quotes, AND/OR/NOT/NEAR, a prefix * or a
// column filter, in which case it is passed through.
func FTSQuery(text string) string {
	if strings.ContainsAny(text, "\"*:()") || strings.Contains(text, " OR ") || strings.Contains(text, " AND ") || strings.Contains(text, " NOT ") || strings.Contains(text, "NEAR") {
		return text
	}
	var terms []string
	for _, w := range strings.Fields(text) {
		terms = append(terms, `"`+strings.ReplaceAll(w, `"`, ``)+`"`)
	}
	return strings.Join(terms, " ")
}

// Search finds sessions whose text matches, best match first, one row per
// session, with a snippet of the best matching message.
func (ix *Index) Search(text string, f Filter) ([]map[string]any, error) {
	lim := f.limit(15)
	sql := fmt.Sprintf(`SELECT %s, m.role AS role, m.sub AS sub, m.seq AS seq,
  snippet(msgs, 0, '[', ']', '…', 14) AS snippet, m.rank AS rank
FROM msgs m JOIN sessions s ON s.host=m.host AND s.kind=m.kind AND s.id=m.session_id
WHERE msgs MATCH %s AND %s ORDER BY m.rank LIMIT %d;`, sessionCols, q(FTSQuery(text)), f.where(), lim*8)
	rows, err := ix.Query(sql)
	if err != nil { // FTS5 syntax error in the user's text: search it as plain words
		plain := strings.NewReplacer(`"`, ` `, `'`, ` `, `(`, ` `, `)`, ` `, `*`, ` `, `:`, ` `).Replace(text)
		sql = strings.Replace(sql, q(FTSQuery(text)), q(FTSQuery(strings.Join(strings.Fields(plain), " "))), 1)
		if rows, err = ix.Query(sql); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	var out []map[string]any
	for _, r := range rows {
		k := fmt.Sprint(r["host"], "/", r["kind"], "/", r["id"])
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
		if len(out) >= lim {
			break
		}
	}
	return out, nil
}

// Show returns one session with its commits. id may be a unique prefix.
func (ix *Index) Show(id string) (map[string]any, []map[string]any, error) {
	rows, err := ix.Query(fmt.Sprintf("SELECT * FROM sessions WHERE id LIKE %s OR id LIKE %s LIMIT 3;", q(id+"%"), q("%_"+id+"%")))
	if err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("no session %q", id)
	}
	if len(rows) > 1 {
		return nil, nil, fmt.Errorf("%q matches several sessions; give more of the id", id)
	}
	s := rows[0]
	cs, err := ix.Query(fmt.Sprintf("SELECT sha,rel,branch,subject FROM commits WHERE host=%s AND kind=%s AND session_id=%s ORDER BY rel DESC;", q(fmt.Sprint(s["host"])), q(fmt.Sprint(s["kind"])), q(fmt.Sprint(s["id"]))))
	return s, cs, err
}

// Text returns a session's indexed messages in order.
func (ix *Index) Text(host, kind, id string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 200
	}
	return ix.Query(fmt.Sprintf("SELECT seq,role,sub,ts,text FROM msgs WHERE host=%s AND kind=%s AND session_id=%s AND seq>=0 ORDER BY seq LIMIT %d;", q(host), q(kind), q(id), limit))
}

// ByCommit finds the sessions that made, pushed or saw a commit (sha or a
// prefix of at least 7 characters).
func (ix *Index) ByCommit(sha string) ([]map[string]any, error) {
	if len(sha) < 7 {
		return nil, fmt.Errorf("give at least 7 characters of the commit")
	}
	return ix.Query(fmt.Sprintf(`SELECT %s, c.sha AS sha, c.rel AS rel, c.subject AS subject FROM commits c
JOIN sessions s ON s.host=c.host AND s.kind=c.kind AND s.id=c.session_id
WHERE c.sha LIKE %s ORDER BY CASE c.rel WHEN 'made' THEN 0 WHEN 'pushed' THEN 1 ELSE 2 END, s.started;`, sessionCols, q(sha+"%")))
}

// DeleteSession drops a session's rows (the archive's copy is removed with
// Archiver.DeleteSession).
func (ix *Index) DeleteSession(host, kind, id string) error {
	w := " WHERE host=" + q(host) + " AND kind=" + q(kind)
	_, err := ix.run("BEGIN; DELETE FROM sessions"+w+" AND id="+q(id)+"; DELETE FROM commits"+w+" AND session_id="+q(id)+"; DELETE FROM artifacts"+w+" AND session_id="+q(id)+"; DELETE FROM msgs"+w+" AND session_id="+q(id)+"; COMMIT;", false)
	return err
}

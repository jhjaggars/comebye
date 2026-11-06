package archive

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
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

// schemaVersion is PRAGMA user_version. An index with another version is rebuilt
// from the archive, into a new file that replaces it when it is complete.
const schemaVersion = 2

const schema = `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS sessions(
  host TEXT, kind TEXT, id TEXT, slug TEXT, cwd TEXT, repo TEXT, branch TEXT,
  title TEXT, first_prompt TEXT, version TEXT, started TEXT, ended TEXT,
  n_msgs INT, n_sub INT, n_bytes INT, n_segs INT, n_streams INT, n_secret INT DEFAULT 0,
  ledger_id TEXT, ledger_agent TEXT, ledger_branch TEXT, plan_id TEXT, ref TEXT, brief TEXT, link_sig TEXT,
  summary TEXT, summary_at TEXT,
  PRIMARY KEY(host,kind,id));
CREATE INDEX IF NOT EXISTS sessions_started ON sessions(started);
CREATE TABLE IF NOT EXISTS artifacts(host TEXT, kind TEXT, session_id TEXT, path TEXT);
CREATE TABLE IF NOT EXISTS commits(
  host TEXT, kind TEXT, session_id TEXT, sha TEXT, rel TEXT, branch TEXT, subject TEXT);
CREATE INDEX IF NOT EXISTS commits_sha ON commits(sha);
CREATE INDEX IF NOT EXISTS commits_session ON commits(host,kind,session_id);
CREATE TABLE IF NOT EXISTS messages(
  id INTEGER PRIMARY KEY, host TEXT, kind TEXT, session_id TEXT, seq INT, role TEXT, sub TEXT, ts TEXT,
  stream TEXT, off INT, tool_id TEXT, is_err INT, secret INT, full_len INT,
  text TEXT); -- last: long text spills to overflow pages, which filtering on the other columns then skips
CREATE INDEX IF NOT EXISTS messages_session ON messages(host,kind,session_id,seq);
CREATE VIRTUAL TABLE IF NOT EXISTS msgs USING fts5(text, content='messages', content_rowid='id', tokenize='porter unicode61');
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO msgs(rowid,text) VALUES(new.id,new.text);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO msgs(msgs,rowid,text) VALUES('delete',old.id,old.text);
END;
CREATE TABLE IF NOT EXISTS gh_prs(repo TEXT, branch TEXT, fetched TEXT, json TEXT, PRIMARY KEY(repo,branch));
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT);
PRAGMA user_version=2;
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
	if err := ix.create(); err != nil {
		return err
	}
	_, err := ix.run(schema, false)
	return err
}

func (ix *Index) create() error {
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
	return nil
}

// version reads the schema version of an existing file; a file that does not
// exist or is empty has the current one, because open will create it as such.
func (ix *Index) version() (int, error) {
	fi, err := os.Stat(ix.Path)
	if os.IsNotExist(err) || (err == nil && fi.Size() == 0) {
		return schemaVersion, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := ix.Query("PRAGMA user_version;")
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	v, _ := rows[0]["user_version"].(float64)
	return int(v), nil
}

// Outdated reports an existing index in another format, which queries cannot read.
func (ix *Index) Outdated() bool {
	v, err := ix.version()
	return err == nil && v != schemaVersion
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
	ID      string `json:"id"`
	Host    string `json:"host"`
	AgentID string `json:"agent_id"`
	// HostInstance is the incarnation of Host the handoff ran on: a spot
	// worker's cloud instance id. Empty for older entries.
	HostInstance string   `json:"host_instance"`
	RepoName     string   `json:"repo_name"`
	Branch       string   `json:"branch"`
	PrevBranch   []string `json:"previous_branches"`
	Worktree     string   `json:"worktree"`
	PlanID       string   `json:"plan_id"`
	Ref          string   `json:"ref"` // backlog item, once handoffs carry one
	Brief        string   `json:"brief"`
	CreatedAt    string   `json:"created_at"`
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

	briefMasked bool // see maskLinks
}

// sig changes when anything the index stores from the link changes.
func (l *Link) sig() string {
	h := sha1.Sum([]byte(strings.Join([]string{l.ID, l.AgentID, l.Branch, l.PlanID, l.Ref, l.RepoName, l.CreatedAt, l.Brief}, "\x00") + instSig(l.HostInstance)))
	return hex.EncodeToString(h[:])
}

// instSig is the instance's share of a link's signature: nothing when there is
// none, so rows linked before instances are not rewritten.
func instSig(in string) string {
	if in == "" {
		return ""
	}
	return "\x00" + in
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
func bestLink(links []Link, scope linkScope, cwd, start string) *Link {
	var best *Link
	var bestD time.Duration
	st, _ := time.Parse(time.RFC3339, start)
	for i := range links {
		l := &links[i]
		if l.Host != scope.host || l.Worktree == "" {
			continue
		}
		// A worker's sessions belong to one VM: a handoff that ran on another
		// instance of the same name is not theirs, whatever the dates say.
		if scope.instance != "" && l.HostInstance != "" && l.HostInstance != scope.instance {
			continue
		}
		if cwd != l.Worktree && !strings.HasPrefix(cwd, strings.TrimSuffix(l.Worktree, "/")+"/") {
			continue
		}
		ct, _ := time.Parse(time.RFC3339, l.CreatedAt)
		if !scope.admits(ct, st) {
			continue
		}
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
	Sessions  int `json:"sessions"`
	Indexed   int `json:"indexed"` // parsed this run
	Unchanged int `json:"unchanged"`
	Removed   int `json:"removed"`
	Messages  int `json:"messages"`
	Threads   int `json:"threads"`
	// Hosts is what an AllHosts run or a run with workers went through beyond
	// a.Host; FailedHosts the ones that could not be read this time.
	Hosts       []string `json:"hosts,omitempty"`
	FailedHosts []string `json:"failed_hosts,omitempty"`
	Errors      []string `json:"errors,omitempty"`
}

// Sync brings the index up to date with the archive: sessions whose stored
// bytes changed are re-read (decrypted, parsed) and their rows replaced;
// sessions deleted on request are dropped; handoff links and summaries are
// re-applied. A fresh index file is a full rebuild.
//
// It covers a.Host and the workers this machine uploaded, so that recall finds
// a worker's sessions, or with opt.AllHosts every host in the store. Threads are
// worked out over every session the index holds.
func (ix *Index) Sync(a *Archiver, o Opener, links []Link, opt SyncOptions) (SyncStats, error) {
	links = maskLinks(links)
	v, err := ix.version()
	if err != nil {
		return SyncStats{}, err
	}
	if v != schemaVersion {
		return ix.rebuild(a, o, links, opt)
	}
	return ix.sync(a, o, links, opt)
}

// rebuild builds the whole index in a new file and moves it into place when it
// is done, so that readers see the old file until then.
func (ix *Index) rebuild(a *Archiver, o Opener, links []Link, opt SyncOptions) (SyncStats, error) {
	tmp := &Index{Path: ix.Path + ".rebuild", Sqlite: ix.Sqlite}
	clean := func(p string) {
		for _, f := range []string{p, p + "-wal", p + "-shm"} {
			os.Remove(f)
		}
	}
	clean(tmp.Path)
	if err := tmp.open(); err != nil {
		return SyncStats{}, err
	}
	// The PR cache is the one thing the archive cannot give back.
	// An index too old to have it fails here, and that is fine.
	_, _ = tmp.run("ATTACH "+q(ix.Path)+" AS old; INSERT OR IGNORE INTO gh_prs SELECT * FROM old.gh_prs; DETACH old;", false)
	st, err := tmp.sync(a, o, links, opt)
	if err != nil {
		clean(tmp.Path)
		return st, err
	}
	// Folds the old file's WAL into it, so that it cannot be applied to the new.
	if err := ix.foldWAL(); err != nil {
		clean(tmp.Path)
		return st, err
	}
	if err := os.Rename(tmp.Path, ix.Path); err != nil {
		clean(tmp.Path)
		return st, err
	}
	return st, nil
}

func (ix *Index) sync(a *Archiver, o Opener, links []Link, opt SyncOptions) (SyncStats, error) {
	var ss SyncStats
	if err := ix.open(); err != nil {
		return ss, err
	}
	claims, err := a.hostClaims()
	if err != nil {
		return ss, err
	}
	var others []string
	if opt.AllHosts {
		if others, err = ix.storeHosts(a); err != nil {
			return ss, err
		}
	} else {
		ss, err = ix.syncHost(a, o, links, opt, scopeOf(a.Host, claims))
		if err != nil {
			return ss, err
		}
		mine := machineID()
		for h, cl := range claims {
			if h != a.Host && cl.Kind == hostKindWorker && cl.Machine == mine {
				others = append(others, h)
			}
		}
		sort.Strings(others)
	}
	for _, h := range others {
		// One host's trouble must not stop the others. Its rows from earlier
		// runs stay, so it keeps its threads.
		st, err := ix.syncHost(a.forHost(h), o, links, opt, scopeOf(h, claims))
		if err != nil {
			ss.Errors = append(ss.Errors, h+": "+err.Error())
			ss.FailedHosts = append(ss.FailedHosts, h)
		}
		ss.Hosts = append(ss.Hosts, h)
		ss.Sessions += st.Sessions
		ss.Indexed += st.Indexed
		ss.Unchanged += st.Unchanged
		ss.Removed += st.Removed
		ss.Messages += st.Messages
		ss.Errors = append(ss.Errors, st.Errors...)
	}
	ts, err := ix.buildThreads(a.Host, links, opt)
	ss.Threads, ss.Errors = ts.Threads, append(ss.Errors, ts.Errors...)
	if err != nil {
		return ss, err
	}
	// Merges the full-text index's segments. Without it the index grows with
	// every session rewritten, and queries read every segment.
	built := time.Now().UTC().Format(time.RFC3339)
	if _, err := ix.run("INSERT INTO msgs(msgs) VALUES('optimize'); INSERT OR REPLACE INTO meta VALUES('built_at',"+q(built)+"); PRAGMA wal_checkpoint(TRUNCATE);", false); err != nil {
		return ss, err
	}
	return ss, nil
}

// storeHosts lists the hosts to index with AllHosts: every host with a segment
// in the store, this host, and any host the index still holds, so that the rows
// of a host whose last session was deleted go too.
func (ix *Index) storeHosts(a *Archiver) ([]string, error) {
	hosts, err := listHosts(a.B)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{a.Host: true}
	for _, h := range hosts {
		seen[h] = true
	}
	rows, err := ix.Query("SELECT DISTINCT host FROM sessions;")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		seen[sget(r, "host")] = true
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out, nil
}

// syncHost brings the rows of one host up to date.
func (ix *Index) syncHost(a *Archiver, o Opener, links []Link, opt SyncOptions, scope linkScope) (SyncStats, error) {
	var ss SyncStats
	state, err := scan(a.B, a.Host)
	if err != nil {
		return ss, err
	}
	gs := a.groups(state)
	tombs, err := a.tombstones()
	if err != nil {
		return ss, err
	}
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
		if tombs[tombKey(a.Host, g.Kind, g.Slug, g.SID)] {
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
		w.exec("DELETE FROM messages WHERE host=" + q(a.Host) + " AND kind=" + q(g.Kind) + " AND session_id=" + q(g.SID) + ";")
		nsub := 0
		for _, m := range p.Msgs {
			if m.Sub != "" {
				nsub++
			}
		}
		repo := RepoFromCwd(p.Cwd)
		w.exec(fmt.Sprintf("INSERT INTO sessions(host,kind,id,slug,cwd,repo,branch,title,first_prompt,version,started,ended,n_msgs,n_sub,n_bytes,n_segs,n_streams,n_secret) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%d,%d,%d,%d,%d,%d);",
			q(a.Host), q(g.Kind), q(g.SID), q(g.Slug), q(p.Cwd), q(repo), q(p.Branch), q(p.Title), q(p.FirstPrompt), q(p.Version),
			q(p.Start), q(p.End), len(p.Msgs)-nsub, nsub, g.Bytes, g.Segs, g.Streams, p.Secrets))
		for _, c := range p.Commits {
			w.exec(fmt.Sprintf("INSERT INTO commits VALUES(%s,%s,%s,%s,%s,%s,%s);", q(a.Host), q(g.Kind), q(g.SID), q(c.SHA), q(c.Rel), q(c.Branch), q(c.Subject)))
		}
		for _, d := range p.Docs {
			w.exec(fmt.Sprintf("INSERT INTO artifacts VALUES(%s,%s,%s,%s);", q(a.Host), q(g.Kind), q(g.SID), q(d)))
		}
		for _, m := range p.Msgs {
			if m.Sub == "" {
				m.Stream = "" // the main file: it follows from the session's slug and id
			}
			w.exec(fmt.Sprintf("INSERT INTO messages(text,host,kind,session_id,seq,role,sub,ts,stream,off,tool_id,is_err,secret,full_len) VALUES(%s,%s,%s,%s,%d,%s,%s,%s,%s,%d,%s,%d,%d,%d);",
				q(m.Text), q(a.Host), q(g.Kind), q(g.SID), m.Seq, q(m.Role), q(m.Sub), q(m.TS), q(m.Stream), m.Off, q(m.ToolID), b2i(m.IsErr), b2i(m.Secret), m.FullLen))
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
			w.exec("DELETE FROM messages WHERE host=" + q(a.Host) + " AND kind=" + q(kind) + " AND session_id=" + q(sid) + ";")
			ss.Removed++
		}
	}
	w.exec("COMMIT;")
	if err := w.close(); err != nil {
		return ss, err
	}
	if err := ix.applyLinks(a.Host, scope, links); err != nil {
		return ss, err
	}
	return ss, ix.applySummaries(a, o)
}

// parseGroup restores a session's files from the archive and parses them.
func (a *Archiver) parseGroup(o Opener, g *group) (*Parsed, error) {
	var main bytes.Buffer
	if _, err := StreamTo(&main, a.B, o, a.Host, g.Kind, g.Main); err != nil {
		return nil, err
	}
	p := &Parsed{stream: g.Main}
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
			p.stream = s
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

func (ix *Index) applyLinks(host string, scope linkScope, links []Link) error {
	rows, err := ix.Query("SELECT kind,id,cwd,started,link_sig FROM sessions WHERE host=" + q(host) + ";")
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
		l := bestLink(links, scope, cwd, start)
		if l == nil {
			continue
		}
		// Only what changed is rewritten: rewriting the brief's row every hour
		// grew the full-text index and cost a scan per linked session.
		sig := l.sig()
		if sget(r, "link_sig") == sig {
			continue
		}
		sess := "host=" + q(host) + " AND kind=" + q(kind) + " AND session_id=" + q(id) + " "
		w.exec("DELETE FROM messages WHERE " + sess + "AND seq=-1 AND role='handoff';")
		w.exec(fmt.Sprintf("INSERT INTO messages(text,host,kind,session_id,seq,role,sub,ts,stream,off,tool_id,is_err,secret,full_len) VALUES(%s,%s,%s,%s,-1,'handoff','',%s,'',0,'',0,%d,0);",
			q(clip(l.Brief, 4000)), q(host), q(kind), q(id), q(l.CreatedAt), b2i(l.briefMasked)))
		w.exec(fmt.Sprintf("UPDATE sessions SET ledger_id=%s, ledger_agent=%s, ledger_branch=%s, plan_id=%s, ref=%s, brief=%s, link_sig=%s, repo=CASE WHEN %s<>'' THEN %s ELSE repo END, n_secret=(SELECT count(*) FROM messages WHERE %sAND secret=1) WHERE host=%s AND kind=%s AND id=%s;",
			q(l.ID), q(l.AgentID), q(l.Branch), q(l.PlanID), q(l.Ref), q(clip(l.Brief, 600)), q(sig), q(l.RepoName), q(l.RepoName), sess, q(host), q(kind), q(id)))
	}
	w.exec("COMMIT;")
	return w.close()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
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
		w.exec(fmt.Sprintf("UPDATE sessions SET summary=%s, summary_at=%s WHERE host=%s AND kind=%s AND id=%s;", q(maskPlain(n.Summary)), q(n.At), q(a.Host), q(kind), q(sid)))
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
	Role         string // said, commands or output (Search only)
	Limit        int
}

// roles maps Filter.Role to the message roles it covers.
func (f Filter) roles() string {
	switch f.Role {
	case "said":
		return "m.role IN ('user','assistant','handoff')"
	case "commands":
		return "m.role = 'tool'"
	case "output":
		return "m.role = 'result'"
	}
	return "1=1"
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
		// A worker's bare name finds every VM that had it (name.instance).
		c = append(c, "(s.host = "+q(f.Host)+" OR substr(s.host,1,"+qn(int64(len(f.Host))+1)+") = "+q(f.Host+".")+")")
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
  snippet(msgs, 0, '[', ']', '…', 14) AS snippet, msgs.rank AS rank
FROM msgs JOIN messages m ON m.id=msgs.rowid JOIN sessions s ON s.host=m.host AND s.kind=m.kind AND s.id=m.session_id
WHERE msgs MATCH %s AND %s AND %s ORDER BY msgs.rank LIMIT %d;`, sessionCols, q(FTSQuery(text)), f.where(), f.roles(), lim*8)
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
	return ix.Query(fmt.Sprintf("SELECT seq,role,sub,ts,text FROM messages WHERE host=%s AND kind=%s AND session_id=%s AND seq>=0 ORDER BY seq LIMIT %d;", q(host), q(kind), q(id), limit))
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
	if ix.Outdated() {
		return nil // the rebuild that replaces it leaves out what was deleted
	}
	w := " WHERE host=" + q(host) + " AND kind=" + q(kind)
	_, err := ix.run("BEGIN; DELETE FROM sessions"+w+" AND id="+q(id)+"; DELETE FROM commits"+w+" AND session_id="+q(id)+"; DELETE FROM artifacts"+w+" AND session_id="+q(id)+"; DELETE FROM messages"+w+" AND session_id="+q(id)+"; COMMIT;", false)
	return err
}

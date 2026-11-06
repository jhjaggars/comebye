package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Reader answers the app's questions from an index file. It never writes: each
// query is its own `sqlite3 -readonly -safe` process, with a time limit and a
// cap on how many run at once. Opening the file per query means a file moved
// into place is picked up with no restart.
type Reader struct {
	Path   string
	Sqlite string // the sqlite3 binary; "" means "sqlite3"
	// Timeout limits one query (default 3s).
	Timeout time.Duration
	slots   chan struct{}
}

// NewReader returns a Reader that runs at most two queries at once.
func NewReader(path string) *Reader {
	return &Reader{Path: path, Timeout: 3 * time.Second, slots: make(chan struct{}, 2)}
}

// ErrNoIndex means there is no usable index file; ErrNoSession that the session
// is not in it (not indexed yet, or deleted).
var (
	ErrNoIndex   = errors.New("no index")
	ErrNoSession = errors.New("no such session")
)

func (r *Reader) query(ctx context.Context, sql string) ([]map[string]any, error) {
	if _, err := os.Stat(r.Path); err != nil {
		return nil, ErrNoIndex
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	bin := r.Sqlite
	if bin == "" {
		bin = "sqlite3"
	}
	cmd := exec.CommandContext(ctx, bin, "-readonly", "-safe", "-bail", "-json", "-cmd", ".timeout 2000", r.Path)
	cmd.Stdin = strings.NewReader(sql)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("sqlite3: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	b := bytes.TrimSpace(out.Bytes())
	if len(b) == 0 {
		return nil, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// IndexStatus describes the file the server reads.
type IndexStatus struct {
	// State: ok, stale (no build for a while), outdated (older format, until
	// the next `archive index` replaces it), missing, or unavailable (it could
	// not be read, sqlite3 missing for example).
	State    string   `json:"state"`
	BuiltAt  string   `json:"built_at,omitempty"`
	Hosts    []string `json:"hosts,omitempty"`
	Sessions int      `json:"sessions"`
	Messages int      `json:"messages"`
	// BuiltAt is when the content last changed. CheckedAt, on an index that a
	// gated timer keeps (`archive index --if-changed`), is when that timer last
	// looked and found it current. State goes by whichever is later.
	CheckedAt string `json:"checked_at,omitempty"`
}

// staleAfter: an index rebuilt by an hourly timer has missed two builds. An index
// with a gated timer records every look, every few minutes, so it can tell sooner.
const (
	staleAfter      = 2*time.Hour + 15*time.Minute
	staleAfterGated = 30 * time.Minute
)

// Status reports the state of the index. Only a failure of the context is an error.
func (r *Reader) Status(ctx context.Context) (IndexStatus, error) {
	if fi, err := os.Stat(r.Path); err != nil || fi.Size() == 0 {
		return IndexStatus{State: "missing"}, nil
	}
	rows, err := r.query(ctx, "PRAGMA user_version;")
	if err != nil {
		if ctx.Err() != nil {
			return IndexStatus{}, ctx.Err()
		}
		return IndexStatus{State: "unavailable"}, nil
	}
	if len(rows) == 0 || num(rows[0], "user_version") != schemaVersion {
		return IndexStatus{State: "outdated"}, nil
	}
	rows, err = r.query(ctx, `SELECT (SELECT value FROM meta WHERE key='built_at') AS built_at, (SELECT value FROM meta WHERE key='checked_at') AS checked_at, count(*) AS sessions, coalesce(sum(n_msgs+n_sub),0) AS messages, group_concat(DISTINCT host) AS hosts FROM sessions;`)
	if err != nil || len(rows) == 0 {
		if ctx.Err() != nil {
			return IndexStatus{}, ctx.Err()
		}
		return IndexStatus{State: "unavailable"}, nil
	}
	st := IndexStatus{State: "ok", BuiltAt: sget(rows[0], "built_at"), CheckedAt: sget(rows[0], "checked_at"), Sessions: num(rows[0], "sessions"), Messages: num(rows[0], "messages")}
	if h := sget(rows[0], "hosts"); h != "" {
		st.Hosts = strings.Split(h, ",")
	}
	limit := staleAfter
	if st.CheckedAt != "" {
		limit = staleAfterGated
	}
	if !seenWithin(limit, st.BuiltAt, st.CheckedAt) {
		st.State = "stale"
	}
	return st, nil
}

// seenWithin reports whether any of these RFC 3339 times is recent enough.
func seenWithin(d time.Duration, times ...string) bool {
	for _, s := range times {
		if t, err := time.Parse(time.RFC3339, s); err == nil && time.Since(t) <= d {
			return true
		}
	}
	return false
}

func num(r map[string]any, k string) int {
	switch v := r[k].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

// Facet is one value of a filter, with how many sessions have it.
type Facet struct {
	Value string `json:"value"`
	N     int    `json:"n"`
}

// Facets lists what the filters can choose from.
type Facets struct {
	Hosts    []Facet `json:"hosts"`
	Kinds    []Facet `json:"kinds"`
	Projects []Facet `json:"projects"`
}

// Facets returns the hosts, agent kinds and projects in the index.
func (r *Reader) Facets(ctx context.Context) (Facets, error) {
	f := Facets{Hosts: []Facet{}, Kinds: []Facet{}, Projects: []Facet{}}
	rows, err := r.query(ctx, `SELECT 'host' AS w, host AS v, count(*) AS n FROM sessions GROUP BY host
UNION ALL SELECT 'kind', kind, count(*) FROM sessions GROUP BY kind
UNION ALL SELECT 'repo', repo, count(*) FROM sessions WHERE repo<>'' GROUP BY repo
ORDER BY 3 DESC, 2;`)
	if err != nil {
		return f, err
	}
	for _, x := range rows {
		v := Facet{sget(x, "v"), num(x, "n")}
		switch sget(x, "w") {
		case "host":
			f.Hosts = append(f.Hosts, v)
		case "kind":
			f.Kinds = append(f.Kinds, v)
		case "repo":
			f.Projects = append(f.Projects, v)
		}
	}
	return f, nil
}

// SessionMeta names a session.
type SessionMeta struct {
	Host     string `json:"host"`
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Title    string `json:"title"`
	Repo     string `json:"repo,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Started  string `json:"started,omitempty"`
	Ended    string `json:"ended,omitempty"`
	Messages int    `json:"messages"`
	Subs     int    `json:"sub_messages,omitempty"`
	Secrets  int    `json:"secrets,omitempty"`
	Ledger   string `json:"ledger_id,omitempty"`
	Thread   string `json:"thread_id,omitempty"`
	Verdict  string `json:"verdict,omitempty"`
}

const metaCols = "s.host AS host, s.kind AS kind, s.id AS id, s.title AS title, s.repo AS repo, s.branch AS branch, s.ledger_branch AS lbranch, s.started AS started, s.ended AS ended, s.n_msgs AS n_msgs, s.n_sub AS n_sub, s.n_secret AS n_secret, s.ledger_id AS ledger_id, x.thread_id AS thread_id, t.verdict AS verdict"

const metaJoins = "LEFT JOIN thread_sessions x ON x.host=s.host AND x.kind=s.kind AND x.session_id=s.id LEFT JOIN threads t ON t.id=x.thread_id"

func metaOf(x map[string]any) SessionMeta {
	b := sget(x, "lbranch")
	if b == "" {
		b = sget(x, "branch")
	}
	return SessionMeta{Host: sget(x, "host"), Kind: sget(x, "kind"), ID: sget(x, "id"), Title: sget(x, "title"), Repo: sget(x, "repo"), Branch: b,
		Started: sget(x, "started"), Ended: sget(x, "ended"), Messages: num(x, "n_msgs"), Subs: num(x, "n_sub"), Secrets: num(x, "n_secret"),
		Ledger: sget(x, "ledger_id"), Thread: sget(x, "thread_id"), Verdict: sget(x, "verdict")}
}

// apiMarks turns the marks a query asks for (U+E000, U+E001: char(57344) and
// char(57345)) into the API's \x02 and \x03. The query cannot ask for control
// bytes directly: older sqlite3 shells write them in -json output as a bare
// "u0002", which comes out as text. Stored text holds neither pair.
var apiMarks = strings.NewReplacer("\ue000", "\x02", "\ue001", "\x03")

// SearchHit is one session that matched, with its best match.
type SearchHit struct {
	SessionMeta
	Matches int    `json:"matches"`
	Seq     int    `json:"seq"`  // the best match, to open the session at
	Sub     string `json:"sub"`  // its subagent, "" for the main thread
	Role    string `json:"role"` // its role
	// Snippet holds \x02 and \x03 around each matched term. Stored text cannot
	// contain them. A client splits on them and never treats this as markup.
	Snippet string `json:"snippet"`
}

// CommitHit is a session tied to a commit whose SHA was searched for.
type CommitHit struct {
	SessionMeta
	SHA     string `json:"sha"`
	Rel     string `json:"rel"`
	Subject string `json:"subject,omitempty"`
}

// SearchResult is a page of search results.
type SearchResult struct {
	Total   int         `json:"total"`
	Hits    []SearchHit `json:"hits"`
	Commits []CommitHit `json:"commits,omitempty"`
}

var (
	shaRE       = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	plainReplac = strings.NewReplacer(`"`, ` `, `'`, ` `, `(`, ` `, `)`, ` `, `*`, ` `, `:`, ` `, `^`, ` `, `-`, ` `, `+`, ` `)
)

// plainFTS is the query as plain words, for when the text is not valid FTS5.
func plainFTS(text string) string {
	return FTSQuery(strings.Join(strings.Fields(plainReplac.Replace(text)), " "))
}

// Search returns one row per session, best first or newest first, skipping skip.
func (r *Reader) Search(ctx context.Context, text string, f Filter, sortBy string, skip int) (SearchResult, error) {
	res := SearchResult{Hits: []SearchHit{}}
	order := "best"
	if sortBy == "new" {
		order = "s.started DESC"
	}
	build := func(match string) string {
		return fmt.Sprintf(`WITH hits AS (
  SELECT m.host AS h, m.kind AS k, m.session_id AS i, count(*) AS matches, min(msgs.rank) AS best, m.id AS mid, m.seq AS seq, m.sub AS sub, m.role AS role
  FROM msgs JOIN messages m ON m.id=msgs.rowid JOIN sessions s ON s.host=m.host AND s.kind=m.kind AND s.id=m.session_id
  WHERE msgs MATCH %s AND %s AND %s GROUP BY m.host, m.kind, m.session_id)
SELECT hits.*, count(*) OVER () AS total, %s
FROM hits JOIN sessions s ON s.host=hits.h AND s.kind=hits.k AND s.id=hits.i %s
ORDER BY %s LIMIT %d OFFSET %d;`, q(match), f.where(), f.roles(), metaCols, metaJoins, order, f.limit(20), skip)
	}
	match := FTSQuery(text)
	rows, err := r.query(ctx, build(match))
	if err != nil && ctx.Err() == nil && !errors.Is(err, ErrNoIndex) && !errors.Is(err, context.DeadlineExceeded) {
		// Not valid FTS5, an unbalanced quote for example: search the words.
		match = plainFTS(text)
		if match == "" {
			return res, nil
		}
		rows, err = r.query(ctx, build(match))
	}
	if err != nil {
		return res, err
	}
	var ids []string
	for _, x := range rows {
		res.Total = num(x, "total")
		ids = append(ids, qn(int64(num(x, "mid"))))
	}
	snips := map[int]string{}
	if len(ids) > 0 {
		sn, err := r.query(ctx, fmt.Sprintf("SELECT rowid AS mid, snippet(msgs,0,char(57344),char(57345),'…',24) AS snippet FROM msgs WHERE msgs MATCH %s AND rowid IN (%s);", q(match), strings.Join(ids, ",")))
		if err != nil {
			return res, err
		}
		for _, x := range sn {
			snips[num(x, "mid")] = apiMarks.Replace(sget(x, "snippet"))
		}
	}
	for _, x := range rows {
		res.Hits = append(res.Hits, SearchHit{SessionMeta: metaOf(x), Matches: num(x, "matches"), Seq: num(x, "seq"), Sub: sget(x, "sub"), Role: sget(x, "role"), Snippet: snips[num(x, "mid")]})
	}
	if t := strings.TrimSpace(text); skip == 0 && shaRE.MatchString(t) {
		cs, err := r.query(ctx, fmt.Sprintf(`SELECT %s, c.sha AS sha, c.rel AS rel, c.subject AS subject FROM commits c JOIN sessions s ON s.host=c.host AND s.kind=c.kind AND s.id=c.session_id %s
WHERE c.sha LIKE %s AND %s ORDER BY CASE c.rel WHEN 'made' THEN 0 WHEN 'pushed' THEN 1 ELSE 2 END, s.started LIMIT 10;`, metaCols, metaJoins, q(t+"%"), f.where()))
		if err != nil {
			return res, err
		}
		for _, x := range cs {
			res.Commits = append(res.Commits, CommitHit{metaOf(x), sget(x, "sha"), sget(x, "rel"), sget(x, "subject")})
		}
	}
	return res, nil
}

// RecentResult is a page of sessions, newest first: what the app shows before
// anything is searched for.
type RecentResult struct {
	Total int           `json:"total"`
	Hits  []SessionMeta `json:"hits"`
}

// Recent lists sessions newest first, skipping skip. Filter.Role is about where
// words are, so it does not apply.
func (r *Reader) Recent(ctx context.Context, f Filter, skip int) (RecentResult, error) {
	res := RecentResult{Hits: []SessionMeta{}}
	rows, err := r.query(ctx, "SELECT count(*) AS n FROM sessions s WHERE "+f.where()+";")
	if err != nil {
		return res, err
	}
	if len(rows) == 1 {
		res.Total = num(rows[0], "n")
	}
	rows, err = r.query(ctx, fmt.Sprintf("SELECT %s FROM sessions s %s WHERE %s ORDER BY s.started DESC, s.host, s.kind, s.id LIMIT %d OFFSET %d;", metaCols, metaJoins, f.where(), f.limit(20), skip))
	if err != nil {
		return res, err
	}
	for _, x := range rows {
		res.Hits = append(res.Hits, metaOf(x))
	}
	return res, nil
}

// PageMsg is one message of a session.
type PageMsg struct {
	Seq     int    `json:"seq"`
	Role    string `json:"role"`
	TS      string `json:"ts,omitempty"`
	Text    string `json:"text"` // with \x02 \x03 around matches when the page was asked for with q
	ToolID  string `json:"tool_id,omitempty"`
	IsErr   bool   `json:"is_err,omitempty"`
	Secret  bool   `json:"secret,omitempty"`
	FullLen int    `json:"full_len,omitempty"`
	Hit     bool   `json:"hit,omitempty"`
}

// SubInfo is one subagent of a session.
type SubInfo struct {
	ID       string `json:"id"`
	Messages int    `json:"messages"`
}

// SessionPage is a part of one thread of a session.
type SessionPage struct {
	Session SessionMeta `json:"session"`
	Sub     string      `json:"sub"`
	Subs    []SubInfo   `json:"subs"`
	Msgs    []PageMsg   `json:"messages"`
	// More messages exist before the first and after the last.
	Before bool `json:"before"`
	After  bool `json:"after"`
}

// PageOpts picks the part. Without Seq, it is the start of the thread. With it,
// Dir says which side: "after" and "before" exclude Seq; otherwise the page is
// around Seq. Q marks the matches in the page.
type PageOpts struct {
	Seq   *int
	Dir   string
	Sub   string
	Limit int
	Q     string
}

func sessWhere(host, kind, id string) string {
	return "host=" + q(host) + " AND kind=" + q(kind) + " AND session_id=" + q(id)
}

const pageCols = "id,seq,role,ts,tool_id,is_err,secret,full_len,text"

// Session returns a page of a session, or ErrNoSession.
func (r *Reader) Session(ctx context.Context, host, kind, id string, o PageOpts) (SessionPage, error) {
	p := SessionPage{Sub: o.Sub, Subs: []SubInfo{}, Msgs: []PageMsg{}}
	rows, err := r.query(ctx, fmt.Sprintf("SELECT %s FROM sessions s %s WHERE s.host=%s AND s.kind=%s AND s.id=%s;", metaCols, metaJoins, q(host), q(kind), q(id)))
	if err != nil {
		return p, err
	}
	if len(rows) == 0 {
		return p, ErrNoSession
	}
	p.Session = metaOf(rows[0])
	base := sessWhere(host, kind, id) + " AND sub=" + q(o.Sub)
	limit := o.Limit
	if limit <= 0 {
		limit = 100
	}
	sql := func(cond, dir string, n int) string {
		return fmt.Sprintf("SELECT %s FROM messages WHERE %s%s ORDER BY seq %s LIMIT %d;", pageCols, base, cond, dir, n)
	}
	var back, fwd []map[string]any
	readBack := func(cond string, n int) error {
		back, err = r.query(ctx, sql(cond, "DESC", n+1))
		if err == nil && len(back) > n {
			back, p.Before = back[:n], true
		}
		return err
	}
	readFwd := func(cond string, n int) error {
		fwd, err = r.query(ctx, sql(cond, "ASC", n+1))
		if err == nil && len(fwd) > n {
			fwd, p.After = fwd[:n], true
		}
		return err
	}
	if o.Seq == nil {
		err = readFwd("", limit)
	} else {
		s := strconv.Itoa(*o.Seq)
		switch o.Dir {
		case "after":
			p.Before = true
			err = readFwd(" AND seq>"+s, limit)
		case "before":
			p.After = true
			err = readBack(" AND seq<"+s, limit)
		default:
			if err = readBack(" AND seq<"+s, limit/2); err == nil {
				err = readFwd(" AND seq>="+s, limit-limit/2)
			}
		}
	}
	if err != nil {
		return p, err
	}
	all := make([]map[string]any, 0, len(back)+len(fwd))
	for i := len(back) - 1; i >= 0; i-- {
		all = append(all, back[i])
	}
	all = append(all, fwd...)
	marked := map[int]string{}
	if strings.TrimSpace(o.Q) != "" && len(all) > 0 {
		ids := make([]string, len(all))
		for i, x := range all {
			ids[i] = qn(int64(num(x, "id")))
		}
		hl := func(match string) ([]map[string]any, error) {
			return r.query(ctx, fmt.Sprintf("SELECT rowid AS id, highlight(msgs,0,char(57344),char(57345)) AS text FROM msgs WHERE msgs MATCH %s AND rowid IN (%s);", q(match), strings.Join(ids, ",")))
		}
		hs, err := hl(FTSQuery(o.Q))
		if err != nil && ctx.Err() == nil {
			hs, err = hl(plainFTS(o.Q))
		}
		if err == nil {
			for _, x := range hs {
				marked[num(x, "id")] = apiMarks.Replace(sget(x, "text"))
			}
		}
	}
	for _, x := range all {
		m := PageMsg{Seq: num(x, "seq"), Role: sget(x, "role"), TS: sget(x, "ts"), Text: sget(x, "text"), ToolID: sget(x, "tool_id"),
			IsErr: num(x, "is_err") != 0, Secret: num(x, "secret") != 0, FullLen: num(x, "full_len")}
		if t, ok := marked[num(x, "id")]; ok {
			m.Text, m.Hit = t, true
		}
		p.Msgs = append(p.Msgs, m)
	}
	subs, err := r.query(ctx, "SELECT sub, count(*) AS n FROM messages WHERE "+sessWhere(host, kind, id)+" AND sub<>'' GROUP BY sub ORDER BY min(seq);")
	if err != nil {
		return p, err
	}
	for _, x := range subs {
		p.Subs = append(p.Subs, SubInfo{sget(x, "sub"), num(x, "n")})
	}
	return p, nil
}

// HitsCap is the most match positions a session returns.
const HitsCap = 500

// Hit is where a match is.
type Hit struct {
	Seq int    `json:"seq"`
	Sub string `json:"sub"`
}

// Hits lists where a query matches in a session, in order.
func (r *Reader) Hits(ctx context.Context, host, kind, id, text string) ([]Hit, error) {
	sql := func(match string) string {
		return fmt.Sprintf("SELECT m.seq AS seq, m.sub AS sub FROM msgs JOIN messages m ON m.id=msgs.rowid WHERE msgs MATCH %s AND m.host=%s AND m.kind=%s AND m.session_id=%s ORDER BY m.seq LIMIT %d;",
			q(match), q(host), q(kind), q(id), HitsCap)
	}
	rows, err := r.query(ctx, sql(FTSQuery(text)))
	if err != nil && ctx.Err() == nil && !errors.Is(err, ErrNoIndex) && !errors.Is(err, context.DeadlineExceeded) {
		rows, err = r.query(ctx, sql(plainFTS(text)))
	}
	if err != nil {
		return nil, err
	}
	out := []Hit{}
	for _, x := range rows {
		out = append(out, Hit{num(x, "seq"), sget(x, "sub")})
	}
	return out, nil
}

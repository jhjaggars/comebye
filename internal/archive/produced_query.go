package archive

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// ProducedItem is one thing a session made, with the message that made it and,
// for a commit, what git and GitHub said about where it went.
type ProducedItem struct {
	Session  SessionMeta `json:"session"`
	Type     string      `json:"type"` // commit | page | pr | issue | comment | release | image
	Ref      string      `json:"ref"`
	URL      string      `json:"url,omitempty"`
	Repo     string      `json:"repo,omitempty"`
	Branch   string      `json:"branch,omitempty"`
	Title    string      `json:"title,omitempty"`
	Rel      string      `json:"rel,omitempty"` // a commit's made | pushed | seen
	Detail   string      `json:"detail,omitempty"`
	Seq      int         `json:"seq"` // the message that made it
	Sub      string      `json:"sub,omitempty"`
	TS       string      `json:"ts,omitempty"`
	PatchID  string      `json:"patch_id,omitempty"`
	PR       string      `json:"pr,omitempty"`        // the PR a commit shipped in, once known
	Verdict  string      `json:"verdict,omitempty"`   // a commit: landed | notinmain | unknown | unchecked
	How      string      `json:"how,omitempty"`       // how the verdict was reached
	LandedAs string      `json:"landed_as,omitempty"` // the commit on main that holds the change, when another SHA
}

var landedAsRE = regexp.MustCompile(`\(on main as ([0-9a-f]{4,40})\)`)

const producedCols = metaCols + `, p.type AS type, p.ref AS ref, p.url AS url, p.repo AS prepo, p.branch AS pbranch, p.title AS ptitle, p.rel AS rel, p.detail AS detail,
p.seq AS pseq, p.sub AS psub, p.ts AS pts, p.patch_id AS patch_id, p.pr AS pr,
(SELECT o.kind FROM outcomes o WHERE o.kind LIKE 'commit-%' AND o.ref=p.ref LIMIT 1) AS okind,
(SELECT o.source FROM outcomes o WHERE o.kind LIKE 'commit-%' AND o.ref=p.ref LIMIT 1) AS ohow,
(SELECT o.text FROM outcomes o WHERE o.kind LIKE 'commit-%' AND o.ref=p.ref LIMIT 1) AS otext`

func producedOf(x map[string]any) ProducedItem {
	it := ProducedItem{Session: metaOf(x), Type: sget(x, "type"), Ref: sget(x, "ref"), URL: sget(x, "url"), Repo: sget(x, "prepo"), Branch: sget(x, "pbranch"),
		Title: sget(x, "ptitle"), Rel: sget(x, "rel"), Detail: sget(x, "detail"), Seq: num(x, "pseq"), Sub: sget(x, "psub"), TS: sget(x, "pts"),
		PatchID: sget(x, "patch_id"), PR: sget(x, "pr")}
	if it.Type == "commit" {
		it.Verdict = strings.TrimPrefix(sget(x, "okind"), "commit-")
		it.How = sget(x, "ohow")
		if m := landedAsRE.FindStringSubmatch(sget(x, "otext")); m != nil {
			it.LandedAs = m[1]
		}
	}
	return it
}

// ProducedBySession is the SQL for what one session made, in the order it did.
func producedBySessionSQL(host, kind, id string) string {
	return fmt.Sprintf(`SELECT %s FROM produced p JOIN sessions s ON s.host=p.host AND s.kind=p.kind AND s.id=p.session_id %s
WHERE p.host=%s AND p.kind=%s AND p.session_id=%s ORDER BY p.seq, p.rowid LIMIT 500;`, producedCols, metaJoins, q(host), q(kind), q(id))
}

var (
	refPG      = regexp.MustCompile(`^pg_[0-9a-f]{4,10}$`)
	refIssue   = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d+)$`)
	refURL     = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/(?:pull|issues)/(\d+)(?:[/?#].*)?$`)
	refRelease = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	refDigest  = regexp.MustCompile(`^sha256:[0-9a-f]{8,64}$`)
	refTagURL  = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/releases/tag/([^\s/]+)/?$`)
)

// ProducedLookupSQL builds the lookup for text that names an artifact: a commit
// (7 to 40 hex characters), a Page id, owner/repo#N or its GitHub URL, a release
// (owner/repo@tag) or an image digest. ok is false for any other text.
// A PR also finds the commits that shipped in it.
func producedLookupSQL(text string, f Filter, limit int) (string, bool) {
	t := strings.TrimSpace(text)
	var cond string
	switch {
	case shaRE.MatchString(strings.ToLower(t)):
		cond = "p.type='commit' AND p.ref LIKE " + q(strings.ToLower(t)+"%")
	case refPG.MatchString(t):
		cond = "p.type='page' AND p.ref LIKE " + q(t+"%")
	case refIssue.MatchString(t):
		cond = "(lower(p.ref)=lower(" + q(t) + ") OR (p.type='commit' AND lower(p.pr)=lower(" + q(t) + ")))"
	case refURL.MatchString(t):
		m := refURL.FindStringSubmatch(t)
		r := m[1] + "#" + m[2]
		cond = "(lower(p.ref)=lower(" + q(r) + ") OR (p.type='commit' AND lower(p.pr)=lower(" + q(r) + ")))"
	case refTagURL.MatchString(t):
		m := refTagURL.FindStringSubmatch(t)
		cond = "lower(p.ref)=lower(" + q(m[1]+"@"+m[2]) + ")"
	case refRelease.MatchString(t):
		cond = "p.type='release' AND lower(p.ref)=lower(" + q(t) + ")"
	case refDigest.MatchString(strings.ToLower(t)):
		cond = "p.type='image' AND p.ref LIKE " + q(strings.ToLower(t)+"%")
	default:
		return "", false
	}
	// A commit's made row first, then the ones that only pushed or saw it.
	return fmt.Sprintf(`SELECT %s FROM produced p JOIN sessions s ON s.host=p.host AND s.kind=p.kind AND s.id=p.session_id %s
WHERE %s AND %s ORDER BY CASE p.rel WHEN 'made' THEN 0 WHEN 'pushed' THEN 1 ELSE 2 END, s.started DESC, p.seq LIMIT %d;`,
		producedCols, metaJoins, cond, f.where(), limit), true
}

func producedRows(rows []map[string]any) []ProducedItem {
	out := make([]ProducedItem, 0, len(rows))
	for _, x := range rows {
		out = append(out, producedOf(x))
	}
	return out
}

// ProducedRef finds the sessions that made the artifact a text names, or
// ok=false when the text is not a reference. Filters narrow it like a search.
func (r *Reader) ProducedRef(ctx context.Context, text string, f Filter, limit int) (items []ProducedItem, ok bool, err error) {
	sql, ok := producedLookupSQL(text, f, limit)
	if !ok {
		return []ProducedItem{}, false, nil
	}
	rows, err := r.query(ctx, sql)
	return producedRows(rows), true, err
}

// ProducedBySession lists what a session made, or ErrNoSession.
func (r *Reader) ProducedBySession(ctx context.Context, host, kind, id string) ([]ProducedItem, error) {
	ex, err := r.query(ctx, fmt.Sprintf("SELECT 1 AS x FROM sessions WHERE host=%s AND kind=%s AND id=%s;", q(host), q(kind), q(id)))
	if err != nil {
		return nil, err
	}
	if len(ex) == 0 {
		return nil, ErrNoSession
	}
	rows, err := r.query(ctx, producedBySessionSQL(host, kind, id))
	return producedRows(rows), err
}

// ProducedRef is the Reader's lookup for the command line, which reads the file directly.
func (ix *Index) ProducedRef(text string, f Filter, limit int) ([]ProducedItem, bool, error) {
	sql, ok := producedLookupSQL(text, f, limit)
	if !ok {
		return nil, false, nil
	}
	rows, err := ix.Query(sql)
	return producedRows(rows), true, err
}

// ProducedBySession lists what a session made.
func (ix *Index) ProducedBySession(host, kind, id string) ([]ProducedItem, error) {
	rows, err := ix.Query(producedBySessionSQL(host, kind, id))
	return producedRows(rows), err
}

// producedCounts counts, for each session, what it made by type, for the chip line of a result row.
func (r *Reader) producedCounts(ctx context.Context, ms []SessionMeta) (map[string]map[string]int, error) {
	out := map[string]map[string]int{}
	if len(ms) == 0 {
		return out, nil
	}
	var keys []string
	for _, m := range ms {
		keys = append(keys, "("+q(m.Host)+","+q(m.Kind)+","+q(m.ID)+")")
	}
	rows, err := r.query(ctx, fmt.Sprintf("SELECT host, kind, session_id, type, count(DISTINCT ref) AS n FROM produced WHERE (host,kind,session_id) IN (VALUES %s) AND NOT (type='commit' AND rel='seen') GROUP BY host, kind, session_id, type;", strings.Join(keys, ",")))
	if err != nil {
		return out, err
	}
	for _, x := range rows {
		k := sget(x, "host") + "\x00" + sget(x, "kind") + "\x00" + sget(x, "session_id")
		if out[k] == nil {
			out[k] = map[string]int{}
		}
		out[k][sget(x, "type")] = num(x, "n")
	}
	return out, nil
}

func countKey(m SessionMeta) string { return m.Host + "\x00" + m.Kind + "\x00" + m.ID }

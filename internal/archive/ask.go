package archive

import (
	"fmt"
	"sort"
	"strings"
)

// Outcome is one fact about how a piece of work ended, with its source.
type Outcome struct {
	Kind   string `json:"kind"` // report, commit-landed, commit-notinmain, commit-unchecked, pr-merged, pr-open, pr-closed, doc, deploy, decision, abandoned, unmerged, handoff
	Ref    string `json:"ref,omitempty"`
	Text   string `json:"text"`
	Source string `json:"source"`
	At     string `json:"at,omitempty"`
}

// SessionRefView names a session of a thread.
type SessionRefView struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Host    string `json:"host"`
	Title   string `json:"title"`
	Started string `json:"started"`
	Handoff string `json:"handoff,omitempty"`
}

// ThreadView is one piece of work: what it was, when, how it ended, and
// where to dig in.
type ThreadView struct {
	ID       string           `json:"id"`
	Title    string           `json:"title"`
	Brief    string           `json:"brief,omitempty"`
	Started  string           `json:"started"`
	Ended    string           `json:"ended"`
	Hosts    string           `json:"hosts"`
	Repos    string           `json:"repos"`
	Branches string           `json:"branches"`
	Verdict  string           `json:"verdict"` // landed | open | unlanded | abandoned | failed | reported | unknown
	Outcome  string           `json:"outcome"`
	Keywords string           `json:"keywords"`
	Summary  string           `json:"summary,omitempty"`
	Outcomes []Outcome        `json:"outcomes,omitempty"`
	Sessions []SessionRefView `json:"sessions"`
	Score    float64          `json:"score,omitempty"`
}

var questionNoise = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`past worked working thing something similar like forgot forget outcome remember previously before once time ago earlier happened happen result results what whats how did were was year month week last long which went gone turned ended end`) {
		m[w] = true
	}
	return m
}()

// QueryTerms reduces a descriptive question to the words that carry the
// topic: lowercase, no stop words, no question filler.
func QueryTerms(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range tokRE.FindAllString(strings.ToLower(text), -1) {
		if stop[w] || questionNoise[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

func orQuery(terms []string) string {
	var p []string
	for _, t := range terms {
		p = append(p, `"`+t+`"`)
	}
	return strings.Join(p, " OR ")
}

func (f Filter) threadWhere() string {
	c := []string{"1=1"}
	if f.Since != "" {
		c = append(c, "t.ended >= "+q(f.Since))
	}
	if f.Until != "" {
		c = append(c, "t.started < "+q(f.Until))
	}
	if f.Host != "" {
		// Exact, as in Filter.where: a worker's bare name finds its VMs (name.instance).
		list := "','||t.hosts||','"
		c = append(c, "(instr("+list+","+q(","+f.Host+",")+")>0 OR instr("+list+","+q(","+f.Host+".")+")>0)")
	}
	if f.Project != "" {
		p := q("%" + f.Project + "%")
		c = append(c, fmt.Sprintf("(t.repos LIKE %s OR t.branches LIKE %s OR t.title LIKE %s OR t.brief LIKE %s)", p, p, p, p))
	}
	if f.Branch != "" {
		c = append(c, "t.branches LIKE "+q("%"+f.Branch+"%"))
	}
	if f.Kind != "" {
		c = append(c, "EXISTS(SELECT 1 FROM thread_sessions x WHERE x.thread_id=t.id AND x.kind="+q(f.Kind)+")")
	}
	if f.Ledger != "" {
		c = append(c, "EXISTS(SELECT 1 FROM thread_sessions x JOIN sessions s ON s.host=x.host AND s.kind=x.kind AND s.id=x.session_id WHERE x.thread_id=t.id AND s.ledger_id="+q(f.Ledger)+")")
	}
	return strings.Join(c, " AND ")
}

// Ask finds the pieces of work a descriptive question is about. It ranks
// threads two ways and fuses them (reciprocal rank): a search over each
// thread's digest (title, handoff brief, outcome, report, keywords,
// summaries, the user's decision lines) and a search over the raw messages
// of its sessions. Any of the question's topic words may match; rare words
// count for more.
func (ix *Index) Ask(question string, f Filter, n int) ([]ThreadView, error) {
	terms := QueryTerms(question)
	if len(terms) == 0 {
		return nil, fmt.Errorf("no topic words in %q", question)
	}
	if len(terms) > 12 {
		terms = terms[:12]
	}
	mq := q(orQuery(terms))
	rank := map[string]float64{}
	add := func(rows []map[string]any) {
		for i, r := range rows {
			rank[sget(r, "thread_id")] += 1.0 / float64(60+i)
		}
	}
	rows, err := ix.Query(fmt.Sprintf("SELECT thread_id, bm25(thread_fts, 8, 5, 5, 4, 2, 3) AS sc FROM thread_fts WHERE thread_fts MATCH %s ORDER BY sc LIMIT 60;", mq))
	if err != nil {
		return nil, err
	}
	add(rows)
	rows, err = ix.Query(fmt.Sprintf(`SELECT thread_id, min(sc) AS sc FROM (
  SELECT x.thread_id AS thread_id, msgs.rank AS sc FROM msgs JOIN messages m ON m.id=msgs.rowid
  JOIN thread_sessions x ON x.host=m.host AND x.kind=m.kind AND x.session_id=m.session_id
  WHERE msgs MATCH %s ORDER BY msgs.rank LIMIT 600) GROUP BY thread_id ORDER BY sc LIMIT 60;`, mq))
	if err != nil {
		return nil, err
	}
	add(rows)
	type sc struct {
		id string
		s  float64
	}
	var ranked []sc
	for id, s := range rank {
		ranked = append(ranked, sc{id, s})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].s != ranked[j].s {
			return ranked[i].s > ranked[j].s
		}
		return ranked[i].id < ranked[j].id
	})
	if n <= 0 {
		n = 5
	}
	var out []ThreadView
	for _, r := range ranked {
		ok, err := ix.Query(fmt.Sprintf("SELECT t.id FROM threads t WHERE t.id=%s AND %s;", q(r.id), f.threadWhere()))
		if err != nil {
			return nil, err
		}
		if len(ok) == 0 {
			continue
		}
		v, err := ix.Thread(r.id)
		if err != nil {
			return nil, err
		}
		v.Score = r.s
		out = append(out, *v)
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

// Thread loads one piece of work by id (or unique prefix).
func (ix *Index) Thread(id string) (*ThreadView, error) {
	rows, err := ix.Query(fmt.Sprintf("SELECT * FROM threads WHERE id LIKE %s LIMIT 3;", q(id+"%")))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("thread %q: %d matches", id, len(rows))
	}
	r := rows[0]
	v := &ThreadView{ID: sget(r, "id"), Title: sget(r, "title"), Brief: sget(r, "brief"), Started: sget(r, "started"), Ended: sget(r, "ended"),
		Hosts: sget(r, "hosts"), Repos: sget(r, "repos"), Branches: sget(r, "branches"), Verdict: sget(r, "verdict"), Outcome: sget(r, "outcome"),
		Keywords: sget(r, "keywords"), Summary: sget(r, "summary")}
	os, err := ix.Query(fmt.Sprintf("SELECT kind,ref,text,source,at FROM outcomes WHERE thread_id=%s ORDER BY rowid;", q(v.ID)))
	if err != nil {
		return nil, err
	}
	for _, o := range os {
		v.Outcomes = append(v.Outcomes, Outcome{sget(o, "kind"), sget(o, "ref"), sget(o, "text"), sget(o, "source"), sget(o, "at")})
	}
	ss, err := ix.Query(fmt.Sprintf(`SELECT s.kind,s.id,s.host,s.title,s.started,s.ledger_id FROM thread_sessions x JOIN sessions s
ON s.host=x.host AND s.kind=x.kind AND s.id=x.session_id WHERE x.thread_id=%s ORDER BY s.started;`, q(v.ID)))
	if err != nil {
		return nil, err
	}
	for _, s := range ss {
		v.Sessions = append(v.Sessions, SessionRefView{sget(s, "kind"), sget(s, "id"), sget(s, "host"), sget(s, "title"), sget(s, "started"), sget(s, "ledger_id")})
	}
	return v, nil
}

// Threads lists pieces of work, newest first.
func (ix *Index) Threads(f Filter, unsummarized bool) ([]ThreadView, error) {
	w := f.threadWhere()
	if unsummarized {
		w += " AND t.summary=''"
	}
	rows, err := ix.Query(fmt.Sprintf("SELECT t.id FROM threads t WHERE %s ORDER BY t.ended DESC LIMIT %d;", w, f.limit(30)))
	if err != nil {
		return nil, err
	}
	var out []ThreadView
	for _, r := range rows {
		v, err := ix.Thread(sget(r, "id"))
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}

// ThreadOf returns the thread a session belongs to, if any.
func (ix *Index) ThreadOf(host, kind, id string) string {
	rows, _ := ix.Query(fmt.Sprintf("SELECT thread_id FROM thread_sessions WHERE host=%s AND kind=%s AND session_id=%s;", q(host), q(kind), q(id)))
	if len(rows) == 0 {
		return ""
	}
	return sget(rows[0], "thread_id")
}

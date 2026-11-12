package archive

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// The app's view of threads (pieces of work), read-only like the rest of Reader.

// ThreadRow is one thread of a Work search: what it was and how it ended, without
// the outcomes and sessions that the thread page loads.
type ThreadRow struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Brief    string `json:"brief,omitempty"`
	Started  string `json:"started"`
	Ended    string `json:"ended"`
	Hosts    string `json:"hosts"`
	Repos    string `json:"repos,omitempty"`
	Branches string `json:"branches,omitempty"`
	Verdict  string `json:"verdict"`
	Outcome  string `json:"outcome,omitempty"`
	Sessions int    `json:"sessions"`
}

// WorkResult is a page of threads that a question is about, best first.
type WorkResult struct {
	Total   int         `json:"total"`
	Threads []ThreadRow `json:"threads"`
}

func rowOfThread(x map[string]any) ThreadRow {
	return ThreadRow{ID: sget(x, "id"), Title: sget(x, "title"), Brief: clip(sget(x, "brief"), 400), Started: sget(x, "started"), Ended: sget(x, "ended"),
		Hosts: sget(x, "hosts"), Repos: sget(x, "repos"), Branches: sget(x, "branches"), Verdict: sget(x, "verdict"), Outcome: clip(sget(x, "outcome"), 300),
		Sessions: num(x, "n_sessions")}
}

// Work finds the threads a question is about, as Index.Ask does (any topic word may
// match; the digest and the messages are ranked and fused), in a few queries however
// many threads there are.
func (r *Reader) Work(ctx context.Context, question string, f Filter, skip int) (WorkResult, error) {
	res := WorkResult{Threads: []ThreadRow{}}
	terms := QueryTerms(question)
	if len(terms) == 0 {
		return res, nil
	}
	if len(terms) > 12 {
		terms = terms[:12]
	}
	mq := q(orQuery(terms))
	rank := map[string]float64{}
	add := func(rows []map[string]any) {
		for i, x := range rows {
			rank[sget(x, "thread_id")] += 1.0 / float64(60+i)
		}
	}
	rows, err := r.query(ctx, fmt.Sprintf("SELECT thread_id, bm25(thread_fts, 8, 5, 5, 4, 2, 3) AS sc FROM thread_fts WHERE thread_fts MATCH %s ORDER BY sc LIMIT 60;", mq))
	if err != nil {
		return res, err
	}
	add(rows)
	rows, err = r.query(ctx, fmt.Sprintf(`SELECT thread_id, min(sc) AS sc FROM (
  SELECT x.thread_id AS thread_id, msgs.rank AS sc FROM msgs JOIN messages m ON m.id=msgs.rowid
  JOIN thread_sessions x ON x.host=m.host AND x.kind=m.kind AND x.session_id=m.session_id
  WHERE msgs MATCH %s ORDER BY msgs.rank LIMIT 600) GROUP BY thread_id ORDER BY sc LIMIT 60;`, mq))
	if err != nil {
		return res, err
	}
	add(rows)
	if len(rank) == 0 {
		return res, nil
	}
	type scored struct {
		id string
		s  float64
	}
	var ranked []scored
	quoted := make([]string, 0, len(rank))
	for id, s := range rank {
		ranked = append(ranked, scored{id, s})
		quoted = append(quoted, q(id))
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].s != ranked[j].s {
			return ranked[i].s > ranked[j].s
		}
		return ranked[i].id < ranked[j].id
	})
	// One query for the filter and the rows of every candidate.
	rows, err = r.query(ctx, fmt.Sprintf(`SELECT t.id, t.title, t.brief, t.started, t.ended, t.hosts, t.repos, t.branches, t.verdict, t.outcome,
  (SELECT count(*) FROM thread_sessions x WHERE x.thread_id=t.id) AS n_sessions
FROM threads t WHERE t.id IN (%s) AND %s;`, strings.Join(quoted, ","), f.threadWhere()))
	if err != nil {
		return res, err
	}
	byID := map[string]ThreadRow{}
	for _, x := range rows {
		byID[sget(x, "id")] = rowOfThread(x)
	}
	var kept []ThreadRow
	for _, c := range ranked {
		if t, ok := byID[c.id]; ok {
			kept = append(kept, t)
		}
	}
	res.Total = len(kept)
	if skip > len(kept) {
		skip = len(kept)
	}
	kept = kept[skip:]
	if n := f.limit(20); len(kept) > n {
		kept = kept[:n]
	}
	res.Threads = append(res.Threads, kept...)
	return res, nil
}

// Thread loads one thread by id (or a unique prefix of 8 or more characters).
func (r *Reader) Thread(ctx context.Context, id string) (*ThreadView, error) {
	rows, err := r.query(ctx, fmt.Sprintf("SELECT * FROM threads WHERE id LIKE %s LIMIT 3;", q(escLike(id)+"%")))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, ErrNoThread
	}
	x := rows[0]
	v := &ThreadView{ID: sget(x, "id"), Title: sget(x, "title"), Brief: sget(x, "brief"), Started: sget(x, "started"), Ended: sget(x, "ended"),
		Hosts: sget(x, "hosts"), Repos: sget(x, "repos"), Branches: sget(x, "branches"), Verdict: sget(x, "verdict"), Outcome: sget(x, "outcome"),
		Keywords: sget(x, "keywords"), Summary: sget(x, "summary"), Outcomes: []Outcome{}, Sessions: []SessionRefView{}}
	os, err := r.query(ctx, fmt.Sprintf("SELECT kind,ref,text,source,at FROM outcomes WHERE thread_id=%s ORDER BY rowid;", q(v.ID)))
	if err != nil {
		return nil, err
	}
	for _, o := range os {
		v.Outcomes = append(v.Outcomes, Outcome{sget(o, "kind"), sget(o, "ref"), sget(o, "text"), sget(o, "source"), sget(o, "at")})
	}
	ss, err := r.query(ctx, fmt.Sprintf(`SELECT s.kind,s.id,s.host,s.title,s.started,s.ledger_id FROM thread_sessions x JOIN sessions s
ON s.host=x.host AND s.kind=x.kind AND s.id=x.session_id WHERE x.thread_id=%s ORDER BY s.started;`, q(v.ID)))
	if err != nil {
		return nil, err
	}
	for _, s := range ss {
		v.Sessions = append(v.Sessions, SessionRefView{sget(s, "kind"), sget(s, "id"), sget(s, "host"), sget(s, "title"), sget(s, "started"), sget(s, "ledger_id")})
	}
	return v, nil
}

// escLike keeps a thread id from acting as a LIKE pattern: ids are hex, so anything
// else cannot match one.
func escLike(id string) string {
	return strings.NewReplacer("%", "", "_", "").Replace(id)
}

// LinkResult is what a handoff or an agent led to: its sessions, and the thread
// they belong to when they have one.
type LinkResult struct {
	Sessions []SessionMeta `json:"sessions"`
	Thread   string        `json:"thread_id,omitempty"`
}

// ForLink lists the sessions that a handoff (f.Ledger) or an agent (f.Agent) made,
// oldest first, for a link to land on.
func (r *Reader) ForLink(ctx context.Context, f Filter) (LinkResult, error) {
	res := LinkResult{Sessions: []SessionMeta{}}
	f.Limit = 0
	rows, err := r.query(ctx, fmt.Sprintf("SELECT %s FROM sessions s %s WHERE %s ORDER BY s.started, s.host, s.kind, s.id LIMIT 50;", metaCols, metaJoins, f.where()))
	if err != nil {
		return res, err
	}
	for _, x := range rows {
		m := metaOf(x)
		res.Sessions = append(res.Sessions, m)
		if res.Thread == "" {
			res.Thread = m.Thread
		}
	}
	return res, nil
}

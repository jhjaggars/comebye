package usage

import (
	"errors"
	"sort"
	"time"
)

// Query asks for a window of usage, grouped by one dimension.
type Query struct {
	From, To time.Time // [From, To)
	// By is what the groups are: kind, model, host, project, role, purpose,
	// handoff, session or token (input, output, cache write, cache read).
	By     string
	Bucket string // hour or day
	TZ     int    // minutes east of UTC, for day buckets
	// Filter keeps only facts whose dimension equals the value (the same
	// dimensions as By, except token).
	Filter map[string]string
	Limit  int // top sessions and handoffs; default 10
	Prices PriceTable
}

// Dimensions a query may group or filter by.
var Dimensions = []string{"kind", "model", "host", "project", "role", "purpose", "handoff", "session"}

// Total is a Tokens with its derived figures.
type Total struct {
	Tokens
	Fresh int64   `json:"fresh"`
	USD   float64 `json:"usd,omitempty"`
	// Unpriced is the fresh tokens that no price covered, so USD is partial.
	Unpriced int64 `json:"unpriced,omitempty"`
	// Hit is cache reads / (cache reads + cache writes + input): how much of
	// the prompt side came from the cache.
	Hit float64 `json:"hit"`
}

func (t *Total) finish() {
	t.Fresh = t.Tokens.Fresh()
	if d := t.CR + t.CW + t.In; d > 0 {
		t.Hit = float64(t.CR) / float64(d)
	}
}

// Group is one slice of the stacked chart.
type Group struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	Total
	Series []int64 `json:"series"`          // fresh tokens per bucket (cache reads for the cache-read group)
	Apart  bool    `json:"apart,omitempty"` // by=token: cache reads, drawn apart from the fresh stack
}

// SessionRow is a session in the top list.
type SessionRow struct {
	Key     string `json:"key"`
	Host    string `json:"host"`
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Project string `json:"project,omitempty"`
	Role    string `json:"role,omitempty"`
	Purpose string `json:"purpose,omitempty"`
	Handoff string `json:"handoff,omitempty"`
	Name    string `json:"name,omitempty"`
	Branch  string `json:"branch,omitempty"`
	First   int64  `json:"first"`
	Last    int64  `json:"last"`
	Total
}

// HandoffRow is a handoff in the top list.
type HandoffRow struct {
	Handoff  string `json:"handoff"`
	Name     string `json:"name,omitempty"`
	Project  string `json:"project,omitempty"`
	Purpose  string `json:"purpose,omitempty"`
	Sessions int    `json:"sessions"`
	Total
}

// Result answers a Query.
type Result struct {
	From        int64        `json:"from"`
	To          int64        `json:"to"`
	Bucket      string       `json:"bucket"`
	By          string       `json:"by"`
	TZ          int          `json:"tz"`
	Buckets     []int64      `json:"buckets"` // start of each bucket, unix seconds
	Totals      Total        `json:"totals"`
	Series      []int64      `json:"series"`    // fresh tokens per bucket, all groups
	CRSeries    []int64      `json:"cr_series"` // cache reads per bucket
	Groups      []Group      `json:"groups"`
	TopSessions []SessionRow `json:"top_sessions"`
	TopHandoffs []HandoffRow `json:"top_handoffs"`
	// Cache is the efficiency of the window: the share of the prompt side served
	// from the cache, reads per write, and the estimate of what the reads saved
	// against paying the input price.
	Cache struct {
		Hit          float64 `json:"hit"`
		ReadPerWrite float64 `json:"read_per_write"`
		SavedUSD     float64 `json:"saved_usd,omitempty"`
	} `json:"cache"`
	// SessionsFrom is the oldest hour that still has per-session detail;
	// older hours are rollups (no top lists, no handoff or session grouping).
	SessionsFrom int64 `json:"sessions_from"`
}

// fact is one (session, hour, model) cell, or an hourly rollup.
type fact struct {
	h                                    int64
	host, kind, model                    string
	project, role, purpose, handoff, key string
	t                                    Tokens
	sess                                 *Session
}

func (f *fact) dim(d string) string {
	switch d {
	case "kind":
		return f.kind
	case "model":
		return f.model
	case "host":
		return f.host
	case "project":
		return orNone(f.project)
	case "role":
		return orNone(f.role)
	case "purpose":
		return orNone(f.purpose)
	case "handoff":
		return orNone(f.handoff)
	case "session":
		return f.key
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// ErrQuery is a bad query.
var ErrQuery = errors.New("bad usage query")

func bucketStart(unix int64, bucket string, tz int) int64 {
	if bucket == "hour" {
		return unix - unix%3600
	}
	off := int64(tz) * 60
	t := unix + off
	t -= ((t % 86400) + 86400) % 86400
	return t - off
}

// Run answers q from the store.
func (st *Store) Run(q Query) (*Result, error) {
	if q.Bucket != "hour" && q.Bucket != "day" {
		q.Bucket = "day"
	}
	if q.By == "" {
		q.By = "kind"
	}
	if q.By != "token" {
		ok := false
		for _, d := range Dimensions {
			ok = ok || d == q.By
		}
		if !ok {
			return nil, ErrQuery
		}
	}
	for k := range q.Filter {
		ok := false
		for _, d := range Dimensions {
			ok = ok || d == k
		}
		if !ok {
			return nil, ErrQuery
		}
	}
	if !q.From.Before(q.To) {
		return nil, ErrQuery
	}
	if q.Limit <= 0 {
		q.Limit = 10
	}
	step := int64(3600)
	if q.Bucket == "day" {
		step = 86400
	}
	from, to := q.From.Unix(), q.To.Unix()
	first := bucketStart(from, q.Bucket, q.TZ)
	var buckets []int64
	for b := first; b < to; {
		buckets = append(buckets, b)
		if q.Bucket == "day" {
			b = bucketStart(b+step+3600*2, q.Bucket, q.TZ) // a fixed offset: the next midnight
		} else {
			b += step
		}
		if len(buckets) > 4000 {
			return nil, ErrQuery
		}
	}
	idx := func(unix int64) int {
		b := bucketStart(unix, q.Bucket, q.TZ)
		i := sort.Search(len(buckets), func(i int) bool { return buckets[i] >= b })
		if i < len(buckets) && buckets[i] == b {
			return i
		}
		return -1
	}

	res := &Result{From: from, To: to, Bucket: q.Bucket, By: q.By, TZ: q.TZ, Buckets: buckets}
	res.Series = make([]int64, len(buckets))
	res.CRSeries = make([]int64, len(buckets))
	groups := map[string]*Group{}
	sessions := map[string]*SessionRow{}
	handoffs := map[string]*HandoffRow{}
	handoffSess := map[string]map[string]bool{}
	var saved float64

	grp := func(key string) *Group {
		g := groups[key]
		if g == nil {
			g = &Group{Key: key, Series: make([]int64, len(buckets))}
			groups[key] = g
		}
		return g
	}
	addTo := func(t *Total, tok Tokens, model string) {
		t.Tokens.Add(tok)
		if usd, ok := q.Prices.USD(model, tok); ok {
			t.USD += usd
		} else {
			t.Unpriced += tok.Fresh()
		}
	}

	st.mu.Lock()
	oldest := st.now().Unix() / 3600
	visit := func(f *fact) {
		if f.h*3600 < from || f.h*3600 >= to {
			return
		}
		for k, v := range q.Filter {
			if f.dim(k) != v {
				return
			}
		}
		i := idx(f.h * 3600)
		if i < 0 {
			return
		}
		addTo(&res.Totals, f.t, f.model)
		res.Series[i] += f.t.Fresh()
		res.CRSeries[i] += f.t.CR
		if p, ok := q.Prices.Lookup(f.model); ok {
			saved += float64(f.t.CR) * (p.In - p.CR) / 1e6
		}
		if q.By == "token" {
			for _, c := range []struct {
				k     string
				v     int64
				apart bool
			}{{"input", f.t.In, false}, {"output", f.t.Out, false}, {"cache write", f.t.CW, false}, {"cache read", f.t.CR, true}} {
				g := grp(c.k)
				g.Apart = c.apart
				g.Series[i] += c.v
				switch c.k {
				case "input":
					g.In += c.v
				case "output":
					g.Out += c.v
				case "cache write":
					g.CW += c.v
				default:
					g.CR += c.v
				}
			}
		} else {
			g := grp(f.dim(q.By))
			addTo(&g.Total, f.t, f.model)
			g.Series[i] += f.t.Fresh()
			if q.By == "handoff" && g.Label == "" && f.sess != nil {
				g.Label = f.sess.L.Name
			}
			if q.By == "session" && g.Label == "" && f.sess != nil {
				g.Label = f.sess.L.Name
			}
		}
		if f.sess != nil {
			s := f.sess
			r := sessions[f.key]
			if r == nil {
				r = &SessionRow{Key: f.key, Host: s.Host, Kind: s.Kind, ID: s.ID, Project: s.L.Project, Role: s.L.Role,
					Purpose: s.L.Purpose, Handoff: s.L.Handoff, Name: s.L.Name, Branch: s.Branch, First: s.First, Last: s.Last}
				sessions[f.key] = r
			}
			addTo(&r.Total, f.t, f.model)
			if s.L.Handoff != "" {
				hr := handoffs[s.L.Handoff]
				if hr == nil {
					hr = &HandoffRow{Handoff: s.L.Handoff, Name: s.L.Name, Project: s.L.Project, Purpose: s.L.Purpose}
					handoffs[s.L.Handoff] = hr
					handoffSess[s.L.Handoff] = map[string]bool{}
				}
				handoffSess[s.L.Handoff][f.key] = true
				addTo(&hr.Total, f.t, f.model)
			}
		}
	}
	var f fact
	for _, s := range st.sessions {
		f = fact{host: s.Host, kind: s.Kind, project: s.L.Project, role: s.L.Role, purpose: s.L.Purpose, handoff: s.L.Handoff, key: s.Key(), sess: s}
		for _, r := range s.Rows {
			f.h, f.model, f.t = r.H, r.M, r.T
			visit(&f)
		}
		if s.First != 0 && s.First/3600 < oldest {
			oldest = s.First / 3600
		}
	}
	for k, t := range st.archive {
		visit(&fact{h: k.H, host: k.Host, kind: k.Kind, model: k.Model, project: k.Project, role: k.Role, purpose: k.Purpose, handoff: k.Handoff, key: "(rolled up)", t: t})
	}
	st.mu.Unlock()
	res.SessionsFrom = oldest * 3600

	res.Totals.finish()
	res.Cache.Hit = res.Totals.Hit
	if res.Totals.CW > 0 {
		res.Cache.ReadPerWrite = float64(res.Totals.CR) / float64(res.Totals.CW)
	}
	res.Cache.SavedUSD = saved

	for _, g := range groups {
		g.finish()
		res.Groups = append(res.Groups, *g)
	}
	sort.Slice(res.Groups, func(i, j int) bool {
		a, b := res.Groups[i], res.Groups[j]
		if q.By == "token" && a.Apart != b.Apart {
			return b.Apart
		}
		if a.Fresh != b.Fresh {
			return a.Fresh > b.Fresh
		}
		return a.Key < b.Key
	})
	if q.By != "token" && len(res.Groups) > 8 {
		other := Group{Key: "other", Series: make([]int64, len(buckets))}
		for _, g := range res.Groups[7:] {
			other.Total.Tokens.Add(g.Tokens)
			other.USD += g.USD
			other.Unpriced += g.Unpriced
			for i, v := range g.Series {
				other.Series[i] += v
			}
		}
		other.finish()
		res.Groups = append(res.Groups[:7], other)
	}
	for _, r := range sessions {
		r.finish()
		res.TopSessions = append(res.TopSessions, *r)
	}
	sort.Slice(res.TopSessions, func(i, j int) bool {
		if res.TopSessions[i].Fresh != res.TopSessions[j].Fresh {
			return res.TopSessions[i].Fresh > res.TopSessions[j].Fresh
		}
		return res.TopSessions[i].Key < res.TopSessions[j].Key
	})
	if len(res.TopSessions) > q.Limit {
		res.TopSessions = res.TopSessions[:q.Limit]
	}
	for id, r := range handoffs {
		r.Sessions = len(handoffSess[id])
		r.finish()
		res.TopHandoffs = append(res.TopHandoffs, *r)
	}
	sort.Slice(res.TopHandoffs, func(i, j int) bool {
		if res.TopHandoffs[i].Fresh != res.TopHandoffs[j].Fresh {
			return res.TopHandoffs[i].Fresh > res.TopHandoffs[j].Fresh
		}
		return res.TopHandoffs[i].Handoff < res.TopHandoffs[j].Handoff
	})
	if len(res.TopHandoffs) > q.Limit {
		res.TopHandoffs = res.TopHandoffs[:q.Limit]
	}
	if res.Groups == nil {
		res.Groups = []Group{}
	}
	if res.TopSessions == nil {
		res.TopSessions = []SessionRow{}
	}
	if res.TopHandoffs == nil {
		res.TopHandoffs = []HandoffRow{}
	}
	return res, nil
}

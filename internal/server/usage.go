package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/usage"
)

// Token usage. Every host scans its agents' transcripts for counts (never
// text) into usage.json in its state directory; the hub gathers its peers'
// counts through GET /api/usage/sessions (a hub-only call, like the worker
// operations), labels each session with its project, handoff and role from
// the ledger, and answers GET /api/usage.
//
// Who may read: the user, and a coordinator (any host; its request is
// forwarded to the hub). A worker, a tagged node and an agent that is
// neither get a 403. Only the user changes the prices, and only the hub
// calls a peer's sessions.

// UsageConfig configures the token counting.
type UsageConfig struct {
	// Off turns the counting off.
	Off bool
	// Sources are the session directories scanned (Claude Code's and Pi's).
	Sources []usage.Source
	// Interval is the pause between scans (default 2 minutes).
	Interval time.Duration
}

const defaultUsageInterval = 2 * time.Minute

type usageState struct {
	store    *usage.Store
	prices   *usagePrices
	mu       sync.Mutex
	scanning bool
	lastScan time.Time
	scanErr  string
}

func (s *Server) initUsage() {
	if s.cfg.StateDir == "" || s.cfg.Usage.Off {
		return
	}
	st, err := usage.Open(filepath.Join(s.cfg.StateDir, "usage.json"), s.fleet.LocalName())
	if err != nil {
		log.Printf("usage: %v (token counting is off; the file is left as it is)", err)
		return
	}
	s.usage = &usageState{store: st}
	if s.ledger != nil {
		s.usage.prices = openUsagePrices(filepath.Join(s.cfg.StateDir, "usage-prices.json"))
	}
}

// RunUsage scans this host's transcripts, and on the hub gathers the
// peers', until ctx ends.
func (s *Server) RunUsage(ctx context.Context) {
	if s.usage == nil {
		return
	}
	iv := s.cfg.Usage.Interval
	if iv <= 0 {
		iv = defaultUsageInterval
	}
	// A second pass soon after the first: the hub has not reached its peers
	// yet when it starts.
	wait := 10 * time.Second
	if iv < wait {
		wait = iv
	}
	for {
		s.usageOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = iv
	}
}

// usageOnce is one pass: scan, gather, label, prune, save.
func (s *Server) usageOnce(ctx context.Context) {
	u := s.usage
	u.mu.Lock()
	u.scanning = true
	u.mu.Unlock()
	_, err := u.store.Scan(ctx, s.cfg.Usage.Sources)
	if s.ledger != nil {
		s.gatherUsage(ctx)
		u.store.Relabel(s.usageLabeler())
	}
	u.store.Prune()
	if serr := u.store.Save(); serr != nil && err == nil {
		err = serr
	}
	u.mu.Lock()
	u.scanning, u.lastScan, u.scanErr = false, time.Now(), ""
	if err != nil && !errors.Is(err, context.Canceled) {
		u.scanErr = err.Error()
		log.Printf("usage: %v", err)
	}
	u.mu.Unlock()
}

// gatherUsage pulls the counts each peer has changed since last time.
func (s *Server) gatherUsage(ctx context.Context) {
	for _, h := range s.fleet.Hosts() {
		if h.Local || h.Status != "ok" {
			continue
		}
		for round := 0; round < 40; round++ {
			since, epoch := s.usage.store.PeerCursor(h.Name)
			q := url.Values{"since": {strconv.FormatInt(since, 10)}, "epoch": {epoch}, "limit": {"100"}}
			status, body, err := s.fleet.CallHost(ctx, h.Name, "usage-sessions", q, nil)
			if err != nil {
				s.usage.store.PeerFailed(h.Name, err.Error())
				break
			}
			if status == http.StatusNotFound {
				s.usage.store.PeerFailed(h.Name, "this peer's comebye does not count tokens yet (upgrade it)")
				break
			}
			var snap usage.Snapshot
			if status != http.StatusOK || json.Unmarshal(body, &snap) != nil {
				s.usage.store.PeerFailed(h.Name, "unexpected answer ("+strconv.Itoa(status)+")")
				break
			}
			s.usage.store.Merge(h.Name, snap)
			if !snap.More {
				break
			}
		}
	}
}

var (
	worktreeRE = regexp.MustCompile(`/\.worktrees/([^/]+)/([^/]+)`)
	scratchRE  = regexp.MustCompile(`^(tmp|pi-agent|\.playwright|comebye-pi\d*|pi\d+)`)
	homeDirRE  = regexp.MustCompile(`^(/home/[^/]+|/Users/[^/]+|/root)$`)
)

// purposeOfPrefix is the purpose a branch prefix suggests.
var purposeOfPrefix = map[string]string{
	"feat": "build", "feature": "build", "fix": "build", "chore": "build", "refactor": "build", "poc": "build", "impl": "build", "build": "build", "test": "build", "docs": "build",
	"research": "research", "investigate": "research", "spike": "research", "diag": "research", "debug": "research", "try": "research",
	"review": "review",
	"plan":   "plan", "design": "plan", "rfc": "plan", "mock": "plan", "proto": "plan",
	"release": "rollout", "deploy": "rollout", "rollout": "rollout", "ci": "rollout", "merge": "rollout", "ops": "rollout",
}

// usagePurpose: the handoff's --purpose when it set one (the flag wins), else
// what the branch prefix suggests.
func usagePurpose(flag, branch string) string {
	if flag != "" {
		if flag == "spike" {
			return "research"
		}
		return flag
	}
	pre := branch
	if i := strings.IndexAny(pre, "-/"); i > 0 {
		pre = pre[:i]
	}
	return purposeOfPrefix[strings.ToLower(pre)]
}

// usageLabeler builds the function that labels a session from the ledger,
// the running coordinators and the path it ran in.
func (s *Server) usageLabeler() func(host, home string, sess *usage.Session) usage.Labels {
	byWT := map[string]ledger.Entry{}
	for _, e := range s.ledger.Entries() {
		if e.Worktree != "" {
			byWT[e.Host+"\x00"+e.Worktree] = e // later entries (a reused worktree) win
		}
	}
	coord := map[string]bool{}
	for _, a := range s.fleet.Agents() {
		if a.CWD != "" && s.isCoordinator(a.ID, a.Name) {
			coord[a.Host+"\x00"+a.CWD+"\x00"+usageKindOf(a.Agent)] = true
		}
	}
	return func(host, home string, sess *usage.Session) usage.Labels {
		cwd := sess.Cwd
		var l usage.Labels
		if e, ok := byWT[host+"\x00"+cwd]; ok && cwd != "" {
			l = usage.Labels{Project: e.RepoName, Role: "worker", Handoff: e.ID, Name: e.AgentName, Agent: e.AgentID, Src: usage.SrcLedger,
				Purpose: usagePurpose(e.Purpose, e.Branch)}
			if l.Name == "" {
				l.Name = e.Branch
			}
			if l.Project == "" {
				l.Project = path.Base(e.Repo)
			}
			if l.Purpose == "" {
				l.Purpose = "other"
			}
			return l
		}
		switch m := worktreeRE.FindStringSubmatch(cwd); {
		case m != nil:
			br := sess.Branch
			if br == "" {
				br = m[2]
			}
			l = usage.Labels{Project: m[1], Role: "worker", Name: m[2], Purpose: usagePurpose("", br)}
			if l.Purpose == "" {
				l.Purpose = "other"
			}
		case cwd != "" && (cwd == home || (home == "" && homeDirRE.MatchString(cwd))):
			l = usage.Labels{Project: "~ (home)", Role: "home", Purpose: "ad-hoc"}
		default:
			l = usage.Labels{Project: path.Base(strings.TrimRight(cwd, "/")), Role: "home", Purpose: "ad-hoc"}
			if l.Project == "" || l.Project == "." || l.Project == "/" {
				l.Project = "?"
			}
		}
		if coord[host+"\x00"+cwd+"\x00"+sess.Kind] && l.Role == "home" {
			l.Role, l.Purpose, l.Src = "coordinator", "coordination", usage.SrcLedger
		}
		if scratchRE.MatchString(l.Project) {
			l.Project = "(scratch)"
		}
		return l
	}
}

func usageKindOf(agent string) string {
	if agent == "pi" {
		return "pi"
	}
	return "claude"
}

// ---- prices ----

type usagePrices struct {
	path string
	mu   sync.Mutex
	cur  map[string]usage.Price
	rev  int
}

func openUsagePrices(p string) *usagePrices {
	u := &usagePrices{path: p, cur: map[string]usage.Price{}}
	b, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("usage prices: %v (using the defaults)", err)
		}
		return u
	}
	var d struct {
		Custom map[string]usage.Price `json:"custom"`
		Rev    int                    `json:"rev"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		log.Printf("usage prices: %s: %v (using the defaults)", p, err)
		return u
	}
	if d.Custom != nil {
		u.cur = d.Custom
	}
	u.rev = d.Rev
	return u
}

func (u *usagePrices) custom() map[string]usage.Price {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[string]usage.Price, len(u.cur))
	for k, v := range u.cur {
		out[k] = v
	}
	return out
}

func (u *usagePrices) set(c map[string]usage.Price) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	b, err := json.Marshal(map[string]any{"custom": c, "rev": u.rev + 1})
	if err != nil {
		return err
	}
	tmp := u.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, u.path); err != nil {
		return err
	}
	u.cur, u.rev = c, u.rev+1
	return nil
}

func (s *Server) priceTable() usage.PriceTable {
	return usage.Effective(s.usage.prices.custom())
}

// ---- HTTP ----

type usageHost struct {
	Name string `json:"name"`
	// Gathered is when the hub last had this host's counts (unix seconds);
	// Err says why not, when it could not.
	Gathered int64  `json:"gathered,omitempty"`
	Err      string `json:"err,omitempty"`
	Local    bool   `json:"local,omitempty"`
}

type usageResponse struct {
	*usage.Result
	AsOf       int64       `json:"as_of"`
	Hosts      []usageHost `json:"hosts"`
	Scanning   bool        `json:"scanning,omitempty"`
	ScanError  string      `json:"scan_error,omitempty"`
	PricesNote string      `json:"prices_note"`
}

func (s *Server) usageReady(w http.ResponseWriter) bool {
	if s.usage == nil || s.usage.prices == nil && s.ledger != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "usage_off", "message": "token counting is off on this instance"})
		return false
	}
	return true
}

func parseWhen(v string, now time.Time) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if v == "now" {
		return now, true
	}
	if d, ok := parseSpan(v); ok {
		return now.Add(-d), true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 1e9 {
		return time.Unix(n, 0), true
	}
	return time.Time{}, false
}

// parseSpan reads 90m, 36h, 14d, 4w.
func parseSpan(v string) (time.Duration, bool) {
	if len(v) < 2 {
		return 0, false
	}
	n, err := strconv.ParseFloat(v[:len(v)-1], 64)
	if err != nil || n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0, false
	}
	switch v[len(v)-1] {
	case 'm':
		return time.Duration(n * float64(time.Minute)), true
	case 'h':
		return time.Duration(n * float64(time.Hour)), true
	case 'd':
		return time.Duration(n * 24 * float64(time.Hour)), true
	case 'w':
		return time.Duration(n * 7 * 24 * float64(time.Hour)), true
	}
	return 0, false
}

// handleUsage answers GET /api/usage. The user, or a coordinator.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	if c.agent() && !s.callerIsCoordinator(c) {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "agent_forbidden", "message": "only the user and coordinators can read token usage"})
		return
	}
	if !s.usageReady(w) {
		return
	}
	qp := r.URL.Query()
	now := time.Now()
	to := now.Add(time.Hour) // include the running hour
	if t, ok := parseWhen(qp.Get("to"), now); ok {
		to = t
	}
	from := to.Add(-14 * 24 * time.Hour)
	if v := firstOf(qp.Get("from"), qp.Get("since")); v != "" {
		t, ok := parseWhen(v, now)
		if !ok {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_from", "message": "from: a time (2026-11-01, RFC 3339) or a span back from now (36h, 14d)"})
			return
		}
		from = t
	}
	if !from.Before(to) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_window", "message": "from must be before to"})
		return
	}
	bucket := qp.Get("bucket")
	if bucket == "" {
		bucket = "day"
		if to.Sub(from) <= 72*time.Hour {
			bucket = "hour"
		}
	}
	if bucket != "hour" && bucket != "day" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_bucket", "message": "bucket is hour or day"})
		return
	}
	if (bucket == "hour" && to.Sub(from) > 31*24*time.Hour) || to.Sub(from) > 3660*24*time.Hour {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "window_too_long", "message": "at most 31 days by the hour, 10 years by the day"})
		return
	}
	tz := 0
	if v := qp.Get("tz"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < -14*60 || n > 14*60 {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_tz", "message": "tz: minutes east of UTC"})
			return
		}
		tz = n
	}
	by := qp.Get("by")
	if by == "" {
		by = "kind"
	}
	filter := map[string]string{}
	for _, d := range usage.Dimensions {
		if v := qp.Get(d); v != "" {
			filter[d] = v
		}
	}
	limit := 10
	if v, err := strconv.Atoi(qp.Get("limit")); err == nil && v > 0 && v <= 100 {
		limit = v
	}
	res, err := s.usage.store.Run(usage.Query{From: from, To: to, By: by, Bucket: bucket, TZ: tz, Filter: filter, Limit: limit, Prices: s.priceTable()})
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_query", "message": "by is one of kind, model, host, project, role, purpose, handoff, session, token"})
		return
	}
	out := usageResponse{Result: res, AsOf: now.Unix(),
		PricesNote: "Dollars are estimates from list prices (cache writes at the 5-minute rate); a model with no price counts tokens only."}
	peers := s.usage.store.Peers()
	for _, h := range s.fleet.Hosts() {
		uh := usageHost{Name: h.Name, Local: h.Local}
		if h.Local {
			s.usage.mu.Lock()
			uh.Gathered, uh.Err = s.usage.lastScan.Unix(), s.usage.scanErr
			if s.usage.lastScan.IsZero() {
				uh.Gathered = 0
			}
			out.Scanning, out.ScanError = s.usage.scanning, s.usage.scanErr
			s.usage.mu.Unlock()
		} else if p, ok := peers[h.Name]; ok {
			uh.Gathered, uh.Err = p.OK, p.Err
		} else if h.Status != "ok" {
			uh.Err = "host is " + h.Status
		}
		out.Hosts = append(out.Hosts, uh)
	}
	writeJSON(w, out)
}

func firstOf(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// handleUsageSessions answers the hub's gather: this host's counts changed
// after a cursor. User role only (the hub calls as itself).
func (s *Server) handleUsageSessions(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "usage_off", "message": "token counting is off on this instance"})
		return
	}
	qp := r.URL.Query()
	since, _ := strconv.ParseInt(qp.Get("since"), 10, 64)
	limit, _ := strconv.Atoi(qp.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if ep := qp.Get("epoch"); ep != "" && ep != s.usage.store.Epoch() {
		since = 0 // the hub's cursor is for another store
	}
	writeJSON(w, s.usage.store.SnapshotSince(since, limit))
}

type pricesDoc struct {
	Defaults map[string]usage.Price `json:"defaults"`
	Custom   map[string]usage.Price `json:"custom"`
	Note     string                 `json:"note"`
}

func (s *Server) handleGetUsagePrices(w http.ResponseWriter, r *http.Request) {
	if !s.usageReady(w) {
		return
	}
	writeJSON(w, pricesDoc{Defaults: usage.DefaultPrices, Custom: s.usage.prices.custom(),
		Note: "Dollars per million tokens. The defaults are estimates from list prices. A custom entry replaces the default for that model name (a name also matches model names that start with it); all zeros removes a default."})
}

func (s *Server) handlePutUsagePrices(w http.ResponseWriter, r *http.Request) {
	if !s.usageReady(w) {
		return
	}
	var in struct {
		Custom map[string]usage.Price `json:"custom"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_body", "message": "send {\"custom\": {\"model\": {\"in\":..,\"out\":..,\"cw\":..,\"cr\":..}}}"})
		return
	}
	clean := map[string]usage.Price{}
	for k, v := range in.Custom {
		k = strings.TrimSpace(k)
		bad := func(f float64) bool { return f < 0 || f > 100000 || math.IsNaN(f) }
		if k == "" || len(k) > 120 || bad(v.In) || bad(v.Out) || bad(v.CW) || bad(v.CR) {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_price", "message": "a model name and four prices between 0 and 100000 dollars per million tokens"})
			return
		}
		clean[k] = v
	}
	if len(clean) > 200 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "too_many", "message": "at most 200 prices"})
		return
	}
	if err := s.usage.prices.set(clean); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "write_failed", "message": err.Error()})
		return
	}
	s.handleGetUsagePrices(w, r)
}

func (s *Server) usageRoutes() {
	// The user, or a coordinator (checked in the handler; a peer's request is
	// forwarded to the hub).
	s.mux.HandleFunc("GET /api/usage", s.authAgent(s.handleUsage))
	// Hub gathering and prices: the user alone.
	s.mux.HandleFunc("GET /api/usage/sessions", s.auth(s.handleUsageSessions))
	s.mux.HandleFunc("GET /api/usage/prices", s.auth(s.handleGetUsagePrices))
	s.mux.HandleFunc("PUT /api/usage/prices", s.auth(s.handlePutUsagePrices))
}

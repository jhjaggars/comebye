package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/provision"
)

// Adding a host, slice 1: the provisioner role and a read-only price browser.
//
// A peer started with -provision-config PATH is a provisioner: it runs the
// worker tool (tools/worker, as a subprocess) for two read-only questions and
// answers only its hub:
//
//	GET /api/provisioner/preflight                       which credentials it holds
//	GET /api/provisioner/catalog?kind&region&score&refresh   its price list
//
// The hub (-provision-host NAME, the default provisioner) relays them to the
// browser, for the user alone:
//
//	GET  /api/provision/nodes                    who can provision, with each one's preflight
//	GET  /api/provision/catalog?kind&region&via&score&small
//	POST /api/provision/refresh   {what, via, kind, region}   (CSRF)
//
// Nothing here creates a cloud resource and the hub holds no cloud
// credential: it asks, caches (5 minutes) and shows. What a peer sends is
// decoded into the types of internal/provision and bounded before the hub
// keeps it. Creating a host is a later slice.

// ProvisionConfig turns the provisioner role (on a peer) or the default
// provisioner (on a hub) on.
type ProvisionConfig struct {
	// Tool runs the worker tool on this peer; nil: this host does not provision.
	Tool *provision.Tool
	// Host is, on the hub, the default provisioner peer.
	Host string
}

const (
	provTTL        = 5 * time.Minute
	provRefreshGap = 20 * time.Second
	provMaxEntries = 64
)

type provNode struct {
	At        time.Time
	Role      bool // false: the peer answered "not a provisioner"
	Preflight *provision.Preflight
	Err       string // the last failure to ask
}

type provCat struct {
	At      time.Time
	Cat     *provision.Catalog
	Forced  time.Time
	LastErr string
}

type provState struct {
	mu    sync.Mutex
	nodes map[string]*provNode
	cats  map[string]*provCat
	now   func() time.Time
}

func newProvState() *provState {
	return &provState{nodes: map[string]*provNode{}, cats: map[string]*provCat{}, now: time.Now}
}

// ---- the peer's side ----

// peerProvGuard is the start of the peer routes: the user role (never an
// agent, a forwarded agent or a tagged node), the hub's own call (the marker no
// browser forward carries), a host that keeps no ledger (a peer), and the role
// switched on. The marker is a guard, not authentication (see peerRelGuard).
func (s *Server) peerProvGuard(w http.ResponseWriter, r *http.Request) bool {
	if !s.relGuard(w, r, false) {
		return false
	}
	if s.ledger != nil || r.Header.Get(fleet.HeaderHubCall) != "1" {
		relErr(w, http.StatusForbidden, "hub_only", "this route is for a hub asking its provisioner; use the app")
		return false
	}
	if s.cfg.Provision.Tool == nil {
		relErr(w, http.StatusNotFound, "not_provisioner", "this host is not a provisioner: start comebye with -provision-config <worker config>")
		return false
	}
	return true
}

// handlePeerPreflight: GET /api/provisioner/preflight.
func (s *Server) handlePeerPreflight(w http.ResponseWriter, r *http.Request) {
	if !s.peerProvGuard(w, r) {
		return
	}
	p, err := s.cfg.Provision.Tool.Preflight(r.Context())
	if err != nil {
		s.audit(r, ledger.AuditEvent{Action: "provision.preflight_failed", Detail: oneLine(err.Error(), 300)})
		code := http.StatusBadGateway
		if errors.Is(err, provision.ErrBusy) {
			code = http.StatusConflict
		}
		relErr(w, code, "preflight_failed", oneLine(provision.Text(err.Error(), 300), 300))
		return
	}
	writeJSON(w, p)
}

var scoreRe = regexp.MustCompile(`^[a-z][a-z0-9]*\.[a-z0-9]+(,[a-z][a-z0-9]*\.[a-z0-9]+){0,9}$`)

// handlePeerCatalog: GET /api/provisioner/catalog.
func (s *Server) handlePeerCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.peerProvGuard(w, r) {
		return
	}
	q := r.URL.Query()
	req := provision.CatalogReq{Kind: q.Get("kind"), Region: q.Get("region"), Refresh: q.Get("refresh") == "1"}
	if req.Kind == "" {
		req.Kind = "aws"
	}
	if req.Kind != "aws" {
		relErr(w, http.StatusBadRequest, "kind_unsupported", "only aws has a price list so far")
		return
	}
	if !provRegionRE.MatchString(req.Region) {
		relErr(w, http.StatusBadRequest, "bad_region", "the region is like us-east-1")
		return
	}
	if sc := q.Get("score"); sc != "" {
		if !scoreRe.MatchString(sc) {
			relErr(w, http.StatusBadRequest, "bad_score", "score takes up to 10 instance types")
			return
		}
		req.Score = strings.Split(sc, ",")
	}
	c, err := s.cfg.Provision.Tool.Catalog(r.Context(), req)
	if err != nil {
		code, class := http.StatusBadGateway, "catalog_failed"
		var ce *provision.CatalogError
		switch {
		case errors.As(err, &ce) && ce.Class == "creds_expired":
			class = "creds_expired"
		case errors.Is(err, provision.ErrBusy):
			code, class = http.StatusConflict, "busy"
		}
		if class == "catalog_failed" {
			s.audit(r, ledger.AuditEvent{Action: "provision.catalog_failed", Detail: oneLine(err.Error(), 300)})
		}
		relErr(w, code, class, oneLine(provision.Text(err.Error(), 300), 300))
		return
	}
	writeJSON(w, c)
}

var provRegionRE = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)

// ---- the hub's side ----

// provCall asks a peer one read-only question and returns its status and
// body; an error is "no answer" (offline, timeout, rejected).
func (s *Server) provCall(ctx context.Context, host, op string, q url.Values) (int, []byte, error) {
	return s.fleet.CallHost(ctx, host, op, q, nil)
}

type provNodeView struct {
	Name      string               `json:"name"`
	Default   bool                 `json:"default"`
	State     string               `json:"state"` // ok, offline, error
	Error     string               `json:"error,omitempty"`
	Preflight *provision.Preflight `json:"preflight,omitempty"`
	CheckedAt string               `json:"checked_at,omitempty"`
	AgeS      int                  `json:"age_s"`
}

// probeNode asks one peer for its preflight and caches the answer, including
// "this peer is not a provisioner" (so the other peers are not asked again for
// five minutes).
func (s *Server) probeNode(ctx context.Context, host string, force bool) *provNode {
	now := s.prov.now()
	s.prov.mu.Lock()
	n := s.prov.nodes[host]
	if n != nil && !force && now.Sub(n.At) < provTTL {
		s.prov.mu.Unlock()
		return n
	}
	s.prov.mu.Unlock()
	code, body, err := s.provCall(ctx, host, "provision-preflight", nil)
	nn := &provNode{At: now}
	switch {
	case err != nil:
		nn.Role, nn.Err = true, oneLine(provision.Text(err.Error(), 200), 200)
		if old := s.cachedNode(host); old != nil && old.Role {
			nn.Preflight = old.Preflight // keep the last good answer, marked by Err
		} else if old != nil && !old.Role {
			nn.Role = false
		}
	case code == http.StatusNotFound || (code == http.StatusForbidden && peerCode(body) == "hub_only"):
		// Not a provisioner (404), or a host that keeps a ledger, which is no
		// peer of this hub's: either way it is not one to offer.
		nn.Role = false
	case code != http.StatusOK:
		nn.Role, nn.Err = true, peerMessage(body, code)
	default:
		pf, perr := provision.ParsePreflight(body)
		if perr != nil {
			nn.Role, nn.Err = true, oneLine(provision.Text(perr.Error(), 200), 200)
		} else {
			nn.Role, nn.Preflight = true, &pf
		}
	}
	s.prov.mu.Lock()
	if len(s.prov.nodes) > provMaxEntries {
		s.prov.nodes = map[string]*provNode{}
	}
	s.prov.nodes[host] = nn
	s.prov.mu.Unlock()
	return nn
}

func (s *Server) cachedNode(host string) *provNode {
	s.prov.mu.Lock()
	defer s.prov.mu.Unlock()
	return s.prov.nodes[host]
}

func peerCode(body []byte) string {
	var e struct{ Code string }
	_ = json.Unmarshal(body, &e)
	return e.Code
}

func peerMessage(body []byte, code int) string {
	var e struct{ Code, Message string }
	_ = json.Unmarshal(body, &e)
	if e.Message == "" {
		return "answered " + http.StatusText(code)
	}
	return oneLine(provision.Text(e.Message, 200), 200)
}

// provNodes lists the provisioners: the default (always, offline or not) and
// every other reachable peer that answers a preflight.
func (s *Server) provNodes(ctx context.Context, force bool) []provNodeView {
	def := s.cfg.Provision.Host
	var hosts []string
	up := map[string]bool{}
	for _, h := range s.fleet.Hosts() {
		if h.Local {
			continue
		}
		hosts = append(hosts, h.Name)
		up[h.Name] = h.Status == fleet.StatusOK
	}
	sort.Strings(hosts)
	views := make([]provNodeView, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h string) {
			defer wg.Done()
			v := provNodeView{Name: h, Default: h == def}
			if !up[h] && h != def {
				v.State = "skip"
				views[i] = v
				return
			}
			n := s.probeNode(ctx, h, force)
			if !n.Role {
				v.State = "skip"
				if h == def { // the default is always shown, with why it cannot be used
					v.State, v.Error = "error", h+" is not a provisioner: start it with -provision-config"
				}
				views[i] = v
				return
			}
			v.State, v.Preflight, v.Error = "ok", n.Preflight, n.Err
			if n.Err != "" {
				v.State = "offline"
				if up[h] {
					v.State = "error"
				}
			}
			if !up[h] && v.State == "ok" {
				v.State = "offline"
			}
			if n.Preflight != nil {
				v.CheckedAt = n.Preflight.CheckedAt
			}
			v.AgeS = int(s.prov.now().Sub(n.At).Seconds())
			views[i] = v
		}(i, h)
	}
	wg.Wait()
	var out []provNodeView
	for _, v := range views {
		if v.State != "skip" {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Default && !out[j].Default })
	return out
}

// handleProvisionNodes: GET /api/provision/nodes.
func (s *Server) handleProvisionNodes(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, false) {
		return
	}
	s.writeNodes(w, r, false)
}

func (s *Server) writeNodes(w http.ResponseWriter, r *http.Request, force bool) {
	nodes := s.provNodes(r.Context(), force)
	writeJSON(w, map[string]any{
		"enabled": len(nodes) > 0,
		"default": s.cfg.Provision.Host,
		"nodes":   nodes,
		"csrf":    s.csrfToken(r),
		"kinds":   []string{"aws"},
	})
}

type provCatView struct {
	Via       string             `json:"via"`
	Kind      string             `json:"kind"`
	Region    string             `json:"region"`
	FetchedAt string             `json:"fetched_at"`
	AgeS      int                `json:"age_s"`
	Stale     string             `json:"stale,omitempty"` // why a cached answer is shown instead of a new one
	Hidden    int                `json:"hidden_small"`
	Small     bool               `json:"small"`
	MinVCPU   int                `json:"min_vcpu"`
	MinMemGiB float64            `json:"min_memory_gib"`
	Catalog   *provision.Catalog `json:"catalog"`
}

// catKey is the cache key: the node, kind, region and shortlist; "small" is
// a view of the cached answer, not a different question.
func catKey(via, kind, region, score string) string {
	return via + "|" + kind + "|" + region + "|" + score
}

func (s *Server) handleProvisionCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, false) {
		return
	}
	s.serveCatalog(w, r, r.URL.Query().Get("via"), r.URL.Query().Get("kind"), r.URL.Query().Get("region"), r.URL.Query().Get("score"), r.URL.Query().Get("small") == "1", false)
}

func (s *Server) serveCatalog(w http.ResponseWriter, r *http.Request, via, kind, region, score string, small, force bool) {
	if via == "" {
		via = s.cfg.Provision.Host
	}
	if kind == "" {
		kind = "aws"
	}
	switch {
	case kind != "aws":
		relErr(w, http.StatusBadRequest, "kind_unsupported", "only aws has a price list so far")
		return
	case via == "" || !fleet.ValidName(via) || !s.fleet.HasPeer(via):
		relErr(w, http.StatusNotFound, "unknown_node", "no such provisioner")
		return
	case score != "" && !scoreRe.MatchString(score):
		relErr(w, http.StatusBadRequest, "bad_score", "score takes up to 10 instance types")
		return
	case region != "" && !provRegionRE.MatchString(region):
		relErr(w, http.StatusBadRequest, "bad_region", "the region is like us-east-1")
		return
	}
	if region == "" { // the node's own default
		if n := s.probeNode(r.Context(), via, false); n.Role && n.Preflight != nil {
			region = n.Preflight.Region
		}
		if region == "" {
			relErr(w, http.StatusBadRequest, "bad_region", "choose a region")
			return
		}
	}
	key := catKey(via, kind, region, score)
	now := s.prov.now()
	s.prov.mu.Lock()
	c := s.prov.cats[key]
	s.prov.mu.Unlock()
	fresh := c != nil && c.Cat != nil && now.Sub(c.At) < provTTL
	if !fresh || force {
		q := url.Values{"kind": {kind}, "region": {region}}
		if score != "" {
			q.Set("score", score)
		}
		if force {
			q.Set("refresh", "1")
		}
		code, body, err := s.provCall(r.Context(), via, "provision-catalog", q)
		var cat *provision.Catalog
		var why string
		switch {
		case err != nil:
			why = oneLine(provision.Text(err.Error(), 200), 200)
		case code == http.StatusNotFound:
			relErr(w, http.StatusNotFound, "not_provisioner", via+" is not a provisioner")
			return
		case code != http.StatusOK:
			var e struct{ Code, Message string }
			_ = json.Unmarshal(body, &e)
			why = peerMessage(body, code)
			if e.Code == "creds_expired" {
				why = "creds_expired: " + why
			}
		default:
			cat, err = provision.ParseCatalog(body)
			if err != nil {
				why = oneLine(provision.Text(err.Error(), 200), 200)
				cat = nil
			} else if cat.Region != region {
				why, cat = "the node answered for another region", nil
			}
		}
		s.prov.mu.Lock()
		if c == nil {
			c = &provCat{}
			if len(s.prov.cats) > provMaxEntries {
				s.prov.cats = map[string]*provCat{}
			}
			s.prov.cats[key] = c
		}
		if cat != nil {
			c.At, c.Cat, c.LastErr = now, cat, ""
		} else {
			c.LastErr = why
		}
		if force {
			c.Forced = now
		}
		s.prov.mu.Unlock()
		if cat == nil && (c.Cat == nil) {
			status := http.StatusBadGateway
			code := "catalog_failed"
			if strings.HasPrefix(why, "creds_expired") {
				code = "creds_expired"
			}
			relErr(w, status, code, why)
			return
		}
	}
	s.prov.mu.Lock()
	cc := *c
	s.prov.mu.Unlock()
	view := provCatView{Via: via, Kind: kind, Region: region, FetchedAt: cc.At.UTC().Format(time.RFC3339), AgeS: int(now.Sub(cc.At).Seconds()),
		Small: small, MinVCPU: provision.MinVCPU, MinMemGiB: provision.MinMemoryGiB}
	if cc.LastErr != "" {
		view.Stale = cc.LastErr
	}
	cp := *cc.Cat
	view.Hidden = 0
	if !small {
		cp.Rows, view.Hidden = cp.HideSmall()
	}
	view.Catalog = &cp
	writeJSON(w, view)
}

type provRefreshBody struct {
	What   string `json:"what"`
	Via    string `json:"via"`
	Kind   string `json:"kind"`
	Region string `json:"region"`
	Score  string `json:"score"`
	Small  bool   `json:"small"`
}

// handleProvisionRefresh: POST /api/provision/refresh. It makes the hub (and,
// for the catalog, the node) ask again now. It reads from the cloud only; it is
// a POST with the CSRF token because it costs the node's calls.
func (s *Server) handleProvisionRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, true) {
		return
	}
	var b provRefreshBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	switch b.What {
	case "nodes":
		s.audit(r, ledger.AuditEvent{Action: "provision.recheck", OK: true, Detail: "preflights"})
		s.writeNodes(w, r, true)
	case "catalog":
		via := b.Via
		if via == "" {
			via = s.cfg.Provision.Host
		}
		kind := b.Kind
		if kind == "" {
			kind = "aws"
		}
		key := catKey(via, kind, b.Region, b.Score)
		s.prov.mu.Lock()
		c := s.prov.cats[key]
		recent := c != nil && s.prov.now().Sub(c.Forced) < provRefreshGap
		s.prov.mu.Unlock()
		if recent {
			relErr(w, http.StatusTooManyRequests, "too_soon", "prices were refreshed a moment ago: wait a few seconds")
			return
		}
		s.audit(r, ledger.AuditEvent{Action: "provision.refresh", OK: true, Host: via, Detail: kind + " " + b.Region})
		s.serveCatalog(w, r, via, kind, b.Region, b.Score, b.Small, true)
	default:
		relErr(w, http.StatusBadRequest, "bad_request", "what is nodes or catalog")
	}
}

// provisionRoutes registers the peer's and the hub's routes. Every one is the
// user role only (s.auth): TestProvisionRoutesAreUserOnly keeps it so.
func (s *Server) provisionRoutes() {
	s.mux.HandleFunc("GET /api/provisioner/preflight", s.auth(s.handlePeerPreflight))
	s.mux.HandleFunc("GET /api/provisioner/catalog", s.auth(s.handlePeerCatalog))
	s.mux.HandleFunc("GET /api/provision/nodes", s.auth(s.handleProvisionNodes))
	s.mux.HandleFunc("GET /api/provision/catalog", s.auth(s.handleProvisionCatalog))
	s.mux.HandleFunc("POST /api/provision/refresh", s.auth(s.handleProvisionRefresh))
}

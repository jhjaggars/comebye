package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/provision"
)

// Adding a host, slice 3: the hosts a provisioner built, what they have cost
// and removing one.
//
// On the provisioner (a peer), for its hub only:
//
//	GET  /api/provisioner/hosts?refresh=1        its hosts with spend so far, caps and state
//	POST /api/provisioner/host/destroy           {name, delete_data_disk, lose_sessions}
//
// On the hub, for the user alone (the CSRF token on the POST):
//
//	GET  /api/provision/hosts?refresh=1
//	POST /api/provision/hosts/{name}/destroy     {via?, confirm, delete_data_disk, lose_sessions}
//
// Spend is the provisioner's estimate (the cloud's launch and stop times times
// the price recorded at launch, plus the disks), never the cloud's bill. The
// hub keeps the last answer from each node and shows it with its age, so a
// provisioner that is asleep leaves the figures on screen, marked old.

const (
	provHostsTTL   = 5 * time.Minute
	provHostsBusy  = 4 * time.Second // while a destroy runs
	provObserveGap = 5 * time.Minute // the provisioner looks at the cloud this often
)

// ---- the provisioner's side ----

// observeHosts folds the cloud's view of the hosts into the book, one worker
// config at a time. A config whose cloud cannot be asked (the login expired)
// leaves its own hosts as they were and says why, for that config only; the
// others are read all the same. It returns the configs that were read.
func (s *Server) observeHosts(ctx context.Context) map[string]bool {
	if s.phosts == nil {
		return nil
	}
	tools := s.cfg.Provision.Tools()
	ok := map[string]bool{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, t := range tools {
		wg.Add(1)
		go func(i int, t *provision.Tool) {
			defer wg.Done()
			if s.observeTool(ctx, t, i == 0) {
				mu.Lock()
				ok[t.Name] = true
				mu.Unlock()
			}
		}(i, t)
	}
	wg.Wait()
	return ok
}

// observeTool reads one config's cloud into the book.
func (s *Server) observeTool(ctx context.Context, t *provision.Tool, first bool) bool {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ws, err := t.Status(cctx)
	s.phostMu.Lock()
	defer s.phostMu.Unlock()
	if s.phostErrs == nil {
		s.phostErrs = map[string]string{}
	}
	if err != nil {
		s.phostErrs[t.Name] = oneLine(provision.Text(err.Error(), 160), 160)
		return false
	}
	s.phosts.ObserveFor(t.Name, first, ws)
	delete(s.phostErrs, t.Name)
	s.phostAt = s.phosts.Now()
	return true
}

// phostStale is the per-config failures as one line. With one config it is the
// bare reason (as it always was); with several each is named by its config.
// The caller holds phostMu.
func (s *Server) phostStale() string {
	tools := s.cfg.Provision.Tools()
	var parts []string
	for _, t := range tools {
		if e := s.phostErrs[t.Name]; e != "" {
			if len(tools) > 1 {
				e = t.Name + ": " + e
			}
			parts = append(parts, e)
		}
	}
	return strings.Join(parts, "; ")
}

type peerHostsAnswer struct {
	provision.HostsView
	ObservedAt string `json:"observed_at,omitempty"`
	Stale      string `json:"stale,omitempty"`
}

// handlePeerHosts: GET /api/provisioner/hosts.
func (s *Server) handlePeerHosts(w http.ResponseWriter, r *http.Request) {
	if !s.peerProvGuard(w, r) {
		return
	}
	s.phostMu.Lock()
	never := s.phostAt.IsZero() && len(s.phostErrs) == 0
	s.phostMu.Unlock()
	if r.URL.Query().Get("refresh") == "1" || never { // never looked yet: adopt what the tool reports now, not in five minutes
		s.observeHosts(r.Context())
	}
	views, total := s.phosts.Views()
	s.phostMu.Lock()
	a := peerHostsAnswer{HostsView: provision.HostsView{V: 1, Hosts: views, Total: total}, Stale: s.phostStale()}
	if !s.phostAt.IsZero() {
		a.ObservedAt = s.phostAt.UTC().Format(time.RFC3339)
	}
	s.phostMu.Unlock()
	writeJSON(w, a)
}

// toolOfHost is the worker config that owns a host: the one that made it or
// reported it. os.ErrNotExist when the book does not know the host; a host
// whose config is no longer set here is an error too, never another config.
func (s *Server) toolOfHost(name string) (*provision.Tool, error) {
	h, ok := s.phosts.Get(name)
	if !ok {
		return nil, os.ErrNotExist
	}
	if t := s.cfg.Provision.toolNamed(h.Owner); t != nil {
		return t, nil
	}
	return nil, provision.ErrNoTool
}

type peerDestroyReq struct {
	Name           string `json:"name"`
	DeleteDataDisk bool   `json:"delete_data_disk"`
	LoseSessions   bool   `json:"lose_sessions"`
}

// handlePeerHostDestroy: POST /api/provisioner/host/destroy.
func (s *Server) handlePeerHostDestroy(w http.ResponseWriter, r *http.Request) {
	if !s.peerProvGuard(w, r) {
		return
	}
	var q peerDestroyReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil || !provision.ValidName(q.Name) {
		relErr(w, http.StatusBadRequest, "bad_request", "give the host's name")
		return
	}
	tool, terr := s.toolOfHost(q.Name)
	if terr != nil {
		relErr(w, http.StatusNotFound, "no_host", "this provisioner did not build "+q.Name+", so it will not destroy it")
		return
	}
	j, err := s.pjobs.destroy(tool, q.Name, destroyReq{DeleteDataDisk: q.DeleteDataDisk, LoseSessions: q.LoseSessions})
	switch {
	case errors.Is(err, os.ErrNotExist):
		relErr(w, http.StatusNotFound, "no_host", "this provisioner did not build "+q.Name+", so it will not destroy it")
		return
	case errors.Is(err, errBusy):
		relErr(w, http.StatusConflict, "busy", "this provisioner is busy with another job: wait for it to finish")
		return
	case err != nil:
		relErr(w, http.StatusConflict, "bad_state", oneLine(err.Error(), 200))
		return
	}
	s.audit(r, ledger.AuditEvent{Action: "provision.destroy", OK: true, Host: q.Name, Detail: "job " + j.ID + dataWord(q.DeleteDataDisk) + sessionsWord(q.LoseSessions)})
	v, _ := s.pjobs.view(j.ID, 0)
	writeJSONStatus(w, http.StatusAccepted, v)
}

func dataWord(del bool) string {
	if del {
		return " (data disk deleted)"
	}
	return " (data disk kept)"
}

func sessionsWord(lose bool) string {
	if lose {
		return " (sessions not saved)"
	}
	return ""
}

// RunProvision is the clock of both roles. On a provisioner, every few minutes
// it asks the cloud how its hosts are doing (so the ledger of what they cost
// keeps up) and does what the safety timers say is due. On a hub it reads what
// the provisioners recorded and raises to-dos (provwatch.go).
func (s *Server) RunProvision(ctx context.Context) {
	gap := s.provObserveGap
	if gap <= 0 {
		gap = provObserveGap
	}
	t := time.NewTicker(gap)
	defer t.Stop()
	s.wakeResume() // a hub restarted mid-wake follows its start job again
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.phosts != nil { // a provisioner: look at the cloud, then do what is due
				s.watchHosts(ctx, s.observeHosts(ctx))
			}
			s.hubProvisionStep(ctx) // a hub: raise the to-dos
		}
	}
}

// ---- the hub's side ----

type provHostsCache struct {
	At     time.Time
	View   *provision.HostsView
	Stale  string
	Err    string
	Forced time.Time
}

// hostsNodes are the nodes the hub asks about hosts: the default provisioner,
// and every node that has been asked to build one.
func (s *Server) hostsNodes() []string {
	set := map[string]bool{}
	if d := s.cfg.Provision.Host; d != "" {
		set[d] = true
	}
	s.hjobs.mu.Lock()
	for _, j := range s.hjobs.jobs {
		set[j.Via] = true
	}
	s.hjobs.mu.Unlock()
	s.prov.mu.Lock()
	for n, c := range s.prov.hosts {
		if c.View != nil {
			set[n] = true
		}
	}
	s.prov.mu.Unlock()
	var out []string
	for n := range set {
		if fleet.ValidName(n) && s.fleet.HasPeer(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// askHosts refreshes one node's hosts if its copy is old (or force).
func (s *Server) askHosts(ctx context.Context, node string, force bool) {
	now := s.prov.now()
	s.prov.mu.Lock()
	c := s.prov.hosts[node]
	if c == nil {
		c = &provHostsCache{}
		if len(s.prov.hosts) > provMaxEntries {
			s.prov.hosts = map[string]*provHostsCache{}
		}
		s.prov.hosts[node] = c
	}
	ttl := provHostsTTL
	if c.View != nil {
		for _, h := range c.View.Hosts {
			if h.State == "destroying" {
				ttl = provHostsBusy
			}
		}
	}
	fresh := c.View != nil && now.Sub(c.At) < ttl
	if fresh && !force || force && !c.Forced.IsZero() && now.Sub(c.Forced) < s.prov.hostsGap {
		s.prov.mu.Unlock()
		return
	}
	if force {
		c.Forced = now
	}
	s.prov.mu.Unlock()
	q := url.Values{}
	if force {
		q.Set("refresh", "1")
	}
	code, body, err := s.fleet.CallHost(ctx, node, "provision-hosts", q, nil)
	s.prov.mu.Lock()
	defer s.prov.mu.Unlock()
	switch {
	case err != nil:
		c.Err = oneLine(provision.Text(node+" did not answer: "+err.Error(), 200), 200)
	case code == http.StatusNotFound:
		c.Err = node + " is not a provisioner"
		c.View = nil
	case code != http.StatusOK:
		c.Err = peerMessage(body, code)
	default:
		v, perr := provision.ParseHosts(body)
		if perr != nil {
			c.Err = oneLine(provision.Text(perr.Error(), 200), 200)
			return
		}
		var extra struct {
			Stale      string `json:"stale"`
			ObservedAt string `json:"observed_at"`
		}
		_ = json.Unmarshal(body, &extra)
		c.View, c.At, c.Err, c.Stale = v, now, "", provision.Text(extra.Stale, 160)
	}
}

type hubHostView struct {
	provision.HostView
	Via  string `json:"via"`
	AgeS int    `json:"age_s"`
}

type hostsNodeView struct {
	Name  string `json:"name"`
	AgeS  int    `json:"age_s"`
	Error string `json:"error,omitempty"`
	Stale string `json:"stale,omitempty"`
}

// hostsAnswer is what the app reads: every node's hosts, newest first, and the
// totals across them.
func (s *Server) hostsAnswer(r *http.Request) map[string]any {
	now := s.prov.now()
	var hosts []hubHostView
	var nodes []hostsNodeView
	var tot provision.Total
	s.prov.mu.Lock()
	names := make([]string, 0, len(s.prov.hosts))
	for n := range s.prov.hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := s.prov.hosts[n]
		nv := hostsNodeView{Name: n, Error: c.Err, Stale: c.Stale}
		if c.View != nil {
			nv.AgeS = int(now.Sub(c.At).Seconds())
			for _, h := range c.View.Hosts {
				hosts = append(hosts, hubHostView{HostView: h, Via: n, AgeS: nv.AgeS})
			}
			tot.USD += c.View.Total.USD
			tot.USDHour += c.View.Total.USDHour
			tot.Live += c.View.Total.Live
		}
		nodes = append(nodes, nv)
	}
	s.prov.mu.Unlock()
	sort.SliceStable(hosts, func(i, j int) bool { return hosts[i].Created > hosts[j].Created })
	if hosts == nil {
		hosts = []hubHostView{}
	}
	return map[string]any{"hosts": hosts, "nodes": nodes, "total": tot, "wakes": s.wake.views(), "csrf": s.csrfToken(r)}
}

// handleProvisionHosts: GET /api/provision/hosts.
func (s *Server) handleProvisionHosts(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, false) {
		return
	}
	force := r.URL.Query().Get("refresh") == "1"
	var wg sync.WaitGroup
	for _, n := range s.hostsNodes() {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			s.askHosts(r.Context(), n, force)
		}(n)
	}
	wg.Wait()
	writeJSON(w, s.hostsAnswer(r))
}

type destroyBody struct {
	Via            string `json:"via"`
	Confirm        string `json:"confirm"`
	DeleteDataDisk bool   `json:"delete_data_disk"`
	LoseSessions   bool   `json:"lose_sessions"`
}

// handleProvisionHostDestroy: POST /api/provision/hosts/{name}/destroy. The
// name typed in the confirm box must be the host's name; the node that built
// the host does the work, saves the worker's sessions first and refuses if it
// cannot (unless the user chose to lose them).
func (s *Server) handleProvisionHostDestroy(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, true) {
		return
	}
	name := r.PathValue("name")
	var b destroyBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		relErr(w, http.StatusBadRequest, "bad_request", "invalid body")
		return
	}
	if !provision.ValidName(name) {
		relErr(w, http.StatusNotFound, "no_host", "no such host")
		return
	}
	if b.Confirm != name {
		relErr(w, http.StatusUnprocessableEntity, "confirm_mismatch", "type the host's name to confirm: nothing was changed")
		return
	}
	// The owner is the node that reports the host, found with a fresh look.
	for _, n := range s.hostsNodes() {
		if b.Via == "" || b.Via == n {
			s.askHosts(r.Context(), n, true)
		}
	}
	var owner string
	var hv *provision.HostView
	s.prov.mu.Lock()
	for n, c := range s.prov.hosts {
		if c.View == nil || (b.Via != "" && b.Via != n) {
			continue
		}
		for i := range c.View.Hosts {
			if c.View.Hosts[i].Name == name && c.View.Hosts[i].State != "destroyed" {
				if owner != "" {
					s.prov.mu.Unlock()
					relErr(w, http.StatusConflict, "ambiguous", "more than one node reports a host with this name: say which")
					return
				}
				owner, hv = n, &c.View.Hosts[i]
			}
		}
	}
	var state string
	if hv != nil {
		state = hv.State
	}
	s.prov.mu.Unlock()
	switch {
	case owner == "":
		relErr(w, http.StatusNotFound, "no_host", "no provisioner reports a host called "+name)
		return
	case state == "destroying":
		relErr(w, http.StatusConflict, "bad_state", name+" is already being destroyed")
		return
	}
	code, body, err := s.callBody(r.Context(), owner, "provision-host-destroy", map[string]any{"name": name, "delete_data_disk": b.DeleteDataDisk, "lose_sessions": b.LoseSessions})
	if err != nil {
		relErr(w, http.StatusBadGateway, "node_unreachable", oneLine(provision.Text(owner+" did not answer: "+err.Error(), 200), 200))
		return
	}
	if code != http.StatusAccepted && code != http.StatusOK {
		relayPeerError(w, code, body)
		return
	}
	pv, ok := parsePeerJob(body)
	if !ok {
		relErr(w, http.StatusBadGateway, "bad_answer", "the node's job was not understood")
		return
	}
	var idb [8]byte
	rand.Read(idb[:])
	j := &hubJob{ID: "hj_" + hex.EncodeToString(idb[:]), Key: "d" + hex.EncodeToString(idb[:]), Via: owner, PeerID: pv.ID, Spec: pv.Spec, Region: pv.Spec.Zone, Op: opDestroy,
		Created: s.hjobs.now(), By: s.by(r, "")}
	applyPeerJob(j, pv)
	s.hjobs.mu.Lock()
	s.hjobs.jobs[j.ID] = j
	s.hjobs.saveLocked()
	s.hjobs.mu.Unlock()
	s.prov.mu.Lock()
	if c := s.prov.hosts[owner]; c != nil {
		c.At = time.Time{} // look again at once: it is "destroying" now
	}
	s.prov.mu.Unlock()
	s.audit(r, ledger.AuditEvent{Action: "provision.destroy", OK: true, Host: owner, Detail: name + " " + j.ID + dataWord(b.DeleteDataDisk) + sessionsWord(b.LoseSessions)})
	s.hjobs.mu.Lock()
	v := s.hubView(j, 0, r)
	s.hjobs.mu.Unlock()
	writeJSONStatus(w, http.StatusAccepted, v)
}

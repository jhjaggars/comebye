package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/provision"
)

// The hub's side of adding a host (slice 2), all for the user alone with the
// CSRF token on every POST:
//
//	POST /api/provision/plan   {via, region, spec}        what a create would do and cost; changes nothing
//	POST /api/provision        {via, region, spec, key}   start a job (the key makes a double click return the same job)
//	GET  /api/provision/jobs/{id}?after=N                 its stages and log after a cursor
//	POST /api/provision/jobs/{id}/retry | /cleanup | /change {zone|type|spot}
//
// The hub validates the spec, re-checks the node's credentials, and forwards
// it to the provisioner, which validates it again, holds it to its own
// ceilings and runs the worker tool. The hub keeps its copy of the journal in
// its state directory and refreshes it from the node while a job is open.

const (
	hjMax       = 50
	hjPollGap   = 700 * time.Millisecond
	hjMaxItems  = 900
	hjKeyMaxAge = 24 * time.Hour
)

var hubJobIDRe = regexp.MustCompile(`^hj_[0-9a-f]{16}$`)

type hubJob struct {
	ID         string           `json:"id"`
	Key        string           `json:"key"`
	Via        string           `json:"via"`
	PeerID     string           `json:"peer_id"`
	Spec       provision.Spec   `json:"spec"`
	Region     string           `json:"region"`
	Op         string           `json:"op,omitempty"` // create (empty) or destroy
	Dry        bool             `json:"dry_run"`
	State      string           `json:"state"`
	Attempt    int              `json:"attempt"`
	Class      string           `json:"class,omitempty"`
	Zone       string           `json:"zone,omitempty"`
	Msg        string           `json:"msg,omitempty"`
	Items      []provision.Item `json:"items"`
	Next       int              `json:"next"`
	Created    time.Time        `json:"created"`
	By         string           `json:"by"`
	Superseded string           `json:"superseded,omitempty"`

	polled  time.Time
	offline string
}

type hubJobs struct {
	mu   sync.Mutex
	path string
	jobs map[string]*hubJob
	now  func() time.Time
}

func openHubJobs(stateDir string) *hubJobs {
	h := &hubJobs{jobs: map[string]*hubJob{}, now: time.Now}
	if stateDir == "" {
		return h
	}
	h.path = filepath.Join(stateDir, "provision-jobs.json")
	b, err := os.ReadFile(h.path)
	if err != nil {
		return h
	}
	var list []*hubJob
	if json.Unmarshal(b, &list) != nil {
		return h
	}
	for _, j := range list {
		if hubJobIDRe.MatchString(j.ID) && jobIDRe.MatchString(j.PeerID) && j.Spec.Validate() == nil && fleet.ValidName(j.Via) {
			h.jobs[j.ID] = j
		}
	}
	return h
}

func (h *hubJobs) saveLocked() {
	if h.path == "" {
		return
	}
	list := make([]*hubJob, 0, len(h.jobs))
	for _, j := range h.jobs {
		list = append(list, j)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Created.Before(list[b].Created) })
	if len(list) > hjMax {
		for _, j := range list[:len(list)-hjMax] {
			delete(h.jobs, j.ID)
		}
		list = list[len(list)-hjMax:]
	}
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	tmp := h.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, h.path)
	}
}

func (h *hubJobs) byKey(key string) *hubJob {
	for _, j := range h.jobs {
		if j.Key == key {
			return j
		}
	}
	return nil
}

func open(state string) bool { return state == jobRunning || state == jobCleaning }

// ---- the request bodies ----

type provPlanBody struct {
	Via    string          `json:"via"`
	Region string          `json:"region"`
	Spec   json.RawMessage `json:"spec"`
	Key    string          `json:"key"`
	Ack    bool            `json:"ack"`
}

func (s *Server) decodeProvBody(w http.ResponseWriter, r *http.Request, needKey bool) (provPlanBody, provision.Spec, bool) {
	var b provPlanBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		relErr(w, http.StatusBadRequest, "bad_request", "invalid body")
		return b, provision.Spec{}, false
	}
	if b.Via == "" {
		b.Via = s.cfg.Provision.Host
	}
	switch {
	case b.Via == "" || !fleet.ValidName(b.Via) || !s.fleet.HasPeer(b.Via):
		relErr(w, http.StatusNotFound, "unknown_node", "no such provisioner")
		return b, provision.Spec{}, false
	case b.Region != "" && !provRegionRE.MatchString(b.Region):
		relErr(w, http.StatusBadRequest, "bad_region", "the region is like us-east-1")
		return b, provision.Spec{}, false
	case needKey && !keyRe.MatchString(b.Key):
		relErr(w, http.StatusBadRequest, "bad_key", "the request needs an idempotency key of 8 to 64 letters, digits, - or _")
		return b, provision.Spec{}, false
	}
	spec, err := provision.ParseSpec(b.Spec)
	if err != nil {
		specErr(w, err)
		return b, spec, false
	}
	// A host is built in the node's own region: a request for another is
	// refused rather than priced for one region and built in a different one.
	if n := s.probeNode(r.Context(), b.Via, false); spec.Kind != provision.KindOpenShell && n.Role && n.Preflight != nil {
		if own := nodeRegion(n.Preflight, spec.Kind); own != "" {
			if b.Region != "" && b.Region != own {
				relErr(w, http.StatusUnprocessableEntity, "bad_region", b.Via+" builds only in "+own)
				return b, spec, false
			}
			b.Region = own
		}
	}
	return b, spec, true
}

// relayPeerError turns a peer's refusal into the hub's answer.
func relayPeerError(w http.ResponseWriter, code int, body []byte) {
	var e struct{ Code, Field, Message string }
	_ = json.Unmarshal(body, &e)
	cls := e.Code
	if !regexp.MustCompile(`^[a-z_]{2,32}$`).MatchString(cls) {
		cls = "peer_error"
	}
	status := code
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	out := map[string]string{"code": cls, "message": peerMessage(body, code)}
	if f := provision.Text(e.Field, 30); f != "" {
		out["field"] = f
	}
	writeJSONStatus(w, status, out)
}

func (s *Server) callBody(ctx context.Context, via, op string, v any) (int, []byte, error) {
	b, _ := json.Marshal(v)
	return s.fleet.CallHost(ctx, via, op, nil, b)
}

// credsOK reports whether the node holds what a real create of this kind needs:
// the kind's own cloud login, the tailnet client, and (a VM is reached over ssh)
// an ssh key.
func credsOK(pf *provision.Preflight, kind string) (bool, []string) {
	if pf == nil {
		return false, []string{"no preflight"}
	}
	var miss []string
	need := []struct {
		n string
		c provision.Check
	}{{"aws", pf.AWS}, {"tailnet", pf.Tailnet}, {"ssh", pf.SSH}}
	if kind == provision.KindGCP {
		need[0] = struct {
			n string
			c provision.Check
		}{"gcp", pf.GCP}
	}
	if kind == provision.KindOpenShell {
		need = []struct {
			n string
			c provision.Check
		}{{"openshell", pf.OpenShell}, {"tailnet", pf.Tailnet}}
	}
	for _, c := range need {
		if c.c.State != provision.StateOK {
			miss = append(miss, c.n+" "+c.c.State)
		}
	}
	return len(miss) == 0, miss
}

// ---- plan ----

type planAnswer struct {
	OK          bool            `json:"ok"`
	DryRun      bool            `json:"dry_run"`
	Plan        *provision.Plan `json:"plan"`
	Cost        *provision.Cost `json:"cost"`
	Warnings    []string        `json:"warnings"`
	Refused     string          `json:"refused,omitempty"`
	RefusedCode string          `json:"refused_code,omitempty"`
}

// handleProvisionPlan: POST /api/provision/plan.
func (s *Server) handleProvisionPlan(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, true) {
		return
	}
	b, spec, ok := s.decodeProvBody(w, r, false)
	if !ok {
		return
	}
	code, body, err := s.callBody(r.Context(), b.Via, "provision-plan", map[string]any{"spec": spec, "region": b.Region})
	switch {
	case err != nil:
		relErr(w, http.StatusBadGateway, "node_unreachable", oneLine(provision.Text(b.Via+" did not answer: "+err.Error(), 200), 200))
		return
	case code != http.StatusOK:
		relayPeerError(w, code, body)
		return
	}
	var a planAnswer
	if json.Unmarshal(body, &a) != nil || a.Plan == nil {
		relErr(w, http.StatusBadGateway, "bad_answer", "the node's plan was not understood")
		return
	}
	a.Plan.Text = provision.MultiLine(a.Plan.Text, 24<<10)
	for i, st := range a.Plan.Stages {
		a.Plan.Stages[i] = provision.Text(st, 20)
	}
	for i, wn := range a.Warnings {
		a.Warnings[i] = provision.Text(wn, 200)
	}
	if a.Cost != nil {
		a.Cost.Basis, a.Cost.Mode = provision.Text(a.Cost.Basis, 200), provision.Text(a.Cost.Mode, 12)
	}
	a.Refused, a.RefusedCode = provision.Text(a.Refused, 200), provision.Text(a.RefusedCode, 30)
	n := s.probeNode(r.Context(), b.Via, false)
	ready, missing := credsOK(n.Preflight, spec.Kind)
	s.audit(r, ledger.AuditEvent{Action: "provision.plan", OK: a.OK, Host: b.Via, Detail: spec.Name + " " + spec.Type})
	writeJSON(w, map[string]any{
		"ok": a.OK, "dry_run": a.DryRun, "via": b.Via, "region": b.Region, "plan": a.Plan, "cost": a.Cost, "warnings": a.Warnings,
		"refused": a.Refused, "refused_code": a.RefusedCode, "spec": spec,
		"ready": ready || a.DryRun, "missing": missing, "csrf": s.csrfToken(r),
	})
}

// ---- create ----

type hubJobView struct {
	ID          string           `json:"id"`
	Via         string           `json:"via"`
	Name        string           `json:"name"`
	Op          string           `json:"op,omitempty"`
	State       string           `json:"state"`
	Dry         bool             `json:"dry_run"`
	Attempt     int              `json:"attempt"`
	Class       string           `json:"class,omitempty"`
	Zone        string           `json:"zone,omitempty"`
	Msg         string           `json:"msg,omitempty"`
	Spec        provision.Spec   `json:"spec"`
	Items       []provision.Item `json:"items"`
	Next        int              `json:"next"`
	Stages      []string         `json:"stages"`
	Alt         *jobAlternatives `json:"alternatives,omitempty"`
	Offline     string           `json:"offline,omitempty"`
	Created     string           `json:"created"`
	Superseded  string           `json:"superseded,omitempty"`
	CanRetry    bool             `json:"can_retry"`
	CanCleanup  bool             `json:"can_cleanup"`
	CSRFRefresh string           `json:"csrf,omitempty"`
}

type jobAlternatives struct {
	Zones    []string `json:"zones,omitempty"`
	OnDemand bool     `json:"on_demand,omitempty"`
}

func (s *Server) hubView(j *hubJob, after int, r *http.Request) hubJobView {
	v := hubJobView{ID: j.ID, Via: j.Via, Name: j.Spec.Name, Op: j.Op, State: j.State, Dry: j.Dry, Attempt: j.Attempt, Class: j.Class, Zone: j.Zone, Msg: j.Msg,
		Spec: j.Spec, Items: []provision.Item{}, Next: j.Next, Stages: provision.StagesFor(j.Spec.Kind), Offline: j.offline,
		Created: j.Created.UTC().Format(time.RFC3339), Superseded: j.Superseded, CSRFRefresh: s.csrfToken(r)}
	for _, it := range j.Items {
		if it.Seq > after {
			v.Items = append(v.Items, it)
		}
	}
	failed := j.State == jobFailed || j.State == jobInterrupted
	v.CanRetry = failed && j.Superseded == "" && j.Op != opDestroy
	v.CanCleanup = failed && j.Superseded == "" && j.Op != opDestroy
	if failed && j.Class == provision.ClassSpotCapacity {
		alt := &jobAlternatives{OnDemand: j.Spec.Spot}
		s.prov.mu.Lock()
		c := s.prov.cats[catKey(j.Via, j.Spec.Kind, j.Region, "")]
		s.prov.mu.Unlock()
		if c != nil && c.Cat != nil {
			for _, row := range c.Cat.Rows {
				if row.Type != j.Spec.Type {
					continue
				}
				for z := range row.Spot {
					if z != j.Zone && z != j.Spec.Zone {
						alt.Zones = append(alt.Zones, z)
					}
				}
			}
			sort.Strings(alt.Zones)
		}
		v.Alt = alt
	}
	return v
}

// handleProvisionCreate: POST /api/provision.
func (s *Server) handleProvisionCreate(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, true) {
		return
	}
	b, spec, ok := s.decodeProvBody(w, r, true)
	if !ok {
		return
	}
	if n := s.probeNode(r.Context(), b.Via, false); n.Preflight != nil && !n.Preflight.DryRun && !b.Ack {
		relErr(w, http.StatusConflict, "ack_required", "a real create needs the review step's acknowledgement of the cost")
		return
	}
	s.hjobs.mu.Lock()
	old := s.hjobs.byKey(b.Key)
	s.hjobs.mu.Unlock()
	if old != nil {
		s.refreshJob(r.Context(), old)
		s.hjobs.mu.Lock()
		v := s.hubView(old, 0, r)
		s.hjobs.mu.Unlock()
		writeJSON(w, v)
		return
	}
	j, status, ok := s.submitJob(w, r, b.Via, b.Region, b.Key, spec)
	if !ok {
		return
	}
	s.hjobs.mu.Lock()
	v := s.hubView(j, 0, r)
	s.hjobs.mu.Unlock()
	writeJSONStatus(w, status, v)
}

// submitJob re-checks the node, asks it to start the job and records it. It
// has written the error response when ok is false.
func (s *Server) submitJob(w http.ResponseWriter, r *http.Request, via, region, key string, spec provision.Spec) (*hubJob, int, bool) {
	n := s.probeNode(r.Context(), via, true)
	if !n.Role || n.Preflight == nil {
		relErr(w, http.StatusConflict, "node_unavailable", via+" is not answering as a provisioner: "+oneLine(provision.Text(n.Err, 120), 120))
		return nil, 0, false
	}
	if !n.Preflight.DryRun {
		if good, missing := credsOK(n.Preflight, spec.Kind); !good {
			relErr(w, http.StatusConflict, "creds_missing", via+" cannot build now: "+strings.Join(missing, ", "))
			return nil, 0, false
		}
	}
	code, body, err := s.callBody(r.Context(), via, "provision-job-create", map[string]any{"spec": spec, "region": region, "key": key})
	if err != nil {
		relErr(w, http.StatusBadGateway, "node_unreachable", oneLine(provision.Text(via+" did not answer: "+err.Error(), 200), 200))
		return nil, 0, false
	}
	if code != http.StatusAccepted && code != http.StatusOK {
		relayPeerError(w, code, body)
		return nil, 0, false
	}
	pv, ok := parsePeerJob(body)
	if !ok {
		relErr(w, http.StatusBadGateway, "bad_answer", "the node's job was not understood")
		return nil, 0, false
	}
	var idb [8]byte
	rand.Read(idb[:])
	j := &hubJob{ID: "hj_" + hex.EncodeToString(idb[:]), Key: key, Via: via, PeerID: pv.ID, Spec: spec, Region: region, Dry: pv.Dry, Created: s.hjobs.now(), By: s.by(r, "")}
	applyPeerJob(j, pv)
	s.hjobs.mu.Lock()
	s.hjobs.jobs[j.ID] = j
	s.hjobs.saveLocked()
	s.hjobs.mu.Unlock()
	s.audit(r, ledger.AuditEvent{Action: "provision.request", OK: true, Host: via, Detail: spec.Name + " " + spec.Type + " " + j.ID + dryWord(j.Dry)})
	return j, code, true
}

func parsePeerJob(b []byte) (*pjobView, bool) {
	var v pjobView
	if json.Unmarshal(b, &v) != nil || !jobIDRe.MatchString(v.ID) {
		return nil, false
	}
	switch v.State {
	case jobRunning, jobOK, jobFailed, jobInterrupted, jobCleaning, jobCleaned:
	default:
		return nil, false
	}
	return &v, true
}

// applyPeerJob merges the node's view of a job into the hub's copy. The items
// the node sends are decoded into the Item type only and cleaned again.
func applyPeerJob(j *hubJob, v *pjobView) {
	j.State, j.Attempt, j.Dry = v.State, v.Attempt, v.Dry
	if v.Op == opDestroy {
		j.Op = opDestroy
	}
	j.Class, j.Zone, j.Msg = v.Class, v.Zone, provision.Text(v.Msg, 300)
	switch j.Class {
	case "", provision.ClassSpotCapacity, provision.ClassCredsExpired, provision.ClassQuota, provision.ClassBootstrap, provision.ClassNeedsInput, provision.ClassOther:
	default:
		j.Class = provision.ClassOther
	}
	for _, it := range v.Items {
		if it.Seq <= j.Next {
			continue
		}
		it.Msg, it.Stage, it.Step = provision.Text(it.Msg, 300), provision.Text(it.Stage, 20), provision.Text(it.Step, 8)
		if it.Kind != "event" && it.Kind != "log" {
			continue
		}
		switch it.State {
		case "", "running", "ok", "failed", "planned":
		default:
			continue
		}
		j.Items = append(j.Items, it)
	}
	if v.Next > j.Next {
		j.Next = v.Next
	}
	if len(j.Items) > hjMaxItems {
		j.Items = append([]provision.Item(nil), j.Items[len(j.Items)-hjMaxItems+100:]...)
	}
}

// refreshJob asks the node for what is new while the job is open.
func (s *Server) refreshJob(ctx context.Context, j *hubJob) {
	s.hjobs.mu.Lock()
	if !open(j.State) || s.hjobs.now().Sub(j.polled) < hjPollGap {
		s.hjobs.mu.Unlock()
		return
	}
	j.polled = s.hjobs.now()
	after, via, pid := j.Next, j.Via, j.PeerID
	s.hjobs.mu.Unlock()
	code, body, err := s.fleet.CallHost(ctx, via, "provision-job", map[string][]string{"id": {pid}, "after": {strconv.Itoa(after)}}, nil)
	s.hjobs.mu.Lock()
	defer s.hjobs.mu.Unlock()
	switch {
	case err != nil:
		j.offline = via + " is not answering: the job goes on there"
	case code == http.StatusNotFound:
		j.offline = ""
		j.State, j.Msg = jobInterrupted, via+" no longer has this job"
		s.hjobs.saveLocked()
	case code != http.StatusOK:
		j.offline = oneLine(peerMessage(body, code), 160)
	default:
		if pv, ok := parsePeerJob(body); ok {
			j.offline = ""
			applyPeerJob(j, pv)
			s.hjobs.saveLocked()
			if j.Op == opDestroy && !open(j.State) {
				s.prov.mu.Lock()
				if c := s.prov.hosts[via]; c != nil {
					c.At = time.Time{} // the host is gone (or still here): look again
				}
				s.prov.mu.Unlock()
			}
		}
	}
}

func (s *Server) jobFromPath(w http.ResponseWriter, r *http.Request) (*hubJob, bool) {
	id := r.PathValue("id")
	s.hjobs.mu.Lock()
	j := s.hjobs.jobs[id]
	s.hjobs.mu.Unlock()
	if !hubJobIDRe.MatchString(id) || j == nil {
		relErr(w, http.StatusNotFound, "no_job", "no such job")
		return nil, false
	}
	return j, true
}

// handleProvisionJob: GET /api/provision/jobs/{id}?after=N.
func (s *Server) handleProvisionJob(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, false) {
		return
	}
	j, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	s.refreshJob(r.Context(), j)
	s.hjobs.mu.Lock()
	v := s.hubView(j, after, r)
	s.hjobs.mu.Unlock()
	writeJSON(w, v)
}

func (s *Server) jobAct(w http.ResponseWriter, r *http.Request, act string) {
	if !s.relGuard(w, r, true) {
		return
	}
	j, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	s.hjobs.mu.Lock()
	failed := (j.State == jobFailed || j.State == jobInterrupted) && j.Op != opDestroy
	via, pid := j.Via, j.PeerID
	s.hjobs.mu.Unlock()
	if !failed {
		relErr(w, http.StatusConflict, "bad_state", "only a failed or interrupted job can be retried or cleaned up")
		return
	}
	code, body, err := s.callBody(r.Context(), via, "provision-job-"+act, map[string]string{"id": pid})
	if err != nil {
		relErr(w, http.StatusBadGateway, "node_unreachable", oneLine(provision.Text(via+" did not answer: "+err.Error(), 200), 200))
		return
	}
	if code != http.StatusOK {
		relayPeerError(w, code, body)
		return
	}
	if pv, ok := parsePeerJob(body); ok {
		s.hjobs.mu.Lock()
		applyPeerJob(j, pv)
		j.offline, j.polled = "", time.Time{}
		s.hjobs.saveLocked()
		s.hjobs.mu.Unlock()
	}
	s.audit(r, ledger.AuditEvent{Action: "provision." + act, OK: true, Host: via, Detail: j.ID})
	s.hjobs.mu.Lock()
	v := s.hubView(j, 0, r)
	s.hjobs.mu.Unlock()
	writeJSON(w, v)
}

func (s *Server) handleProvisionJobRetry(w http.ResponseWriter, r *http.Request) {
	s.jobAct(w, r, "retry")
}
func (s *Server) handleProvisionJobCleanup(w http.ResponseWriter, r *http.Request) {
	s.jobAct(w, r, "cleanup")
}

// handleProvisionJobChange: POST /api/provision/jobs/{id}/change {zone|type|spot}
// starts a new job from a failed one with exactly one of those changed.
func (s *Server) handleProvisionJobChange(w http.ResponseWriter, r *http.Request) {
	if !s.relGuard(w, r, true) {
		return
	}
	j, ok := s.jobFromPath(w, r)
	if !ok {
		return
	}
	var c struct {
		Zone *string `json:"zone"`
		Type *string `json:"type"`
		Spot *bool   `json:"spot"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		relErr(w, http.StatusBadRequest, "bad_request", "invalid body")
		return
	}
	n := 0
	for _, set := range []bool{c.Zone != nil, c.Type != nil, c.Spot != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		relErr(w, http.StatusBadRequest, "bad_request", "change exactly one of zone, type or spot")
		return
	}
	s.hjobs.mu.Lock()
	state, spec, via, region, super := j.State, j.Spec, j.Via, j.Region, j.Superseded
	dry, isDestroy := j.Dry, j.Op == opDestroy
	s.hjobs.mu.Unlock()
	if (state != jobFailed && state != jobInterrupted && state != jobCleaned) || super != "" || isDestroy {
		relErr(w, http.StatusConflict, "bad_state", "only a failed job can be changed")
		return
	}
	switch {
	case c.Zone != nil:
		if !dry && state != jobCleaned && spec.Zone != *c.Zone {
			relErr(w, http.StatusConflict, "cleanup_first", "clean up what the failed attempt made before moving to another zone")
			return
		}
		spec.Zone = *c.Zone
	case c.Type != nil:
		spec.Type = *c.Type
	default:
		spec.Spot = *c.Spot
	}
	if err := spec.Validate(); err != nil {
		specErr(w, err)
		return
	}
	var kb [10]byte
	rand.Read(kb[:])
	nj, status, ok := s.submitJob(w, r, via, region, "c"+hex.EncodeToString(kb[:]), spec)
	if !ok {
		return
	}
	s.hjobs.mu.Lock()
	j.Superseded = nj.ID
	s.hjobs.saveLocked()
	v := s.hubView(nj, 0, r)
	s.hjobs.mu.Unlock()
	writeJSONStatus(w, status, v)
}

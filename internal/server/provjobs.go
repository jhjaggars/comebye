package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/provision"
)

// Adding a host, slice 2: the provisioner's jobs (this file) and the hub's
// routes for them (provhub.go).
//
// A job is one run of `comebye-worker create --events --spec -` on the
// provisioner. Each stage event and each line of the tool's human text is an
// Item in the job's journal, a file in the state directory, so a restart of
// either side loses nothing: a job that was running when the provisioner
// stopped comes back as "interrupted" and Retry is safe (create resumes and
// never mints a second key for an enrolled node).
//
// Until the provisioner is started with -provision-create, every job runs
// `create --dry-run`: the plan as events, nothing created, no credential
// needed. One job runs at a time per provisioner.

const (
	pjMaxItems = 800
	pjKeep     = 40 // journals kept on disk
)

// Job states.
const (
	jobRunning     = "running"
	jobOK          = "ok"
	jobFailed      = "failed"
	jobInterrupted = "interrupted"
	jobCleaning    = "cleaning"
	jobCleaned     = "cleaned"
)

const (
	opDestroy = "destroy"
	opStart   = "start"
	opStop    = "stop"
)

// hostOp says the job acts on a host that exists (destroy, start, stop),
// not one being built: it is not retried, cleaned up or changed.
func hostOp(op string) bool { return op == opDestroy || op == opStart || op == opStop }

var keyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
var jobIDRe = regexp.MustCompile(`^pj_[0-9a-f]{16}$`)

// pjob is one job on the provisioner, as it is journalled.
type pjob struct {
	ID      string           `json:"id"`
	Key     string           `json:"key"`
	Spec    provision.Spec   `json:"spec"`
	Region  string           `json:"region,omitempty"`
	Dry     bool             `json:"dry_run"`
	Tool    string           `json:"tool,omitempty"` // the worker config (Tool.Name) the job runs through
	Op      string           `json:"op,omitempty"`   // "" or create: build a host; destroy: remove one; start: power a stopped one on
	Cost    *provision.Cost  `json:"cost,omitempty"`
	Destroy *destroyReq      `json:"destroy,omitempty"`
	State   string           `json:"state"`
	Attempt int              `json:"attempt"`
	Class   string           `json:"class,omitempty"`
	Zone    string           `json:"zone,omitempty"`
	Msg     string           `json:"msg,omitempty"`
	Started time.Time        `json:"started"`
	Ended   time.Time        `json:"ended,omitempty"`
	Next    int              `json:"next"`
	Items   []provision.Item `json:"items"`
}

// destroyReq is what a destroy job was asked to do.
type destroyReq struct {
	DeleteDataDisk bool   `json:"delete_data_disk"`
	LoseSessions   bool   `json:"lose_sessions"`
	StartFirst     bool   `json:"start_first,omitempty"` // a timer's destroy of a stopped host starts it to save its sessions
	Why            string `json:"why,omitempty"`         // set when a timer, not a person, asked
}

type provJobs struct {
	book *provision.HostBook // the hosts this node has built
	mu   sync.Mutex
	dir  string
	jobs map[string]*pjob
	busy string // the id of the job whose tool is running
	now  func() time.Time

	interruptedDestroy []string // destroys a restart cut short: their hosts are marked at start-up
}

func openProvJobs(stateDir string) *provJobs {
	m := &provJobs{jobs: map[string]*pjob{}, now: time.Now, book: provision.OpenHostBook("")}
	if stateDir == "" {
		return m
	}
	m.dir = filepath.Join(stateDir, "provision", "jobs")
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		m.dir = ""
		return m
	}
	ents, _ := os.ReadDir(m.dir)
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(m.dir, e.Name()))
		if err != nil {
			continue
		}
		var j pjob
		if json.Unmarshal(b, &j) != nil || !jobIDRe.MatchString(j.ID) || j.Spec.Validate() != nil {
			continue
		}
		if j.State == jobRunning || j.State == jobCleaning {
			j.State = jobInterrupted
			j.Msg = "the provisioner restarted while this was running"
			j.Ended = m.now()
			m.saveLocked(&j)
			if j.Op == opDestroy {
				m.interruptedDestroy = append(m.interruptedDestroy, j.Spec.Name)
			}
		}
		m.jobs[j.ID] = &j
	}
	return m
}

func (m *provJobs) saveLocked(j *pjob) {
	if m.dir == "" {
		return
	}
	b, err := json.Marshal(j)
	if err != nil {
		return
	}
	tmp := filepath.Join(m.dir, j.ID+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, filepath.Join(m.dir, j.ID+".json"))
	}
}

func (m *provJobs) pruneLocked() {
	if len(m.jobs) <= pjKeep {
		return
	}
	var all []*pjob
	for _, j := range m.jobs {
		all = append(all, j)
	}
	sort.Slice(all, func(a, b int) bool { return all[a].Started.Before(all[b].Started) })
	for _, j := range all[:len(all)-pjKeep] {
		if j.State == jobRunning || j.State == jobCleaning {
			continue
		}
		delete(m.jobs, j.ID)
		if m.dir != "" {
			os.Remove(filepath.Join(m.dir, j.ID+".json"))
		}
	}
}

func newJobID() string {
	var b [8]byte
	rand.Read(b[:])
	return "pj_" + hex.EncodeToString(b[:])
}

func (m *provJobs) byKey(key string) *pjob {
	for _, j := range m.jobs {
		if j.Key == key {
			return j
		}
	}
	return nil
}

var errBusy = errors.New("another job is running on this provisioner")
var errTooMany = errors.New("this provisioner has built as many hosts as it is allowed")

// start adds a job and runs it. A key already seen returns that job (existing).
func (m *provJobs) start(tool *provision.Tool, spec provision.Spec, key, region string, cost *provision.Cost) (j *pjob, existing bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.byKey(key); old != nil {
		return old, true, nil
	}
	if m.busy != "" {
		return nil, false, errBusy
	}
	if tool.Real {
		max, built := tool.MaxHosts, 0
		if max <= 0 {
			max = 3
		}
		for _, o := range m.jobs {
			if !o.Dry && o.State == jobOK && o.Op != opDestroy {
				if h, ok := m.book.Get(o.Spec.Name); ok && h.Destroyed != nil {
					continue // destroyed since: its place is free again
				}
				built++
			}
		}
		if built >= max {
			return nil, false, errTooMany
		}
	}
	j = &pjob{ID: newJobID(), Key: key, Spec: spec, Region: region, Cost: cost, Tool: tool.Name, Dry: !tool.Real, State: jobRunning, Attempt: 1, Started: m.now()}
	m.jobs[j.ID] = j
	m.pruneLocked()
	m.busy = j.ID
	m.saveLocked(j)
	go m.run(tool, j, func(ctx context.Context, emit func(provision.Item)) provision.JobResult {
		return tool.RunCreate(ctx, spec, j.Dry, emit)
	}, jobOK)
	return j, false, nil
}

// again runs a failed or interrupted job once more.
func (m *provJobs) again(tool *provision.Tool, id string) (*pjob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	switch {
	case j == nil:
		return nil, os.ErrNotExist
	case m.busy != "":
		return nil, errBusy
	case j.State != jobFailed && j.State != jobInterrupted:
		return nil, errors.New("only a failed or interrupted job can be retried")
	case j.Op == opDestroy:
		return nil, errors.New("a destroy is asked for again from the host, not retried")
	case j.Op == opStart:
		return nil, errors.New("a start is asked for again from the host (Start again), not retried")
	case j.Op == opStop:
		return nil, errors.New("a stop is asked for again from the host (Stop), not retried")
	}
	j.State, j.Class, j.Zone, j.Msg, j.Ended = jobRunning, "", "", "", time.Time{}
	j.Attempt++
	j.Dry = !tool.Real
	m.busy = j.ID
	m.saveLocked(j)
	spec := j.Spec
	go m.run(tool, j, func(ctx context.Context, emit func(provision.Item)) provision.JobResult {
		return tool.RunCreate(ctx, spec, j.Dry, emit)
	}, jobOK)
	return j, nil
}

// cleanup removes what a failed job made. In a dry run nothing was made, and
// the job says so without running the tool.
func (m *provJobs) cleanup(tool *provision.Tool, id string) (*pjob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	switch {
	case j == nil:
		return nil, os.ErrNotExist
	case m.busy != "":
		return nil, errBusy
	case j.State != jobFailed && j.State != jobInterrupted:
		return nil, errors.New("only a failed or interrupted job can be cleaned up")
	case j.Op == opDestroy:
		return nil, errors.New("a destroy has nothing to clean up")
	case j.Op == opStart || j.Op == opStop:
		return nil, errors.New("a " + j.Op + " has nothing to clean up")
	}
	if j.Dry {
		j.Next++
		j.Items = append(j.Items, provision.Item{Seq: j.Next, Attempt: j.Attempt, Kind: "log", Msg: "dry run: nothing was created, so there is nothing to clean up"})
		j.State, j.Ended = jobCleaned, m.now()
		m.saveLocked(j)
		return j, nil
	}
	j.State, j.Ended = jobCleaning, time.Time{}
	m.busy = j.ID
	m.saveLocked(j)
	name := j.Spec.Name
	go m.run(tool, j, func(ctx context.Context, emit func(provision.Item)) provision.JobResult {
		return tool.RunCleanup(ctx, name, emit)
	}, jobCleaned)
	return j, nil
}

// hold takes the job slot for a short thing the node does by itself (a stop at
// the cap); false when a job holds it.
func (m *provJobs) hold() (release func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy != "" {
		return nil, false
	}
	m.busy = "watch"
	return func() {
		m.mu.Lock()
		if m.busy == "watch" {
			m.busy = ""
		}
		m.mu.Unlock()
	}, true
}

// destroy starts the removal of a host this node built. One job at a time.
func (m *provJobs) destroy(tool *provision.Tool, name string, req destroyReq) (*pjob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.book.Get(name)
	switch {
	case !ok:
		return nil, os.ErrNotExist
	case m.busy != "":
		return nil, errBusy
	case h.Destroyed != nil:
		return nil, errors.New("this host is already destroyed")
	case h.Adopted:
		return nil, errors.New("this host was made with the worker tool, not from the app: destroy it with the tool")
	}
	j := &pjob{ID: newJobID(), Key: "d-" + newJobID(), Spec: h.Spec, Region: h.Region, Op: opDestroy, Tool: tool.Name, Destroy: &req, Dry: false, State: jobRunning, Attempt: 1, Started: m.now()}
	m.jobs[j.ID] = j
	m.pruneLocked()
	m.busy = j.ID
	m.book.BeginDestroy(name, j.ID)
	m.saveLocked(j)
	go m.run(tool, j, func(ctx context.Context, emit func(provision.Item)) provision.JobResult {
		return tool.RunDestroy(ctx, name, provision.DestroyOpts{DeleteDataDisk: req.DeleteDataDisk, LoseSessions: req.LoseSessions, StartFirst: req.StartFirst}, emit)
	}, jobOK)
	return j, nil
}

var errNeedsConfirm = errors.New("needs confirm")

// errNotStopped is a start asked of a host the cloud does not say is stopped.
type errNotStopped struct{ state string }

func (e *errNotStopped) Error() string {
	return "the cloud says this host is " + e.state + ", not stopped"
}

// startHost starts a stopped host this node knows (built from the app or
// adopted). One job at a time. A host the budget cap stopped, or one due to be
// destroyed, starts only when the caller has confirmed that.
func (m *provJobs) startHost(tool *provision.Tool, name string, confirmCap bool) (*pjob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.book.Get(name)
	switch {
	case !ok:
		return nil, os.ErrNotExist
	case h.Destroyed != nil || h.Destroying != "":
		return nil, errors.New("this host is destroyed or being destroyed")
	case m.busy != "":
		return nil, errBusy
	case h.State != "STOPPED":
		return nil, &errNotStopped{state: stateWord(h.State)}
	}
	if at, ok := h.DestroyDueAt(); (h.CapStopped != nil || ok && !m.now().Before(at)) && !confirmCap {
		return nil, errNeedsConfirm
	}
	j := &pjob{ID: newJobID(), Key: "s-" + newJobID(), Spec: h.Spec, Region: h.Region, Op: opStart, Tool: tool.Name, Dry: false, State: jobRunning, Attempt: 1, Started: m.now()}
	m.jobs[j.ID] = j
	m.pruneLocked()
	m.busy = j.ID
	m.saveLocked(j)
	go m.run(tool, j, func(ctx context.Context, emit func(provision.Item)) provision.JobResult {
		return tool.RunStart(ctx, name, emit)
	}, jobOK)
	return j, nil
}

// stopHost stops a running host this node knows (built from the app or
// adopted), disks kept. One job at a time.
func (m *provJobs) stopHost(tool *provision.Tool, name string) (*pjob, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.book.Get(name)
	switch {
	case !ok:
		return nil, os.ErrNotExist
	case h.Destroyed != nil || h.Destroying != "":
		return nil, errors.New("this host is destroyed or being destroyed")
	case m.busy != "":
		return nil, errBusy
	case h.State != "RUNNING":
		return nil, &errNotRunning{state: stateWord(h.State)}
	}
	j := &pjob{ID: newJobID(), Key: "p-" + newJobID(), Spec: h.Spec, Region: h.Region, Op: opStop, Tool: tool.Name, Dry: false, State: jobRunning, Attempt: 1, Started: m.now()}
	m.jobs[j.ID] = j
	m.pruneLocked()
	m.busy = j.ID
	m.saveLocked(j)
	go m.run(tool, j, func(ctx context.Context, emit func(provision.Item)) provision.JobResult {
		return tool.RunStopJob(ctx, name, emit)
	}, jobOK)
	return j, nil
}

// errNotRunning is a stop asked of a host the cloud does not say is running.
type errNotRunning struct{ state string }

func (e *errNotRunning) Error() string {
	return "the cloud says this host is " + e.state + ", not running"
}

func stateWord(s string) string {
	switch s {
	case "RUNNING":
		return "running"
	case "STOPPED":
		return "stopped"
	case "PENDING":
		return "starting"
	case "STOPPING":
		return "stopping"
	case "GONE":
		return "gone"
	}
	return "in an unknown state"
}

// run executes one attempt and records how it ended; okState is the state a
// clean exit leaves the job in.
func (m *provJobs) run(tool *provision.Tool, j *pjob, f func(context.Context, func(provision.Item)) provision.JobResult, okState string) {
	res := f(context.Background(), func(it provision.Item) {
		m.mu.Lock()
		defer m.mu.Unlock()
		j.Next++
		it.Seq, it.Attempt = j.Next, j.Attempt
		if !hostOp(j.Op) && !j.Dry && it.Kind == "event" && (it.Stage == "instance" || it.Stage == "sandbox") && it.State == "ok" {
			// The instance exists from here on: it costs money, so it is on the books.
			var est provision.Estimate
			m.book.Add(j.Spec.Name, j.ID, j.Spec, j.Region, j.Cost, est)
			m.book.Update(j.Spec.Name, func(h *provision.Host) {
				if h.Owner == "" {
					h.Owner = tool.Name
				}
			})
		}
		j.Items = append(j.Items, it)
		if len(j.Items) > pjMaxItems {
			j.Items = append([]provision.Item(nil), j.Items[len(j.Items)-pjMaxItems+100:]...)
		}
		m.saveLocked(j)
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	j.Ended = m.now()
	if res.OK {
		j.State = okState
	} else {
		j.State, j.Class, j.Zone, j.Msg = jobFailed, res.Class, res.Zone, res.Msg
	}
	switch {
	case j.Op == opStart:
		if res.OK { // the next look at the cloud fills in the rest
			m.book.Update(j.Spec.Name, func(h *provision.Host) { h.State, h.StoppedAt, h.CapStopped = "RUNNING", nil, nil })
		}
	case j.Op == opStop:
		if res.OK {
			m.book.MarkStopped(j.Spec.Name, false)
		}
	case j.Op == opDestroy:
		m.book.EndDestroy(j.Spec.Name, res.OK, res.OK && !j.Destroy.DeleteDataDisk, res.Msg)
	case okState == jobCleaned && res.OK:
		m.book.EndDestroy(j.Spec.Name, true, false, "") // a failed create, cleaned up with its data disk
	}
	if m.busy == j.ID {
		m.busy = ""
	}
	m.saveLocked(j)
}

// view is a job and the items after a cursor, as the hub reads it.
type pjobView struct {
	ID      string           `json:"id"`
	Op      string           `json:"op,omitempty"`
	State   string           `json:"state"`
	Dry     bool             `json:"dry_run"`
	Attempt int              `json:"attempt"`
	Class   string           `json:"class,omitempty"`
	Zone    string           `json:"zone,omitempty"`
	Msg     string           `json:"msg,omitempty"`
	Spec    provision.Spec   `json:"spec"`
	Started string           `json:"started"`
	Ended   string           `json:"ended,omitempty"`
	Next    int              `json:"next"`
	Items   []provision.Item `json:"items"`
}

func (m *provJobs) view(id string, after int) (*pjobView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[id]
	if j == nil {
		return nil, false
	}
	return m.viewLocked(j, after), true
}

func (m *provJobs) viewLocked(j *pjob, after int) *pjobView {
	v := &pjobView{ID: j.ID, Op: j.Op, State: j.State, Dry: j.Dry, Attempt: j.Attempt, Class: j.Class, Zone: j.Zone, Msg: j.Msg, Spec: j.Spec,
		Started: j.Started.UTC().Format(time.RFC3339), Next: j.Next, Items: []provision.Item{}}
	if !j.Ended.IsZero() {
		v.Ended = j.Ended.UTC().Format(time.RFC3339)
	}
	for _, it := range j.Items {
		if it.Seq > after {
			v.Items = append(v.Items, it)
		}
	}
	return v
}

// ---- the peer's routes ----

type peerPlanReq struct {
	Spec   json.RawMessage `json:"spec"`
	Region string          `json:"region"`
	Key    string          `json:"key"`
}

func decodePlanReq(w http.ResponseWriter, r *http.Request, needKey bool) (provision.Spec, peerPlanReq, bool) {
	var q peerPlanReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil {
		relErr(w, http.StatusBadRequest, "bad_request", "invalid body")
		return provision.Spec{}, q, false
	}
	spec, err := provision.ParseSpec(q.Spec)
	if err != nil {
		specErr(w, err)
		return spec, q, false
	}
	if q.Region != "" && !provRegionRE.MatchString(q.Region) {
		relErr(w, http.StatusBadRequest, "bad_region", "the region is like us-east-1")
		return spec, q, false
	}
	if needKey && !keyRe.MatchString(q.Key) {
		relErr(w, http.StatusBadRequest, "bad_key", "the request needs an idempotency key of 8 to 64 letters, digits, - or _")
		return spec, q, false
	}
	return spec, q, true
}

func specErr(w http.ResponseWriter, err error) {
	var se *provision.SpecError
	if errors.As(err, &se) {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]string{"code": "bad_spec", "field": se.Field, "message": se.Msg})
		return
	}
	relErr(w, http.StatusUnprocessableEntity, "bad_spec", oneLine(err.Error(), 200))
}

// provCeiling prices the spec from this node's own price list and holds it to
// the ceiling. A price list that cannot be had is a warning in a dry run and a
// refusal in a real one: a cost that cannot be checked is not allowed to spend.
func (s *Server) provCeiling(ctx context.Context, spec provision.Spec, region string) (cost *provision.Cost, warn string, err error) {
	tool := s.cfg.Provision.toolForKind(spec.Kind)
	if spec.Kind == provision.KindOpenShell {
		return provision.SandboxCost(), "", nil // nothing here is priced, so no ceiling applies
	}
	if spec.Kind == provision.KindKubeVirt {
		return provision.ClusterCost(), "", nil // held by the namespace quota and -provision-max-hosts instead
	}
	if region == "" {
		return nil, "the region was not given, so the cost could not be checked", s.ceilingUnknown(tool, "the region was not given")
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cat, cerr := tool.Catalog(cctx, provision.CatalogReq{Kind: spec.Kind, Region: region})
	if cerr == nil {
		cost, cerr = provision.EstimateCost(spec, cat)
	}
	if cerr != nil {
		why := oneLine(provision.Text(cerr.Error(), 160), 160)
		return nil, "the cost could not be priced: " + why, s.ceilingUnknown(tool, why)
	}
	max := tool.MaxUSDHour
	if max <= 0 {
		max = 1.0
	}
	if cost.USDHour > max {
		return cost, "", &ceilingError{max: max, got: cost.USDHour}
	}
	return cost, "", nil
}

type ceilingError struct{ max, got float64 }

func (e *ceilingError) Error() string {
	return "this host would cost about $" + strconv.FormatFloat(e.got, 'f', 3, 64) + " an hour; this provisioner allows at most $" + strconv.FormatFloat(e.max, 'f', 2, 64)
}

type unpricedError struct{ why string }

func (e *unpricedError) Error() string { return "the cost cannot be checked (" + e.why + ")" }

// ceilingUnknown is an error only where something real would be created.
func (s *Server) ceilingUnknown(tool *provision.Tool, why string) error {
	if !tool.Real {
		return nil
	}
	return &unpricedError{why: why}
}

// handlePeerPlan: POST /api/provisioner/plan.
func (s *Server) handlePeerPlan(w http.ResponseWriter, r *http.Request) {
	if !s.peerProvGuard(w, r) {
		return
	}
	spec, q, ok := decodePlanReq(w, r, false)
	if !ok {
		return
	}
	tool := s.cfg.Provision.toolForKind(spec.Kind)
	plan, err := tool.PlanFor(r.Context(), spec)
	if err != nil {
		relErr(w, http.StatusUnprocessableEntity, "plan_refused", oneLine(err.Error(), 300))
		return
	}
	cost, warn, cerr := s.provCeiling(r.Context(), spec, q.Region)
	resp := map[string]any{"ok": cerr == nil, "dry_run": !tool.Real, "plan": plan, "cost": cost}
	var warns []string
	if warn != "" {
		warns = append(warns, warn)
	}
	if cerr != nil {
		resp["refused"] = oneLine(cerr.Error(), 200)
		var ce *ceilingError
		if errors.As(cerr, &ce) {
			resp["refused_code"] = "over_ceiling"
		} else {
			resp["refused_code"] = "unpriced"
		}
	}
	resp["warnings"] = warns
	writeJSON(w, resp)
}

// handlePeerJobCreate: POST /api/provisioner/jobs.
func (s *Server) handlePeerJobCreate(w http.ResponseWriter, r *http.Request) {
	if !s.peerProvGuard(w, r) {
		return
	}
	spec, q, ok := decodePlanReq(w, r, true)
	if !ok {
		return
	}
	tool := s.cfg.Provision.toolForKind(spec.Kind)
	// An old key returns its job without pricing or running anything again.
	s.pjobs.mu.Lock()
	old := s.pjobs.byKey(q.Key)
	s.pjobs.mu.Unlock()
	var cost *provision.Cost
	if old == nil {
		var err error
		if cost, _, err = s.provCeiling(r.Context(), spec, q.Region); err != nil {
			code := "over_ceiling"
			var ue *unpricedError
			if errors.As(err, &ue) {
				code = "unpriced"
			}
			relErr(w, http.StatusUnprocessableEntity, code, oneLine(err.Error(), 200))
			return
		}
	}
	j, existing, err := s.pjobs.start(tool, spec, q.Key, q.Region, cost)
	if errors.Is(err, errBusy) {
		relErr(w, http.StatusConflict, "busy", "this provisioner is already building a host: wait for it to finish")
		return
	}
	if errors.Is(err, errTooMany) {
		relErr(w, http.StatusConflict, "too_many_hosts", "this provisioner has built as many hosts as it is allowed (-provision-max-hosts)")
		return
	}
	if err != nil {
		relErr(w, http.StatusInternalServerError, "failed", oneLine(err.Error(), 200))
		return
	}
	if !existing {
		s.audit(r, ledger.AuditEvent{Action: "provision.job", OK: true, Detail: "job " + j.ID + " " + spec.Name + " " + spec.Type + dryWord(j.Dry)})
	}
	v, _ := s.pjobs.view(j.ID, 0)
	st := http.StatusAccepted
	if existing {
		st = http.StatusOK
	}
	writeJSONStatus(w, st, v)
}

func dryWord(dry bool) string {
	if dry {
		return " (dry run)"
	}
	return ""
}

// handlePeerHostStart: POST /api/provisioner/host/start {name, confirm_cap}.
func (s *Server) handlePeerHostStart(w http.ResponseWriter, r *http.Request) {
	if !s.peerProvGuard(w, r) {
		return
	}
	var q struct {
		Name       string `json:"name"`
		ConfirmCap bool   `json:"confirm_cap"`
		Why        string `json:"why"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil || !provision.ValidName(q.Name) {
		relErr(w, http.StatusBadRequest, "bad_request", "give the host's name")
		return
	}
	// Look at the cloud first: a host that came up (or went) since the last
	// look is not started again.
	tool, terr := s.toolOfHost(q.Name)
	if terr == nil {
		s.observeTool(r.Context(), tool, tool == s.cfg.Provision.Tool)
	}
	var j *pjob
	var err error
	if terr != nil {
		err = terr
	} else {
		j, err = s.pjobs.startHost(tool, q.Name, q.ConfirmCap)
	}
	var ns *errNotStopped
	switch {
	case errors.Is(err, os.ErrNotExist):
		relErr(w, http.StatusNotFound, "no_host", "this provisioner does not know a host called "+q.Name)
		return
	case errors.Is(err, errBusy):
		relErr(w, http.StatusConflict, "busy", "this provisioner is busy with another job: wait for it to finish")
		return
	case errors.Is(err, errNeedsConfirm):
		relErr(w, http.StatusConflict, "needs_confirm", q.Name+" was stopped at its budget cap or is due to be destroyed: starting it needs a confirmation")
		return
	case errors.As(err, &ns):
		relErr(w, http.StatusConflict, "not_stopped", oneLine(err.Error(), 200))
		return
	case err != nil:
		relErr(w, http.StatusConflict, "bad_state", oneLine(err.Error(), 200))
		return
	}
	s.audit(r, ledger.AuditEvent{Action: "provision.start", OK: true, Host: q.Name, Detail: "job " + j.ID + cleanWhy(q.Why)})
	v, _ := s.pjobs.view(j.ID, 0)
	writeJSONStatus(w, http.StatusAccepted, v)
}

// handlePeerHostStop: POST /api/provisioner/host/stop {name, why}.
func (s *Server) handlePeerHostStop(w http.ResponseWriter, r *http.Request) {
	if !s.peerProvGuard(w, r) {
		return
	}
	var q struct {
		Name string `json:"name"`
		Why  string `json:"why"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil || !provision.ValidName(q.Name) {
		relErr(w, http.StatusBadRequest, "bad_request", "give the host's name")
		return
	}
	// Look at the cloud first: a host that stopped by itself since the last
	// look is not stopped again.
	tool, terr := s.toolOfHost(q.Name)
	if terr == nil {
		s.observeTool(r.Context(), tool, tool == s.cfg.Provision.Tool)
	}
	var j *pjob
	var err error
	if terr != nil {
		err = terr
	} else {
		j, err = s.pjobs.stopHost(tool, q.Name)
	}
	var nr *errNotRunning
	switch {
	case errors.Is(err, os.ErrNotExist):
		relErr(w, http.StatusNotFound, "no_host", "this provisioner does not know a host called "+q.Name)
		return
	case errors.Is(err, errBusy):
		relErr(w, http.StatusConflict, "busy", "this provisioner is busy with another job: wait for it to finish")
		return
	case errors.As(err, &nr):
		relErr(w, http.StatusConflict, "not_running", oneLine(err.Error(), 200))
		return
	case err != nil:
		relErr(w, http.StatusConflict, "bad_state", oneLine(err.Error(), 200))
		return
	}
	s.audit(r, ledger.AuditEvent{Action: "provision.stop", OK: true, Host: q.Name, Detail: "job " + j.ID + cleanWhy(q.Why)})
	v, _ := s.pjobs.view(j.ID, 0)
	writeJSONStatus(w, http.StatusAccepted, v)
}

func cleanWhy(w string) string {
	if w = oneLine(provision.Text(w, 80), 80); w != "" {
		return " (" + w + ")"
	}
	return ""
}

// handlePeerJob: GET /api/provisioner/job?id&after.
func (s *Server) handlePeerJob(w http.ResponseWriter, r *http.Request) {
	if !s.peerProvGuard(w, r) {
		return
	}
	id := r.URL.Query().Get("id")
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	if !jobIDRe.MatchString(id) {
		relErr(w, http.StatusBadRequest, "bad_id", "not a job id")
		return
	}
	v, ok := s.pjobs.view(id, after)
	if !ok {
		relErr(w, http.StatusNotFound, "no_job", "no such job here")
		return
	}
	writeJSON(w, v)
}

func (s *Server) peerJobAct(w http.ResponseWriter, r *http.Request, act string) {
	if !s.peerProvGuard(w, r) {
		return
	}
	var b struct{ ID string }
	b.ID = ""
	var raw struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&raw); err != nil || !jobIDRe.MatchString(raw.ID) {
		relErr(w, http.StatusBadRequest, "bad_request", "give the job id")
		return
	}
	tool := s.toolOfJob(raw.ID)
	var err error
	if act == "retry" {
		_, err = s.pjobs.again(tool, raw.ID)
	} else {
		_, err = s.pjobs.cleanup(tool, raw.ID)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		relErr(w, http.StatusNotFound, "no_job", "no such job here")
		return
	case errors.Is(err, errBusy):
		relErr(w, http.StatusConflict, "busy", "this provisioner is already building a host: wait for it to finish")
		return
	case err != nil:
		relErr(w, http.StatusConflict, "bad_state", oneLine(err.Error(), 200))
		return
	}
	s.audit(r, ledger.AuditEvent{Action: "provision." + act, OK: true, Detail: "job " + raw.ID})
	v, _ := s.pjobs.view(raw.ID, 0)
	writeJSON(w, v)
}

func (s *Server) handlePeerJobRetry(w http.ResponseWriter, r *http.Request) {
	s.peerJobAct(w, r, "retry")
}
func (s *Server) handlePeerJobCleanup(w http.ResponseWriter, r *http.Request) {
	s.peerJobAct(w, r, "cleanup")
}

// toolOfJob is the worker config a job ran through (the first when the job was
// recorded before there were several, or its config is no longer set).
func (s *Server) toolOfJob(id string) *provision.Tool {
	s.pjobs.mu.Lock()
	var name string
	var kind string
	if j := s.pjobs.jobs[id]; j != nil {
		name, kind = j.Tool, j.Spec.Kind
	}
	s.pjobs.mu.Unlock()
	if t := s.cfg.Provision.toolNamed(name); t != nil && name != "" {
		return t
	}
	return s.cfg.Provision.toolForKind(kind)
}

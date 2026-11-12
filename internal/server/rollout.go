package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/upgrade"
)

// The rolling rollout: one "Roll out to all" for a tagged release.
//
// A rollout is a job saved in <state-dir>/release/rollout.json, driven by a
// loop on the ledger host that reads it every few seconds. Nothing is kept
// in memory that the file and the fleet cannot rebuild: each tick takes the
// job and the hosts' own upgrade records and versions, and does the one next
// thing. So a hub restart, or a closed tab, loses nothing.
//
// Order: the canary peer first, a soak, then the other peers one at a time
// (and, in the hub wave, the hub itself, last). A host's step is
//
//	pending -> upgrading -> verifying -> (soaking) -> ok
//
// The gates are the ones that already exist: the host's own upgrade record
// says ok (its smoke check ran, and it put the old binary back if it did
// not), and then the hub sees the host back, online, on the target version.
// The first failure stops the whole rollout and leaves the rest untouched; a
// host that stays offline is skipped and listed, so "Roll out again" can
// finish it later.

// Rollout states.
const (
	roRunning   = "running"
	roPaused    = "paused"    // after the step in flight ended; Resume continues
	roStopped   = "stopped"   // a gate failed; Resume retries the failed step
	roDone      = "done"      // every host was handled (some may be skipped)
	roCancelled = "cancelled" // the user ended it; nothing was reverted
)

// Step phases.
const (
	roPending   = "pending"
	roUpgrading = "upgrading" // the host's run was started; waiting for its record
	roVerifying = "verifying" // the record is ok; waiting for the host to be back on the version
	roSoaking   = "soaking"   // canary only: watching the host for a while
	roOK        = "ok"
	roSkipped   = "skipped"
	roFailed    = "failed"
)

// Waves.
const (
	waveCanary = "canary"
	wavePeers  = "peers"
	waveHub    = "hub"
)

// The waits. Constants at first; a setting can come later.
const (
	rolloutTick         = 5 * time.Second
	rolloutSoak         = 2 * time.Minute
	rolloutBackWait     = 5 * time.Minute                     // after the record is ok: back online on the version
	rolloutOfflineGrace = 2 * time.Minute                     // an offline host is skipped after this long
	rolloutRunWait      = upgrade.StaleAfter + 30*time.Second // a run with no verdict at all
	rolloutStartRetry   = 5 * time.Minute                     // a host that cannot be told to start
	rolloutHostCallWait = 20 * time.Second
)

// RolloutStep is one host of the plan.
type RolloutStep struct {
	Host  string `json:"host"`
	Wave  string `json:"wave"`
	Phase string `json:"phase"`
	// Run is the id of the host's upgrade record this step started or adopted.
	Run  string `json:"run,omitempty"`
	From string `json:"from,omitempty"` // the version the host ran when its step began
	To   string `json:"to,omitempty"`   // the version the hub saw after it
	// At is when the step's current phase began (the clock of its timeouts).
	At      time.Time  `json:"at,omitempty"`
	Started *time.Time `json:"started,omitempty"`
	Done    *time.Time `json:"done,omitempty"`
	// OfflineSince starts the grace before an offline host is skipped;
	// TryingSince the retry window of a start that could not be sent.
	OfflineSince *time.Time `json:"offline_since,omitempty"`
	TryingSince  *time.Time `json:"trying_since,omitempty"`
	Attempt      int        `json:"attempt,omitempty"`
	// Mode "deploy": the step is the hub wave's deploy command, not an
	// upgrade of the host itself (rollout_hub.go). Dry: it was a dry run.
	Mode string `json:"mode,omitempty"`
	Dry  bool   `json:"dry,omitempty"`
	// Detail is the one line the card shows beside the phase (why it waits,
	// why it was skipped, what failed).
	Detail string `json:"detail,omitempty"`
	Output string `json:"output,omitempty"` // a failed step's output (the host's own, already cleaned)
}

// Rollout is the job.
type Rollout struct {
	ID      string `json:"id"`
	Todo    string `json:"todo"` // the release item
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	State   string `json:"state"`
	// PauseRequested: Pause was pressed while a step was in flight; the job
	// pauses when that step ends.
	PauseRequested bool          `json:"pause_requested,omitempty"`
	By             string        `json:"by"` // who confirmed it; the loop acts as them
	Canary         string        `json:"canary,omitempty"`
	Plan           []RolloutStep `json:"plan"`
	Started        time.Time     `json:"started"`
	Updated        time.Time     `json:"updated"`
	Rev            int           `json:"rev"` // counts saves: a tick that raced a control sees it changed
	Finished       *time.Time    `json:"finished,omitempty"`
	// Stop is why a stopped rollout stopped.
	Stop *RolloutStop `json:"stop,omitempty"`
	// Note is the line the band shows: what it is doing now, or why it waits.
	Note string `json:"note,omitempty"`
}

// RolloutStop says which host's gate failed and why.
type RolloutStop struct {
	Host   string    `json:"host"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// live: the job still has something to do or to decide.
func (j Rollout) live() bool {
	return j.State == roRunning || j.State == roPaused || j.State == roStopped
}

// rolloutOps is everything the engine asks of the outside world, so the
// loop can be tested with a fake fleet and a fake clock.
type rolloutOps interface {
	// Hosts is a fresh fleet answer, marked for the job's version.
	Hosts(ctx context.Context, version string) []relHost
	// StartUpgrade tells the host to upgrade to the job's version. A refusal
	// the host itself gave is a *peerFault.
	StartUpgrade(ctx context.Context, job Rollout, host string) (upgrade.Record, error)
	// StartDeploy starts the hub wave's deploy command (on the release host)
	// and returns the start time that names its record.
	StartDeploy(ctx context.Context, job Rollout) (string, error)
	// DeployStatus is the deploy record, as the release host keeps it.
	DeployStatus(ctx context.Context) (deployStatus, error)
}

// rolloutEngine drives the job file.
type rolloutEngine struct {
	mu    sync.Mutex // one tick or control at a time; the file is read and written under it
	path  string
	ops   rolloutOps
	now   func() time.Time
	audit func(ledger.AuditEvent)
	// hosts is the fleet answer of the tick in progress (guarded by mu).
	hosts []relHost
	// Timings; tests shorten them.
	soak, backWait, offlineGrace, runWait, startRetry time.Duration
	// onState is told, after the file was written, whenever the job's state
	// changed (started, paused, resumed, stopped, done, cancelled): the
	// release item's note follows it.
	onState func(Rollout)
}

func newRolloutEngine(path string, ops rolloutOps, audit func(ledger.AuditEvent)) *rolloutEngine {
	return &rolloutEngine{path: path, ops: ops, now: func() time.Time { return time.Now().UTC() }, audit: audit,
		soak: rolloutSoak, backWait: rolloutBackWait, offlineGrace: rolloutOfflineGrace, runWait: rolloutRunWait, startRetry: rolloutStartRetry}
}

var (
	errRolloutActive = errors.New("a rollout is already in progress: pause or cancel it first")
	errRolloutNone   = errors.New("there is no rollout")
	errRolloutState  = errors.New("the rollout is not in a state this applies to")
)

// ---- the file ----

func (e *rolloutEngine) load() (Rollout, bool) {
	if e.path == "" {
		return Rollout{}, false
	}
	b, err := os.ReadFile(e.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("rollout: %v", err)
		}
		return Rollout{}, false
	}
	var j Rollout
	if err := json.Unmarshal(b, &j); err != nil || j.ID == "" {
		log.Printf("rollout: %s is not a rollout: %v", e.path, err)
		return Rollout{}, false
	}
	return j, true
}

func (e *rolloutEngine) save(j *Rollout) error {
	if e.path == "" {
		return errors.New("this host has no state directory")
	}
	j.Updated = e.now()
	j.Rev++
	b, err := json.MarshalIndent(*j, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.path), 0o700); err != nil {
		return err
	}
	tmp := e.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.path)
}

// Get is the job as saved.
func (e *rolloutEngine) Get() (Rollout, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.load()
}

// ---- planning ----

// planRollout orders the hosts of a rollout. order, when given, is the
// user's own list (the confirm's); otherwise the canary comes first, then
// the other peers in fleet order. A host already on the version is left out
// of the default plan. The hub is not an ordinary peer: its wave is added
// by the caller. A reachable host that does not let the app upgrade it, or
// a name that is no host, refuses the plan.
func planRollout(hosts []relHost, order []string, canary string) ([]RolloutStep, error) {
	by := map[string]relHost{}
	var peers []string
	for _, h := range hosts {
		by[h.Name] = h
		if !h.Hub {
			peers = append(peers, h.Name)
		}
	}
	if order == nil {
		for _, n := range peers {
			if !by[n].Current {
				order = append(order, n)
			}
		}
		if canary != "" {
			for i, n := range order {
				if n == canary {
					order = append([]string{n}, append(append([]string{}, order[:i]...), order[i+1:]...)...)
					break
				}
			}
		}
	}
	seen := map[string]bool{}
	var plan []RolloutStep
	var notEnabled []string
	for _, n := range order {
		h, ok := by[n]
		switch {
		case !ok:
			return nil, fmt.Errorf("%q is not a host of this fleet", n)
		case h.Hub:
			return nil, fmt.Errorf("%s is the hub: it has its own wave, last", n)
		case seen[n]:
			return nil, fmt.Errorf("%s is listed twice", n)
		}
		seen[n] = true
		if hostUp(h) && !h.Enabled && !h.Current {
			notEnabled = append(notEnabled, n)
		}
		wave := wavePeers
		if len(plan) == 0 && len(order) > 1 { // a lone host is not a canary: there is nothing to hold back
			wave = waveCanary
		}
		plan = append(plan, RolloutStep{Host: n, Wave: wave, Phase: roPending})
	}
	if len(notEnabled) > 0 {
		return nil, fmt.Errorf("%s: the app is not allowed to upgrade %s (start comebye there with -release-upgrade)", strings.Join(notEnabled, ", "), map[bool]string{true: "it", false: "them"}[len(notEnabled) == 1])
	}
	return plan, nil
}

// hostUp: the hub sees the host and was able to ask it.
func hostUp(h relHost) bool { return h.Status == "ok" && !h.Unknown }

// ---- controls ----

// Start saves a new job. It refuses while another is live: one rollout at a
// time. The caller has checked the release (tagged, build green) and built
// the plan.
func (e *rolloutEngine) Start(todo, version, commit, by string, plan []RolloutStep) (Rollout, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur, ok := e.load(); ok && cur.live() {
		return cur, errRolloutActive
	}
	if len(plan) == 0 {
		return Rollout{}, errors.New("the plan has no hosts")
	}
	now := e.now()
	var idb [3]byte
	_, _ = rand.Read(idb[:])
	j := Rollout{ID: fmt.Sprintf("r%x", idb), Todo: todo, Version: version, Commit: commit, State: roRunning, By: by,
		Canary: plan[0].Host, Plan: plan, Started: now}
	j.Note = "starting"
	if err := e.save(&j); err != nil {
		return Rollout{}, err
	}
	names := make([]string, len(plan))
	for i, st := range plan {
		names[i] = st.Host
		if st.Wave == waveHub {
			names[i] = "|" + st.Host
		}
	}
	e.emit(j, "", "rollout.start", true, fmt.Sprintf("%s plan=[%s] canary=%s id=%s", version, strings.Join(names, " "), j.Canary, j.ID), by)
	if e.onState != nil {
		e.onState(j)
	}
	return j, nil
}

// Pause: takes effect when the step in flight ends. A job with no step in
// flight pauses at once.
func (e *rolloutEngine) Pause(by string) (Rollout, error) {
	return e.control(by, func(j *Rollout) (string, string, error) {
		if j.State != roRunning {
			return "", "", errRolloutState
		}
		if i := inFlight(*j); i >= 0 {
			j.PauseRequested = true
			j.Note = "pausing after " + j.Plan[i].Host + "…"
			return "rollout.pause", "after " + j.Plan[i].Host, nil
		}
		j.State, j.Note = roPaused, "paused"
		return "rollout.pause", "now", nil
	})
}

// Resume continues a paused job, or retries the failed step of a stopped one.
func (e *rolloutEngine) Resume(by string) (Rollout, error) {
	return e.control(by, func(j *Rollout) (string, string, error) {
		switch j.State {
		case roPaused:
			j.State, j.PauseRequested, j.Note = roRunning, false, "resuming"
			return "rollout.resume", "", nil
		case roStopped:
			host := ""
			for i := range j.Plan {
				if st := &j.Plan[i]; st.Phase == roFailed {
					host = st.Host
					*st = RolloutStep{Host: st.Host, Wave: st.Wave, Mode: st.Mode, Phase: roPending, Attempt: st.Attempt + 1}
				}
			}
			j.State, j.Stop, j.Note = roRunning, nil, "retrying "+host
			return "rollout.resume", "retry " + host, nil
		}
		return "", "", errRolloutState
	})
}

// Cancel ends the job. Nothing is reverted: every host keeps its version,
// and a run already started on a host finishes by itself.
func (e *rolloutEngine) Cancel(by string) (Rollout, error) {
	return e.control(by, func(j *Rollout) (string, string, error) {
		if !j.live() {
			return "", "", errRolloutState
		}
		now := e.now()
		j.State, j.PauseRequested, j.Finished, j.Note = roCancelled, false, &now, "cancelled"
		return "rollout.cancel", "", nil
	})
}

func (e *rolloutEngine) control(by string, fn func(*Rollout) (action, detail string, err error)) (Rollout, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.load()
	if !ok {
		return Rollout{}, errRolloutNone
	}
	action, detail, err := fn(&j)
	if err != nil {
		return j, err
	}
	if err := e.save(&j); err != nil {
		return j, err
	}
	e.emit(j, "", action, true, detail, by)
	if e.onState != nil {
		e.onState(j)
	}
	return j, nil
}

// inFlight is the index of a step that has been started and is not over.
func inFlight(j Rollout) int {
	for i, st := range j.Plan {
		switch st.Phase {
		case roUpgrading, roVerifying, roSoaking:
			return i
		}
	}
	return -1
}

// ---- the loop ----

// Run ticks until ctx ends. A job found running at start-up is a rollout
// that survived a restart of this server.
func (e *rolloutEngine) Run(ctx context.Context) {
	e.resumeAfterRestart()
	t := time.NewTicker(rolloutTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("rollout: %v", r)
					}
				}()
				e.tick(ctx)
			}()
		}
	}
}

func (e *rolloutEngine) resumeAfterRestart() {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.load()
	if !ok || j.State != roRunning {
		return
	}
	detail := "resumed after a restart of the hub"
	if i := inFlight(j); i >= 0 {
		detail += ", at " + j.Plan[i].Host + " (" + j.Plan[i].Phase + ")"
	}
	e.emit(j, "", "rollout.resume-after-restart", true, detail, "comebye")
}

// tick reads the job and does the next thing. It is safe to call twice at
// once and after a restart: it only ever acts on what the file and the
// fleet say.
func (e *rolloutEngine) tick(ctx context.Context) {
	e.mu.Lock()
	j, ok := e.load()
	e.mu.Unlock()
	if !ok || j.State != roRunning {
		return
	}
	// The fleet is asked outside the lock, so Pause and Cancel never wait
	// for a slow host; the answer is only used if the job did not change.
	hosts := e.ops.Hosts(ctx, j.Version)
	e.mu.Lock()
	defer e.mu.Unlock()
	cur, ok := e.load()
	if !ok || cur.State != roRunning || cur.ID != j.ID || cur.Rev != j.Rev {
		return
	}
	j = cur
	e.hosts = hosts
	before := j
	for n := 0; n <= 2*len(j.Plan)+2; n++ {
		if !e.advance(ctx, &j, hosts) || j.State != roRunning {
			break
		}
	}
	if sameJob(before, j) {
		return
	}
	if err := e.save(&j); err != nil {
		log.Printf("rollout: %v", err)
		return
	}
	if j.State != before.State && e.onState != nil {
		e.onState(j)
	}
}

func sameJob(a, b Rollout) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// current is the step the job is on: the first not yet ok or skipped.
func current(j Rollout) int {
	for i, st := range j.Plan {
		if st.Phase != roOK && st.Phase != roSkipped {
			return i
		}
	}
	return -1
}

func findHost(hosts []relHost, name string) (relHost, bool) {
	for _, h := range hosts {
		if h.Name == name {
			return h, true
		}
	}
	return relHost{}, false
}

// advance does one thing to the job's current step. It reports true when
// the job moved on without calling out, so the next step can be looked at
// with the same fleet answer; after a call out it reports false (the answer
// is then older than the call).
func (e *rolloutEngine) advance(ctx context.Context, j *Rollout, hosts []relHost) bool {
	i := current(*j)
	if i < 0 {
		now := e.now()
		j.State, j.Finished, j.Note = roDone, &now, ""
		var skipped, ok []string
		for _, st := range j.Plan {
			if st.Phase == roSkipped {
				skipped = append(skipped, st.Host)
			} else {
				ok = append(ok, st.Host)
			}
		}
		detail := fmt.Sprintf("ok %d of %d hosts", len(ok), len(j.Plan))
		if len(skipped) > 0 {
			detail += fmt.Sprintf(", %d skipped (%s)", len(skipped), strings.Join(skipped, ", "))
		}
		e.emit(*j, "", "rollout.finish", true, detail+" elapsed="+now.Sub(j.Started).Round(time.Second).String(), "comebye")
		return false
	}
	st := &j.Plan[i]
	now := e.now()
	if st.Phase == roFailed {
		j.State = roStopped // a failed step with a running job: only a hand-edited file
		return false
	}
	h, found := findHost(hosts, st.Host)

	switch st.Phase {
	case roPending:
		if j.PauseRequested {
			j.State, j.PauseRequested, j.Note = roPaused, false, "paused"
			return false
		}
		if st.Mode == stepModeDeploy {
			return e.beginHub(ctx, j, st, h, found, now)
		}
		return e.begin(ctx, j, st, h, found, now)
	case roUpgrading:
		if st.Mode == stepModeDeploy {
			return e.watchHub(ctx, j, st, h, found, now)
		}
		return e.watchRun(j, st, h, found, now)
	case roVerifying:
		return e.verify(j, st, h, found, now)
	case roSoaking:
		return e.soaking(j, st, h, found, now)
	}
	return false
}

// begin: a step whose turn has come.
func (e *rolloutEngine) begin(ctx context.Context, j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	switch {
	case !found:
		e.skip(j, st, now, "no longer in the fleet")
		return true
	case h.Current:
		st.Detail = "already on " + h.Version
		e.finishStep(j, st, h, now, "already on the version")
		return true
	case !hostUp(h):
		if st.OfflineSince == nil {
			since := now
			if h.Since != nil && hostDown(h) {
				since = *h.Since
			}
			st.OfflineSince = &since
		}
		if now.Sub(*st.OfflineSince) >= e.offlineGrace {
			e.skip(j, st, now, "offline for "+now.Sub(*st.OfflineSince).Round(time.Second).String())
			return true
		}
		st.Detail = "offline: waiting for it to come back"
		j.Note = st.Host + " is offline: waiting for it to come back"
		return false
	case !h.Enabled:
		e.fail(j, st, now, "this host does not let the app upgrade it (-release-upgrade)", "")
		return false
	}
	st.OfflineSince = nil
	// Adopt a run that is already going for this version: the hub died after
	// sending the start, or a person started it by hand.
	if r := h.Record; r != nil && r.Version == j.Version && !r.Rollback && r.Live(now) {
		e.started(j, st, h, *r, now, "adopted the run already going")
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, rolloutHostCallWait)
	rec, err := e.ops.StartUpgrade(cctx, *j, st.Host)
	cancel()
	if err != nil {
		var pf *peerFault
		refused := errors.As(err, &pf) && pf.code != "upgrade_running" && pf.code != ""
		if st.TryingSince == nil {
			t := now
			st.TryingSince = &t
		}
		if refused || now.Sub(*st.TryingSince) >= e.startRetry {
			e.fail(j, st, now, "could not start the upgrade: "+oneLine(err.Error(), 300), "")
			return false
		}
		st.Detail = "could not start yet: " + oneLine(err.Error(), 160)
		j.Note = st.Host + ": " + st.Detail
		return false
	}
	e.started(j, st, h, rec, now, "")
	return false
}

func hostDown(h relHost) bool { return h.Status != "ok" }

func (e *rolloutEngine) started(j *Rollout, st *RolloutStep, h relHost, rec upgrade.Record, now time.Time, how string) {
	t := now
	st.Phase, st.Run, st.From, st.At, st.Started = roUpgrading, rec.ID, h.Version, now, &t
	st.TryingSince, st.Detail = nil, how
	j.Note = "upgrading " + st.Host
	detail := fmt.Sprintf("start from=%s run=%s", h.Version, rec.ID)
	if how != "" {
		detail += " (" + how + ")"
	}
	e.emit(*j, st.Host, "rollout.step", true, detail, "comebye")
}

// watchRun: the host's run, through its own record.
func (e *rolloutEngine) watchRun(j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	j.Note = "upgrading " + st.Host
	var rec *upgrade.Record
	if found {
		rec = h.Record
	}
	if rec != nil && rec.ID != st.Run {
		// The run was replaced by another: judge by the host, not by a record
		// that is not ours.
		if h.Current && hostUp(h) && (rec.State == upgrade.RecOK || rec.State == upgrade.RecDryRun) {
			st.Run = rec.ID
		} else {
			e.fail(j, st, now, "another upgrade run replaced this one on the host ("+rec.ID+", "+rec.State+")", rec.Output)
			return false
		}
	}
	if rec != nil && rec.ID == st.Run {
		switch rec.State {
		case upgrade.RecOK, upgrade.RecDryRun:
			st.Phase, st.At, st.Detail = roVerifying, now, ""
			j.Note = "verifying " + st.Host
			return true
		case upgrade.RecFailed, upgrade.RecRolledBack:
			why := rec.State
			if rec.Error != "" {
				why += ": " + oneLine(rec.Error, 300)
			}
			e.fail(j, st, now, why, rec.Output)
			return false
		}
	}
	if now.Sub(st.At) >= e.runWait {
		e.fail(j, st, now, "no verdict from the upgrade run after "+e.runWait.Round(time.Second).String(), "")
		return false
	}
	if rec == nil {
		st.Detail = "waiting for the host's upgrade record"
	}
	return false
}

// verify: the record is ok; the hub must see the host back on the version.
func (e *rolloutEngine) verify(j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	j.Note = "verifying " + st.Host
	dry := st.Dry || (found && h.DryRun && h.Record != nil && h.Record.State == upgrade.RecDryRun)
	if found && hostUp(h) && (h.Current || dry) && (st.Mode != stepModeDeploy || e.peersBack(*j, st)) {
		st.To = h.Version
		if st.Wave == waveCanary && e.soak > 0 {
			st.Phase, st.At, st.Detail = roSoaking, now, ""
			j.Note = "soaking " + st.Host
			e.emit(*j, st.Host, "rollout.gate", true, fmt.Sprintf("upgrade=ok back=ok version=%s took=%s", h.Version, took(*st, now)), "comebye")
			return false
		}
		e.finishStep(j, st, h, now, "")
		return true
	}
	if now.Sub(st.At) >= e.backWait {
		why := fmt.Sprintf("not back online on %s within %s", j.Version, e.backWait.Round(time.Second))
		switch {
		case !found:
			why += " (not in the fleet any more)"
		case !hostUp(h):
			why += " (" + h.Status + ")"
		default:
			why += " (it runs " + orUnknown(h.Version) + ")"
		}
		e.fail(j, st, now, why, "")
		return false
	}
	st.Detail = "waiting for it to be back on " + j.Version
	return false
}

func orUnknown(v string) string {
	if v == "" {
		return "an unknown version"
	}
	return v
}

func took(st RolloutStep, now time.Time) string {
	if st.Started == nil {
		return "?"
	}
	return now.Sub(*st.Started).Round(time.Second).String()
}

// soaking: the canary is watched for a while; any change for the worse
// stops the rollout before another host is touched.
func (e *rolloutEngine) soaking(j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	j.Note = "soaking " + st.Host
	switch {
	case !found || !hostUp(h):
		e.fail(j, st, now, "went offline while soaking", "")
		return false
	case !h.Current && !(h.DryRun && h.Record != nil && h.Record.State == upgrade.RecDryRun):
		e.fail(j, st, now, "no longer on "+j.Version+" while soaking (it runs "+orUnknown(h.Version)+")", "")
		return false
	case h.Record != nil && h.Record.ID != st.Run && (h.Record.State == upgrade.RecFailed || h.Record.State == upgrade.RecRolledBack):
		e.fail(j, st, now, "a new upgrade run failed while soaking: "+oneLine(h.Record.Error, 200), h.Record.Output)
		return false
	}
	if left := e.soak - now.Sub(st.At); left > 0 {
		st.Detail = "soak: " + left.Round(time.Second).String() + " left"
		return false
	}
	e.emit(*j, st.Host, "rollout.gate", true, fmt.Sprintf("soak=ok %s", e.soak), "comebye")
	e.finishStep(j, st, h, now, "")
	return true
}

func (e *rolloutEngine) finishStep(j *Rollout, st *RolloutStep, h relHost, now time.Time, note string) {
	t := now
	st.Phase, st.Done, st.Detail = roOK, &t, note
	if st.To == "" {
		st.To = h.Version
	}
	e.emit(*j, st.Host, "rollout.gate", true, fmt.Sprintf("done version=%s took=%s %s", st.To, took(*st, now), note), "comebye")
	if j.PauseRequested {
		if current(*j) >= 0 {
			j.State, j.PauseRequested, j.Note = roPaused, false, "paused after "+st.Host
			e.emit(*j, "", "rollout.pause", true, "now, after "+st.Host, "comebye")
		}
	}
}

func (e *rolloutEngine) skip(j *Rollout, st *RolloutStep, now time.Time, why string) {
	t := now
	st.Phase, st.Done, st.Detail = roSkipped, &t, why
	e.emit(*j, st.Host, "rollout.skip", true, why, "comebye")
}

func (e *rolloutEngine) fail(j *Rollout, st *RolloutStep, now time.Time, why, output string) {
	st.Phase, st.Detail, st.Output = roFailed, oneLine(why, 400), output
	j.State, j.Note = roStopped, "stopped at "+st.Host
	j.Stop = &RolloutStop{Host: st.Host, Reason: oneLine(why, 400), At: now}
	e.emit(*j, st.Host, "rollout.stop", false, why, "comebye")
}

// emit writes one audit entry carrying the rollout id and the release item.
func (e *rolloutEngine) emit(j Rollout, host, action string, ok bool, detail, by string) {
	if e.audit == nil {
		return
	}
	d := detail
	if !strings.Contains(d, "id="+j.ID) {
		d = strings.TrimSpace(d + " id=" + j.ID)
	}
	e.audit(ledger.AuditEvent{By: by, Action: action, Todo: j.Todo, Host: host, OK: ok, Detail: d})
}

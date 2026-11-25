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
	"slices"
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
	roWaking    = "waking"    // the host was stopped (idle off); it is being started, and is not yet counted offline or soaking
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
	rolloutSoak         = rolloutSoakDefault                  // the canary's (rollout_canary.go); the setting can change it
	rolloutBackWait     = 5 * time.Minute                     // after the record is ok: back online on the version
	rolloutOfflineGrace = 2 * time.Minute                     // an offline host is skipped after this long
	rolloutRunWait      = upgrade.StaleAfter + 30*time.Second // a run with no verdict at all
	rolloutStartRetry   = 5 * time.Minute                     // a host that cannot be told to start
	rolloutHostCallWait = 20 * time.Second
	rolloutWakeWait     = 10 * time.Minute // a stopped worker: started and joined the hub (the start job 7 min, the join 3)
	rolloutCatchUpEvery = time.Minute      // a done job looks for its offline hosts this often
	rolloutCatchUpFor   = 7 * 24 * time.Hour
	// rolloutBusyWait: a host busy with what a restart would cut short (a
	// sign-in, a tagging, a deploy: upgradeStatus.Busy) is waited for this
	// long, then upgraded anyway, with a line in the step's detail.
	rolloutBusyWait = 15 * time.Minute
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
	// BusySince is when the step first found its host busy (relHost.Busy);
	// saved, so a hub restart does not start the wait again.
	BusySince *time.Time `json:"busy_since,omitempty"`
	// BusyNote is how long the step waited for its busy host, or that it
	// went ahead at the limit; the finished step keeps it in its detail.
	BusyNote string `json:"busy_note,omitempty"`
	// Woke: the host was stopped and this rollout started it (issue #66).
	Woke bool `json:"woke,omitempty"`
	// Mode "deploy": the step is the hub wave's deploy command, not an
	// upgrade of the host itself (rollout_hub.go). Dry: it was a dry run.
	Mode string `json:"mode,omitempty"`
	Dry  bool   `json:"dry,omitempty"`
	// CatchUp: the step was skipped because the host was offline (or a
	// stopped worker could not be started); the loop upgrades it when it is
	// back, while the job is done (catchUp).
	CatchUp bool `json:"catch_up,omitempty"`
	// Reask: the hub step stopped because the release host could not be
	// asked to start the deploy, or said it has no command; the loop asks it
	// again and retries the step once it answers with one (retryHub).
	Reask bool `json:"reask,omitempty"`
	// SoakUntil is when the canary's soak ends: set as it starts, so a hub
	// restart resumes the same soak, neither from zero nor skipped.
	// ProcStart is the host's process start time the soak began with: a
	// later one is a restart (rollout_canary.go).
	SoakUntil *time.Time `json:"soak_until,omitempty"`
	ProcStart *time.Time `json:"proc_start,omitempty"`
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
	PauseRequested bool   `json:"pause_requested,omitempty"`
	By             string `json:"by"` // who confirmed it; the loop acts as them
	// Auto: the hub started it by itself once the approved release's build
	// was green (rollout_auto.go). An automatic rollout never wakes a
	// stopped worker: it is upgraded when it next starts (catchUp).
	Auto   bool   `json:"auto,omitempty"`
	Canary string `json:"canary,omitempty"`
	// SoakSkipped: the user skipped the canary's soak (a tapped rollout,
	// for an urgent fix), at the confirm or while it soaked.
	SoakSkipped bool          `json:"soak_skipped,omitempty"`
	Plan        []RolloutStep `json:"plan"`
	Started     time.Time     `json:"started"`
	Updated     time.Time     `json:"updated"`
	Rev         int           `json:"rev"` // counts saves: a tick that raced a control sees it changed
	Finished    *time.Time    `json:"finished,omitempty"`
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
	// Wake is asked once for a host that is not up: when the provisioner knows
	// it as a stopped worker, it starts it (or joins the start already going)
	// and says wakeStarting. wakeNone means it is not a stopped provisioned
	// worker (the old offline grace applies); wakeRefused carries why a stopped
	// worker was not started (rate limit, budget cap, provisioner offline).
	Wake(ctx context.Context, job Rollout, host string) wakeVerdict
	// WakeStatus follows the wake of a host.
	WakeStatus(ctx context.Context, host string) wakeVerdict
}

// What the hub says about waking a host for a rollout.
const (
	wakeNone    = "none"
	wakeRefused = "refused"
	// wakeStarting, wakeUp and wakeFailed are the wake book's own words (wake.go).
)

type wakeVerdict struct {
	Kind   string
	Step   string // the tool's last line, or what the hub waits for
	Reason string // why it was refused or failed
	Code   string // for a refusal: a short code the app reads (agent_forbidden, needs_confirm, wake_limit, ...)
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
	// Timings; tests shorten them. soakFor, when set and above 0, is the
	// soak a canary gets when it starts soaking (the hub's setting); else soak.
	soakFor                                                     func() time.Duration
	soak, backWait, offlineGrace, runWait, startRetry, wakeWait time.Duration
	busyWait                                                    time.Duration
	// catchUpEvery is how often a done job with hosts skipped while offline
	// asks the fleet whether they are back; lastCatchUp is when it last did.
	catchUpEvery time.Duration
	lastCatchUp  time.Time
	// tickEvery is how often Run ticks (rolloutTick; -test-mode makes it shorter).
	tickEvery time.Duration
	// onState is told, after the file was written, whenever the job's state
	// changed (started, paused, resumed, stopped, done, cancelled): the
	// release item's note follows it.
	onState func(Rollout)
	// onHubTrouble is told when the hub's step stops or is skipped (also a
	// step planned skipped): the hub files its To do (rollout_alarm.go).
	onHubTrouble func(Rollout, RolloutStep)
	// onSoakFail is told when the canary's soak failed: the hub files its
	// To do (alarmSoak).
	onSoakFail func(Rollout, RolloutStep)
	// lastReask is when retryHub last asked the release host.
	lastReask time.Time
	// catchUpAlways, when it says true (the automatic rollout is on), lifts
	// the week's limit on catchUp: a stopped host is upgraded whenever it
	// next starts (issue #141, slice 4).
	catchUpAlways func() bool
}

func newRolloutEngine(path string, ops rolloutOps, audit func(ledger.AuditEvent)) *rolloutEngine {
	return &rolloutEngine{path: path, ops: ops, now: func() time.Time { return time.Now().UTC() }, audit: audit,
		catchUpEvery: rolloutCatchUpEvery,
		soak:         rolloutSoak, backWait: rolloutBackWait, offlineGrace: rolloutOfflineGrace, runWait: rolloutRunWait, startRetry: rolloutStartRetry, wakeWait: rolloutWakeWait,
		busyWait: rolloutBusyWait}
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
// user's own list (the confirm's); otherwise the peers in fleet order.
// There is no canary or soak unless canary names the first peer (the
// -release-canary opt-in); then it goes first, alone, and soaks. A host already on the version is left out
// of the default plan. The hub is not an ordinary peer: its wave is added
// by the caller (rolloutPlan), and so are the peers upgraded with the hub
// (with_hub). A reachable host that does not let the app upgrade it is in
// the plan already skipped, with the reason; a name that is no host refuses
// the plan.
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
		if h.WithHub {
			continue // the hub's wave upgrades it (rolloutPlan)
		}
		wave := wavePeers
		if len(plan) == 0 && len(order) > 1 && n == canary { // opt-in only (-release-canary); a lone host is not a canary
			wave = waveCanary
		}
		st := RolloutStep{Host: n, Wave: wave, Phase: roPending}
		if hostUp(h) && !h.Enabled && !h.Current {
			// It does not stop the others: it is listed, skipped, with the reason.
			st.Wave, st.Phase, st.Detail = wavePeers, roSkipped, "the app is not allowed to upgrade it (start comebye there with -release-upgrade)"
		}
		plan = append(plan, st)
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
	return e.start(todo, version, commit, by, plan, false, false)
}

// StartSkipSoak is Start with the canary's soak skipped (the confirm's
// skip_soak, for an urgent fix): the canary still goes first, and does not
// soak.
func (e *rolloutEngine) StartSkipSoak(todo, version, commit, by string, plan []RolloutStep) (Rollout, error) {
	return e.start(todo, version, commit, by, noSoak(plan), false, true)
}

// StartAuto is Start for the hub's own automatic rollout. It refuses, too,
// when the job on file is already this release item's, whatever its state:
// one release is rolled out automatically once, even across a restart.
func (e *rolloutEngine) StartAuto(todo, version, commit, by string, plan []RolloutStep) (Rollout, error) {
	return e.start(todo, version, commit, by, plan, true, false)
}

var errRolloutSame = errors.New("this release already has a rollout")

func (e *rolloutEngine) start(todo, version, commit, by string, plan []RolloutStep, auto, skipSoak bool) (Rollout, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cur, ok := e.load(); ok && cur.live() {
		return cur, errRolloutActive
	} else if ok && auto && cur.Todo == todo {
		return cur, errRolloutSame
	}
	if !actionable(plan) {
		return Rollout{}, errors.New("the plan has no host the app can upgrade")
	}
	now := e.now()
	var idb [3]byte
	_, _ = rand.Read(idb[:])
	j := Rollout{ID: fmt.Sprintf("r%x", idb), Todo: todo, Version: version, Commit: commit, State: roRunning, By: by,
		Auto: auto, SoakSkipped: skipSoak, Plan: plan, Started: now}
	for i := range plan {
		if plan[i].Phase == roSkipped && plan[i].Done == nil {
			plan[i].Done = &now
		}
	}
	if plan[0].Wave == waveCanary {
		j.Canary = plan[0].Host
	}
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
	how := ""
	if auto {
		how = " auto=true"
	}
	if skipSoak {
		how += " soak=skipped"
	}
	for _, st := range plan {
		if isHubStep(st) && st.Phase == roSkipped && e.onHubTrouble != nil {
			e.onHubTrouble(j, st)
		}
	}
	e.emit(j, "", "rollout.start", true, fmt.Sprintf("%s plan=[%s] canary=%q%s id=%s", version, strings.Join(names, " "), j.Canary, how, j.ID), by)
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
	every := rolloutTick
	if e.tickEvery > 0 {
		every = e.tickEvery
	}
	t := time.NewTicker(every)
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
	if ok && j.State == roDone {
		e.catchUp(ctx, j)
		return
	}
	if ok && j.State == roStopped {
		e.retryHub(ctx, j)
		return
	}
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
	// before is a deep copy: advance changes the steps in place, and a
	// shallow copy shares them, so a step-only change (the soak's time
	// left) would look like no change and never be saved.
	before := j
	before.Plan = slices.Clone(j.Plan)
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

// catchUp: a done job whose offline hosts were skipped upgrades each one
// when it is back. The step is reopened and the job runs again, through the
// same gates; it is done again after it. A host that was upgraded meanwhile
// (by hand, or by a later rollout) is marked done. It stops looking a week
// after the job finished, and a newer rollout replaces the job anyway.
func (e *rolloutEngine) catchUp(ctx context.Context, j Rollout) {
	now := e.now()
	limited := e.catchUpAlways == nil || !e.catchUpAlways()
	if j.Finished == nil || (limited && now.Sub(*j.Finished) > rolloutCatchUpFor) || now.Sub(e.lastCatchUp) < e.catchUpEvery {
		return
	}
	if !wantsCatchUp(j) {
		return
	}
	e.lastCatchUp = now
	e.reopen(j, e.ops.Hosts(ctx, j.Version), false)
}

// wantsCatchUp: the job has a host it skipped while it was away.
func wantsCatchUp(j Rollout) bool {
	for _, st := range j.Plan {
		if st.Phase == roSkipped && st.CatchUp {
			return true
		}
	}
	return false
}

// CatchUpNow is catchUp asked by the automatic rollout's own loop, which saw
// a host behind this job's version: no time limit, and a reachable host that
// is not in the plan at all (a worker made at the old version after the job
// was planned) gets a step too. It reports the hosts it reopened or added.
func (e *rolloutEngine) CatchUpNow(ctx context.Context) []string {
	e.mu.Lock()
	j, ok := e.load()
	e.mu.Unlock()
	if !ok || j.State != roDone {
		return nil
	}
	return e.reopen(j, e.ops.Hosts(ctx, j.Version), true)
}

// reopen puts each skipped host that is back, and with addNew each reachable
// host behind that the plan does not have, back in the done job, which runs
// again through the same gates. It reports the hosts it reopened.
func (e *rolloutEngine) reopen(j Rollout, hosts []relHost, addNew bool) []string {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	cur, ok := e.load()
	if !ok || cur.ID != j.ID || cur.Rev != j.Rev || cur.State != roDone {
		return nil
	}
	var back, already []string
	for i := range cur.Plan {
		st := &cur.Plan[i]
		if st.Phase != roSkipped || !st.CatchUp {
			continue
		}
		h, found := findHost(hosts, st.Host)
		switch {
		case !found || !hostUp(h):
		case h.Current:
			t := now
			st.Phase, st.Done, st.To, st.CatchUp, st.Detail = roOK, &t, h.Version, false, "on the version since it is back"
			already = append(already, st.Host)
		case h.Enabled:
			*st = RolloutStep{Host: st.Host, Wave: st.Wave, Phase: roPending, Attempt: st.Attempt + 1, Detail: "back: catching up"}
			back = append(back, st.Host)
		}
	}
	var added []string
	if addNew {
		in := map[string]bool{}
		for _, st := range cur.Plan {
			in[st.Host] = true
		}
		for _, h := range hosts {
			if in[h.Name] || h.Hub || h.WithHub || h.Current || !hostUp(h) || !h.Enabled {
				continue
			}
			cur.Plan = append(cur.Plan, RolloutStep{Host: h.Name, Wave: wavePeers, Phase: roPending, Detail: "behind: catching up"})
			added = append(added, h.Name)
		}
	}
	if len(back) == 0 && len(already) == 0 && len(added) == 0 {
		return nil
	}
	upgrading := append(append([]string{}, back...), added...)
	if len(upgrading) > 0 {
		cur.State, cur.Finished, cur.Note = roRunning, nil, "catching up "+strings.Join(upgrading, ", ")
	}
	if err := e.save(&cur); err != nil {
		log.Printf("rollout: %v", err)
		return nil
	}
	for _, n := range back {
		e.emit(cur, n, "rollout.catchup", true, "back online: upgrading it now", "comebye")
	}
	for _, n := range added {
		e.emit(cur, n, "rollout.catchup", true, "on an older version: upgrading it now", "comebye")
	}
	for _, n := range already {
		e.emit(cur, n, "rollout.catchup", true, "back online, already on the version", "comebye")
	}
	if len(upgrading) > 0 && e.onState != nil {
		e.onState(cur)
	}
	return upgrading
}

// retryHub: a job stopped at the hub because the release host could not be
// asked, or had no deploy command, retries the hub step once the release
// host answers with a command (rollout.catchup). A deploy that ran and
// failed is not retried by itself: that is the user's Retry.
func (e *rolloutEngine) retryHub(ctx context.Context, j Rollout) {
	now := e.now()
	i := -1
	for k, st := range j.Plan {
		if st.Phase == roFailed && st.Mode == stepModeDeploy && st.Reask {
			i = k
		}
	}
	if i < 0 || j.Stop == nil || now.Sub(j.Stop.At) > rolloutCatchUpFor || now.Sub(e.lastReask) < e.catchUpEvery {
		return
	}
	e.lastReask = now
	cctx, cancel := context.WithTimeout(ctx, rolloutHostCallWait)
	ds, err := e.ops.DeployStatus(cctx)
	cancel()
	if err != nil || !ds.Enabled || (ds.Record != nil && ds.Record.State == depRunning) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cur, ok := e.load()
	if !ok || cur.ID != j.ID || cur.Rev != j.Rev || cur.State != roStopped {
		return
	}
	st := &cur.Plan[i]
	*st = RolloutStep{Host: st.Host, Wave: st.Wave, Mode: st.Mode, Phase: roPending, Attempt: st.Attempt + 1, Detail: "the release host answers: retrying the deploy"}
	cur.State, cur.Stop, cur.Note = roRunning, nil, "retrying the hub: the release host answers"
	if err := e.save(&cur); err != nil {
		log.Printf("rollout: %v", err)
		return
	}
	e.emit(cur, st.Host, "rollout.catchup", true, "the release host answers with a deploy command: retrying the hub", "comebye")
	if e.onState != nil {
		e.onState(cur)
	}
}

// isHubStep: the hub's own step (not a peer upgraded with it).
func isHubStep(st RolloutStep) bool { return st.Wave == waveHub && st.Mode != stepModeWithHub }

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
		// A skipped host is not an upgraded one: a rollout that skipped any
		// (above all the hub) is finished, but not ok, and says so. A host
		// skipped because it was stopped or offline is expected: it is
		// caught up when it is back, so it alone does not make it not ok.
		detail := fmt.Sprintf("ok: all %d hosts", len(j.Plan))
		good := len(skipped) == 0
		switch behind := behindOnly(*j); {
		case good:
		case behind != nil:
			good = true
			detail = fmt.Sprintf("ok: %d of %d hosts upgraded, %d behind until they are back (%s)", len(ok), len(j.Plan), len(behind), strings.Join(behind, ", "))
		default:
			detail = fmt.Sprintf("not ok: %d of %d hosts upgraded, %d skipped (%s)", len(ok), len(j.Plan), len(skipped), strings.Join(skipped, ", "))
			if hubSkipped(*j) {
				detail += "; the hub was not bumped"
			}
		}
		e.emit(*j, "", "rollout.finish", good, detail+" elapsed="+now.Sub(j.Started).Round(time.Second).String(), "comebye")
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
		switch st.Mode {
		case stepModeDeploy:
			return e.beginHub(ctx, j, st, h, found, now)
		case stepModeWithHub:
			return e.beginWithHub(j, st, h, found, now)
		}
		return e.begin(ctx, j, st, h, found, now)
	case roUpgrading:
		if st.Mode == stepModeDeploy {
			return e.watchHub(ctx, j, st, h, found, now)
		}
		return e.watchRun(j, st, h, found, now)
	case roWaking:
		return e.watchWake(ctx, j, st, h, found, now)
	case roVerifying:
		if st.Mode == stepModeWithHub {
			return e.verifyWithHub(j, st, h, found, now)
		}
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
		if st.OfflineSince == nil && st.Mode != stepModeDeploy && !j.Auto {
			// A stopped provisioned worker is started, not waited for: the
			// offline grace is for hosts that went quiet for unknown reasons.
			cctx, cancel := context.WithTimeout(ctx, rolloutHostCallWait)
			v := e.ops.Wake(cctx, *j, st.Host)
			cancel()
			switch v.Kind {
			case wakeStarting:
				st.Phase, st.At, st.Woke, st.Detail = roWaking, now, true, wakeText(st.Host, v)
				j.Note = st.Detail
				e.emit(*j, st.Host, "rollout.wake", true, "start requested: it is stopped", "comebye")
				return false
			case wakeRefused:
				e.emit(*j, st.Host, "rollout.wake", false, "not started: "+v.Reason, "comebye")
				e.skip(j, st, now, v.Reason)
				st.CatchUp = true
				return true
			}
		}
		if st.OfflineSince == nil {
			since := now
			if h.Since != nil && hostDown(h) {
				since = *h.Since
			}
			st.OfflineSince = &since
		}
		if now.Sub(*st.OfflineSince) >= e.offlineGrace {
			e.skip(j, st, now, "offline for "+now.Sub(*st.OfflineSince).Round(time.Second).String())
			st.CatchUp = true
			return true
		}
		st.Detail = "offline: waiting for it to come back"
		j.Note = st.Host + " is offline: waiting for it to come back"
		return false
	case !h.Enabled:
		e.skip(j, st, now, "the app is not allowed to upgrade it (start comebye there with -release-upgrade)")
		return true
	}
	st.OfflineSince = nil
	// Adopt a run that is already going for this version: the hub died after
	// sending the start, or a person started it by hand.
	if r := h.Record; r != nil && r.Version == j.Version && !r.Rollback && r.Live(now) {
		e.started(j, st, h, *r, now, "adopted the run already going")
		return false
	}
	wait, how := e.waitBusy(j, st, h, now)
	if wait {
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
	e.started(j, st, h, rec, now, how)
	st.BusyNote = how
	return false
}

// waitBusy holds a step whose host is busy with what a restart would cut
// short (h.Busy: an agent sign-in, a tagging, a deploy), for up to busyWait
// from when the step first saw it so. It says whether to wait this tick
// and, once the wait is over, the line the step's detail keeps: why it went
// ahead (the limit), or that it waited.
func (e *rolloutEngine) waitBusy(j *Rollout, st *RolloutStep, h relHost, now time.Time) (bool, string) {
	// BusySince stays until the step starts (started, hubStarted clear it):
	// a start that fails after the wait does not begin another.
	if len(h.Busy) == 0 {
		if st.BusySince == nil {
			return false, ""
		}
		return false, "waited " + now.Sub(*st.BusySince).Round(time.Second).String() + " while it was busy"
	}
	why := strings.Join(h.Busy, ", ")
	if st.BusySince == nil {
		t := now
		st.BusySince = &t
		e.emit(*j, st.Host, "rollout.busy", true, "waiting: busy ("+why+")", "comebye")
	}
	waited := now.Sub(*st.BusySince)
	if waited < e.busyWait {
		st.Detail = "waiting: busy (" + why + ")"
		j.Note = st.Host + " is busy (" + why + "): waiting up to " + fmtWait(e.busyWait-waited) + " more"
		return true, ""
	}
	return false, "went ahead after waiting " + fmtWait(e.busyWait) + ": still busy (" + why + ")"
}

// fmtWait is a short "14m", "40s" for the time left of a wait.
func fmtWait(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int((d+time.Minute-1)/time.Minute))
	}
	return d.Round(time.Second).String()
}

func wakeText(host string, v wakeVerdict) string {
	t := "starting " + host + "…"
	if v.Step != "" {
		t += " " + oneLine(v.Step, 120)
	}
	return t
}

// watchWake: a stopped host is being started. It is neither offline nor in
// flight: the step carries on as an ordinary one once the host has joined
// the hub, and is skipped, with the reason, if the start fails or takes too
// long (one sleeping VM must not stop the rollout).
func (e *rolloutEngine) watchWake(ctx context.Context, j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	if !found {
		e.skip(j, st, now, "no longer in the fleet")
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, rolloutHostCallWait)
	v := e.ops.WakeStatus(cctx, st.Host)
	cancel()
	took := now.Sub(st.At).Round(time.Second)
	switch {
	case v.Kind == wakeFailed:
		e.emit(*j, st.Host, "rollout.wake", false, "could not wake: "+oneLine(v.Reason, 200), "comebye")
		e.skip(j, st, now, "could not wake "+st.Host+": "+oneLine(v.Reason, 200))
		st.CatchUp = true
		return true
	case hostUp(h):
		st.Phase, st.At, st.Detail, st.OfflineSince = roPending, now, "", nil
		e.emit(*j, st.Host, "rollout.wake", true, "joined the hub after "+took.String(), "comebye")
		return true
	case v.Kind != wakeStarting && v.Kind != wakeUp:
		e.emit(*j, st.Host, "rollout.wake", false, "the wake was lost", "comebye")
		e.skip(j, st, now, "could not wake "+st.Host+": the hub lost track of the start")
		st.CatchUp = true
		return true
	case now.Sub(st.At) >= e.wakeWait:
		e.emit(*j, st.Host, "rollout.wake", false, "did not join within "+e.wakeWait.String(), "comebye")
		e.skip(j, st, now, "could not wake "+st.Host+": it did not join the hub within "+e.wakeWait.Round(time.Second).String())
		st.CatchUp = true
		return true
	}
	st.Detail = wakeText(st.Host, v)
	j.Note = st.Detail
	return false
}

func hostDown(h relHost) bool { return h.Status != "ok" }

func (e *rolloutEngine) started(j *Rollout, st *RolloutStep, h relHost, rec upgrade.Record, now time.Time, how string) {
	t := now
	st.Phase, st.Run, st.From, st.At, st.Started = roUpgrading, rec.ID, h.Version, now, &t
	st.TryingSince, st.BusySince, st.Detail = nil, nil, how
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
		if d := e.soakDuration(); st.Wave == waveCanary && d > 0 {
			until := now.Add(d)
			st.Phase, st.At, st.SoakUntil, st.ProcStart = roSoaking, now, &until, h.ProcStart
			st.Detail = "soak: " + d.Round(time.Second).String() + " left of " + d.Round(time.Second).String()
			j.Note = "soaking " + st.Host
			e.emit(*j, st.Host, "rollout.gate", true, fmt.Sprintf("upgrade=ok back=ok version=%s took=%s soak=%s", h.Version, took(*st, now), d.Round(time.Second)), "comebye")
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
// stops the rollout before another host is touched (rollout_canary.go).
// The soak's end is in the step (SoakUntil), so a restart of the hub in the
// middle carries on with the time that is left.
func (e *rolloutEngine) soaking(j *Rollout, st *RolloutStep, h relHost, found bool, now time.Time) bool {
	j.Note = "soaking " + st.Host
	until := st.At.Add(e.soak) // a job saved before SoakUntil was kept
	if st.SoakUntil != nil {
		until = *st.SoakUntil
	} else {
		st.SoakUntil = &until
	}
	switch {
	case !found:
		e.failSoak(j, st, now, "left the fleet during the soak", "")
		return false
	case !hostUp(h):
		why := "went offline during the soak (a failed health check)"
		if h.Unknown {
			why = "did not answer its health check during the soak"
		}
		e.failSoak(j, st, now, why, "")
		return false
	case !h.Current && !(h.DryRun && h.Record != nil && h.Record.State == upgrade.RecDryRun):
		e.failSoak(j, st, now, "went back to "+orUnknown(h.Version)+" during the soak: it no longer runs "+j.Version, "")
		return false
	case h.Record != nil && h.Record.ID != st.Run && (h.Record.State == upgrade.RecFailed || h.Record.State == upgrade.RecRolledBack):
		e.failSoak(j, st, now, "a new upgrade run failed during the soak: "+oneLine(h.Record.Error, 200), h.Record.Output)
		return false
	case st.ProcStart != nil && h.ProcStart != nil && !h.ProcStart.Equal(*st.ProcStart):
		e.failSoak(j, st, now, fmt.Sprintf("comebye restarted on %s during the soak (it started again at %s, %s into the soak): a crash loop",
			st.Host, h.ProcStart.UTC().Format("15:04:05"), h.ProcStart.Sub(st.At).Round(time.Second)), "")
		return false
	}
	if st.ProcStart == nil && h.ProcStart != nil {
		t := *h.ProcStart // the host says it only now (a hub that kept no start time)
		st.ProcStart = &t
	}
	total := until.Sub(st.At).Round(time.Second)
	if left := until.Sub(now); left > 0 {
		st.Detail = "soak: " + left.Round(time.Second).String() + " left of " + total.String()
		return false
	}
	e.emit(*j, st.Host, "rollout.gate", true, fmt.Sprintf("soak=ok %s", total), "comebye")
	st.Detail = ""
	e.finishStep(j, st, h, now, "soaked "+total.String())
	return true
}

// failSoak stops the rollout at the canary, and files its To do.
func (e *rolloutEngine) failSoak(j *Rollout, st *RolloutStep, now time.Time, why, output string) {
	e.fail(j, st, now, why, output)
	if e.onSoakFail != nil {
		e.onSoakFail(*j, *st)
	}
}

func (e *rolloutEngine) finishStep(j *Rollout, st *RolloutStep, h relHost, now time.Time, note string) {
	t := now
	st.Phase, st.Done, st.Detail = roOK, &t, note
	if st.To == "" {
		st.To = h.Version
	}
	if st.Woke && note == "" {
		st.Detail = "woke it, then upgraded; it powers off again when idle"
		note = "(woke it)"
	}
	if st.BusyNote != "" {
		if st.Detail != "" {
			st.Detail += " · "
		}
		st.Detail += st.BusyNote
	}
	e.emit(*j, st.Host, "rollout.gate", true, fmt.Sprintf("done version=%s took=%s %s", st.To, took(*st, now), note), "comebye")
	if j.PauseRequested {
		if current(*j) >= 0 {
			j.State, j.PauseRequested, j.Note = roPaused, false, "paused after "+st.Host
			e.emit(*j, "", "rollout.pause", true, "now, after "+st.Host, "comebye")
		}
	}
}

// behindOnly names the job's skipped hosts when every one of them was
// skipped only for being stopped or offline (to be caught up when it is
// back); nil when there is none, or when any skip had another reason (the
// hub, a host the app may not upgrade, an error).
func behindOnly(j Rollout) []string {
	var out []string
	for _, st := range j.Plan {
		if st.Phase != roSkipped {
			continue
		}
		if !st.CatchUp || st.Wave == waveHub {
			return nil
		}
		out = append(out, st.Host)
	}
	return out
}

// hubSkipped: the job's hub step was skipped (the hub was not upgraded).
func hubSkipped(j Rollout) bool {
	for _, st := range j.Plan {
		if isHubStep(st) && st.Phase == roSkipped {
			return true
		}
	}
	return false
}

func (e *rolloutEngine) skip(j *Rollout, st *RolloutStep, now time.Time, why string) {
	t := now
	st.Phase, st.Done, st.Detail = roSkipped, &t, why
	e.emit(*j, st.Host, "rollout.skip", true, why, "comebye")
	if isHubStep(*st) && e.onHubTrouble != nil {
		e.onHubTrouble(*j, *st)
	}
}

func (e *rolloutEngine) fail(j *Rollout, st *RolloutStep, now time.Time, why, output string) {
	st.Phase, st.Detail, st.Output = roFailed, oneLine(why, 400), output
	j.State, j.Note = roStopped, "stopped at "+st.Host
	j.Stop = &RolloutStop{Host: st.Host, Reason: oneLine(why, 400), At: now}
	e.emit(*j, st.Host, "rollout.stop", false, why, "comebye")
	if isHubStep(*st) && e.onHubTrouble != nil {
		e.onHubTrouble(*j, *st)
	}
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

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/provision"
)

// Waking a stopped worker (issue #66, slice 1).
//
// A worker that powered itself off (the idle timer, a budget cap) is a
// provisioned host the cloud says is STOPPED, which is not the same as a host
// whose network is dead. The hub keeps the provisioners' last look at their
// hosts (provhosts.go), and this file adds the one thing it did not have: a
// start. The provisioner runs `worker start NAME` as a job (provjobs.go); the
// hub follows the job, then waits until the peer is ok in the fleet before it
// calls the wake done.
//
//	POST /api/provision/hosts/{name}/start   {via?, confirm_cap}   the user, or the coordinator
//	GET  /api/provision/wake                 the wake book, for the app's progress
//
// The wake book (wake.json in the state directory) holds one entry per host:
// starting, up or failed, with the step line, who asked and when. A restart of
// the hub loses nothing: a wake that was following a job follows it again.

const (
	wakeStarting = "starting"
	wakeUp       = "up"
	wakeFailed   = "failed"

	wakeBusyWait = 2 * time.Minute // a busy provisioner: ask again for this long
	wakeJoinWait = 3 * time.Minute // started, and then this long for the peer to be ok
	wakeJobWait  = 12 * time.Minute

	// The limits for automatic wakes (slices 2 and 3 call wakeAllow). A tap on
	// Start ignores them.
	wakeHostPerHour  = 3
	wakeFleetPerHour = 10
)

// wakeBackoff is how long after the nth failed automatic wake the next waits.
var wakeBackoff = []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

type wakeEntry struct {
	Host     string    `json:"host"`
	Via      string    `json:"via"`
	State    string    `json:"state"`
	Step     string    `json:"step,omitempty"` // the tool's last line, or what the hub waits for
	Reason   string    `json:"reason,omitempty"`
	Job      string    `json:"job,omitempty"` // the hub job id
	By       string    `json:"by"`
	Why      string    `json:"why,omitempty"` // tap, rollout, ...
	Since    time.Time `json:"since"`
	Updated  time.Time `json:"updated"`
	Failures int       `json:"failures,omitempty"`
	LastTry  time.Time `json:"last_attempt,omitempty"`
}

type wakeFile struct {
	Hosts map[string]*wakeEntry `json:"hosts"`
	Auto  []wakeAuto            `json:"auto,omitempty"`
}

// wakeAuto is one automatic wake, for the hourly limits.
type wakeAuto struct {
	Host string    `json:"host"`
	At   time.Time `json:"at"`
}

type wakeBook struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
	m    map[string]*wakeEntry
	auto []wakeAuto

	busyWait, joinWait, pollGap, retryGap, sessionGrace time.Duration // tests shorten them
}

func openWakeBook(stateDir string) *wakeBook {
	b := &wakeBook{now: time.Now, m: map[string]*wakeEntry{}, busyWait: wakeBusyWait, joinWait: wakeJoinWait, pollGap: 2 * time.Second, retryGap: 3 * time.Second, sessionGrace: wakeSessionGrace}
	if stateDir == "" {
		return b
	}
	b.path = filepath.Join(stateDir, "wake.json")
	raw, err := os.ReadFile(b.path)
	if err != nil {
		return b
	}
	var f wakeFile
	if json.Unmarshal(raw, &f) != nil {
		return b
	}
	for h, e := range f.Hosts {
		if e != nil && provision.ValidName(h) && e.Host == h {
			b.m[h] = e
		}
	}
	b.auto = f.Auto
	return b
}

func (b *wakeBook) saveLocked() {
	if b.path == "" {
		return
	}
	raw, err := json.Marshal(wakeFile{Hosts: b.m, Auto: b.auto})
	if err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		os.Rename(tmp, b.path)
	}
}

// wakeView is an entry as the app reads it.
type wakeView struct {
	Host    string `json:"host"`
	Via     string `json:"via"`
	State   string `json:"state"`
	Step    string `json:"step,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Job     string `json:"job,omitempty"`
	By      string `json:"by"`
	Why     string `json:"why,omitempty"`
	Since   string `json:"since"`
	Updated string `json:"updated"`
	Fails   int    `json:"failures,omitempty"`
}

func (e *wakeEntry) view() wakeView {
	return wakeView{Host: e.Host, Via: e.Via, State: e.State, Step: e.Step, Reason: e.Reason, Job: e.Job, By: e.By, Why: e.Why,
		Since: e.Since.UTC().Format(time.RFC3339), Updated: e.Updated.UTC().Format(time.RFC3339), Fails: e.Failures}
}

func (b *wakeBook) views() map[string]wakeView {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]wakeView{}
	for h, e := range b.m {
		out[h] = e.view()
	}
	return out
}

func (b *wakeBook) get(host string) (wakeView, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.m[host]
	if e == nil {
		return wakeView{}, false
	}
	return e.view(), true
}

// begin adds a starting entry, or returns the one already running (joined).
func (b *wakeBook) begin(host, via, by, why string) (e *wakeEntry, joined bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if old := b.m[host]; old != nil && old.State == wakeStarting {
		return old, true
	}
	now := b.now()
	fails := 0
	if old := b.m[host]; old != nil {
		fails = old.Failures
	}
	e = &wakeEntry{Host: host, Via: via, State: wakeStarting, Step: "asking " + via + " to start it", By: by, Why: why, Since: now, Updated: now, Failures: fails, LastTry: now}
	b.m[host] = e
	b.saveLocked()
	return e, false
}

func (b *wakeBook) update(host string, f func(e *wakeEntry)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.m[host]; e != nil {
		f(e)
		e.Updated = b.now()
		b.saveLocked()
	}
}

func (b *wakeBook) fail(host, reason string) {
	b.update(host, func(e *wakeEntry) {
		e.State, e.Reason, e.Step = wakeFailed, provision.Text(reason, 200), ""
		e.Failures++
	})
}

func (b *wakeBook) succeed(host string) {
	b.update(host, func(e *wakeEntry) { e.State, e.Reason, e.Step, e.Failures = wakeUp, "", "", 0 })
}

// wakeAllow is the rate limit for an automatic wake (rollout, a prompt to a
// stopped host): slices 2 and 3 call it before wake and record with
// noteAuto. A tap on Start never asks. It names what to do when it refuses.
func (b *wakeBook) wakeAllow(host string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if e := b.m[host]; e != nil && e.State == wakeFailed && e.Failures > 0 {
		i := e.Failures - 1
		if i >= len(wakeBackoff) {
			i = len(wakeBackoff) - 1
		}
		if wait := e.LastTry.Add(wakeBackoff[i]).Sub(now); wait > 0 {
			return fmt.Errorf("%s did not start the last time: it tries again in %s; tap Start to start it now", host, wait.Round(time.Minute))
		}
	}
	perHost, fleet := 0, 0
	for _, a := range b.auto {
		if now.Sub(a.At) < time.Hour {
			fleet++
			if a.Host == host {
				perHost++
			}
		}
	}
	switch {
	case perHost >= wakeHostPerHour:
		return fmt.Errorf("%s woke %d times this hour; tap Start to start it anyway", host, perHost)
	case fleet >= wakeFleetPerHour:
		return fmt.Errorf("%d workers have woken this hour; tap Start to start %s anyway", fleet, host)
	}
	return nil
}

func (b *wakeBook) noteAuto(host string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	keep := b.auto[:0]
	for _, a := range b.auto {
		if now.Sub(a.At) < time.Hour {
			keep = append(keep, a)
		}
	}
	b.auto = append(keep, wakeAuto{Host: host, At: now})
	b.saveLocked()
}

// ---- the hub's route ----

type wakeBody struct {
	Via        string `json:"via"`
	ConfirmCap bool   `json:"confirm_cap"`
}

func (s *Server) peerStatus(name string) string {
	if s.peerStatusHook != nil {
		return s.peerStatusHook(name)
	}
	for _, h := range s.fleet.Hosts() {
		if h.Name == name {
			return h.Status
		}
	}
	return ""
}

// ownerOf finds the node whose book lists the host (fresh look), and its record.
func (s *Server) ownerOf(ctx context.Context, name, via string) (owner string, hv *provision.HostView, ambiguous bool) {
	for _, n := range s.hostsNodes() {
		if via == "" || via == n {
			s.askHosts(ctx, n, true)
		}
	}
	s.prov.mu.Lock()
	defer s.prov.mu.Unlock()
	for n, c := range s.prov.hosts {
		if c.View == nil || (via != "" && via != n) {
			continue
		}
		for i := range c.View.Hosts {
			h := c.View.Hosts[i]
			if h.Name == name && h.State != "destroyed" {
				if owner != "" {
					return "", nil, true
				}
				cp := h
				owner, hv = n, &cp
			}
		}
	}
	return owner, hv, false
}

// handleProvisionHostStart: POST /api/provision/hosts/{name}/start. Only the
// user (with the page's CSRF token) or the coordinator may start a worker: it
// costs money, and an agent can loop. It answers at once (202) with the wake;
// the app reads GET /api/provision/wake for progress.
func (s *Server) handleProvisionHostStart(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	switch {
	case c.agent() && !s.callerIsCoordinator(c):
		relErr(w, http.StatusForbidden, "agent_forbidden", "only the user or the coordinator may start a worker: ask the user to start it")
		return
	case !c.agent() && !s.csrfOK(r):
		relErr(w, http.StatusForbidden, "csrf", "this page's token is missing or stale: reload the page")
		return
	}
	name := r.PathValue("name")
	var b wakeBody
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&b); err != nil {
			relErr(w, http.StatusBadRequest, "bad_request", "invalid body")
			return
		}
	}
	if !provision.ValidName(name) || !s.fleet.HasPeer(name) {
		relErr(w, http.StatusNotFound, "no_host", name+" is not a worker on this hub")
		return
	}
	if cur, ok := s.wake.get(name); ok && cur.State == wakeStarting {
		writeJSONStatus(w, http.StatusOK, s.wakeAnswer(r, cur)) // one wake at a time: joined
		return
	}
	owner, hv, amb := s.ownerOf(r.Context(), name, b.Via)
	switch {
	case amb:
		relErr(w, http.StatusConflict, "ambiguous", "more than one node reports a host with this name: say which")
		return
	case owner == "":
		relErr(w, http.StatusNotFound, "not_provisioned", name+" is not a provisioned worker: no provisioner lists it")
		return
	}
	if st := s.peerStatus(owner); st != "ok" {
		relErr(w, http.StatusConflict, "provisioner_offline", owner+" is offline, and it is the node that can start "+name+": try again when it is back")
		return
	}
	switch hv.State {
	case "stopped":
	case "destroying", "destroyed", "gone":
		relErr(w, http.StatusConflict, "bad_state", name+" is "+hv.State+": it cannot be started")
		return
	default:
		relErr(w, http.StatusConflict, "not_stopped", "the cloud says "+name+" is "+hv.State+", not stopped: if it is unreachable that is a network fault, and starting it would not help")
		return
	}
	due := false // the destroy timer has run out: it would destroy the host again soon
	if t, err := time.Parse(time.RFC3339, hv.DestroyDue); err == nil && !time.Now().Before(t) {
		due = true
	}
	if (hv.CapStopped != "" || due) && !b.ConfirmCap {
		msg := name + " was stopped at its budget cap"
		if hv.CapStopped == "" {
			msg = name + " is due to be destroyed"
		}
		writeJSONStatus(w, http.StatusConflict, map[string]any{"code": "needs_confirm", "message": msg + ": starting it again costs money. Confirm to start it.", "cap_usd": hv.BudgetCapUSD, "spent_usd": hv.Spend.USD})
		return
	}
	by := s.by(r, "")
	e, joined := s.wake.begin(name, owner, by, "tap")
	v, _ := s.wake.get(name)
	if !joined {
		s.audit(r, ledger.AuditEvent{Action: "provision.start", OK: true, Host: owner, Detail: name + " started from the app" + capWord(b.ConfirmCap && (hv.CapStopped != "" || due))})
		go s.wakeRun(e.Host, owner, b.ConfirmCap, by, false)
	}
	writeJSONStatus(w, http.StatusAccepted, s.wakeAnswer(r, v))
}

func capWord(confirmed bool) string {
	if confirmed {
		return " (the budget cap or the destroy timer was confirmed)"
	}
	return ""
}

func (s *Server) wakeAnswer(r *http.Request, v wakeView) map[string]any {
	return map[string]any{"wake": v, "csrf": s.csrfToken(r)}
}

// handleProvisionWake: GET /api/provision/wake.
func (s *Server) handleProvisionWake(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"wakes": s.wake.views(), "csrf": s.csrfToken(r)})
}

// ---- following a wake ----

// wakeRun asks the provisioner to start the host, follows its job, and waits
// for the peer to be ok. It ends the entry as up or failed, never leaves it
// starting.
func (s *Server) wakeRun(host, via string, confirmCap bool, by string, resumed bool) {
	b := s.wake
	ctx, cancel := context.WithTimeout(context.Background(), wakeJobWait+b.joinWait+b.busyWait)
	defer cancel()
	var jobID string
	if resumed {
		if v, ok := b.get(host); ok {
			jobID = v.Job
		}
	}
	if jobID == "" {
		j, err := s.wakeAsk(ctx, host, via, confirmCap, by)
		if err != nil {
			b.fail(host, err.Error())
			s.auditBy(ledger.AuditEvent{Action: "provision.start_failed", By: by, OK: false, Host: via, Detail: host + ": " + oneLine(err.Error(), 160)})
			return
		}
		jobID = j
	}
	// follow the job
	deadline := time.Now().Add(wakeJobWait)
	for {
		s.hjobs.mu.Lock()
		j := s.hjobs.jobs[jobID]
		s.hjobs.mu.Unlock()
		if j == nil {
			b.fail(host, "the hub lost track of the start job")
			return
		}
		s.refreshJob(ctx, j)
		s.hjobs.mu.Lock()
		st, msg, line, offline := j.State, j.Msg, lastLine(j.Items), j.offline
		s.hjobs.mu.Unlock()
		if line != "" {
			b.update(host, func(e *wakeEntry) { e.Step = line })
		} else if offline != "" {
			b.update(host, func(e *wakeEntry) { e.Step = offline })
		}
		if !open(st) {
			if st != jobOK {
				if msg == "" {
					msg = "the start did not finish"
				}
				b.fail(host, msg)
				s.auditBy(ledger.AuditEvent{Action: "provision.start_failed", By: by, OK: false, Host: via, Detail: host + ": " + oneLine(msg, 160)})
				return
			}
			break
		}
		if time.Now().After(deadline) {
			b.fail(host, "the start took more than "+wakeJobWait.String())
			return
		}
		select {
		case <-ctx.Done():
			b.fail(host, "the start took too long")
			return
		case <-time.After(b.pollGap):
		}
	}
	// started: wait until the peer is ok in the fleet
	b.update(host, func(e *wakeEntry) { e.Step = "started; waiting for " + host + " to join the hub" })
	until := time.Now().Add(b.joinWait)
	for s.peerStatus(host) != "ok" {
		if time.Now().After(until) {
			b.fail(host, "it started, but did not join the hub within "+b.joinWait.String())
			return
		}
		time.Sleep(b.pollGap)
	}
	b.succeed(host)
	s.afterWake(host)
	s.prov.mu.Lock()
	if c := s.prov.hosts[via]; c != nil {
		c.At = time.Time{} // look again at once: it is running now
	}
	s.prov.mu.Unlock()
	s.auditBy(ledger.AuditEvent{Action: "provision.start_done", By: by, OK: true, Host: via, Detail: host + " is up"})
}

func lastLine(items []provision.Item) string {
	for i := len(items) - 1; i >= 0; i-- {
		if m := strings.TrimSpace(items[i].Msg); m != "" {
			return m
		}
	}
	return ""
}

// wakeAsk calls the provisioner's start, retrying while it is busy, and
// records the hub job. It returns the hub job id.
func (s *Server) wakeAsk(ctx context.Context, host, via string, confirmCap bool, by string) (string, error) {
	b := s.wake
	until := time.Now().Add(b.busyWait)
	for {
		code, body, err := s.callBody(ctx, via, "provision-host-start", map[string]any{"name": host, "confirm_cap": confirmCap, "why": "from the app: " + by})
		switch {
		case err != nil:
			return "", errors.New(oneLine(provision.Text(via+" did not answer: "+err.Error(), 160), 160))
		case code == http.StatusAccepted || code == http.StatusOK:
			pv, ok := parsePeerJob(body)
			if !ok {
				return "", errors.New("the node's job was not understood")
			}
			var idb [8]byte
			rand.Read(idb[:])
			j := &hubJob{ID: "hj_" + hex.EncodeToString(idb[:]), Key: "s" + hex.EncodeToString(idb[:]), Via: via, PeerID: pv.ID, Spec: pv.Spec, Region: pv.Spec.Zone, Op: opStart, Created: s.hjobs.now(), By: by}
			applyPeerJob(j, pv)
			s.hjobs.mu.Lock()
			s.hjobs.jobs[j.ID] = j
			s.hjobs.saveLocked()
			s.hjobs.mu.Unlock()
			b.update(host, func(e *wakeEntry) { e.Job, e.Step = j.ID, "starting "+host })
			return j.ID, nil
		case code == http.StatusConflict && strings.Contains(string(body), `"busy"`):
			if time.Now().After(until) {
				return "", errors.New(via + " stayed busy with another job: try again in a moment")
			}
			b.update(host, func(e *wakeEntry) { e.Step = "waiting for the provisioner" })
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(b.retryGap):
			}
		default:
			return "", errors.New(oneLine(peerMessage(body, code), 160))
		}
	}
}

// wakeResume picks up the wakes a restart of the hub cut short: one that had
// a job follows it again; one that never reached the provisioner fails with
// that reason (a retry is a tap).
func (s *Server) wakeResume() {
	b := s.wake
	b.mu.Lock()
	var run []*wakeEntry
	var names []string
	for h, e := range b.m {
		if e.State == wakeStarting {
			run = append(run, e)
			names = append(names, h)
		}
	}
	b.mu.Unlock()
	sort.Strings(names)
	for _, e := range run {
		if e.Job == "" {
			b.fail(e.Host, "the hub restarted before the provisioner was asked: tap Start again")
			continue
		}
		go s.wakeRun(e.Host, e.Via, false, e.By, true)
	}
}

// rolloutWakeMin is about how long a woken worker runs for a rollout: a boot
// and join (about 2 minutes) and the upgrade and verify (about 2).
const rolloutWakeMin = 4

// markWakeable tells the rollout preview which offline hosts are stopped
// provisioned workers that the rollout would start (issue #66), from the
// provisioners' last look (cached: a preview asks no cloud). It mirrors the
// refusals of wakeAuto: a budget-cap stop, a host due to be destroyed and a
// provisioner that is offline are not started by a rollout. A host that is
// not a stopped provisioned worker is left as "offline".
func (s *Server) markWakeable(ctx context.Context, hosts []relHost) {
	if s.wake == nil {
		return
	}
	for _, h := range hosts {
		if !h.Local && h.Status != "ok" {
			// something is down: make sure the provisioners' books are not empty (a cached look is used when it is fresh)
			for _, n := range s.hostsNodes() {
				s.askHosts(ctx, n, false)
			}
			break
		}
	}
	for i := range hosts {
		h := &hosts[i]
		if h.Local || h.Status == "ok" || !provision.ValidName(h.Name) {
			continue
		}
		hv, owner := s.cachedHost(h.Name)
		if hv == nil || hv.State != "stopped" {
			continue
		}
		switch {
		case hv.CapStopped != "":
			h.WakeNote = "stopped by its budget cap: start it from Peers"
		case dueNow(hv.DestroyDue):
			h.WakeNote = "stopped and due to be destroyed: start it from Peers if you want it"
		case s.peerStatus(owner) != "ok":
			h.WakeNote = "stopped, but " + owner + " (the node that can start it) is offline"
		default:
			h.Stopped, h.WakeMin = true, rolloutWakeMin
		}
	}
}

func dueNow(due string) bool {
	t, err := time.Parse(time.RFC3339, due)
	return err == nil && !time.Now().Before(t)
}

// cachedHost finds a host in the provisioners' cached books (no network).
// nil when none lists it, or more than one does.
func (s *Server) cachedHost(name string) (*provision.HostView, string) {
	s.prov.mu.Lock()
	defer s.prov.mu.Unlock()
	var out *provision.HostView
	var owner string
	for n, c := range s.prov.hosts {
		if c.View == nil {
			continue
		}
		for i := range c.View.Hosts {
			h := c.View.Hosts[i]
			if h.Name == name && h.State != "destroyed" {
				if out != nil {
					return nil, ""
				}
				cp := h
				out, owner = &cp, n
			}
		}
	}
	return out, owner
}

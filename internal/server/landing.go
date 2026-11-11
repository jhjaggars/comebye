package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Landing (docs/lifecycle.md): after the user OKs it, `comebye land`
// records the approval here, moving the worker's entry to landing, and
// then sends the worker the land prompt. When the worker reports again,
// the hub checks its worktree on its host: clean, and every commit of the
// approved tip in the base, and the entry is landed; otherwise it goes
// back to finished with the check's verdict. Either way the requester of
// the handoff is told.

// landBody is the JSON body of POST /api/landings.
type landBody struct {
	AgentID string `json:"agent_id"`
	// Note is the user's OK as the coordinator relays it.
	Note string `json:"note"`
	Base string `json:"base"`
	// Branch names the branch the work lands from when it is not the
	// handoff's own (a follow-on task on a new branch); the landing check
	// then follows it.
	Branch string `json:"branch"`
	// Cancel undoes a landing whose prompt could not be sent.
	Cancel bool `json:"cancel"`
}

const maxLandNote = 500

// handleLanding records the approval to land a finished worker's work.
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b landBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil || b.AgentID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if c := callerOf(r); c.agent() && b.AgentID == c.agentRef() {
		writeJSONStatus(w, http.StatusConflict, apiErr("self", "an agent cannot land its own work this way: the coordinator does, after the user OKs it"))
		return
	}
	e, ok := s.ledger.ByAgent(b.AgentID)
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, apiErr("no_handoff", "this agent has no handoff in the ledger"))
		return
	}
	if b.Cancel {
		done, err := s.ledger.CancelLanding(b.AgentID)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, apiErr("ledger_error", err.Error()))
			return
		}
		writeJSON(w, map[string]any{"cancelled": done})
		return
	}
	b.Base = strings.TrimSpace(b.Base)
	if b.Base == "" {
		b.Base = "main"
	}
	if !validBranch(b.Base) {
		writeJSONStatus(w, http.StatusBadRequest, apiErr("bad_branch", "invalid base"))
		return
	}
	b.Branch = strings.TrimSpace(b.Branch)
	if b.Branch != "" && !validBranch(b.Branch) {
		writeJSONStatus(w, http.StatusBadRequest, apiErr("bad_branch", "invalid branch"))
		return
	}
	note := strings.TrimSpace(b.Note)
	if note == "" {
		writeJSONStatus(w, http.StatusBadRequest, apiErr("note_required", "say how the user OKed landing (--note), in their words"))
		return
	}
	if len(note) > maxLandNote {
		note = clip(note, maxLandNote)
	}
	retry := e.Status == ledger.Landing && e.FinishedAt == nil
	if !retry && (e.Status != ledger.Finished || strings.TrimSpace(e.FinalReport) == "") {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"code": "not_ready",
			"message": "not ready to land: its ledger entry is " + string(e.Status) + reportPhrase(e) + "; land after the worker has finished and reported", "entry": e})
		return
	}
	// The tip being approved, so the landing check can tell that it, not
	// just whatever the worktree holds then, reached the base.
	if retry && b.Branch == "" && e.Approval != nil {
		b.Branch = e.Approval.Branch
	}
	tip := ""
	if e.Worktree != "" && s.knownHost(e.Host) {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		c, lerr := s.checksOn(ctx, e.Host, e.Worktree, orDefault(b.Branch, e.Branch), b.Base, checkOpts{}.forEntry(e))
		cancel()
		if lerr == nil && c.Exists && !c.Primary {
			tip = c.Tip
		}
	}
	if retry && tip == "" && e.Approval != nil {
		tip = e.Approval.Tip
	}
	got, ok, err := s.ledger.StartLanding(b.AgentID, ledger.Approval{By: s.by(r, ""), Note: note, Base: b.Base, Tip: tip, Branch: b.Branch})
	switch {
	case err != nil:
		writeJSONStatus(w, http.StatusInternalServerError, apiErr("ledger_error", err.Error()))
	case !ok:
		writeJSONStatus(w, http.StatusConflict, map[string]any{"code": "not_ready", "message": "not ready to land: its ledger entry is " + string(got.Status), "entry": got})
	default:
		writeJSON(w, map[string]any{"entry": got, "retry": retry})
	}
}

func reportPhrase(e ledger.Entry) string {
	if strings.TrimSpace(e.FinalReport) == "" {
		return " and has no final report"
	}
	return ""
}

// How a landing is followed. The work can reach the base at any moment, and
// nothing says when, so an open landing is re-checked on a schedule that
// starts quick and backs off, up to a cap; a report from the worker, or the
// worker going idle, checks it at once. A landing is not failed because a
// check found the work not on the base yet: only a worker that said it was
// done (or stopped) with nothing running can fail it (landFailure).
const (
	landFirst     = 15 * time.Second // the first re-check after a check
	landMax       = 5 * time.Minute  // the longest wait between checks
	landGrace     = 2 * time.Minute  // a report this old, and nothing running: failed
	landStopAfter = 6 * time.Hour    // stop re-checking a landing open this long
	ciPoll        = 30 * time.Second // CI of a landed commit
	ciPollMax     = 2 * time.Minute
	ciStopAfter   = 45 * time.Minute // stop watching CI this long after landing
	ciNoneAfter   = 5 * time.Minute  // no CI run found this long after landing: none
)

// landSched is when an entry's landing is checked next.
type landSched struct {
	next    time.Time
	wait    time.Duration
	sig     time.Time // the entry's FinishedAt then: a new report resets it
	stopped bool
}

// landDue reports whether e is due a check now, and the schedule state.
func (s *Server) landDue(e ledger.Entry, now time.Time) bool {
	s.coord.mu.Lock()
	defer s.coord.mu.Unlock()
	sc, ok := s.coord.landSched[e.ID]
	sig := time.Time{}
	if e.FinishedAt != nil {
		sig = *e.FinishedAt
	}
	if !ok || !sc.sig.Equal(sig) {
		return true // never checked, or the worker reported since
	}
	return !sc.stopped && !now.Before(sc.next)
}

// landNext schedules the entry's next check after one made now.
func (s *Server) landNext(e ledger.Entry, now time.Time, first, max time.Duration, stop bool) {
	s.coord.mu.Lock()
	defer s.coord.mu.Unlock()
	sc := s.coord.landSched[e.ID]
	sig := time.Time{}
	if e.FinishedAt != nil {
		sig = *e.FinishedAt
	}
	if !sc.sig.Equal(sig) || sc.wait == 0 {
		sc.wait = first
	} else {
		sc.wait = min(sc.wait*3/2, max)
	}
	sc.sig, sc.next, sc.stopped = sig, now.Add(sc.wait), stop
	s.coord.landSched[e.ID] = sc
}

// checkLandings starts the checks of the landings that are due.
func (s *Server) checkLandings() {
	now := time.Now()
	for _, e := range s.ledger.LandingsToCheck() {
		if e.Status == ledger.Landing && e.FinishedAt == nil && (e.Approval == nil || e.Approval.Tip == "") {
			continue // no report, and no approved tip to look for
		}
		if e.Status == ledger.Landed && ciOver(e, now) {
			continue
		}
		if !s.landDue(e, now) {
			continue
		}
		key := "land/" + e.ID
		s.coord.mu.Lock()
		busy := s.coord.checking[key]
		s.coord.checking[key] = true
		s.coord.mu.Unlock()
		if busy {
			continue
		}
		go func() {
			defer func() {
				s.coord.mu.Lock()
				delete(s.coord.checking, key)
				s.coord.mu.Unlock()
			}()
			s.checkLanding(e)
		}()
	}
}

// ciOver: there is no point in watching this landed entry's CI any more.
func ciOver(e ledger.Entry, now time.Time) bool {
	l := e.LandCheck
	if l == nil || l.CI.Settled() {
		return true
	}
	if now.Sub(l.At) > ciStopAfter {
		return true
	}
	return l.CI != nil && l.CI.State == ledger.CINone && now.Sub(l.At) > ciNoneAfter
}

// checkLanding checks a landing on its host and settles it, or a landed
// entry's CI.
func (s *Server) checkLanding(e ledger.Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	now := time.Now()
	if e.Status == ledger.Landed {
		s.checkCI(ctx, e, now)
		return
	}
	it := s.landingChecks(ctx, e)
	lc := landingVerdict(it)
	if lc.OK && e.FinishedAt == nil && (e.Approval == nil || e.Approval.Tip == "") {
		lc.OK = false // cannot tell that the approved work is what is on the base
	}
	if !lc.OK {
		sg := s.landSignals(ctx, e, now)
		failed, why := landFailure(e, sg)
		lc.Reason = why
		if failed {
			lc.State = ledger.LandFailed
			lc.Verdict = why + ": " + lc.Verdict
		} else {
			lc.State = ledger.LandPending
		}
	}
	stop := !lc.OK && e.Approval != nil && now.Sub(e.Approval.At) > landStopAfter
	lc.Stopped = stop
	lc.At = now.UTC()
	if _, err := s.ledger.SetLandCheck(e.ID, e.FinishedAt, lc); err != nil {
		log.Printf("ledger: %v", err)
	}
	s.landNext(e, now, landFirst, landMax, stop)
}

// checkCI reads the CI of a landed entry's commit and records it.
func (s *Server) checkCI(ctx context.Context, e ledger.Entry, now time.Time) {
	sha := landedSHA(e)
	if sha == "" {
		s.landNext(e, now, ciPoll, ciPollMax, true)
		return
	}
	opts := checkOpts{}.forEntry(e)
	opts.CI = sha
	base := e.Base
	if e.Approval != nil && e.Approval.Base != "" {
		base = e.Approval.Base
	}
	c, lerr := s.checksOn(ctx, e.Host, e.Worktree, landBranch(e), base, opts)
	if lerr == nil && c.CI != nil {
		if _, _, err := s.ledger.SetLandCI(e.ID, *c.CI); err != nil {
			log.Printf("ledger: %v", err)
		}
	}
	s.landNext(e, now, ciPoll, ciPollMax, false)
}

// landBranch is the branch a landing is checked on: the one the approval
// names (work landed from a follow-on branch), else the entry's own.
func landBranch(e ledger.Entry) string {
	if e.Approval != nil && e.Approval.Branch != "" {
		return e.Approval.Branch
	}
	return e.Branch
}

// landedSHA is the commit whose CI says how the landing went: the newest
// base commit the work landed as, else the approved tip (a fast-forward
// push lands the commits as they are).
func landedSHA(e ledger.Entry) string {
	if l := e.LandCheck; l != nil {
		if n := len(l.Landed); n > 0 {
			return l.Landed[n-1]
		}
	}
	if e.Approval != nil {
		return e.Approval.Tip
	}
	return ""
}

// landSignals are what the hub can see of the worker while it judges a
// landing that is not on the base.
type landSignals struct {
	Now     time.Time
	Present bool              // the worker's agent is in the fleet
	Status  herdr.AgentStatus // its pane's status
	// BgKnown: the session says whether tasks run in the background; Bg is
	// how many, BgDesc the first one's description.
	BgKnown bool
	Bg      int
	BgDesc  string
}

func (s *Server) landSignals(ctx context.Context, e ledger.Entry, now time.Time) landSignals {
	sg := landSignals{Now: now}
	for _, a := range s.fleet.Agents() {
		if a.ID == e.AgentID {
			sg.Present, sg.Status = true, a.Status
			break
		}
	}
	if sg.Present {
		tasks, known := s.backgroundOf(ctx, e, true)
		sg.BgKnown, sg.Bg = known, len(tasks)
		if len(tasks) > 0 {
			sg.BgDesc = tasks[0].Desc
		}
	}
	return sg
}

// landFailure decides whether a landing whose work is not on the base has
// failed, and says why in the worker's terms (or, when it has not, what the
// hub is waiting for). A landing fails only when the worker has said it is
// done (its report) or stopped (it went idle after working), a grace has
// passed, and nothing is running: its pane is not working or blocked and,
// where the session can say so, no background task is open. A progress note
// newer than the report says it is going on. The hub never fails a landing
// merely because a check ran at a bad moment.
func landFailure(e ledger.Entry, sg landSignals) (failed bool, why string) {
	switch {
	case e.FinishedAt == nil:
		return false, waitingOn(sg, "the worker has not reported")
	case e.Progress != nil && e.Progress.At.After(*e.FinishedAt):
		return false, "the worker is still landing: " + progressLabel(e.Progress)
	case sg.Now.Sub(*e.FinishedAt) < landGrace:
		return false, "the worker reported; checking again"
	case !sg.Present:
		return true, "the worker's session is gone and its work is not on the base"
	case sg.Status == herdr.StatusWorking || sg.Status == herdr.StatusBlocked:
		return false, "the worker is still " + string(sg.Status)
	case !sg.BgKnown && e.ReportSource != "explicit":
		return false, "the worker is idle; its background tasks cannot be read"
	case sg.Bg > 0:
		return false, "waiting on a background task" + parenthesized(sg.BgDesc)
	case e.ReportSource == "explicit":
		return true, "the worker said it was done, but its work is not on the base"
	}
	return true, "the worker stopped, and its work is not on the base"
}

func waitingOn(sg landSignals, def string) string {
	switch {
	case !sg.Present:
		return def
	case sg.Status == herdr.StatusWorking:
		return "the worker is working"
	case sg.Bg > 0:
		return "waiting on a background task" + parenthesized(sg.BgDesc)
	}
	return def
}

// progressLabel is a progress note as one phrase.
func progressLabel(p *ledger.Progress) string {
	if p.Phase != "" {
		return map[string]string{"rebase": "rebasing", "tests": "tests running", "push": "pushing", "ci": "pushed, CI running"}[p.Phase]
	}
	return clip(p.Text, 80)
}

// landingChecks runs the removal checks for a landing, with the approved
// tip as a commit that must be in the base too.
func (s *Server) landingChecks(ctx context.Context, e ledger.Entry) *removal {
	base := e.Base
	opts := checkOpts{}.forEntry(e)
	if a := e.Approval; a != nil {
		if a.Base != "" {
			base = a.Base
		}
		if a.Tip != "" {
			opts.Also = []string{a.Tip}
		}
	}
	it := &removal{ID: e.AgentID, Host: e.Host, EntryID: e.ID, Repo: e.RepoName, Branch: landBranch(e), Base: base,
		Worktree: e.Worktree, Warnings: []string{}, Notes: []string{}, opts: opts}
	s.checkRemoval(ctx, it)
	return it
}

// landingVerdict reads a landing's checks: OK only when they ran, found
// the work (in its worktree, or recorded in the repository), and nothing
// is outside the base.
func landingVerdict(it *removal) ledger.LandCheck {
	lc := ledger.LandCheck{At: time.Now().UTC(), Verdict: checkVerdict(it), Unmerged: -1, Unpushed: -1}
	c := it.Checks
	if c == nil {
		return lc
	}
	lc.Base = c.BaseUsed
	lc.Unmerged = c.Unmerged
	if c.Detail != nil {
		lc.Unpushed = c.Detail.Unpushed
	}
	lc.OK = it.ChecksError == "" && len(it.Warnings) == 0 && c.Error == "" && c.Unmerged == 0 && !c.Primary && (c.Exists || c.Gone)
	for _, l := range c.Landed {
		if l.As != "" {
			lc.Landed = append(lc.Landed, l.As)
		}
	}
	return lc
}

// bgCache is an agent's background tasks as last read.
type bgCache struct {
	tasks []transcript.BackgroundTask
	known bool
	at    time.Time
	busy  bool
}

const bgTTL = 10 * time.Second

// backgroundOf says what a worker has running in the background. With
// wait it reads the session now (a decision rests on it); without, it
// returns what was read last and refreshes it in the background when it is
// older than bgTTL (a page asks every few seconds). known is false when the
// session cannot say (another agent kind, or it could not be read).
func (s *Server) backgroundOf(ctx context.Context, e ledger.Entry, wait bool) (tasks []transcript.BackgroundTask, known bool) {
	if e.AgentID == "" || e.PaneID == "" {
		return nil, false
	}
	read := func(ctx context.Context) {
		l := s.latestOf(ctx, e.Host, e.PaneID)
		c := bgCache{at: time.Now()}
		if l != nil {
			c.tasks, c.known = l.Background, l.BackgroundKnown
		}
		s.coord.mu.Lock()
		s.coord.bg[e.AgentID] = c
		s.coord.mu.Unlock()
	}
	s.coord.mu.Lock()
	c, ok := s.coord.bg[e.AgentID]
	stale := !ok || time.Since(c.at) > bgTTL
	refresh := stale && !c.busy
	if refresh && !wait {
		c.busy = true
		s.coord.bg[e.AgentID] = c
	}
	s.coord.mu.Unlock()
	switch {
	case wait && stale:
		read(ctx)
	case refresh:
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			read(ctx)
		}()
	}
	s.coord.mu.Lock()
	c = s.coord.bg[e.AgentID]
	s.coord.mu.Unlock()
	return c.tasks, c.known
}

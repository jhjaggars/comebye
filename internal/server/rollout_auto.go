package server

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// The automatic rollout (issue #141, slice 1).
//
// The release itself stays the user's tap: they approve the item, and the
// app tags it. Once the tag's Release workflow is green, the hub starts the
// same rollout "Roll out to all" starts (every peer behind, then the hub
// wave), with nobody at the page. The setting auto_rollout (settings.json,
// on unless switched off) turns it off; Roll out to all then stays the way.
//
// It starts once per release item. The item carries the mark
// (TodoRelease.AutoRollout: the job's id, or why there was nothing to do),
// and the job on file names its item too, so a hub that restarts anywhere
// between the tag and the mark neither misses the release nor starts it
// twice: the loop only reads what is saved.
//
// An automatic rollout does not wake a stopped worker: it is in the plan
// already skipped, and the catch-up upgrades it when it next starts.

const (
	// autoRolloutEvery: how often the release loop's auto step looks for a
	// release to roll out (-test-mode: autoRolloutTestEvery).
	autoRolloutEvery     = time.Minute
	autoRolloutTestEvery = 2 * time.Second
	// autoRolloutWindow: a release tagged longer ago than this is left to
	// Roll out to all (an old item the hub sees after an upgrade, say).
	autoRolloutWindow = 24 * time.Hour
	autoRolloutBy     = "comebye (automatic rollout)"
)

// autoRolloutOn: the setting in force. Unset is on, except under
// -test-mode: the browser tests tap Roll out to all themselves, and a hub
// that rolled out by itself would race them (a test that wants it sets it).
func (s *Server) autoRolloutOn() bool {
	if v := s.settings.get().AutoRollout; v != nil {
		return *v
	}
	return !s.cfg.TestMode
}

// autoCandidate is the newest open, tagged, unmarked release item inside the
// window, if any.
func (s *Server) autoCandidate(now time.Time) (Todo, bool) {
	var best Todo
	found := false
	for _, it := range s.todos.list("") {
		r := it.Release
		if r == nil || r.State != relTagged || r.Dismissed || r.AutoRollout != "" || r.ApprovedBy == "" {
			continue
		}
		if r.TaggedAt == nil || now.Sub(*r.TaggedAt) > autoRolloutWindow {
			continue
		}
		if !found || newerVersion(r.Version, best.Release.Version) {
			best, found = it, true
		}
	}
	return best, found
}

// autoRolloutTick does the one next thing: nothing, mark the item, or start.
// It reports what it did, for the tests.
func (s *Server) autoRolloutTick(ctx context.Context) string {
	if s.rollout == nil || s.cfg.Release.Tool == nil || !s.autoRolloutOn() {
		return "off"
	}
	it, ok := s.autoCandidate(time.Now())
	if !ok {
		return "none"
	}
	cur, has := s.rollout.Get()
	if has && cur.Todo == it.ID {
		// Started already (by hand, or by this loop before a restart that
		// came between the start and the mark).
		s.markAuto(it.ID, cur.ID)
		return "marked " + cur.ID
	}
	stale := has && cur.State == roStopped && newerVersion(it.Release.Version, cur.Version)
	if has && cur.live() && !stale {
		return "busy" // another rollout is live: this one waits for it
	}
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	bd, err := s.relBuild(cctx, it, relBuildTTL)
	if err != nil {
		return "build unknown: " + err.Error()
	}
	if bd.State != "success" {
		return "build " + bd.State // red, or still running: no rollout
	}
	if stale {
		// A rollout of an older release that stopped would hold this one back
		// for good: it is superseded (the newer one upgrades the same hosts,
		// through the same gates), and its release item is closed for it.
		if _, err := s.rollout.Supersede(cur.ID, it.Release.Version, "comebye"); err != nil {
			return "busy"
		}
		s.supersedeItem(cur, it.Release.Version)
	}
	hosts := s.rollout.ops.Hosts(cctx, it.Release.Version)
	s.markWakeable(cctx, hosts)
	dst, derr := s.rollout.ops.DeployStatus(cctx)
	if derr == nil && dst.Record != nil && dst.Record.State == depRunning {
		return "deploy running"
	}
	plan, err := rolloutPlan(hosts, nil, s.canaryName(), true, hubDeploys(hosts, dst, derr, s.cfg.Release.Host != ""))
	if err != nil {
		s.markAuto(it.ID, "none: "+oneLine(err.Error(), 120))
		return "bad plan"
	}
	plan = skipStopped(plan, hosts)
	if !actionable(plan) {
		s.markAuto(it.ID, "none: nothing to upgrade")
		return "nothing to do"
	}
	j, err := s.rollout.StartAuto(it.ID, it.Release.Version, it.Release.Commit, autoRolloutBy, plan)
	switch {
	case errors.Is(err, errRolloutSame):
		s.markAuto(it.ID, j.ID)
		return "marked " + j.ID
	case errors.Is(err, errRolloutActive):
		return "busy"
	case err != nil:
		log.Printf("auto rollout: %s: %v", it.Release.Version, err)
		return "error: " + err.Error()
	}
	s.markAuto(it.ID, j.ID)
	s.auditBy(ledger.AuditEvent{By: "comebye", Action: "rollout.auto", Todo: it.ID, OK: true,
		Detail: it.Release.Version + ": approved by " + oneLine(it.Release.ApprovedBy, 80) + ", build green: started " + j.ID})
	s.closeOlderUnstarted(it)
	return "started " + j.ID
}

// supersedeItem closes the release item of a rollout a newer release's
// superseded, and its rollout alarm with it: the newer release is the one
// to watch now, and its own stop, if any, files its own.
func (s *Server) supersedeItem(old Rollout, version string) {
	if s.todos == nil || old.Todo == "" {
		return
	}
	note := rolloutNoteOf(Rollout{State: roCancelled, Version: old.Version, SupersededBy: version})
	closed, err := s.todos.mutate(old.Todo, func(t *Todo) error {
		if !t.active() {
			return errTodoClosed
		}
		now := time.Now().UTC()
		t.State, t.ClosedBy, t.ClosedAt, t.Notice = TodoDone, "comebye", &now, nil
		if t.Release != nil {
			rel := *t.Release
			rel.RolloutNote = note
			t.Release = &rel
		}
		return nil
	})
	if err == nil {
		s.auditBy(ledger.AuditEvent{By: "comebye", Todo: closed.ID, Action: "release.superseded", OK: true,
			Detail: old.Version + ": its rollout " + old.ID + " stopped; superseded by " + version + ": the item was closed"})
	}
	prefix := SourceRollout + ":" + old.Todo + ":"
	for _, a := range s.todos.list("") {
		if !a.active() || a.Source != SourceRollout || !strings.HasPrefix(a.AutoKey, prefix) {
			continue
		}
		if _, err := s.todos.close(a.ID, TodoDone, AutoCreator, "superseded by "+version); err != nil && !errors.Is(err, errTodoClosed) {
			log.Printf("auto rollout: close alarm %s: %v", a.ID, err)
		}
	}
}

// closeOlderUnstarted closes every older release item that never started a
// rollout, now that a newer release's rollout (it) has started: an older
// release the hub never got to (a stopped rollout held it back, then a newer
// one superseded that) would otherwise stay open for good, since nothing
// rolls it out any more and the hosts move past it. Its note says what
// replaced it (release.superseded). An item whose rollout ever started is
// left to its own close (sweepReleasesWith, supersedeItem), and so is one
// being tagged right now.
func (s *Server) closeOlderUnstarted(it Todo) {
	if s.todos == nil || it.Release == nil {
		return
	}
	v := it.Release.Version
	for _, o := range s.todos.list("") {
		r := o.Release
		if r == nil || o.ID == it.ID || !o.active() || r.Dismissed || !newerVersion(v, r.Version) || s.tagInFlight(o.ID) {
			continue
		}
		if r.AutoRollout != "" && !strings.HasPrefix(r.AutoRollout, "none") {
			continue // the automatic rollout started one
		}
		if s.rolloutEverStarted(o.ID) {
			continue
		}
		note := r.Version + " was never rolled out: superseded by " + v + ", which rolls out instead"
		closed, err := s.todos.mutate(o.ID, func(t *Todo) error {
			if !t.active() {
				return errTodoClosed
			}
			now := time.Now().UTC()
			t.State, t.ClosedBy, t.ClosedAt, t.Notice = TodoDone, "comebye", &now, nil
			if t.Release != nil {
				rel := *t.Release
				rel.RolloutNote = note
				t.Release = &rel
			}
			return nil
		})
		if err != nil {
			continue
		}
		s.auditBy(ledger.AuditEvent{By: "comebye", Todo: closed.ID, Action: "release.superseded", OK: true,
			Detail: r.Version + ": no rollout had started; superseded by " + v + ": the item was closed"})
	}
}

// rolloutEverStarted: the audit log has a rollout.start for the item.
func (s *Server) rolloutEverStarted(id string) bool {
	if s.ledger == nil {
		return false
	}
	for _, ev := range s.ledger.AuditLog(id) {
		if ev.Action == "rollout.start" {
			return true
		}
	}
	return false
}

// markAuto records on the release item that its automatic rollout is
// settled: the job's id, or "none: why".
func (s *Server) markAuto(id, mark string) {
	_, err := s.todos.mutate(id, func(t *Todo) error {
		if t.Release == nil || t.Release.AutoRollout == mark {
			return errNoChange
		}
		rel := *t.Release
		rel.AutoRollout = mark
		t.Release = &rel
		return nil
	})
	if err != nil && err != errNoChange {
		log.Printf("auto rollout: mark %s: %v", id, err)
	}
}

// skipStopped puts a stopped provisioned worker in the plan already skipped,
// to be caught up when it next starts: an automatic rollout wakes nobody.
func skipStopped(plan []RolloutStep, hosts []relHost) []RolloutStep {
	for i := range plan {
		st := &plan[i]
		if st.Phase != roPending || st.Wave == waveHub {
			continue
		}
		if h, ok := findHost(hosts, st.Host); ok && (h.Stopped || h.WakeNote != "") {
			st.Phase, st.CatchUp, st.Detail = roSkipped, true, "stopped: an automatic rollout does not start it; it is upgraded when it next starts"
		}
	}
	return plan
}

// The catch-up (issue #141, slice 4). A host that was stopped or offline
// while its release rolled out is upgraded when it is back, with no time
// limit, while the automatic rollout is on; and so is a reachable host that
// came up on an older version (a worker made at the old version). It does
// not depend on the rollout having had a job: a release where every running
// host was already current ("none: nothing to upgrade") still catches its
// stopped hosts up, through a job started then.

// autoTarget is the newest tagged release item the automatic rollout has
// settled (marked), open or closed: the version hosts catch up to.
func (s *Server) autoTarget() (Todo, bool) {
	var best Todo
	found := false
	for _, it := range s.todos.list("all") {
		r := it.Release
		if r == nil || r.State != relTagged || r.Dismissed || r.AutoRollout == "" {
			continue
		}
		if !found || newerVersion(r.Version, best.Release.Version) {
			best, found = it, true
		}
	}
	return best, found
}

// catchUpPlan is the automatic rollout's say in the engine's one catch-up
// (rolloutEngine.catchUp) for the hosts behind the newest auto-rolled
// release: the job's own catch-up, that and any reachable host behind it
// (the job is that release's), nothing, or a job of its own when that
// release has none.
func (s *Server) catchUpPlan(cur Rollout, has bool) catchUpPlan {
	if s.rollout == nil || s.cfg.Release.Tool == nil || !s.autoRolloutOn() {
		return ownCatchUp(cur, has)
	}
	it, ok := s.autoTarget()
	switch {
	case !ok:
		return ownCatchUp(cur, has)
	case has && cur.live():
		return catchUpPlan{Why: "busy"}
	case has && cur.Todo != it.ID && !newerVersion(it.Release.Version, cur.Version):
		return ownCatchUp(cur, has) // the job on file is for this version or a newer one
	case has && cur.Todo == it.ID && cur.State != roDone:
		return catchUpPlan{Why: "none"} // cancelled: the user's call stands
	case has && cur.Todo == it.ID:
		return catchUpPlan{Reopen: true, AddNew: true}
	}
	return catchUpPlan{Start: func(ctx context.Context) string { return s.startCatchUp(ctx, it) }}
}

// startCatchUp starts a job that catches up the hosts behind the release
// item it, which has no job of its own (or an older release's is on file):
// every reachable host behind it, through the same gates. It reports what
// it did, for the tests.
func (s *Server) startCatchUp(ctx context.Context, it Todo) string {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	v := it.Release.Version
	hosts := s.rollout.ops.Hosts(cctx, v)
	behind := false
	for _, h := range hosts {
		behind = behind || (!h.Hub && !h.WithHub && !h.Current && hostUp(h) && h.Enabled)
	}
	if !behind {
		return "current"
	}
	bd, err := s.relBuild(cctx, it, relBuildTTL)
	if err != nil || bd.State != "success" {
		return "build not green"
	}
	s.markWakeable(cctx, hosts)
	dst, derr := s.rollout.ops.DeployStatus(cctx)
	if derr == nil && dst.Record != nil && dst.Record.State == depRunning {
		return "deploy running"
	}
	plan, err := rolloutPlan(hosts, nil, s.canaryName(), true, hubDeploys(hosts, dst, derr, s.cfg.Release.Host != ""))
	if err != nil {
		return "bad plan"
	}
	plan = skipStopped(plan, hosts)
	if !actionable(plan) {
		return "current"
	}
	j, err := s.rollout.StartCatchUp(it.ID, v, it.Release.Commit, autoRolloutBy, plan)
	if err != nil {
		return "error: " + err.Error()
	}
	s.auditBy(ledger.AuditEvent{By: "comebye", Action: "rollout.auto", Todo: it.ID, OK: true,
		Detail: v + ": a host is behind it: started " + j.ID + " to catch it up"})
	return "started " + j.ID
}

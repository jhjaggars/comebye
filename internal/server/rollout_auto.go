package server

import (
	"context"
	"errors"
	"log"
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
	autoRolloutEvery = time.Minute
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

// RunAutoRollout looks for a release to roll out every minute until ctx
// ends. Only the ledger host with a release tool has one to start.
func (s *Server) RunAutoRollout(ctx context.Context) {
	if s.rollout == nil || s.todos == nil || s.fleet == nil {
		return
	}
	every := autoRolloutEvery
	if s.cfg.TestMode {
		every = 2 * time.Second
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
						log.Printf("auto rollout: %v", r)
					}
				}()
				s.autoRolloutTick(ctx)
			}()
		}
	}
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
	if has && cur.live() {
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
	hosts := s.rollout.ops.Hosts(cctx, it.Release.Version)
	s.markWakeable(cctx, hosts)
	dst, derr := s.rollout.ops.DeployStatus(cctx)
	if derr == nil && dst.Record != nil && dst.Record.State == depRunning {
		return "deploy running"
	}
	plan, err := rolloutPlan(hosts, nil, s.cfg.Release.Canary, true, hubDeploys(hosts, dst, derr, s.cfg.Release.Host != ""))
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
	return "started " + j.ID
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

package server

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/park"
)

// The sweep loop's clean-up (see sweeploop.go): remove a marked session when the
// hub itself proves it has nothing to lose. Nothing here relies on a cached or
// earlier result: right before a session is removed it is reviewed again on its
// host (the same review the removals review runs, with the comparison against
// origin), so a session that changed since it was marked (a new commit, a dirty
// tree, an unpushed commit) is not removed.

const sweepCleanReason = "nothing to lose: clean, every commit in the default branch, nothing unpushed (cleaned up by the sweep)"

// sweepCleanPrefilter keeps the marked sessions the loop may look at: a ledger
// worker that is not a coordinator, not a proposal to discard work, marked long
// enough ago for the ask and the automatic removal to have had their turn, and
// not waiting on the session's answer.
func (s *Server) sweepCleanPrefilter(now time.Time) []*removal {
	var out []*removal
	for _, it := range s.marked() {
		m := it.Mark
		if it.EntryID == "" || it.Coordinator || m.Discard || now.Sub(m.At) < s.sweeps.grace {
			continue
		}
		if m.Ask != nil && m.Ask.State == ledger.AskAsking {
			continue
		}
		out = append(out, it)
	}
	return out
}

// sweepCleanVerdict says whether a reviewed session (review has run, with the
// origin check asked for) may be removed by the loop; when not, why not. A
// session whose agent is still there needs its SAFE answer, like the automatic
// removal; one whose agent is gone has nobody to ask, so the hub's own proof
// decides (workProven), and a LEFT answer that is not fully resolved keeps it.
func sweepCleanVerdict(it *removal, f autoFacts) (bool, string) {
	m := it.Mark
	switch {
	case it.EntryID == "":
		return false, "not a handoff worker"
	case it.Coordinator:
		return false, "it is a coordinator"
	case m.Discard || it.DeliveredDiscard:
		return false, "the mark proposes discarding work"
	case sessionBusy(it):
		return false, "the agent is " + string(it.Live.Status)
	case f.Background > 0:
		return false, "it has a background task running"
	case f.TodoWaiting:
		return false, "a to-do is waiting on it"
	case f.Queued > 0:
		return false, "it has queued prompts"
	case it.Live.Present:
		return autoRemoveVerdict(it, f)
	case m.Ask != nil && (m.Ask.State == ledger.AskAsking || m.Ask.ReaskedAt != nil || m.Ask.CleaningAt != nil):
		return false, "the session is being asked"
	case m.Ask != nil && m.Ask.State == ledger.AskUnsafe:
		return false, "the session listed something left that is not settled"
	}
	return workProven(it)
}

// sweepCleanStep removes what has nothing to lose. forced: a sweep just ran, so
// look now rather than at the next sweepCleanLook.
func (s *Server) sweepCleanStep(ctx context.Context, now time.Time, forced bool) []sweepDone {
	l := s.sweeps
	st := l.get()
	if !forced && now.Sub(st.CleanLook) < l.look {
		return nil
	}
	l.mu.Lock()
	l.st.CleanLook = now
	l.saveLocked()
	l.mu.Unlock()
	cands := s.sweepCleanPrefilter(now)
	if len(cands) == 0 {
		return nil
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		done []sweepDone
	)
	sem := make(chan struct{}, 3)
	for _, it := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if d := s.sweepCleanOne(ctx, it, now); d != nil {
				mu.Lock()
				done = append(done, *d)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	s.recordDone(now, done)
	return done
}

// sweepCleanOne reviews one session again on its host and removes it when
// nothing is left to lose.
func (s *Server) sweepCleanOne(ctx context.Context, it *removal, now time.Time) *sweepDone {
	key := "autorm/" + it.ID // shared with the automatic removal: one at a time
	s.coord.mu.Lock()
	if s.coord.checking[key] {
		s.coord.mu.Unlock()
		return nil
	}
	s.coord.checking[key] = true
	s.coord.mu.Unlock()
	defer func() {
		s.coord.mu.Lock()
		delete(s.coord.checking, key)
		s.coord.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, removeTimeout+90*time.Second)
	defer cancel()
	it.opts.Origin = true
	s.review(ctx, it)
	if ok, _ := sweepCleanVerdict(it, s.autoFactsOf(it)); !ok {
		return nil
	}
	// The mark may have been withdrawn or replaced while the checks ran.
	if !s.markCurrent(it.Mark) {
		return nil
	}
	how := inBaseHow(it.Checks)
	tip := ""
	if it.Checks != nil {
		tip = it.Checks.Tip
	}
	if it.Live.Present && it.Checks.Origin != nil && it.Checks.Origin.Records {
		it.recordPark = park.WhyAutoRemoved // a live session can be resumed from Recently closed
	}
	o := s.confirmOne(ctx, it, false, "")
	label := orDefault(it.Name, it.EntryID)
	if !o.OK {
		log.Printf("sweep clean of %s: %s: %s", it.ID, o.Code, o.Message)
		s.auditBy(ledger.AuditEvent{By: AutoCreator, Action: "worker.sweep_remove", Host: it.Host, OK: false,
			Detail: fmt.Sprintf("%s (%s): refused: %s %s", label, it.EntryID, o.Code, o.Message)})
		return nil
	}
	rec := ledger.AutoRemoval{At: now.UTC(), Reason: sweepCleanReason, Landed: how}
	if o.Result != nil {
		rec.WorktreeRemoved, rec.Kept, rec.ParkID = o.Result.WorktreeRemoved, o.Result.Kept, o.Result.ParkID
	}
	if _, _, err := s.ledger.Update(it.EntryID, func(e *ledger.Entry) { e.AutoRemoved = &rec }); err != nil {
		log.Printf("ledger: %v", err)
	}
	detail := fmt.Sprintf("%s (%s, branch %s at %s): %s; %s", label, it.EntryID, it.Branch, tip, sweepCleanReason, how)
	if !rec.WorktreeRemoved {
		detail += "; the worktree was kept: " + rec.Kept
	}
	s.auditBy(ledger.AuditEvent{By: AutoCreator, Action: "worker.sweep_remove", Host: it.Host, OK: true, Detail: detail})
	s.queueLine("autoremove", it.EntryID, "autorm/"+it.EntryID, "removed "+safeText(label)+" automatically (the sweep): "+sweepCleanReason+".")
	return &sweepDone{At: now.UTC(), Kind: doneSession, Name: label, Repo: it.Repo, Host: it.Host, Branch: it.Branch, Tip: tip, Note: rec.Kept}
}

// sweepCloseOldItem closes the To do item an earlier version of the loop filed
// ("N sessions are ready to remove"): the loop cleans up itself now and files none.
func (s *Server) sweepCloseOldItem() {
	if s.todos == nil {
		return
	}
	for _, t := range s.todos.list("") {
		if t.Source == SourceSweep && t.active() {
			if _, err := s.todos.close(t.ID, TodoDropped, AutoCreator, "the hub now cleans up safe sessions by itself and files no item for it"); err != nil {
				log.Printf("sweep loop: close %s: %v", t.ID, err)
			}
		}
	}
}

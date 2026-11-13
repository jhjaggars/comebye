package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// Automatic removal. A worker that is marked ready to remove (by its
// coordinator or by a sweep) and whose every check is clean is removed by the
// hub without the user's tap, so the removals review shows only the ones that
// need them. "Clean" is decided here and nowhere else (autoRemoveVerdict) and
// is rechecked at removal time:
//
//   - the work is in the base: every branch commit has an equivalent on
//     origin/<default> (git cherry, after a fetch that worked; a checkout's
//     own, possibly stale, main is not trusted), or the branch has no commits
//     (a research / Page-only handoff), or its pull request was merged;
//   - nothing is unpushed, and the worktree has no uncommitted or untracked
//     files (ignored files do not count);
//   - the session answered SAFE (not "did not answer", UNSAFE or unchecked);
//   - no open to-do waits on the agent and it has no queued prompts;
//   - it is not a coordinator, and is not working, blocked or running a
//     background task.
//
// Anything else stays in the review exactly as before. The removal itself is
// the ordinary one (removeSession), which keeps the worktree whenever anything
// is unmerged and never deletes a branch. Each auto-removal is audited,
// reported to the coordinator in the digest, and listed in the review for a
// day with an Undo (autoremoveundo: the branch is still there, so the worktree
// is recreated and a fresh worker started on it).

const (
	// autoRemoveEvery is how often one marked worker is looked at again.
	autoRemoveEvery = 3 * time.Minute
	// AutoRemoveWindow is how long the review keeps showing an auto-removal.
	AutoRemoveWindow = 24 * time.Hour
	autoRemoveReason = "landed, clean, SAFE"
)

// autoFacts are what the verdict needs from outside the removal.
type autoFacts struct {
	// TodoWaiting: an open to-do waits on the agent. Queued: prompts queued
	// for it. Background: tasks it runs in the background.
	TodoWaiting bool
	Queued      int
	Background  int
}

// autoRemoveVerdict says whether a reviewed removal (review has run, with the
// origin check asked for) may be removed without the user's tap; when not,
// why not. It is the single place the rule lives.
func autoRemoveVerdict(it *removal, f autoFacts) (ok bool, why string) {
	c, m := it.Checks, it.Mark
	switch {
	case it.EntryID == "":
		return false, "not a handoff worker"
	case it.Coordinator:
		return false, "it is a coordinator"
	case it.Live.Present && (it.Live.Status == herdr.StatusWorking || it.Live.Status == herdr.StatusBlocked):
		return false, "the agent is " + string(it.Live.Status)
	case f.Background > 0:
		return false, "it has a background task running"
	case f.TodoWaiting:
		return false, "a to-do is waiting on it"
	case f.Queued > 0:
		return false, "it has queued prompts"
	case m.Discard:
		return false, "the mark proposes discarding the work"
	case m.Ask == nil || m.Ask.State != ledger.AskSafe:
		return false, "the session has not answered SAFE"
	case m.Ask.ReaskedAt != nil:
		return false, "the session is being asked again"
	case it.Worktree == "" || it.ChecksError != "" || c == nil:
		return false, "its worktree could not be checked"
	case !c.Exists || c.Primary || c.Error != "" || c.InProgress != "":
		return false, "its worktree is not in a state to remove"
	case c.Dirty || c.DirtyFiles > 0:
		return false, "the worktree has uncommitted or untracked files"
	case len(it.Warnings) > 0:
		return false, it.Warnings[0]
	}
	o := c.Origin
	switch {
	case o == nil:
		return false, "the host did not compare with origin"
	case !o.Fetched || o.Ref == "":
		return false, "not compared with origin: " + o.Why
	case o.Unmerged < 0:
		return false, "could not tell whether its commits are in " + o.Ref
	case o.Unmerged > 0 && o.MergedPR == 0:
		return false, fmt.Sprintf("%d commits not in %s", o.Unmerged, o.Ref)
	}
	d := c.Detail
	switch {
	case d == nil || d.Unpushed < 0:
		return false, "could not tell whether anything is unpushed"
	case d.Unpushed > 0 && o.MergedPR == 0:
		return false, fmt.Sprintf("%d commits on no remote", d.Unpushed)
	}
	return true, ""
}

// inBaseHow says how the work is in the base, for the audit entry.
func inBaseHow(c *worktreeChecks) string {
	o := c.Origin
	switch {
	case o == nil:
		return ""
	case o.Unmerged == 0 && len(c.Landed) == 0:
		return "no commits beyond " + o.Ref
	case o.Unmerged == 0:
		return "every commit is in " + o.Ref
	case o.MergedPR > 0:
		return fmt.Sprintf("merged as PR #%d", o.MergedPR)
	}
	return ""
}

// autoFactsOf gathers the outside facts about an agent id.
func (s *Server) autoFactsOf(it *removal) autoFacts {
	var f autoFacts
	if s.todos != nil {
		for _, t := range s.todos.list("") {
			if t.active() && t.Waiting != nil && t.Waiting.ID == it.ID {
				f.TodoWaiting = true
			}
		}
	}
	if s.pq != nil {
		f.Queued = len(s.pq.list(it.ID, false))
	}
	for _, a := range s.fleet.Agents() {
		if a.ID == it.ID || (a.Host == it.Host && a.PaneID == it.PaneID && it.PaneID != "") {
			f.Background = len(s.backgroundOfAgent(a))
		}
	}
	return f
}

// autoRemoveCandidate is the cheap filter before any git runs: a ledger
// worker marked with a SAFE answer.
func autoRemoveCandidate(it *removal) bool {
	m := it.Mark
	return it.EntryID != "" && !m.Discard && m.Ask != nil && m.Ask.State == ledger.AskSafe &&
		!it.Coordinator && !(it.Live.Present && (it.Live.Status == herdr.StatusWorking || it.Live.Status == herdr.StatusBlocked))
}

// autoRemoveStep looks at every marked worker once and removes the clean
// ones; it returns the ids removed. Each worker is looked at at most once per
// autoRemoveEvery.
func (s *Server) autoRemoveStep(ctx context.Context, now time.Time) []string {
	if !s.autoRemove || s.ledger == nil {
		return nil
	}
	var removed []string
	for _, it := range s.marked() {
		if !autoRemoveCandidate(it) {
			continue
		}
		key := "autorm/" + it.ID
		s.coord.mu.Lock()
		last, seen := s.coord.calling[key]
		if (seen && now.Sub(last) < autoRemoveEvery) || s.coord.checking[key] {
			s.coord.mu.Unlock()
			continue
		}
		s.coord.calling[key] = now
		s.coord.checking[key] = true
		s.coord.mu.Unlock()
		done := s.autoRemoveOne(ctx, it)
		s.coord.mu.Lock()
		delete(s.coord.checking, key)
		s.coord.mu.Unlock()
		if done {
			removed = append(removed, it.ID)
		}
	}
	return removed
}

// autoRemoveOne reviews one marked worker and removes it when it is clean.
func (s *Server) autoRemoveOne(ctx context.Context, it *removal) bool {
	ctx, cancel := context.WithTimeout(ctx, removeTimeout+90*time.Second)
	defer cancel()
	it.opts.Origin = true
	s.review(ctx, it)
	if ok, _ := autoRemoveVerdict(it, s.autoFactsOf(it)); !ok {
		return false
	}
	// The mark may have been withdrawn or replaced while the checks ran.
	if !s.markCurrent(it.Mark) {
		return false
	}
	how := inBaseHow(it.Checks)
	o := s.confirmOne(ctx, it, false, "")
	if !o.OK {
		log.Printf("auto remove of %s: %s: %s", it.ID, o.Code, o.Message)
		s.auditBy(ledger.AuditEvent{By: AutoCreator, Action: "worker.auto_remove", Host: it.Host, OK: false,
			Detail: fmt.Sprintf("%s (%s): refused: %s %s", it.Name, it.EntryID, o.Code, o.Message)})
		return false
	}
	rec := ledger.AutoRemoval{At: time.Now().UTC(), Reason: autoRemoveReason, Landed: how}
	if o.Result != nil {
		rec.WorktreeRemoved, rec.Kept = o.Result.WorktreeRemoved, o.Result.Kept
	}
	if _, _, err := s.ledger.Update(it.EntryID, func(e *ledger.Entry) { e.AutoRemoved = &rec }); err != nil {
		log.Printf("ledger: %v", err)
	}
	label := orDefault(it.Name, it.EntryID)
	detail := fmt.Sprintf("%s (%s, branch %s): %s; %s", label, it.EntryID, it.Branch, autoRemoveReason, how)
	if !rec.WorktreeRemoved {
		detail += "; the worktree was kept: " + rec.Kept
	}
	s.auditBy(ledger.AuditEvent{By: AutoCreator, Action: "worker.auto_remove", Host: it.Host, OK: true, Detail: detail})
	line := fmt.Sprintf("removed %s automatically: %s", safeText(label), autoRemoveReason)
	if !rec.WorktreeRemoved {
		line = fmt.Sprintf("closed %s automatically (%s) but kept its worktree: %s", safeText(label), autoRemoveReason, safeText(rec.Kept))
	}
	s.queueLine("autoremove", it.EntryID, "autorm/"+it.EntryID, line+".")
	return true
}

// autoRemovedView is one recent auto-removal as the review lists it.
type autoRemovedView struct {
	EntryID         string     `json:"entry_id"`
	Host            string     `json:"host"`
	Name            string     `json:"name,omitempty"`
	Repo            string     `json:"repo,omitempty"`
	Branch          string     `json:"branch,omitempty"`
	At              time.Time  `json:"at"`
	Reason          string     `json:"reason"`
	Landed          string     `json:"landed,omitempty"`
	WorktreeRemoved bool       `json:"worktree_removed"`
	Kept            string     `json:"kept,omitempty"`
	UndoneAt        *time.Time `json:"undone_at,omitempty"`
	UndoNote        string     `json:"undo_note,omitempty"`
	// CanUndo: the branch can be set up again (a repository and a branch
	// are recorded, the host is known, and it was not undone already).
	CanUndo bool `json:"can_undo"`
}

// recentAutoRemovals lists the auto-removals of the last AutoRemoveWindow,
// newest first.
func (s *Server) recentAutoRemovals(now time.Time) []autoRemovedView {
	out := []autoRemovedView{}
	if s.ledger == nil {
		return out
	}
	for _, e := range s.ledger.Entries() {
		a := e.AutoRemoved
		if a == nil || e.Status != ledger.Removed || now.Sub(a.At) > AutoRemoveWindow {
			continue
		}
		out = append(out, autoRemovedView{
			EntryID: e.ID, Host: e.Host, Name: e.AgentName, Repo: e.RepoName, Branch: e.Branch, At: a.At,
			Reason: a.Reason, Landed: a.Landed, WorktreeRemoved: a.WorktreeRemoved, Kept: a.Kept,
			UndoneAt: a.UndoneAt, UndoNote: a.UndoNote,
			CanUndo: a.UndoneAt == nil && e.Repo != "" && e.Branch != "" && s.knownHost(e.Host),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// handleUndoAutoRemove starts a worker on the branch of an auto-removed one:
// POST {entry_id}. The removal kept the branch, so the worktree is created
// again (or reused when it was kept) and a fresh agent starts in it; the old
// conversation is not restored.
func (s *Server) handleUndoAutoRemove(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b struct {
		EntryID string `json:"entry_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil || b.EntryID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	e, ok := s.ledger.Get(b.EntryID)
	switch {
	case !ok || e.AutoRemoved == nil:
		relErr(w, http.StatusNotFound, "not_found", "that worker was not removed automatically")
		return
	case e.AutoRemoved.UndoneAt != nil:
		relErr(w, http.StatusConflict, "undone", "that removal was already undone")
		return
	case time.Since(e.AutoRemoved.At) > AutoRemoveWindow:
		relErr(w, http.StatusConflict, "too_old", "an automatic removal can be undone for a day")
		return
	case e.Repo == "" || e.Branch == "" || !s.knownHost(e.Host):
		relErr(w, http.StatusConflict, "not_feasible", "undo is not possible: the repository, the branch or the host is not known any more")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), removeTimeout)
	defer cancel()
	body := workerBody{Repo: e.Repo, Branch: e.Branch, Base: e.Base, Kind: e.Kind, Name: e.AgentName, Model: e.Model, Thinking: e.Thinking}
	res, lerr := s.workerOn(ctx, e.Host, body)
	if lerr != nil && lerr.code == "agent_name_taken" {
		body.Name = ""
		res, lerr = s.workerOn(ctx, e.Host, body)
	}
	if lerr != nil {
		relErr(w, lerr.status, "not_feasible", "undo is not possible: "+lerr.message)
		return
	}
	note := "a new worker started on branch " + res.Branch + " in " + res.Worktree + "; the old conversation is not restored"
	now := time.Now().UTC()
	if _, _, err := s.ledger.Update(e.ID, func(x *ledger.Entry) {
		if x.AutoRemoved != nil {
			x.AutoRemoved.UndoneAt, x.AutoRemoved.UndoNote = &now, note
		}
	}); err != nil {
		log.Printf("ledger: %v", err)
	}
	s.audit(r, ledger.AuditEvent{Action: "worker.auto_remove_undo", Host: e.Host, OK: true, Detail: e.AgentName + " (" + e.ID + "): " + note})
	writeJSON(w, map[string]any{"ok": true, "note": note, "agent": res.Agent, "worktree": res.Worktree})
}

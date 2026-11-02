package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// The sweep: find the ledger workers that look finished, check each on
// its own host, mark the ones with nothing to lose as ready to remove
// (the normal mark, so the session is asked and the user confirms in the
// removals review) and list the rest with why. It never removes anything.

// Sweep classes.
const (
	SweepReady     = "ready"      // marked (or would be, in a dry run)
	SweepNeedsLook = "needs_look" // not marked: a person or the coordinator decides
	SweepSkip      = "skip"       // not a candidate (already marked, a coordinator, a primary checkout…)
)

// defaultSweepIdle is how long a running worker's idle agent must have
// been idle to count as finished when it never reported.
const defaultSweepIdle = 30 * time.Minute

// sweepBody is the JSON body of a sweep.
type sweepBody struct {
	// DryRun lists without marking.
	DryRun bool `json:"dry_run"`
	// IdleAfter ("45m"): a worker still recorded as running whose agent
	// has been idle this long also counts. Default 30m; "0" turns it off.
	IdleAfter string `json:"idle_after"`
	// Notify tells a coordinator the outcome (never on a dry run).
	Notify bool `json:"notify"`
}

// sweepItem is one worker's outcome.
type sweepItem struct {
	ID      string `json:"id"` // the host-qualified agent id
	EntryID string `json:"entry_id,omitempty"`
	Host    string `json:"host"`
	Name    string `json:"name,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Class   string `json:"class"`
	// Reason is the mark's reason for ready items, else why it needs a
	// look or was skipped.
	Reason string `json:"reason"`
	Marked bool   `json:"marked,omitempty"`
}

// sweepResult is the sweep's answer.
type sweepResult struct {
	DryRun    bool        `json:"dry_run"`
	Ready     []sweepItem `json:"ready"`
	NeedsLook []sweepItem `json:"needs_look"`
	Skipped   []sweepItem `json:"skipped"`
	// Notified is who got the summary, when asked for and delivered.
	Notified string `json:"notified,omitempty"`
	// NotifyError says why a requested notice was not delivered.
	NotifyError string `json:"notify_error,omitempty"`
}

// sweepCandidate is a worker the sweep looks at.
type sweepCandidate struct {
	e     ledger.Entry
	agent *herdr.AgentInfo // nil: gone
}

// sweepClassify decides one worker from its live state and checks. It is
// pure: the single place the rules live.
func sweepClassify(e ledger.Entry, live *herdr.AgentInfo, c *worktreeChecks, checkErr string) (class, reason string) {
	if live != nil && (live.Status == herdr.StatusWorking || live.Status == herdr.StatusBlocked) {
		return SweepNeedsLook, "the agent is " + string(live.Status)
	}
	if checkErr != "" {
		return SweepNeedsLook, "checks failed: " + checkErr
	}
	if c == nil {
		return SweepNeedsLook, "no worktree to check"
	}
	switch {
	case !c.Exists:
		warn, msg := c.goneVerdict()
		if warn {
			return SweepNeedsLook, msg
		}
		return SweepReady, "Sweep: " + sweepState(e) + "; " + msg
	case c.Primary:
		return SweepSkip, "it works in the repository's primary checkout"
	case c.InProgress != "":
		return SweepNeedsLook, "a " + c.InProgress + " is in progress"
	case c.Dirty:
		return SweepNeedsLook, fmt.Sprintf("uncommitted changes (%d files)", c.DirtyFiles)
	case c.Error != "":
		return SweepNeedsLook, "checks failed: " + c.Error
	case c.Unmerged < 0:
		return SweepNeedsLook, "could not tell whether its branch is merged"
	case c.Unmerged > 0:
		unpushed := -1
		if c.Detail != nil {
			unpushed = c.Detail.Unpushed
		}
		switch {
		case unpushed > 0:
			return SweepNeedsLook, fmt.Sprintf("%d commits not in %s, %d of them on no remote", c.Unmerged, c.BaseUsed, unpushed)
		case unpushed == 0:
			return SweepNeedsLook, fmt.Sprintf("pushed but not merged: %d commits not in %s (an open PR?)", c.Unmerged, c.BaseUsed)
		}
		return SweepNeedsLook, fmt.Sprintf("%d commits not in %s", c.Unmerged, c.BaseUsed)
	}
	if c.Detail != nil && c.Detail.Unpushed > 0 {
		return SweepNeedsLook, fmt.Sprintf("%d commits on no remote", c.Detail.Unpushed)
	}
	return SweepReady, sweepReason(e, c)
}

func sweepState(e ledger.Entry) string {
	switch e.Status {
	case ledger.Finished:
		return "finished"
	case ledger.Landed:
		return "landed"
	}
	return "idle"
}

// sweepReason is the mark's reason for a ready worker, from its checks.
func sweepReason(e ledger.Entry, c *worktreeChecks) string {
	base := c.BaseUsed
	if base == "" {
		base = "its base"
	}
	r := fmt.Sprintf("Sweep: %s; worktree clean; every commit of %s is in %s", sweepState(e), c.Branch, base)
	if len(c.Landed) > 0 {
		r += " (" + landedText(base, c.Landed) + ")"
	}
	return r + "; nothing unpushed"
}

// sweepCandidates picks the workers to look at and the ones to skip with
// a reason. self is the caller's own agent id (never swept).
func (s *Server) sweepCandidates(agents []herdr.AgentInfo, idleAfter time.Duration, self string, now time.Time) (cands []sweepCandidate, skipped []sweepItem) {
	live := map[string]*herdr.AgentInfo{}
	for i := range agents {
		live[agents[i].ID] = &agents[i]
	}
	for _, e := range s.ledger.Entries() {
		item := sweepItem{ID: e.AgentID, EntryID: e.ID, Host: e.Host, Name: e.AgentName, Repo: e.RepoName, Branch: e.Branch, Class: SweepSkip}
		a := live[e.AgentID]
		name := e.AgentName
		if a != nil {
			name = a.Name
		}
		switch e.Status {
		case ledger.ReadyToRemove:
			item.Reason = "already marked ready to remove"
			skipped = append(skipped, item)
			continue
		case ledger.Finished, ledger.Landed:
		case ledger.Running:
			// Still recorded as running: only an agent that has sat idle
			// for a while counts.
			if idleAfter <= 0 || a == nil || (a.Status != herdr.StatusIdle && a.Status != herdr.StatusDone) {
				continue
			}
			since := a.StatusSince
			if since == nil {
				since = a.LastActive
			}
			if since == nil || now.Sub(*since) < idleAfter {
				continue
			}
		default:
			continue // waiting, removed, failed
		}
		switch {
		case e.AgentID == "":
			item.Reason = "no agent"
		case s.isCoordinator(e.AgentID, name):
			item.Reason = "a coordinator is never swept"
		case self != "" && e.AgentID == self:
			item.Reason = "the caller itself"
		case e.Worktree == "":
			item.Reason = "no worktree of its own"
		case !s.knownHost(e.Host):
			item.Class, item.Reason = SweepNeedsLook, "unknown host "+e.Host
		default:
			cands = append(cands, sweepCandidate{e: e, agent: a})
			continue
		}
		skipped = append(skipped, item)
	}
	return cands, skipped
}

// sweep runs a sweep. mark records one ready worker's mark (nil: dry run).
func (s *Server) sweep(ctx context.Context, b sweepBody, self, by string) sweepResult {
	idle := defaultSweepIdle
	if v := strings.TrimSpace(b.IdleAfter); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			idle = d
		}
	}
	res := sweepResult{DryRun: b.DryRun, Ready: []sweepItem{}, NeedsLook: []sweepItem{}, Skipped: []sweepItem{}}
	cands, skipped := s.sweepCandidates(s.fleet.Agents(), idle, self, time.Now())
	res.Skipped = append(res.Skipped, skipped...)
	for i := len(res.Skipped) - 1; i >= 0; i-- {
		if res.Skipped[i].Class == SweepNeedsLook {
			res.NeedsLook = append(res.NeedsLook, res.Skipped[i])
			res.Skipped = append(res.Skipped[:i], res.Skipped[i+1:]...)
		}
	}
	items := make([]sweepItem, len(cands))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, c := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e := c.e
			// The host's checks fetch origin first, so a branch merged
			// upstream since the last fetch is seen as merged.
			chk, lerr := s.checksOn(ctx, e.Host, e.Worktree, e.Branch, e.Base, checkOpts{Detail: true}.forEntry(e))
			msg := ""
			if lerr != nil {
				msg = lerr.message
			}
			class, reason := sweepClassify(e, c.agent, chk, msg)
			items[i] = sweepItem{ID: e.AgentID, EntryID: e.ID, Host: e.Host, Name: e.AgentName, Repo: e.RepoName, Branch: e.Branch, Class: class, Reason: reason}
		}()
	}
	wg.Wait()
	for i, it := range items {
		switch it.Class {
		case SweepReady:
			if !b.DryRun {
				m := ledger.Mark{AgentID: it.ID, By: by, Reason: clip(it.Reason, 500)}
				if _, _, err := s.applyMark(m, cands[i].agent, true); err == nil {
					it.Marked = true
				} else {
					it.Class, it.Reason = SweepNeedsLook, "could not mark it: "+err.Error()
				}
			}
		}
		switch it.Class {
		case SweepReady:
			res.Ready = append(res.Ready, it)
		case SweepNeedsLook:
			res.NeedsLook = append(res.NeedsLook, it)
		default:
			res.Skipped = append(res.Skipped, it)
		}
	}
	for _, l := range []*[]sweepItem{&res.Ready, &res.NeedsLook, &res.Skipped} {
		sort.SliceStable(*l, func(i, j int) bool { return (*l)[i].ID < (*l)[j].ID })
	}
	if b.Notify && !b.DryRun && (len(res.Ready) > 0 || len(res.NeedsLook) > 0) {
		res.Notified, res.NotifyError = s.notifySweep(ctx, res)
	}
	return res
}

// sweepSummary is the text a coordinator gets.
func sweepSummary(res sweepResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "A sweep for finished agents marked %d ready to remove (the user confirms them in the app's removals review).", len(res.Ready))
	for _, it := range res.Ready {
		fmt.Fprintf(&sb, "\n- ready: %s %s: %s", it.ID, it.Branch, it.Reason)
	}
	if len(res.NeedsLook) > 0 {
		sb.WriteString("\nThese need a look (not marked); mark them with `comebye mark` if you judge them done:")
		for _, it := range res.NeedsLook {
			fmt.Fprintf(&sb, "\n- %s %s: %s", it.ID, it.Branch, it.Reason)
		}
	}
	return sb.String()
}

// notifySweep tells an idle coordinator the outcome.
func (s *Server) notifySweep(ctx context.Context, res sweepResult) (to, errMsg string) {
	a, why := s.briefCoordinator()
	if a == nil {
		return "", "no idle coordinator to tell (" + why + ")"
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := s.promptAgent(ctx, *a, sweepSummary(res)); err != nil {
		return "", err.Error()
	}
	return a.ID, ""
}

// handleSweep: POST {dry_run?, idle_after?, notify?}. The user and agents
// may sweep, as both may mark; an agent is never swept by itself.
func (s *Server) handleSweep(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b sweepBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
	}
	self := ""
	if c := callerOf(r); c.agent() {
		self = c.agentRef()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	writeJSON(w, s.sweep(ctx, b, self, s.by(r, "")))
}

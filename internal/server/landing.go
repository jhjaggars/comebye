package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
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
	tip := ""
	if e.Worktree != "" && s.knownHost(e.Host) {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		c, lerr := s.checksOn(ctx, e.Host, e.Worktree, e.Branch, b.Base, checkOpts{}.forEntry(e))
		cancel()
		if lerr == nil && c.Exists && !c.Primary {
			tip = c.Tip
		}
	}
	if retry && tip == "" && e.Approval != nil {
		tip = e.Approval.Tip
	}
	got, ok, err := s.ledger.StartLanding(b.AgentID, ledger.Approval{By: s.by(r, ""), Note: note, Base: b.Base, Tip: tip})
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

// checkLandings starts the checks of landings whose report is in.
func (s *Server) checkLandings() {
	for _, e := range s.ledger.LandingsToCheck() {
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

// checkLanding checks a landing on its host and settles it.
func (s *Server) checkLanding(e ledger.Entry) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	lc := landingVerdict(s.landingChecks(ctx, e))
	if _, err := s.ledger.SetLandCheck(e.ID, *e.FinishedAt, lc); err != nil {
		log.Printf("ledger: %v", err)
	}
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
	it := &removal{ID: e.AgentID, Host: e.Host, EntryID: e.ID, Repo: e.RepoName, Branch: e.Branch, Base: base,
		Worktree: e.Worktree, Warnings: []string{}, Notes: []string{}, opts: opts}
	s.checkRemoval(ctx, it)
	return it
}

// landingVerdict reads a landing's checks: OK only when they ran, found
// the work (in its worktree, or recorded in the repository), and nothing
// is outside the base.
func landingVerdict(it *removal) ledger.LandCheck {
	lc := ledger.LandCheck{At: time.Now().UTC(), Verdict: checkVerdict(it)}
	c := it.Checks
	if c == nil {
		return lc
	}
	lc.Base = c.BaseUsed
	lc.OK = it.ChecksError == "" && len(it.Warnings) == 0 && c.Error == "" && c.Unmerged == 0 && !c.Primary && (c.Exists || c.Gone)
	for _, l := range c.Landed {
		if l.As != "" {
			lc.Landed = append(lc.Landed, l.As)
		}
	}
	return lc
}

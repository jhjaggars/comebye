package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// Delivered work: an agent's findings live in Pages, so the notes and docs
// it left in its worktree (uncommitted files, local commits holding only
// those docs) need not be kept. `comebye deliver` records which Pages
// capture the work; the removals review then lists what a discard drops as
// notes rather than warnings, and the user discards every such item with
// one confirmation. Nothing is discarded here: only the user's confirm,
// carrying each item's tip, does it.

const (
	maxDelivered   = 20  // pages in one record
	maxNotCaptured = 10  // statements of what the pages lack
	maxLostLines   = 40  // files / commits listed in a ledger record
	notCapturedLen = 300 // one statement
)

// deliveredBody is the JSON body of POST /api/deliveries.
type deliveredBody struct {
	AgentID     string   `json:"agent_id"`
	Pages       []string `json:"pages"`
	NotCaptured []string `json:"not_captured"`
	By          string   `json:"by"`
}

func writeDeliveryErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSONStatus(w, status, map[string]string{"code": code, "message": msg})
}

// handleDeliver records pages as capturing an agent's work:
// POST {agent_id, pages: [pg_…], not_captured?: [text]}. The user and a
// coordinator may record it for any agent; an agent only for itself.
func (s *Server) handleDeliver(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	if s.pages == nil {
		writeDeliveryErr(w, http.StatusServiceUnavailable, "pages_off", "pages are off on this hub")
		return
	}
	var b deliveredBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil || b.AgentID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	c := callerOf(r)
	if c.agent() && b.AgentID != c.agentRef() && !s.callerIsCoordinator(c) {
		writeDeliveryErr(w, http.StatusForbidden, "not_yours", "an agent records the delivery of its own work; only the user or a coordinator records another's")
		return
	}
	if len(b.Pages) == 0 || len(b.Pages) > maxDelivered {
		writeDeliveryErr(w, http.StatusBadRequest, "bad_pages", "name the pages that hold the work (pg_…), at most 20")
		return
	}
	var notCaptured []string
	for _, n := range b.NotCaptured {
		if n = oneLine(n, notCapturedLen); n != "" {
			notCaptured = append(notCaptured, n)
		}
	}
	if len(notCaptured) > maxNotCaptured {
		notCaptured = notCaptured[:maxNotCaptured]
	}
	entry, isWorker := s.ledger.ByAgent(b.AgentID)
	var agent *herdr.AgentInfo
	for _, a := range s.fleet.Agents() {
		if a.ID == b.AgentID {
			agent = &a
			break
		}
	}
	if agent == nil && !isWorker {
		writeDeliveryErr(w, http.StatusNotFound, "agent_not_found", "no such agent")
		return
	}
	d := ledger.Delivery{AgentID: b.AgentID, NotCaptured: notCaptured, By: s.by(r, b.By)}
	if isWorker {
		d.EntryID, d.Name = entry.ID, entry.AgentName
	}
	if agent != nil {
		d.Name = agent.Name
	}
	seen := map[string]bool{}
	for _, id := range b.Pages {
		id = strings.TrimSpace(id)
		if seen[id] {
			continue
		}
		seen[id] = true
		p, ok := s.pages.get(id)
		if !ok {
			writeDeliveryErr(w, http.StatusBadRequest, "page_not_found", id+": no such page")
			return
		}
		if why := s.pageNotOf(p, b.AgentID, d.EntryID); why != "" {
			writeDeliveryErr(w, http.StatusBadRequest, "page_not_theirs", id+": "+why)
			return
		}
		d.Pages = append(d.Pages, ledger.PageRef{ID: p.ID, Title: p.Title})
	}
	d, err := s.ledger.SetDelivery(d)
	if err != nil {
		writeDeliveryErr(w, http.StatusInternalServerError, "ledger_error", err.Error())
		return
	}
	writeJSON(w, map[string]any{"delivery": d})
}

// pageNotOf says why page p is not the work of the agent (and of its
// handoff entry): "" when it is. A page counts when the agent published it;
// or when it is filed under the agent's handoff and was published by the
// user or a coordinator (a coordinator publishing for a worker whose pane
// could not).
func (s *Server) pageNotOf(p Page, agentID, entryID string) string {
	if p.Agent == agentID {
		return ""
	}
	if entryID != "" && p.Handoff == entryID {
		if p.Agent == "" || s.isCoordinator(p.Agent, "") {
			return ""
		}
		return "it is filed under the handoff but published by " + p.By + ", who is neither the agent nor a coordinator"
	}
	if p.Handoff != "" && entryID == "" {
		return "it is filed under handoff " + p.Handoff + ", and this agent has no handoff"
	}
	return "it was published by " + nonEmpty(p.By, "someone else") + ", not by this agent or for its handoff"
}

func nonEmpty(s, d string) string {
	if s != "" {
		return s
	}
	return d
}

// deliveredPage is a Page that captures work, as the review shows it.
type deliveredPage struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Path  string `json:"path"`
	URL   string `json:"url,omitempty"`
}

// deliveredView is what the review shows of an item's delivery: the pages
// and what discarding it drops (the lists are capped; the counts are not).
type deliveredView struct {
	Pages       []deliveredPage `json:"pages"`
	NotCaptured []string        `json:"not_captured,omitempty"`
	At          string          `json:"at,omitempty"`
	By          string          `json:"by,omitempty"`
	// Holds: the Pages count as capturing the work, so the dirty files and
	// local commits are notes and the item can be discarded in bulk.
	Holds bool `json:"holds"`
	// Why says what stops it holding.
	Why string `json:"why,omitempty"`
	// What a discard drops: the counts are exact, the lists capped.
	Files      int      `json:"files"`
	FileList   []string `json:"file_list,omitempty"`
	Commits    int      `json:"commits"`
	CommitList []string `json:"commit_list,omitempty"`
}

// deliveryFor finds the delivery record that applies to a marked item: the
// agent's, unless it was recorded for another incarnation of the agent id.
func (s *Server) deliveryFor(it *removal) *ledger.Delivery {
	d, ok := s.ledger.DeliveryOf(it.ID)
	if !ok {
		return nil
	}
	if d.EntryID != it.EntryID {
		return nil
	}
	if d.Name != "" && it.Name != "" && d.Name != it.Name {
		return nil
	}
	return &d
}

// deliveredPages resolves a delivery's pages for display; a page deleted
// since is dropped (the record then names fewer pages, or none, and the
// work no longer counts as captured).
func (s *Server) deliveredPages(d *ledger.Delivery) []deliveredPage {
	var out []deliveredPage
	for _, ref := range d.Pages {
		p, ok := s.pages.get(ref.ID)
		if !ok {
			continue
		}
		dp := deliveredPage{ID: p.ID, Title: p.Title, Path: "/p/" + p.ID}
		if base := s.pagesBase(); base != "" {
			dp.URL = base + "/p/" + p.ID
		}
		out = append(out, dp)
	}
	return out
}

// deliveryHolds decides, before the git checks, whether an item's delivery
// lets its dirty files and local commits be notes: the mark proposes a
// discard, pages still exist, the recorder flagged nothing as missing from
// them, and the session did not answer UNSAFE after they were recorded (or
// is still being asked). It returns the pages and, when it does not hold,
// the reason.
func (s *Server) deliveryHolds(it *removal) (*deliveredView, bool) {
	if s.pages == nil || s.ledger == nil || !it.Mark.Discard {
		return nil, false
	}
	d := s.deliveryFor(it)
	if d == nil {
		return nil, false
	}
	v := &deliveredView{Pages: s.deliveredPages(d), NotCaptured: d.NotCaptured, At: d.At.UTC().Format("2006-01-02T15:04:05Z"), By: d.By}
	switch a := it.Mark.Ask; {
	case len(v.Pages) == 0:
		v.Why = "the pages recorded for it no longer exist"
	case len(d.NotCaptured) > 0:
		v.Why = "its pages do not hold everything"
	case a != nil && a.State == ledger.AskUnsafe && a.At.After(d.At):
		v.Why = "the session answered UNSAFE after its pages were recorded"
	case a != nil && a.State == ledger.AskAsking:
		v.Why = "the session has not yet answered whether it is safe to remove"
	default:
		v.Holds = true
	}
	return v, v.Holds
}

// recordLost fills in what discarding the item drops, from its checks, and
// says so in a note.
func (v *deliveredView) recordLost(c *worktreeChecks, lostCommits bool) (note string) {
	v.Files = c.Dirty0()
	if c.Detail != nil {
		for _, f := range c.Detail.Status {
			if len(v.FileList) < maxLostLines {
				v.FileList = append(v.FileList, f)
			}
		}
		if lostCommits {
			for _, cm := range c.Detail.Commits {
				if len(v.CommitList) < maxLostLines {
					v.CommitList = append(v.CommitList, shortSHA(cm.SHA)+" "+cm.Subject)
				}
			}
		}
	}
	if lostCommits {
		v.Commits = c.Unmerged
	}
	var titles []string
	for _, p := range v.Pages {
		titles = append(titles, "\u201c"+p.Title+"\u201d")
	}
	var drops []string
	if v.Files > 0 {
		drops = append(drops, itoa(v.Files)+" uncommitted "+plural(v.Files, "file", "files")+listed(v.FileList))
	}
	if v.Commits > 0 {
		drops = append(drops, itoa(v.Commits)+" unmerged "+plural(v.Commits, "commit", "commits")+listed(v.CommitList))
	}
	return "captured in " + strings.Join(titles, ", ") + "; discarding drops " + strings.Join(drops, " and ")
}

// Dirty0 is the number of uncommitted files, 0 when the tree is clean.
func (c *worktreeChecks) Dirty0() int {
	if !c.Dirty {
		return 0
	}
	if c.DirtyFiles > 0 {
		return c.DirtyFiles
	}
	return 1
}

// listed renders up to 5 items as ": a, b, c" (with "…" when more).
func listed(items []string) string {
	if len(items) == 0 {
		return ""
	}
	more := ""
	if len(items) > 5 {
		items, more = items[:5], ", \u2026"
	}
	return " (" + strings.Join(items, ", ") + more + ")"
}

// notCaptured is the statements the recorder made of what its pages lack
// (nil-safe).
func (v *deliveredView) notCaptured() []string {
	if v == nil {
		return nil
	}
	return v.NotCaptured
}

// lostWork is the summary of what a discard dropped, kept in the ledger
// record of the removal: the files and commits, and the Pages that
// captured the work (Captured: the user discarded on the strength of them).
type lostWork struct {
	Captured   bool            `json:"captured"`
	Pages      []deliveredPage `json:"pages,omitempty"`
	Files      int             `json:"files"`
	FileList   []string        `json:"file_list,omitempty"`
	Commits    int             `json:"commits"`
	CommitList []string        `json:"commit_list,omitempty"`
	Branch     string          `json:"branch,omitempty"`
	Tip        string          `json:"tip,omitempty"`
}

// lostOf summarises what discarding a reviewed item drops; nil when its
// checks are not there.
func lostOf(it *removal) *lostWork {
	c := it.Checks
	if c == nil || !c.Exists {
		return nil
	}
	v := it.Delivered
	if !it.DeliveredDiscard || v == nil {
		v = &deliveredView{}
		v.recordLost(c, c.Unmerged > 0 && c.Equivalent == "")
	}
	return &lostWork{Captured: it.DeliveredDiscard, Pages: v.Pages, Files: v.Files, FileList: v.FileList,
		Commits: v.Commits, CommitList: v.CommitList, Branch: c.Branch, Tip: c.Tip}
}

// withLost adds the summary to a removal's JSON record as "lost".
func withLost(raw json.RawMessage, l *lostWork) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	b, err := json.Marshal(l)
	if err != nil {
		return raw
	}
	m["lost"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

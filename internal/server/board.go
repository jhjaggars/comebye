package server

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// The board: GET /api/board. A read-only view built only from records the
// hub already holds (the to-do list, the ledger, the fleet's agents, the
// release items), so nobody has to retype state. Each lane's rule is below
// and in docs/api.md; nothing is stored and nothing is guessed.

const (
	boardLandedWindow = 48 * time.Hour // "landed (recent)"
	boardMaxItems     = 40             // items listed per lane; Count is always the full number
)

// Lane keys, in the order they are served.
const (
	laneNeeds   = "needs_you"
	laneRunning = "running"
	laneWaiting = "waiting"
	laneLanding = "landing"
	laneReports = "reports"
	laneLanded  = "landed"
	laneHubDid  = "hub_did"
)

// BoardItem is one line of a lane.
type BoardItem struct {
	ID     string `json:"id"`   // to-do id, ledger entry id
	Type   string `json:"type"` // "todo", "handoff", "release", "dialog"
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	// Phase qualifies State for a handoff whose ledger status does not say
	// it all: "between passes (every 1h)" for a loop worker waiting for its
	// next pass (State stays "running").
	Phase   string `json:"phase,omitempty"`
	Project string `json:"project"`
	Kind    string `json:"kind,omitempty"`  // a to-do's kind
	State   string `json:"state,omitempty"` // a handoff's ledger status, a release's state
	Since   string `json:"since,omitempty"` // RFC 3339: when it entered the state
	Agent   string `json:"agent,omitempty"` // fleet id of the agent, to open it
	// Report is true when the handoff has a final report to read; Pages are
	// the Pages published for it (the Reports lane links them).
	Report bool      `json:"report,omitempty"`
	Pages  []PageRef `json:"pages,omitempty"`
}

// PageRef is a published Page named by a board item.
type PageRef struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

// BoardLane is one lane with its rule.
type BoardLane struct {
	Key   string      `json:"key"`
	Name  string      `json:"name"`
	Rule  string      `json:"rule"`
	Count int         `json:"count"`
	Items []BoardItem `json:"items"`
}

// BoardView is the response.
type BoardView struct {
	At    time.Time   `json:"at"`
	Lanes []BoardLane `json:"lanes"`
	// Loose counts what the lanes leave out on purpose: entries marked ready
	// to remove that never landed. Reports is kept for older readers and is
	// always 0: finished research, plans, reviews and spikes are the
	// "reports" lane.
	// Archived counts the archived sessions (finished work put away,
	// archived.go): they are in no lane, and the footer links the count to
	// the Archived screen.
	Loose struct {
		Reports       int `json:"reports"`
		ReadyToRemove int `json:"ready_to_remove"`
		Archived      int `json:"archived"`
	} `json:"loose"`
}

// handleBoard answers GET /api/board.
func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	now := time.Now()
	agents := s.fleet.Agents()
	s.observeLedger(agents)
	live := map[string]herdr.AgentInfo{}
	for _, a := range agents {
		live[a.ID] = a
	}
	entries := s.ledger.Entries()
	byAgent := map[string]ledger.Entry{}
	for _, e := range entries {
		if e.AgentID != "" {
			byAgent[e.AgentID] = e
		}
	}
	var todos []Todo
	if s.todos != nil {
		todos = s.todos.list("")
	}

	lane := func(key, name, rule string) *BoardLane {
		return &BoardLane{Key: key, Name: name, Rule: rule, Items: []BoardItem{}}
	}
	needs := lane(laneNeeds, "Needs you", "Open to-do items (a rollout of a tagged release is under Waiting on others), and running handoffs whose agent is stuck on a dialog.")
	running := lane(laneRunning, "Running", "Handoffs in state running whose agent is not stuck on a dialog.")
	waiting := lane(laneWaiting, "Waiting on others", "Handoffs queued behind earlier steps (state waiting), and tagged releases still rolling out.")
	landing := lane(laneLanding, "Ready to land, or landing", "Finished handoffs whose work is meant to merge (not research, plan, review, spike, poc, eval or test work, declared or from the branch name) and that are not marked, and handoffs in state landing.")
	reports := lane(laneReports, "Reports to read", "Finished research, plan, review, spike, poc, eval and test handoffs (declared, or from the branch name) that are not marked ready to remove: their work is the report (and the Pages published for it), not a branch to merge.")
	landed := lane(laneLanded, "Landed, recent", "Handoffs the hub found in the base (landed, or marked ready to remove after landing) within the last 48 hours.")
	// add keeps every item; the lanes are sorted and cut to boardMaxItems
	// afterwards, so the cut never drops the newest.
	add := func(l *BoardLane, it BoardItem) {
		l.Count++
		l.Items = append(l.Items, it)
	}
	pagesOf := map[string][]PageRef{}
	if s.pages != nil {
		for _, p := range s.pages.list() {
			if p.Handoff != "" {
				pagesOf[p.Handoff] = append(pagesOf[p.Handoff], PageRef{ID: p.ID, Title: p.Title})
			}
		}
	}
	title := func(e ledger.Entry) string {
		if e.AgentName != "" {
			return e.AgentName
		}
		if e.Branch != "" {
			return e.Branch
		}
		return e.ID
	}
	project := func(e ledger.Entry) string {
		if e.RepoName != "" {
			return e.RepoName
		}
		return "Other"
	}
	since := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	detail := func(e ledger.Entry) string {
		if e.Progress != nil && e.Progress.Text != "" {
			return oneLine(e.Progress.Text, 140)
		}
		return oneLine(e.Brief, 140)
	}

	// Needs you: to-dos, oldest first.
	for _, t := range todos {
		if t.State != TodoOpen {
			continue
		}
		if t.Release != nil && t.Release.State == relTagged {
			if t.Release.Dismissed {
				continue
			}
			add(waiting, BoardItem{ID: t.ID, Type: "release", Title: "Release " + t.Release.Version, Detail: "tagged; rolling out to the hosts. The Releases tab shows the next step.", Project: "Releases", Kind: t.Kind, State: t.Release.State, Since: since(t.UpdatedAt)})
			continue
		}
		proj := "Not tied to a project"
		agent := ""
		if t.Waiting != nil {
			agent = t.Waiting.ID
			if e, ok := byAgent[t.Waiting.ID]; ok {
				proj = project(e)
			}
		}
		if t.Release != nil {
			proj = "Releases"
		}
		add(needs, BoardItem{ID: t.ID, Type: "todo", Title: oneLine(t.Question, 200), Project: proj, Kind: t.Kind, State: t.State, Since: since(t.CreatedAt), Agent: agent})
	}

	var view BoardView
	for _, e := range entries {
		a, isLive := live[e.AgentID]
		it := BoardItem{ID: e.ID, Type: "handoff", Title: title(e), Detail: detail(e), Project: project(e), State: string(e.Status), Since: since(e.CreatedAt), Agent: e.AgentID}
		switch e.Status {
		case ledger.Waiting:
			if len(e.After) > 0 {
				it.Detail = "queued behind " + strings.Join(e.After, ", ")
			}
			add(waiting, it)
		case ledger.Running:
			if e.BetweenPasses() && e.LastPassAt != nil {
				it.Phase, it.Since = "between passes ("+loopEveryText(e)+")", since(*e.LastPassAt)
				it.Detail = it.Phase
			}
			if isLive && a.Status == herdr.StatusBlocked {
				it.Type, it.Detail = "dialog", "stuck on a dialog: it needs an answer"
				add(needs, it)
			} else {
				add(running, it)
			}
		case ledger.Landing:
			if e.Approval != nil {
				it.Since = since(e.Approval.At)
			}
			add(landing, it)
		case ledger.Finished:
			if e.FinishedAt != nil {
				it.Since = since(*e.FinishedAt)
			}
			switch {
			case e.Mark != nil:
				view.Loose.ReadyToRemove++
			case ledger.DisposablePurpose(ledger.EffectivePurpose(e)):
				it.Report = e.FinalReport != ""
				it.Pages = pagesOf[e.ID]
				it.Kind = ledger.EffectivePurpose(e)
				add(reports, it)
			default:
				add(landing, it)
			}
		case ledger.Archived:
			view.Loose.Archived++
		case ledger.Landed, ledger.ReadyToRemove:
			if e.Status == ledger.ReadyToRemove && e.MarkedFrom != ledger.Landed {
				view.Loose.ReadyToRemove++
				break
			}
			at := e.CreatedAt
			if e.FinishedAt != nil {
				at = *e.FinishedAt
			}
			if e.LandCheck != nil && e.LandCheck.State == ledger.LandLanded && !e.LandCheck.At.IsZero() {
				at = e.LandCheck.At
			}
			if now.Sub(at) > boardLandedWindow {
				break
			}
			it.Since = since(at)
			add(landed, it)
		}
	}
	// Newest first in the lanes that are about time; the to-do list is
	// oldest first (the longest wait first).
	// The cut comes after the sort: what a long lane hides is the oldest.
	for _, l := range []*BoardLane{running, waiting, landing, reports, landed} {
		sort.SliceStable(l.Items, func(i, j int) bool { return l.Items[i].Since > l.Items[j].Since })
	}
	sort.SliceStable(needs.Items, func(i, j int) bool { return needs.Items[i].Since < needs.Items[j].Since })
	for _, l := range []*BoardLane{needs, running, waiting, landing, reports, landed} {
		if len(l.Items) > boardMaxItems {
			l.Items = l.Items[:boardMaxItems]
		}
	}

	view.At = now.UTC()
	view.Lanes = []BoardLane{*needs, *running, *waiting, *landing, *reports, *landed}
	if l := s.hubDidLane(now); l != nil {
		view.Lanes = append(view.Lanes, *l)
	}
	writeJSON(w, view)
}

// hubDidLane is the "Hub did" lane: what the hub handled itself in the last
// 24 hours (events.go), as a count and the newest lines. Nil when there is
// nothing, so a quiet day shows no lane.
func (s *Server) hubDidLane(now time.Time) *BoardLane {
	evs := s.events.query(now.Add(-hubDidWindow), nil, 0)
	line, lineAt, haveLine := "", time.Time{}, false
	if s.sweeps != nil && s.ledger != nil {
		line, lineAt, haveLine = s.sweepBoardLine()
	}
	if len(evs) == 0 && !haveLine {
		return nil
	}
	l := &BoardLane{Key: laneHubDid, Name: "Hub did", Count: len(evs), Items: []BoardItem{},
		Rule: "What the hub handled itself in the last 24 hours (landed work, automatic parks and removals, cleanups, sweeps, dialogs that cleared): a count and the newest five. `comebye log` lists them all. The first line says when the scheduled sweep last ran and what it marked."}
	if haveLine {
		l.Count++
		l.Items = append(l.Items, BoardItem{ID: "sweep-last", Type: "event", Title: line, Kind: evSweep, Since: lineAt.UTC().Format(time.RFC3339), Project: "Hub"})
	}
	for i, ev := range evs {
		if i >= hubDidLines {
			break
		}
		l.Items = append(l.Items, BoardItem{ID: ev.ID, Type: "event", Title: ev.Text, Kind: ev.Kind, Since: ev.At.UTC().Format(time.RFC3339), Project: "Hub"})
	}
	return l
}

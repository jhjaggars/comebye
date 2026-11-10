package server

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// The briefing board: GET /api/board. A read-only view built only from
// records the hub already holds (the to-do list, the ledger, the fleet's
// agents, the release items), so the coordinator's briefing need not
// retype state. Each lane's rule is below and in docs/api.md; nothing is
// stored and nothing is guessed. The coordinator's own words reach the board
// in two places and are parsed from the latest briefing: the headline (its
// first line) and notes, bullets that start with an id (see boardNotes).

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
	laneLanded  = "landed"
)

// BoardItem is one line of a lane.
type BoardItem struct {
	ID      string `json:"id"`   // to-do id, ledger entry id
	Type    string `json:"type"` // "todo", "handoff", "release", "dialog"
	Title   string `json:"title"`
	Detail  string `json:"detail,omitempty"`
	Project string `json:"project"`
	Kind    string `json:"kind,omitempty"`  // a to-do's kind
	State   string `json:"state,omitempty"` // a handoff's ledger status, a release's state
	Since   string `json:"since,omitempty"` // RFC 3339: when it entered the state
	Agent   string `json:"agent,omitempty"` // fleet id of the agent, to open it
	// Why is the coordinator's reason for a Needs you item, Note its note on
	// any item, both from the latest briefing.
	Why  string `json:"why,omitempty"`
	Note string `json:"note,omitempty"`
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
	At       time.Time   `json:"at"`
	Headline string      `json:"headline,omitempty"`
	BriefAt  *time.Time  `json:"brief_at,omitempty"`
	BriefBy  string      `json:"brief_by,omitempty"`
	Lanes    []BoardLane `json:"lanes"`
	// Loose counts what the lanes leave out on purpose: reports to read
	// (finished work that is not meant to land) and entries marked ready to
	// remove that never landed.
	Loose struct {
		Reports       int `json:"reports"`
		ReadyToRemove int `json:"ready_to_remove"`
	} `json:"loose"`
	// UnkeyedNotes are bullets of the briefing that look like notes but name
	// no item the board knows (an id that is gone, a typo).
	UnkeyedNotes []string `json:"unkeyed_notes,omitempty"`
}

var (
	boardNoteRE = regexp.MustCompile("^\\s*[-*+]\\s+(?:\\*\\*|__|`)?([A-Za-z0-9][A-Za-z0-9._#/@:-]*?)(?:\\*\\*|__|`)?\\s*(?::|\\x{2014}|\\x{2013}|\\s-\\s)(?:\\*\\*|__)?\\s*(.+)$")
	boardMDRE   = regexp.MustCompile("[*_`#]+")
)

// boardHeadline is the first line of text of a briefing, without Markdown
// emphasis and heading marks.
func boardHeadline(md string) string {
	for _, l := range strings.Split(strings.ReplaceAll(md, "\r", ""), "\n") {
		t := strings.TrimSpace(l)
		if t == "" || briefRuleRE.MatchString(t) {
			continue
		}
		return oneLine(strings.TrimSpace(boardMDRE.ReplaceAllString(t, "")), 240)
	}
	return ""
}

// boardNotes parses the notes of a briefing: a list item that starts with an
// id, bold or in backticks or bare, then ":" or a dash and the note. Items in
// the "Needs you" section are reasons ("why"), not notes. The returned maps
// are keyed by the id as written; lines whose id is not in known are returned
// as unkeyed.
func boardNotes(md string, known func(string) bool) (notes, why map[string]string, unkeyed []string) {
	notes, why = map[string]string{}, map[string]string{}
	entries, found := briefNeedsEntries(md)
	if found {
		for _, e := range entries {
			for _, id := range briefIDRE.FindAllString(e, 1) {
				w := strings.TrimSpace(strings.Trim(briefItemRE.ReplaceAllString(e, ""), " "))
				w = strings.TrimSpace(strings.Trim(boardMDRE.ReplaceAllString(strings.Replace(w, id, "", 1), ""), " :—–-"))
				why[id] = oneLine(w, 240)
			}
		}
	}
	needs := map[string]bool{}
	for _, e := range entries {
		needs[e] = true
	}
	inNeeds, inCode := false, false
	for _, l := range strings.Split(strings.ReplaceAll(md, "\r", ""), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		if h, ok := briefHeadingText(l); ok {
			inNeeds = briefNeedsRE.MatchString(h)
			continue
		}
		if inNeeds {
			continue
		}
		m := boardNoteRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		id, text := m[1], oneLine(strings.TrimSpace(boardMDRE.ReplaceAllString(m[2], "")), 240)
		if known(id) {
			if notes[id] == "" {
				notes[id] = text
			}
		} else if looksLikeID(id) {
			unkeyed = append(unkeyed, oneLine(id+": "+text, 120))
		}
	}
	return notes, why, unkeyed
}

// looksLikeID is true for a to-do id (t + 10 hex) or a ledger entry id (h + 12
// hex): a bullet starting with one that the board does not know is reported.
var idShapeRE = regexp.MustCompile(`^(?:t[0-9a-f]{10}|h[0-9a-f]{12})$`)

func looksLikeID(s string) bool { return idShapeRE.MatchString(s) }

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
	landing := lane(laneLanding, "Ready to land, or landing", "Finished handoffs whose work is meant to merge (not a research, plan, review or spike) and that are not marked, and handoffs in state landing.")
	landed := lane(laneLanded, "Landed, recent", "Handoffs the hub found in the base (landed, or marked ready to remove after landing) within the last 48 hours.")
	add := func(l *BoardLane, it BoardItem) {
		l.Count++
		if len(l.Items) < boardMaxItems {
			l.Items = append(l.Items, it)
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
	known := map[string]bool{}
	for _, t := range todos {
		if t.State != TodoOpen {
			continue
		}
		known[t.ID] = true
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
		known[e.ID] = true
		if e.AgentName != "" {
			known[e.AgentName] = true
		}
		a, isLive := live[e.AgentID]
		it := BoardItem{ID: e.ID, Type: "handoff", Title: title(e), Detail: detail(e), Project: project(e), State: string(e.Status), Since: since(e.CreatedAt), Agent: e.AgentID}
		switch e.Status {
		case ledger.Waiting:
			if len(e.After) > 0 {
				it.Detail = "queued behind " + strings.Join(e.After, ", ")
			}
			add(waiting, it)
		case ledger.Running:
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
			case ledger.DisposablePurpose(e.Purpose):
				view.Loose.Reports++
			default:
				add(landing, it)
			}
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
	for _, l := range []*BoardLane{running, waiting, landing, landed} {
		sort.SliceStable(l.Items, func(i, j int) bool { return l.Items[i].Since > l.Items[j].Since })
	}
	sort.SliceStable(needs.Items, func(i, j int) bool { return needs.Items[i].Since < needs.Items[j].Since })

	view.At = now.UTC()
	view.Lanes = []BoardLane{*needs, *running, *waiting, *landing, *landed}
	if b, err := s.brief.get(); err == nil && b != nil && strings.TrimSpace(b.Markdown) != "" {
		at := b.At
		view.BriefAt, view.BriefBy = &at, b.By
		view.Headline = boardHeadline(b.Markdown)
		notes, why, unkeyed := boardNotes(b.Markdown, func(id string) bool { return known[id] })
		view.UnkeyedNotes = unkeyed
		for li := range view.Lanes {
			for ii := range view.Lanes[li].Items {
				it := &view.Lanes[li].Items[ii]
				it.Note = notes[it.ID]
				if it.Note == "" && it.Type != "todo" {
					it.Note = notes[it.Title] // a worker by its name
				}
				if view.Lanes[li].Key == laneNeeds {
					it.Why = why[it.ID]
				}
			}
		}
	}
	writeJSON(w, view)
}

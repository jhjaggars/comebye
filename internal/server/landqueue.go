package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// The landing queue (internal/ledger/landqueue.go has the data and rules).
// This file is the hub's side: telling a worker it is up, telling the
// coordinator when a turn timed out, and the routes a coordinator uses to
// look at, reorder or skip a queue.

// DefaultLandTurnTimeout is how long a worker may hold its turn to land
// before the slot is released.
const DefaultLandTurnTimeout = 45 * time.Minute

// queueState is an entry's place in its landing queue, as the ledger shows it.
type queueState struct {
	Position  int        `json:"position"` // 1 is the front
	Ahead     int        `json:"ahead"`
	Total     int        `json:"total"`
	Turn      bool       `json:"turn"` // the worker has been told it is up
	TurnAt    *time.Time `json:"turn_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"` // when a held turn is released
}

// queueStates maps an agent id to its queue state.
func (s *Server) queueStates() map[string]queueState {
	out := map[string]queueState{}
	for _, v := range s.ledger.Queues() {
		for _, it := range v.Items {
			q := queueState{Position: it.Position, Ahead: it.Position - 1, Total: len(v.Items), Turn: it.Turn, TurnAt: it.TurnAt}
			if it.TurnAt != nil && s.landTurn > 0 {
				x := it.TurnAt.Add(s.landTurn)
				q.ExpiresAt = &x
			}
			out[it.AgentID] = q
		}
	}
	return out
}

func (s *Server) queueStateOf(agentID string) *queueState {
	if q, ok := s.queueStates()[agentID]; ok {
		return &q
	}
	return nil
}

// advanceLandQueues hands out turns and times out a turn held too long. It
// runs with every ledger observation, so a turn passes within seconds of the
// landing before it settling.
func (s *Server) advanceLandQueues() {
	ch, err := s.ledger.AdvanceQueues(time.Now(), s.landTurn)
	if err != nil {
		log.Printf("ledger: %v", err)
	}
	for _, e := range ch.Granted {
		s.tellUp(e)
	}
	for _, e := range ch.Ended {
		mins := int(s.landTurn / time.Minute)
		s.tellAgent(e.AgentID, e.AgentName, fmt.Sprintf("Your turn to land on `%s` ran out after %d minutes, so the hub gave the slot to the next worker. Stop landing now. If your push already went through, report it with `comebye report -`; otherwise wait: the coordinator decides whether you queue again.", QueueBase(e), mins))
		if e.ReplyTo != nil && e.ReplyTo.AgentID != "" {
			s.tellAgent(e.ReplyTo.AgentID, e.ReplyTo.Name, fmt.Sprintf("Landing queue for %s/%s: %s did not finish landing in its turn (%d minutes), so the hub released the slot and the next worker is up. Its landing is still checked; run `comebye land %s --note … ` again to requeue it if it did not land.", e.RepoName, QueueBase(e), nameOr(e), mins, nameOr(e)))
		}
	}
}

// QueueBase is the branch an entry's landing is queued for.
func QueueBase(e ledger.Entry) string { return ledger.QueueKeyOf(e).Base }

func nameOr(e ledger.Entry) string {
	if e.AgentName != "" {
		return e.AgentName
	}
	return e.AgentID
}

// tellUp tells a worker its turn to land has come.
func (s *Server) tellUp(e ledger.Entry) {
	base := QueueBase(e)
	s.tellAgent(e.AgentID, e.AgentName, upText(base))
}

func upText(base string) string {
	return "You're up: it is your turn to land on `" + base + "`. Nobody else is landing there now, and you hold the slot until you report. Do the land steps you were sent: `git fetch origin`; if `origin/" + base + "` is still the commit you last rebased and tested on, push at once, otherwise rebase onto it, re-run the tests, and push as a fast-forward. Keep telling the hub what you are doing with `comebye report --progress --phase …`. Report with `comebye report -` when you have landed, or if you cannot: that frees the slot for the next worker. The slot is released automatically if you hold it too long."
}

func (s *Server) tellAgent(id, name, text string) {
	if id == "" || s.pq == nil {
		return
	}
	if _, _, err := s.pq.add(QueuedPrompt{Agent: id, Name: name, Text: text, From: "comebye hub"}); err != nil {
		log.Printf("landing queue: telling %s: %v", id, err)
		return
	}
	s.deliverQueue(s.fleet.Agents())
}

// ---- routes ----

type queueBodyReq struct {
	AgentID string `json:"agent_id"`
	// Action: "move" (to a position), "skip" (to the back; if it holds
	// the turn, the turn passes on).
	Action string `json:"action"`
	To     int    `json:"to"`
}

// handleLandQueue lists the landing queues (GET /api/landings/queue).
func (s *Server) handleLandQueue(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	qs := s.ledger.Queues()
	if qs == nil {
		qs = []ledger.QueueView{}
	}
	writeJSON(w, map[string]any{"queues": qs, "turn_timeout_seconds": int(s.landTurn / time.Second)})
}

// handleLandQueueEdit reorders or skips (POST /api/landings/queue).
func (s *Server) handleLandQueueEdit(w http.ResponseWriter, r *http.Request) {
	if s.ledgerOff(w, r) {
		return
	}
	var b queueBodyReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil || b.AgentID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if c := callerOf(r); c.agent() && b.AgentID == c.agentRef() {
		writeJSONStatus(w, http.StatusConflict, apiErr("self", "an agent cannot reorder its own landing"))
		return
	}
	switch strings.TrimSpace(b.Action) {
	case "move":
		if b.To < 1 {
			writeJSONStatus(w, http.StatusBadRequest, apiErr("bad_position", "to is a place in the queue, 1 for the front"))
			return
		}
		v, ok, err := s.ledger.MoveInQueue(b.AgentID, b.To)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, apiErr("ledger_error", err.Error()))
			return
		}
		if !ok {
			writeJSONStatus(w, http.StatusConflict, apiErr("not_queued", "that agent is not waiting in a landing queue (or it holds the turn: skip it instead)"))
			return
		}
		writeJSON(w, map[string]any{"queue": v})
	case "skip":
		had, next, ok, err := s.ledger.SkipInQueue(b.AgentID, time.Now())
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, apiErr("ledger_error", err.Error()))
			return
		}
		if !ok {
			writeJSONStatus(w, http.StatusConflict, apiErr("not_queued", "that agent is not in a landing queue"))
			return
		}
		if e, found := s.ledger.ByAgent(b.AgentID); found && had {
			s.tellAgent(e.AgentID, e.AgentName, "The coordinator skipped your turn to land: stop landing now (do not push) and wait; the hub tells you when it is your turn again.")
		}
		if next != nil {
			s.tellUp(*next)
		}
		v, _ := s.ledger.QueueOf(b.AgentID)
		writeJSON(w, map[string]any{"queue": v, "skipped_turn": had})
	default:
		writeJSONStatus(w, http.StatusBadRequest, apiErr("bad_action", "action is move or skip"))
	}
}

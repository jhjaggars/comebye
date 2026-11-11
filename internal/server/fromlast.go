package server

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/question"
	"github.com/jhjaggars/comebye/internal/state"
)

// "Send to To do": the same extraction the idle detector runs
// (questionTodos), on demand, on an agent's last message. The user presses
// it in the app, or an agent (or the user in its session) runs
// `comebye todo from-last`. Unlike the detector's guesses these items are
// kept at once, are not held back by a person's item already waiting on the
// agent, and are filed whatever the agent's status or whoever is watching.

// fromLastBody is the request: the agent whose last message to read (a
// fleet id "host/pane", a pane id or a name). An agent caller may leave it
// out and means itself, and may name only itself.
type fromLastBody struct {
	Agent string `json:"agent"`
}

// findFleetAgent resolves target to an agent of the fleet: a fleet id, a
// pane id or a name (local panes win).
func (s *Server) findFleetAgent(target string) (herdr.AgentInfo, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return herdr.AgentInfo{}, false
	}
	var byName *herdr.AgentInfo
	for _, a := range s.fleet.Agents() {
		a := a
		switch {
		case state.AgentKey(&a) == target || a.ID == target:
			return a, true
		case a.PaneID == target && s.isLocalAgent(a):
			return a, true
		case a.Name != "" && a.Name == target && byName == nil:
			byName = &a
		}
	}
	if byName != nil {
		return *byName, true
	}
	return herdr.AgentInfo{}, false
}

// handleTodoFromLast: POST /api/todos/from-last {agent} files one to-do per
// question in the agent's last message and answers {todos, filed, existing,
// found}.
func (s *Server) handleTodoFromLast(w http.ResponseWriter, r *http.Request) {
	if s.todoOff(w, r) {
		return
	}
	var b fromLastBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
	}
	bad := func(status int, code, msg string) {
		writeJSONStatus(w, status, map[string]string{"code": code, "message": msg})
	}
	c := callerOf(r)
	target := strings.TrimSpace(b.Agent)
	if c.agent() {
		self := ""
		if c.pane != "" {
			self = s.replyID(c)
		}
		a, ok := s.findFleetAgent(target)
		if target == "" {
			target = self
		} else if ok && state.AgentKey(&a) != self {
			bad(http.StatusForbidden, "self_only", "an agent can send only its own last message to the To do list")
			return
		}
	}
	if target == "" {
		bad(http.StatusBadRequest, "bad_agent", "name the agent whose last message to send")
		return
	}
	a, ok := s.findFleetAgent(target)
	if !ok {
		bad(http.StatusNotFound, "unknown_agent", "no such agent")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), autoReadTimeout)
	defer cancel()
	latest := s.latestOf(ctx, a.Host, a.PaneID)
	if latest == nil || strings.TrimSpace(latest.Text) == "" {
		bad(http.StatusNotFound, "no_message", "the agent has no message to read yet")
		return
	}
	items := s.questionTodos(a, latest.Text, 0, false)
	id := state.AgentKey(&a)
	by := s.by(r, "")
	out := struct {
		Todos    []Todo `json:"todos"`
		Filed    int    `json:"filed"`
		Existing int    `json:"existing"`
		Found    int    `json:"found"`
	}{Todos: []Todo{}, Found: len(items)}
	for _, it := range items {
		it.CreatedBy = by
		if c.agent() && c.pane != "" {
			it.CreatedByID = s.replyID(c)
		}
		h := sha1.Sum([]byte(question.Key(it.Question)))
		it.AutoKey = SourceQuestion + ":" + id + ":last:" + hex.EncodeToString(h[:6]) + "@" + latest.Time
		if s.todos.activeQuestion(id, it.Question) {
			out.Existing++
			continue
		}
		got, dup, err := s.todos.addExtracted(it)
		switch {
		case err != nil:
			todoErr(w, err)
			return
		case dup:
			out.Existing++
		default:
			out.Filed++
			out.Todos = append(out.Todos, s.todoGone(got))
		}
	}
	writeJSON(w, out)
}

// addExtracted stores an item filed by hand from an agent's message:
// deduplicated on its AutoKey, bound only by the global cap.
func (t *todoStore) addExtracted(it Todo) (out Todo, dup bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	active := 0
	for _, e := range t.data.Items {
		if e.AutoKey != "" && e.AutoKey == it.AutoKey {
			return e, true, nil
		}
		if e.active() {
			active++
		}
	}
	if active >= todoMaxActive {
		return it, false, errTodoFull
	}
	now := t.now()
	it.ID, it.CreatedAt, it.UpdatedAt, it.State = newTodoID(), now, now, TodoOpen
	t.data.Items = append(t.data.Items, it)
	return it, false, t.save()
}

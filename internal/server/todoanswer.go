package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/jhjaggars/comebye/internal/dialog"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
)

// Answering a hub-blocked to-do from the To do panel.
//
// A hub-blocked item (autotodo.go) carries the dialog it was filed for. A
// choice made on it ("ok 2", what the option buttons send) is not just
// recorded: it is sent to the agent's pane through the same answerDialog the
// dialog endpoint uses, with its fingerprint check and its dedupe, on the
// agent's own host. If the dialog has gone or changed since, nothing is
// typed: the item is dropped (or kept, for a duplicate) and the app is told.

var okOne = regexp.MustCompile(`(?i)^\s*ok\s+(\d+)\s*$`)

// blockedAgent finds the live agent an item waits on.
func (s *Server) blockedAgent(id string) (herdr.AgentInfo, bool) {
	for _, a := range s.fleet.Agents() {
		if state.AgentKey(&a) == id {
			return a, true
		}
	}
	return herdr.AgentInfo{}, false
}

// answerBlockedTodo handles an app answer to an item bound to a dialog. It
// reports whether it wrote the response; false leaves the answer to the
// ordinary path (any other kind of item, or one already closed).
func (s *Server) answerBlockedTodo(w http.ResponseWriter, r *http.Request, text string) bool {
	id := r.PathValue("id")
	it, ok := s.todos.get(id)
	if !ok || it.Source != SourceBlocked || it.Dialog == nil || !it.active() || it.Waiting == nil {
		return false
	}
	m := okOne.FindStringSubmatch(text)
	n := 0
	if m != nil {
		n, _ = strconv.Atoi(m[1])
	}
	if n < 1 || n > len(it.Dialog.Rows) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_answer", "message": "choose one of the dialog's options (ok 1 to ok " + strconv.Itoa(len(it.Dialog.Rows)) + ")"})
		return true
	}
	by := s.by(r, "")
	a, found := s.blockedAgent(it.Waiting.ID)
	if !found {
		s.dropBlocked(id, "the agent is gone")
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "agent_gone", "message": "the agent is gone: nothing was sent"})
		return true
	}
	body := answerBody{Action: dialog.ActionSelect, Index: it.Dialog.Rows[n-1], Fingerprint: it.Dialog.Fingerprint}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	status, resp := s.sendDialogAnswer(ctx, a, body)
	if status == http.StatusOK {
		// Answered. Record it without a notice: the dialog's own agent
		// needs no relay, and the user is the one who did it.
		if _, err := s.todos.answer(id, TodoAnswer{Text: text, By: by, Via: "dialog"}); err != nil {
			todoErr(w, err)
			return true
		}
		out, err := s.todos.close(id, TodoDone, by, "")
		if err != nil {
			todoErr(w, err)
			return true
		}
		writeJSON(w, map[string]any{"todo": s.todoGone(out)})
		return true
	}
	code, msg := respCode(resp)
	switch code {
	case "no_dialog":
		s.dropBlocked(id, "the dialog was already gone")
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": code, "message": "the agent is no longer at that dialog: nothing was sent"})
	case "dialog_changed":
		s.dropBlocked(id, "the dialog changed")
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": code, "message": "the dialog changed since this item was filed: nothing was sent. Open the agent to see it"})
	default: // duplicate, unreachable host, herdr refusal: the item stays
		if msg == "" {
			msg = "the answer could not be sent"
		}
		if status < 400 {
			status = http.StatusBadGateway
		}
		writeJSONStatus(w, status, map[string]string{"code": code, "message": msg})
	}
	return true
}

// dropBlocked closes the item as no longer applying.
func (s *Server) dropBlocked(id, reason string) {
	_, _ = s.todos.close(id, TodoDropped, AutoCreator, reason)
}

// sendDialogAnswer answers the agent's dialog on whichever host owns it,
// through the answer endpoint's own logic.
func (s *Server) sendDialogAnswer(ctx context.Context, a herdr.AgentInfo, body answerBody) (int, any) {
	if s.isLocalAgent(a) {
		cur := a
		if fresh := s.resolveAgent(a.PaneID); fresh != nil {
			cur = *fresh
		}
		status, resp := answerDialog(ctx, s.client, cur, s.pendingAsk(cur), body, answers, time.Now())
		if s.manager != nil && status == http.StatusOK {
			s.manager.Refresh()
		}
		return status, resp
	}
	raw, _ := json.Marshal(body)
	status, data, err := s.fleet.CallAgent(ctx, a.Host, a.PaneID, "answer", raw)
	if err != nil {
		return errorBody(err)
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		out = map[string]any{"message": string(data)}
	}
	return status, out
}

// respCode pulls the code and message out of an answer response body.
func respCode(resp any) (code, msg string) {
	switch v := resp.(type) {
	case map[string]string:
		return v["code"], v["message"]
	case map[string]any:
		c, _ := v["code"].(string)
		m, _ := v["message"].(string)
		return c, m
	}
	return "", ""
}

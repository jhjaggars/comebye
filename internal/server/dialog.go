package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/dialog"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Tap-to-answer for agent dialogs.
//
//	GET  /api/agents/{target}/dialog → {"dialog": Dialog|null}
//	POST /api/agents/{target}/answer   {"action","index","fingerprint"}
//
// The answer is a check-then-send: the server re-reads the screen (and the
// transcript), re-detects the dialog, refuses with 409 dialog_changed if its
// fingerprint differs from the one the phone rendered, computes the keys
// from the *current* highlight, and sends them in one agent.send_keys call.

// paneIO is the slice of the herdr client the dialog endpoints use.
type paneIO interface {
	ReadPane(ctx context.Context, paneID, source string, lines int) (herdr.ReadResult, error)
	SendKeys(ctx context.Context, target string, keys []string) error
}

// answerDedupeWindow rejects a second answer to the same dialog content: the
// screen can still show a just-answered dialog for a moment, and a double tap
// would otherwise answer it (or the next identical one) twice.
const answerDedupeWindow = 3 * time.Second

// answerDedupe remembers recently answered (pane, fingerprint) pairs.
type answerDedupe struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newAnswerDedupe() *answerDedupe { return &answerDedupe{seen: map[string]time.Time{}} }

// claim records key unless it was claimed within the window; it reports
// whether the caller may proceed.
func (d *answerDedupe) claim(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, t := range d.seen {
		if now.Sub(t) >= answerDedupeWindow {
			delete(d.seen, k)
		}
	}
	if _, ok := d.seen[key]; ok {
		return false
	}
	d.seen[key] = now
	return true
}

func (d *answerDedupe) release(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen, key)
}

// answers is the process-wide dedupe table for the answer endpoint.
var answers = newAnswerDedupe()

// pendingAsk returns the agent's unanswered transcript question, if any.
// Read failures just mean "no ask": the screen is still checked.
// Pi's ask_user_question and Claude Code's AskUserQuestion both arrive here.
func (s *Server) pendingAsk(agent herdr.AgentInfo) *transcript.Ask {
	path, err := transcript.SessionPath(agent, s.manager.Agents())
	if err != nil {
		return nil
	}
	t, err := transcript.Read(path, 1, 0)
	if err != nil {
		return nil
	}
	return t.Ask
}

// currentDialog detects the dialog the agent is waiting on. Only agents that
// are actually waiting are examined: herdr says blocked, or a transcript
// question is open. Claude Code's AskUserQuestion does not reliably flip
// herdr to blocked (the coordinator showed "done" while stuck on one), so an
// open question counts whatever the status. An idle agent's prompt box must
// never be "answered" with an Enter, so without either signal nothing is read.
func currentDialog(ctx context.Context, io paneIO, agent herdr.AgentInfo, ask *transcript.Ask) (*dialog.Dialog, error) {
	if agent.Status != herdr.StatusBlocked && ask == nil {
		return nil, nil
	}
	read, err := io.ReadPane(ctx, agent.PaneID, "visible", 0)
	if err != nil {
		return nil, err
	}
	d := dialog.Resolve(read.Text, agent.Agent, ask)
	// A question the transcript still calls open but that is not on screen
	// is only believable while the agent is working; on a finished agent it
	// is a stale record (the session was killed or restarted).
	if d != nil && d.Kind == dialog.KindAsk && agent.Status != herdr.StatusWorking && agent.Status != herdr.StatusBlocked {
		return nil, nil
	}
	return d, nil
}

// errDialogOpen refuses free text for an agent that has a dialog on screen:
// the text and Enter would be typed into the dialog (a multi-select's
// free-text row, for one) instead of reaching the prompt.
var errDialogOpen = &herdr.Error{Code: "dialog_open", Message: "the agent is waiting on a question: answer it first (or cancel it)"}

// guardPrompt returns errDialogOpen when agent has a question on screen.
// herdr only refuses prompts while the agent is blocked, which is not
// reliable for Claude's multi-select. Read failures fail open: herdr's own
// refusal still applies.
func (s *Server) guardPrompt(ctx context.Context, agent herdr.AgentInfo) error {
	return promptGuard(ctx, s.client, agent, s.pendingAsk(agent))
}

func promptGuard(ctx context.Context, io paneIO, agent herdr.AgentInfo, ask *transcript.Ask) error {
	if ask == nil {
		return nil
	}
	d, err := currentDialog(ctx, io, agent, ask)
	if err != nil || d == nil || d.Kind == dialog.KindAsk {
		return nil
	}
	return errDialogOpen
}

func (s *Server) handleDialog(w http.ResponseWriter, r *http.Request) {
	agent := s.resolveAgent(r.PathValue("target"))
	if agent == nil {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	d, err := currentDialog(ctx, s.client, *agent, s.pendingAsk(*agent))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"dialog": d})
}

// handleActivity reports what a working agent's own status line says it is
// doing (GET /api/agents/{target}/activity → {"activity": Activity|null}).
// The phone polls it while it shows a working agent, so "compacting" and
// the current todo show instead of a bare "Thinking". Agents that are not
// working are not read.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	agent := s.resolveAgent(r.PathValue("target"))
	if agent == nil {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	if agent.Status != herdr.StatusWorking {
		writeJSON(w, map[string]any{"activity": nil})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	read, err := s.client.ReadPane(ctx, agent.PaneID, "visible", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"activity": dialog.DetectActivity(read.Text, agent.Agent)})
}

// answerBody is the JSON body of an answer request.
type answerBody struct {
	Action      string `json:"action"`
	Index       int    `json:"index"`
	Fingerprint string `json:"fingerprint"`
}

func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	var body answerBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	agent := s.resolveAgent(r.PathValue("target"))
	if agent == nil {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	status, resp := answerDialog(ctx, s.client, *agent, s.pendingAsk(*agent), body, answers, time.Now())
	writeJSONStatus(w, status, resp)
}

// answerDialog is the answer endpoint's logic, separated for tests. It
// returns the HTTP status and JSON body.
func answerDialog(ctx context.Context, io paneIO, agent herdr.AgentInfo, ask *transcript.Ask, body answerBody, dd *answerDedupe, now time.Time) (int, any) {
	switch body.Action {
	case dialog.ActionSelect, dialog.ActionToggle, dialog.ActionSubmit, dialog.ActionCancel, dialog.ActionReset:
	default:
		return http.StatusBadRequest, map[string]string{"code": "bad_answer", "message": "action must be select, toggle, submit, cancel or reset"}
	}
	if body.Fingerprint == "" {
		return http.StatusBadRequest, map[string]string{"code": "bad_answer", "message": "fingerprint is required"}
	}
	d, err := currentDialog(ctx, io, agent, ask)
	if err != nil {
		return errorBody(err)
	}
	if d == nil {
		return http.StatusConflict, map[string]any{"code": "no_dialog", "message": "the agent is not waiting on a dialog", "dialog": nil}
	}
	if d.Fingerprint != body.Fingerprint {
		return http.StatusConflict, map[string]any{"code": "dialog_changed", "message": "the question changed", "dialog": d}
	}
	keys, err := dialog.Keys(d, body.Action, body.Index)
	if err != nil {
		return http.StatusBadRequest, map[string]any{"code": "bad_answer", "message": err.Error(), "dialog": d}
	}
	claimKey := agent.PaneID + "\x00" + d.Fingerprint
	if body.Action == dialog.ActionReset {
		// Leaving the text field changes nothing in the fingerprint; keep it
		// from blocking the answer that follows.
		claimKey += "\x00reset"
	}
	if !dd.claim(claimKey, now) {
		return http.StatusConflict, map[string]any{"code": "duplicate", "message": "already answered", "dialog": d}
	}
	err = io.SendKeys(ctx, agent.PaneID, keys)
	var he *herdr.Error
	if err != nil && d.Keys == dialog.KeysDigits && body.Action == dialog.ActionSelect && errors.As(err, &he) &&
		he.Code != "pane_not_found" && he.Code != "agent_not_found" {
		// herdr validates every key before writing any, so a structured
		// refusal (e.g. digits unsupported) wrote nothing: use arrows.
		if alt, aerr := dialog.ArrowKeys(d, body.Action, body.Index); aerr == nil {
			keys = alt
			err = io.SendKeys(ctx, agent.PaneID, keys)
		}
	}
	if err != nil {
		dd.release(claimKey)
		return errorBody(err)
	}
	return http.StatusOK, map[string]any{"ok": true, "keys": keys}
}

// errorBody mirrors writeError for handlers that return (status, body).
func errorBody(err error) (int, any) {
	var he *herdr.Error
	if errors.As(err, &he) {
		status := http.StatusBadGateway
		switch he.Code {
		case "agent_blocked", "dialog_open":
			status = http.StatusConflict
		case "pane_not_found", "agent_not_found":
			status = http.StatusNotFound
		}
		return status, map[string]string{"code": he.Code, "message": he.Message}
	}
	return http.StatusBadGateway, map[string]string{"code": "herdr_error", "message": err.Error()}
}

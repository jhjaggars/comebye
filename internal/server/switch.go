package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/state"
)

// Switching the model of a running agent. A running agent takes no
// command-line flags, so the switch is the agent's own slash command typed
// into its prompt, checked by hand against the real programs:
//
//	claude: /model <alias|id>
//	pi:     /model provider/id and /thinking <level>
//
// pi's /model with a name it does not have opens its model selector and
// leaves it open, so a pi model is only sent when it is in pi's list on
// that host. Other kinds have no slash command that was verified and are
// refused.

// switchKinds are the kinds whose running agents can be switched.
var switchKinds = []string{"pi", "claude"}

// switchCommands is the slash commands that switch a running agent of kind
// to model (and pi's thinking level), after checking them like a launch's.
func switchCommands(kind, model, thinking string, catalog *modelCatalog) ([]string, *modelError) {
	model, thinking = strings.TrimSpace(model), strings.TrimSpace(thinking)
	ok := false
	for _, k := range switchKinds {
		ok = ok || k == kind
	}
	if !ok {
		return nil, &modelError{"switch_unsupported", "switching a running " + kind + " agent's model is not supported (supported: " + strings.Join(switchKinds, ", ") + ")"}
	}
	if model == "" && thinking == "" {
		return nil, &modelError{"model_required", "name a model, or for pi a thinking level: a running agent cannot go back to its default without a name"}
	}
	if _, err := modelArgs(kind, model, thinking, catalog); err != nil {
		return nil, err
	}
	var cmds []string
	if model != "" {
		cmds = append(cmds, "/model "+model)
	}
	if thinking != "" {
		cmds = append(cmds, "/thinking "+thinking) // after the model: a new model may clamp the level
	}
	return cmds, nil
}

type switchBody struct {
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
}

// switchModel does the switch on this host: status and JSON answer.
func (s *Server) switchModel(ctx context.Context, target string, b switchBody) (int, any) {
	fail := func(status int, code, msg string) (int, any) {
		return status, map[string]string{"code": code, "message": msg}
	}
	agent := s.resolveAgent(target)
	if agent == nil {
		return fail(http.StatusNotFound, "agent_not_found", "no such agent")
	}
	var cat *modelCatalog
	if agent.Agent == "pi" {
		cat = s.modelCatalog(ctx, "pi")
	}
	cmds, merr := switchCommands(agent.Agent, b.Model, b.Thinking, cat)
	if merr != nil {
		return fail(http.StatusBadRequest, merr.code, merr.message)
	}
	// A slash command typed while the agent works would be queued as a
	// message, and one typed over a dialog would answer it.
	if agent.Status != herdr.StatusIdle {
		return fail(http.StatusConflict, "agent_busy", "the agent is "+string(agent.Status)+": switch its model when it is idle")
	}
	if err := s.guardPrompt(ctx, *agent); err != nil {
		return fail(http.StatusConflict, "dialog_open", err.Error())
	}
	for i, c := range cmds {
		if i > 0 {
			select {
			case <-ctx.Done():
				return fail(http.StatusGatewayTimeout, "timeout", "timed out between commands")
			case <-time.After(s.switchGap):
			}
		}
		if _, err := s.client.Prompt(ctx, agent.PaneID, c, herdr.PromptOptions{}); err != nil {
			return fail(http.StatusBadGateway, "switch_failed", "could not send "+c+": "+errMessage(err))
		}
	}
	return http.StatusOK, map[string]any{
		"agent": map[string]string{"pane_id": agent.PaneID, "name": agent.Name, "agent": agent.Agent},
		"model": strings.TrimSpace(b.Model), "thinking": strings.TrimSpace(b.Thinking), "commands": cmds,
	}
}

// handleSwitchModel is POST /api/[hosts/{host}/]agents/{target}/model:
// {model?, thinking?}. The agent's host runs the switch (a peer's through
// the hub); the hub then updates the handoff entry of a worker, so the
// ledger shows the model it runs now.
func (s *Server) handleSwitchModel(w http.ResponseWriter, r *http.Request) {
	host, target := r.PathValue("host"), r.PathValue("target")
	if host == "" {
		host = s.fleet.LocalName()
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	var b switchBody
	if err != nil || json.Unmarshal(raw, &b) != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var status int
	var data []byte
	switch {
	case host == s.fleet.LocalName():
		var payload any
		status, payload = s.switchModel(ctx, target, b)
		data, _ = json.Marshal(payload)
	case s.fleet.HasPeer(host):
		if status, data, err = s.fleet.CallAgent(ctx, host, target, "model", raw); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, map[string]string{"code": "host_unreachable", "message": err.Error()})
			return
		}
	default:
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "unknown_host", "message": "no such host"})
		return
	}
	if status == http.StatusOK && s.ledger != nil {
		var res struct {
			Agent struct {
				PaneID string `json:"pane_id"`
			} `json:"agent"`
		}
		if json.Unmarshal(data, &res) == nil && res.Agent.PaneID != "" {
			id := state.QualifiedID(host, res.Agent.PaneID)
			if e, ok := s.ledger.ByAgent(id); ok {
				_, _, uerr := s.ledger.Update(e.ID, func(e *ledger.Entry) {
					if m := strings.TrimSpace(b.Model); m != "" {
						e.Model = m
					}
					if t := strings.TrimSpace(b.Thinking); t != "" {
						e.Thinking = t
					}
				})
				if uerr != nil {
					log.Printf("ledger: %v", uerr)
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

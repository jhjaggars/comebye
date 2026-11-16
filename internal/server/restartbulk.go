package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/park"
	"github.com/jhjaggars/comebye/internal/state"
)

// Bulk restart: every eligible agent of one kind on one host, one at a time,
// after a key rotation. Coordinators are never touched. A refusal or a
// failure of one agent does not stop the others, and a dry run changes
// nothing. The user's alone, like a single restart.

// bulkBody is the body of POST /api/restart/bulk. On the host route
// (POST /api/hosts/{host}/restart/bulk) the host comes from the path.
type bulkBody struct {
	Host   string `json:"host"`
	Kind   string `json:"kind"`
	DryRun bool   `json:"dry_run"`
}

// Outcomes of one agent in a bulk restart.
const (
	bulkRestarted    = "restarted"
	bulkWouldRestart = "would_restart" // a dry run: it passed every check
	bulkRefused      = "refused"       // a check said no; the agent is untouched
	bulkFailed       = "failed"        // something broke (see the code)
	bulkSkipped      = "skipped"       // a coordinator
)

// bulkRow is one agent's result.
type bulkRow struct {
	Agent   string `json:"agent"` // host/pane
	PaneID  string `json:"pane_id"`
	Name    string `json:"name,omitempty"`
	Outcome string `json:"outcome"`
	// Code and Message say why for refused, failed and skipped.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	OldPID  int    `json:"old_pid,omitempty"`
	NewPID  int    `json:"new_pid,omitempty"`
	Session string `json:"session,omitempty"`
	// FlagsRecovered is false when only the session could be kept (pi).
	FlagsRecovered *bool  `json:"flags_recovered,omitempty"`
	Warning        string `json:"warning,omitempty"`
}

// bulkResult is the answer of a bulk restart.
type bulkResult struct {
	Host         string    `json:"host"`
	Kind         string    `json:"kind"`
	DryRun       bool      `json:"dry_run,omitempty"`
	Results      []bulkRow `json:"results"`
	Restarted    int       `json:"restarted"`
	WouldRestart int       `json:"would_restart"`
	Refused      int       `json:"refused"`
	Failed       int       `json:"failed"`
	Skipped      int       `json:"skipped"`
}

func (s *Server) handleRestartBulk(w http.ResponseWriter, r *http.Request) {
	var body bulkBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if h := r.PathValue("host"); h != "" {
		body.Host = h
	}
	if body.Host == "" {
		body.Host = s.fleet.LocalName()
	}
	if body.Kind == "" || !park.Supported(body.Kind) {
		writeLaunchError(w, &launchError{status: http.StatusBadRequest, code: "bad_request", message: "which kind? (kind: claude, pi or opencode)"})
		return
	}
	local := body.Host == s.fleet.LocalName()
	if !local && !s.fleet.HasPeer(body.Host) {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "unknown_host", "message": "no such host"})
		return
	}
	res := s.restartBulk(r.Context(), body, local, s.by(r, ""))
	writeJSON(w, res)
}

// bulkTargets are the agents of kind on host, in pane order.
func (s *Server) bulkTargets(host, kind string, local bool) []herdr.AgentInfo {
	var src []herdr.AgentInfo
	if local {
		src = s.localAgents()
	} else {
		for _, a := range s.fleet.Agents() {
			if a.Host == host {
				src = append(src, a)
			}
		}
	}
	var out []herdr.AgentInfo
	for _, a := range src {
		if a.Agent == kind {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PaneID < out[j].PaneID })
	return out
}

func (s *Server) restartBulk(ctx context.Context, b bulkBody, local bool, by string) *bulkResult {
	res := &bulkResult{Host: b.Host, Kind: b.Kind, DryRun: b.DryRun, Results: []bulkRow{}}
	for _, a := range s.bulkTargets(b.Host, b.Kind, local) {
		id := state.QualifiedID(b.Host, a.PaneID)
		row := bulkRow{Agent: id, PaneID: a.PaneID, Name: a.Name}
		switch {
		case s.isCoordinator(id, a.Name):
			row.Outcome, row.Code, row.Message = bulkSkipped, "coordinator", "a coordinator is never restarted in bulk; restart it by name"
		case ctx.Err() != nil:
			row.Outcome, row.Code, row.Message = bulkFailed, "canceled", "the request ended before this agent's turn; it was not touched"
		default:
			s.bulkOne(ctx, &row, restartBody{Agent: a.PaneID, Name: a.Name, DryRun: b.DryRun}, b.Host, local, by)
		}
		switch row.Outcome {
		case bulkRestarted:
			res.Restarted++
		case bulkWouldRestart:
			res.WouldRestart++
		case bulkRefused:
			res.Refused++
		case bulkFailed:
			res.Failed++
		case bulkSkipped:
			res.Skipped++
		}
		res.Results = append(res.Results, row)
	}
	return res
}

// bulkOne restarts one agent, here or through the peer, and fills in row.
func (s *Server) bulkOne(ctx context.Context, row *bulkRow, rb restartBody, host string, local bool, by string) {
	var rr *restartResult
	var code, msg string
	var status int
	if local {
		var le *launchError
		rr, le = s.restartAgent(ctx, rb, by)
		if le != nil {
			status, code, msg = le.status, le.code, le.message
		}
	} else {
		raw, _ := json.Marshal(rb)
		st, data, err := s.fleet.CallHost(ctx, host, "restart", nil, raw)
		switch {
		case err != nil:
			status, code, msg = http.StatusBadGateway, "host_error", "the host could not be reached: "+err.Error()
		case st/100 != 2:
			var e struct{ Code, Message string }
			_ = json.Unmarshal(data, &e)
			status, code, msg = st, e.Code, e.Message
			if msg == "" {
				msg = strings.TrimSpace(string(data))
			}
		default:
			rr = &restartResult{}
			if err := json.Unmarshal(data, rr); err != nil {
				rr, status, code, msg = nil, http.StatusBadGateway, "host_error", "the host's answer could not be read"
			}
		}
	}
	if rr == nil {
		row.Code, row.Message = code, msg
		// A check said no (409), or there is nothing there: refused. Anything
		// else broke.
		if status == http.StatusConflict || status == http.StatusNotFound {
			row.Outcome = bulkRefused
		} else {
			row.Outcome = bulkFailed
		}
		return
	}
	row.Outcome = bulkRestarted
	if rr.DryRun {
		row.Outcome = bulkWouldRestart
	}
	row.OldPID, row.NewPID, row.Session, row.Warning = rr.OldPID, rr.NewPID, rr.Session, rr.Warn
	rec := rr.ArgsFrom != "session"
	row.FlagsRecovered = &rec
}

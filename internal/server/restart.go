package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/park"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Restart an agent in place.
//
// The agent's process is ended with its own exit command, and the same
// agent kind is started again in the same pane on the same session, so a
// changed environment (a rotated key, a new config) takes effect without the
// user restarting by hand. It works for any idle claude, pi or opencode
// agent: named or not, in a worktree or a repository's primary checkout. Park's
// rules (a named agent in a linked worktree) do not apply, since nothing is
// closed and nothing is removed.
//
// The hub never kills anything. The exit is the agent's own (`/exit` for
// claude and opencode, Ctrl+D at pi's empty editor); if the pane's shell is
// not foreground again within restartExitWait the restart is abandoned and the
// agent is left as it was.

// restartExitWait is how long the old process gets to end.
const restartExitWait = 20 * time.Second

// restartBody is the body of POST /api/restart.
type restartBody struct {
	// Agent is the pane id; Name, when set, must still be its name.
	Agent string `json:"agent"`
	Name  string `json:"name"`
	// ForceSchedule restarts a claude session that has a loop or wakeup
	// scheduled (a restart ends it).
	ForceSchedule bool `json:"force_schedule"`
}

// restartResult is the answer of a restart.
type restartResult struct {
	Agent browserAgent `json:"agent"`
	// Session is the session the agent runs on, before and after.
	Session string `json:"session"`
	OldPID  int    `json:"old_pid,omitempty"`
	NewPID  int    `json:"new_pid,omitempty"`
	// Args are the arguments the agent was started with; ArgsFrom says where
	// they came from: "recorded" (the launch the hub or park noted), "process"
	// (the live command line) or "session" (the session alone: pi hides its
	// flags).
	Args     []string `json:"args"`
	ArgsFrom string   `json:"args_from"`
	Note     string   `json:"note,omitempty"`
	Warn     string   `json:"warning,omitempty"`
}

// restartPending is a restart whose old process has ended but whose start did
// not work. The pane is at a shell with no agent in it, so a retry of the
// request has to carry on from here.
type restartPending struct {
	name        string // "" for an unnamed agent
	kind        string
	sessionKind string
	session     string
	args        []string // the full start arguments
	base        []string // the launch arguments without the session flag
	from        string
	unrecovered bool
	oldPID      int
	at          time.Time
}

// restartPendingTTL is how long a failed restart waits for its retry.
const restartPendingTTL = time.Hour

// restartState guards the per-pane restarts.
type restartState struct {
	mu      sync.Mutex
	busy    map[string]bool
	pending map[string]*restartPending
}

func (r *restartState) begin(pane string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.busy[pane] {
		return false
	}
	if r.busy == nil {
		r.busy = map[string]bool{}
	}
	r.busy[pane] = true
	return true
}

func (r *restartState) end(pane string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.busy, pane)
}

func (r *restartState) pendingFor(pane string) *restartPending {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pending[pane]
	if p != nil && time.Since(p.at) > restartPendingTTL {
		delete(r.pending, pane)
		return nil
	}
	return p
}

func (r *restartState) setPending(pane string, p *restartPending) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p == nil {
		delete(r.pending, pane)
		return
	}
	if r.pending == nil {
		r.pending = map[string]*restartPending{}
	}
	r.pending[pane] = p
}

// handleRestart is POST /api/restart: end the agent's process and start it
// again in the same pane on the same session. The user's alone.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	var body restartBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.restartWait+launchStartTimeout+60*time.Second)
	defer cancel()
	res, lerr := s.restartAgent(ctx, body, s.by(r, ""))
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	writeJSON(w, res)
}

// restartExit is how a kind's process is ended: text typed as a prompt, or a
// key. Checked live: claude and opencode take /exit; pi leaves on Ctrl+D at an
// empty editor (the input guard has just checked it is empty).
func restartExit(kind string) (text, key string) {
	if kind == "pi" {
		return "", "ctrl+d"
	}
	return "/exit", ""
}

// shellForeground reports whether the pane's foreground process is its shell
// again.
func shellForeground(pi herdr.ProcessInfo) bool {
	if pi.ShellPID == 0 {
		return false
	}
	if pi.ProcessGroup == pi.ShellPID {
		return true
	}
	if len(pi.Processes) == 0 {
		return false
	}
	for _, p := range pi.Processes {
		if p.PID != pi.ShellPID {
			return false
		}
	}
	return true
}

func restartRefusal(status int, code, msg string) *launchError {
	return &launchError{status: status, code: code, message: msg + "; nothing was closed"}
}

func (s *Server) restartAgent(ctx context.Context, b restartBody, by string) (*restartResult, *launchError) {
	if b.Agent == "" {
		return nil, &launchError{status: http.StatusBadRequest, code: "bad_request", message: "which agent? (agent: its pane id)"}
	}
	if !s.restart.begin(b.Agent) {
		return nil, &launchError{status: http.StatusConflict, code: "restart_running", message: "a restart of that pane is already under way"}
	}
	defer s.restart.end(b.Agent)

	live, err := s.client.ListAgents(ctx)
	if err != nil {
		return nil, herdrFailure("list agents", err)
	}
	var agent *herdr.AgentInfo
	for i := range live {
		if live[i].PaneID == b.Agent {
			agent = &live[i]
		}
	}
	if agent == nil {
		// A restart whose old process ended but whose start failed: carry on.
		if p := s.restart.pendingFor(b.Agent); p != nil && (b.Name == "" || b.Name == p.name) {
			return s.restartStart(ctx, b.Agent, p, by)
		}
		return nil, &launchError{status: http.StatusNotFound, code: "no_such_agent", message: "no agent runs in that pane; nothing was closed"}
	}
	// An agent runs in the pane: any earlier failed restart is moot.
	s.restart.setPending(b.Agent, nil)
	if b.Name != "" && agent.Name != b.Name {
		return nil, &launchError{status: http.StatusConflict, code: "agent_changed", message: "another agent runs in that pane now; nothing was closed"}
	}
	kind := agent.Agent
	if !park.Supported(kind) {
		return nil, restartRefusal(http.StatusConflict, "kind_not_restartable", "only claude, pi and opencode agents can be restarted, not "+kind)
	}
	if agent.Status == herdr.StatusWorking || agent.Status == herdr.StatusBlocked {
		return nil, restartRefusal(http.StatusConflict, "agent_working", "the agent is "+string(agent.Status)+"; restart it when it is idle")
	}
	// The manager's copy carries the background tasks (herdr's list does not).
	for _, la := range s.localAgents() {
		if la.PaneID == agent.PaneID && len(la.Background) > 0 {
			t := la.Background[0]
			return nil, restartRefusal(http.StatusConflict, "background_task", "a background task is running ("+strings.TrimSpace(t.Kind+" "+t.Label)+"); a restart would kill it")
		}
	}
	// A dialog open, or text typed in the input box.
	if err := s.guardPrompt(ctx, *agent); err != nil {
		var he *herdr.Error
		switch {
		case errors.Is(err, errDialogOpen):
			return nil, restartRefusal(http.StatusConflict, "dialog_open", "a dialog is open in the pane; answer it first")
		case errors.As(err, &he) && (he.Code == "dialog_open" || he.Code == "input_busy"):
			return nil, restartRefusal(http.StatusConflict, he.Code, he.Message)
		}
		return nil, restartRefusal(http.StatusBadGateway, "herdr_error", "could not check the pane: "+errMessage(err))
	}
	sk, sv, lerr := s.sessionRef(ctx, *agent, live)
	if lerr != nil {
		return nil, lerr
	}
	if kind == "claude" && !b.ForceSchedule {
		path := transcript.DefaultResolver.SessionFileByID(sv, agent.CWD)
		pend, err := pendingSchedule(path, time.Now())
		switch {
		case path == "" || err != nil:
			return nil, restartRefusal(http.StatusConflict, "schedule_uncertain", "the session could not be read for a loop or wakeup (force_schedule restarts anyway)")
		case pend:
			return nil, restartRefusal(http.StatusConflict, "schedule_pending", "the session has a loop or wakeup scheduled, which a restart ends (force_schedule restarts anyway)")
		}
	}

	// The arguments, before anything closes: the launch the hub recorded, else
	// the live command line, else (pi hides its flags) the session alone.
	p := &restartPending{name: agent.Name, kind: kind, sessionKind: sk, session: sv, at: time.Now()}
	var proc herdr.Process
	pi, perr := s.client.ProcessInfo(ctx, agent.PaneID)
	if perr == nil {
		proc, _ = pi.Find(kind)
	}
	if proc.PID != 0 {
		p.oldPID = proc.PID
	}
	switch launch, ok := s.recordedLaunch(agent.Name); {
	case ok && launch.Kind == kind:
		p.base, p.from = launch.Args, "recorded"
		p.args, err = park.ResumeArgs(kind, sk, sv, launch.Args)
	case proc.PID != 0 && (len(proc.Argv) > 1 || kind != "pi"):
		p.args, err = park.RestartArgs(kind, proc.Argv, sk, sv)
		p.from = "process"
		if err == nil {
			p.base = p.args[2:]
		}
	case kind == "pi":
		// pi rewrites its process title: its flags are not readable.
		p.args, err = park.ResumeArgs(kind, sk, sv, nil)
		p.from, p.unrecovered = "session", true
	default:
		return nil, restartRefusal(http.StatusBadGateway, "args_unknown", "the agent's command line could not be read ("+errString(perr)+"), so its flags cannot be kept")
	}
	if err != nil {
		return nil, restartRefusal(http.StatusConflict, "not_restartable", err.Error())
	}

	// The state may have moved since the list was read.
	if cur, err := s.client.GetAgent(ctx, agent.PaneID); err == nil && (cur.Status == herdr.StatusWorking || cur.Status == herdr.StatusBlocked) {
		return nil, restartRefusal(http.StatusConflict, "agent_working", "the agent is "+string(cur.Status)+"; restart it when it is idle")
	}

	// End the process with its own exit.
	text, key := restartExit(kind)
	var serr error
	if key != "" {
		serr = s.client.SendKeys(ctx, agent.PaneID, []string{key})
	} else {
		_, serr = s.client.Prompt(ctx, agent.PaneID, text, herdr.PromptOptions{})
	}
	if serr != nil {
		return nil, restartRefusal(http.StatusBadGateway, "exit_failed", "could not send the exit: "+errMessage(serr))
	}
	deadline := time.Now().Add(s.restartWait)
	for {
		// The shell is foreground again, and herdr has let go of the agent
		// (it holds its name for a moment after the process ends: starting
		// then fails with "agent name ... is already used").
		if pi, err := s.client.ProcessInfo(ctx, agent.PaneID); err == nil && shellForeground(pi) && s.paneAgentGone(ctx, agent.PaneID) {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			s.auditRestart(by, *agent, p, false, fmt.Sprintf("the process did not end within %s; left running", s.restartWait))
			return nil, &launchError{status: http.StatusGatewayTimeout, code: "exit_not_confirmed",
				message: fmt.Sprintf("the agent did not end within %s after its exit command; it was left running (nothing was killed)", s.restartWait)}
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.restartPoll):
		}
	}
	s.restart.setPending(agent.PaneID, p)
	return s.restartStart(ctx, agent.PaneID, p, by)
}

// paneAgentGone reports whether herdr no longer lists an agent in the pane.
func (s *Server) paneAgentGone(ctx context.Context, pane string) bool {
	live, err := s.client.ListAgents(ctx)
	if err != nil {
		return false
	}
	for _, a := range live {
		if a.PaneID == pane {
			return false
		}
	}
	return true
}

func errString(err error) string {
	if err == nil {
		return "no such process"
	}
	return errMessage(err)
}

// recordedLaunch is how the hub or park started the agent called name.
func (s *Server) recordedLaunch(name string) (park.Launch, bool) {
	if name == "" {
		return park.Launch{}, false
	}
	ps := s.parkStore()
	if ps == nil {
		return park.Launch{}, false
	}
	return ps.LaunchOf(name)
}

// restartStart starts the agent again in a pane whose old process is gone.
func (s *Server) restartStart(ctx context.Context, pane string, p *restartPending, by string) (*restartResult, *launchError) {
	name := p.name
	temp := name == ""
	if temp {
		var rnd [4]byte
		_, _ = rand.Read(rnd[:])
		name = "restart-" + hex.EncodeToString(rnd[:])
	}
	snap := herdr.AgentInfo{PaneID: pane, Name: p.name, Agent: p.kind}
	agent, err := s.startAgent(ctx, name, p.kind, pane, p.args...)
	ps := s.parkStore()
	if err != nil {
		if ps != nil && temp {
			ps.ForgetLaunch(name)
		}
		s.auditRestart(by, snap, p, false, "the start failed: "+errMessage(err))
		return nil, &launchError{status: http.StatusBadGateway, code: "start_failed",
			message: "the old process ended but " + p.kind + " did not start again: " + errMessage(err) + ". The pane is at its shell; send the restart again to retry"}
	}
	s.restart.setPending(pane, nil)
	if ps != nil {
		// startAgent noted the resume flags as the launch; a park later must
		// keep the original arguments, not stack a second resume flag.
		if temp || p.unrecovered {
			// Nothing real to remember: the launch's flags are unknown, and
			// noting the session-only start would pass for them next time.
			ps.ForgetLaunch(name)
		} else {
			ps.NoteLaunch(name, p.kind, p.base, time.Now())
		}
	}
	if agent.PaneID == "" {
		agent.PaneID = pane
	}
	target := name
	if temp {
		if err := s.client.RenameAgent(ctx, pane, ""); err != nil {
			log.Printf("restart: could not clear the temporary name %s of %s: %v", name, pane, err)
		}
		target = pane
	}
	if s.manager != nil {
		s.manager.Refresh()
	}
	host := s.fleet.LocalName()
	res := &restartResult{
		Agent:   browserAgent{ID: state.QualifiedID(host, pane), Host: host, PaneID: pane, Name: p.name, Agent: p.kind, Status: agent.Status, CWD: agent.CWD},
		Session: p.session, OldPID: p.oldPID, Args: p.args, ArgsFrom: p.from,
	}
	if pi, err := s.client.ProcessInfo(ctx, pane); err == nil {
		if np, ok := pi.Find(p.kind); ok {
			res.NewPID = np.PID
		}
	}
	res.Note = restartNote(p)
	if queued, err := s.deliverOrQueue(ctx, res.Agent.ID, target, res.Note); err != nil {
		res.Warn = "the agent restarted, but the catch-up note was not delivered: " + err.Error()
	} else if queued {
		res.Warn = "the agent restarted at a dialog: the catch-up note is queued and goes in once it is answered"
	}
	s.auditRestart(by, snap, p, true, fmt.Sprintf("pid %d -> %d, args from %s", p.oldPID, res.NewPID, p.from))
	if s.fleet != nil {
		s.fleet.Changed()
	}
	return res, nil
}

// restartNote is what the restarted agent is told first.
func restartNote(p *restartPending) string {
	var sb strings.Builder
	sb.WriteString("Comebye: you were restarted in place just now, in the same pane and on the same session, so that a changed environment (a rotated key, a new setting) takes effect. ")
	sb.WriteString("A new process runs your session, so nothing you had running survived: check any background task, loop, wakeup or dev server you started.")
	if p.unrecovered {
		sb.WriteString(" Your launch flags could not be read back (pi hides them), so you were started with the session alone: pi restores the session's last model, but flags such as a system prompt, skills or tool limits were not re-applied.")
	}
	return sb.String()
}

func (s *Server) auditRestart(by string, a herdr.AgentInfo, p *restartPending, ok bool, detail string) {
	who := a.Name
	if who == "" {
		who = "unnamed " + a.Agent
	}
	s.auditBy(ledger.AuditEvent{By: by, Action: "agent.restart", Host: s.fleet.LocalName(),
		Detail: fmt.Sprintf("%s (%s) %s: %s", who, a.PaneID, p.kind, detail), OK: ok})
}

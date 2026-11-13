package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/park"
	"github.com/jhjaggars/comebye/internal/scratch"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// Park and resume. Parking closes an agent's pane and keeps its worktree,
// branch and session; a park record (internal/park, parked.json in the state
// directory) keeps what a resume needs. Resume starts a new pane in the same
// worktree with the agent kind's own resume flag and tells the agent what
// happened. The host writes the record before it closes the pane, so a park
// is recorded whatever the hub is doing.

const (
	// parkHistory is how long a resumed agent's record stays as history.
	parkHistory = 30 * 24 * time.Hour
	// opencodeListTimeout bounds `opencode session list`.
	opencodeListTimeout = 20 * time.Second
)

// parkStore opens the store on first use; nil when there is no state directory
// or it cannot be read (parking then answers with the reason).
func (s *Server) parkStore() *park.Store {
	s.parkOnce.Do(func() {
		if s.cfg.StateDir == "" {
			s.parkErr = errors.New("this host has no state directory, so it cannot keep park records")
			return
		}
		s.parks, s.parkErr = park.Open(s.cfg.StateDir)
		if s.parks != nil {
			s.parks.Prune(parkHistory, time.Now())
		}
	})
	return s.parks
}

func (s *Server) parkUnavailable() *launchError {
	if s.parkStore() != nil {
		return nil
	}
	msg := "parking is not available on this host"
	if s.parkErr != nil {
		msg = s.parkErr.Error()
	}
	return &launchError{status: http.StatusServiceUnavailable, code: "park_unavailable", message: msg}
}

// parkBody is the body of POST /api/park.
type parkBody struct {
	// Agent is the pane id; Name, when set, must still be its name.
	Agent string `json:"agent"`
	Name  string `json:"name"`
	// Dirty confirms parking a worktree with uncommitted edits (they stay in place).
	Dirty bool `json:"dirty"`
	// Why: "manual" (default), "suggested" or "auto" (the hub's own park).
	Why string `json:"why"`
}

// parkResult is the answer of a park.
type parkResult struct {
	Record  park.Record `json:"record"`
	Closed  string      `json:"closed"`
	Scratch string      `json:"scratch,omitempty"` // what happened to the scratchpad
}

// handlePark is POST /api/park: close the agent, keep everything, write the record.
func (s *Server) handlePark(w http.ResponseWriter, r *http.Request) {
	var body parkBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), removeTimeout)
	defer cancel()
	res, lerr := s.parkAgent(ctx, body, s.by(r, ""))
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	writeJSON(w, res)
}

func (s *Server) parkAgent(ctx context.Context, b parkBody, by string) (*parkResult, *launchError) {
	if lerr := s.parkUnavailable(); lerr != nil {
		return nil, lerr
	}
	roots := s.checkRoots()
	if len(roots) == 0 {
		return nil, errLaunchDisabled
	}
	if b.Agent == "" {
		return nil, &launchError{status: http.StatusBadRequest, code: "bad_request", message: "which agent? (agent: its pane id)"}
	}
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
		return nil, &launchError{status: http.StatusNotFound, code: "no_such_agent", message: "no agent runs in that pane; nothing was closed"}
	}
	if b.Name != "" && agent.Name != b.Name {
		return nil, &launchError{status: http.StatusConflict, code: "agent_changed", message: "another agent runs in that pane now; nothing was closed"}
	}
	kind := agent.Agent
	if !park.Supported(kind) {
		return nil, &launchError{status: http.StatusConflict, code: "kind_not_parkable", message: "only claude, pi and opencode agents can be parked, not " + kind}
	}
	if agent.Status == herdr.StatusWorking || agent.Status == herdr.StatusBlocked {
		return nil, &launchError{status: http.StatusConflict, code: "agent_working", message: "the agent is " + string(agent.Status) + "; park it when it is idle"}
	}
	name := agent.Name
	if name == "" {
		return nil, &launchError{status: http.StatusConflict, code: "agent_unnamed", message: "the agent has no name to resume it under"}
	}
	// The manager's copy carries the background tasks (herdr's list does not).
	for _, la := range s.localAgents() {
		if la.PaneID == agent.PaneID && len(la.Background) > 0 {
			t := la.Background[0]
			return nil, &launchError{status: http.StatusConflict, code: "background_task", message: "a background task is running (" + strings.TrimSpace(t.Kind+" "+t.Label) + "); parking would kill it"}
		}
	}
	wt, lerr := resolveLaunchDir(agent.CWD, roots)
	if lerr != nil {
		return nil, lerr
	}
	checks := checkWorktree(ctx, wt, "", "")
	if !checks.Exists || checks.Error != "" {
		return nil, &launchError{status: http.StatusConflict, code: "no_worktree", message: "the agent's directory is not a git checkout; nothing was closed"}
	}
	if checks.Primary {
		return nil, &launchError{status: http.StatusConflict, code: "primary_checkout", message: "a repository's primary checkout is a home session; it is not parked"}
	}
	if checks.Dirty && !b.Dirty {
		return nil, &launchError{status: http.StatusConflict, code: "dirty_worktree",
			message: fmt.Sprintf("the worktree has %d uncommitted file(s); parking keeps them in place. Confirm to park anyway", checks.DirtyFiles)}
	}
	sk, sv, lerr := s.sessionRef(ctx, *agent, live)
	if lerr != nil {
		return nil, lerr
	}

	if b.Why == "auto" && kind == "claude" {
		// The hub parks by itself only when nothing is scheduled to wake the session.
		path := transcript.DefaultResolver.SessionFileByID(sv, agent.CWD)
		pend, err := pendingSchedule(path, time.Now())
		switch {
		case path == "" || err != nil:
			return nil, &launchError{status: http.StatusConflict, code: "auto_uncertain", message: "the session could not be read for a loop or wakeup; nothing was closed"}
		case pend:
			return nil, &launchError{status: http.StatusConflict, code: "auto_loop", message: "the session has a loop or wakeup scheduled; nothing was closed"}
		}
	}

	ps := s.parkStore()
	launch, _ := ps.LaunchOf(name)
	if launch.Kind != "" && launch.Kind != kind {
		launch = park.Launch{} // another kind lived under this name before
	}
	why := b.Why
	if why != "suggested" && why != "auto" {
		why = "manual"
	}
	rec := park.Record{
		ID: park.NewID(), Name: name, Kind: kind, Host: s.fleet.LocalName(),
		SessionKind: sk, Session: sv, CWD: wt, Worktree: wt, Branch: checks.Branch,
		LaunchArgs: launch.Args, PaneID: agent.PaneID, Why: why, By: by,
		LastStatus: string(agent.Status), Dirty: checks.Dirty, DirtyFiles: checks.DirtyFiles,
		ParkedAt: time.Now().UTC(),
	}
	if _, err := park.ResumeArgs(kind, sk, sv, rec.LaunchArgs); err != nil {
		return nil, &launchError{status: http.StatusConflict, code: "not_resumable", message: err.Error()}
	}
	rec.WorktreeBytes = park.TreeSize(wt, 3*time.Second)
	rec.SessionBytes = s.sessionBytes(rec, wt)
	// The record first: a park is never closed without one.
	if err := ps.Add(rec); err != nil {
		return nil, &launchError{status: http.StatusInternalServerError, code: "record_failed", message: "could not write the park record: " + err.Error()}
	}
	rr, lerr := s.removeSession(ctx, removeBody{Agent: agent.PaneID, Name: agent.Name, Worktree: wt, RemoveWorktree: false})
	if lerr != nil {
		_ = ps.Delete(rec.ID)
		return nil, lerr
	}
	out := &parkResult{Record: rec, Closed: rr.Closed}
	// The scratchpad goes to disk now that the process is gone.
	if kind == "claude" {
		from, to, n, note := s.moveScratchOut(wt, rec.ID)
		out.Scratch = note
		if from != "" {
			upd, err := ps.Update(rec.ID, func(r *park.Record) { r.ScratchFrom, r.ScratchTo, r.ScratchBytes = from, to, n })
			if err == nil {
				out.Record = upd
			}
		}
	}
	return out, nil
}

// recordForRemoval writes the park record of an agent that is about to be
// closed and have its worktree removed (an automatic removal): its session and
// launch arguments, and where its work landed, so Resume can bring it back.
func (s *Server) recordForRemoval(ctx context.Context, agent *herdr.AgentInfo, live []herdr.AgentInfo, c worktreeChecks, why string) (*park.Record, *launchError) {
	no := func(code, msg string) (*park.Record, *launchError) {
		return nil, &launchError{status: http.StatusConflict, code: code, message: msg + "; nothing was closed"}
	}
	if lerr := s.parkUnavailable(); lerr != nil {
		return nil, lerr
	}
	switch {
	case agent == nil:
		return no("no_record", "no agent runs there, so there is no session to keep a record of")
	case !park.Supported(agent.Agent):
		return no("no_record", "only claude, pi and opencode sessions can be resumed, not "+agent.Agent)
	case agent.Name == "":
		return no("no_record", "the agent has no name to resume it under")
	case !c.Exists || c.Path == "" || c.Primary:
		return no("no_record", "there is no worktree of its own")
	}
	sk, sv, lerr := s.sessionRef(ctx, *agent, live)
	if lerr != nil {
		return nil, lerr
	}
	ps := s.parkStore()
	launch, _ := ps.LaunchOf(agent.Name)
	if launch.Kind != "" && launch.Kind != agent.Agent {
		launch = park.Launch{}
	}
	rec := park.Record{
		ID: park.NewID(), Name: agent.Name, Kind: agent.Agent, Host: s.fleet.LocalName(),
		SessionKind: sk, Session: sv, CWD: c.Path, Worktree: c.Path, Branch: c.Branch, Repo: c.Repo,
		LaunchArgs: launch.Args, PaneID: agent.PaneID, Why: why, By: AutoCreator, LastStatus: string(agent.Status),
		ParkedAt: time.Now().UTC(),
	}
	if def := defaultBranch(ctx, c.Repo); def != "" {
		rec.BaseRef = "origin/" + def
		rec.Landed, _ = runGit(ctx, c.Repo, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/remotes/origin/"+def+"^{commit}")
	}
	if _, err := park.ResumeArgs(rec.Kind, sk, sv, rec.LaunchArgs); err != nil {
		return no("not_resumable", err.Error())
	}
	rec.SessionBytes = s.sessionBytes(rec, c.Path)
	if err := ps.Add(rec); err != nil {
		return nil, &launchError{status: http.StatusInternalServerError, code: "record_failed", message: "could not write the park record: " + err.Error() + "; nothing was closed"}
	}
	return &rec, nil
}

// recreateWorktree makes a removed worktree again for a resume: on its branch
// when that is still there, else on a new branch of that name at the commit
// the work landed in (origin's default branch then).
func (s *Server) recreateWorktree(ctx context.Context, rec park.Record, roots []string) (string, *launchError) {
	bad := func(code, msg string) (string, *launchError) {
		return "", &launchError{status: http.StatusConflict, code: code, message: msg}
	}
	repo, lerr := resolveLaunchDir(rec.Repo, roots)
	if lerr != nil || rec.Repo == "" || !primaryCheckout(ctx, repo) {
		return bad("repo_gone", "the repository "+homeLabel(rec.Repo)+" is gone; the worktree cannot be recreated")
	}
	path := filepath.Clean(rec.Worktree)
	if !withinAny(path, roots) || !validBranch(rec.Branch) {
		return bad("not_resumable", "the record's worktree or branch is not one comebye can recreate")
	}
	if _, err := os.Lstat(path); err == nil {
		return bad("path_exists", path+" exists again but is not a worktree of the record's repository")
	}
	fetchOrigin(ctx, repo)
	var out string
	var err error
	var note string
	if refExists(ctx, repo, "refs/heads/"+rec.Branch) {
		out, err = runGit(ctx, repo, "worktree", "add", "--", path, rec.Branch)
		note = "Your worktree was removed when your work had landed; it was recreated on your branch " + rec.Branch + "."
	} else {
		start := ""
		for _, c := range []string{rec.Landed, "refs/remotes/" + rec.BaseRef} {
			if c != "" && refExists(ctx, repo, c) {
				start = c
				break
			}
		}
		if start == "" {
			return bad("not_resumable", "the branch "+rec.Branch+" and the commit its work landed in are both gone")
		}
		out, err = runGit(ctx, repo, "worktree", "add", "-b", rec.Branch, "--", path, start)
		note = "Your worktree and branch were removed when your work had landed; they were recreated as branch " + rec.Branch + " at " + shortSHA(start) + " (" + orDefault(rec.BaseRef, "origin's default branch") + ", where your work is)."
	}
	if err != nil {
		return bad("worktree_failed", "could not recreate the worktree: "+clip(out+" "+err.Error(), 300))
	}
	return note, nil
}

// sessionRef captures what the agent kind needs to resume: a claude session id,
// a pi session file, an opencode session id.
func (s *Server) sessionRef(ctx context.Context, a herdr.AgentInfo, live []herdr.AgentInfo) (kind, value string, lerr *launchError) {
	fail := func(msg string) (string, string, *launchError) {
		return "", "", &launchError{status: http.StatusConflict, code: "no_session", message: msg + "; nothing was closed"}
	}
	switch a.Agent {
	case "claude":
		if sess := a.AgentSession; sess != nil && sess.Kind == "id" && sess.Value != "" {
			if p := transcript.DefaultResolver.SessionFileByID(sess.Value, a.CWD); p != "" {
				return "id", sess.Value, nil
			}
		}
		p, err := transcript.SessionPath(a, live)
		if err != nil {
			return fail("claude's session could not be identified (" + err.Error() + ")")
		}
		id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if id == "" || id == filepath.Base(p) {
			return fail("claude's session file has an unexpected name")
		}
		return "id", id, nil
	case "pi":
		p, err := transcript.SessionPath(a, live)
		if err != nil {
			return fail("pi's session file is not known (" + err.Error() + ")")
		}
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
			return fail("pi's session file " + p + " does not exist")
		}
		return "path", p, nil
	case "opencode":
		id, err := s.opencodeSession(ctx, a, live)
		if err != nil {
			return fail(err.Error())
		}
		return "id", id, nil
	}
	return fail("unsupported agent kind")
}

// ocSession is one row of `opencode session list --format json`.
type ocSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Updated   int64  `json:"updated"`
	Directory string `json:"directory"`
}

// opencodeSessions lists opencode's sessions (newest first).
func (s *Server) opencodeSessions(ctx context.Context, dir string) ([]ocSession, error) {
	bin := lookExec("opencode")
	if bin == "" {
		return nil, errors.New("opencode is not installed on this host")
	}
	ctx, cancel := context.WithTimeout(ctx, opencodeListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "session", "list", "--format", "json", "-n", "300")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode session list failed: %v", err)
	}
	var rows []ocSession
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("opencode session list gave unreadable output: %v", err)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Updated > rows[j].Updated })
	return rows, nil
}

// opencodeSession finds the session of the opencode agent a: the most recent
// one for its directory, refused when another opencode agent shares the
// directory (it could be theirs).
func (s *Server) opencodeSession(ctx context.Context, a herdr.AgentInfo, live []herdr.AgentInfo) (string, error) {
	if sess := a.AgentSession; sess != nil && sess.Kind == "id" && sess.Value != "" {
		return sess.Value, nil
	}
	for _, o := range live {
		if o.PaneID != a.PaneID && o.Agent == "opencode" && sameDir(o.CWD, a.CWD) {
			return "", errors.New("another opencode agent works in the same directory, so its session cannot be told apart")
		}
	}
	rows, err := s.opencodeSessions(ctx, a.CWD)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if sameDir(r.Directory, a.CWD) {
			return r.ID, nil
		}
	}
	return "", errors.New("opencode has no session for " + a.CWD)
}

// moveScratchOut moves the agent's /tmp scratchpad project to disk (the state
// directory), freeing RAM; resume moves it back.
func (s *Server) moveScratchOut(wt, id string) (from, to string, n int64, note string) {
	if s.cfg.ScratchDir == "" || s.cfg.StateDir == "" {
		return "", "", 0, "no scratch directory is configured: nothing moved"
	}
	opts := s.scratchOpts(false)
	var src string
	for _, p := range []string{wt} {
		cand := filepath.Join(opts.ClaudeRoot(), scratch.ProjectDir(p))
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			src = cand
		}
	}
	if src == "" {
		return "", "", 0, "no scratchpad to move"
	}
	dst := filepath.Join(s.cfg.StateDir, "parked-scratch", id)
	n, err := park.MoveTree(src, dst)
	if err != nil {
		log.Printf("park: scratchpad %s: %v", src, err)
		return "", "", 0, "the scratchpad stays in /tmp: " + err.Error()
	}
	return src, dst, n, "moved " + scratch.HumanBytes(n) + " to disk"
}

// moveScratchBack returns the scratchpad to where Claude looks for it.
func moveScratchBack(r park.Record) string {
	if r.ScratchTo == "" || r.ScratchFrom == "" {
		return ""
	}
	if _, err := os.Stat(r.ScratchTo); err != nil {
		return "the scratchpad on disk is gone"
	}
	if _, err := os.Stat(r.ScratchFrom); err == nil {
		return "the scratchpad folder exists again in /tmp; the parked copy stays at " + r.ScratchTo
	}
	if _, err := park.MoveTree(r.ScratchTo, r.ScratchFrom); err != nil {
		log.Printf("park: scratchpad back %s: %v", r.ScratchTo, err)
		return "the scratchpad could not be moved back: " + err.Error()
	}
	return "scratchpad moved back"
}

// resumeBody is the body of POST /api/resume.
type resumeBody struct {
	ID string `json:"id"`
	// NoNote skips the catch-up note.
	NoNote bool `json:"no_note"`
}

// resumeResult is the answer of a resume.
type resumeResult struct {
	Agent   browserAgent `json:"agent"`
	Record  park.Record  `json:"record"`
	Note    string       `json:"note,omitempty"`
	Scratch string       `json:"scratch,omitempty"`
	Warn    string       `json:"warning,omitempty"`
	// Recreated says how a removed worktree was made again.
	Recreated string `json:"recreated,omitempty"`
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	var body resumeBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), launchTimeout)
	defer cancel()
	res, lerr := s.resumeAgent(ctx, body)
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	writeJSON(w, res)
}

func (s *Server) resumeAgent(ctx context.Context, b resumeBody) (*resumeResult, *launchError) {
	if lerr := s.parkUnavailable(); lerr != nil {
		return nil, lerr
	}
	ps := s.parkStore()
	rec, ok := ps.Get(b.ID)
	if !ok {
		return nil, &launchError{status: http.StatusNotFound, code: "no_such_record", message: "no parked agent has that id"}
	}
	if !rec.Parked() {
		return nil, &launchError{status: http.StatusConflict, code: "not_parked", message: rec.Name + " was already resumed"}
	}
	roots := s.checkRoots()
	if len(roots) == 0 {
		return nil, errLaunchDisabled
	}
	dir, lerr := resolveLaunchDir(rec.Worktree, roots)
	recreated := ""
	if lerr != nil && lerr.code == "not_found" && rec.WorktreeGone {
		var rerr *launchError
		if recreated, rerr = s.recreateWorktree(ctx, rec, roots); rerr != nil {
			return nil, rerr
		}
		dir, lerr = resolveLaunchDir(rec.Worktree, roots)
	}
	if lerr != nil {
		if lerr.code == "not_found" {
			lerr.message = "the worktree " + homeLabel(rec.Worktree) + " is gone; it cannot be resumed"
		}
		return nil, lerr
	}
	if lerr := s.sessionStillThere(ctx, rec, dir); lerr != nil {
		return nil, lerr
	}
	args, err := park.ResumeArgs(rec.Kind, rec.SessionKind, rec.Session, rec.LaunchArgs)
	if err != nil {
		return nil, &launchError{status: http.StatusConflict, code: "not_resumable", message: err.Error()}
	}
	live, err := s.client.ListAgents(ctx)
	if err != nil {
		return nil, herdrFailure("list agents", err)
	}
	for _, a := range live {
		if a.Name == rec.Name {
			return nil, &launchError{status: http.StatusConflict, code: "agent_name_taken", message: "an agent named " + rec.Name + " is running now; nothing was resumed"}
		}
		if a.Agent == rec.Kind && sameDir(a.CWD, dir) {
			return nil, &launchError{status: http.StatusConflict, code: "worktree_in_use", message: "another agent (" + a.DisplayName() + ") works in that worktree"}
		}
	}
	// The scratchpad before the agent, so it finds it where it left it.
	scratchNote := moveScratchBack(rec)

	ws, err := s.client.CreateWorkspace(ctx, dir, rec.Name, false)
	if err != nil {
		return nil, herdrFailure("open a workspace", err)
	}
	pane := ws.RootPaneID
	if pane == "" {
		if panes, err := s.client.ListPanes(ctx, ws.WorkspaceID); err == nil && len(panes) > 0 {
			pane = panes[0].PaneID
		}
		if pane == "" {
			return nil, &launchError{status: http.StatusBadGateway, code: "launch_failed", message: "could not find the new workspace's pane", workspace: ws.WorkspaceID}
		}
	}
	agent, err := s.startAgent(ctx, rec.Name, rec.Kind, pane, args...)
	if err != nil {
		return nil, &launchError{status: http.StatusBadGateway, code: "launch_failed",
			message: "could not start " + rec.Kind + " on the session: " + errMessage(err) + ". The workspace stays open", workspace: ws.WorkspaceID}
	}
	// startAgent noted the resume flags as the launch; the next park must keep
	// the original launch arguments, not stack a second --resume on them.
	ps.NoteLaunch(rec.Name, rec.Kind, rec.LaunchArgs, time.Now())
	if s.manager != nil {
		s.manager.Refresh()
	}
	if agent.PaneID == "" {
		agent.PaneID = pane
	}
	now := time.Now().UTC()
	upd, uerr := ps.Update(rec.ID, func(r *park.Record) { r.ResumedAt, r.ResumedPane = &now, agent.PaneID })
	if uerr != nil {
		log.Printf("park: record %s after resume: %v", rec.ID, uerr)
		upd = rec
	}
	host := s.fleet.LocalName()
	res := &resumeResult{
		Agent:  browserAgent{ID: state.QualifiedID(host, agent.PaneID), Host: host, PaneID: agent.PaneID, Name: rec.Name, Agent: rec.Kind, Status: agent.Status, CWD: dir},
		Record: upd, Scratch: scratchNote, Recreated: recreated,
	}
	if !b.NoNote {
		res.Note = s.catchUpNote(ctx, rec, dir, now)
		if recreated != "" {
			res.Note = recreated + " " + res.Note
		}
		if queued, err := s.deliverOrQueue(ctx, res.Agent.ID, rec.Name, res.Note); err != nil {
			res.Warn = "the agent started, but the catch-up note was not delivered: " + err.Error()
		} else if queued {
			res.Warn = "the agent started at a dialog: the catch-up note is queued and goes in once it is answered"
		}
	}
	return res, nil
}

// sessionStillThere checks the session the record names exists before a pane
// is opened for it.
func (s *Server) sessionStillThere(ctx context.Context, rec park.Record, dir string) *launchError {
	gone := func(what string) *launchError {
		return &launchError{status: http.StatusConflict, code: "session_gone", message: what + "; it cannot be resumed"}
	}
	switch rec.Kind {
	case "pi":
		if fi, err := os.Stat(rec.Session); err != nil || !fi.Mode().IsRegular() {
			return gone("pi's session file " + rec.Session + " is gone")
		}
	case "claude":
		if transcript.DefaultResolver.SessionFileByID(rec.Session, dir) == "" {
			return gone("claude's session " + rec.Session + " is gone")
		}
	case "opencode":
		rows, err := s.opencodeSessions(ctx, dir)
		if err != nil {
			return &launchError{status: http.StatusBadGateway, code: "opencode_failed", message: err.Error()}
		}
		for _, r := range rows {
			if r.ID == rec.Session {
				return nil
			}
		}
		return gone("opencode no longer lists session " + rec.Session)
	}
	return nil
}

// catchUpNote is what the resumed agent is told first.
func (s *Server) catchUpNote(ctx context.Context, rec park.Record, dir string, now time.Time) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Comebye: you were parked (your pane was closed) %s and resumed just now, in the same worktree and on the same branch. ", parkAgo(now.Sub(rec.ParkedAt)))
	sb.WriteString("A new process runs your session, so nothing you had running survived: check any background task, loop, wakeup or dev server you started. Run `git status` first.")
	if rec.Dirty {
		fmt.Fprintf(&sb, " You were parked with %d uncommitted file(s); they were left as they were.", rec.DirtyFiles)
	}
	if base := defaultBranch(ctx, dir); base != "" {
		if out, err := runGit(ctx, dir, "rev-list", "--count", "--since="+rec.ParkedAt.Format(time.RFC3339), "origin/"+base); err == nil && out != "" && out != "0" {
			fmt.Fprintf(&sb, " Since then %s commit(s) landed on %s.", strings.TrimSpace(out), base)
		}
	}
	return sb.String()
}

func parkAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// handleParked is GET /api/parked: this host's records (?all=1 includes the
// resumed ones, kept as history).
func (s *Server) handleParked(w http.ResponseWriter, r *http.Request) {
	if lerr := s.parkUnavailable(); lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	recs := s.parkStore().List(r.URL.Query().Get("all") != "1")
	if recs == nil {
		recs = []park.Record{}
	}
	writeJSON(w, map[string]any{"host": s.fleet.LocalName(), "parked": recs})
}

// parkPinBody is the body of POST /api/park/pin.
type parkPinBody struct {
	ID     string `json:"id"`
	Pinned bool   `json:"pinned"`
}

// handleParkPin is POST /api/park/pin: keep (or stop keeping) a parked agent
// out of park suggestions and retention clean-up.
func (s *Server) handleParkPin(w http.ResponseWriter, r *http.Request) {
	var b parkPinBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&b); err != nil || b.ID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if lerr := s.parkUnavailable(); lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	rec, err := s.parkStore().Update(b.ID, func(r *park.Record) { r.Pinned = b.Pinned })
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "no_such_record", "message": "no parked agent has that id"})
		return
	}
	writeJSON(w, map[string]any{"record": rec})
}

func (s *Server) parkMirrorPath() string {
	if s.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.StateDir, "parked-mirror.json")
}

func (s *Server) loadParkMirror() parkMirror {
	m := parkMirror{Hosts: map[string]parkMirrorHost{}}
	if p := s.parkMirrorPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &m)
		}
	}
	if m.Hosts == nil {
		m.Hosts = map[string]parkMirrorHost{}
	}
	return m
}

func (s *Server) saveParkMirror(m parkMirror) {
	p := s.parkMirrorPath()
	if p == "" {
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// sessionBytes is the size of the session file a record keeps (0 when unknown).
func (s *Server) sessionBytes(r park.Record, wt string) int64 {
	p := ""
	switch r.Kind {
	case "pi":
		p = r.Session
	case "claude":
		p = transcript.DefaultResolver.SessionFileByID(r.Session, wt)
	}
	if fi, err := os.Stat(p); p != "" && err == nil {
		return fi.Size()
	}
	return 0
}

// dropParkedScratch deletes the scratchpad a removed record kept on disk (only
// ever inside this host's parked-scratch directory).
func (s *Server) dropParkedScratch(r park.Record) {
	if r.ScratchTo == "" || s.cfg.StateDir == "" {
		return
	}
	root := filepath.Join(s.cfg.StateDir, "parked-scratch")
	if !within(filepath.Clean(r.ScratchTo), root) || filepath.Clean(r.ScratchTo) == root {
		return
	}
	if err := os.RemoveAll(r.ScratchTo); err != nil {
		log.Printf("park: removing %s: %v", r.ScratchTo, err)
	}
}

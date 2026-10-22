package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
)

// Workers: an agent in its own git worktree, on its own branch. These are
// the host-level operations behind the hub's handoffs and removals (see
// ledger.go); every instance serves them for its own host, and a hub calls
// a peer's through fleet.CallHost.
//
// A worker's worktree lives at <worktree root>/<repo>/<branch> (slashes in
// the branch become '-'; the root is ~/.worktrees by default) and opens as
// a herdr workspace nested under the repository's primary workspace, the
// way herdr's own worktree flow does. The repository must be a primary
// checkout inside the launch roots; checks and removals also accept paths
// in the worktree root.

const (
	// workerTimeout bounds a worker start: git, worktree.create (which may
	// check out a large tree) and the agent's start.
	workerTimeout = 150 * time.Second
	// removeTimeout bounds a removal.
	removeTimeout = 90 * time.Second
	// maxRepos caps the repository list.
	maxRepos = 200
)

// worktreeRoot is where workers' worktrees go, symlinks resolved when it
// exists.
func (s *Server) worktreeRoot() string {
	r := s.cfg.WorktreeRoot
	if r == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		r = filepath.Join(home, ".worktrees")
	}
	if p, err := filepath.EvalSymlinks(r); err == nil {
		return p
	}
	return filepath.Clean(r)
}

// checkRoots are the roots checks and removals may touch: the launch roots
// and the worktree root (when it exists and launching is on).
func (s *Server) checkRoots() []string {
	roots := s.launchRoots()
	if len(roots) == 0 {
		return nil
	}
	if wt := s.worktreeRoot(); filepath.IsAbs(wt) {
		if fi, err := os.Stat(wt); err == nil && fi.IsDir() {
			roots = append(roots, wt)
		}
	}
	return roots
}

// pickKind validates an agent kind for this host (default: the one most
// running agents use).
func (s *Server) pickKind(kind string, running []herdr.AgentInfo) (string, *launchError) {
	kinds := s.launchKinds(running)
	if kind == "" {
		kind = defaultKind(kinds, running)
	}
	for _, k := range kinds {
		if k == kind {
			return kind, nil
		}
	}
	return "", &launchError{status: http.StatusBadRequest, code: "bad_kind", message: "agent kind " + kind + " is not available on this host"}
}

var errLaunchDisabled = &launchError{status: http.StatusForbidden, code: "launch_disabled", message: "launching is not available on this host"}

// herdrFailure turns a herdr (or socket) error into a launchError.
func herdrFailure(step string, err error) *launchError {
	e := &launchError{status: http.StatusBadGateway, code: "herdr_error", message: "could not " + step + ": " + errMessage(err)}
	var he *herdr.Error
	if errors.As(err, &he) {
		e.code = he.Code
	}
	return e
}

// workerBody is the JSON body of a worker start.
type workerBody struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	// Prompt is sent as is, except that {{worktree}} and {{base}} are
	// replaced with the worktree's path and the base branch.
	Prompt string `json:"prompt"`
}

// workerResult is a started worker.
type workerResult struct {
	Agent       browserAgent `json:"agent"`
	WorkspaceID string       `json:"workspace_id"`
	Worktree    string       `json:"worktree"`
	Branch      string       `json:"branch"`
	Base        string       `json:"base"`
	Repo        string       `json:"repo"`
	// Reused: the branch's worktree existed and was opened, not created.
	Reused      bool   `json:"reused"`
	PromptError string `json:"prompt_error,omitempty"`
}

// handleWorker starts a worker: POST {repo, branch, base?, kind?, name?,
// prompt}.
func (s *Server) handleWorker(w http.ResponseWriter, r *http.Request) {
	var body workerBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLaunchPrompt+64<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), workerTimeout)
	defer cancel()
	res, lerr := s.startWorker(ctx, body)
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	writeJSON(w, res)
}

// startWorker validates a worker start, creates (or reopens) the branch's
// worktree as a workspace, starts the agent in a free pane of it and sends
// the prompt.
func (s *Server) startWorker(ctx context.Context, b workerBody) (*workerResult, *launchError) {
	roots := s.launchRoots()
	if len(roots) == 0 {
		return nil, errLaunchDisabled
	}
	repo, lerr := resolveLaunchDir(b.Repo, roots)
	if lerr != nil {
		return nil, lerr
	}
	if !primaryCheckout(ctx, repo) {
		return nil, &launchError{status: http.StatusBadRequest, code: "not_a_repo", message: "that directory is not the top of a git repository's primary checkout"}
	}
	branch := strings.TrimSpace(b.Branch)
	if !validBranch(branch) || !gitRefFormatOK(ctx, branch) {
		return nil, &launchError{status: http.StatusBadRequest, code: "bad_branch", message: "branch names are letters, digits, '.', '_', '-' and '/' (no '..', no leading '-' or '.', at most 100)"}
	}
	running := s.localAgents()
	kind, lerr := s.pickKind(b.Kind, running)
	if lerr != nil {
		return nil, lerr
	}
	name := strings.TrimSpace(b.Name)
	named := name != ""
	if named && !agentNameRe.MatchString(name) {
		return nil, &launchError{status: http.StatusBadRequest, code: "bad_name", message: "names are lowercase letters, digits, - and _, starting with a letter (at most 32)"}
	}
	if len(b.Prompt) > maxLaunchPrompt {
		return nil, &launchError{status: http.StatusRequestEntityTooLarge, code: "prompt_too_large", message: "the prompt is too long"}
	}
	base := strings.TrimSpace(b.Base)
	if base != "" {
		if !validBranch(base) || !refExists(ctx, repo, base) {
			return nil, &launchError{status: http.StatusBadRequest, code: "bad_base", message: "no such base branch: " + base}
		}
	} else if base = defaultBranch(ctx, repo); base == "" {
		return nil, &launchError{status: http.StatusBadRequest, code: "bad_base", message: "the repository has no default branch; give a base"}
	}
	wtRoot := s.worktreeRoot()
	if !filepath.IsAbs(wtRoot) {
		return nil, &launchError{status: http.StatusInternalServerError, code: "no_worktree_root", message: "no worktree root (home directory unknown)"}
	}
	wtPath := filepath.Join(wtRoot, filepath.Base(repo), branchDirName(branch))
	label := filepath.Base(repo) + "/" + branchDirName(branch)

	live, err := s.client.ListAgents(ctx)
	if err != nil {
		return nil, herdrFailure("list agents", err)
	}
	taken := map[string]bool{}
	for _, a := range live {
		if a.Name != "" {
			taken[a.Name] = true
		}
	}
	if !named {
		name = defaultAgentName("/"+branchDirName(branch), func(n string) bool { return taken[n] })
	} else if taken[name] {
		return nil, &launchError{status: http.StatusConflict, code: "agent_name_taken", message: "an agent named " + name + " is already running"}
	}

	wts, err := s.client.ListWorktrees(ctx, repo)
	if err != nil {
		return nil, herdrFailure("list the repository's worktrees", err)
	}
	var existing *herdr.Worktree
	for i, wt := range wts {
		if wt.Branch == branch || wt.Branch == "refs/heads/"+branch || filepath.Clean(wt.Path) == wtPath {
			existing = &wts[i]
			break
		}
	}
	var opened herdr.OpenedWorktree
	reused := existing != nil
	if reused {
		path := filepath.Clean(existing.Path)
		switch {
		case !existing.IsLinked:
			return nil, &launchError{status: http.StatusConflict, code: "branch_in_primary", message: "branch " + branch + " is checked out in the primary checkout; a worker needs its own branch"}
		case strings.TrimPrefix(existing.Branch, "refs/heads/") != branch:
			return nil, &launchError{status: http.StatusConflict, code: "path_taken", message: path + " is a worktree for another branch"}
		}
		if real, err := filepath.EvalSymlinks(path); err != nil || !withinAny(real, s.checkRoots()) {
			return nil, &launchError{status: http.StatusForbidden, code: "outside_roots", message: "the branch's worktree is outside the launch and worktree roots"}
		}
		opened, err = s.client.OpenWorktree(ctx, herdr.WorktreeRequest{CWD: repo, Path: path, Label: label})
		if err != nil {
			return nil, herdrFailure("open the branch's worktree", err)
		}
		if opened.Path == "" {
			opened.Path = path
		}
	} else {
		if _, err := os.Lstat(wtPath); err == nil {
			return nil, &launchError{status: http.StatusConflict, code: "path_exists", message: wtPath + " exists but is not a worktree of this repository"}
		}
		req := herdr.WorktreeRequest{CWD: repo, Branch: branch, Path: wtPath, Label: label}
		if !branchExists(ctx, repo, branch) {
			req.Base = base
		}
		opened, err = s.client.CreateWorktree(ctx, req)
		if err != nil {
			return nil, herdrFailure("create the worktree", err)
		}
		if opened.Path == "" {
			opened.Path = wtPath
		}
	}

	pane, lerr := s.freePane(ctx, opened, reused)
	if lerr != nil {
		return nil, lerr
	}
	agent, err := s.startAgent(ctx, name, kind, pane)
	if err != nil {
		e := herdrFailure("start "+kind, err)
		e.message += fmt.Sprintf(". The worktree's workspace %q is left open in herdr.", opened.Label)
		if e.code == "agent_name_taken" {
			e.status = http.StatusConflict
		}
		e.workspace = opened.WorkspaceID
		return nil, e
	}
	if s.manager != nil {
		s.manager.Refresh()
	}
	if agent.PaneID == "" {
		agent.PaneID = pane
	}
	host := s.fleet.LocalName()
	res := &workerResult{
		Agent: browserAgent{
			ID: state.QualifiedID(host, agent.PaneID), Host: host, PaneID: agent.PaneID,
			Name: name, Agent: kind, Status: agent.Status, CWD: opened.Path,
		},
		WorkspaceID: opened.WorkspaceID, Worktree: opened.Path, Branch: branch, Base: base, Repo: repo, Reused: reused,
	}
	prompt := strings.NewReplacer("{{worktree}}", opened.Path, "{{base}}", base).Replace(b.Prompt)
	if strings.TrimSpace(prompt) != "" {
		if _, err := s.client.Prompt(ctx, name, prompt, herdr.PromptOptions{}); err != nil {
			res.PromptError = "the worker started, but its brief was not sent: " + errMessage(err)
		}
	}
	return res, nil
}

// freePane picks the pane to start a worker in: a new worktree's root
// pane, else a pane of the workspace that hosts no live agent (starting
// over a running agent would clobber it).
func (s *Server) freePane(ctx context.Context, opened herdr.OpenedWorktree, reused bool) (string, *launchError) {
	if !reused && opened.RootPaneID != "" {
		return opened.RootPaneID, nil
	}
	panes, err := s.client.ListPanes(ctx, opened.WorkspaceID)
	if err != nil {
		return "", herdrFailure("list the workspace's panes", err)
	}
	live, err := s.client.ListAgents(ctx)
	if err != nil {
		return "", herdrFailure("list agents", err)
	}
	busy := map[string]string{}
	for _, a := range live {
		busy[a.PaneID] = a.DisplayName()
	}
	var free []string
	for _, p := range panes {
		if p.PaneID == "" || p.Agent != "" {
			continue
		}
		if _, ok := busy[p.PaneID]; ok {
			continue
		}
		free = append(free, p.PaneID)
	}
	for _, p := range free {
		if p == opened.RootPaneID {
			return p, nil
		}
	}
	if len(free) > 0 {
		return free[0], nil
	}
	var names []string
	for _, p := range panes {
		if n, ok := busy[p.PaneID]; ok {
			names = append(names, n)
		}
	}
	return "", &launchError{status: http.StatusConflict, code: "no_free_pane", workspace: opened.WorkspaceID,
		message: "every pane of the worktree's workspace already runs an agent (" + strings.Join(names, ", ") + "); prompt it instead"}
}

// handleChecks answers the safety checks for a worktree: GET ?path=
// (inside the launch or worktree roots) &branch= &base=.
func (s *Server) handleChecks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	roots := s.checkRoots()
	if len(roots) == 0 {
		writeLaunchError(w, errLaunchDisabled)
		return
	}
	branch, base := q.Get("branch"), q.Get("base")
	if (branch != "" && !validBranch(branch)) || (base != "" && !validBranch(base)) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_branch", "message": "invalid branch"})
		return
	}
	path, lerr := resolveLaunchDir(q.Get("path"), roots)
	if lerr != nil {
		if lerr.code == "not_found" {
			writeJSON(w, worktreeChecks{Path: filepath.Clean(q.Get("path")), Unmerged: -1})
			return
		}
		writeLaunchError(w, lerr)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	writeJSON(w, checkWorktree(ctx, path, branch, base))
}

// removeBody is the JSON body of a removal.
type removeBody struct {
	// Agent is the agent's pane id; Name, when set, must still be its name
	// (a pane reused by another agent is not closed).
	Agent       string `json:"agent"`
	Name        string `json:"name"`
	WorkspaceID string `json:"workspace_id"`
	// Worktree defaults to the workspace's linked checkout.
	Worktree       string `json:"worktree"`
	Branch         string `json:"branch"`
	Base           string `json:"base"`
	RemoveWorktree bool   `json:"remove_worktree"`
	// AllowWorking closes an agent that is working or blocked; without it
	// such an agent is refused.
	AllowWorking bool `json:"allow_working"`
}

// removeResult says what a removal did.
type removeResult struct {
	// Closed is "workspace", "pane", or "" when nothing was open.
	Closed          string          `json:"closed"`
	PaneID          string          `json:"pane_id,omitempty"`
	WorkspaceID     string          `json:"workspace_id,omitempty"`
	Worktree        string          `json:"worktree,omitempty"`
	WorktreeRemoved bool            `json:"worktree_removed"`
	Kept            string          `json:"kept,omitempty"` // why the worktree stayed
	Checks          *worktreeChecks `json:"checks,omitempty"`
}

// handleRemove closes a session and, if asked and safe, removes its
// worktree: POST {agent, name?, workspace_id?, worktree?, branch?, base?,
// remove_worktree, allow_working?}.
func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	var body removeBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), removeTimeout)
	defer cancel()
	res, lerr := s.removeSession(ctx, body)
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	writeJSON(w, res)
}

// removeSession closes the agent's workspace when nothing else lives in it
// (never a repository's primary workspace), else just its pane; then, when
// asked, removes the worktree if it is clean, merged and unused. Branches
// are never deleted.
func (s *Server) removeSession(ctx context.Context, b removeBody) (*removeResult, *launchError) {
	roots := s.checkRoots()
	if len(roots) == 0 {
		return nil, errLaunchDisabled
	}
	if (b.Branch != "" && !validBranch(b.Branch)) || (b.Base != "" && !validBranch(b.Base)) {
		return nil, &launchError{status: http.StatusBadRequest, code: "bad_branch", message: "invalid branch"}
	}
	if b.Agent == "" && b.WorkspaceID == "" && b.Worktree == "" {
		return nil, &launchError{status: http.StatusBadRequest, code: "bad_request", message: "nothing to remove"}
	}
	live, err := s.client.ListAgents(ctx)
	if err != nil {
		return nil, herdrFailure("list agents", err)
	}
	var agent *herdr.AgentInfo
	for i := range live {
		if b.Agent != "" && live[i].PaneID == b.Agent {
			agent = &live[i]
		}
	}
	wsID := b.WorkspaceID
	if agent != nil {
		if (b.Name != "" && agent.Name != b.Name) || (wsID != "" && agent.WorkspaceID != wsID) {
			return nil, &launchError{status: http.StatusConflict, code: "agent_changed", message: "another agent runs in that pane now; nothing was closed"}
		}
		if !b.AllowWorking && (agent.Status == herdr.StatusWorking || agent.Status == herdr.StatusBlocked) {
			return nil, &launchError{status: http.StatusConflict, code: "agent_working", message: "the agent is " + string(agent.Status) + "; nothing was closed"}
		}
		wsID = agent.WorkspaceID
	}
	workspaces, err := s.client.ListWorkspaces(ctx)
	if err != nil {
		return nil, herdrFailure("list workspaces", err)
	}
	var ws *herdr.Workspace
	for i := range workspaces {
		if wsID != "" && workspaces[i].WorkspaceID == wsID {
			ws = &workspaces[i]
		}
	}
	linkedWS := ws != nil && ws.Worktree != nil && ws.Worktree.IsLinked
	primaryWS := ws != nil && ws.Worktree != nil && !ws.Worktree.IsLinked

	res := &removeResult{WorkspaceID: wsID}
	wt := b.Worktree
	if wt == "" && linkedWS {
		wt = ws.Worktree.CheckoutPath
	}
	var checks worktreeChecks
	if wt != "" {
		p, lerr := resolveLaunchDir(wt, roots)
		switch {
		case lerr != nil && lerr.code == "not_found":
			checks = worktreeChecks{Path: filepath.Clean(wt), Unmerged: -1}
		case lerr != nil:
			return nil, lerr
		default:
			checks = checkWorktree(ctx, p, b.Branch, b.Base)
		}
		wt = checks.Path
		res.Worktree, res.Checks = wt, &checks
	}

	// Close: the whole workspace only when no other agent lives in it and
	// it is not a repository's home; a pane otherwise.
	others := 0
	for _, a := range live {
		if a.WorkspaceID == wsID && (agent == nil || a.PaneID != agent.PaneID) {
			others++
		}
	}
	wholeWS := ws != nil && others == 0 && !primaryWS
	switch {
	case agent != nil && wholeWS:
		if err := s.client.CloseWorkspace(ctx, wsID); err != nil {
			return nil, herdrFailure("close the workspace", err)
		}
		res.Closed, res.PaneID = "workspace", agent.PaneID
	case agent != nil:
		if err := s.client.ClosePane(ctx, agent.PaneID); err != nil {
			return nil, herdrFailure("close the pane", err)
		}
		res.Closed, res.PaneID = "pane", agent.PaneID
	case wholeWS && linkedWS && sameDir(ws.Worktree.CheckoutPath, wt):
		// The agent is gone but its worktree's workspace (a shell) is
		// still open.
		if err := s.client.CloseWorkspace(ctx, wsID); err != nil {
			return nil, herdrFailure("close the workspace", err)
		}
		res.Closed = "workspace"
	}
	if s.manager != nil && res.Closed != "" {
		s.manager.Refresh()
	}

	switch {
	case wt == "":
	case !b.RemoveWorktree:
		res.Kept = "removing the worktree was not asked for"
	default:
		ok, why := checks.Removable()
		if ok {
			closedWS := ""
			if res.Closed == "workspace" {
				closedWS = wsID
			}
			why = s.worktreeInUse(ctx, checks, live, res.PaneID, closedWS)
			ok = why == ""
		}
		if ok {
			if err := removeWorktree(ctx, checks.Repo, wt); err != nil {
				why = err.Error()
			} else {
				res.WorktreeRemoved = true
			}
		}
		if !res.WorktreeRemoved {
			res.Kept = why
		}
	}
	return res, nil
}

// worktreeInUse explains why a clean, merged worktree must still stay:
// another live agent works in it, or a workspace still has it open.
// closedPane and closedWS are what the removal just closed.
func (s *Server) worktreeInUse(ctx context.Context, c worktreeChecks, live []herdr.AgentInfo, closedPane, closedWS string) string {
	for _, a := range live {
		if a.PaneID == closedPane || (closedWS != "" && a.WorkspaceID == closedWS) {
			continue
		}
		if cwd, err := filepath.EvalSymlinks(a.CWD); err == nil && within(cwd, c.Path) {
			return "another agent (" + a.DisplayName() + ") works in it"
		}
	}
	wts, err := s.client.ListWorktrees(ctx, c.Repo)
	if err != nil {
		return "could not list the repository's worktrees: " + errMessage(err)
	}
	for _, wt := range wts {
		if sameDir(wt.Path, c.Path) && wt.OpenWorkspaceID != "" && wt.OpenWorkspaceID != closedWS {
			return "a workspace (" + wt.OpenWorkspaceID + ") still has it open"
		}
	}
	return ""
}

// sameDir compares two paths after resolving symlinks.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// repoEntry is one git repository offered in the handoff form.
type repoEntry struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Label         string `json:"label"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// handleRepos lists the primary git checkouts inside the launch roots:
// a root itself, its children and grandchildren (hidden directories and
// symlinks skipped).
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	roots := s.launchRoots()
	if len(roots) == 0 {
		writeLaunchError(w, errLaunchDisabled)
		return
	}
	out := []repoEntry{}
	seen := map[string]bool{}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if len(out) >= maxRepos || seen[dir] {
			return
		}
		seen[dir] = true
		if fi, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
			out = append(out, repoEntry{Name: filepath.Base(dir), Path: dir, Label: homeLabel(dir), DefaultBranch: defaultBranchFast(dir)})
			return // nested repositories are not offered
		}
		if depth == 2 {
			return
		}
		list, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range list {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				walk(filepath.Join(dir, e.Name()), depth+1)
			}
		}
	}
	for _, root := range roots {
		walk(root, 0)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].Path < out[j].Path
	})
	writeJSON(w, map[string]any{"repos": out, "truncated": len(out) >= maxRepos})
}

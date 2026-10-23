package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/playbook"
	"github.com/jhjaggars/comebye/internal/state"
)

// Launching: the app can start a new agent in a directory on a host. The
// directory must lie inside one of the host's launch roots (-launch-root,
// default ~/code), checked after resolving symlinks, so a phone can browse
// and start agents in your checkouts but nowhere else on the machine.
//
// A launch opens a new herdr workspace in the directory (unfocused, so the
// terminal you are sitting at stays put), starts the agent in its shell
// pane with agent.start (which answers once the agent is ready for input),
// and submits the first prompt, if any.

const (
	// maxDirEntries caps one directory listing.
	maxDirEntries = 300
	// maxRecentDirs caps the "recent" directories in the launch options.
	maxRecentDirs = 10
	// launchStartTimeout is herdr's wait for the agent to become ready;
	// launchTimeout bounds the whole launch request.
	launchStartTimeout = 60 * time.Second
	launchTimeout      = 90 * time.Second
	// maxLaunchPrompt matches the prompt endpoint's body cap.
	maxLaunchPrompt = 1 << 20
)

// agentNameRe is herdr's rule for agent names.
var agentNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// kindExecutables lists, per kind, executables besides the kind's own
// name that mean it is installed (herdr's detection aliases).
var kindExecutables = map[string][]string{
	"agy":    {"antigravity"},
	"cursor": {"cursor-agent"},
	"kiro":   {"kiro-cli"},
	"muse":   {"muse-cli"},
}

// userBinDirs are searched besides PATH: a systemd user unit's PATH is
// minimal, while agents are usually installed per user.
var userBinDirs = []string{"~/.local/bin", "~/.npm-global/bin", "~/.bun/bin", "~/go/bin", "~/.cargo/bin", "/usr/local/bin", "/opt/homebrew/bin"}

// installed reports whether an executable named name is on PATH or in one
// of userBinDirs.
func installed(name string) bool {
	if _, err := exec.LookPath(name); err == nil {
		return true
	}
	home, _ := os.UserHomeDir()
	for _, d := range userBinDirs {
		if rest, ok := strings.CutPrefix(d, "~/"); ok {
			if home == "" {
				continue
			}
			d = filepath.Join(home, rest)
		}
		if fi, err := os.Stat(filepath.Join(d, name)); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

// launchRoots returns the configured roots that exist, symlinks resolved,
// without duplicates. Checked per request, so a root created later counts.
func (s *Server) launchRoots() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range s.cfg.LaunchRoots {
		if !filepath.IsAbs(r) {
			continue
		}
		p, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(p); err != nil || !fi.IsDir() || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// within reports whether path p (clean, absolute) is root or below it.
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func withinAny(p string, roots []string) bool {
	for _, r := range roots {
		if within(p, r) {
			return true
		}
	}
	return false
}

// launchError is a refused launch request.
type launchError struct {
	status        int
	code, message string
	// workspace is a workspace the failed request left open, if any.
	workspace string
}

// resolveLaunchDir resolves p to a real directory inside one of roots.
// Paths that do not resolve are reported as missing only when they would
// lie inside a root, so the answers say nothing about the rest of the disk.
func resolveLaunchDir(p string, roots []string) (string, *launchError) {
	if p == "" {
		return "", &launchError{status: http.StatusBadRequest, code: "bad_path", message: "a directory is required"}
	}
	if !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		return "", &launchError{status: http.StatusBadRequest, code: "bad_path", message: "the directory must be an absolute path"}
	}
	clean := filepath.Clean(p)
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if withinAny(clean, roots) {
			return "", &launchError{status: http.StatusNotFound, code: "not_found", message: "no such directory"}
		}
		return "", &launchError{status: http.StatusForbidden, code: "outside_roots", message: "that directory is outside the launch roots"}
	}
	if !withinAny(real, roots) {
		return "", &launchError{status: http.StatusForbidden, code: "outside_roots", message: "that directory is outside the launch roots"}
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", &launchError{status: http.StatusBadRequest, code: "not_a_directory", message: "not a directory"}
	}
	return real, nil
}

func writeLaunchError(w http.ResponseWriter, e *launchError) {
	body := map[string]string{"code": e.code, "message": e.message}
	if e.workspace != "" {
		body["workspace_id"] = e.workspace
	}
	writeJSONStatus(w, e.status, body)
}

// homeLabel shows p with the home directory as ~.
func homeLabel(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+"/"); ok {
		return "~/" + rest
	}
	return p
}

// isGitDir reports whether dir is a git checkout (.git is a directory, or
// a file in a linked worktree).
func isGitDir(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// launchDir is one directory offered in the launch sheet.
type launchDir struct {
	Name  string `json:"name,omitempty"`
	Path  string `json:"path"`
	Label string `json:"label,omitempty"`
	Git   bool   `json:"git"`
}

// localAgents is the local host's cached agent list (none without a
// manager, in tests).
func (s *Server) localAgents() []herdr.AgentInfo {
	if s.manager == nil {
		return nil
	}
	return s.manager.Agents()
}

// launchKinds returns the herdr agent kinds that can be launched here:
// those installed on this host plus those already running on it (proof
// enough that they work), in herdr's order.
func (s *Server) launchKinds(running []herdr.AgentInfo) []string {
	have := map[string]bool{}
	for _, a := range running {
		have[a.Agent] = true
	}
	find := s.findExec
	if find == nil {
		find = installed
	}
	var out []string
	for _, k := range herdr.AgentKinds {
		ok := have[k] || find(k)
		for _, alt := range kindExecutables[k] {
			ok = ok || find(alt)
		}
		if ok {
			out = append(out, k)
		}
	}
	return out
}

// defaultKind is the kind most running agents here use, else the first.
func defaultKind(kinds []string, running []herdr.AgentInfo) string {
	if len(kinds) == 0 {
		return ""
	}
	n := map[string]int{}
	for _, a := range running {
		n[a.Agent]++
	}
	best := kinds[0]
	for _, k := range kinds {
		if n[k] > n[best] {
			best = k
		}
	}
	return best
}

// recentDirs returns the distinct working directories of this host's
// agents that lie inside the roots, most recently active first.
func recentDirs(running []herdr.AgentInfo, roots []string) []launchDir {
	agents := append([]herdr.AgentInfo(nil), running...)
	sort.SliceStable(agents, func(i, j int) bool {
		a, b := agents[i].LastActive, agents[j].LastActive
		if a == nil || b == nil {
			return a != nil
		}
		return a.After(*b)
	})
	out := []launchDir{}
	seen := map[string]bool{}
	for _, a := range agents {
		if a.CWD == "" || !filepath.IsAbs(a.CWD) {
			continue
		}
		p, err := filepath.EvalSymlinks(a.CWD)
		if err != nil || seen[p] || !withinAny(p, roots) {
			continue
		}
		seen[p] = true
		out = append(out, launchDir{Name: filepath.Base(p), Path: p, Label: homeLabel(p), Git: isGitDir(p)})
		if len(out) == maxRecentDirs {
			break
		}
	}
	return out
}

// handleLaunchOptions tells the launch sheet what it can offer on this
// host: whether launching is on, the roots, the agent kinds and the
// directories agents recently ran in.
func (s *Server) handleLaunchOptions(w http.ResponseWriter, r *http.Request) {
	running := s.localAgents()
	roots := s.launchRoots()
	resp := map[string]any{"enabled": false, "roots": []launchDir{}, "kinds": []string{}, "default_kind": "", "recent": []launchDir{}}
	switch {
	case len(s.cfg.LaunchRoots) == 0:
		resp["reason"] = "launching is turned off on this host"
	case len(roots) == 0:
		labels := make([]string, len(s.cfg.LaunchRoots))
		for i, r := range s.cfg.LaunchRoots {
			labels[i] = homeLabel(r)
		}
		resp["reason"] = "no launch root exists on this host (" + strings.Join(labels, ", ") + ")"
	default:
		kinds := s.launchKinds(running)
		rootDirs := make([]launchDir, len(roots))
		for i, p := range roots {
			rootDirs[i] = launchDir{Name: homeLabel(p), Path: p, Label: homeLabel(p), Git: isGitDir(p)}
		}
		resp["enabled"] = len(kinds) > 0
		if len(kinds) == 0 {
			resp["reason"] = "no supported agent is installed on this host"
		}
		resp["roots"] = rootDirs
		resp["kinds"] = kinds
		resp["default_kind"] = defaultKind(kinds, running)
		resp["recent"] = recentDirs(running, roots)
		if d := s.coordinatorDir(); d != "" {
			resp["coordinator_dir"] = homeLabel(d)
		}
	}
	writeJSON(w, resp)
}

// handleLaunchDirs lists the subdirectories of ?path= (inside a launch
// root), or the roots themselves without one. Hidden directories are left
// out, and so are symlinks that lead outside the roots.
func (s *Server) handleLaunchDirs(w http.ResponseWriter, r *http.Request) {
	roots := s.launchRoots()
	if len(roots) == 0 {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "launch_disabled", "message": "launching is not available on this host"})
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		entries := make([]launchDir, len(roots))
		for i, root := range roots {
			entries[i] = launchDir{Name: homeLabel(root), Path: root, Git: isGitDir(root)}
		}
		writeJSON(w, map[string]any{"path": "", "entries": entries})
		return
	}
	dir, lerr := resolveLaunchDir(p, roots)
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	list, err := os.ReadDir(dir)
	if err != nil {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "unreadable", "message": "cannot read that directory"})
		return
	}
	entries := []launchDir{}
	for _, e := range list {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		switch {
		case e.IsDir():
		case e.Type()&os.ModeSymlink != 0:
			// Offered only if it leads to a directory the app may open.
			real, err := filepath.EvalSymlinks(full)
			if err != nil || !withinAny(real, roots) {
				continue
			}
			if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
				continue
			}
		default:
			continue
		}
		entries = append(entries, launchDir{Name: name, Path: full})
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if a != b {
			return a < b
		}
		return entries[i].Name < entries[j].Name
	})
	truncated := len(entries) > maxDirEntries
	if truncated {
		entries = entries[:maxDirEntries]
	}
	for i := range entries {
		entries[i].Git = isGitDir(entries[i].Path)
	}
	resp := map[string]any{"path": dir, "label": homeLabel(dir), "git": isGitDir(dir), "entries": entries, "truncated": truncated}
	// The parent is offered while it is still inside a root; above a root
	// the app goes back to the list of roots.
	if parent := filepath.Dir(dir); parent != dir && withinAny(parent, roots) {
		resp["parent"] = parent
	}
	writeJSON(w, resp)
}

// launchBody is the JSON body of a launch request.
type launchBody struct {
	Dir    string `json:"dir"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// Coordinator: the "Coordinator" preset. The agent starts in the
	// coordinator directory (Dir is ignored; it need not be in a launch
	// root), is named coordinator (or coordinator-N), gets the harness's
	// permission arguments and a first prompt pointing it at the
	// playbook, and is recorded as a coordinator on the hub.
	Coordinator bool `json:"coordinator"`
}

// coordinatorDir is where the Coordinator preset starts agents, symlinks
// resolved ("" when unknown).
func (s *Server) coordinatorDir() string {
	d := s.cfg.CoordinatorDir
	if d == "" || d == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		d = home
	}
	if !filepath.IsAbs(d) {
		return ""
	}
	real, err := filepath.EvalSymlinks(d)
	if err != nil {
		return ""
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return ""
	}
	return real
}

// handleLaunch starts a new agent: a fresh workspace in the directory, the
// agent started in its pane, then the first prompt. A failure after the
// workspace exists leaves it open (with its shell) for the user to look
// at, and says so.
func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	var body launchBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLaunchPrompt+64<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	roots := s.launchRoots()
	if len(roots) == 0 {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "launch_disabled", "message": "launching is not available on this host"})
		return
	}
	var dir string
	if body.Coordinator {
		// The one launch outside the roots: a fixed directory from the
		// server's configuration, never one from the request.
		if dir = s.coordinatorDir(); dir == "" {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "no_coordinator_dir", "message": "the coordinator directory (-coordinator-dir) does not exist on this host"})
			return
		}
	} else {
		var lerr *launchError
		if dir, lerr = resolveLaunchDir(body.Dir, roots); lerr != nil {
			writeLaunchError(w, lerr)
			return
		}
	}
	running := s.localAgents()
	kinds := s.launchKinds(running)
	kind := body.Kind
	if kind == "" {
		kind = defaultKind(kinds, running)
	}
	known := false
	for _, k := range kinds {
		known = known || k == kind
	}
	if !known {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_kind", "message": "agent kind " + kind + " is not available on this host"})
		return
	}
	name := strings.TrimSpace(body.Name)
	named := name != ""
	if named && !agentNameRe.MatchString(name) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"code": "bad_name", "message": "names are lowercase letters, digits, - and _, starting with a letter (at most 32)"})
		return
	}
	if len(body.Prompt) > maxLaunchPrompt {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "prompt_too_large", "message": "the prompt is too long"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), launchTimeout)
	defer cancel()
	// Names must be unique among live agents: check herdr itself rather
	// than the cache, which may lag a launch made a moment ago.
	live, err := s.client.ListAgents(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	taken := map[string]bool{}
	for _, a := range live {
		if a.Name != "" {
			taken[a.Name] = true
		}
	}
	switch {
	case !named && body.Coordinator:
		name = defaultAgentName(RoleCoordinator, func(n string) bool { return taken[n] })
	case !named:
		name = defaultAgentName(dir, func(n string) bool { return taken[n] })
	case taken[name]:
		writeJSONStatus(w, http.StatusConflict, map[string]string{"code": "agent_name_taken", "message": "an agent named " + name + " is already running"})
		return
	}
	var args []string
	if body.Coordinator {
		args = append(playbook.AgentArgs(kind, name), s.cfg.CoordinatorArgs[kind]...)
		if strings.TrimSpace(body.Prompt) == "" {
			body.Prompt = playbook.FirstPrompt(kind)
		}
	}

	// The first prompt goes on the command line where the harness takes
	// one; otherwise it is sent once the agent is up (below).
	promptArgs := firstPromptArgs(kind, body.Prompt)
	args = append(args, promptArgs...)

	label := filepath.Base(dir)
	if named || body.Coordinator {
		label = name
	}
	ws, err := s.client.CreateWorkspace(ctx, dir, label, false)
	if err != nil {
		writeError(w, err)
		return
	}
	pane := ws.RootPaneID
	if pane == "" {
		panes, err := s.client.ListPanes(ctx, ws.WorkspaceID)
		if err == nil && len(panes) > 0 {
			pane = panes[0].PaneID
		}
		if pane == "" {
			s.launchFailed(w, ws, "find the new workspace's pane", err)
			return
		}
	}
	agent, err := s.startAgent(ctx, name, kind, pane, args...)
	if err != nil {
		s.launchFailed(w, ws, "start "+kind, err)
		return
	}
	if s.manager != nil {
		s.manager.Refresh()
	}
	if agent.PaneID == "" {
		agent.PaneID = pane
	}
	host := s.fleet.LocalName()
	id := state.QualifiedID(host, agent.PaneID)
	resp := map[string]any{
		"agent": browserAgent{
			ID: id, Host: host, PaneID: agent.PaneID,
			Name: name, Agent: kind, Status: agent.Status, CWD: dir,
		},
		"workspace_id": ws.WorkspaceID,
	}
	if body.Coordinator {
		resp["coordinator"] = true
		if s.roles != nil { // on the hub; a hub launching on a peer records it itself
			if err := s.recordRole(id, name, RoleCoordinator, s.by(r, "")); err != nil {
				log.Printf("roles: %v", err)
			}
		}
	}
	if promptArgs == nil && strings.TrimSpace(body.Prompt) != "" {
		// Submission only: the app shows the agent working from here.
		if _, err := s.client.Prompt(ctx, name, body.Prompt, herdr.PromptOptions{}); err != nil {
			resp["prompt_error"] = "the agent started, but the first prompt was not sent: " + errMessage(err)
		}
	}
	writeJSON(w, resp)
}

// maxArgPrompt is the longest first prompt passed on the command line;
// herdr types the command into the pane's shell.
const maxArgPrompt = 4 << 10

// firstPromptArgs passes a launch's first prompt as the harness's own
// initial-prompt argument (claude [prompt], pi [messages...]), nil when the
// prompt has to be sent with agent.prompt instead. Sent with agent.prompt
// right after agent.start, it can reach the TUI before it reads input and
// be lost; a coordinator then never learns its role. "--" ends option
// parsing, so a variadic option (claude's --allowedTools) cannot swallow
// the prompt and a prompt starting with "-" stays a prompt. Multi-line and
// long prompts go through agent.prompt, since they would be typed into a
// shell.
func firstPromptArgs(kind, prompt string) []string {
	if strings.TrimSpace(prompt) == "" || len(prompt) > maxArgPrompt || strings.ContainsAny(prompt, "\r\n") {
		return nil
	}
	switch kind {
	case "claude", "pi":
		return []string{"--", prompt}
	}
	return nil
}

// startAgent runs agent.start and then waits for the launch to become
// an active named agent. It retries briefly while herdr reports the pane
// unavailable: a brand-new workspace's shell may still be starting up
// (reading its rc files) when the first attempt arrives, and herdr also
// refuses a pane whose shell is not yet at its prompt with
// agent_pane_busy. Once herdr admits the launch it waits for the name to
// become promptable: herdr's socket API answers agent.start as soon as
// the launch is queued, before the agent is detected in the pane and
// ready for input, and agent.prompt refuses a name whose launch has not
// activated ("is not an active named agent") — the wait mirrors what
// herdr's own CLI does after agent.start.
func (s *Server) startAgent(ctx context.Context, name, kind, pane string, args ...string) (herdr.AgentInfo, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		a, err := s.client.StartAgent(ctx, name, kind, pane, launchStartTimeout.Milliseconds(), args...)
		if err == nil {
			return a, s.waitForNamedAgent(ctx, name)
		}
		var he *herdr.Error
		if !errors.As(err, &he) || (he.Code != "agent_pane_unavailable" && he.Code != "agent_pane_busy") || time.Now().After(deadline) {
			return a, err
		}
		select {
		case <-ctx.Done():
			return a, err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// waitForNamedAgent polls agent.get until herdr resolves name to an
// agent that is ready for input (InteractiveReady): herdr resolves the
// name as soon as the launch is registered, but agent.prompt needs the
// launch to have activated, a few seconds later, once the agent is
// detected in its pane. herdr drops a launch that never activates, so
// the wait is also where a broken launch is reported instead of every
// later prompt failing with "is not an active named agent".
func (s *Server) waitForNamedAgent(ctx context.Context, name string) error {
	waitCtx, cancel := context.WithTimeout(ctx, s.agentReadyWait)
	defer cancel()
	for {
		a, err := s.client.GetAgent(waitCtx, name)
		switch {
		case err == nil && a.InteractiveReady:
			return nil
		case err != nil:
			var he *herdr.Error
			if !errors.As(err, &he) {
				return fmt.Errorf("agent %s: %w", name, err) // transport or parse error, not "not ready yet"
			}
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("agent %s did not become an active named agent within %s", name, s.agentReadyWait)
		case <-time.After(s.agentReadyPoll):
		}
	}
}

// launchFailed reports a launch that failed after its workspace was
// created. The workspace is left for the user to inspect.
func (s *Server) launchFailed(w http.ResponseWriter, ws herdr.CreatedWorkspace, step string, err error) {
	code, status := "launch_failed", http.StatusBadGateway
	var he *herdr.Error
	if errors.As(err, &he) {
		code = he.Code
		if he.Code == "agent_name_taken" {
			status = http.StatusConflict
		}
	}
	label := ws.Label
	if label == "" {
		label = ws.WorkspaceID
	}
	writeJSONStatus(w, status, map[string]string{
		"code":         code,
		"message":      fmt.Sprintf("could not %s: %s. The new workspace %q is left open in herdr for you to inspect or close.", step, errMessage(err), label),
		"workspace_id": ws.WorkspaceID,
	})
}

// errMessage is err's text, herdr's message alone for a herdr error.
func errMessage(err error) string {
	if err == nil {
		return "unknown error"
	}
	var he *herdr.Error
	if errors.As(err, &he) && he.Message != "" {
		return he.Message
	}
	return err.Error()
}

// defaultAgentName derives an agent name from the directory's name
// (lowercased, other characters folded into -), adding -2, -3, ... when
// taken says the name is in use.
func defaultAgentName(dir string, taken func(string) bool) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(filepath.Base(dir)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(b.String(), "-_")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = strings.TrimRight("agent-"+name, "-")
	}
	if len(name) > 28 { // room for a -NN suffix
		name = strings.TrimRight(name[:28], "-_")
	}
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", name, i); !taken(n) {
			return n
		}
	}
}

// handleHostLaunch is POST /api/hosts/{host}/launch: the local host's
// launch, or a peer's through the hub. A Coordinator launch on a peer is
// made by the hub itself (fleet.CallHost) so it can record the new agent's
// role; any other launch is proxied as is.
func (s *Server) handleHostLaunch(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	switch {
	case host == s.fleet.LocalName():
		s.handleLaunch(w, r)
		return
	case !s.fleet.HasPeer(host):
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "unknown_host", "message": "no such host"})
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLaunchPrompt+64<<10))
	if err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	var body launchBody
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if !body.Coordinator {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		s.fleet.ProxyHost(w, r, host, "launch")
		return
	}
	status, data, err := s.fleet.CallHost(r.Context(), host, "launch", nil, raw)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"code": "host_unreachable", "message": err.Error()})
		return
	}
	if status == http.StatusOK && s.roles != nil {
		var res struct {
			Agent browserAgent `json:"agent"`
		}
		if json.Unmarshal(data, &res) == nil && res.Agent.PaneID != "" {
			if err := s.recordRole(state.QualifiedID(host, res.Agent.PaneID), res.Agent.Name, RoleCoordinator, s.by(r, "")); err != nil {
				log.Printf("roles: %v", err)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// onHostLaunch routes a host-qualified launch call like onHost does for
// agent calls: the local host's to the local handler, a peer's to it.
func (s *Server) onHostLaunch(op string, local http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := r.PathValue("host")
		switch {
		case host == s.fleet.LocalName():
			local(w, r)
		case s.fleet.HasPeer(host):
			s.fleet.ProxyHost(w, r, host, op)
		default:
			writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "unknown_host", "message": "no such host"})
		}
	}
}

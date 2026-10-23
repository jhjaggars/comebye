// Package herdr is a minimal client for herdr's local socket API.
//
// Protocol (v22, verified against a live server):
//
//   - Transport is a unix socket carrying newline-delimited JSON.
//   - A connection answers exactly ONE request and is then closed by the
//     server, with one exception: events.subscribe converts the connection
//     into a long-lived push stream.
//   - Requests:  {"id":"<string>","method":"...","params":{...}}
//   - Responses: {"id":"...","result":{"type":"...",...}} or
//     {"id":"...","error":{"code":"...","message":"..."}}
//   - Push events on a subscription stream: {"event":"<kind>","data":{...}}
//     with no id field. If the subscriber falls behind, the server sends
//     {"id":"<original>","error":{"code":"events_lost",...}} and closes.
package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultSocketPath is herdr's default API socket location.
const DefaultSocketPath = "~/.config/herdr/herdr.sock"

// Error is a structured error response from the server.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// IsAgentBlocked reports whether err is herdr's "agent is waiting for input"
// refusal, which agent.prompt returns instead of typing into a dialog.
func IsAgentBlocked(err error) bool {
	var he *Error
	return errors.As(err, &he) && he.Code == "agent_blocked"
}

// envelope covers responses, push events, and stream errors in one shape.
type envelope struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
	Event  string          `json:"event"`
	Data   json.RawMessage `json:"data"`
}

// Client issues one-shot requests against the herdr socket. Each Call opens
// its own connection, so Calls are safe for concurrent use.
type Client struct {
	socketPath string
	nextID     atomic.Int64
}

// NewClient returns a client for the socket at socketPath.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// SocketPath reports the socket path the client was constructed with.
func (c *Client) SocketPath() string { return c.socketPath }

// Call sends one request and returns the raw "result" payload. A herdr-side
// failure is returned as *Error.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := fmt.Sprintf("hp%d", c.nextID.Add(1))
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial herdr socket: %w", err)
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	req := struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{ID: id, Method: method, Params: params}
	if req.Params == nil {
		req.Params = struct{}{}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if env.ID != id {
		return nil, fmt.Errorf("unexpected response id %q (want %q)", env.ID, id)
	}
	if env.Error != nil {
		return nil, env.Error
	}
	return env.Result, nil
}

// AgentStatus is herdr's agent lifecycle state.
type AgentStatus string

const (
	StatusIdle    AgentStatus = "idle"
	StatusWorking AgentStatus = "working"
	StatusBlocked AgentStatus = "blocked"
	StatusDone    AgentStatus = "done"
	StatusUnknown AgentStatus = "unknown"
)

// AgentSessionInfo references the agent's own session (e.g. a Pi JSONL file).
type AgentSessionInfo struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"` // "id" | "path"
	Value  string `json:"value"`
}

// AgentInfo is one recognized agent occupying a pane. Only the fields the
// phone app uses are typed; the rest are dropped. Worktree is not reported
// by herdr's agent.list — the state manager joins it in from the session
// snapshot's workspace data before broadcasting.
type AgentInfo struct {
	Name             string            `json:"name"`
	PaneID           string            `json:"pane_id"`
	WorkspaceID      string            `json:"workspace_id"`
	TabID            string            `json:"tab_id"`
	TerminalID       string            `json:"terminal_id"`
	Agent            string            `json:"agent"`
	DisplayAgent     string            `json:"display_agent"`
	Status           AgentStatus       `json:"agent_status"`
	Title            string            `json:"terminal_title"`
	TitleStripped    string            `json:"terminal_title_stripped"`
	CWD              string            `json:"cwd"`
	Focused          bool              `json:"focused"`
	StateLabels      map[string]string `json:"state_labels"`
	AgentSession     *AgentSessionInfo `json:"agent_session"`
	Revision         uint64            `json:"revision"`
	InteractiveReady bool              `json:"interactive_ready"`
	Worktree         *WorktreeInfo     `json:"worktree,omitempty"`
	// LastActive is when the agent last did something: the Pi session
	// file's mtime when there is one, else when this server first saw the
	// agent in its current status. herdr itself reports no timestamps.
	LastActive *time.Time `json:"last_active,omitempty"`
	// StatusSince is when comebye saw the agent enter its current
	// status (so the app can show "working for 2m"); unknown after a
	// restart until the next change, when it is the restart time.
	StatusSince *time.Time `json:"status_since,omitempty"`
	// Host names the comebye instance that owns the agent and ID is the
	// host-qualified "<host>/<pane_id>". Pane IDs are only unique per host.
	// Neither comes from herdr: comebye stamps them before broadcasting.
	Host string `json:"host,omitempty"`
	ID   string `json:"id,omitempty"`
}

// WorktreeInfo describes the repository checkout hosting a workspace.
// Branch is the trailing path segment of a linked worktree's checkout
// (the ~/.worktrees/<repo>/<branch> convention); it is empty for the main
// checkout.
type WorktreeInfo struct {
	RepoName     string `json:"repo"`
	CheckoutPath string `json:"checkout"`
	IsLinked     bool   `json:"linked"`
	Branch       string `json:"branch,omitempty"`
}

// DisplayName picks the best human label for an agent.
func (a AgentInfo) DisplayName() string {
	switch {
	case a.Name != "":
		return a.Name
	case a.TitleStripped != "":
		return a.TitleStripped
	case a.DisplayAgent != "":
		return a.DisplayAgent
	case a.Agent != "":
		return a.Agent
	default:
		return a.PaneID
	}
}

// ListAgents fetches the current agent list (agent.list).
func (c *Client) ListAgents(ctx context.Context) ([]AgentInfo, error) {
	raw, err := c.Call(ctx, "agent.list", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Type   string      `json:"type"`
		Agents []AgentInfo `json:"agents"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse agent.list: %w", err)
	}
	return res.Agents, nil
}

// WorkspaceWorktrees reads session.snapshot and maps workspace_id to the
// repository checkout hosting that workspace. This is the authoritative
// source for repo/branch display.
func (c *Client) WorkspaceWorktrees(ctx context.Context) (map[string]*WorktreeInfo, error) {
	raw, err := c.Call(ctx, "session.snapshot", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Snapshot struct {
			Workspaces []struct {
				WorkspaceID string `json:"workspace_id"`
				Worktree    *struct {
					RepoName     string `json:"repo_name"`
					RepoRoot     string `json:"repo_root"`
					CheckoutPath string `json:"checkout_path"`
					IsLinked     bool   `json:"is_linked_worktree"`
				} `json:"worktree"`
			} `json:"workspaces"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse session.snapshot: %w", err)
	}
	out := make(map[string]*WorktreeInfo, len(res.Snapshot.Workspaces))
	for _, w := range res.Snapshot.Workspaces {
		if w.Worktree == nil {
			continue
		}
		info := &WorktreeInfo{
			RepoName:     w.Worktree.RepoName,
			CheckoutPath: w.Worktree.CheckoutPath,
			IsLinked:     w.Worktree.IsLinked,
		}
		if info.IsLinked && info.CheckoutPath != "" {
			// ~/.worktrees/<repo>/<branch> convention: the trailing segment
			// is the branch (or the worktree's label).
			parts := strings.Split(strings.Trim(info.CheckoutPath, "/"), "/")
			info.Branch = parts[len(parts)-1]
		}
		out[w.WorkspaceID] = info
	}
	return out, nil
}

// ReadResult is the payload of pane.read / agent.read.
type ReadResult struct {
	Text      string `json:"text"`
	Revision  uint64 `json:"revision"`
	Truncated bool   `json:"truncated"`
	Format    string `json:"format"`
	Source    string `json:"source"`
}

// ReadPane reads a pane's recent output. source is typically "recent_unwrapped".
func (c *Client) ReadPane(ctx context.Context, paneID, source string, lines int) (ReadResult, error) {
	params := struct {
		PaneID string `json:"pane_id"`
		Source string `json:"source"`
		Lines  *int   `json:"lines"`
	}{PaneID: paneID, Source: source}
	if lines > 0 {
		params.Lines = &lines
	}
	raw, err := c.Call(ctx, "pane.read", params)
	if err != nil {
		return ReadResult{}, err
	}
	var res struct {
		Type string     `json:"type"`
		Read ReadResult `json:"read"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return ReadResult{}, fmt.Errorf("parse pane.read: %w", err)
	}
	return res.Read, nil
}

// PromptOptions controls the optional wait attached to agent.prompt.
type PromptOptions struct {
	Wait      bool
	Until     []AgentStatus
	TimeoutMs int64
}

// Prompt sends text to an agent via agent.prompt. target is a live agent name
// or the hosting pane ID. With Wait, herdr starts a wait in the same call so
// "send this, tell me when done" has no race; the response then reflects the
// post-wait agent state.
func (c *Client) Prompt(ctx context.Context, target, text string, opts PromptOptions) (AgentInfo, error) {
	params := struct {
		Target string      `json:"target"`
		Text   string      `json:"text"`
		Wait   *waitParams `json:"wait"`
	}{Target: target, Text: text}
	if opts.Wait {
		w := &waitParams{Until: make([]string, len(opts.Until))}
		for i, s := range opts.Until {
			w.Until[i] = string(s)
		}
		if opts.TimeoutMs > 0 {
			t := opts.TimeoutMs
			w.TimeoutMs = &t
		}
		params.Wait = w
	}
	raw, err := c.Call(ctx, "agent.prompt", params)
	if err != nil {
		return AgentInfo{}, err
	}
	var res struct {
		Type  string    `json:"type"`
		Agent AgentInfo `json:"agent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return AgentInfo{}, fmt.Errorf("parse agent.prompt: %w", err)
	}
	return res.Agent, nil
}

type waitParams struct {
	Until     []string `json:"until"`
	TimeoutMs *int64   `json:"timeout_ms"`
}

// AgentKinds are the kinds agent.start accepts (herdr 0.9.5, protocol 22;
// the schema types kind as a plain string, the CLI lists the values).
var AgentKinds = []string{
	"pi", "claude", "codex", "gemini", "cursor", "devin", "agy", "cline",
	"omp", "mastracode", "opencode", "copilot", "kimi", "kiro", "droid",
	"amp", "grok", "hermes", "kilo", "qodercli", "qwen", "letta", "maki", "muse",
}

// CreatedWorkspace is the part of workspace.create's answer the app uses.
type CreatedWorkspace struct {
	WorkspaceID string
	Label       string
	// RootPaneID is the new workspace's only pane (empty if herdr did not
	// report one; ListPanes finds it then).
	RootPaneID string
}

// CreateWorkspace opens a new workspace (one tab, one shell pane) in cwd.
// With focus false the user's terminal stays where it is.
func (c *Client) CreateWorkspace(ctx context.Context, cwd, label string, focus bool) (CreatedWorkspace, error) {
	raw, err := c.Call(ctx, "workspace.create", struct {
		CWD   string `json:"cwd"`
		Label string `json:"label,omitempty"`
		Focus bool   `json:"focus"`
	}{CWD: cwd, Label: label, Focus: focus})
	if err != nil {
		return CreatedWorkspace{}, err
	}
	var res struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
			Label       string `json:"label"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return CreatedWorkspace{}, fmt.Errorf("parse workspace.create: %w", err)
	}
	if res.Workspace.WorkspaceID == "" {
		return CreatedWorkspace{}, errors.New("workspace.create returned no workspace")
	}
	return CreatedWorkspace{WorkspaceID: res.Workspace.WorkspaceID, Label: res.Workspace.Label, RootPaneID: res.RootPane.PaneID}, nil
}

// PaneInfo is the part of a pane.list entry the app uses.
type PaneInfo struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	Agent       string `json:"agent"`
	CWD         string `json:"cwd"`
}

// ListPanes lists the panes of one workspace (all panes when workspaceID
// is empty).
func (c *Client) ListPanes(ctx context.Context, workspaceID string) ([]PaneInfo, error) {
	params := struct {
		WorkspaceID string `json:"workspace_id,omitempty"`
	}{WorkspaceID: workspaceID}
	raw, err := c.Call(ctx, "pane.list", params)
	if err != nil {
		return nil, err
	}
	var res struct {
		Panes []PaneInfo `json:"panes"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse pane.list: %w", err)
	}
	return res.Panes, nil
}

// ClosePane closes one pane (pane.close). A workspace's last pane closing
// closes the workspace too.
func (c *Client) ClosePane(ctx context.Context, paneID string) error {
	_, err := c.Call(ctx, "pane.close", struct {
		PaneID string `json:"pane_id"`
	}{PaneID: paneID})
	return err
}

// Workspace is the part of a workspace.list entry the app uses.
type Workspace struct {
	WorkspaceID string             `json:"workspace_id"`
	Label       string             `json:"label"`
	PaneCount   int                `json:"pane_count"`
	Worktree    *WorkspaceWorktree `json:"worktree"`
}

// WorkspaceWorktree is the checkout hosting a workspace (null for a
// workspace outside any git repository).
type WorkspaceWorktree struct {
	RepoName     string `json:"repo_name"`
	RepoRoot     string `json:"repo_root"`
	CheckoutPath string `json:"checkout_path"`
	IsLinked     bool   `json:"is_linked_worktree"`
}

// ListWorkspaces lists every workspace (workspace.list).
func (c *Client) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	raw, err := c.Call(ctx, "workspace.list", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse workspace.list: %w", err)
	}
	return res.Workspaces, nil
}

// CloseWorkspace closes one workspace (workspace.close), never its group:
// a repository's primary workspace keeps its nested worktree workspaces.
func (c *Client) CloseWorkspace(ctx context.Context, workspaceID string) error {
	_, err := c.Call(ctx, "workspace.close", struct {
		WorkspaceID string `json:"workspace_id"`
		CloseGroup  bool   `json:"close_group"`
	}{WorkspaceID: workspaceID})
	return err
}

// Worktree is one entry of worktree.list.
type Worktree struct {
	Path            string `json:"path"`
	Branch          string `json:"branch"`
	Label           string `json:"label"`
	IsLinked        bool   `json:"is_linked_worktree"`
	IsBare          bool   `json:"is_bare"`
	IsDetached      bool   `json:"is_detached"`
	IsPrunable      bool   `json:"is_prunable"`
	OpenWorkspaceID string `json:"open_workspace_id"`
}

// ListWorktrees lists the worktrees of the repository containing cwd
// (worktree.list).
func (c *Client) ListWorktrees(ctx context.Context, cwd string) ([]Worktree, error) {
	raw, err := c.Call(ctx, "worktree.list", struct {
		CWD string `json:"cwd"`
	}{CWD: cwd})
	if err != nil {
		return nil, err
	}
	var res struct {
		Worktrees []Worktree `json:"worktrees"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("parse worktree.list: %w", err)
	}
	return res.Worktrees, nil
}

// WorktreeRequest is a worktree.create or worktree.open request. CWD is
// inside the repository; herdr nests the new workspace under the
// repository's primary workspace.
type WorktreeRequest struct {
	CWD    string `json:"cwd"`
	Branch string `json:"branch,omitempty"`
	Base   string `json:"base,omitempty"` // create only: start point of a new branch
	Path   string `json:"path,omitempty"`
	Label  string `json:"label,omitempty"`
	Focus  bool   `json:"focus"`
}

// OpenedWorktree is the part of worktree.create's or worktree.open's
// answer the app uses.
type OpenedWorktree struct {
	WorkspaceID string
	Label       string
	RootPaneID  string
	Path        string
	Branch      string
	// AlreadyOpen: worktree.open found the worktree's workspace open.
	AlreadyOpen bool
}

// CreateWorktree creates a git worktree and opens it as a workspace
// (worktree.create); the branch is created from Base when it does not
// exist yet.
func (c *Client) CreateWorktree(ctx context.Context, req WorktreeRequest) (OpenedWorktree, error) {
	return c.worktreeCall(ctx, "worktree.create", req)
}

// OpenWorktree opens an existing worktree's workspace, or finds it open
// (worktree.open). Base is ignored.
func (c *Client) OpenWorktree(ctx context.Context, req WorktreeRequest) (OpenedWorktree, error) {
	req.Base = ""
	return c.worktreeCall(ctx, "worktree.open", req)
}

func (c *Client) worktreeCall(ctx context.Context, method string, req WorktreeRequest) (OpenedWorktree, error) {
	raw, err := c.Call(ctx, method, req)
	if err != nil {
		return OpenedWorktree{}, err
	}
	var res struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
			Label       string `json:"label"`
		} `json:"workspace"`
		RootPane struct {
			PaneID string `json:"pane_id"`
		} `json:"root_pane"`
		Worktree struct {
			Path   string `json:"path"`
			Branch string `json:"branch"`
		} `json:"worktree"`
		AlreadyOpen bool `json:"already_open"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return OpenedWorktree{}, fmt.Errorf("parse %s: %w", method, err)
	}
	if res.Workspace.WorkspaceID == "" {
		return OpenedWorktree{}, fmt.Errorf("%s returned no workspace", method)
	}
	return OpenedWorktree{
		WorkspaceID: res.Workspace.WorkspaceID, Label: res.Workspace.Label, RootPaneID: res.RootPane.PaneID,
		Path: res.Worktree.Path, Branch: res.Worktree.Branch, AlreadyOpen: res.AlreadyOpen,
	}, nil
}

// StartAgent starts a supported agent kind under name in paneID, which
// must be at an interactive shell prompt. herdr answers once the launch
// is admitted, before the agent is detected in the pane and ready for
// input; a caller must wait for the name to become an active named agent
// (GetAgent) before prompting it. timeoutMs (3001..300000; 0 leaves
// herdr's default of 30s) bounds herdr's own wait for the agent. args are
// passed to the agent's executable after its name (herdr's
// AgentStartParams.args); they are always sent, even empty, matching the
// herdr CLI's serialization.
func (c *Client) StartAgent(ctx context.Context, name, kind, paneID string, timeoutMs int64, args ...string) (AgentInfo, error) {
	if args == nil {
		args = []string{}
	}
	params := struct {
		Name      string   `json:"name"`
		Kind      string   `json:"kind"`
		PaneID    string   `json:"pane_id"`
		TimeoutMs *int64   `json:"timeout_ms,omitempty"`
		Args      []string `json:"args"`
	}{Name: name, Kind: kind, PaneID: paneID, Args: args}
	if timeoutMs > 0 {
		params.TimeoutMs = &timeoutMs
	}
	raw, err := c.Call(ctx, "agent.start", params)
	if err != nil {
		return AgentInfo{}, err
	}
	var res struct {
		Agent AgentInfo `json:"agent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return AgentInfo{}, fmt.Errorf("parse agent.start: %w", err)
	}
	return res.Agent, nil
}

// GetAgent fetches one agent by name or pane ID (agent.get). A name
// resolves as soon as its launch is registered — before the agent is
// ready for input — and stops resolving when the launch is dropped or
// never registers; InteractiveReady on the answer says when prompting
// will be accepted.
func (c *Client) GetAgent(ctx context.Context, target string) (AgentInfo, error) {
	raw, err := c.Call(ctx, "agent.get", struct {
		Target string `json:"target"`
	}{Target: target})
	if err != nil {
		return AgentInfo{}, err
	}
	var res struct {
		Type  string    `json:"type"`
		Agent AgentInfo `json:"agent"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return AgentInfo{}, fmt.Errorf("parse agent.get: %w", err)
	}
	return res.Agent, nil
}

// allowedKeys is the closed set of key names the phone app may send. This is
// deliberate defense in depth: the web layer is a remote shell, so key input
// is restricted to navigation/interrupt keys that make sense from a phone,
// plus the single characters that answer selection dialogs: digits 1-9
// (Claude Code-style numbered choices) and y/n ([y/N] confirmations). No
// other printable characters, so free text still has to go through
// agent.prompt, which refuses while a dialog is open.
var allowedKeys = map[string]bool{
	"esc": true, "enter": true, "tab": true, "space": true,
	"up": true, "down": true, "left": true, "right": true,
	"home": true, "end": true, "pageup": true, "pagedown": true,
	"backspace": true, "delete": true, "ctrl+c": true, "ctrl+d": true,
	"ctrl+z": true, "ctrl+u": true, "ctrl+w": true, "ctrl+l": true,
	"1": true, "2": true, "3": true, "4": true, "5": true,
	"6": true, "7": true, "8": true, "9": true,
	"y": true, "n": true,
}

// ValidKey reports whether key is in the sendable whitelist.
func ValidKey(key string) bool { return allowedKeys[key] }

// SendKeys sends whitelisted key names to the agent occupying target.
func (c *Client) SendKeys(ctx context.Context, target string, keys []string) error {
	for _, k := range keys {
		if !allowedKeys[k] {
			return fmt.Errorf("key %q not allowed", k)
		}
	}
	_, err := c.Call(ctx, "agent.send_keys", struct {
		Target string   `json:"target"`
		Keys   []string `json:"keys"`
	}{Target: target, Keys: keys})
	return err
}

// PushEvent is one event pushed on a subscription stream.
type PushEvent struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

// AgentStatusChangedData is the data payload of pane.agent_status_changed.
type AgentStatusChangedData struct {
	PaneID       string            `json:"pane_id"`
	WorkspaceID  string            `json:"workspace_id"`
	Status       AgentStatus       `json:"agent_status"`
	Agent        string            `json:"agent"`
	DisplayAgent string            `json:"display_agent"`
	Title        string            `json:"title"`
	StateLabels  map[string]string `json:"state_labels"`
}

// Subscription describes one entry in an events.subscribe request.
type Subscription struct {
	Type   string `json:"type"`
	PaneID string `json:"pane_id,omitempty"`
}

// StatusSubscription is a convenience constructor for the per-pane
// agent_status_changed subscription, which requires a concrete pane_id.
func StatusSubscription(paneID string) Subscription {
	return Subscription{Type: "pane.agent_status_changed", PaneID: paneID}
}

// GlobalSubscriptions are pane-level subscriptions that need no pane_id. They
// cover pane lifecycle (and thus the need to rebuild the per-pane status
// subscription set), agent detection, and generic pane updates.
func GlobalSubscriptions() []Subscription {
	types := []string{
		"pane.created", "pane.updated", "pane.closed", "pane.exited",
		"pane.moved", "pane.agent_detected",
	}
	subs := make([]Subscription, len(types))
	for i, t := range types {
		subs[i] = Subscription{Type: t}
	}
	return subs
}

// streamConn is one live subscription connection.
type streamConn struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Stream runs a self-healing subscription connection. It reconnects with
// backoff on errors, events_lost, or connection close, calling onEvent for
// each push and onResync whenever the stream (re)starts or is torn down so
// consumers can re-read authoritative state. The per-pane subscription set is
// fixed per connection by herdr, so callers must Restart when the pane set
// changes.
type Stream struct {
	client *Client
	mu     sync.Mutex
	subs   []Subscription
	conn   *streamConn

	onEvent  func(PushEvent)
	onResync func(reason string)
}

// NewStream creates a Stream; it is not running until Start.
func NewStream(client *Client, onEvent func(PushEvent), onResync func(reason string)) *Stream {
	return &Stream{client: client, onEvent: onEvent, onResync: onResync}
}

// Start launches the stream with the given subscription set. Calling Start
// again while running is a no-op unless the set changed.
func (s *Stream) Start(subs []Subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && sameSubs(s.subs, subs) {
		return
	}
	if s.conn != nil {
		s.conn.cancel()
		<-s.conn.done // wait for the old loop to fully drain
	}
	s.subs = append([]Subscription(nil), subs...)
	ctx, cancel := context.WithCancel(context.Background())
	conn := &streamConn{cancel: cancel, done: make(chan struct{})}
	s.conn = conn
	go s.loop(ctx, conn, s.subs)
}

// Stop tears the stream down.
func (s *Stream) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.cancel()
		<-s.conn.done
		s.conn = nil
	}
}

func sameSubs(a, b []Subscription) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[Subscription]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

func (s *Stream) loop(ctx context.Context, conn *streamConn, subs []Subscription) {
	defer close(conn.done)
	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return
		}
		lost, err := s.runOnce(ctx, subs)
		if ctx.Err() != nil {
			return
		}
		reason := "stream_closed"
		if lost {
			reason = "events_lost"
		} else if err != nil {
			reason = "stream_error: " + err.Error()
		}
		if s.onResync != nil {
			s.onResync(reason)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}
}

// runOnce dials, subscribes, and pumps events until the stream ends. It
// returns lost=true when herdr reported events_lost.
func (s *Stream) runOnce(ctx context.Context, subs []Subscription) (lost bool, err error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", s.client.socketPath)
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// The subscribe request's id; the ack and any events_lost error echo it.
	const subID = "hp-sub"
	req := struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params struct {
			Subscriptions []Subscription `json:"subscriptions"`
		} `json:"params"`
	}{ID: subID, Method: "events.subscribe"}
	req.Params.Subscriptions = subs
	body, _ := json.Marshal(req)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return false, fmt.Errorf("write subscribe: %w", err)
	}

	// No hard deadline once subscribed; rely on ctx for cancellation and a
	// read heartbeat check below.
	_ = conn.SetDeadline(time.Time{})

	type readerResult struct {
		lost bool
		err  error
	}
	ch := make(chan readerResult, 1)
	go func() {
		r := bufio.NewReader(conn)
		for {
			line, rerr := r.ReadBytes('\n')
			if len(line) > 0 {
				var env envelope
				if jerr := json.Unmarshal(line, &env); jerr == nil {
					switch {
					case env.Error != nil:
						if env.Error.Code == "events_lost" {
							ch <- readerResult{lost: true}
							return
						}
						ch <- readerResult{err: env.Error}
						return
					case env.Event != "":
						// Push events carry no id; the ack (id == subID,
						// subscription_started) needs no action.
						if s.onEvent != nil {
							s.onEvent(PushEvent{Event: env.Event, Data: env.Data})
						}
					}
					continue
				}
			}
			if rerr != nil {
				ch <- readerResult{err: rerr}
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		_ = conn.Close()
		<-ch // reader goroutine exits once the conn is closed
		return false, ctx.Err()
	case res := <-ch:
		return res.lost, res.err
	}
}

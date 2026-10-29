package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/agenttoken"
)

// Header names, as the server expects them (see server/agentauth.go).
const (
	headerToken     = "X-Comebye-Agent-Token"
	headerAgent     = "X-Comebye-Agent"
	headerAgentHost = "X-Comebye-Agent-Host"
)

// DefaultURL is the local Comebye's address.
const DefaultURL = "http://127.0.0.1:8741"

// StateDir is the server's default state directory: COMEBYE_STATE_DIR,
// else $XDG_STATE_HOME/comebye, else ~/.local/state/comebye.
func StateDir(getenv func(string) string) string {
	if d := getenv("COMEBYE_STATE_DIR"); d != "" {
		return expandHome(d, getenv)
	}
	if x := getenv("XDG_STATE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "comebye")
	}
	return expandHome("~/.local/state/comebye", getenv)
}

func expandHome(p string, getenv func(string) string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home := getenv("HOME")
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		if home != "" {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// apiError is a failed API call.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string {
	switch {
	case e.status == http.StatusForbidden && e.code == "agent_forbidden":
		return "not allowed for agents: " + e.message
	case e.message != "":
		return e.message
	case e.code != "":
		return e.code
	}
	return fmt.Sprintf("HTTP %d", e.status)
}

// client talks to the local Comebye as the agent role.
type client struct {
	base      string
	tokenPath string
	token     string
	tokenErr  error
	pane      string
	host      string
	http      *http.Client

	// Cached per run.
	agents  *agentsResp
	whoami  *whoamiResp
	ledgerC *ledgerResp
}

func newClient(e *Env) (*client, error) {
	base := strings.TrimRight(strings.TrimSpace(e.Getenv("COMEBYE_URL")), "/")
	if base == "" {
		base = DefaultURL
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("COMEBYE_URL %q: want http://127.0.0.1:<port>", base)
	}
	dir := StateDir(e.Getenv)
	c := &client{base: base, tokenPath: agenttoken.Path(dir), pane: e.Getenv("HERDR_PANE_ID"), http: e.HTTP}
	c.token, c.tokenErr = agenttoken.Read(dir)
	if h, err := e.Hostname(); err == nil {
		c.host = h
	}
	if c.http == nil {
		c.http = &http.Client{
			Timeout: 200 * time.Second,
			// The local server never redirects; a redirect would carry the
			// token somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &http.Transport{Proxy: nil},
		}
	}
	return c, nil
}

// call makes one API request: body (if any) is sent as JSON, a 2xx JSON
// answer is decoded into out (if any).
func (c *client) call(method, path string, query url.Values, body, out any) error {
	raw, err := c.raw(method, path, query, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unexpected answer from %s: %v", path, err)
	}
	return nil
}

// raw makes the request and returns the 2xx body.
func (c *client) raw(method, path string, query url.Values, body any) ([]byte, error) {
	if c.tokenErr != nil {
		if errors.Is(c.tokenErr, os.ErrNotExist) {
			return nil, fmt.Errorf("no agent token at %s: is Comebye running on this host? It creates the token at startup in its state directory (set COMEBYE_STATE_DIR if it uses another one)", c.tokenPath)
		}
		return nil, fmt.Errorf("agent token: %v", c.tokenErr)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set(headerToken, c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "comebye-cli/1")
	if c.pane != "" {
		req.Header.Set(headerAgent, c.pane)
	}
	if c.host != "" {
		req.Header.Set(headerAgentHost, c.host)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, fmt.Errorf("Comebye at %s did not answer in time", c.base)
		}
		return nil, fmt.Errorf("cannot reach Comebye at %s: is it running? (set COMEBYE_URL if it listens elsewhere)", c.base)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return data, nil
	}
	e := &apiError{status: resp.StatusCode}
	var m struct{ Code, Message string }
	if json.Unmarshal(data, &m) == nil && (m.Code != "" || m.Message != "") {
		e.code, e.message = m.Code, m.Message
	} else {
		e.message = strings.TrimSpace(string(data))
		if len(e.message) > 300 {
			e.message = e.message[:300]
		}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		e.message = "the local Comebye did not accept the agent token (" + c.tokenPath + "); was it created by the server that listens at " + c.base + "?"
	}
	return nil, e
}

// API shapes (the fields the CLI uses).

type worktreeInfo struct {
	Repo     string `json:"repo"`
	Checkout string `json:"checkout"`
	Linked   bool   `json:"linked"`
	Branch   string `json:"branch"`
}

type apiAgent struct {
	ID         string        `json:"id"`
	Host       string        `json:"host"`
	PaneID     string        `json:"pane_id"`
	Name       string        `json:"name"`
	Agent      string        `json:"agent"`
	Status     string        `json:"agent_status"`
	CWD        string        `json:"cwd"`
	Title      string        `json:"terminal_title_stripped"`
	Worktree   *worktreeInfo `json:"worktree"`
	LastActive *time.Time    `json:"last_active"`
}

type apiHost struct {
	Name   string `json:"name"`
	Local  bool   `json:"local"`
	Status string `json:"status"`
	Error  string `json:"error"`
	// Class is the hub's diagnosis while the peer is down: dns, tcp,
	// tls or http. Since is when it last changed status; Addr is the
	// address that last answered when the peer has several.
	Class string     `json:"class"`
	Since *time.Time `json:"since"`
	Addr  string     `json:"addr"`
}

type agentsResp struct {
	Agents []apiAgent `json:"agents"`
	Hosts  []apiHost  `json:"hosts"`
}

type whoamiResp struct {
	Host   string `json:"host"`
	Role   string `json:"role"`
	Agent  string `json:"agent"`
	By     string `json:"by"`
	Hub    string `json:"hub"`
	Ledger bool   `json:"ledger"`
}

type apiMark struct {
	AgentID string    `json:"agent_id"`
	Host    string    `json:"host"`
	Name    string    `json:"name"`
	By      string    `json:"by"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
	Ask     *apiAsk   `json:"ask"`
}

// apiAsk is a marked session's answer about its own removal.
type apiAsk struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

type apiEntry struct {
	ID          string     `json:"id"`
	Host        string     `json:"host"`
	Repo        string     `json:"repo"`
	RepoName    string     `json:"repo_name"`
	Branch      string     `json:"branch"`
	Base        string     `json:"base"`
	Kind        string     `json:"kind"`
	AgentID     string     `json:"agent_id"`
	PaneID      string     `json:"pane_id"`
	AgentName   string     `json:"agent_name"`
	Worktree    string     `json:"worktree"`
	From        string     `json:"from"`
	Status      string     `json:"status"`
	Error       string     `json:"error"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	FinalReport string     `json:"final_report"`
	Mark        *apiMark   `json:"mark"`
	PlanID      string     `json:"plan_id"`
	After       []string   `json:"after"`
	Callback    *struct {
		State string `json:"state"`
		To    string `json:"to"`
	} `json:"callback"`
	Live struct {
		Present bool   `json:"present"`
		Status  string `json:"agent_status"`
	} `json:"live"`
	Candidate bool `json:"candidate"`
	Checks    *struct {
		Dirty    bool   `json:"dirty"`
		Unmerged int    `json:"unmerged_commits"`
		BaseUsed string `json:"base_used"`
	} `json:"checks"`
}

type apiRole struct {
	Role string    `json:"role"`
	Name string    `json:"name"`
	By   string    `json:"by"`
	At   time.Time `json:"at"`
}

type ledgerResp struct {
	Entries []apiEntry         `json:"entries"`
	Marks   []apiMark          `json:"marks"`
	Roles   map[string]apiRole `json:"roles"`
}

func (c *client) getAgents() (*agentsResp, error) {
	if c.agents != nil {
		return c.agents, nil
	}
	var r agentsResp
	if err := c.call("GET", "/api/agents", nil, nil, &r); err != nil {
		return nil, err
	}
	c.agents = &r
	return c.agents, nil
}

func (c *client) getWhoami() (*whoamiResp, error) {
	if c.whoami != nil {
		return c.whoami, nil
	}
	var r whoamiResp
	if err := c.call("GET", "/api/whoami", nil, nil, &r); err != nil {
		return nil, err
	}
	c.whoami = &r
	return c.whoami, nil
}

func (c *client) getLedger() (*ledgerResp, error) {
	if c.ledgerC != nil {
		return c.ledgerC, nil
	}
	var r ledgerResp
	if err := c.call("GET", "/api/ledger", nil, nil, &r); err != nil {
		return nil, err
	}
	c.ledgerC = &r
	return c.ledgerC, nil
}

// target is a resolved agent.
type target struct {
	ID, Host, Pane, Name string
	Live                 *apiAgent
}

func (t target) label() string {
	if t.Name != "" {
		return t.Name + " (" + t.ID + ")"
	}
	return t.ID
}

// resolve turns an agent argument into a target: a host-qualified id
// (host/pane), a bare pane id on the local host, or an agent name
// (unique across hosts).
func (c *client) resolve(arg string) (target, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return target{}, usageErr("an agent is required (host/pane, pane id or name)")
	}
	ar, err := c.getAgents()
	if err != nil {
		return target{}, err
	}
	of := func(a *apiAgent) target {
		return target{ID: a.ID, Host: a.Host, Pane: a.PaneID, Name: a.Name, Live: a}
	}
	for i := range ar.Agents {
		if ar.Agents[i].ID == arg {
			return of(&ar.Agents[i]), nil
		}
	}
	if host, pane, ok := strings.Cut(arg, "/"); ok {
		if host == "" || pane == "" || strings.Contains(pane, "/") {
			return target{}, usageErr("agent %q: a host-qualified id is host/pane, e.g. laharl/w1Q:p1", arg)
		}
		// Not running (e.g. a finished worker that is gone): passed as is.
		return target{ID: arg, Host: host, Pane: pane}, nil
	}
	local := ""
	if w, err := c.getWhoami(); err == nil {
		local = w.Host
	}
	var elsewhere []string
	for i := range ar.Agents {
		a := &ar.Agents[i]
		if a.PaneID == arg {
			if a.Host == local {
				return of(a), nil
			}
			elsewhere = append(elsewhere, a.ID)
		}
	}
	var named []*apiAgent
	for i := range ar.Agents {
		if ar.Agents[i].Name != "" && ar.Agents[i].Name == arg {
			named = append(named, &ar.Agents[i])
		}
	}
	switch len(named) {
	case 1:
		return of(named[0]), nil
	case 0:
	default:
		var ids []string
		for _, a := range named {
			ids = append(ids, a.ID)
		}
		return target{}, fmt.Errorf("agent name %q is ambiguous: it matches %s; use a host-qualified id", arg, strings.Join(ids, ", "))
	}
	if len(elsewhere) > 0 {
		return target{}, fmt.Errorf("no agent %q on this host (%s); did you mean %s?", arg, local, strings.Join(elsewhere, " or "))
	}
	return target{}, fmt.Errorf("no agent named or with pane id %q (see `comebye agents`)", arg)
}

// hostPath is a per-agent API path on the agent's host.
func hostPath(t target, op string) string {
	return "/api/hosts/" + url.PathEscape(t.Host) + "/agents/" + url.PathEscape(t.Pane) + "/" + op
}

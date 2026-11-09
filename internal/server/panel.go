package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
)

// The side panels (Diff and Doc) beside an agent's chat.
//
// The browser (or the agent itself, for its own) asks the hub:
//
//	GET /api/[hosts/H/]agents/T/diff   per-file diffs of the agent's checkout
//	GET /api/[hosts/H/]agents/T/file   its pinned document, a file it names,
//	                                   or (?list=1) the documents it may pick
//	POST /api/show                     `comebye show`: declare / clear the doc
//
// The hub finds the agent's worktree and its declared document, then does the
// read on the agent's host: locally, or by fleet.CallHost for a peer
// (GET /api/panel/diff and /file there, which only the hub calls). The host
// judges the paths; see panelfile.go.

// panelTarget finds the agent a panel request is about, and refuses an agent
// asking about another one: the user reads any agent's panels, an agent only
// its own.
func (s *Server) panelTarget(w http.ResponseWriter, r *http.Request) (herdr.AgentInfo, string, bool) {
	host, target := s.hostOf(r), r.PathValue("target")
	for _, a := range s.fleet.Agents() {
		if !(sameHost(a.Host, host) || strings.HasPrefix(a.ID, host+"/")) || (a.PaneID != target && (a.Name == "" || a.Name != target)) {
			continue
		}
		if c := callerOf(r); c.agent() && (c.agentRef() == "" || c.agentRef() != a.ID) {
			writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "not_yours", "message": "an agent reads the panels of its own session only"})
			return a, "", false
		}
		return a, host, true
	}
	writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "unknown_agent", "message": "no such agent"})
	return herdr.AgentInfo{}, "", false
}

// agentDir is where an agent works: its worktree, else its directory.
func agentDir(a herdr.AgentInfo) string {
	if a.Worktree != nil && a.Worktree.CheckoutPath != "" {
		return a.Worktree.CheckoutPath
	}
	return a.CWD
}

// panelCall runs op ("diff" or "file") for the agent on its host.
func (s *Server) panelCall(ctx context.Context, host, op string, q url.Values) (int, map[string]any) {
	if sameHost(host, s.fleet.LocalName()) {
		return s.panelRun(ctx, op, q)
	}
	status, body, err := s.fleet.CallHost(ctx, host, "panel-"+op, q, nil)
	if err != nil {
		return http.StatusBadGateway, map[string]any{"code": "host_unreachable", "message": err.Error()}
	}
	var out map[string]any
	if json.Unmarshal(body, &out) != nil || out == nil {
		return http.StatusBadGateway, map[string]any{"code": "bad_answer", "message": host + " sent an answer this app does not understand"}
	}
	return status, out
}

func (s *Server) handlePanelDiff(w http.ResponseWriter, r *http.Request) {
	a, host, ok := s.panelTarget(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	q := url.Values{"dir": {agentDir(a)}}
	if v := r.URL.Query().Get("since"); v != "" {
		q.Set("since", v)
	}
	status, out := s.panelCall(ctx, host, "diff", q)
	writeJSONStatus(w, status, out)
}

func (s *Server) handlePanelFile(w http.ResponseWriter, r *http.Request) {
	a, host, ok := s.panelTarget(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	in := r.URL.Query()
	pin, hasPin := s.pins.get(a.ID)
	q := url.Values{"dir": {agentDir(a)}}
	if hasPin {
		q.Set("pin", pin.Path)
	}
	for _, k := range []string{"path", "list", "since"} {
		if v := in.Get(k); v != "" {
			q.Set(k, v)
		}
	}
	// No path: the declared document, if there is one.
	if q.Get("path") == "" && q.Get("list") == "" {
		if !hasPin {
			writeJSON(w, map[string]any{"pin": nil})
			return
		}
		q.Set("path", pin.Path)
		if pin.Section != "" {
			q.Set("section", pin.Section)
		}
	}
	status, out := s.panelCall(ctx, host, "file", q)
	if status == http.StatusOK {
		if hasPin {
			out["pin"] = pin
		} else {
			out["pin"] = nil
		}
	}
	writeJSONStatus(w, status, out)
}

// handlePanelHost is the host side (GET /api/panel/{diff,file}): the hub's
// call only, never an agent's or a browser's.
func (s *Server) handlePanelHost(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(fleet.HeaderHubCall) != "1" {
			writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "hub_only", "message": "this route is for the hub reading a peer's panel"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer cancel()
		status, out := s.panelRun(ctx, op, r.URL.Query())
		writeJSONStatus(w, status, out)
	}
}

// panelRun does the read, on this host. dir is the agent's worktree (the hub
// says where; it must be inside the home directory or the launch roots), pin
// the document the agent declared.
func (s *Server) panelRun(ctx context.Context, op string, q url.Values) (int, map[string]any) {
	fail := func(e *fileErr) (int, map[string]any) {
		return e.status, map[string]any{"code": e.code, "message": e.message}
	}
	dir := q.Get("dir")
	switch op {
	case "diff":
		d, ok := realDir(dir)
		if !ok || !(withinHome(d) || withinAny(d, s.checkRoots())) {
			return fail(ferr(http.StatusNotFound, "no_dir", "this agent's directory is not readable here"))
		}
		return http.StatusOK, toMap(panelDiffOf(ctx, d, q.Get("since")))
	case "file":
		pr, e := newPanelRoots(dir, q.Get("pin"))
		if e != nil {
			return fail(e)
		}
		if q.Get("list") != "" {
			return http.StatusOK, map[string]any{"files": pr.listPanelDocs(ctx), "dir": pr.dir}
		}
		doc, e := pr.readPanelDoc(q.Get("path"), q.Get("since"), q.Get("section"), q.Get("meta") != "")
		if e != nil {
			return fail(e)
		}
		rel := ""
		if pr.dir != "" && within(doc.Path, pr.dir) {
			rel, _ = filepath.Rel(pr.dir, doc.Path)
		}
		return http.StatusOK, map[string]any{"doc": toMap(doc), "rel": filepath.ToSlash(rel), "dir": pr.dir}
	}
	return fail(ferr(http.StatusNotFound, "not_found", "no such panel"))
}

func withinHome(p string) bool {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return false
	}
	hr, ok := realDir(h)
	return ok && within(p, hr)
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// showBody is `comebye show`.
type showBody struct {
	AgentID string `json:"agent_id"`
	Path    string `json:"path"`
	Section string `json:"section"`
	Clear   bool   `json:"clear"`
}

// handleShow records (or clears) the document an agent wants the user to
// read beside its chat. An agent declares its own; the user or a
// coordinator, anyone's.
func (s *Server) handleShow(w http.ResponseWriter, r *http.Request) {
	var b showBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil || b.AgentID == "" {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	c := callerOf(r)
	if c.agent() && b.AgentID != c.agentRef() && !s.callerIsCoordinator(c) {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "not_yours", "message": "an agent declares its own document; only the user or a coordinator sets another's"})
		return
	}
	var agent *herdr.AgentInfo
	for _, a := range s.fleet.Agents() {
		if a.ID == b.AgentID {
			a := a
			agent = &a
			break
		}
	}
	if agent == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"code": "unknown_agent", "message": "no such agent"})
		return
	}
	if b.Clear {
		if err := s.pins.set(b.AgentID, docPin{}); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "store_error", "message": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "cleared": true})
		return
	}
	section := oneLine(b.Section, 200)
	// The host judges the file now, so the agent hears at once that the
	// panel will not open it.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	host := agent.Host
	if host == "" {
		host, _, _ = strings.Cut(agent.ID, "/")
	}
	q := url.Values{"dir": {agentDir(*agent)}, "pin": {b.Path}, "path": {b.Path}, "meta": {"1"}}
	if section != "" {
		q.Set("section", section)
	}
	status, out := s.panelCall(ctx, host, "file", q)
	if status != http.StatusOK {
		writeJSONStatus(w, status, out)
		return
	}
	doc, _ := out["doc"].(map[string]any)
	real, _ := doc["path"].(string)
	if real == "" {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"code": "bad_answer", "message": "the host did not name the file"})
		return
	}
	if err := s.pins.set(b.AgentID, docPin{Path: real, Section: section, At: time.Now(), By: s.by(r, "")}); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"code": "store_error", "message": err.Error()})
		return
	}
	res := map[string]any{"ok": true, "path": real, "section": section}
	if f, ok := doc["section_found"].(bool); ok && !f {
		res["warning"] = "no heading matches \"" + section + "\" in the file: the panel opens it at the top"
	}
	writeJSON(w, res)
}

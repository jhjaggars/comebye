// A stateful stand-in for herdr's socket API, for the day-in-the-life smoke (tools/smoke). It keeps workspaces, panes
// and agents in memory, makes real git worktrees (so the hub's git checks and a worker's landing run against real
// repositories), starts "agents" that are nothing but a record, and keeps every prompt it receives so the driver can
// play the agent. tools/devserver is the other fake: fixed agents for the browser checks. This one lets a hub start,
// park, resume and restart workers.
//
// It never touches a real herdr or the user's herdr config: it only listens on the socket it is given (a path under
// the run's temp directory). The driver talks to it with `smoke.*` methods (see answer).
//
//	fakeherdr -sock PATH -home DIR
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type agent struct {
	Name      string `json:"name"`
	PaneID    string `json:"pane_id"`
	Workspace string `json:"workspace_id"`
	TabID     string `json:"tab_id"`
	Agent     string `json:"agent"`
	Display   string `json:"display_agent"`
	Status    string `json:"agent_status"`
	Title     string `json:"terminal_title"`
	CWD       string `json:"cwd"`
	Session   any    `json:"agent_session,omitempty"`
	Ready     bool   `json:"interactive_ready"`
	Revision  uint64 `json:"revision"`

	argv    []string
	screen  []string
	prompts []string // everything typed to the agent with agent.prompt, in order
	keys    []string
}

type pane struct {
	ID, Workspace, CWD string
	agent              *agent
}

type workspace struct {
	ID, Label, CWD string
	root           string // the repository's primary checkout ("" outside a repository)
	linked         bool
	panes          []*pane
}

type fake struct {
	mu         sync.Mutex
	home       string
	workspaces []*workspace
	nextWS     int
	nextPane   map[string]int
	subs       []net.Conn
}

func main() {
	sock := flag.String("sock", "", "unix socket to listen on")
	home := flag.String("home", "", "directory for session files (claude's and pi's)")
	flag.Parse()
	if *sock == "" || *home == "" {
		log.Fatal("fakeherdr: -sock and -home are required")
	}
	f := &fake{home: *home, nextPane: map[string]int{}}
	_ = os.Remove(*sock)
	l, err := net.Listen("unix", *sock)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("smoke fake herdr listening on", *sock)
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go f.serve(c)
	}
}

func (f *fake) serve(c net.Conn) {
	r := bufio.NewReader(c)
	line, err := r.ReadBytes('\n')
	if err != nil {
		c.Close()
		return
	}
	var req struct {
		ID     string         `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if json.Unmarshal(line, &req) != nil {
		c.Close()
		return
	}
	if req.Method == "events.subscribe" {
		f.write(c, map[string]any{"id": req.ID, "result": map[string]any{"type": "subscription_started"}})
		f.mu.Lock()
		f.subs = append(f.subs, c)
		f.mu.Unlock()
		return
	}
	defer c.Close()
	res, e := f.answer(req.Method, req.Params)
	if e != nil {
		f.write(c, map[string]any{"id": req.ID, "error": e})
		return
	}
	f.write(c, map[string]any{"id": req.ID, "result": res})
}

func (f *fake) write(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	c.Write(append(b, '\n'))
}

type rpcErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func str(p map[string]any, k string) string { s, _ := p[k].(string); return s }

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (f *fake) pane(id string) *pane {
	for _, w := range f.workspaces {
		for _, p := range w.panes {
			if p.ID == id {
				return p
			}
		}
	}
	return nil
}

func (f *fake) agentOf(target string) *agent {
	for _, w := range f.workspaces {
		for _, p := range w.panes {
			if a := p.agent; a != nil && (a.Name == target || a.PaneID == target) {
				return a
			}
		}
	}
	return nil
}

func (f *fake) agents() []*agent {
	out := []*agent{}
	for _, w := range f.workspaces {
		for _, p := range w.panes {
			if p.agent != nil {
				out = append(out, p.agent)
			}
		}
	}
	return out
}

// exit ends the agent's process: its pane stays, at the shell.
func (f *fake) exit(a *agent) {
	for _, w := range f.workspaces {
		for _, p := range w.panes {
			if p.agent == a {
				p.agent = nil
			}
		}
	}
}

func (f *fake) newWorkspace(cwd, label string) *workspace {
	f.nextWS++
	w := &workspace{ID: fmt.Sprintf("w%d", f.nextWS), Label: label, CWD: cwd}
	if top, err := git(cwd, "rev-parse", "--show-toplevel"); err == nil {
		w.root = top
		if common, err := git(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
			w.root = filepath.Dir(common)
			w.linked = top != w.root
		}
	}
	f.addPane(w)
	f.workspaces = append(f.workspaces, w)
	return w
}

func (f *fake) addPane(w *workspace) *pane {
	f.nextPane[w.ID]++
	p := &pane{ID: fmt.Sprintf("%s:p%d", w.ID, f.nextPane[w.ID]), Workspace: w.ID, CWD: w.CWD}
	w.panes = append(w.panes, p)
	return p
}

func wsJSON(w *workspace) map[string]any {
	m := map[string]any{"workspace_id": w.ID, "label": w.Label, "pane_count": len(w.panes), "worktree": nil}
	if w.root != "" {
		m["worktree"] = map[string]any{"repo_name": filepath.Base(w.root), "repo_root": w.root, "checkout_path": w.CWD, "is_linked_worktree": w.linked}
	}
	return m
}

func (f *fake) opened(w *workspace, branch string, already bool) map[string]any {
	return map[string]any{"type": "worktree_opened", "already_open": already,
		"workspace": map[string]any{"workspace_id": w.ID, "label": w.Label},
		"root_pane": map[string]any{"pane_id": w.panes[0].ID},
		"worktree":  map[string]any{"path": w.CWD, "branch": branch}}
}

// sessionFor makes the session an agent of kind keeps: a Claude session id and its file under home, or a Pi
// session file path. The hub reads these to park and resume.
func (f *fake) sessionFor(kind, cwd string, n int) (any, error) {
	id := fmt.Sprintf("5a0c0de0-0000-4000-8000-%012d", n)
	switch kind {
	case "claude":
		dir := filepath.Join(f.home, ".claude", "projects", strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(cwd))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		p := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(p, []byte(`{"type":"user","uuid":"u1","timestamp":"`+time.Now().UTC().Format(time.RFC3339Nano)+`","message":{"role":"user","content":"hello"}}`+"\n"), 0o644); err != nil {
			return nil, err
		}
		return map[string]string{"source": "smoke", "agent": "claude", "kind": "id", "value": id}, nil
	case "pi":
		dir := filepath.Join(f.home, ".pi", "agent", "sessions", "smoke")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		p := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(p, []byte(`{"type":"session","id":"`+id+`"}`+"\n"), 0o644); err != nil {
			return nil, err
		}
		return map[string]string{"source": "smoke", "agent": "pi", "kind": "path", "value": p}, nil
	}
	return nil, nil
}

func (f *fake) emit(ev map[string]any) {
	b, _ := json.Marshal(ev)
	for _, c := range f.subs {
		c.Write(append(b, '\n'))
	}
}

func (f *fake) statusEvent(a *agent) {
	f.emit(map[string]any{"event": "pane.agent_status_changed", "data": map[string]any{
		"pane_id": a.PaneID, "workspace_id": a.Workspace, "agent_status": a.Status, "agent": a.Agent, "display_agent": a.Display, "title": a.Title}})
}

func (f *fake) answer(method string, p map[string]any) (any, *rpcErr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := str(p, "target")
	if target == "" {
		target = str(p, "pane_id")
	}
	switch method {
	case "agent.list":
		return map[string]any{"type": "agent_list", "agents": f.agents()}, nil
	case "agent.get":
		if a := f.agentOf(target); a != nil {
			return map[string]any{"type": "agent_info", "agent": a}, nil
		}
		return nil, &rpcErr{"agent_not_found", target}
	case "session.snapshot":
		ws := []any{}
		for _, w := range f.workspaces {
			ws = append(ws, wsJSON(w))
		}
		return map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"workspaces": ws}}, nil
	case "workspace.list":
		ws := []any{}
		for _, w := range f.workspaces {
			ws = append(ws, wsJSON(w))
		}
		return map[string]any{"type": "workspace_list", "workspaces": ws}, nil
	case "workspace.create":
		cwd := str(p, "cwd")
		if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
			return nil, &rpcErr{"invalid_cwd", cwd + " is not a directory"}
		}
		w := f.newWorkspace(cwd, str(p, "label"))
		return map[string]any{"type": "workspace_created", "workspace": map[string]any{"workspace_id": w.ID, "label": w.Label},
			"root_pane": map[string]any{"pane_id": w.panes[0].ID}}, nil
	case "workspace.close":
		for i, w := range f.workspaces {
			if w.ID == str(p, "workspace_id") {
				f.workspaces = append(f.workspaces[:i:i], f.workspaces[i+1:]...)
				return map[string]any{"type": "ok"}, nil
			}
		}
		return nil, &rpcErr{"workspace_not_found", str(p, "workspace_id")}
	case "pane.list":
		out := []any{}
		for _, w := range f.workspaces {
			if id := str(p, "workspace_id"); id != "" && id != w.ID {
				continue
			}
			for _, pn := range w.panes {
				name := ""
				if pn.agent != nil {
					name = pn.agent.Agent
				}
				out = append(out, map[string]any{"pane_id": pn.ID, "workspace_id": w.ID, "agent": name, "cwd": pn.CWD})
			}
		}
		return map[string]any{"type": "pane_list", "panes": out}, nil
	case "pane.close":
		for _, w := range f.workspaces {
			for i, pn := range w.panes {
				if pn.ID == str(p, "pane_id") {
					w.panes = append(w.panes[:i:i], w.panes[i+1:]...)
					if len(w.panes) == 0 {
						for j, x := range f.workspaces {
							if x == w {
								f.workspaces = append(f.workspaces[:j:j], f.workspaces[j+1:]...)
								break
							}
						}
					}
					return map[string]any{"type": "ok"}, nil
				}
			}
		}
		return nil, &rpcErr{"pane_not_found", str(p, "pane_id")}
	case "pane.process_info":
		pn := f.pane(str(p, "pane_id"))
		if pn == nil {
			return nil, &rpcErr{"pane_not_found", str(p, "pane_id")}
		}
		procs := []any{map[string]any{"argv": []string{"sh"}, "cmdline": "sh", "cwd": pn.CWD, "name": "sh", "pid": 4000}}
		group := 4000
		if a := pn.agent; a != nil {
			procs = []any{map[string]any{"argv": a.argv, "cmdline": strings.Join(a.argv, " "), "cwd": a.CWD, "name": a.Agent, "pid": 4242}}
			group = 4242
		}
		return map[string]any{"type": "pane_process_info", "process_info": map[string]any{
			"pane_id": pn.ID, "foreground_process_group_id": group, "shell_pid": 4000, "foreground_processes": procs}}, nil
	case "pane.read":
		a := f.agentOf(target)
		if a == nil {
			return nil, &rpcErr{"pane_not_found", target}
		}
		return map[string]any{"type": "pane_read", "read": map[string]any{
			"text": strings.Join(a.screen, "\n"), "revision": a.Revision, "format": "plain", "source": p["source"]}}, nil
	case "worktree.list":
		return f.worktreeList(str(p, "cwd"))
	case "worktree.create", "worktree.open":
		return f.worktreeOpen(method == "worktree.create", p)
	case "agent.start":
		pn := f.pane(str(p, "pane_id"))
		if pn == nil {
			return nil, &rpcErr{"pane_not_found", str(p, "pane_id")}
		}
		if pn.agent != nil {
			return nil, &rpcErr{"pane_busy", "pane already runs an agent"}
		}
		name, kind := str(p, "name"), str(p, "kind")
		if name != "" && f.agentOf(name) != nil {
			return nil, &rpcErr{"agent_name_taken", name}
		}
		args := []string{kind}
		if xs, ok := p["args"].([]any); ok {
			for _, x := range xs {
				if s, ok := x.(string); ok {
					args = append(args, s)
				}
			}
		}
		// A resume brings its session back: --resume ID (claude), --session PATH (pi).
		var sess any
		for i, a := range args {
			if (a == "--resume" || a == "--session") && i+1 < len(args) {
				v := args[i+1]
				if kind == "pi" {
					sess = map[string]string{"source": "smoke", "agent": "pi", "kind": "path", "value": v}
				} else {
					sess = map[string]string{"source": "smoke", "agent": kind, "kind": "id", "value": v}
				}
			}
		}
		if sess == nil {
			var err error
			if sess, err = f.sessionFor(kind, pn.CWD, len(f.agents())+1); err != nil {
				return nil, &rpcErr{"session_failed", err.Error()}
			}
		}
		a := &agent{Name: name, PaneID: pn.ID, Workspace: pn.Workspace, TabID: pn.Workspace + ":t1", Agent: kind, Display: strings.ToUpper(kind[:1]) + kind[1:],
			Status: "idle", Title: name, CWD: pn.CWD, Session: sess, Ready: true, Revision: 1, argv: args, screen: []string{name + " ready"}}
		pn.agent = a
		f.statusEvent(a)
		return map[string]any{"type": "agent_started", "agent": a}, nil
	case "agent.send_keys":
		a := f.agentOf(target)
		if a == nil {
			return nil, &rpcErr{"agent_not_found", target}
		}
		ks, _ := p["keys"].([]any)
		for _, k := range ks {
			if fmt.Sprint(k) == "ctrl+d" { // pi leaves on Ctrl+D at an empty editor
				f.exit(a)
				return map[string]any{"type": "ok"}, nil
			}
			a.keys = append(a.keys, fmt.Sprint(k))
			a.screen = append(a.screen, fmt.Sprintf("[keys] %v", k))
		}
		a.Revision++
		return map[string]any{"type": "ok"}, nil
	case "agent.prompt":
		a := f.agentOf(target)
		if a == nil {
			return nil, &rpcErr{"agent_not_found", target}
		}
		if a.Status == "blocked" {
			return nil, &rpcErr{"agent_blocked", "agent is waiting for input"}
		}
		text := str(p, "text")
		if text == "/exit" { // the agent's own exit (what a restart types): the pane is back at its shell
			f.exit(a)
			return map[string]any{"type": "agent_prompted", "agent": a}, nil
		}
		a.prompts = append(a.prompts, text)
		a.screen = append(a.screen, "> "+text)
		a.Revision++
		// A real agent starts a turn on a prompt, and the hub checks that it did. The turn lasts until the driver ends it
		// (smoke.status): the stand-in's work takes however long the script takes.
		if a.Status == "idle" || a.Status == "done" {
			a.Status = "working"
			a.Revision++
			f.statusEvent(a)
		}
		return map[string]any{"type": "agent_prompted", "agent": a}, nil

	// The driver's own methods: it plays the agents.
	case "smoke.prompts": // {target} -> every prompt the agent was typed, in order
		a := f.agentOf(target)
		if a == nil {
			return nil, &rpcErr{"agent_not_found", target}
		}
		return map[string]any{"prompts": a.prompts, "keys": a.keys}, nil
	case "smoke.screen": // {target, text}: what the agent's pane shows (a half-typed input box, say); no event, as herdr sends none for a keystroke
		a := f.agentOf(target)
		if a == nil {
			return nil, &rpcErr{"agent_not_found", target}
		}
		a.screen = strings.Split(str(p, "text"), "\n")
		a.Revision++
		return map[string]any{"type": "ok"}, nil
	case "smoke.status": // {target, status}: the agent's status as herdr would report it
		a := f.agentOf(target)
		if a == nil {
			return nil, &rpcErr{"agent_not_found", target}
		}
		a.Status = str(p, "status")
		a.Revision++
		f.statusEvent(a)
		return map[string]any{"type": "ok"}, nil
	case "smoke.add_agent": // {name, kind, cwd}: an agent that already runs (the coordinator)
		cwd := str(p, "cwd")
		w := f.newWorkspace(cwd, str(p, "name"))
		sess, _ := f.sessionFor(str(p, "kind"), cwd, len(f.agents())+1)
		kind := str(p, "kind")
		a := &agent{Name: str(p, "name"), PaneID: w.panes[0].ID, Workspace: w.ID, TabID: w.ID + ":t1", Agent: kind, Display: strings.ToUpper(kind[:1]) + kind[1:],
			Status: "idle", Title: str(p, "name"), CWD: cwd, Session: sess, Ready: true, Revision: 1, argv: []string{kind}, screen: []string{str(p, "name") + " ready"}}
		w.panes[0].agent = a
		f.statusEvent(a)
		return map[string]any{"agent": a}, nil
	}
	return nil, &rpcErr{"unknown_method", method}
}

func (f *fake) worktreeList(cwd string) (any, *rpcErr) {
	out, err := git(cwd, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, &rpcErr{"not_a_repo", out}
	}
	var wts []any
	var cur map[string]any
	first := true
	flush := func() {
		if cur != nil {
			if p, _ := cur["path"].(string); p != "" {
				cur["is_linked_worktree"] = !first
				cur["label"] = filepath.Base(p)
				cur["open_workspace_id"] = ""
				for _, w := range f.workspaces {
					if w.CWD == p {
						cur["open_workspace_id"] = w.ID
					}
				}
				wts = append(wts, cur)
				first = false
			}
		}
		cur = map[string]any{}
	}
	cur = map[string]any{}
	for l := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(l, "worktree "):
			flush()
			cur["path"] = strings.TrimPrefix(l, "worktree ")
		case strings.HasPrefix(l, "branch "):
			cur["branch"] = strings.TrimPrefix(l, "branch ")
		case l == "detached":
			cur["is_detached"] = true
		case l == "bare":
			cur["is_bare"] = true
		}
	}
	flush()
	if wts == nil {
		wts = []any{}
	}
	return map[string]any{"type": "worktree_list", "worktrees": wts}, nil
}

func (f *fake) worktreeOpen(create bool, p map[string]any) (any, *rpcErr) {
	cwd, path, branch := str(p, "cwd"), str(p, "path"), str(p, "branch")
	if create {
		args := []string{"worktree", "add"}
		if base := str(p, "base"); base != "" {
			args = append(args, "-b", branch, path, base)
		} else {
			args = append(args, path, branch)
		}
		if out, err := git(cwd, args...); err != nil {
			return nil, &rpcErr{"worktree_failed", out}
		}
	} else if branch == "" {
		if b, err := git(path, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			branch = b
		}
	}
	for _, w := range f.workspaces {
		if w.CWD == path {
			return f.opened(w, branch, true), nil
		}
	}
	w := f.newWorkspace(path, str(p, "label"))
	return f.opened(w, branch, false), nil
}

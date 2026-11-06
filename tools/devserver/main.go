// Command devserver is a FAKE herdr for driving the Comebye web app in a
// real browser (see tools/uitest and CONTRIBUTING.md). It is dev/test only:
// it answers the slice of herdr's socket API that the app uses, on a unix
// socket, so the real comebye binary can run against it:
//
//	go run ./tools/devserver -sock /tmp/fake-herdr.sock -dir /tmp/fake-herdr
//	comebye -sock /tmp/fake-herdr.sock -insecure-no-auth -push=false ...
//
// It has no imports from the rest of the module, so it builds and runs
// unchanged next to any older checkout of the app.
//
// Two agents: "alpha" (a Claude agent with a transcript file: the chat
// view) and "bravo" (a codex agent with no session: the raw terminal
// fallback). Prompting an agent appends the prompt to its transcript/screen,
// flips it working after -status-lag, and after -reply-delay appends a reply
// and flips it idle again. Both flips are pushed to event subscribers.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
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

	repo       string   // fixture agents: the workspace's repository ("" for none)
	checkout   string   // ... and its checkout path
	linked     bool     // ... and whether that is a linked worktree
	trust      bool     // shows the trust prompt until an Enter arrives
	screen     []string // pane text (bravo; alpha mirrors its transcript here)
	transcript string   // JSONL path ("" for none)
	n          int
}

type fake struct {
	mu         sync.Mutex
	agents     []*agent
	subs       []net.Conn
	statusLag  time.Duration
	replyDelay time.Duration
	noStatus   bool
}

func main() {
	sock := flag.String("sock", "/tmp/fake-herdr.sock", "unix socket to listen on")
	dir := flag.String("dir", "", "directory for transcript files (default: a temp dir)")
	lag := flag.Duration("status-lag", 300*time.Millisecond, "delay between a prompt and the working status")
	reply := flag.Duration("reply-delay", 2500*time.Millisecond, "delay between a prompt and the reply (then idle)")
	noStatus := flag.Bool("no-status", false, "never report working/idle: the status stays idle, as when herdr misses or delays a transition")
	fixtures := flag.Bool("fixtures", false, "add the dev-harness fixture agents (worker, coordinator, home, blocked, long names, ...) after alpha and bravo")
	hostTag := flag.String("fixture-host", "", "with -fixtures: a tag that makes the fixture agents a second host's (a peer): a few agents named <tag>-*")
	trust := flag.Bool("trust-prompt", false, "add a Pi agent that herdr reports as idle while its screen shows the \"Trust project folder?\" prompt (any Enter answers it)")
	recallFixture := flag.String("recall-fixture", "", "write a synthetic transcript index to this file and exit")
	flag.Parse()
	if *recallFixture != "" {
		if err := writeRecallFixture(*recallFixture); err != nil {
			log.Fatalf("recall fixture: %v", err)
		}
		return
	}
	if *dir == "" {
		d, err := os.MkdirTemp("", "fake-herdr")
		if err != nil {
			log.Fatal(err)
		}
		*dir = d
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatal(err)
	}
	f := &fake{statusLag: *lag, replyDelay: *reply, noStatus: *noStatus}
	alphaFile := filepath.Join(*dir, "alpha.jsonl")
	if err := os.WriteFile(alphaFile, nil, 0o644); err != nil {
		log.Fatal(err)
	}
	f.agents = []*agent{
		{Name: "alpha", PaneID: "w1:p1", Workspace: "w1", TabID: "w1:t1", Agent: "claude", Display: "Claude",
			Status: "idle", Title: "alpha", CWD: "/tmp/alpha", Ready: true, transcript: alphaFile,
			Session: map[string]string{"source": "fake", "agent": "claude", "kind": "path", "value": alphaFile},
			screen:  []string{"alpha ready"}},
		{Name: "bravo", PaneID: "w2:p1", Workspace: "w2", TabID: "w2:t1", Agent: "codex", Display: "Codex",
			Status: "idle", Title: "bravo", CWD: "/tmp/bravo", Ready: true,
			screen: []string{"bravo ready"}},
	}
	f.record(f.agents[0], "user", "hello alpha")
	f.record(f.agents[0], "assistant", "alpha says hi")
	if *fixtures {
		f.addFixtures(*dir, *hostTag)
	}
	if *trust && !*fixtures {
		f.addTrustPrompt()
	}
	_ = os.Remove(*sock)
	l, err := net.Listen("unix", *sock)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("fake herdr listening on", *sock, "transcripts in", *dir)
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
		return // stays open: a push stream
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

func (f *fake) find(target string) *agent {
	for _, a := range f.agents {
		if a.Name == target || a.PaneID == target {
			return a
		}
	}
	return nil
}

func (f *fake) answer(method string, p map[string]any) (any, *rpcErr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target, _ := p["target"].(string)
	if target == "" {
		target, _ = p["pane_id"].(string)
	}
	switch method {
	case "agent.list":
		return map[string]any{"type": "agent_list", "agents": f.agents}, nil
	case "session.snapshot":
		return map[string]any{"type": "session_snapshot", "snapshot": map[string]any{"workspaces": f.workspaces()}}, nil
	case "agent.get":
		if a := f.find(target); a != nil {
			return map[string]any{"type": "agent_info", "agent": a}, nil
		}
		return nil, &rpcErr{"agent_not_found", target}
	case "pane.read":
		a := f.find(target)
		if a == nil {
			return nil, &rpcErr{"pane_not_found", target}
		}
		return map[string]any{"type": "pane_read", "read": map[string]any{
			"text": strings.Join(a.screen, "\n"), "revision": a.Revision, "format": "plain", "source": p["source"]}}, nil
	case "agent.send_keys":
		a := f.find(target)
		if a == nil {
			return nil, &rpcErr{"agent_not_found", target}
		}
		ks, _ := p["keys"].([]any)
		if a.trust && slices.Contains(ks, any("enter")) {
			a.screen = []string{"pi ready"} // the prompt is answered
		}
		// Recorded on the screen, so a check can see which keys arrived.
		for _, k := range ks {
			f.record(a, "assistant", fmt.Sprintf("[keys] %v", k))
		}

		return map[string]any{"type": "ok"}, nil
	case "agent.prompt":
		a := f.find(target)
		if a == nil {
			return nil, &rpcErr{"agent_not_found", target}
		}
		if a.Status == "blocked" {
			return nil, &rpcErr{"agent_blocked", "agent is waiting for input"}
		}
		text, _ := p["text"].(string)
		f.record(a, "user", text)
		go f.respond(a, text)
		return map[string]any{"type": "agent_prompted", "agent": a}, nil
	}
	return nil, &rpcErr{"unknown_method", method}
}

// respond plays the agent's side of a prompt: working, then a reply, then idle.
func (f *fake) respond(a *agent, text string) {
	time.Sleep(f.statusLag)
	f.setStatus(a, "working")
	time.Sleep(f.replyDelay - f.statusLag)
	f.mu.Lock()
	f.record(a, "assistant", "reply to: "+text)
	f.mu.Unlock()
	f.setStatus(a, "idle")
}

func (f *fake) setStatus(a *agent, st string) {
	if f.noStatus {
		return
	}
	f.mu.Lock()
	a.Status = st
	a.Revision++
	ev, _ := json.Marshal(map[string]any{"event": "pane.agent_status_changed", "data": map[string]any{
		"pane_id": a.PaneID, "workspace_id": a.Workspace, "agent_status": st, "agent": a.Agent, "display_agent": a.Display, "title": a.Title}})
	subs := append([]net.Conn(nil), f.subs...)
	f.mu.Unlock()
	for _, c := range subs {
		c.Write(append(ev, '\n'))
	}
}

// record appends one line to the agent's screen and, if it has one, its
// Claude-format transcript. Callers hold f.mu (or are single-threaded).
func (f *fake) record(a *agent, role, text string) {
	a.n++
	a.Revision++
	if role == "user" {
		a.screen = append(a.screen, "> "+text)
	} else {
		a.screen = append(a.screen, text)
	}
	if a.transcript == "" {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	var rec map[string]any
	if role == "user" {
		rec = map[string]any{"type": "user", "uuid": fmt.Sprintf("u%d", a.n), "timestamp": ts,
			"message": map[string]any{"role": "user", "content": text}}
	} else {
		rec = map[string]any{"type": "assistant", "uuid": fmt.Sprintf("a%d", a.n), "timestamp": ts,
			"message": map[string]any{"id": fmt.Sprintf("msg_%d", a.n), "model": "fake",
				"content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	b, _ := json.Marshal(rec)
	fh, err := os.OpenFile(a.transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Print(err)
		return
	}
	defer fh.Close()
	fh.Write(append(b, '\n'))
}

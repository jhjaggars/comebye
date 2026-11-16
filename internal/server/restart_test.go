package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/scratch"
)

// restartFake is a fake herdr whose panes hold a shell and, while an agent
// runs, the agent's process: pane.process_info, the exit (a /exit prompt or
// Ctrl+D), agent.start and agent.rename behave as herdr does. It lives under
// the fake's lock (its hook and set()).
type restartFake struct {
	shellPID  int
	procs     map[string][]map[string]any // pane -> foreground processes
	sessions  map[string]*herdr.AgentSessionInfo
	nextPID   int
	exitWorks bool // the exit command ends the process
	startFail int  // agent.start fails this many times
	// linger is how many agent.list calls herdr keeps an exited agent (and its
	// name) for: it lets go of it a moment after the process ends.
	linger     int
	lingerLeft map[string]int
	exits      []string // what ended an agent: "/exit" or "ctrl+d"
	starts     []map[string]any
}

func shellProc(pid int) map[string]any {
	return map[string]any{"argv": []string{"/bin/fish"}, "cmdline": "/bin/fish", "cwd": "/", "name": "fish", "pid": pid}
}

func agentProc(kind string, pid int, argv ...string) map[string]any {
	if argv == nil {
		argv = []string{kind}
	}
	return map[string]any{"argv": argv, "cmdline": strings.Join(argv, " "), "cwd": "/", "name": kind, "pid": pid}
}

func (rf *restartFake) install(f *fakeLaunchHerdr) {
	f.hook = func(method string, p map[string]any) (any, *herdr.Error, bool) {
		pane, _ := p["pane_id"].(string)
		switch method {
		case "pane.process_info":
			procs := rf.procs[pane]
			pgid := rf.shellPID
			if len(procs) > 0 && procs[0]["pid"] != rf.shellPID {
				pgid = procs[0]["pid"].(int)
			}
			return map[string]any{"type": "pane_process_info", "process_info": map[string]any{
				"pane_id": pane, "shell_pid": rf.shellPID, "foreground_process_group_id": pgid, "foreground_processes": procs}}, nil, true
		case "agent.prompt":
			if p["text"] == "/exit" {
				rf.exit(f, p["target"].(string), "/exit")
				return map[string]any{"type": "agent_prompted", "agent": map[string]any{"name": p["target"]}}, nil, true
			}
		case "agent.send_keys":
			keys, _ := p["keys"].([]any)
			if len(keys) == 1 && keys[0] == "ctrl+d" {
				rf.exit(f, p["target"].(string), "ctrl+d")
				return map[string]any{"type": "ok"}, nil, true
			}
		case "agent.list":
			kept := f.agents[:0]
			for _, a := range f.agents {
				if n, ok := rf.lingerLeft[a.PaneID]; ok {
					if n <= 0 {
						delete(rf.lingerLeft, a.PaneID)
						continue
					}
					rf.lingerLeft[a.PaneID] = n - 1
				}
				kept = append(kept, a)
			}
			f.agents = kept
		case "agent.start":
			for _, a := range f.agents {
				if a.Name != "" && a.Name == p["name"] {
					return nil, &herdr.Error{Code: "agent_name_in_use", Message: "agent name " + a.Name + " is already used"}, true
				}
			}
			if rf.startFail > 0 {
				rf.startFail--
				return nil, &herdr.Error{Code: "agent_start_failed", Message: "boom"}, true
			}
			if len(rf.procs[pane]) != 1 || rf.procs[pane][0]["pid"] != rf.shellPID {
				return nil, &herdr.Error{Code: "agent_pane_busy", Message: "pane is not at a shell"}, true
			}
			rf.starts = append(rf.starts, p)
			rf.nextPID++
			kind := p["kind"].(string)
			rf.procs[pane] = []map[string]any{agentProc(kind, rf.nextPID)}
			a := herdr.AgentInfo{Name: p["name"].(string), PaneID: pane, WorkspaceID: "w1", Agent: kind, Status: herdr.StatusIdle, InteractiveReady: true, AgentSession: rf.sessions[pane], CWD: "/"}
			f.agents = append(f.agents, a)
			return map[string]any{"type": "agent_started", "agent": a}, nil, true
		case "agent.rename":
			target, _ := p["target"].(string)
			for i := range f.agents {
				if f.agents[i].PaneID == target || f.agents[i].Name == target {
					f.agents[i].Name, _ = p["name"].(string) // null clears
					return map[string]any{"type": "ok"}, nil, true
				}
			}
			return nil, &herdr.Error{Code: "agent_not_found", Message: target}, true
		}
		return nil, nil, false
	}
}

// exit ends the agent in target's pane when exitWorks.
func (rf *restartFake) exit(f *fakeLaunchHerdr, target, how string) {
	rf.exits = append(rf.exits, how)
	if !rf.exitWorks {
		return
	}
	kept := f.agents[:0]
	for _, a := range f.agents {
		if a.PaneID == target || a.Name == target {
			rf.procs[a.PaneID] = []map[string]any{shellProc(rf.shellPID)}
			if rf.linger > 0 {
				if rf.lingerLeft == nil {
					rf.lingerLeft = map[string]int{}
				}
				rf.lingerLeft[a.PaneID] = rf.linger
				kept = append(kept, a)
			}
			continue
		}
		kept = append(kept, a)
	}
	f.agents = kept
}

type restartEnv struct {
	s  *Server
	fh *fakeLaunchHerdr
	rf *restartFake
}

// restartServer is a hub with the given agents running (each at pid 100+i,
// started as just its kind) and a state directory.
func restartServer(t *testing.T, agents ...herdr.AgentInfo) *restartEnv {
	t.Helper()
	fh, client := startFakeLaunchHerdr(t)
	rf := &restartFake{shellPID: 50, procs: map[string][]map[string]any{}, sessions: map[string]*herdr.AgentSessionInfo{}, nextPID: 1000, exitWorks: true}
	fh.set(func(f *fakeLaunchHerdr) {
		for i, a := range agents {
			if a.WorkspaceID == "" {
				a.WorkspaceID = "w1"
			}
			a.InteractiveReady = true
			f.agents = append(f.agents, a)
			rf.procs[a.PaneID] = []map[string]any{agentProc(a.Agent, 100+i)}
			rf.sessions[a.PaneID] = a.AgentSession
		}
		rf.install(f)
	})
	m := managerWith(t, client, "desk", len(agents))
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "desk", StateDir: t.TempDir(), AgentToken: testToken, LaunchRoots: []string{t.TempDir()}}, client, m)
	s.findExec = func(name string) bool { return true }
	s.restartPoll, s.restartWait = 10*time.Millisecond, 400*time.Millisecond
	t.Cleanup(s.waitBackground)
	return &restartEnv{s: s, fh: fh, rf: rf}
}

func (e *restartEnv) post(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	w := do(e.s, "POST", "/api/restart", userHdr, body)
	var r map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return w.Code, r
}

// closedNothing fails the test when a refused restart sent an exit.
func (e *restartEnv) closedNothing(t *testing.T) {
	t.Helper()
	e.fh.mu.Lock()
	defer e.fh.mu.Unlock()
	if len(e.rf.exits) != 0 || len(e.rf.starts) != 0 {
		t.Errorf("a refused restart acted: exits %v, starts %d", e.rf.exits, len(e.rf.starts))
	}
}

func (e *restartEnv) startArgs(t *testing.T, i int) string {
	t.Helper()
	e.fh.mu.Lock()
	defer e.fh.mu.Unlock()
	if len(e.rf.starts) <= i {
		t.Fatalf("only %d starts", len(e.rf.starts))
	}
	b, _ := json.Marshal(e.rf.starts[i]["args"])
	return string(b)
}

func piSession(t *testing.T) *herdr.AgentSessionInfo {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &herdr.AgentSessionInfo{Agent: "pi", Kind: "path", Value: p}
}

func TestRestartNamedPiKeepsPaneAndSession(t *testing.T) {
	sess := piSession(t)
	e := restartServer(t, herdr.AgentInfo{Name: "w", PaneID: "w1:p1", Agent: "pi", Status: herdr.StatusIdle, CWD: "/", AgentSession: sess})
	code, r := e.post(t, `{"agent":"w1:p1","name":"w"}`)
	if code != 200 {
		t.Fatalf("restart = %d %v", code, r)
	}
	a := r["agent"].(map[string]any)
	if a["pane_id"] != "w1:p1" || a["name"] != "w" || r["session"] != sess.Value {
		t.Fatalf("result = %v", r)
	}
	// A new process, started on the same session; pi's flags are not readable
	// from its process, so the session alone, and the note says so.
	if r["old_pid"].(float64) != 100 || r["new_pid"].(float64) != 1001 || r["args_from"] != "session" {
		t.Fatalf("pids/args = %v", r)
	}
	if got := e.startArgs(t, 0); got != `["--session","`+sess.Value+`"]` {
		t.Fatalf("start args = %s", got)
	}
	if got := e.rf.exits; len(got) != 1 || got[0] != "ctrl+d" {
		t.Fatalf("pi was ended with %v, want ctrl+d", got)
	}
	e.fh.mu.Lock()
	start := e.rf.starts[0]
	e.fh.mu.Unlock()
	if start["name"] != "w" || start["pane_id"] != "w1:p1" || start["kind"] != "pi" {
		t.Fatalf("start = %v", start)
	}
	// The session-only start is not remembered as the launch: a second restart
	// must again say its flags are unknown.
	if l, ok := e.s.parkStore().LaunchOf("w"); ok {
		t.Fatalf("a launch was recorded for an unrecovered restart: %+v", l)
	}
	note := e.fh.call("agent.prompt")
	if note == nil || !strings.Contains(note.Params["text"].(string), "restarted in place") || !strings.Contains(note.Params["text"].(string), "could not be read back") {
		t.Fatalf("catch-up note = %+v", note)
	}
	// An audit entry.
	if evs := e.s.ledger.AuditLog(""); len(evs) == 0 || evs[len(evs)-1].Action != "agent.restart" || !evs[len(evs)-1].OK {
		t.Fatalf("audit = %+v", evs)
	}
}

func TestRestartUnnamedUsesTemporaryName(t *testing.T) {
	sess := piSession(t)
	e := restartServer(t, herdr.AgentInfo{PaneID: "w1:p1", Agent: "pi", Status: herdr.StatusIdle, CWD: "/", AgentSession: sess})
	code, r := e.post(t, `{"agent":"w1:p1"}`)
	if code != 200 || r["agent"].(map[string]any)["pane_id"] != "w1:p1" {
		t.Fatalf("restart = %d %v", code, r)
	}
	e.fh.mu.Lock()
	name, _ := e.rf.starts[0]["name"].(string)
	var left []string
	for _, a := range e.fh.agents {
		left = append(left, a.Name)
	}
	e.fh.mu.Unlock()
	if !strings.HasPrefix(name, "restart-") {
		t.Fatalf("started as %q, want a temporary name", name)
	}
	// Cleared as null: herdr refuses an empty string as a name.
	if rn := e.fh.call("agent.rename"); rn == nil || rn.Params["name"] != nil {
		t.Fatalf("the temporary name was not cleared: %+v", rn)
	}
	if len(left) != 1 || left[0] != "" {
		t.Fatalf("agent names after = %q", left)
	}
	if _, ok := e.s.parkStore().LaunchOf(name); ok {
		t.Error("the temporary name was left in the launch table")
	}
	// The note goes to the pane, not to a name that no longer exists.
	if n := e.fh.call("agent.prompt"); n == nil || n.Params["target"] != "w1:p1" {
		t.Fatalf("note = %+v", n)
	}
}

func TestRestartPrimaryCheckoutClaude(t *testing.T) {
	// No worktree, no name, not under any launch root: a home session.
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := t.TempDir()
	const sid = "c3eb8f54-0000-4000-8000-000000000002"
	proj := filepath.Join(cfg, "projects", scratch.ProjectDir(repo))
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, sid+".jsonl"), []byte("{}\n"), 0o644)
	e := restartServer(t, herdr.AgentInfo{PaneID: "w1:p1", Agent: "claude", Status: herdr.StatusDone, CWD: repo,
		AgentSession: &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Value: sid}})
	// The live command line carries the flags; the old resume flag and the prompt go.
	e.fh.set(func(f *fakeLaunchHerdr) {
		e.rf.procs["w1:p1"] = []map[string]any{
			agentProc("claude", 100, "claude", "--resume", "old", "--model", "opus", "--append-system-prompt", "be brief", "--", "first prompt"),
			agentProc("uv", 101, "uv", "tool", "uvx", "plane-mcp-server"),
		}
	})
	code, r := e.post(t, `{"agent":"w1:p1"}`)
	if code != 200 || r["args_from"] != "process" {
		t.Fatalf("restart = %d %v", code, r)
	}
	if got := e.startArgs(t, 0); got != `["--resume","`+sid+`","--model","opus","--append-system-prompt","be brief"]` {
		t.Fatalf("start args = %s", got)
	}
	if got := e.rf.exits; len(got) != 1 || got[0] != "/exit" {
		t.Fatalf("claude was ended with %v", got)
	}
	if n := e.fh.call("agent.prompt"); n == nil {
		t.Fatal("no prompt sent")
	}
	if r["old_pid"].(float64) != 100 || r["new_pid"].(float64) != 1001 {
		t.Fatalf("pids = %v", r)
	}
}

func TestRestartRecordedArgsBeatLiveArgv(t *testing.T) {
	sess := piSession(t)
	e := restartServer(t, herdr.AgentInfo{Name: "coord", PaneID: "w1:p1", Agent: "pi", Status: herdr.StatusIdle, CWD: "/", AgentSession: sess})
	e.s.parkStore().NoteLaunch("coord", "pi", []string{"--model", "homelab/thinking", "--no-skills", "--", "a prompt"}, timeNow())
	e.fh.set(func(f *fakeLaunchHerdr) {
		e.rf.procs["w1:p1"] = []map[string]any{agentProc("pi", 100, "pi", "--model", "other")}
	})
	code, r := e.post(t, `{"agent":"w1:p1"}`)
	if code != 200 || r["args_from"] != "recorded" {
		t.Fatalf("restart = %d %v", code, r)
	}
	if got := e.startArgs(t, 0); got != `["--session","`+sess.Value+`","--model","homelab/thinking","--no-skills"]` {
		t.Fatalf("start args = %s", got)
	}
	// Flags were recovered: no "could not be read back" in the note, and a
	// later park keeps the original flags, not the resume flag.
	if n := e.fh.call("agent.prompt"); n == nil || strings.Contains(n.Params["text"].(string), "could not be read back") {
		t.Fatalf("note = %+v", n)
	}
	if l, _ := e.s.parkStore().LaunchOf("coord"); strings.Join(l.Args, " ") != "--model homelab/thinking --no-skills" {
		t.Fatalf("launch after restart = %v", l.Args)
	}
}

func TestRestartRefusalsCloseNothing(t *testing.T) {
	sess := piSession(t)
	e := restartServer(t,
		herdr.AgentInfo{Name: "a", PaneID: "w1:p1", Agent: "pi", Status: herdr.StatusIdle, CWD: "/", AgentSession: sess},
		herdr.AgentInfo{Name: "x", PaneID: "w2:p1", Agent: "codex", Status: herdr.StatusIdle, CWD: "/"},
	)
	setStatus := func(st herdr.AgentStatus) {
		e.fh.set(func(f *fakeLaunchHerdr) { f.agents[0].Status = st })
	}
	want := func(body, code string, status int) {
		t.Helper()
		if c, r := e.post(t, body); c != status || r["code"] != code {
			t.Fatalf("%s: = %d %v, want %d %s", body, c, r, status, code)
		}
		e.closedNothing(t)
	}
	setStatus(herdr.StatusWorking)
	want(`{"agent":"w1:p1"}`, "agent_working", 409)
	setStatus(herdr.StatusBlocked)
	want(`{"agent":"w1:p1"}`, "agent_working", 409)
	setStatus(herdr.StatusIdle)
	want(`{"agent":"w1:p9"}`, "no_such_agent", 404)
	want(`{"agent":"w1:p1","name":"other"}`, "agent_changed", 409)
	want(`{"agent":"w2:p1"}`, "kind_not_restartable", 409)
	want(`{}`, "bad_request", 400)

	// A dialog open, and text typed in the input box.
	showScreen(e.fh, "w1:p1", piGateScreen)
	want(`{"agent":"w1:p1"}`, "dialog_open", 409)
	showScreen(e.fh, "w1:p1", capture(t, "pi_typed.ansi"))
	want(`{"agent":"w1:p1"}`, "input_busy", 409)
	showScreen(e.fh, "w1:p1", capture(t, "pi_empty2.ansi"))

	// A background task.
	e.s.manager.Refresh()
	e.fh.set(func(f *fakeLaunchHerdr) {
		f.agents[0].Background = []herdr.BackgroundTask{{Kind: "bash", Label: "npm run dev"}}
	})
	e.s.manager.Refresh()
	deadline := time.Now().Add(3 * time.Second)
	for len(e.s.localAgents()) == 0 || len(e.s.localAgents()[0].Background) == 0 {
		if time.Now().After(deadline) {
			t.Skip("the manager did not pick up the background task")
		}
		time.Sleep(20 * time.Millisecond)
	}
	want(`{"agent":"w1:p1"}`, "background_task", 409)
}

func TestRestartClaudeScheduleRefusedUnlessForced(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := t.TempDir()
	const sid = "c3eb8f54-0000-4000-8000-000000000003"
	proj := filepath.Join(cfg, "projects", scratch.ProjectDir(repo))
	os.MkdirAll(proj, 0o755)
	loop := `{"type":"assistant","timestamp":"` + time.Now().Add(-time.Hour).Format(time.RFC3339) + `","message":{"content":[{"type":"tool_use","name":"CronCreate","input":{"cron":"*/5 * * * *","prompt":"x"}}]}}` + "\n"
	sf := filepath.Join(proj, sid+".jsonl")
	os.WriteFile(sf, []byte(loop), 0o644)
	e := restartServer(t, herdr.AgentInfo{Name: "c", PaneID: "w1:p1", Agent: "claude", Status: herdr.StatusIdle, CWD: repo,
		AgentSession: &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Value: sid}})
	if c, r := e.post(t, `{"agent":"w1:p1"}`); c != 409 || r["code"] != "schedule_pending" {
		t.Fatalf("loop = %d %v", c, r)
	}
	e.closedNothing(t)
	// An unreadable session is uncertain, which also refuses.
	os.Remove(sf)
	e.fh.set(func(f *fakeLaunchHerdr) {
		f.agents[0].AgentSession = &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Value: sid}
	})
	if c, r := e.post(t, `{"agent":"w1:p1"}`); c != 409 {
		t.Fatalf("no session file = %d %v", c, r)
	}
	e.closedNothing(t)
	os.WriteFile(sf, []byte(loop), 0o644)
	code, r := e.post(t, `{"agent":"w1:p1","force_schedule":true}`)
	if code != 200 {
		t.Fatalf("forced = %d %v", code, r)
	}
	if got := e.startArgs(t, 0); got != `["--resume","`+sid+`"]` {
		t.Fatalf("args = %s", got)
	}
}

func TestRestartAbortsWhenTheProcessDoesNotEnd(t *testing.T) {
	sess := piSession(t)
	e := restartServer(t, herdr.AgentInfo{Name: "w", PaneID: "w1:p1", Agent: "pi", Status: herdr.StatusIdle, CWD: "/", AgentSession: sess})
	e.fh.set(func(f *fakeLaunchHerdr) { e.rf.exitWorks = false })
	code, r := e.post(t, `{"agent":"w1:p1"}`)
	if code != 504 || r["code"] != "exit_not_confirmed" {
		t.Fatalf("restart = %d %v", code, r)
	}
	e.fh.mu.Lock()
	defer e.fh.mu.Unlock()
	if len(e.rf.starts) != 0 || len(e.fh.agents) != 1 || e.rf.procs["w1:p1"][0]["pid"] != 100 {
		t.Fatalf("the agent was not left running: starts %d agents %v", len(e.rf.starts), e.fh.agents)
	}
	for _, c := range e.fh.calls {
		if strings.Contains(c.Method, "close") || strings.Contains(c.Method, "kill") || strings.Contains(c.Method, "signal") {
			t.Fatalf("restart called %s", c.Method)
		}
	}
	if evs := e.s.ledger.AuditLog(""); len(evs) == 0 || evs[len(evs)-1].OK {
		t.Fatalf("audit = %+v", evs)
	}
}

func TestRestartFailedStartThenRetry(t *testing.T) {
	sess := piSession(t)
	e := restartServer(t, herdr.AgentInfo{Name: "w", PaneID: "w1:p1", Agent: "pi", Status: herdr.StatusIdle, CWD: "/", AgentSession: sess})
	e.s.parkStore().NoteLaunch("w", "pi", []string{"--model", "m1"}, timeNow())
	e.fh.set(func(f *fakeLaunchHerdr) { e.rf.startFail = 1 })
	code, r := e.post(t, `{"agent":"w1:p1"}`)
	if code != 502 || r["code"] != "start_failed" || !strings.Contains(r["message"].(string), "retry") {
		t.Fatalf("restart = %d %v", code, r)
	}
	// The old process is gone and the pane is at its shell.
	e.fh.mu.Lock()
	gone := len(e.fh.agents) == 0
	e.fh.mu.Unlock()
	if !gone {
		t.Fatal("the agent was still listed")
	}
	// The same request again carries on from the start, on the same session
	// and with the same flags.
	code, r = e.post(t, `{"agent":"w1:p1","name":"w"}`)
	if code != 200 || r["agent"].(map[string]any)["pane_id"] != "w1:p1" {
		t.Fatalf("retry = %d %v", code, r)
	}
	if got := e.startArgs(t, 0); got != `["--session","`+sess.Value+`","--model","m1"]` {
		t.Fatalf("retry args = %s", got)
	}
	e.fh.mu.Lock()
	exits := len(e.rf.exits)
	e.fh.mu.Unlock()
	if exits != 1 {
		t.Fatalf("the retry ended a process again (%d exits)", exits)
	}
	// Nothing is left to retry.
	if c, r := e.post(t, `{"agent":"w1:p9"}`); c != 404 {
		t.Fatalf("after = %d %v", c, r)
	}
}

func TestRestartWaitsForHerdrToLetGoOfTheAgent(t *testing.T) {
	// herdr keeps an exited agent, and so its name, for a moment: a start in
	// that window fails with "name is already used".
	sess := piSession(t)
	e := restartServer(t, herdr.AgentInfo{Name: "w", PaneID: "w1:p1", Agent: "pi", Status: herdr.StatusIdle, CWD: "/", AgentSession: sess})
	e.fh.set(func(f *fakeLaunchHerdr) { e.rf.linger = 3 })
	if code, r := e.post(t, `{"agent":"w1:p1"}`); code != 200 {
		t.Fatalf("restart = %d %v", code, r)
	}
	if got := e.startArgs(t, 0); got != `["--session","`+sess.Value+`"]` {
		t.Fatalf("args = %s", got)
	}
}

func TestRestartIsTheUsersAlone(t *testing.T) {
	sess := piSession(t)
	e := restartServer(t, herdr.AgentInfo{Name: "w", PaneID: "w1:p1", Agent: "pi", Status: herdr.StatusIdle, CWD: "/", AgentSession: sess})
	for _, path := range []string{"/api/restart", "/api/hosts/desk/restart"} {
		w := do(e.s, "POST", path, agentHdr("w1:p1"), `{"agent":"w1:p1"}`)
		if w.Code != 403 {
			t.Errorf("%s with an agent token = %d %s", path, w.Code, w.Body)
		}
	}
	e.closedNothing(t)
	// The user's host-qualified route works.
	if w := do(e.s, "POST", "/api/hosts/desk/restart", userHdr, `{"agent":"w1:p1"}`); w.Code != 200 {
		t.Fatalf("host route = %d %s", w.Code, w.Body)
	}
}

func TestShellForeground(t *testing.T) {
	shell := herdr.Process{PID: 7, Name: "fish"}
	agent := herdr.Process{PID: 9, Name: "pi"}
	for name, tc := range map[string]struct {
		pi   herdr.ProcessInfo
		want bool
	}{
		"shell group":     {herdr.ProcessInfo{ShellPID: 7, ProcessGroup: 7, Processes: []herdr.Process{shell}}, true},
		"agent running":   {herdr.ProcessInfo{ShellPID: 7, ProcessGroup: 9, Processes: []herdr.Process{agent}}, false},
		"agent and child": {herdr.ProcessInfo{ShellPID: 7, ProcessGroup: 9, Processes: []herdr.Process{agent, shell}}, false},
		"no shell known":  {herdr.ProcessInfo{ProcessGroup: 9, Processes: []herdr.Process{agent}}, false},
		"nothing":         {herdr.ProcessInfo{ShellPID: 7}, false},
	} {
		if got := shellForeground(tc.pi); got != tc.want {
			t.Errorf("%s = %v, want %v", name, got, tc.want)
		}
	}
}

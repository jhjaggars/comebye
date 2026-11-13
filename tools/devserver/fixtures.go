package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// workspaces is session.snapshot's workspace list: the fixture agents that
// have a repository report their checkout, as herdr does for a git pane.
func (f *fake) workspaces() []any {
	out := []any{}
	for _, a := range f.agents {
		if a.repo == "" {
			continue
		}
		out = append(out, map[string]any{"workspace_id": a.Workspace, "worktree": map[string]any{
			"repo_name": a.repo, "repo_root": "/home/u/code/" + a.repo,
			"checkout_path": a.checkout, "is_linked_worktree": a.linked}})
	}
	return out
}

// A permission-style selection dialog, as Claude draws it: the app detects
// it on the visible screen and shows the "needs your answer" card.
var dialogScreen = []string{
	"● I need to pick a database for the cache layer.",
	"",
	"──────────────────────────────────────────────────────────────",
	"",
	"  Database",
	"",
	" Which database should the cache use?",
	"",
	"❯ 1. Redis",
	"     fast, in-memory",
	"  2. SQLite",
	"     embedded, durable",
	"  3. Postgres",
	"     already deployed",
	"  4. Type something.",
	"",
	"──────────────────────────────────────────────────────────────",
	"",
	" Enter to select · ↑/↓ to navigate · Esc to cancel",
}

// Claude Code's startup "trust this folder" dialog, as drawn by 2.1.x.
var claudeTrustScreen = []string{
	"──────────────────────────────────────────────────────────────────────",
	" Accessing workspace:",
	"",
	" /home/u/code/scratch",
	"",
	" Quick safety check: Is this a project you created or one you trust? (Like",
	" your own code, a well-known open source project, or work from your team).",
	" If not, take a moment to review what's in this folder first.",
	"",
	" Claude Code'll be able to read, edit, and execute files here.",
	"",
	" Security guide",
	"",
	" ❯ No, exit",
	"   Yes, I trust this folder",
	"",
	" Enter to confirm · Esc to cancel",
}

// addFixtures appends the agents the dev harness looks at, one per kind of
// row the list and the agent view draw differently. With a tag they are a
// second host's (a peer): a short list named after the tag.
func (f *fake) addFixtures(dir, tag string) {
	mk := func(name, pane, kind, display, status, title, cwd string) *agent {
		ws := pane[:len(pane)-3]
		a := &agent{Name: name, PaneID: pane, Workspace: ws, TabID: ws + ":t1", Agent: kind, Display: display,
			Status: status, Title: title, CWD: cwd, Ready: true, screen: []string{name + " ready"}}
		if kind == "claude" {
			file := filepath.Join(dir, name+".jsonl")
			if err := os.WriteFile(file, nil, 0o644); err != nil {
				log.Fatal(err)
			}
			a.transcript = file
			a.Session = map[string]string{"source": "fake", "agent": "claude", "kind": "path", "value": file}
		}
		f.agents = append(f.agents, a)
		return a
	}
	chat := func(a *agent, turns ...string) {
		for i, t := range turns {
			role := "assistant"
			if i%2 == 0 {
				role = "user"
			}
			f.record(a, role, t)
		}
	}
	if tag != "" {
		a := mk(tag+"-build", "w21:p1", "claude", "Claude", "idle", tag+"-build", "/srv/"+tag+"/build")
		chat(a, "status of the nightly?", "The nightly finished green on "+tag+".")
		mk(tag+"-docs", "w22:p1", "codex", "Codex", "working", tag+"-docs", "/srv/"+tag+"/docs")
		f.addContextAgents(dir, tag)
		return
	}

	// A worker: a linked worktree under ~/.worktrees, working.
	w := mk("fix-detail-menu", "w3:p1", "claude", "Claude", "working", "fix the agent view menu", "/home/u/.worktrees/comebye/fix-detail-menu")
	w.repo, w.checkout, w.linked = "comebye", w.CWD, true
	chat(w, "Make the ⋯ menu in the agent view always visible.",
		"I'll start by reading `web/app.js` for the detail header.\n\n- the menu button is `#btn-more`\n- it is hidden below 400px\n\n```css\n#btn-more { display: none; }\n```\n\nI'll change that rule now.")

	// A coordinator (given its role by dev.sh through the API).
	c := mk("laharl", "w4:p1", "claude", "Claude", "idle", "coordinator", "/home/u/code/comebye")
	c.repo, c.checkout = "comebye", c.CWD
	chat(c, "what is outstanding?", "Two workers are running: **fix-detail-menu** and **old-worker** (finished, marked ready to remove).")

	// A home agent in another repository's primary checkout.
	h := mk("notes-home", "w5:p1", "pi", "Pi", "idle", "notes", "/home/u/code/notes")
	h.repo, h.checkout = "notes", h.CWD
	h.screen = []string{"Trust project folder?", "  /home/u/code/notes", "", " ↑↓ navigate  enter select  escape/ctrl+c cancel"} // an unrecognised dialog on an idle agent

	// Blocked on a dialog: the app shows the question card.
	b := mk("needs-input", "w6:p1", "claude", "Claude", "blocked", "waiting on you", "/home/u/code/notes")
	b.repo, b.checkout = "notes", b.CWD
	chat(b, "pick a cache database", "I need to pick a database for the cache layer.")
	b.screen = dialogScreen

	f.addTrustPrompt()

	// Claude at its startup "trust this folder" dialog (herdr: blocked).
	ct := mk("claude-trust", "w10:p1", "claude", "Claude", "blocked", "trust this folder", "/home/u/code/scratch")
	ct.repo, ct.checkout = "scratch", ct.CWD
	chat(ct, "set up the scratch repo", "Starting in /home/u/code/scratch.")
	ct.screen = claudeTrustScreen

	// Marked ready to remove (dev.sh marks it through the API).
	m := mk("old-worker", "w7:p1", "claude", "Claude", "idle", "done", "/home/u/.worktrees/comebye/old-worker")
	m.repo, m.checkout, m.linked = "comebye", m.CWD, true
	chat(m, "finish the sweep change", "Done: merged into the branch and all tests pass.")

	// Long names and a long title, which the rows must truncate.
	l := mk("refactor-the-authentication-middleware-to-support-rotating-signing-keys", "w8:p1", "claude", "Claude", "idle",
		"Refactor the authentication middleware to support rotating signing keys without downtime for existing sessions",
		"/home/u/.worktrees/some-very-long-repository-name-for-testing/feature-branch-with-a-very-long-descriptive-name")
	l.repo, l.checkout, l.linked = "some-very-long-repository-name-for-testing", l.CWD, true
	chat(l, "go", "A long reply: "+longText)
	f.addContextAgents(dir, "")
}

// addContextAgents adds agents whose sessions carry context usage, one per
// look of the chip in the agent view: below the warning, warning, danger, a
// model whose window is not known (tokens only), no usage at all, a Pi
// session, and a session compacted after its last reply (hidden). Their
// replies grow the context by ctxStep, so the chip moves while a test talks to them.
func (f *fake) addContextAgents(dir, tag string) {
	mk := func(name, pane, kind, model string, tok, step int, pi bool) *agent {
		ws := pane[:len(pane)-3]
		a := &agent{Name: name, PaneID: pane, Workspace: ws, TabID: ws + ":t1", Agent: kind, Display: kind,
			Status: "idle", Title: name, CWD: "/home/u/code/ctx", Ready: true, screen: []string{name + " ready"},
			ctxTok: tok, ctxStep: step, ctxModel: model, ctxPi: pi}
		file := filepath.Join(dir, name+".jsonl")
		var head []byte
		if pi {
			head = []byte(`{"type":"session","version":3,"id":"01a1b1d1-3cf9-753f-b736-4219294a78a3","timestamp":"2026-11-06T17:04:02.298Z","cwd":"/home/u/code/ctx"}` + "\n")
		}
		if err := os.WriteFile(file, head, 0o644); err != nil {
			log.Fatal(err)
		}
		a.transcript = file
		a.Session = map[string]string{"source": "fake", "agent": kind, "kind": "path", "value": file}
		f.agents = append(f.agents, a)
		return a
	}
	ask := func(a *agent) {
		f.record(a, "user", "how full are you?")
		f.record(a, "assistant", "Quite.")
	}
	if tag != "" { // a peer host has one, to see the chip on an agent of another host
		ask(mk(tag+"-ctx", "w23:p1", "claude", "claude-sonnet-4-6", 124000-20000, 20000, false))
		return
	}
	ask(mk("ctx-ok", "w31:p1", "claude", "claude-sonnet-4-6", 124000-20000, 20000, false))
	ask(mk("ctx-warn", "w32:p1", "claude", "claude-opus-5-5", 830000-5000, 5000, false))
	ask(mk("ctx-danger", "w33:p1", "claude", "claude-sonnet-4-6", 192000-1000, 1000, false))
	ask(mk("ctx-live", "w38:p1", "claude", "claude-sonnet-4-6", 60000-20000, 20000, false))  // the test talks to this one
	ask(mk("ctx-live2", "w39:p1", "claude", "claude-sonnet-4-6", 60000-20000, 20000, false)) // the same, for the second viewport
	ask(mk("ctx-nowindow", "w34:p1", "claude", "claude-haiku-5-5", 40000-1000, 1000, false))
	ask(mk("ctx-pi", "w35:p1", "pi", "m1", 64000-4000, 4000, true))
	none := mk("ctx-none", "w36:p1", "claude", "", 0, 0, false)
	ask(none)
	st := mk("ctx-compacted", "w37:p1", "claude", "claude-sonnet-4-6", 100000-1000, 1000, false)
	ask(st)
	if fh, err := os.OpenFile(st.transcript, os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		fh.WriteString(`{"type":"system","subtype":"compact_boundary","timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","content":"Conversation compacted"}` + "\n")
		fh.Close()
	}
}

const longText = "Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. " +
	"Averyveryveryveryveryveryveryveryveryveryveryveryveryveryveryveryverylongunbrokenwordtocheckwrapping-and-overflow-in-the-chat-bubble."

// Pi's startup "Trust project folder?" prompt. herdr reports Pi as idle
// while it is up; comebye reads the screen and shows the agent as blocked.
var trustScreen = []string{
	"",
	" Trust project folder?",
	" /home/u/.worktrees/notes/fix-gateway-creator",
	"",
	" This allows pi to load .pi settings and resources, install missing project packages, and execute project extensions.",
	"",
	" → Trust",
	"   Trust parent folder (/home/u/.worktrees/notes)",
	"   Trust (this session only)",
	"   Do not trust",
	"   Do not trust (this session only)",
	"",
	" ↑↓ navigate  enter select  escape/ctrl+c cancel",
}

// addTrustPrompt adds the Pi agent that sits at the trust prompt (status
// idle, as herdr reports it).
func (f *fake) addTrustPrompt() {
	f.agents = append(f.agents, &agent{Name: "trust-prompt", PaneID: "w9:p1", Workspace: "w9", TabID: "w9:t1", Agent: "pi", Display: "Pi",
		Status: "idle", Title: "trust-prompt", CWD: "/home/u/.worktrees/notes/fix-gateway-creator", Ready: true,
		repo: "notes", checkout: "/home/u/.worktrees/notes/fix-gateway-creator", linked: true,
		trust: true, screen: trustScreen})
}

// addLongCommand adds a Claude agent that herdr calls blocked (a dialog phrase
// in recent command output matched one of its rules) while nobody is asked
// anything: a long command runs under a spinner. comebye shows it as working.
func (f *fake) addLongCommand() {
	f.agents = append(f.agents, &agent{Name: "long-command", PaneID: "w8:p1", Workspace: "w8", TabID: "w8:t1", Agent: "claude", Display: "Claude",
		Status: "blocked", Title: "watching a run", CWD: "/home/u/code/rollout", Ready: true,
		repo: "notes", checkout: "/home/u/code/notes",
		screen: []string{
			"● Bash(gh run watch 42669596484 --exit-status)",
			"  ⎿  Run CI (42669596484) · main",
			"     Do you want to proceed? (printed by the job, not asked of you)",
			"",
			"✻ Running… (3m 2s · ↓ 2.1k tokens)",
			"",
			"────────────────────────────────────────",
			" >",
			"────────────────────────────────────────",
			"  ⏵⏵ auto mode on (shift+tab to cycle)",
		}})
}

// addAutoBlock adds a Claude agent on an auto-mode permission prompt: a
// classifier reason and a live "will automatically block this request"
// countdown sit between the command and the question. Same screen as
// internal/dialog/testdata/claude_autoblock.txt.
func (f *fake) addAutoBlock() {
	f.agents = append(f.agents, &agent{Name: "auto-block", PaneID: "w11:p1", Workspace: "w11", TabID: "w11:t1", Agent: "claude", Display: "Claude",
		Status: "blocked", Title: "clean up the live worktree", CWD: "/home/u/code/comebye", Ready: true,
		repo: "comebye", checkout: "/home/u/code/comebye",
		screen: strings.Split(autoBlockScreen, "\n")})
}

const autoBlockScreen = `    kill $(cat $T/cbpid) $(sed -n 1p $T/pids) 2>/dev/null; rm -rf $(cat $T/sd)
    git worktree remove --force $T/home/.worktrees/comebye/hc && git branch -D h…


────────────────────────────────────────────────────────────────────────────────
 Bash command
 Run shell command
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 │ S=/tmp/claude-1000/scratchpad; T=$S/live
 │ kill $(cat $T/cbpid) $(sed -n 1p $T/pids) 2>/dev/null; rm -rf $(cat $T/sd)
 │ git worktree remove --force $T/home/.worktrees/comebye/hc && git branch -D hubcheck-live -q; git worktree list | head -3
 │ sed -i 's/"no commits missing from a remote"/"every commit is on a remote"/' internal/server/hubcheck.go
 │ git add internal/server/hubcheck.go && git commit -q -m "hubcheck: say 'every commit is on a remote' when there is no upstream
 │
 │ Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
 │ git fetch -q origin && git rebase origin/main 2>&1 | tail -2; git log --oneline origin/main..HEAD
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 │ Dangerous rm operation on statically-unresolvable target: command substitution output
 ⚠ Claude Code will automatically block this request in 1:17, to avoid holding up an unattended session


 Do you want to proceed?
 ❯ 1. Yes
   2. No`

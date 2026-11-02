package main

import (
	"log"
	"os"
	"path/filepath"
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

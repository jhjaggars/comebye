// Package cli is the comebye command line: the subcommands an agent acting
// as coordinator uses to read the fleet and the hub's ledger and to do the
// workflow's steps (hand off, prompt, mark, publish the briefing). It
// talks to the local Comebye server with the local agent token, as the
// restricted "agent" role; a peer forwards to its hub. It runs in the same
// binary as the server: `comebye <subcommand>`.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/playbook"
)

// Env is the CLI's environment (replaced in tests).
type Env struct {
	Stdout, Stderr io.Writer
	Stdin          io.Reader
	Getenv         func(string) string
	Hostname       func() (string, error)
	// HTTP, when set, makes the requests (tests).
	HTTP *http.Client
	// Now is the current time and Sleep waits (tests replace them; nil
	// Sleep is time.Sleep).
	Now   func() time.Time
	Sleep func(time.Duration)
}

// OSEnv is the real environment.
func OSEnv() *Env {
	return &Env{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Getenv: os.Getenv, Hostname: os.Hostname, Now: time.Now}
}

type command struct {
	name, args, summary string
	run                 func(e *Env, args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"agents", "[--host H] [--json]", "every agent on every host: project, branch, status, role", cmdAgents},
		{"ledger", "[--active] [--json]", "the hub's handoffs: status, branch, worker, report, marks, hints", cmdLedger},
		{"read", "AGENT [--lines N] [--report] [--json]", "an agent's recent messages, or its final report", cmdRead},
		{"inspect", "AGENT [--diff] [--json]", "an agent's git state on its host: uncommitted files, unmerged and unpushed commits", cmdInspect},
		{"handoff", "--host H --repo PATH --branch B [--base X] [--kind K] [--model M] [--thinking T] [--name N] --brief FILE|- [--plan ID] [--json]", "hand work to a new worker on its own branch and worktree", cmdHandoff},
		{"model", "AGENT [MODEL] [--thinking T] [--json]", "switch a running agent's model (claude, pi; it must be idle); pi also takes --thinking", cmdModel},
		{"models", "[--host H] [--kind K] [--json]", "the models an agent kind can be started with on a host (for `handoff --model`)", cmdModels},
		{"plan", "create --title T --size N | show ID | list [--json]", "group handoffs into one change: the user is told once when all of them are done", cmdPlan},
		{"release", "ID [--json]", "start a handoff that is waiting for earlier steps now (e.g. a step that went idle without reporting)", cmdRelease},
		{"cancel", "ID [--json]", "give up on a handoff that is waiting for earlier steps", cmdCancel},
		{"report", "FILE|- | --progress [--phase rebase|tests|push|ci] TEXT|-", "(workers) declare yourself done and send your final report to whoever handed you the task; --progress sends a short note on what you are doing instead, which is not a report and finishes nothing", cmdReport},
		{"prompt", "AGENT TEXT|- [--queue] [--json]", "send an agent a message; --queue holds it until the agent can take it", cmdPrompt},
		{"land", "AGENT --note \"<the user's OK>\" [--base main] [--deploy] [--queue] [--json]", "after the user OKs it, record the approval and tell a finished worker to land its own work: rebase as plain commits, test, fast-forward push, watch CI, delete its branch, report; the hub then checks it landed", cmdLand},
		{"queue", "[AGENT] [--all] [--cancel ID] [--json]", "prompts queued with `prompt --queue`: pending ones, or withdraw one", cmdQueue},
		{"todo", "add QUESTION [--kind decision|action|followup|release --version vX.Y.Z --commit REF] [--option LABEL[|DETAIL] ... [--recommend N[,M]]] [--context FILE|-] [--link [LABEL=]URL] [--handoff ID] [--waiting AGENT | --no-wait] | list [--all | --state S] | show ID | answer ID TEXT|- | done ID | drop ID [--reason R]  [--json]", "the to-do list beside the briefing: questions and follow-ups the user has to deal with, with context; any agent adds, the user answers in the app, the coordinator relays answers (answer, done and drop are the coordinator's)", cmdTodo},
		{"publish", "FILE|- [--title T] [--kind html|md] [--key K | --update ID] [--if-version N] [--handoff ID] [--json]", "publish an HTML or Markdown page (a report, a chart, a write-up) to the hub and print its link; the user reads it in the app's Pages. --update ID or --key K adds a version to the same link", cmdPublish},
		{"pages", "list [--mine] [--handoff ID] | show ID [--body [--version N]] | link ID | rm ID  [--json] | guide | new [--type doc|mockup|deck|dashboard] [--title T]", "the published pages: list them, show one (with its source), print its link, or delete one of yours; guide prints how to make a good page (the comebye-pages skill), new prints a starter page built on the Comebye page template", cmdPages},
		{"skill", "install|status [--claude] [--pi] [--force] [--json]", "install the comebye-pages skill (how to make a good page) where claude (~/.claude/skills) and pi (~/.pi/agent/skills) find it, or say whether it is there and current; never replaces a copy you edited unless --force", cmdSkill},
		{"mark", "AGENT --reason R [--discard] [--no-ask] [--wait] [--json]", "propose removing a session, or discarding its work (asks the session first; the user confirms in the app)", cmdMark},
		{"sweep", "[--dry-run] [--idle-after D] [--notify] [--json]", "find finished workers, check each on its host and mark the ones with nothing to lose as ready to remove; list the rest with why (never removes anything)", cmdSweep},
		{"unmark", "AGENT [--json]", "withdraw a removal proposal", cmdUnmark},
		{"brief", "set FILE|- [--no-check] | get | schedule [--every D] [--at HH:MM] [--tz ZONE] [--off] [--dismiss] [--json]", "publish or read the briefing shown in the app; show or set its schedule", cmdBrief},
		{"settings", "[show | set [--default-kind K] [--allow K,K|any] [--model KIND=M] [--thinking pi=LEVEL] | clear [--field default-kind|allowed|models]] [--host H] [--login L] [--json]", "a host's agent settings: the kind, model and thinking level a start uses when none is asked for, and the kinds it may start. Anyone reads them; setting is the user's alone", cmdSettings},
		{"peer", "list | show NAME | add NAME URL[|URL...] [--tag T] | remove NAME  [--login L] [--json]", "(hub, the user's alone) the hub's peers: list them with their status and version, add one live, or remove a dynamic one; peers set with -peers are listed but only changed by editing the hub's environment", cmdPeer},
		{"stt", "fetch | status [--verify] [--state-dir DIR] [--json]", "voice input for the app's mic buttons: `fetch` downloads the speech model and runtime once on this host from pinned upstream URLs, checked against pinned SHA-256s; `status` says whether they are installed (this host only)", cmdSTT},
		{"playbook", "[--harness pi|claude|generic]", "print the coordinator playbook", cmdPlaybook},
		{"presets", "claude|pi|generic", "print how to let a harness run comebye without prompts", cmdPresets},
		{"archive", "keygen --out DIR | run | verify | status | index [--ledger FILE|auto|none] [--git=false] [--gh] | restore SESSION --to DIR | delete SESSION [--yes] | summary SESSION TEXT|- | rewrap --to-dir DIR [--to-encryption age|none --to-recipient KEYS]  --encryption age|none [--dir D | --s3 URL --s3-credentials F] [--recipient K --identity F] [--json]", "keep every agent session forever: byte-exact, encrypted, append-only copies of Claude and pi session files, and an index to search them", cmdArchive},
		{"recall", "ask QUESTION|threads|todo|list|search WORDS|show THREAD|SESSION [--text N]|commit SHA  [--since D --until D --project P --branch B --kind K --ledger ID --limit N] [--json]", "answer \"we worked on something like this once, what was the outcome?\": finds the piece of work and shows how it ended (commits on main, PRs, report, deploy), its sessions, and the conversation behind a commit", cmdRecall},
		{"idle", "[--minutes N] [--json] [--dry-run]", "exit 0 when this host has been idle for N minutes (default 120): no herdr agent working and no interactive ssh session; exit 1 otherwise. Poll it: a poll that sees activity renews the clock. This host only", cmdIdle},
		{"upgrade", "[--version vX.Y.Z|latest | --from-source [--ref R] [--repo DIR]] [--dry-run] [--force] [--rollback] [--restart auto|systemd|exit [--pid N]]", "install a new comebye over this one and restart it: a verified release (default) or a source build; smoke-checks and rolls back by itself; restarts the user service, or with --restart exit (the default without systemd) stops the server so its supervisor starts it again; this host only", cmdUpgrade},
		{"whoami", "[--json]", "who this agent is to Comebye: id, host, hub", cmdWhoami},
		{"help", "[COMMAND]", "this help", nil},
	}
}

// IsCommand reports whether name is a subcommand (main uses it to choose
// between the CLI and the server).
func IsCommand(name string) bool {
	for _, c := range commands {
		if c.name == name {
			return true
		}
	}
	return false
}

// Summary lists the subcommands, for the server's -h.
func Summary() string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(tw, "  comebye %s\t%s\n", c.name, c.summary)
	}
	tw.Flush()
	return b.String()
}

// usageError is a mistake in the command line (exit status 2).
type usageError struct{ msg string }

func (u *usageError) Error() string { return u.msg }

func usageErr(format string, a ...any) error { return &usageError{fmt.Sprintf(format, a...)} }

// Run runs a subcommand (args[0]) and returns the exit status: 0 on
// success, 1 when the call failed, 2 for a usage error.
func Run(args []string, e *Env) int {
	if e.Now == nil {
		e.Now = time.Now
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) > 1 {
			for _, c := range commands {
				if c.name == args[1] && c.run != nil {
					fmt.Fprintf(e.Stdout, "usage: comebye %s %s\n\n%s\n", c.name, c.args, c.summary)
					return 0
				}
			}
		}
		help(e.Stdout)
		return 0
	}
	for _, c := range commands {
		if c.name != args[0] || c.run == nil {
			continue
		}
		err := c.run(e, args[1:])
		var ue *usageError
		switch {
		case err == nil:
			return 0
		case errors.Is(err, errNotIdle):
			return 1
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(e.Stdout, "usage: comebye %s %s\n\n%s\n", c.name, c.args, c.summary)
			return 0
		case errors.As(err, &ue):
			fmt.Fprintf(e.Stderr, "comebye %s: %s\nusage: comebye %s %s\n", c.name, ue.msg, c.name, c.args)
			return 2
		default:
			fmt.Fprintf(e.Stderr, "comebye %s: %s\n", c.name, err)
			return 1
		}
	}
	fmt.Fprintf(e.Stderr, "comebye: unknown command %q\n\n", args[0])
	help(e.Stderr)
	return 2
}

func help(w io.Writer) {
	fmt.Fprint(w, `comebye: the coordinator's command line for Comebye.

Usage:
`)
	for _, c := range commands {
		fmt.Fprintf(w, "  comebye %s %s\n      %s\n", c.name, c.args, c.summary)
	}
	fmt.Fprint(w, `
AGENT is a host-qualified id (laharl/w1Q:p1), a pane id on this host
(w1Q:p1) or an agent name; an ambiguous name is refused.

The CLI talks to the local Comebye (COMEBYE_URL, default `+DefaultURL+`)
with the agent token in its state directory (COMEBYE_STATE_DIR), as the
restricted agent role; $HERDR_PANE_ID identifies the calling agent.
Without a subcommand (or with flags), comebye runs the server: see
comebye -h.
`)
}

// parse parses flags that may come before, between or after positional
// arguments, and returns the positional ones.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageErr("%v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// A lone "-" is an argument (stdin), and so is everything after
		// "--" (fs.Parse consumed it already when it stopped there).
		if len(args) > len(rest) && args[len(args)-len(rest)-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func jsonOut(e *Env, v any) error {
	enc := json.NewEncoder(e.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// rawJSON pretty-prints a JSON answer.
func rawJSON(e *Env, data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		_, err := e.Stdout.Write(data)
		return err
	}
	return jsonOut(e, v)
}

// readInput reads FILE, or stdin for "-".
func readInput(e *Env, arg string, limit int64) (string, error) {
	var r io.Reader
	if arg == "-" {
		r = e.Stdin
	} else {
		f, err := os.Open(arg)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > limit {
		return "", fmt.Errorf("%s is larger than %d bytes", arg, limit)
	}
	return string(b), nil
}

func ago(e *Env, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := e.Now().Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- agents ----

type agentRow struct {
	ID     string `json:"id"`
	Host   string `json:"host"`
	PaneID string `json:"pane_id"`
	Name   string `json:"name,omitempty"`
	Kind   string `json:"kind,omitempty"`
	// Model and Thinking are known for ledger workers: what the handoff
	// asked for (empty: the agent's default, or an agent not handed off).
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	Status   string `json:"status"`
	Project  string `json:"project,omitempty"`
	Branch   string `json:"branch,omitempty"`
	CWD      string `json:"cwd,omitempty"`
	// Role: coordinator, worker (a ledger handoff or a linked worktree),
	// home (a repository's primary checkout) or "".
	Role   string `json:"role,omitempty"`
	Ledger string `json:"ledger_status,omitempty"`
	Self   bool   `json:"self,omitempty"`
	// Instance is the host's incarnation: it differs when a host name has
	// been taken over by another machine (--json only).
	Instance string `json:"instance,omitempty"`
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// worktreePath matches ~/.worktrees/<repo>/<branch>, a worker's checkout
// by convention.
func worktreeParts(cwd string) (repo, branch string, ok bool) {
	i := strings.Index(cwd, "/.worktrees/")
	if i < 0 {
		return "", "", false
	}
	parts := strings.Split(cwd[i+len("/.worktrees/"):], "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func cmdAgents(e *Env, args []string) error {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	hostF := fs.String("host", "", "only this host's agents")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	ar, err := c.getAgents()
	if err != nil {
		return err
	}
	who, _ := c.getWhoami()
	lg, _ := c.getLedger() // optional: no hub, no ledger
	entries := map[string]apiEntry{}
	roles := map[string]apiRole{}
	if lg != nil {
		for _, en := range lg.Entries {
			if en.AgentID != "" && activeStatus(en.Status) {
				entries[en.AgentID] = en
			}
		}
		roles = lg.Roles
	}
	var rows []agentRow
	for _, a := range ar.Agents {
		if *hostF != "" && a.Host != *hostF {
			continue
		}
		r := agentRow{ID: a.ID, Host: a.Host, PaneID: a.PaneID, Name: a.Name, Kind: a.Agent, Status: a.Status, CWD: a.CWD, Instance: a.Instance}
		en, isEntry := entries[a.ID]
		if isEntry && en.HostInstance != "" && a.Instance != "" && en.HostInstance != a.Instance {
			isEntry = false // an earlier machine of that name
		}
		wtRepo, wtBranch, inWT := worktreeParts(a.CWD)
		switch {
		case a.Worktree != nil && a.Worktree.Repo != "":
			r.Project, r.Branch = a.Worktree.Repo, a.Worktree.Branch
		case isEntry:
			r.Project, r.Branch = en.RepoName, en.Branch
		case inWT:
			r.Project, r.Branch = wtRepo, wtBranch
		case a.CWD != "":
			r.Project = baseName(a.CWD)
		}
		if isEntry {
			r.Ledger = en.Status
			r.Model, r.Thinking = en.Model, en.Thinking
			if r.Branch == "" {
				r.Branch = en.Branch
			}
		}
		switch {
		case roles[a.ID].Role == "coordinator" && (roles[a.ID].Name == "" || a.Name == "" || roles[a.ID].Name == a.Name):
			r.Role = "coordinator"
		case isEntry || (a.Worktree != nil && a.Worktree.Linked) || inWT:
			r.Role = "worker"
		case a.Worktree != nil:
			r.Role = "home"
		}
		r.Self = who != nil && who.Agent != "" && who.Agent == a.ID
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (a.Role == "coordinator") != (b.Role == "coordinator") {
			return a.Role == "coordinator"
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if (a.Role == "worker") != (b.Role == "worker") {
			return b.Role == "worker"
		}
		return a.ID < b.ID
	})
	if *asJSON {
		if rows == nil {
			rows = []agentRow{}
		}
		return jsonOut(e, map[string]any{"agents": rows, "hosts": ar.Hosts})
	}
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tNAME\tKIND\tMODEL\tSTATUS\tPROJECT\tBRANCH\tROLE")
	for _, r := range rows {
		role := r.Role
		if r.Ledger != "" && r.Ledger != "running" {
			role += " (" + r.Ledger + ")"
		}
		if r.Self {
			role = strings.TrimSpace(role + " (you)")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, dash(r.Name), dash(r.Kind), dash(modelLabel(r.Model, r.Thinking)), r.Status, dash(r.Project), dash(r.Branch), dash(role))
	}
	tw.Flush()
	for _, h := range ar.Hosts {
		if !h.Local && h.Status != "ok" && (*hostF == "" || *hostF == h.Name) {
			// "peer bee unreachable: dns (since 3h ago): cannot resolve
			// ..." - the class names what broke, the since for how long,
			// the error how to fix it - instead of a bare error string.
			if h.Status == "connecting" {
				fmt.Fprintf(e.Stdout, "peer %s: connecting, not reached yet\n", h.Name)
				continue
			}
			why := "unreachable"
			if h.Class != "" {
				why = h.Class
			}
			since := ""
			if h.Since != nil {
				if a := ago(e, *h.Since); a != "" {
					since = " (since " + a + ")"
				}
			}
			fmt.Fprintf(e.Stdout, "peer %s unreachable: %s%s", h.Name, why, since)
			if h.Error != "" {
				fmt.Fprintf(e.Stdout, ": %s", h.Error)
			}
			// Which address last answered, when the peer has more than one.
			if h.Addr != "" {
				fmt.Fprintf(e.Stdout, " (last answered at %s)", h.Addr)
			}
			fmt.Fprint(e.Stdout, "\n")
			fmt.Fprintln(e.Stdout, "  its agents are not listed; the hub keeps retrying")
		}
	}
	for _, w := range versionWarnings(ar.Hosts) {
		if *hostF == "" || strings.Contains(w, "host "+*hostF+" ") {
			fmt.Fprintln(e.Stdout, w)
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "(no agents)")
	}
	return nil
}

// versionWarnings lists one line per reachable peer whose version differs
// from the hub's, or that reports none (too old to send the header).
func versionWarnings(hosts []apiHost) []string {
	hub := ""
	for _, h := range hosts {
		if h.Local {
			hub = h.Version
		}
	}
	var out []string
	for _, h := range hosts {
		switch {
		case h.Local || h.Status != "ok" || hub == "":
		case h.Version == "":
			out = append(out, fmt.Sprintf("warning: host %s does not report its version (too old?); the hub runs %s", h.Name, hub))
		case h.Version != hub:
			out = append(out, fmt.Sprintf("warning: host %s runs %s, the hub runs %s; upgrade the one that is behind (comebye upgrade on that host)", h.Name, h.Version, hub))
		}
	}
	return out
}

// ---- ledger ----

func cmdLedger(e *Env, args []string) error {
	fs := flag.NewFlagSet("ledger", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	active := fs.Bool("active", false, "hide removed and failed handoffs")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	if *asJSON {
		data, err := c.raw("GET", "/api/ledger", nil, nil)
		if err != nil {
			return err
		}
		return rawJSON(e, data)
	}
	lg, err := c.getLedger()
	if err != nil {
		return err
	}
	shown := 0
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ENTRY\tSTATUS\tHOST\tREPO\tBRANCH\tWORKER\tMODEL\tREPORT\tNOTES")
	for _, en := range lg.Entries {
		if *active && (en.Status == "removed" || en.Status == "failed") {
			continue
		}
		shown++
		worker := en.AgentName
		if en.AgentID != "" {
			worker = strings.TrimSpace(worker + " " + en.AgentID)
		}
		var notes []string
		switch {
		case en.Status == "removed" || en.Status == "failed" || en.Status == "lost":
		case en.Live.Present:
			notes = append(notes, "agent "+en.Live.Status)
		case en.AgentID != "":
			notes = append(notes, "agent gone")
		}
		if en.Candidate {
			into := "its base"
			if en.Checks != nil && en.Checks.BaseUsed != "" {
				into = en.Checks.BaseUsed
			}
			notes = append(notes, "candidate: finished, clean and merged into "+into)
		} else if en.Checks != nil && (en.Status == "finished" || en.Status == "landed") {
			if en.Checks.Dirty {
				notes = append(notes, "uncommitted changes")
			}
			if en.Checks.Unmerged > 0 {
				notes = append(notes, fmt.Sprintf("not merged: %d commits not in %s", en.Checks.Unmerged, dash(en.Checks.BaseUsed)))
			}
		}
		if a := en.Approval; a != nil {
			notes = append(notes, "landing approved by "+dash(a.By)+": "+dash(a.Note))
		}
		if l := en.LandCheck; l != nil {
			switch {
			case l.OK && len(l.Landed) > 0:
				notes = append(notes, "landed on "+dash(l.Base)+" as "+strings.Join(shortSHAs(l.Landed), ", "))
			case l.OK:
				notes = append(notes, "landed on "+dash(l.Base))
			default:
				notes = append(notes, "landing check failed: "+dash(l.Verdict))
			}
		}
		if en.PlanID != "" {
			notes = append(notes, "plan "+en.PlanID)
		}
		if en.Status == "waiting" {
			notes = append(notes, "waiting for "+strings.Join(en.After, ", ")+" to report (`comebye release "+en.ID+"` starts it now)")
		}
		if en.Callback != nil {
			cb := "callback " + en.Callback.State
			if en.Callback.To != "" {
				cb += " to " + en.Callback.To
			}
			notes = append(notes, cb)
		}
		if en.Mark != nil {
			notes = append(notes, "marked by "+dash(en.Mark.By)+": "+dash(en.Mark.Reason))
			if a := askText(en.Mark.Ask); a != "" {
				notes = append(notes, a)
			}
		}
		if en.Error != "" {
			notes = append(notes, "error: "+en.Error)
		}
		report := "no"
		if en.FinalReport != "" {
			report = "yes"
		} else if en.Status == "running" {
			report = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", en.ID, en.Status, en.Host, dash(en.RepoName), en.Branch, dash(worker), dash(modelLabel(en.Model, en.Thinking)), report, strings.Join(notes, "; "))
	}
	tw.Flush()
	if shown == 0 {
		fmt.Fprintln(e.Stdout, "(no handoffs)")
	}
	if len(lg.Marks) > 0 {
		fmt.Fprintln(e.Stdout, "\nOther sessions marked ready to remove:")
		for _, m := range lg.Marks {
			fmt.Fprintf(e.Stdout, "  %s %s: marked by %s %s: %s\n", m.AgentID, dash(m.Name), dash(m.By), ago(e, m.At), dash(m.Reason))
			if a := askText(m.Ask); a != "" {
				fmt.Fprintf(e.Stdout, "    %s\n", a)
			}
		}
	}
	return nil
}

// ---- read ----

type apiMessage struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Time   string `json:"time"`
	Text   string `json:"text"`
	Note   string `json:"note"`
	Tool   string `json:"tool"`
	Args   string `json:"args"`
	Result string `json:"result"`
	Err    bool   `json:"err"`
	Ms     int64  `json:"ms"`
}

func cmdRead(e *Env, args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	lines := fs.Int("lines", 20, "how many recent messages (raw terminal: lines)")
	report := fs.Bool("report", false, "print a worker's final report from the ledger (any other agent: its last reply)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want exactly one AGENT")
	}
	if *lines < 1 || *lines > 500 {
		return usageErr("--lines must be between 1 and 500")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	if *report {
		return readReport(e, c, pos[0], *asJSON)
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	data, err := c.raw("GET", hostPath(t, "transcript"), url.Values{"limit": {strconv.Itoa(*lines)}}, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.code == "no_session" {
		// No session file (an agent kind without transcripts): the
		// terminal's recent output instead.
		out, err := c.raw("GET", hostPath(t, "output"), url.Values{"lines": {strconv.Itoa(max(*lines, 40))}}, nil)
		if err != nil {
			return err
		}
		if *asJSON {
			return rawJSON(e, out)
		}
		var o struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(out, &o)
		fmt.Fprintf(e.Stdout, "%s: no session transcript; recent terminal output:\n\n%s\n", t.label(), strings.TrimRight(o.Text, "\n"))
		return nil
	}
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var tr struct {
		Messages []apiMessage `json:"messages"`
		More     bool         `json:"more"`
	}
	if err := json.Unmarshal(data, &tr); err != nil {
		return fmt.Errorf("unexpected transcript: %v", err)
	}
	status := ""
	if t.Live != nil {
		status = " · " + t.Live.Status
	}
	fmt.Fprintf(e.Stdout, "%s%s · last %d messages\n", t.label(), status, len(tr.Messages))
	for _, m := range tr.Messages {
		renderMessage(e.Stdout, m)
	}
	return nil
}

func renderMessage(w io.Writer, m apiMessage) {
	when := ""
	if t, err := time.Parse(time.RFC3339Nano, m.Time); err == nil {
		when = " · " + t.Local().Format("Jan 2 15:04")
	}
	switch m.Kind {
	case "user", "assistant":
		fmt.Fprintf(w, "\n── %s%s\n%s\n", m.Kind, when, strings.TrimRight(m.Text, "\n"))
	case "tool":
		res := "running"
		switch {
		case m.Err:
			res = "error"
		case m.Result != "" || m.Ms > 0:
			res = "ok"
		}
		first, _, _ := strings.Cut(strings.TrimSpace(m.Result), "\n")
		if len(first) > 120 {
			first = first[:120] + "…"
		}
		args := m.Args
		if len(args) > 160 {
			args = args[:160] + "…"
		}
		line := fmt.Sprintf("  · %s %s → %s", m.Tool, args, res)
		if first != "" {
			line += ": " + first
		}
		fmt.Fprintln(w, line)
	default:
		if t := strings.TrimSpace(m.Note + m.Text); t != "" {
			fmt.Fprintf(w, "\n── %s%s\n%s\n", m.Kind, when, t)
		}
	}
}

// readReport prints the final report of a ledger worker: AGENT is a
// worker's agent (live or not) or a ledger entry id.
func readReport(e *Env, c *client, arg string, asJSON bool) error {
	lg, err := c.getLedger()
	if err != nil {
		return err
	}
	var found *apiEntry
	for i := range lg.Entries {
		if lg.Entries[i].ID == arg {
			found = &lg.Entries[i]
		}
	}
	if found == nil {
		ids := map[string]bool{arg: true}
		if t, err := c.resolve(arg); err == nil {
			ids[t.ID] = true
		}
		for i := len(lg.Entries) - 1; i >= 0 && found == nil; i-- {
			en := &lg.Entries[i]
			if ids[en.AgentID] || (en.AgentName != "" && en.AgentName == arg) || en.Branch == arg {
				found = en
			}
		}
	}
	if found == nil {
		return lastReply(e, c, arg, asJSON)
	}
	if asJSON {
		return jsonOut(e, map[string]any{"entry": found.ID, "agent_id": found.AgentID, "branch": found.Branch, "status": found.Status, "final_report": found.FinalReport})
	}
	if found.FinalReport == "" {
		return fmt.Errorf("%s (%s) has no final report yet (status %s)", found.Branch, found.ID, found.Status)
	}
	fmt.Fprintf(e.Stdout, "Final report of %s on %s/%s (%s, %s):\n\n%s\n", dash(found.AgentName), found.RepoName, found.Branch, found.ID, found.Status, strings.TrimRight(found.FinalReport, "\n"))
	return nil
}

// lastReply stands in for the final report of an agent that is not a
// ledger worker (started before the ledger, or not by a handoff): its
// latest assistant message.
func lastReply(e *Env, c *client, arg string, asJSON bool) error {
	t, err := c.resolve(arg)
	if err != nil {
		return err
	}
	data, err := c.raw("GET", hostPath(t, "transcript"), url.Values{"limit": {"40"}}, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.code == "no_session" {
		return fmt.Errorf("%s is not a worker in the ledger and has no session transcript; try `comebye read %s`", t.label(), arg)
	}
	if err != nil {
		return err
	}
	var tr struct {
		Messages []apiMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &tr); err != nil {
		return fmt.Errorf("unexpected transcript: %v", err)
	}
	var last *apiMessage
	for i := len(tr.Messages) - 1; i >= 0 && last == nil; i-- {
		if m := &tr.Messages[i]; m.Kind == "assistant" && strings.TrimSpace(m.Text) != "" {
			last = m
		}
	}
	if asJSON {
		text := ""
		if last != nil {
			text = last.Text
		}
		return jsonOut(e, map[string]any{"agent_id": t.ID, "ledger": false, "last_reply": text})
	}
	if last == nil {
		return fmt.Errorf("%s is not a worker in the ledger and has no reply in its recent messages", t.label())
	}
	when := ""
	if a := ago(e, parseTime(last.Time)); a != "" {
		when = " (" + a + ")"
	}
	fmt.Fprintf(e.Stdout, "%s is not a worker in the ledger; its last reply%s:\n\n%s\n", t.label(), when, strings.TrimRight(last.Text, "\n"))
	return nil
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// ---- inspect ----

type apiInspection struct {
	AgentID  string `json:"agent_id"`
	Host     string `json:"host"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	EntryID  string `json:"entry_id"`
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
	Checks   struct {
		Path        string   `json:"path"`
		Exists      bool     `json:"exists"`
		Primary     bool     `json:"primary"`
		Branch      string   `json:"branch"`
		Dirty       bool     `json:"dirty"`
		Unmerged    int      `json:"unmerged_commits"`
		BaseUsed    string   `json:"base_used"`
		Tip         string   `json:"tip"`
		Gone        bool     `json:"gone"`
		RepoMissing string   `json:"repo_missing"`
		RepoNote    string   `json:"repo_note"`
		Records     []string `json:"records"`
		Landed      []struct {
			SHA string `json:"sha"`
			As  string `json:"as"`
		} `json:"landed"`
		InProgress string `json:"in_progress"`
		Error      string `json:"error"`
		Detail     *struct {
			Status  []string `json:"status"`
			Commits []struct {
				SHA     string `json:"sha"`
				Subject string `json:"subject"`
			} `json:"commits"`
			DiffStat      string `json:"diff_stat"`
			Upstream      string `json:"upstream"`
			Unpushed      int    `json:"unpushed"`
			Diff          string `json:"diff"`
			WorkDiff      string `json:"work_diff"`
			DiffTruncated bool   `json:"diff_truncated"`
		} `json:"detail"`
	} `json:"checks"`
}

// landedSuffix says where commits that landed under other SHAs went.
func landedSuffix(base string, l []struct {
	SHA string `json:"sha"`
	As  string `json:"as"`
}) string {
	var as []string
	for _, x := range l {
		if len(x.As) >= 7 {
			as = append(as, x.As[:7])
		}
	}
	switch {
	case len(as) > 0:
		return fmt.Sprintf(" (landed on %s as %s)", base, strings.Join(as, ", "))
	case len(l) > 0:
		return fmt.Sprintf(" (landed on %s under other SHAs)", base)
	}
	return ""
}

func cmdInspect(e *Env, args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	diff := fs.Bool("diff", false, "include the diffs (the branch against its base, then uncommitted changes)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want exactly one AGENT")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	q := url.Values{"agent": {t.ID}}
	if *diff {
		q.Set("diff", "1")
	}
	data, err := c.raw("GET", "/api/inspect", q, nil)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var in apiInspection
	if err := json.Unmarshal(data, &in); err != nil {
		return fmt.Errorf("unexpected answer: %v", err)
	}
	ch := in.Checks
	w := e.Stdout
	status := in.Status
	if status == "" {
		status = "gone"
	}
	fmt.Fprintf(w, "%s · %s · on %s\n", t.label(), status, in.Host)
	if m := modelLabel(in.Model, in.Thinking); m != "" {
		fmt.Fprintf(w, "model:    %s\n", m)
	}
	switch {
	case !ch.Exists:
		fmt.Fprintf(w, "checkout: %s does not exist (any more)\n", ch.Path)
		switch {
		case !ch.Gone && ch.RepoMissing != "":
			fmt.Fprintf(w, "merged:   unknown · %s\n", ch.RepoMissing)
		case !ch.Gone:
			fmt.Fprintf(w, "merged:   unknown · no repository on record to check its commits against\n")
		case ch.Error != "":
			fmt.Fprintf(w, "merged:   unknown · %s\n", ch.Error)
		case ch.Unmerged > 0:
			fmt.Fprintf(w, "merged:   NO · %s has %d commits not in %s\n", strings.Join(ch.Records, ", "), ch.Unmerged, dash(ch.BaseUsed))
		case len(ch.Records) == 0:
			fmt.Fprintf(w, "merged:   yes · the worktree and branch were removed; nothing of it exists outside %s\n", dash(ch.BaseUsed))
		default:
			fmt.Fprintf(w, "merged:   yes · every commit of %s is in %s%s\n", strings.Join(ch.Records, ", "), dash(ch.BaseUsed), landedSuffix(ch.BaseUsed, ch.Landed))
		}
		if ch.RepoNote != "" {
			fmt.Fprintf(w, "          (%s)\n", ch.RepoNote)
		}
		return nil
	case ch.Primary:
		fmt.Fprintf(w, "checkout: %s (the repository's primary checkout)\n", ch.Path)
	default:
		fmt.Fprintf(w, "worktree: %s\n", ch.Path)
	}
	fmt.Fprintf(w, "branch:   %s (tip %s)\n", dash(ch.Branch), dash(shortTip(ch.Tip)))
	if ch.InProgress != "" {
		fmt.Fprintf(w, "WARNING:  a %s is in progress\n", ch.InProgress)
	}
	if ch.Error != "" {
		fmt.Fprintf(w, "error:    %s\n", ch.Error)
	}
	d := ch.Detail
	if d == nil {
		return nil
	}
	base := dash(ch.BaseUsed)
	switch {
	case ch.Unmerged < 0:
		fmt.Fprintf(w, "merged:   unknown · could not compare with %s\n", base)
	case ch.Unmerged == 0:
		fmt.Fprintf(w, "merged:   yes · every commit is in %s%s\n", base, landedSuffix(ch.BaseUsed, ch.Landed))
	default:
		fmt.Fprintf(w, "merged:   NO · %d commits not in %s (a squash-merged PR whose patch differs still counts: check the PR)\n", ch.Unmerged, base)
	}
	pushed := "no upstream"
	if d.Upstream != "" {
		pushed = "upstream " + d.Upstream
	}
	switch {
	case d.Unpushed > 0:
		pushed += fmt.Sprintf(" · %d commits on no remote (only here)", d.Unpushed)
	case d.Unpushed == 0 && ch.Unmerged > 0:
		pushed += " · every unmerged commit is on a remote, but pushed is not merged"
	case d.Unpushed == 0:
		pushed += " · nothing only here"
	default:
		pushed += " · unknown"
	}
	fmt.Fprintf(w, "pushed:   %s\n", pushed)
	if len(d.Commits) > 0 {
		fmt.Fprintf(w, "\nUnmerged commits:\n")
		for _, cm := range d.Commits {
			fmt.Fprintf(w, "  %s %s\n", cm.SHA, cm.Subject)
		}
	}
	if len(d.Status) > 0 {
		fmt.Fprintf(w, "\nUncommitted (git status --porcelain):\n")
		for _, l := range d.Status {
			fmt.Fprintf(w, "  %s\n", l)
		}
	} else {
		fmt.Fprintf(w, "\nNo uncommitted changes.\n")
	}
	if d.DiffStat != "" {
		fmt.Fprintf(w, "\nChanged against the base:\n%s\n", d.DiffStat)
	}
	if d.Diff != "" {
		fmt.Fprintf(w, "\n── diff: the branch against its base\n%s", d.Diff)
	}
	if d.WorkDiff != "" {
		fmt.Fprintf(w, "\n── diff: uncommitted changes\n%s", d.WorkDiff)
	}
	if d.DiffTruncated {
		fmt.Fprintf(w, "\n(diff truncated)\n")
	}
	return nil
}

func shortTip(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// ---- handoff ----

func cmdHandoff(e *Env, args []string) error {
	fs := flag.NewFlagSet("handoff", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	host := fs.String("host", "", "the host to run the worker on")
	repo := fs.String("repo", "", "the repository's primary checkout (absolute path on that host)")
	branch := fs.String("branch", "", "the worker's branch (created from --base when new)")
	base := fs.String("base", "", "the base branch (default: the repository's default branch)")
	kind := fs.String("kind", "", "the agent kind (default: the host's own default, see `comebye settings --host H`; naming another kind is allowed but warned about, and refused outside the host's allowed kinds)")
	purpose := fs.String("purpose", "", "research, plan, review or spike when the branch is not meant to merge (its report is the deliverable; the sweep then lists it apart)")
	name := fs.String("name", "", "the worker's name (default: from the branch)")
	model := fs.String("model", "", "the worker's model (see `comebye models`; default: the agent's own)")
	thinking := fs.String("thinking", "", "pi's thinking level: "+strings.Join(thinkingLevels, ", ")+" (default: pi's own)")
	brief := fs.String("brief", "", "the brief: a markdown file, or - for stdin")
	plan := fs.String("plan", "", "join this plan (from `comebye plan create`)")
	var after stringList
	fs.Var(&after, "after", "wait for this earlier step of the plan (its id or branch; repeat or comma-separate) and start when it has reported")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) > 0:
		return usageErr("unexpected argument %q", pos[0])
	case *host == "", *repo == "", *branch == "", *brief == "":
		return usageErr("--host, --repo, --branch and --brief are required")
	case !strings.HasPrefix(*repo, "/"):
		return usageErr("--repo must be an absolute path on the host")
	case len(after) > 0 && *plan == "":
		return usageErr("--after needs --plan")
	case *purpose != "" && !ledger.DisposablePurpose(*purpose):
		return usageErr("--purpose must be one of %s", strings.Join(ledger.DisposablePurposes, ", "))
	}
	text, err := readInput(e, *brief, 1<<20)
	if err != nil {
		return fmt.Errorf("brief: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		return usageErr("the brief is empty")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	body := map[string]any{"host": *host, "repo": *repo, "branch": *branch, "brief": text}
	if len(after) > 0 {
		body["after"] = []string(after)
	}
	for k, v := range map[string]string{"base": *base, "kind": *kind, "name": *name, "plan": *plan, "model": *model, "thinking": *thinking, "purpose": *purpose} {
		if v != "" {
			body[k] = v
		}
	}
	data, err := c.raw("POST", "/api/handoffs", nil, body)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Entry     apiEntry `json:"entry"`
		Existing  bool     `json:"existing"`
		Restarted bool     `json:"restarted"`
		Queued    bool     `json:"queued"`
		Worker    struct {
			Warnings []string `json:"warnings"`
		} `json:"worker"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("unexpected answer: %v", err)
	}
	// A warning about the host's settings goes to stderr, where a script
	// that parses stdout still sees it.
	for _, w := range r.Worker.Warnings {
		fmt.Fprintf(e.Stderr, "comebye handoff: warning: %s\n", w)
	}
	en := r.Entry
	switch {
	case r.Queued:
		fmt.Fprintf(e.Stdout, "Queued %s on %s (%s): it starts when %s has reported, with the report(s) quoted after its brief. `comebye release %s` starts it now; `comebye cancel %s` gives up.\n",
			en.Branch, en.Host, en.ID, strings.Join(en.After, ", "), en.ID, en.ID)
	case r.Restarted:
		fmt.Fprintf(e.Stdout, "The earlier worker on %s was gone: started %s (%s) in its worktree.\n", en.Branch, dash(en.AgentName), en.AgentID)
	case r.Existing:
		fmt.Fprintf(e.Stdout, "Already handed off: %s on %s is %s (%s, %s). Nothing new was started; use `comebye prompt` to give it more work.\n", en.Branch, en.Host, en.Status, dash(en.AgentName), en.AgentID)
	default:
		fmt.Fprintf(e.Stdout, "Handed off %s on %s to %s (%s).\n", en.Branch, en.Host, dash(en.AgentName), en.AgentID)
	}
	fmt.Fprintf(e.Stdout, "  entry:    %s\n  worktree: %s\n  base:     %s\n", en.ID, dash(en.Worktree), dash(en.Base))
	if en.Kind != "" && en.KindSource != "" {
		fmt.Fprintf(e.Stdout, "  agent:    %s (%s)\n", en.Kind, kindSourceLabel(en.KindSource))
	}
	if m := modelLabel(en.Model, en.Thinking); m != "" {
		fmt.Fprintf(e.Stdout, "  model:    %s\n", m)
	}
	if en.Error != "" {
		fmt.Fprintf(e.Stdout, "  warning:  %s\n", en.Error)
	}
	return nil
}

// ---- prompt ----

func cmdPrompt(e *Env, args []string) error {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	queue := fs.Bool("queue", false, "hold the prompt on the hub and send it once the agent is idle with no question open (up to 2 hours) instead of failing")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usageErr("want AGENT and TEXT (or - to read stdin)")
	}
	text := strings.Join(pos[1:], " ")
	if len(pos) == 2 && pos[1] == "-" {
		if text, err = readInput(e, "-", 1<<20); err != nil {
			return err
		}
	}
	if strings.TrimSpace(text) == "" {
		return usageErr("the text is empty")
	}
	// TEXT is sent as is, never read from a file: a path that exists is
	// almost always a mistake, and the agent would get the path itself.
	if len(pos) == 2 && pos[1] != "-" {
		if fi, err := os.Stat(pos[1]); err == nil && fi.Mode().IsRegular() {
			return usageErr("%q is a file, and TEXT is sent as is; to send its contents, run: comebye prompt %s - < %s", pos[1], pos[0], pos[1])
		}
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	if c.pane != "" {
		if w, err := c.getWhoami(); err == nil && w.Agent == t.ID {
			return errors.New("refusing to prompt yourself")
		}
	}
	return sendPrompt(e, c, t, text, *queue, *asJSON)
}

// sendPrompt delivers text to t through the normal prompt path (the host's
// dialog guard applies), or queues it on the hub.
func sendPrompt(e *Env, c *client, t target, text string, queue, asJSON bool) error {
	if queue {
		return queuePrompt(e, c, t, text, asJSON)
	}
	data, err := c.raw("POST", hostPath(t, "prompt"), nil, map[string]string{"text": text})
	var ae *apiError
	if errors.As(err, &ae) && ae.code == "input_busy" {
		// The user has text in that agent's input box: typing over it would
		// mix the two messages. Hold the prompt on the hub instead; it is
		// sent once the box has been empty for a few seconds.
		if !asJSON {
			fmt.Fprintf(e.Stdout, "Held: the user is typing in %s's input box.\n", t.label())
		}
		if qerr := queuePrompt(e, c, t, text, asJSON); qerr != nil {
			return fmt.Errorf("the user is typing in %s's input box, and the prompt could not be held: %v", t.label(), qerr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if asJSON {
		return rawJSON(e, data)
	}
	fmt.Fprintf(e.Stdout, "Sent to %s.\n", t.label())
	return nil
}

// ---- land ----

// landPrompt is the standard land request sent to a finished worker.
func landPrompt(base string, deploy bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your report is accepted and the user has OKed landing your work. This message is your authorisation to do exactly these steps, and nothing beyond them:\n\n")
	fmt.Fprintf(&b, "1. `git fetch origin`, then rebase (or cherry-pick) your commits onto the latest `origin/%s` as plain commits: no merge commits, and no merge commits that exist only to carry a signature. Resolve conflicts in your own work; if one is not obviously yours to resolve, stop and report. Before pushing, check that `git log --merges origin/%s..HEAD` prints nothing. Sign commits only as the repository's git config says (`commit.gpgsign`); if signing fails, stop and report: never push unsigned commits to a repository that wants them signed, and never add a merge commit to carry a signature.\n", base, base)
	b.WriteString("2. Re-run the tests (and vet/format checks) on the rebased result: all of the project's tests, including its documented UI or browser tests (see its docs or CLAUDE.md), not only the Go or unit ones. If they fail, stop and report; do not push.\n")
	fmt.Fprintf(&b, "3. Push as a fast-forward of `%s` (`git push origin HEAD:%s`). Never force-push, never rewrite pushed history. If the push is rejected because %s moved, fetch, rebase again and re-test; do not force.\n", base, base, base)
	fmt.Fprintf(&b, "4. Watch CI for the pushed commit until it finishes: `gh run list --branch %s --limit 1` gives the run ID for your push (check that it is your commit's run), then `gh run watch ID --exit-status`. If the watch might outlast a tool timeout, run it in the background and wait for it to finish. Confirm the result with `gh run view ID --json conclusion`: do not pipe the watch through `tail` or the like, which hides its exit status. If CI fails, report the failure; do not push fixes unasked.\n", base)
	fmt.Fprintf(&b, "5. Clean up your branch: detach your worktree first (`git switch --detach origin/%s`), then delete the local branch (`git branch -D BRANCH`; it is merged, but your SHAs may differ from the pushed ones). Delete the remote branch only if `git ls-remote --heads origin BRANCH` shows it; if you never pushed it, there is nothing to delete. The worktree stays; the session is removed separately.\n", base)
	b.WriteString("\nWhile a long step runs (rebase, tests, CI), tell the hub what you are doing with `comebye report --progress --phase rebase|tests|push|ci \"<what you are doing>\"`. That is a progress note: it finishes nothing, and the card shows \"landing: tests running\". Never send `comebye report -` for it, nor before you have pushed: a report is you saying you are done, and the hub then checks that your work is on `origin/" + base + "`. If you wait on a background task, say so in a note first; the task ending wakes you.\n")
	b.WriteString("\nDo all of this in your own worktree. Never touch the repository's primary checkout, or any other checkout: it may hold the user's uncommitted work.\n")
	if deploy {
		b.WriteString("\n6. Deploy as the project's docs describe, once CI is green.\n")
		b.WriteString("\nThen report with `comebye report -`, in the same format as your first report (Summary, Changes, Verification grouped as Verified live / Unit tests only / Not verified, Follow-ups): the landed commit hashes, the test and CI results, and the deploy result.\n")
	} else {
		b.WriteString("\nDo not deploy: that is a separate step, done only when asked.\n")
		b.WriteString("\nThen report with `comebye report -`, in the same format as your first report (Summary, Changes, Verification grouped as Verified live / Unit tests only / Not verified, Follow-ups): the landed commit hashes and the test and CI results.\n")
	}
	b.WriteString("\nThe hub watches `origin/" + base + "` while you land: the entry counts as landed only if every commit you were approved to land is in `origin/" + base + "`, and it shows landed as soon as they are. It is shown as failed only if you report that you are done (or stop) and they are not.\n")
	return b.String()
}

func cmdLand(e *Env, args []string) error {
	fs := flag.NewFlagSet("land", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	queue := fs.Bool("queue", false, "hold the prompt on the hub and send it once the agent is idle with no question open (up to 2 hours) instead of failing")
	base := fs.String("base", "main", "the branch to land on")
	deploy := fs.Bool("deploy", false, "also deploy after CI is green (only when the user asked for it)")
	note := fs.String("note", "", "how the user OKed landing, in their words (required)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want exactly one AGENT")
	}
	if strings.TrimSpace(*base) == "" || strings.HasPrefix(*base, "-") || strings.ContainsAny(*base, " \t\n`$\\") {
		return usageErr("bad --base %q", *base)
	}
	if strings.TrimSpace(*note) == "" {
		return usageErr("say how the user OKed landing: --note \"<their words>\" (land only after they did)")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	if c.pane != "" {
		if w, err := c.getWhoami(); err == nil && w.Agent == t.ID {
			return errors.New("refusing to prompt yourself")
		}
	}
	lg, err := c.getLedger()
	if err != nil {
		return err
	}
	var en *apiEntry
	for i := range lg.Entries {
		if lg.Entries[i].AgentID == t.ID && lg.Entries[i].Status != "removed" && lg.Entries[i].Status != "failed" && lg.Entries[i].Status != "lost" {
			en = &lg.Entries[i]
		}
	}
	if en == nil {
		return fmt.Errorf("%s has no handoff in the ledger: land is for workers started with `comebye handoff`", t.label())
	}
	retry := en.Status == "landing" && en.FinishedAt == nil
	if !retry && (en.Status != "finished" || strings.TrimSpace(en.FinalReport) == "") {
		return fmt.Errorf("%s is not ready to land: its ledger entry is %q and %s; land after the worker has finished and reported", t.label(), en.Status, map[bool]string{true: "has a final report", false: "has no final report"}[strings.TrimSpace(en.FinalReport) != ""])
	}
	// Record the approval first (the entry becomes landing), then send the
	// prompt; if it cannot be sent, the landing is undone.
	if _, err := c.raw("POST", "/api/landings", nil, map[string]string{"agent_id": t.ID, "note": *note, "base": *base}); err != nil {
		return err
	}
	if err := sendPrompt(e, c, t, landPrompt(*base, *deploy), *queue, *asJSON); err != nil {
		if _, cerr := c.raw("POST", "/api/landings", nil, map[string]any{"agent_id": t.ID, "cancel": true}); cerr != nil {
			return fmt.Errorf("%w (and undoing the landing failed: %v; its entry still says landing)", err, cerr)
		}
		return fmt.Errorf("%w (the landing was not recorded)", err)
	}
	return nil
}

type apiQueued struct {
	ID        string     `json:"id"`
	Agent     string     `json:"agent_id"`
	Name      string     `json:"name"`
	Text      string     `json:"text"`
	From      string     `json:"from"`
	At        time.Time  `json:"at"`
	State     string     `json:"state"`
	Tries     int        `json:"tries"`
	SettledAt *time.Time `json:"settled_at"`
	Held      string     `json:"held"`
}

func queuePrompt(e *Env, c *client, t target, text string, asJSON bool) error {
	data, err := c.raw("POST", "/api/queue", nil, map[string]string{"agent": t.ID, "name": t.Name, "text": text})
	if err != nil {
		return err
	}
	if asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Queued    apiQueued `json:"queued"`
		Duplicate bool      `json:"duplicate"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if r.Duplicate {
		fmt.Fprintf(e.Stdout, "Already queued for %s as %s; nothing new added.\n", t.label(), r.Queued.ID)
		return nil
	}
	fmt.Fprintf(e.Stdout, "Queued %s for %s: it is sent when the agent is idle with no question open and nobody is typing in its input box, for up to 2 hours. Withdraw it with `comebye queue --cancel %s`.\n", r.Queued.ID, t.label(), r.Queued.ID)
	return nil
}

// ---- queue ----

func cmdQueue(e *Env, args []string) error {
	fs := flag.NewFlagSet("queue", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	all := fs.Bool("all", false, "include prompts delivered, expired, dropped or withdrawn in the last day")
	cancel := fs.String("cancel", "", "withdraw the pending prompt with this id")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("want at most one AGENT")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	if *cancel != "" {
		if len(pos) > 0 || *all {
			return usageErr("--cancel takes only an id")
		}
		data, err := c.raw("DELETE", "/api/queue/"+url.PathEscape(*cancel), nil, nil)
		if err != nil {
			return err
		}
		if *asJSON {
			return rawJSON(e, data)
		}
		fmt.Fprintf(e.Stdout, "Withdrew %s.\n", *cancel)
		return nil
	}
	q := url.Values{}
	if len(pos) == 1 {
		t, err := c.resolve(pos[0])
		if err != nil {
			return err
		}
		q.Set("agent", t.ID)
	}
	if *all {
		q.Set("all", "1")
	}
	data, err := c.raw("GET", "/api/queue", q, nil)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Queue []apiQueued `json:"queue"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if len(r.Queue) == 0 {
		fmt.Fprintln(e.Stdout, "No queued prompts.")
		return nil
	}
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tTO\tFROM\tQUEUED\tTEXT")
	for _, it := range r.Queue {
		text := strings.Join(strings.Fields(it.Text), " ")
		if r := []rune(text); len(r) > 60 {
			text = string(r[:60]) + "…"
		}
		state := it.State
		if it.Held != "" && it.State == "pending" {
			state = "held (typing)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", it.ID, state, strings.TrimSpace(it.Name+" "+it.Agent), it.From, it.At.Local().Format("15:04"), text)
	}
	return tw.Flush()
}

// ---- mark / unmark ----

type apiSweepItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Reason string `json:"reason"`
	Marked bool   `json:"marked"`
	AdHoc  bool   `json:"ad_hoc"`
}

// sweepGroupNote says what happened to a group a flag opts into.
func sweepGroupNote(on, dry bool, hint string) string {
	switch {
	case on && !dry:
		return "marked"
	case on:
		return "would mark"
	}
	return "not marked; " + hint
}

func cmdSweep(e *Env, args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	dry := fs.Bool("dry-run", false, "list what would be marked, mark nothing")
	idle := fs.String("idle-after", "", "also count a worker still recorded as running whose agent has been idle this long (default 30m; 0 turns it off)")
	notify := fs.Bool("notify", false, "tell an idle coordinator the outcome (not with --dry-run)")
	markDelivered := fs.Bool("mark-delivered", false, "also mark the research/plan/review/spike workers that reported, to discard their local commits (the user confirms each)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	data, err := c.raw("POST", "/api/sweep", nil, map[string]any{"dry_run": *dry, "idle_after": *idle, "notify": *notify, "mark_delivered": *markDelivered})
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		DryRun      bool           `json:"dry_run"`
		Ready       []apiSweepItem `json:"ready"`
		NeedsLook   []apiSweepItem `json:"needs_look"`
		Delivered   []apiSweepItem `json:"delivered"`
		Gone        []apiSweepItem `json:"gone"`
		Skipped     []apiSweepItem `json:"skipped"`
		Notified    string         `json:"notified"`
		NotifyError string         `json:"notify_error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	verb := "Marked ready to remove"
	if r.DryRun {
		verb = "Would mark ready to remove (dry run)"
	}
	section := func(title string, items []apiSweepItem) {
		fmt.Fprintf(e.Stdout, "%s (%d)\n", title, len(items))
		for _, it := range items {
			tag := ""
			if it.AdHoc {
				tag = "  (no handoff)"
			}
			fmt.Fprintf(e.Stdout, "  %s  %s  %s%s\n    %s\n", it.ID, it.Name, it.Branch, tag, it.Reason)
		}
	}
	section(verb, r.Ready)
	section("Needs a look (not marked)", r.NeedsLook)
	section("Work delivered in the report; branch has local commits ("+sweepGroupNote(*markDelivered, r.DryRun, "mark with --mark-delivered")+")", r.Delivered)
	section("Gone: nothing left to lose or check ("+sweepGroupNote(true, r.DryRun, "")+")", r.Gone)
	fmt.Fprintf(e.Stdout, "Skipped: %d\n", len(r.Skipped))
	if r.Notified != "" {
		fmt.Fprintf(e.Stdout, "Told %s.\n", r.Notified)
	} else if r.NotifyError != "" {
		fmt.Fprintf(e.Stdout, "Could not tell a coordinator: %s\n", r.NotifyError)
	}
	if !r.DryRun && len(r.Ready) > 0 {
		fmt.Fprintln(e.Stdout, "The user confirms removal in the app's removals review.")
	}
	return nil
}

func cmdMark(e *Env, args []string) error {
	fs := flag.NewFlagSet("mark", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	reason := fs.String("reason", "", "why the session can go (required)")
	discard := fs.Bool("discard", false, "propose discarding its work too: the worktree even if dirty or unmerged, and the branch")
	noAsk := fs.Bool("no-ask", false, "do not ask the session whether it is safe to remove")
	wait := fs.Bool("wait", false, "wait (up to about 2 minutes) for the session's answer and print it")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want exactly one AGENT")
	}
	if strings.TrimSpace(*reason) == "" {
		return usageErr("--reason is required: say why the session can go")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	data, err := c.raw("POST", "/api/marks", nil, map[string]any{"agent_id": t.ID, "reason": *reason, "discard": *discard, "no_ask": *noAsk})
	if err != nil {
		return err
	}
	var r struct {
		Mark apiMark `json:"mark"`
	}
	_ = json.Unmarshal(data, &r)
	ask := r.Mark.Ask
	if *wait && ask != nil && ask.State == "asking" {
		if ask, err = waitAsk(e, c, t.ID, r.Mark.At); err != nil {
			return err
		}
	}
	if *asJSON {
		var v map[string]any
		if *wait && json.Unmarshal(data, &v) == nil {
			if m, ok := v["mark"].(map[string]any); ok {
				m["ask"] = ask
				return jsonOut(e, v)
			}
		}
		return rawJSON(e, data)
	}
	if *discard {
		fmt.Fprintf(e.Stdout, "Proposed discarding %s: its session, worktree and branch. The user sees what would be lost and confirms in the app.\n", t.label())
	} else {
		fmt.Fprintf(e.Stdout, "Marked %s ready to remove. The user reviews the checks and confirms in the app.\n", t.label())
	}
	switch {
	case ask == nil:
	case ask.State == "asking":
		fmt.Fprintf(e.Stdout, "Asked the session whether it is safe to remove; its answer shows in the app's review and in comebye ledger (or use --wait).\n")
	default:
		fmt.Fprintf(e.Stdout, "%s.\n", upperFirst(askText(ask)))
	}
	return nil
}

// askWaitFor bounds mark --wait: the hub gives up after 2 minutes.
const askWaitFor = 150 * time.Second

// waitAsk polls the ledger until the mark made at markedAt has an answer.
func waitAsk(e *Env, c *client, agentID string, markedAt time.Time) (*apiAsk, error) {
	sleep := e.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	for waited := time.Duration(0); ; waited += 2 * time.Second {
		var lg ledgerResp
		if err := c.call("GET", "/api/ledger", nil, nil, &lg); err != nil {
			return nil, err
		}
		marks := lg.Marks
		for _, en := range lg.Entries {
			if en.Mark != nil && en.AgentID == agentID {
				marks = append(marks, *en.Mark)
			}
		}
		var m *apiMark
		for i := range marks {
			if marks[i].AgentID == agentID {
				m = &marks[i]
			}
		}
		switch {
		case m == nil || !m.At.Equal(markedAt):
			return nil, errors.New("the mark was cleared or replaced while waiting")
		case m.Ask == nil || m.Ask.State != "asking":
			return m.Ask, nil
		case waited >= askWaitFor:
			return m.Ask, nil
		}
		sleep(2 * time.Second)
	}
}

// askText is a session's answer about its own removal, for people.
func askText(a *apiAsk) string {
	if a == nil {
		return ""
	}
	why := ""
	if a.Reason != "" {
		why = " (" + a.Reason + ")"
	}
	switch a.State {
	case "asking":
		return "asking the session whether it is safe to remove"
	case "safe":
		return "the session says SAFE" + why
	case "unsafe":
		return "the session says UNSAFE: " + dash(a.Reason)
	case "unclear":
		return "the session answered neither SAFE nor UNSAFE: " + dash(a.Reason)
	case "no_answer":
		return "the session did not answer (not the same as safe)" + why
	case "not_asked":
		return "the session was not asked" + why
	}
	return "session answer: " + a.State
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

type apiPlan struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Size    int    `json:"size"`
	State   string `json:"state"`
	Done    int    `json:"done"`
	Members []struct {
		ID       string `json:"id"`
		Host     string `json:"host"`
		RepoName string `json:"repo_name"`
		Branch   string `json:"branch"`
		Status   string `json:"status"`
		Outcome  string `json:"outcome"`
		Reported bool   `json:"reported"`
	} `json:"members"`
}

func cmdPlan(e *Env, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	title := fs.String("title", "", "create: what the change is, in a few words")
	size := fs.Int("size", 0, "create: how many workers the whole change takes")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr("want create, show ID or list")
	}
	if (*title != "" || *size != 0) && pos[0] != "create" {
		return usageErr("--title and --size go with plan create")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	switch pos[0] {
	case "create":
		if len(pos) != 1 || strings.TrimSpace(*title) == "" || *size < 1 {
			return usageErr("want plan create --title T --size N")
		}
		data, err := c.raw("POST", "/api/plans", nil, map[string]any{"title": *title, "size": *size})
		if err != nil {
			return err
		}
		if *asJSON {
			return rawJSON(e, data)
		}
		var r struct {
			Plan apiPlan `json:"plan"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("unexpected answer: %v", err)
		}
		fmt.Fprintf(e.Stdout, "Plan %s created (%d workers). Hand each one off with `comebye handoff --plan %s …`; the user is told once all %d are done.\n", r.Plan.ID, r.Plan.Size, r.Plan.ID, r.Plan.Size)
		return nil
	case "show", "list":
		path := "/api/plans"
		if pos[0] == "show" {
			if len(pos) != 2 {
				return usageErr("want plan show ID")
			}
			path += "/" + url.PathEscape(pos[1])
		} else if len(pos) != 1 {
			return usageErr("plan list takes no argument")
		}
		data, err := c.raw("GET", path, nil, nil)
		if err != nil {
			return err
		}
		if *asJSON {
			return rawJSON(e, data)
		}
		var r struct {
			Plan  *apiPlan  `json:"plan"`
			Plans []apiPlan `json:"plans"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("unexpected answer: %v", err)
		}
		if r.Plan != nil {
			r.Plans = []apiPlan{*r.Plan}
		}
		if len(r.Plans) == 0 {
			fmt.Fprintln(e.Stdout, "No plans.")
		}
		for _, p := range r.Plans {
			fmt.Fprintf(e.Stdout, "%s  %s  %s  %d of %d finished (%d handed off)\n", p.ID, p.State, p.Title, p.Done, p.Size, len(p.Members))
			for _, m := range p.Members {
				fmt.Fprintf(e.Stdout, "  %s  %s  %s/%s on %s: %s (%s)\n", m.ID, m.Outcome, dash(m.RepoName), m.Branch, m.Host, m.Status, map[bool]string{true: "report captured", false: "no report yet"}[m.Reported])
			}
		}
		return nil
	}
	return usageErr("want create, show ID or list")
}

// stringList is a repeatable, comma-separable flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func cmdRelease(e *Env, args []string) error { return queuedAction(e, args, "release") }
func cmdCancel(e *Env, args []string) error  { return queuedAction(e, args, "cancel") }

// queuedAction is release or cancel: both act on a waiting handoff by id.
func queuedAction(e *Env, args []string, verb string) error {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want exactly one handoff ID (see `comebye ledger`)")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	data, err := c.raw("POST", "/api/handoffs/"+url.PathEscape(pos[0])+"/"+verb, nil, map[string]string{})
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Entry apiEntry `json:"entry"`
	}
	_ = json.Unmarshal(data, &r)
	if verb == "cancel" {
		fmt.Fprintf(e.Stdout, "Cancelled %s (%s).\n", r.Entry.Branch, r.Entry.ID)
	} else {
		fmt.Fprintf(e.Stdout, "Started %s on %s (%s, %s).\n", r.Entry.Branch, r.Entry.Host, dash(r.Entry.AgentName), r.Entry.AgentID)
	}
	return nil
}

func cmdReport(e *Env, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	progress := fs.Bool("progress", false, "send a progress note, not a report: it never finishes or lands anything")
	phase := fs.String("phase", "", "with --progress, the step of landing you are on: rebase, tests, push or ci")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want report FILE (or - for stdin)")
	}
	if *phase != "" && !*progress {
		return usageErr("--phase goes with --progress")
	}
	if *progress {
		return cmdProgress(e, pos[0], *phase, *asJSON)
	}
	text, err := readInput(e, pos[0], 64<<10)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return usageErr("the report is empty")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	data, err := c.raw("POST", "/api/report", nil, map[string]string{"text": text})
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Entry apiEntry `json:"entry"`
	}
	_ = json.Unmarshal(data, &r)
	if probs := reportProblems(text); len(probs) > 0 {
		fmt.Fprintf(e.Stderr, "warning: the report is not in the handoff's format: %s. It was recorded anyway; send a corrected one with `comebye report -` (it replaces this one) unless you were told to use another format.\n", strings.Join(probs, "; "))
		fmt.Fprintf(e.Stdout, "Report recorded for %s (%s).\n", r.Entry.Branch, r.Entry.ID)
		return nil
	}
	fmt.Fprintf(e.Stdout, "Report recorded for %s (%s). You can stop now.\n", r.Entry.Branch, r.Entry.ID)
	return nil
}

// cmdProgress sends a progress note: what the worker is doing now. The hub
// keeps it for the card ("landing: tests running") and does not take it as
// a report, so it is the right thing to send while a long step runs.
func cmdProgress(e *Env, src, phase string, asJSON bool) error {
	text, err := readInput(e, src, 4<<10)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return usageErr("the note is empty")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	data, err := c.raw("POST", "/api/report", nil, map[string]any{"text": text, "progress": true, "phase": phase})
	if err != nil {
		return err
	}
	if asJSON {
		return rawJSON(e, data)
	}
	fmt.Fprintln(e.Stdout, "Progress noted. It is not a report: nothing is finished or landed by it. Keep going, and send your report with `comebye report -` when you are done.")
	return nil
}

// reportSections are the final report's sections, and verifyGroups the
// Verification section's groups (docs/lifecycle.md).
var (
	reportSections = []string{"Summary", "Changes", "Verification", "Follow-ups", "Decisions for the user"}
	verifyGroups   = []string{"Verified live", "Unit tests only", "Not verified"}
)

// reportProblems lists what a report lacks of the fixed format: a
// "## <section>" heading for each section, and each Verification group
// named inside the Verification section.
func reportProblems(text string) []string {
	headings := map[string]int{} // lower-cased "## " heading -> line index
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if h, ok := strings.CutPrefix(strings.TrimSpace(l), "## "); ok {
			headings[strings.ToLower(strings.TrimSpace(h))] = i
		}
	}
	var missing []string
	for _, s := range reportSections {
		if _, ok := headings[strings.ToLower(s)]; !ok {
			missing = append(missing, s)
		}
	}
	var probs []string
	if len(missing) > 0 {
		probs = append(probs, "missing ## "+strings.Join(missing, ", ## "))
	}
	if at, ok := headings["verification"]; ok {
		var sec []string
		for _, l := range lines[at+1:] {
			if strings.HasPrefix(strings.TrimSpace(l), "## ") {
				break
			}
			sec = append(sec, l)
		}
		body := strings.ToLower(strings.Join(sec, "\n"))
		var gone []string
		for _, g := range verifyGroups {
			if !strings.Contains(body, strings.ToLower(g)) {
				gone = append(gone, g)
			}
		}
		if len(gone) > 0 {
			probs = append(probs, "Verification does not name the group(s) "+strings.Join(gone, ", ")+" (group it as Verified live / Unit tests only / Not verified; say \"none\" for an empty group)")
		}
	}
	return probs
}

// activeStatus says whether a ledger status still has a session to track.
func activeStatus(s string) bool {
	switch s {
	case "running", "finished", "landing", "landed", "ready_to_remove":
		return true
	}
	return false
}

func shortSHAs(shas []string) []string {
	out := make([]string, len(shas))
	for i, s := range shas {
		out[i] = s[:min(7, len(s))]
	}
	return out
}

func cmdUnmark(e *Env, args []string) error {
	fs := flag.NewFlagSet("unmark", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want exactly one AGENT")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	data, err := c.raw("POST", "/api/marks/clear", nil, map[string]string{"agent_id": t.ID})
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Cleared bool `json:"cleared"`
	}
	_ = json.Unmarshal(data, &r)
	if r.Cleared {
		fmt.Fprintf(e.Stdout, "Unmarked %s.\n", t.label())
	} else {
		fmt.Fprintf(e.Stdout, "%s was not marked.\n", t.label())
	}
	return nil
}

// ---- brief ----

func cmdBrief(e *Env, args []string) error {
	fs := flag.NewFlagSet("brief", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	every := fs.String("every", "", "schedule: brief every D (a duration: 24h, 12h; 1h to 720h)")
	at := fs.String("at", "", "schedule: at this time of day (HH:MM; none for no time of day)")
	tz := fs.String("tz", "", "schedule: the time zone of --at (IANA, e.g. Europe/Berlin; default the hub's)")
	skip := fs.String("skip", "", "schedule: on (default) skips a slot when nothing a briefing reports changed; off asks at every slot")
	maxQuiet := fs.String("max-quiet", "", "schedule: with --skip on, ask anyway when the last briefing is this old (default 6h, 1h or more)")
	off := fs.Bool("off", false, "schedule: turn scheduled briefings off")
	dismiss := fs.Bool("dismiss", false, "schedule: dismiss the note about missed scheduled briefings")
	noCheck := fs.Bool("no-check", false, "set: publish even if a \"Needs you\" line has no id of an active to-do item")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr("want set FILE|-, get or schedule")
	}
	setsSchedule := *every != "" || *at != "" || *tz != "" || *skip != "" || *maxQuiet != "" || *off || *dismiss
	if setsSchedule && pos[0] != "schedule" {
		return usageErr("--every, --at, --tz, --skip, --max-quiet, --off and --dismiss go with brief schedule")
	}
	if *noCheck && pos[0] != "set" {
		return usageErr("--no-check goes with brief set")
	}
	if *skip != "" && *skip != "on" && *skip != "off" {
		return usageErr("--skip is on or off")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	switch pos[0] {
	case "schedule":
		if len(pos) != 1 {
			return usageErr("brief schedule takes flags only")
		}
		return briefSchedule(e, c, *asJSON, *every, *at, *tz, *skip, *maxQuiet, *off, *dismiss)
	case "set":
		if len(pos) != 2 {
			return usageErr("want brief set FILE (or - for stdin)")
		}
		text, err := readInput(e, pos[1], 64<<10)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return usageErr("the briefing is empty")
		}
		body := map[string]any{"markdown": text}
		if *noCheck {
			body["no_check"] = true
		}
		data, err := c.raw("POST", "/api/brief", nil, body)
		if err != nil {
			return err
		}
		if *asJSON {
			return rawJSON(e, data)
		}
		fmt.Fprintln(e.Stdout, "Published the briefing; it shows at the top of the app.")
		var r struct {
			Warnings []string `json:"warnings"`
		}
		if json.Unmarshal(data, &r) == nil {
			for _, w := range r.Warnings {
				fmt.Fprintf(e.Stdout, "Warning: %s\n", w)
			}
		}
		return nil
	case "get":
		if len(pos) != 1 {
			return usageErr("brief get takes no arguments")
		}
		data, err := c.raw("GET", "/api/brief", nil, nil)
		if err != nil {
			return err
		}
		if *asJSON {
			return rawJSON(e, data)
		}
		var r struct {
			Brief *struct {
				Markdown string    `json:"markdown"`
				By       string    `json:"by"`
				At       time.Time `json:"at"`
			} `json:"brief"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("unexpected answer: %v", err)
		}
		if r.Brief == nil {
			fmt.Fprintln(e.Stdout, "(no briefing published yet)")
			return nil
		}
		fmt.Fprintf(e.Stdout, "Briefing from %s, %s:\n\n%s\n", dash(r.Brief.By), ago(e, r.Brief.At), strings.TrimRight(r.Brief.Markdown, "\n"))
		return nil
	}
	return usageErr("unknown brief action %q (want set, get or schedule)", pos[0])
}

// briefSched is the hub's schedule as GET /api/brief/schedule has it.
type briefSched struct {
	Enabled    bool       `json:"enabled"`
	Every      string     `json:"every"`
	At         string     `json:"at"`
	TZ         string     `json:"tz"`
	Always     bool       `json:"always"`
	MaxQuiet   string     `json:"max_quiet"`
	CheckedAt  *time.Time `json:"checked_at"`
	By         string     `json:"by"`
	Updated    time.Time  `json:"updated"`
	State      string     `json:"state"`
	Next       *time.Time `json:"next"`
	PromptedAt *time.Time `json:"prompted_at"`
	PromptedTo string     `json:"prompted_to"`
	Missed     *struct {
		ID        string    `json:"id"`
		Since     time.Time `json:"since"`
		Count     int       `json:"count"`
		Reason    string    `json:"reason"`
		Dismissed bool      `json:"dismissed"`
	} `json:"missed"`
}

// briefSchedule shows the schedule, or sets it (flags not given keep
// their current values), or turns it off, or dismisses missed runs.
func briefSchedule(e *Env, c *client, asJSON bool, every, at, tz, skip, maxQuiet string, off, dismiss bool) error {
	get := func() (*briefSched, []byte, error) {
		data, err := c.raw("GET", "/api/brief/schedule", nil, nil)
		if err != nil {
			return nil, nil, err
		}
		var r struct {
			Schedule briefSched `json:"schedule"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, nil, fmt.Errorf("unexpected answer: %v", err)
		}
		return &r.Schedule, data, nil
	}
	cur, data, err := get()
	if err != nil {
		return err
	}
	switch {
	case off && (every != "" || at != "" || tz != "" || skip != "" || maxQuiet != ""):
		return usageErr("--off takes no --every, --at, --tz, --skip or --max-quiet")
	case dismiss:
		if cur.Missed == nil {
			return fmt.Errorf("no missed scheduled briefings to dismiss")
		}
		if _, err := c.raw("POST", "/api/brief/schedule/dismiss", nil, map[string]string{"id": cur.Missed.ID}); err != nil {
			return err
		}
	case off:
		if _, err := c.raw("POST", "/api/brief/schedule", nil, map[string]any{"enabled": false}); err != nil {
			return err
		}
	case every != "" || at != "" || tz != "" || skip != "" || maxQuiet != "":
		if every == "" {
			every = cur.Every
		}
		if every == "" {
			return usageErr("--every is required the first time (e.g. --every 24h)")
		}
		switch at {
		case "":
			at = cur.At
		case "none":
			at = ""
		}
		if tz == "" {
			tz = cur.TZ
		}
		always := cur.Always
		switch skip {
		case "on":
			always = false
		case "off":
			always = true
		}
		if maxQuiet == "" {
			maxQuiet = cur.MaxQuiet
		}
		if _, err := c.raw("POST", "/api/brief/schedule", nil, map[string]any{"enabled": true, "every": every, "at": at, "tz": tz, "always": always, "max_quiet": maxQuiet}); err != nil {
			return err
		}
	}
	if dismiss || off || every != "" || at != "" || tz != "" || skip != "" || maxQuiet != "" {
		if cur, data, err = get(); err != nil {
			return err
		}
	}
	if asJSON {
		return rawJSON(e, data)
	}
	when := func(t time.Time) string {
		if cur.TZ != "" {
			if loc, err := time.LoadLocation(cur.TZ); err == nil {
				t = t.In(loc)
			}
		}
		return t.Format("Mon Jan 2 15:04 MST")
	}
	if !cur.Enabled {
		fmt.Fprint(e.Stdout, "Scheduled briefings are off.")
		if cur.Every != "" {
			fmt.Fprintf(e.Stdout, " (Last set to every %s%s; turn on again with brief schedule --every %s.)", cur.Every, atText(cur.At, cur.TZ), cur.Every)
		}
		fmt.Fprintln(e.Stdout)
		return nil
	}
	fmt.Fprintf(e.Stdout, "Scheduled briefings: every %s%s, set by %s %s.\n", cur.Every, atText(cur.At, cur.TZ), dash(cur.By), ago(e, cur.Updated))
	if cur.Always {
		fmt.Fprintln(e.Stdout, "Asks at every slot, changed or not (--skip off).")
	} else {
		mq := cur.MaxQuiet
		if mq == "" {
			mq = "6h"
		}
		fmt.Fprintf(e.Stdout, "Skips a slot when nothing a briefing reports changed, but asks when the last briefing is %s old (--skip, --max-quiet).\n", mq)
	}
	if cur.CheckedAt != nil {
		fmt.Fprintf(e.Stdout, "Checked %s: nothing changed, no coordinator asked.\n", ago(e, *cur.CheckedAt))
	}
	if cur.Next != nil {
		fmt.Fprintf(e.Stdout, "Next: %s.\n", when(*cur.Next))
	}
	switch cur.State {
	case "prompted":
		fmt.Fprintf(e.Stdout, "Asked %s for this one %s; waiting for it to publish.\n", dash(cur.PromptedTo), ago(e, *cur.PromptedAt))
	case "waiting":
		fmt.Fprintln(e.Stdout, "Due now; waiting for a coordinator to be idle.")
	}
	if m := cur.Missed; m != nil {
		dis := ""
		if m.Dismissed {
			dis = " (dismissed)"
		}
		fmt.Fprintf(e.Stdout, "Missed %d scheduled briefing(s) since %s: %s%s.\n", m.Count, when(m.Since), missText(m.Reason), dis)
	}
	return nil
}

func atText(at, tz string) string {
	switch {
	case at == "":
		return ""
	case tz == "":
		return " at " + at
	}
	return " at " + at + " " + tz
}

func missText(reason string) string {
	switch reason {
	case "no_coordinator":
		return "no coordinator was running"
	case "busy":
		return "the coordinator stayed busy"
	case "no_briefing":
		return "the coordinator was asked but published nothing"
	}
	return reason
}

// ---- playbook / presets (local, no server) ----

func cmdPlaybook(e *Env, args []string) error {
	fs := flag.NewFlagSet("playbook", flag.ContinueOnError)
	harness := fs.String("harness", "generic", "pi, claude or generic")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	text, err := playbook.Render(*harness)
	if err != nil {
		return usageErr("%v", err)
	}
	_, err = io.WriteString(e.Stdout, text)
	return err
}

func cmdPresets(e *Env, args []string) error {
	fs := flag.NewFlagSet("presets", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want one harness: claude, pi or generic")
	}
	text, err := playbook.Preset(pos[0])
	if err != nil {
		return usageErr("%v", err)
	}
	if _, err := io.WriteString(e.Stdout, text); err != nil {
		return err
	}
	if note := playbook.PresetNote(pos[0]); note != "" {
		fmt.Fprint(e.Stderr, note)
	}
	return nil
}

// ---- whoami ----

func cmdWhoami(e *Env, args []string) error {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	w, err := c.getWhoami()
	if err != nil {
		return err
	}
	if *asJSON {
		return jsonOut(e, map[string]any{
			"agent": w.Agent, "by": w.By, "host": w.Host, "role": w.Role, "hub": w.Hub, "public_url": w.PublicURL, "ledger": w.Ledger, "version": w.Version,
			"pane": c.pane, "url": c.base, "token_file": c.tokenPath,
		})
	}
	who := w.By
	if who == "" {
		who = "an agent"
	}
	fmt.Fprintf(e.Stdout, "You are %s, with the %s role, on host %s.\n", who, w.Role, w.Host)
	if c.pane == "" {
		fmt.Fprintln(e.Stdout, "HERDR_PANE_ID is not set: you are not in a herdr pane, so your actions are not attributed to an agent.")
	}
	switch w.Hub {
	case "self":
		fmt.Fprintln(e.Stdout, "This Comebye is the hub: it keeps the ledger.")
	case "":
		fmt.Fprintln(e.Stdout, "This Comebye keeps no ledger and has no hub configured (-hub): handoffs, marks and briefings will fail.")
	default:
		fmt.Fprintf(e.Stdout, "The ledger is on the hub at %s; this Comebye forwards to it.\n", w.Hub)
	}
	fmt.Fprintf(e.Stdout, "Comebye: %s (token %s)\n", c.base, c.tokenPath)
	if w.Version != "" {
		fmt.Fprintf(e.Stdout, "Version: %s\n", w.Version)
	}
	return nil
}

// ---- models ----

// thinkingLevels are pi's --thinking values (the server checks them).
var thinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// modelLabel is "model", "model (thinking high)" or "" for the defaults.
func modelLabel(model, thinking string) string {
	switch {
	case model != "" && thinking != "":
		return model + " (thinking " + thinking + ")"
	case model != "":
		return model
	case thinking != "":
		return "(thinking " + thinking + ")"
	}
	return ""
}

type apiCatalog struct {
	Kind   string `json:"kind"`
	Models []struct {
		ID       string `json:"id"`
		Thinking bool   `json:"thinking"`
		Note     string `json:"note"`
	} `json:"models"`
	Default         string   `json:"default"`
	DefaultThinking string   `json:"default_thinking"`
	Levels          []string `json:"thinking_levels"`
	Source          string   `json:"source"`
	Exact           bool     `json:"exact"`
	Error           string   `json:"error"`
}

func cmdModels(e *Env, args []string) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	host := fs.String("host", "", "the host (default: this one)")
	kind := fs.String("kind", "", "only this agent kind (default: every installed kind that takes a model)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	path := "/api/models"
	if *host != "" {
		path = "/api/hosts/" + url.PathEscape(*host) + "/models"
	}
	q := url.Values{}
	if *kind != "" {
		q.Set("kind", *kind)
	}
	data, err := c.raw("GET", path, q, nil)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Host     string       `json:"host"`
		Catalogs []apiCatalog `json:"catalogs"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("unexpected answer: %v", err)
	}
	if len(r.Catalogs) == 0 {
		fmt.Fprintf(e.Stdout, "(no installed agent kind on %s takes a model)\n", dash(r.Host))
		return nil
	}
	for i, ct := range r.Catalogs {
		if i > 0 {
			fmt.Fprintln(e.Stdout)
		}
		def := "the agent's own default"
		if ct.Default != "" {
			def = ct.Default
		}
		if ct.DefaultThinking != "" {
			def += ", thinking " + ct.DefaultThinking
		}
		fmt.Fprintf(e.Stdout, "%s on %s (source: %s; default: %s)\n", ct.Kind, dash(r.Host), ct.Source, def)
		if ct.Error != "" {
			fmt.Fprintf(e.Stdout, "  warning: %s\n", ct.Error)
		}
		for _, m := range ct.Models {
			line := "  " + m.ID
			if m.ID == ct.Default {
				line += "  (default)"
			}
			if m.Note != "" {
				line += "  " + m.Note
			}
			fmt.Fprintln(e.Stdout, line)
		}
		if len(ct.Models) == 0 {
			fmt.Fprintln(e.Stdout, "  (no list: any model name the agent accepts may be used)")
		} else if !ct.Exact {
			fmt.Fprintln(e.Stdout, "  (not exhaustive: other names the agent accepts may be used)")
		}
		if len(ct.Levels) > 0 {
			fmt.Fprintf(e.Stdout, "  thinking levels: %s\n", strings.Join(ct.Levels, ", "))
		}
	}
	return nil
}

// cmdModel switches a running agent's model.
func cmdModel(e *Env, args []string) error {
	fs := flag.NewFlagSet("model", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	thinking := fs.String("thinking", "", "pi's thinking level: "+strings.Join(thinkingLevels, ", "))
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 || (len(pos) == 1 && *thinking == "") {
		return usageErr("want AGENT and a MODEL (pi: or just --thinking)")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	body := map[string]string{}
	if len(pos) == 2 {
		body["model"] = pos[1]
	}
	if *thinking != "" {
		body["thinking"] = *thinking
	}
	data, err := c.raw("POST", hostPath(t, "model"), nil, body)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	fmt.Fprintf(e.Stdout, "Switched %s to %s (sent %s).\n", t.label(), modelLabel(body["model"], body["thinking"]), switchSent(data))
	return nil
}

func switchSent(data []byte) string {
	var r struct {
		Commands []string `json:"commands"`
	}
	json.Unmarshal(data, &r)
	return strings.Join(r.Commands, ", ")
}

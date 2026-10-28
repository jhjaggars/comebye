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
		{"handoff", "--host H --repo PATH --branch B [--base X] [--kind K] [--name N] --brief FILE|- [--plan ID] [--json]", "hand work to a new worker on its own branch and worktree", cmdHandoff},
		{"plan", "create --title T --size N | show ID | list [--json]", "group handoffs into one change: the user is told once when all of them are done", cmdPlan},
		{"report", "FILE|-", "(workers) declare yourself done and send your final report to whoever handed you the task", cmdReport},
		{"prompt", "AGENT TEXT|- [--json]", "send an agent a message", cmdPrompt},
		{"mark", "AGENT --reason R [--discard] [--no-ask] [--wait] [--json]", "propose removing a session, or discarding its work (asks the session first; the user confirms in the app)", cmdMark},
		{"unmark", "AGENT [--json]", "withdraw a removal proposal", cmdUnmark},
		{"brief", "set FILE|- | get | schedule [--every D] [--at HH:MM] [--tz ZONE] [--off] [--dismiss] [--json]", "publish or read the briefing shown in the app; show or set its schedule", cmdBrief},
		{"playbook", "[--harness pi|claude|generic]", "print the coordinator playbook", cmdPlaybook},
		{"presets", "claude|pi|generic", "print how to let a harness run comebye without prompts", cmdPresets},
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
	ID      string `json:"id"`
	Host    string `json:"host"`
	PaneID  string `json:"pane_id"`
	Name    string `json:"name,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Status  string `json:"status"`
	Project string `json:"project,omitempty"`
	Branch  string `json:"branch,omitempty"`
	CWD     string `json:"cwd,omitempty"`
	// Role: coordinator, worker (a ledger handoff or a linked worktree),
	// home (a repository's primary checkout) or "".
	Role   string `json:"role,omitempty"`
	Ledger string `json:"ledger_status,omitempty"`
	Self   bool   `json:"self,omitempty"`
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
			if en.AgentID != "" && (en.Status == "running" || en.Status == "finished" || en.Status == "ready_to_remove") {
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
		r := agentRow{ID: a.ID, Host: a.Host, PaneID: a.PaneID, Name: a.Name, Kind: a.Agent, Status: a.Status, CWD: a.CWD}
		en, isEntry := entries[a.ID]
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
	fmt.Fprintln(tw, "AGENT\tNAME\tKIND\tSTATUS\tPROJECT\tBRANCH\tROLE")
	for _, r := range rows {
		role := r.Role
		if r.Ledger != "" && r.Ledger != "running" {
			role += " (" + r.Ledger + ")"
		}
		if r.Self {
			role = strings.TrimSpace(role + " (you)")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, dash(r.Name), dash(r.Kind), r.Status, dash(r.Project), dash(r.Branch), dash(role))
	}
	tw.Flush()
	for _, h := range ar.Hosts {
		if !h.Local && h.Status != "ok" && (*hostF == "" || *hostF == h.Name) {
			fmt.Fprintf(e.Stdout, "host %s is %s%s: its agents are not listed\n", h.Name, h.Status, map[bool]string{true: " (" + h.Error + ")", false: ""}[h.Error != ""])
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.Stdout, "(no agents)")
	}
	return nil
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
	fmt.Fprintln(tw, "ENTRY\tSTATUS\tHOST\tREPO\tBRANCH\tWORKER\tREPORT\tNOTES")
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
		case en.Status == "removed" || en.Status == "failed":
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
		} else if en.Checks != nil && en.Status == "finished" {
			if en.Checks.Dirty {
				notes = append(notes, "uncommitted changes")
			}
			if en.Checks.Unmerged > 0 {
				notes = append(notes, fmt.Sprintf("not merged: %d commits not in %s", en.Checks.Unmerged, dash(en.Checks.BaseUsed)))
			}
		}
		if en.PlanID != "" {
			notes = append(notes, "plan "+en.PlanID)
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", en.ID, en.Status, en.Host, dash(en.RepoName), en.Branch, dash(worker), report, strings.Join(notes, "; "))
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
	AgentID string `json:"agent_id"`
	Host    string `json:"host"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	EntryID string `json:"entry_id"`
	Checks  struct {
		Path       string `json:"path"`
		Exists     bool   `json:"exists"`
		Primary    bool   `json:"primary"`
		Branch     string `json:"branch"`
		Dirty      bool   `json:"dirty"`
		Unmerged   int    `json:"unmerged_commits"`
		BaseUsed   string `json:"base_used"`
		Tip        string `json:"tip"`
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
	switch {
	case !ch.Exists:
		fmt.Fprintf(w, "checkout: %s does not exist (any more)\n", ch.Path)
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
		fmt.Fprintf(w, "merged:   yes · every commit is in %s\n", base)
	default:
		fmt.Fprintf(w, "merged:   NO · %d commits not in %s (a squash-merged PR still counts here: check the PR)\n", ch.Unmerged, base)
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
	kind := fs.String("kind", "", "the agent kind (default: the host's most used)")
	name := fs.String("name", "", "the worker's name (default: from the branch)")
	brief := fs.String("brief", "", "the brief: a markdown file, or - for stdin")
	plan := fs.String("plan", "", "join this plan (from `comebye plan create`)")
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
	body := map[string]string{"host": *host, "repo": *repo, "branch": *branch, "brief": text}
	for k, v := range map[string]string{"base": *base, "kind": *kind, "name": *name, "plan": *plan} {
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
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("unexpected answer: %v", err)
	}
	en := r.Entry
	switch {
	case r.Restarted:
		fmt.Fprintf(e.Stdout, "The earlier worker on %s was gone: started %s (%s) in its worktree.\n", en.Branch, dash(en.AgentName), en.AgentID)
	case r.Existing:
		fmt.Fprintf(e.Stdout, "Already handed off: %s on %s is %s (%s, %s). Nothing new was started; use `comebye prompt` to give it more work.\n", en.Branch, en.Host, en.Status, dash(en.AgentName), en.AgentID)
	default:
		fmt.Fprintf(e.Stdout, "Handed off %s on %s to %s (%s).\n", en.Branch, en.Host, dash(en.AgentName), en.AgentID)
	}
	fmt.Fprintf(e.Stdout, "  entry:    %s\n  worktree: %s\n  base:     %s\n", en.ID, dash(en.Worktree), dash(en.Base))
	if en.Error != "" {
		fmt.Fprintf(e.Stdout, "  warning:  %s\n", en.Error)
	}
	return nil
}

// ---- prompt ----

func cmdPrompt(e *Env, args []string) error {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
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
	data, err := c.raw("POST", hostPath(t, "prompt"), nil, map[string]string{"text": text})
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	fmt.Fprintf(e.Stdout, "Sent to %s.\n", t.label())
	return nil
}

// ---- mark / unmark ----

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

func cmdReport(e *Env, args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want report FILE (or - for stdin)")
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
	fmt.Fprintf(e.Stdout, "Report recorded for %s (%s). You can stop now.\n", r.Entry.Branch, r.Entry.ID)
	return nil
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
	off := fs.Bool("off", false, "schedule: turn scheduled briefings off")
	dismiss := fs.Bool("dismiss", false, "schedule: dismiss the note about missed scheduled briefings")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr("want set FILE|-, get or schedule")
	}
	setsSchedule := *every != "" || *at != "" || *tz != "" || *off || *dismiss
	if setsSchedule && pos[0] != "schedule" {
		return usageErr("--every, --at, --tz, --off and --dismiss go with brief schedule")
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
		return briefSchedule(e, c, *asJSON, *every, *at, *tz, *off, *dismiss)
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
		data, err := c.raw("POST", "/api/brief", nil, map[string]string{"markdown": text})
		if err != nil {
			return err
		}
		if *asJSON {
			return rawJSON(e, data)
		}
		fmt.Fprintln(e.Stdout, "Published the briefing; it shows at the top of the app.")
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
func briefSchedule(e *Env, c *client, asJSON bool, every, at, tz string, off, dismiss bool) error {
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
	case off && (every != "" || at != "" || tz != ""):
		return usageErr("--off takes no --every, --at or --tz")
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
	case every != "" || at != "" || tz != "":
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
		if _, err := c.raw("POST", "/api/brief/schedule", nil, map[string]any{"enabled": true, "every": every, "at": at, "tz": tz}); err != nil {
			return err
		}
	}
	if dismiss || off || every != "" {
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
			"agent": w.Agent, "by": w.By, "host": w.Host, "role": w.Role, "hub": w.Hub, "ledger": w.Ledger,
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
	return nil
}

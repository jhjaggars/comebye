package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jhjaggars/comebye/internal/digest"
)

// Several coordinators (docs/agents.md "Several coordinators"): each has its
// own notes file and its own digest; the main one keeps the original path.

// coordNameRe is what a coordinator's name must look like to name a
// directory (the hub's agent-name rule).
var coordNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// coordinatorsDir holds the other coordinators' directories: <state>/coordinators/<name>/.
func coordinatorsDir(e *Env) string { return filepath.Join(StateDir(e.Getenv), "coordinators") }

// coordStatePath is the coordinator's notes file: the main coordinator's is
// the one every install already has (<state>/coordinator/state.md, so an
// upgrade loses no notes); another's is <state>/coordinators/<name>/state.md.
func coordStatePath(e *Env, name string, main bool) string {
	if main || !coordNameRe.MatchString(name) {
		return filepath.Join(coordinatorDir(e), stateFileName)
	}
	return filepath.Join(coordinatorsDir(e), name, stateFileName)
}

// apiCoordinator is one entry of GET /api/coordinators.
type apiCoordinator struct {
	ID       string    `json:"id"`
	Host     string    `json:"host"`
	PaneID   string    `json:"pane_id"`
	Name     string    `json:"name"`
	Main     bool      `json:"main"`
	Status   string    `json:"status"`
	At       time.Time `json:"at"`
	Handoffs []string  `json:"handoffs"`
	Closed   []string  `json:"closed"`
	Todos    []string  `json:"todos"`
	Plans    []string  `json:"plans"`
}

func (c *client) getCoordinators() ([]apiCoordinator, error) {
	var r struct {
		Coordinators []apiCoordinator `json:"coordinators"`
	}
	if err := c.call("GET", "/api/coordinators", nil, nil, &r); err != nil {
		return nil, err
	}
	return r.Coordinators, nil
}

// coordScope is what one coordinator owns, for filtering the digest.
type coordScope struct {
	self     apiCoordinator
	mine     map[string]bool // ids of its handoffs, to-dos and plans
	foreign  map[string]bool // the others'
	others   []apiCoordinator
	scoped   bool // more than one coordinator: the digest keeps to its own
	notFound bool
}

// scopeFor finds agent (a qualified id) among coordinators. Alone, or not
// found (not a recorded coordinator, an old hub): nothing is filtered.
func scopeFor(cs []apiCoordinator, agent string) coordScope {
	sc := coordScope{mine: map[string]bool{}, foreign: map[string]bool{}}
	found := false
	for _, c := range cs {
		if c.ID == agent {
			sc.self, found = c, true
		}
	}
	if !found {
		sc.notFound = true
		return sc
	}
	for _, c := range cs {
		ids := append(append(append(append([]string(nil), c.Handoffs...), c.Closed...), c.Todos...), c.Plans...)
		if c.ID == sc.self.ID {
			for _, id := range ids {
				sc.mine[id] = true
			}
			continue
		}
		sc.others = append(sc.others, c)
		for _, id := range ids {
			sc.foreign[id] = true
		}
	}
	sc.scoped = len(sc.others) > 0
	return sc
}

// keepEvent says whether a hub-log event (about subject) is this
// coordinator's: its own work's, or, for main, what no other coordinator owns.
func (sc coordScope) keepEvent(subject string) bool {
	return sc.mine[subject] || (sc.self.Main && !sc.foreign[subject])
}

// othersSummary is one line per other coordinator.
func (sc coordScope) othersSummary() string {
	var lines []string
	for _, o := range sc.others {
		tag := ""
		if o.Main {
			tag = " (main)"
		}
		lines = append(lines, fmt.Sprintf("- %s%s: %d handoffs, %d to-dos, %d plans open", orDashStr(o.Name), tag, len(o.Handoffs), len(o.Todos), len(o.Plans)))
	}
	return strings.Join(lines, "\n")
}

func orDashStr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// applyScope narrows the digest's input to the coordinator's own work.
func applyScope(in *digest.Input, sc coordScope, plans []apiPlan) {
	in.Name, in.Main, in.Scoped = sc.self.Name, sc.self.Main, true
	in.Others = sc.othersSummary()
	todos := in.Todos[:0:0]
	for _, t := range in.Todos {
		if sc.mine[t.ID] {
			todos = append(todos, t)
		}
	}
	in.Todos = todos
	hs := in.Handoffs[:0:0]
	for _, h := range in.Handoffs {
		if sc.mine[h.ID] {
			hs = append(hs, h)
		}
	}
	in.Handoffs = hs
	for _, p := range plans {
		if sc.mine[p.ID] {
			in.Plans = append(in.Plans, digest.Plan{ID: p.ID, Title: p.Title, Size: p.Size, Done: p.Done})
		}
	}
	for i, l := range in.Board {
		kept := l.Items[:0:0]
		for _, it := range l.Items {
			if it.ID == "" || sc.keepEvent(it.ID) {
				kept = append(kept, it)
			}
		}
		in.Board[i].Count -= len(l.Items) - len(kept)
		if in.Board[i].Count < len(kept) {
			in.Board[i].Count = len(kept)
		}
		in.Board[i].Items = kept
	}
}

// ---- list ----

func cmdCoordList(e *Env, args []string) error {
	fs := flag.NewFlagSet("coordinator list", flag.ContinueOnError)
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
	cs, err := c.getCoordinators()
	if err != nil {
		return err
	}
	me := ""
	if w, err := c.getWhoami(); err == nil {
		me = w.Agent
	}
	if *asJSON {
		type row struct {
			apiCoordinator
			StateFile string `json:"state_file"`
			You       bool   `json:"you,omitempty"`
		}
		rows := make([]row, 0, len(cs))
		for _, co := range cs {
			rows = append(rows, row{co, coordStatePath(e, co.Name, co.Main), co.ID == me})
		}
		return jsonOut(e, map[string]any{"coordinators": rows})
	}
	if len(cs) == 0 {
		fmt.Fprintln(e.Stdout, "no coordinator is registered")
		return nil
	}
	tw := tabwriter.NewWriter(e.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tMAIN\tHOST/PANE\tSTATUS\tHANDOFFS\tTO-DOS\tPLANS\tSTATE FILE")
	for _, co := range cs {
		name := dash(co.Name)
		if co.ID == me {
			name += " *"
		}
		mainMark := ""
		if co.Main {
			mainMark = "main"
		}
		state := coordStatePath(e, co.Name, co.Main)
		if co.Host != "" && onOtherHost(c, co.Host) {
			state += " (on " + co.Host + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\n", name, mainMark, firstNonEmpty(co.ID, co.Host+"/"+co.PaneID), dash(co.Status), len(co.Handoffs), len(co.Todos), len(co.Plans), state)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if me != "" {
		fmt.Fprintln(e.Stdout, "\n* you. Open counts are each coordinator's own (work nobody owns counts as main's).")
	}
	return nil
}

// onOtherHost reports whether host is not this CLI's own host (so a path printed for
// it is on another machine). Unknown: false.
func onOtherHost(c *client, host string) bool {
	me, err := c.getWhoami()
	return err == nil && me.Host != "" && host != me.Host
}

// ---- new ----

func cmdCoordNew(e *Env, args []string) error {
	fs := flag.NewFlagSet("coordinator new", flag.ContinueOnError)
	host := fs.String("host", "", "the host to start it on (default: the host COMEBYE_URL talks to)")
	dir := fs.String("dir", "", "the directory it works in, an absolute path inside the host's launch roots (default <state>/coordinators/NAME on that host)")
	kind := fs.String("kind", "", "the agent kind (default: the host's default kind)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	login := fs.String("login", "", "the Tailscale login to act as (default $COMEBYE_LOGIN, else the first of $COMEBYE_TS_ALLOW_LOGIN)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want one name: comebye coordinator new NAME [--host H] [--dir DIR]")
	}
	name := pos[0]
	if !coordNameRe.MatchString(name) {
		return usageErr("a coordinator's name is lowercase letters, digits, - and _, starting with a letter (at most 32), got %q", name)
	}
	if *dir != "" && !filepath.IsAbs(*dir) {
		return usageErr("--dir must be an absolute path on the host that runs it")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	// Starting an agent is the user's alone: an agent token is refused.
	if c.user = peerLogin(e, *login); c.user == "" {
		return usageErr("coordinator new needs the user's login: pass --login you@example.com (or set COMEBYE_LOGIN)")
	}
	body := map[string]any{"coordinator": true, "new_coordinator": true, "name": name}
	if *dir != "" {
		body["dir"] = *dir
	}
	if *kind != "" {
		body["kind"] = *kind
	}
	path := "/api/launch"
	if *host != "" {
		path = "/api/hosts/" + *host + "/launch"
	}
	data, err := c.raw("POST", path, nil, body)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Agent struct {
			ID     string `json:"id"`
			Host   string `json:"host"`
			PaneID string `json:"pane_id"`
			Name   string `json:"name"`
			CWD    string `json:"cwd"`
		} `json:"agent"`
		Warnings    []string `json:"warnings"`
		PromptError string   `json:"prompt_error"`
		PromptNote  string   `json:"prompt_note"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("unexpected answer: %v", err)
	}
	where := firstNonEmpty(r.Agent.ID, r.Agent.Host+"/"+r.Agent.PaneID)
	fmt.Fprintf(e.Stdout, "Started coordinator %s (%s) in %s\n", name, where, dash(r.Agent.CWD))
	fmt.Fprintf(e.Stdout, "  notes:   %s (on %s; it becomes the digest's state file)\n", filepath.Join(coordinatorsDir(e), name, stateFileName), firstNonEmpty(r.Agent.Host, *host, "its host"))
	fmt.Fprintf(e.Stdout, `
Talk to it: open %s in the app (it is pinned with the Coordinators), or in its herdr pane.
Work it hands off, to-dos its workers file and agents it prompts answer to it, not to main.
`, name)
	for _, wn := range r.Warnings {
		fmt.Fprintf(e.Stdout, "warning: %s\n", wn)
	}
	if r.PromptError != "" {
		fmt.Fprintf(e.Stdout, "warning: %s\n", r.PromptError)
	}
	if r.PromptNote != "" {
		fmt.Fprintf(e.Stdout, "note: %s\n", r.PromptNote)
	}
	return nil
}

// scopedLedger keeps the entries that are the coordinator's own (all of them
// when it is not scoped), so its handoffs and events are its alone.
func scopedLedger(lg *ledgerResp, sc coordScope) *ledgerResp {
	if !sc.scoped {
		return lg
	}
	out := *lg
	out.Entries = nil
	for _, en := range lg.Entries {
		if sc.keepEvent(en.ID) {
			out.Entries = append(out.Entries, en)
		}
	}
	return &out
}

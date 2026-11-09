package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/digest"
	"github.com/jhjaggars/comebye/internal/playbook"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// ---- coordinator ----

// hookNote is what the coordinator hook leaves in a session when it starts
// (startup, resume, compact): the hub looks for hookMarker (server/reset.go)
// before it clears the session, so it never clears one that would come back
// blank.
const hookNote = "comebye-hook:v1 — this coordinator session was started with the Comebye reset hook. If the hub clears your context (a \"fresh start\"), it injects a digest of the briefing, to-dos, handoffs and your state file; `comebye coordinator digest` prints it any time."

// stateFileName is the coordinator's own notes, kept beside the hook's
// settings.
const stateFileName = "state.md"

func coordinatorDir(e *Env) string { return filepath.Join(StateDir(e.Getenv), "coordinator") }

func cmdCoordinator(e *Env, args []string) error {
	if len(args) == 0 {
		return usageErr("want digest, install-hook, status or reset")
	}
	switch args[0] {
	case "digest":
		return cmdCoordDigest(e, args[1:])
	case "install-hook":
		return cmdCoordInstall(e, args[1:])
	case "status":
		return cmdCoordStatus(e, args[1:])
	case "reset":
		return cmdCoordReset(e, args[1:])
	}
	return usageErr("unknown coordinator command %q (want digest, install-hook, status or reset)", args[0])
}

// ---- digest ----

func cmdCoordDigest(e *Env, args []string) error {
	fs := flag.NewFlagSet("coordinator digest", flag.ContinueOnError)
	hook := fs.Bool("hook", false, "the SessionStart hook's mode: print the hook's JSON (the digest as additionalContext) for a coordinator, nothing for any other agent; never fails")
	note := fs.Bool("note", false, "with --hook: print the short note a session start leaves instead of the digest")
	harness := fs.String("harness", "claude", "the playbook harness to point at: pi, claude or generic")
	max := fs.Int("max-bytes", digest.MaxBytes, "the digest's size limit")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	if *note && !*hook {
		return usageErr("--note goes with --hook")
	}
	if *hook {
		return coordHook(e, *note, *harness)
	}
	text, _ := buildDigest(e, *harness)
	if *max > 0 && len(text) > *max {
		text = text[:*max]
	}
	_, err = io.WriteString(e.Stdout, text)
	return err
}

// coordHook is the hook's body. Whatever goes wrong, it exits 0 with the
// best digest it can make: a failing hook must not keep a session from
// starting.
func coordHook(e *Env, note bool, harness string) error {
	c, err := newClient(e)
	if err == nil && !isCoordinator(c) {
		return nil // not the coordinator: the hook is for it alone
	}
	var ctx, msg string
	if note {
		ctx = hookNote
	} else {
		text, problems := buildDigest(e, harness)
		ctx = text
		msg = fmt.Sprintf("Comebye: fresh start — your context was cleared and a digest (%.1f KB) was loaded.", float64(len(text))/1024)
		if len(problems) > 0 {
			msg += " Not available: " + strings.Join(problems, "; ") + "."
		}
	}
	out := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": ctx}}
	if msg != "" {
		out["systemMessage"] = msg
	}
	return jsonOut(e, out)
}

// isCoordinator reports whether this agent is a recorded coordinator. When
// the hub cannot be asked it says yes: the hook is installed for the
// coordinator, and a digest it should not get is better than none.
func isCoordinator(c *client) bool {
	me, err := c.getWhoami()
	if err != nil || me.Agent == "" {
		return true
	}
	var r struct {
		Roles map[string]apiRole `json:"roles"`
	}
	if err := c.call("GET", "/api/roles", nil, nil, &r); err != nil {
		return true
	}
	rl, ok := r.Roles[me.Agent]
	return ok && rl.Role == "coordinator"
}

// buildDigest gathers the digest's parts and renders it. problems lists
// what could not be read (the digest says so rather than failing).
func buildDigest(e *Env, harness string) (string, []string) {
	now := e.Now()
	in := digest.Input{Now: now, Harness: harness}
	var problems []string
	c, err := newClient(e)
	if err != nil {
		problems = append(problems, err.Error())
	} else {
		if me, err := c.getWhoami(); err == nil {
			in.Agent = me.Agent
		}
		var cs struct {
			Last *struct {
				At time.Time `json:"at"`
			} `json:"last"`
		}
		if err := c.call("GET", "/api/coordinator", nil, nil, &cs); err != nil {
			problems = append(problems, "last reset ("+err.Error()+")")
		} else if cs.Last != nil {
			in.LastReset = cs.Last.At
		}
		var br struct {
			Brief *struct {
				Markdown string    `json:"markdown"`
				By       string    `json:"by"`
				At       time.Time `json:"at"`
			} `json:"brief"`
		}
		if err := c.call("GET", "/api/brief", nil, nil, &br); err != nil {
			problems = append(problems, "briefing")
		} else if br.Brief != nil {
			in.Brief = &digest.Brief{Markdown: br.Brief.Markdown, By: br.Brief.By, At: br.Brief.At}
		}
		var td struct {
			Todos []apiTodo `json:"todos"`
		}
		if err := c.call("GET", "/api/todos", nil, nil, &td); err != nil {
			problems = append(problems, "to-dos")
		}
		for _, t := range td.Todos {
			d := digest.Todo{ID: t.ID, Kind: t.Kind, State: t.State, Question: t.Question, At: t.CreatedAt}
			if t.Waiting != nil {
				d.Waiting = firstNonEmpty(t.Waiting.Name, t.Waiting.ID)
			}
			if t.Answer != nil {
				d.Answer = "answered " + digest.Age(t.Answer.At, now) + " ago by " + t.Answer.By + (map[bool]string{true: ", to relay", false: ""}[t.State == "answered"])
			}
			in.Todos = append(in.Todos, d)
		}
		if lg, err := c.getLedger(); err != nil {
			problems = append(problems, "ledger")
		} else {
			since := in.LastReset
			if floor := now.Add(-48 * time.Hour); since.IsZero() || since.Before(floor) {
				since = now.Add(-24 * time.Hour)
				if !in.LastReset.IsZero() && in.LastReset.After(floor) {
					since = in.LastReset
				}
			}
			in.Handoffs, in.Events = ledgerDigest(lg, since)
		}
	}
	// The coordinator's own notes, and the memory index it loads.
	sp := filepath.Join(coordinatorDir(e), stateFileName)
	in.StatePath = sp
	if tail, size, err := readTail(sp, 3*digest.StateTailBytes); err == nil {
		in.StateTail, in.StateSize = tail, size
	}
	in.MemoryIndex = memoryIndex(e)
	return digest.Build(in), problems
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// ledgerDigest picks the handoffs that are still going and the events
// since a time (newest 40).
func ledgerDigest(lg *ledgerResp, since time.Time) ([]digest.Handoff, []digest.Event) {
	var hs []digest.Handoff
	var es []digest.Event
	for _, en := range lg.Entries {
		who := en.AgentName
		label := fmt.Sprintf("`%s` %s %s", en.ID, en.RepoName, en.Branch)
		switch en.Status {
		case "waiting", "running", "landing", "finished":
			h := digest.Handoff{ID: en.ID, Repo: en.RepoName, Branch: en.Branch, Worker: who, Host: en.Host, Status: en.Status, Updated: en.CreatedAt}
			if en.FinishedAt != nil {
				h.Updated = *en.FinishedAt
			}
			switch {
			case en.Status == "landing" && en.LandCheck != nil:
				h.State = "landing: " + oneLine(en.LandCheck.Verdict, 100)
			case en.Status == "landing":
				h.State = "approved; the worker is landing it"
			case en.Status == "finished":
				h.State = "reported; waiting for the user's OK to land (see its report: `comebye read " + en.ID + " --report`)"
			case en.Status == "waiting":
				h.State = "queued behind " + strings.Join(en.After, ", ")
			case en.Live.Present:
				h.State = "agent " + en.Live.Status
			default:
				h.State = "agent gone"
			}
			hs = append(hs, h)
		}
		if en.CreatedAt.After(since) {
			es = append(es, digest.Event{At: en.CreatedAt, Text: "handed off " + label + " to " + firstNonEmpty(who, "a worker")})
		}
		if en.FinishedAt != nil && en.FinishedAt.After(since) {
			es = append(es, digest.Event{At: *en.FinishedAt, Text: label + " reported (status now " + en.Status + ")"})
		}
		if en.RemovedAt != nil && en.RemovedAt.After(since) {
			es = append(es, digest.Event{At: *en.RemovedAt, Text: label + " removed"})
		}
		if en.LostAt != nil && en.LostAt.After(since) {
			es = append(es, digest.Event{At: *en.LostAt, Text: label + " lost: " + oneLine(en.Error, 100)})
		}
	}
	sort.SliceStable(es, func(i, j int) bool { return es[i].At.After(es[j].At) })
	if len(es) > 40 {
		es = es[:40]
	}
	return hs, es
}

// readTail reads the last max bytes of a file and the file's size.
func readTail(path string, max int) (string, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	size := fi.Size()
	start := size - int64(max)
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return "", 0, err
	}
	return string(buf), int(size), nil
}

// memoryIndex is the path of the memory index Claude loads for the
// directory this runs in ("" when there is none).
func memoryIndex(e *Env) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	root := e.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home := e.Getenv("HOME")
		if home == "" {
			if home, err = os.UserHomeDir(); err != nil {
				return ""
			}
		}
		root = filepath.Join(home, ".claude")
	}
	p := filepath.Join(root, "projects", transcript.ClaudeProjectSlug(cwd), "memory", "MEMORY.md")
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p
	}
	return ""
}

// ---- install-hook ----

// hookSettings is the settings file the Coordinator preset passes to a
// Claude coordinator (--settings). The "clear" matcher injects the digest;
// the other start sources leave a short note, which is how the hub knows
// the session has the hook.
func hookSettings(bin string) map[string]any {
	cmd := func(extra string) map[string]any {
		return map[string]any{"type": "command", "command": shellQuote(bin) + " coordinator digest --hook" + extra, "timeout": 30}
	}
	return map[string]any{"hooks": map[string]any{"SessionStart": []any{
		map[string]any{"matcher": "clear", "hooks": []any{cmd("")}},
		map[string]any{"matcher": "startup|resume|compact", "hooks": []any{cmd(" --note")}},
	}}}
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func cmdCoordInstall(e *Env, args []string) error {
	fs := flag.NewFlagSet("coordinator install-hook", flag.ContinueOnError)
	bin := fs.String("bin", "", "the comebye binary the hook runs (default: this one)")
	uninstall := fs.Bool("uninstall", false, "remove the settings file")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q", pos[0])
	}
	path := filepath.Join(coordinatorDir(e), playbook.HookSettingsName)
	if *uninstall {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Fprintf(e.Stdout, "removed %s\n", path)
		return nil
	}
	b := *bin
	if b == "" {
		if b, err = os.Executable(); err != nil {
			return fmt.Errorf("cannot find this binary (pass --bin): %v", err)
		}
	}
	if !filepath.IsAbs(b) {
		return usageErr("--bin must be an absolute path")
	}
	data, err := json.MarshalIndent(hookSettings(b), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, `Installed the coordinator reset hook: %s

It is scoped to the coordinator: the app's Coordinator preset passes it to
Claude with --settings, so no other session (and not ~/.claude/settings.json)
gets it. Claude reads --settings when it starts, so the hook takes effect in
a coordinator started AFTER this: start a new one with the Coordinator preset
(or by hand: claude --settings %s ...). The hub refuses to clear a session
that was not started with it.
`, path, path)
	return nil
}

// ---- status / reset ----

func cmdCoordStatus(e *Env, args []string) error {
	fs := flag.NewFlagSet("coordinator status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	refresh := fs.Bool("refresh", false, "read each coordinator's context size now (the user's alone)")
	login := fs.String("login", "", "with --refresh: the Tailscale login to act as (default $COMEBYE_LOGIN, else the first of $COMEBYE_TS_ALLOW_LOGIN)")
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
	var q map[string][]string
	if *refresh {
		if c.user = peerLogin(e, *login); c.user == "" {
			return usageErr("--refresh needs the user's login: pass --login you@example.com (or set COMEBYE_LOGIN)")
		}
		q = map[string][]string{"refresh": {"1"}}
	}
	data, err := c.raw("GET", "/api/coordinator", q, nil)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var st struct {
		Threshold int `json:"threshold"`
		Last      *struct {
			At           time.Time `json:"at"`
			Name         string    `json:"name"`
			Trigger      string    `json:"trigger"`
			Tokens       int       `json:"tokens"`
			DigestLanded bool      `json:"digest_landed"`
		} `json:"last"`
		Due map[string]struct {
			Since  time.Time `json:"since"`
			Tokens int       `json:"tokens"`
			Why    string    `json:"why"`
		} `json:"due"`
		Coordinators []struct {
			ID, Name, Status, Error string
			Context                 *struct {
				Tokens     int  `json:"tokens"`
				HookActive bool `json:"hook_active"`
			}
		} `json:"coordinators"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("unexpected answer: %v", err)
	}
	if st.Threshold > 0 {
		fmt.Fprintf(e.Stdout, "automatic fresh start: at the first briefing after %dk tokens\n", st.Threshold/1000)
	} else {
		fmt.Fprintln(e.Stdout, "automatic fresh start: off (the hub's -coordinator-reset-tokens is 0)")
	}
	if st.Last != nil {
		fmt.Fprintf(e.Stdout, "last fresh start: %s ago, %s, at %dk tokens, %s, digest landed: %v\n", digest.Age(st.Last.At, e.Now()), st.Last.Trigger, st.Last.Tokens/1000, st.Last.Name, st.Last.DigestLanded)
	} else {
		fmt.Fprintln(e.Stdout, "last fresh start: never")
	}
	for id, d := range st.Due {
		fmt.Fprintf(e.Stdout, "due: %s (%dk tokens, since %s ago) %s\n", id, d.Tokens/1000, digest.Age(d.Since, e.Now()), d.Why)
	}
	for _, co := range st.Coordinators {
		line := fmt.Sprintf("coordinator %s %s: %s", co.Name, co.ID, co.Status)
		if co.Context != nil {
			line += fmt.Sprintf(", context %dk tokens, hook %s", co.Context.Tokens/1000, map[bool]string{true: "active", false: "NOT in this session"}[co.Context.HookActive])
		}
		if co.Error != "" {
			line += " (" + co.Error + ")"
		}
		fmt.Fprintln(e.Stdout, line)
	}
	return nil
}

func cmdCoordReset(e *Env, args []string) error {
	fs := flag.NewFlagSet("coordinator reset", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	login := fs.String("login", "", "the Tailscale login to act as (one of the hub's -ts-allow-login; default $COMEBYE_LOGIN, else the first of $COMEBYE_TS_ALLOW_LOGIN)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageErr("want at most one coordinator (its agent id, host/pane)")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	// A fresh start is the user's alone: an agent token is refused, so the
	// call goes out as the user's Tailscale login, to the hub.
	if c.user = peerLogin(e, *login); c.user == "" {
		return usageErr("coordinator reset needs the user's login: pass --login you@example.com (or set COMEBYE_LOGIN); it must be one of the hub's -ts-allow-login, and COMEBYE_URL must be the hub's")
	}
	body := map[string]string{}
	if len(pos) == 1 {
		body["agent_id"] = pos[0]
	}
	data, err := c.raw("POST", "/api/coordinator/reset", nil, body)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Tokens       int  `json:"tokens"`
		Renamed      bool `json:"renamed"`
		DigestLanded bool `json:"digest_landed"`
		Record       struct {
			AgentID, Name string
		} `json:"record"`
	}
	_ = json.Unmarshal(data, &r)
	fmt.Fprintf(e.Stdout, "cleared %s (%s) at %dk tokens; digest landed: %v\n", r.Record.Name, r.Record.AgentID, r.Tokens/1000, r.DigestLanded)
	if !r.DigestLanded {
		fmt.Fprintln(e.Stdout, "WARNING: the digest was not seen in the new session; look at the pane")
	}
	return nil
}

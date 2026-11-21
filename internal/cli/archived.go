package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jhjaggars/comebye/internal/buildout"
)

// `comebye archived`: finished work put away (the hub's archived.go). It lists
// the archived sessions, archives one at once (add) and brings one back
// (restore). Archiving and restoring are the user's or a coordinator's; an agent
// never archives itself.

type archivedItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

type archivedRecord struct {
	At            time.Time      `json:"at"`
	By            string         `json:"by"`
	Why           string         `json:"why"`
	From          string         `json:"from"`
	Left          []archivedItem `json:"left"`
	ParkID        string         `json:"park_id"`
	BuildFreed    int64          `json:"build_freed"`
	BuildDirs     []string       `json:"build_dirs"`
	BuildPending  bool           `json:"build_pending"`
	WorktreeBytes int64          `json:"worktree_bytes"`
	Unmerged      int            `json:"unmerged"`
	Unpushed      int            `json:"unpushed"`
}

type archivedListResp struct {
	Archived []struct {
		ID      string         `json:"id"`
		Name    string         `json:"name"`
		Host    string         `json:"host"`
		Project string         `json:"project"`
		Branch  string         `json:"branch"`
		Purpose string         `json:"purpose"`
		Archive archivedRecord `json:"archive"`
	} `json:"archived"`
	Count         int   `json:"count"`
	BuildFreed    int64 `json:"build_freed"`
	WorktreeBytes int64 `json:"worktree_bytes"`
}

func cmdArchived(e *Env, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "add":
			return cmdArchivedAdd(e, args[1:])
		case "restore":
			return cmdArchivedRestore(e, args[1:])
		case "list":
			args = args[1:]
		}
	}
	fs := flag.NewFlagSet("archived", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("unexpected argument %q (want add, restore or list)", pos[0])
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	data, err := c.raw("GET", "/api/archived", nil, nil)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r archivedListResp
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if r.Count == 0 {
		fmt.Fprintln(e.Stdout, "(nothing archived)")
		return nil
	}
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPROJECT\tBRANCH\tARCHIVED\tWHY\tNOT IN MAIN\tLEFT\tDISK\tBUILD FREED")
	for _, a := range r.Archived {
		x := a.Archive
		commits := "?"
		if x.Unmerged >= 0 {
			commits = fmt.Sprintf("%d", x.Unmerged)
			if x.Unpushed > 0 {
				commits += fmt.Sprintf(" (%d unpushed)", x.Unpushed)
			} else if x.Unpushed == 0 && x.Unmerged > 0 {
				commits += " (pushed)"
			}
		}
		var left []string
		for _, it := range x.Left {
			left = append(left, strings.TrimSpace(orDash(it.Text, it.Name)))
		}
		freed := "-"
		switch {
		case x.BuildPending:
			freed = "pending (host down)"
		case x.BuildFreed > 0:
			freed = buildout.Human(x.BuildFreed)
			if len(x.BuildDirs) > 0 {
				freed += ": " + strings.Join(x.BuildDirs, ", ")
			}
		}
		disk := "-"
		if x.WorktreeBytes > 0 {
			disk = buildout.Human(x.WorktreeBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, dash(a.Project), dash(a.Branch), ago(e, x.At), clipText(x.Why, 60), commits, dash(clipText(strings.Join(left, "; "), 60)), disk, freed)
	}
	tw.Flush()
	fmt.Fprintf(e.Stdout, "\n%d archived; %s of build output freed", r.Count, buildout.Human(r.BuildFreed))
	if r.WorktreeBytes > 0 {
		fmt.Fprintf(e.Stdout, "; their worktrees use %s", buildout.Human(r.WorktreeBytes))
	}
	fmt.Fprintln(e.Stdout, ". No source is ever deleted. Bring one back with: comebye archived restore NAME [--resume]")
	return nil
}

func orDash(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return dash(b)
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func cmdArchivedAdd(e *Env, args []string) error {
	fs := flag.NewFlagSet("archived add", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	reason := fs.String("reason", "", "why it can be put away (required for an agent)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want one NAME (a worker's name, its agent id or its handoff id)")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	data, err := c.raw("POST", "/api/archived", nil, map[string]any{"ref": pos[0], "reason": *reason})
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Entry struct {
			ID        string         `json:"id"`
			AgentName string         `json:"agent_name"`
			Branch    string         `json:"branch"`
			Archive   archivedRecord `json:"archive"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	x := r.Entry.Archive
	fmt.Fprintf(e.Stdout, "Archived %s (%s, %s). Its worktree source, branches and transcript are kept.\n", orDash(r.Entry.AgentName, r.Entry.ID), r.Entry.ID, dash(r.Entry.Branch))
	if x.ParkID != "" {
		fmt.Fprintf(e.Stdout, "Its pane was parked as %s.\n", x.ParkID)
	}
	switch {
	case x.BuildPending:
		fmt.Fprintln(e.Stdout, "Its host did not answer: the build output is cleared when it is up.")
	case x.BuildFreed > 0:
		fmt.Fprintf(e.Stdout, "Freed %s of build output.\n", buildout.Human(x.BuildFreed))
	}
	fmt.Fprintf(e.Stdout, "Bring it back with: comebye archived restore %s\n", orDash(r.Entry.AgentName, r.Entry.ID))
	return nil
}

func cmdArchivedRestore(e *Env, args []string) error {
	fs := flag.NewFlagSet("archived restore", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	resume := fs.Bool("resume", false, "also resume its parked pane")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageErr("want one NAME (a worker's name, its agent id or its handoff id)")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	data, err := c.raw("POST", "/api/archived/restore", nil, map[string]any{"ref": pos[0], "resume": *resume})
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	var r struct {
		Entry struct {
			ID        string `json:"id"`
			AgentName string `json:"agent_name"`
			Status    string `json:"status"`
		} `json:"entry"`
		Resume *struct {
			Agent struct {
				ID string `json:"id"`
			} `json:"agent"`
		} `json:"resume"`
		ResumeError string `json:"resume_error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "Restored %s (%s): it is %s again.\n", orDash(r.Entry.AgentName, r.Entry.ID), r.Entry.ID, r.Entry.Status)
	switch {
	case r.ResumeError != "":
		fmt.Fprintf(e.Stdout, "Not resumed: %s\n", r.ResumeError)
	case r.Resume != nil:
		fmt.Fprintf(e.Stdout, "Resumed as %s.\n", r.Resume.Agent.ID)
	}
	return nil
}

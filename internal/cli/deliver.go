package cli

import (
	"flag"
	"fmt"
	"regexp"
	"strings"
)

var pageArgRE = regexp.MustCompile(`^pg_[0-9a-f]{10}$`)

// recordDelivery tells the hub that pages capture an agent's work.
func recordDelivery(c *client, agentID string, pages, notCaptured []string) ([]byte, error) {
	return c.raw("POST", "/api/deliveries", nil, map[string]any{"agent_id": agentID, "pages": pages, "not_captured": notCaptured})
}

func cmdDeliver(e *Env, args []string) error {
	fs := flag.NewFlagSet("deliver", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	var notCaptured multiText
	fs.Var(&notCaptured, "not-captured", "something your pages do NOT hold (code, a data file…): while set, your files and commits still count as work to keep (repeatable)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	var pages []string
	agent := ""
	for _, p := range pos {
		switch {
		case pageArgRE.MatchString(p):
			pages = append(pages, p)
		case agent == "":
			agent = p
		default:
			return usageErr("unexpected argument %q (pages look like pg_0123456789)", p)
		}
	}
	if len(pages) == 0 {
		return usageErr("want deliver [AGENT] PAGE… (the pg_… ids `comebye publish` printed); AGENT is you by default")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	id := ""
	if agent == "" {
		w, err := c.getWhoami()
		if err != nil {
			return err
		}
		if id = w.Agent; id == "" {
			return usageErr("you are not an agent in a herdr pane: name the AGENT whose work the pages capture")
		}
	} else {
		t, err := c.resolve(agent)
		if err != nil {
			return err
		}
		id = t.ID
	}
	data, err := recordDelivery(c, id, pages, notCaptured)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	s := "s"
	if len(pages) == 1 {
		s = ""
	}
	fmt.Fprintf(e.Stdout, "Recorded %d page%s as capturing the work of %s.\n", len(pages), s, id)
	if len(notCaptured) > 0 {
		fmt.Fprintln(e.Stdout, "Not captured, so it stays a warning in the removal review: "+strings.Join(notCaptured, "; ")+".")
	} else {
		fmt.Fprintln(e.Stdout, "If the agent is marked `--discard`, the removal review now lists its uncommitted files and local commits as captured, and the user can discard them all with one confirmation.")
	}
	return nil
}

// multiText is a repeatable flag whose values may contain commas.
type multiText []string

func (l *multiText) String() string { return strings.Join(*l, "; ") }
func (l *multiText) Set(v string) error {
	if v = strings.TrimSpace(v); v != "" {
		*l = append(*l, v)
	}
	return nil
}

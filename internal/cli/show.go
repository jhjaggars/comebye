package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
)

// cmdShow declares the document the user should read beside an agent's
// chat (the app's Doc panel). The hub checks the file at once, so an agent
// hears here that the panel will not open it.
func cmdShow(e *Env, args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	section := fs.String("section", "", "the heading to open the document at")
	clear := fs.Bool("clear", false, "stop showing a document")
	who := fs.String("agent", "", "the agent whose panel it is (yours by default)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *clear && (len(pos) > 0 || *section != "") {
		return usageErr("--clear takes no FILE or --section")
	}
	if !*clear && len(pos) != 1 {
		return usageErr("want show FILE [--section HEADING] (or show --clear)")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	id := ""
	if *who == "" {
		w, err := c.getWhoami()
		if err != nil {
			return err
		}
		if id = w.Agent; id == "" {
			return usageErr("you are not an agent in a herdr pane: name the AGENT with --agent")
		}
	} else {
		t, err := c.resolve(*who)
		if err != nil {
			return err
		}
		id = t.ID
	}
	body := map[string]any{"agent_id": id}
	if *clear {
		body["clear"] = true
	} else {
		abs, err := filepath.Abs(pos[0])
		if err != nil {
			return err
		}
		body["path"], body["section"] = abs, *section
	}
	data, err := c.raw("POST", "/api/show", nil, body)
	if err != nil {
		return err
	}
	if *asJSON {
		return rawJSON(e, data)
	}
	if *clear {
		fmt.Fprintln(e.Stdout, "The Doc panel shows no document for this agent now.")
		return nil
	}
	var res struct {
		Path    string `json:"path"`
		Warning string `json:"warning"`
	}
	_ = json.Unmarshal(data, &res)
	fmt.Fprintf(e.Stdout, "The Doc panel beside this agent's chat now opens %s.\n", res.Path)
	if res.Warning != "" {
		fmt.Fprintln(e.Stdout, "Warning: "+res.Warning+".")
	}
	return nil
}

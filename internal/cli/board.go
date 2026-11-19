package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/digest"
)

// boardLane is a lane of GET /api/board (docs/board.md).
type boardLane struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Count int    `json:"count"`
	Items []struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Title string `json:"title"`
		Kind  string `json:"kind"`
		State string `json:"state"`
		Since string `json:"since"`
	} `json:"items"`
}

// digestLane is the lane as the digest package prints it.
func (l boardLane) digestLane() digest.Lane {
	out := digest.Lane{Key: l.Key, Name: l.Name, Count: l.Count}
	for _, it := range l.Items {
		li := digest.LaneItem{ID: it.ID, Title: it.Title, Status: firstNonEmpty(it.State, it.Kind)}
		if t, err := time.Parse(time.RFC3339, it.Since); err == nil {
			li.Since = t
		}
		out.Items = append(out.Items, li)
	}
	return out
}

func getBoard(c *client) ([]boardLane, error) {
	var v struct {
		Lanes []boardLane `json:"lanes"`
	}
	if err := c.call("GET", "/api/board", nil, nil, &v); err != nil {
		return nil, err
	}
	return v.Lanes, nil
}

// cmdBoard prints the hub's board as plain text lanes.
func cmdBoard(e *Env, args []string) error {
	fs := flag.NewFlagSet("board", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output (the hub's /api/board answer)")
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
		data, err := c.raw("GET", "/api/board", nil, nil)
		if err != nil {
			return err
		}
		return rawJSON(e, data)
	}
	var v struct {
		Lanes []boardLane `json:"lanes"`
	}
	data, err := c.raw("GET", "/api/board", nil, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("unexpected answer: %v", err)
	}
	now := e.Now()
	var parts []string
	for _, l := range v.Lanes {
		name := strings.ToLower(l.Name)
		dl := l.digestLane()
		dl.Name = name
		parts = append(parts, strings.TrimRight(digest.FormatLane(dl, now), "\n"))
	}
	if len(parts) == 0 {
		parts = append(parts, "(the board is empty)")
	}
	fmt.Fprintln(e.Stdout, strings.Join(parts, "\n\n"))
	return nil
}

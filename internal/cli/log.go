package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// logEvent is one event of GET /api/log: something the hub handled itself.
type logEvent struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	Subject string    `json:"subject,omitempty"`
	Who     string    `json:"who,omitempty"`
	Text    string    `json:"text"`
}

// getLog reads the hub's event log, newest first.
func getLog(c *client, since, kinds string, limit int) ([]logEvent, error) {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if kinds != "" {
		q.Set("kind", kinds)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var v struct {
		Events []logEvent `json:"events"`
	}
	path := "/api/log"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if err := c.call("GET", path, nil, nil, &v); err != nil {
		return nil, err
	}
	return v.Events, nil
}

// cmdLog prints what the hub handled itself: notices that are not typed
// into the coordinator's pane any more.
func cmdLog(e *Env, args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	since := fs.String("since", "24h", "how far back: a duration such as 90m, 24h or 2d")
	kind := fs.String("kind", "", "only these kinds, comma separated: landed, park, autoremove, cleanup, sweep, blocked, unblocked, held, relay, relay-failed")
	limit := fs.Int("limit", 0, "at most this many events (newest first; 0: all)")
	asJSON := fs.Bool("json", false, "machine-readable output (the hub's /api/log answer)")
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
	evs, err := getLog(c, *since, *kind, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		if evs == nil {
			evs = []logEvent{}
		}
		b, _ := json.Marshal(map[string]any{"events": evs})
		return rawJSON(e, b)
	}
	if len(evs) == 0 {
		fmt.Fprintf(e.Stdout, "(the hub handled nothing in the last %s)\n", *since)
		return nil
	}
	now := e.Now()
	for _, ev := range evs {
		fmt.Fprintf(e.Stdout, "%s  %-10s %s\n", logWhen(ev.At, now), ev.Kind, ev.Text)
	}
	return nil
}

// logWhen is a time as HH:MM today, or the date and time before.
func logWhen(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return strings.TrimSpace(t.Format("Jan 2 15:04"))
}

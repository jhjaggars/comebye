package cli

import (
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// apiUsage is the part of GET /api/usage the command prints.
type apiUsage struct {
	From   int64  `json:"from"`
	To     int64  `json:"to"`
	By     string `json:"by"`
	Bucket string `json:"bucket"`
	Totals usageTotal
	Groups []struct {
		Key   string `json:"key"`
		Label string `json:"label"`
		usageTotal
		Apart bool `json:"apart"`
	} `json:"groups"`
	TopSessions []struct {
		Host    string `json:"host"`
		Kind    string `json:"kind"`
		ID      string `json:"id"`
		Project string `json:"project"`
		Name    string `json:"name"`
		Role    string `json:"role"`
		usageTotal
	} `json:"top_sessions"`
	TopHandoffs []struct {
		Handoff  string `json:"handoff"`
		Name     string `json:"name"`
		Project  string `json:"project"`
		Sessions int    `json:"sessions"`
		usageTotal
	} `json:"top_handoffs"`
	Cache struct {
		Hit          float64 `json:"hit"`
		ReadPerWrite float64 `json:"read_per_write"`
		SavedUSD     float64 `json:"saved_usd"`
	} `json:"cache"`
	Hosts []struct {
		Name     string `json:"name"`
		Err      string `json:"err"`
		Gathered int64  `json:"gathered"`
	} `json:"hosts"`
}

type usageTotal struct {
	In       int64   `json:"in"`
	Out      int64   `json:"out"`
	CW       int64   `json:"cw"`
	CR       int64   `json:"cr"`
	Calls    int64   `json:"calls"`
	Fresh    int64   `json:"fresh"`
	USD      float64 `json:"usd"`
	Unpriced int64   `json:"unpriced"`
	Hit      float64 `json:"hit"`
}

// tokens prints a count as 1.2M, 340k or 87.
func tokens(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1e4:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1e3:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}

func dollars(t usageTotal) string {
	if t.USD == 0 && t.Unpriced > 0 {
		return "-"
	}
	s := fmt.Sprintf("$%.2f", t.USD)
	if t.Unpriced > 0 {
		s += "+"
	}
	return s
}

func cmdUsage(e *Env, args []string) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	by := fs.String("by", "kind", "group by: kind, model, host, project, role, purpose, handoff, session or token")
	since := fs.String("since", "14d", "from: a span back from now (36h, 14d) or a date (2026-11-01)")
	until := fs.String("until", "", "to: a date or span (default: now)")
	bucket := fs.String("bucket", "", "hour or day (default: hour for up to 3 days, else day)")
	limit := fs.Int("limit", 10, "how many top sessions and handoffs")
	login := fs.String("login", "", "act as the user (Tailscale login; default: the agent token, which works for a coordinator)")
	filters := map[string]*string{}
	for _, d := range []string{"kind", "model", "host", "project", "role", "purpose", "handoff"} {
		filters[d] = fs.String(d, "", "only this "+d)
	}
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageErr("usage takes flags only")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	if *login != "" {
		c.user = peerLogin(e, *login)
	}
	q := url.Values{"by": {*by}, "since": {*since}, "limit": {strconv.Itoa(*limit)}}
	if *until != "" {
		q.Set("to", *until)
	}
	if *bucket != "" {
		q.Set("bucket", *bucket)
	}
	_, off := time.Now().Zone()
	q.Set("tz", strconv.Itoa(off/60))
	for d, v := range filters {
		if *v != "" {
			q.Set(d, *v)
		}
	}
	if *asJSON {
		var raw map[string]any
		if err := c.call("GET", "/api/usage?"+q.Encode(), nil, nil, &raw); err != nil {
			return err
		}
		return jsonOut(e, raw)
	}
	var a apiUsage
	if err := c.call("GET", "/api/usage?"+q.Encode(), nil, nil, &a); err != nil {
		return err
	}
	w := e.Stdout
	from, to := time.Unix(a.From, 0), time.Unix(a.To, 0)
	fmt.Fprintf(w, "Tokens %s to %s, by %s\n", from.Format("2006-01-02 15:04"), to.Format("2006-01-02 15:04"), a.By)
	t := a.Totals
	fmt.Fprintf(w, "  fresh %s (input %s + output %s + cache writes %s), cache reads %s apart, %d replies\n",
		tokens(t.Fresh), tokens(t.In), tokens(t.Out), tokens(t.CW), tokens(t.CR), t.Calls)
	fmt.Fprintf(w, "  cache: %.0f%% of the prompt side came from the cache (%.1f reads per write)\n", a.Cache.Hit*100, a.Cache.ReadPerWrite)
	fmt.Fprintf(w, "  estimated cost %s", dollars(t))
	if t.Unpriced > 0 {
		fmt.Fprintf(w, " (%s fresh tokens have no price; set one in the app's Usage screen)", tokens(t.Unpriced))
	}
	fmt.Fprintln(w, " - list prices, an estimate")
	for _, h := range a.Hosts {
		if h.Err != "" {
			fmt.Fprintf(w, "  ! %s: %s\n", h.Name, h.Err)
		}
	}
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "\n%s\tfresh\tshare\tcache read\thit\test. cost\n", strings.ToUpper(a.By))
	for _, g := range a.Groups {
		name := g.Key
		if g.Label != "" && g.Label != g.Key {
			name += " (" + g.Label + ")"
		}
		share := 0.0
		if t.Fresh > 0 && !g.Apart {
			share = float64(g.Fresh) / float64(t.Fresh) * 100
		}
		fresh := tokens(g.Fresh)
		if a.By == "token" {
			fresh = tokens(g.In + g.Out + g.CW + g.CR)
		}
		fmt.Fprintf(tw, "%s\t%s\t%.0f%%\t%s\t%.0f%%\t%s\n", name, fresh, share, tokens(g.CR), g.Hit*100, dollars(g.usageTotal))
	}
	tw.Flush()
	if len(a.TopHandoffs) > 0 {
		fmt.Fprintln(w, "\nTop handoffs")
		tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		for _, h := range a.TopHandoffs {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d sessions\t%s\n", h.Handoff, dash(h.Name), dash(h.Project), h.Sessions, tokens(h.Fresh))
		}
		tw.Flush()
	}
	if len(a.TopSessions) > 0 {
		fmt.Fprintln(w, "\nTop sessions")
		tw = tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
		for _, s := range a.TopSessions {
			id := s.ID
			if len(id) > 8 {
				id = id[len(id)-8:]
			}
			fmt.Fprintf(tw, "  %s/%s %s\t%s\t%s\t%s\n", s.Host, s.Kind, id, dash(s.Project), dash(s.Role), tokens(s.Fresh))
		}
		tw.Flush()
	}
	return nil
}

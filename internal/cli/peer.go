package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// ---- peer ----

// peerLogin is who the user-only registry calls are made as: --login,
// COMEBYE_LOGIN, else the first of the server's allowlist
// (COMEBYE_TS_ALLOW_LOGIN) when this shell has it.
func peerLogin(e *Env, flagVal string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(e.Getenv("COMEBYE_LOGIN")); v != "" {
		return v
	}
	first, _, _ := strings.Cut(e.Getenv("COMEBYE_TS_ALLOW_LOGIN"), ",")
	return strings.TrimSpace(first)
}

type apiPeer struct {
	Name    string     `json:"name"`
	URLs    []string   `json:"urls"`
	Source  string     `json:"source"`
	AddedBy string     `json:"added_by"`
	AddedAt *time.Time `json:"added_at"`
	Tag     string     `json:"tag"`
	Status  string     `json:"status"`
	Error   string     `json:"error"`
	Class   string     `json:"class"`
	Agents  int        `json:"agents"`
	Version string     `json:"version"`

	VersionMismatch bool `json:"version_mismatch"`
}

func cmdPeer(e *Env, args []string) error {
	fs := flag.NewFlagSet("peer", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	login := fs.String("login", "", "the Tailscale login to act as (one of the hub's -ts-allow-login; default $COMEBYE_LOGIN, else the first of $COMEBYE_TS_ALLOW_LOGIN)")
	tag := fs.String("tag", "", "add: the tailnet tag the peer is expected to carry (tag:worker); recorded only")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usageErr("want list, show NAME, add NAME URL[|URL...] or remove NAME")
	}
	if *tag != "" && pos[0] != "add" {
		return usageErr("--tag goes with peer add")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	// The registry is the user's alone: an agent token is refused, so the
	// calls go out as the user's Tailscale login instead.
	c.user = peerLogin(e, *login)
	if c.user == "" {
		return usageErr("peer needs the user's login: pass --login you@example.com (or set COMEBYE_LOGIN); it must be one of the hub's -ts-allow-login")
	}
	switch pos[0] {
	case "list":
		if len(pos) != 1 {
			return usageErr("peer list takes no arguments")
		}
		data, err := c.raw("GET", "/api/peers", nil, nil)
		if err != nil {
			return err
		}
		if *asJSON {
			return rawJSON(e, data)
		}
		var r struct {
			Peers   []apiPeer `json:"peers"`
			Version string    `json:"version"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return fmt.Errorf("unexpected answer: %v", err)
		}
		if len(r.Peers) == 0 {
			fmt.Fprintln(e.Stdout, "(no peers; add one with `comebye peer add NAME https://NAME.tailnet.ts.net`)")
			return nil
		}
		tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSOURCE\tSTATUS\tAGENTS\tVERSION\tDEFAULT\tADDRESS\tADDED")
		defaults := peerDefaults(c, r.Peers)
		for _, p := range r.Peers {
			ver := p.Version
			switch {
			case ver == "":
				ver = "unknown"
			case p.VersionMismatch:
				ver += " (differs from the hub's " + r.Version + ")"
			}
			added := "-"
			if p.AddedBy != "" || p.AddedAt != nil {
				added = strings.TrimSpace(p.AddedBy)
				if p.AddedAt != nil {
					added = strings.TrimSpace(added + " " + p.AddedAt.Format("2006-01-02"))
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", p.Name, p.Source, p.Status, p.Agents, ver, defaults[p.Name], strings.Join(p.URLs, " | "), added)
		}
		tw.Flush()
		for _, p := range r.Peers {
			if p.Status == "down" && p.Error != "" {
				fmt.Fprintf(e.Stdout, "peer %s is down: %s\n", p.Name, p.Error)
			}
		}
		return nil
	case "show":
		if len(pos) != 2 {
			return usageErr("want peer show NAME")
		}
		return peerShow(e, c, pos[1], *asJSON)
	case "add":
		if len(pos) != 3 {
			return usageErr("want peer add NAME URL[|URL...]")
		}
		var urls []string
		for _, u := range strings.Split(pos[2], "|") {
			if u = strings.TrimSpace(u); u != "" {
				urls = append(urls, u)
			}
		}
		data, err := c.raw("POST", "/api/peers", nil, map[string]any{"name": pos[1], "urls": urls, "tag": *tag})
		if err != nil {
			return err
		}
		if *asJSON {
			return rawJSON(e, data)
		}
		fmt.Fprintf(e.Stdout, "Peer %s added at %s; the hub is connecting to it now (`comebye peer list` shows its status).\n", pos[1], strings.Join(urls, " | "))
		return nil
	case "remove":
		if len(pos) != 2 {
			return usageErr("want peer remove NAME")
		}
		if _, err := c.raw("DELETE", "/api/peers/"+url.PathEscape(pos[1]), nil, nil); err != nil {
			return err
		}
		if *asJSON {
			return jsonOut(e, map[string]any{"removed": pos[1]})
		}
		fmt.Fprintf(e.Stdout, "Peer %s removed.\n", pos[1])
		return nil
	}
	return usageErr("unknown peer command %q (want list, show, add or remove)", pos[0])
}

// peerDefaults reads each reachable peer's settings, in parallel, for the
// DEFAULT column: "-" no default set, "?" the peer did not answer (down, or
// too old to have settings).
func peerDefaults(c *client, peers []apiPeer) map[string]string {
	out := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range peers {
		if p.Status != "ok" {
			mu.Lock()
			out[p.Name] = "?"
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			label := defaultLabel(fetchSettings(c, name))
			mu.Lock()
			out[name] = label
			mu.Unlock()
		}(p.Name)
	}
	wg.Wait()
	return out
}

// peerShow prints one peer: how the hub sees it, and what it can start.
func peerShow(e *Env, c *client, name string, asJSON bool) error {
	data, err := c.raw("GET", "/api/peers", nil, nil)
	if err != nil {
		return err
	}
	var r struct {
		Peers   []apiPeer `json:"peers"`
		Version string    `json:"version"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("unexpected answer: %v", err)
	}
	var p *apiPeer
	for i := range r.Peers {
		if r.Peers[i].Name == name {
			p = &r.Peers[i]
		}
	}
	if p == nil {
		return fmt.Errorf("no peer named %s (see `comebye peer list`)", name)
	}
	var set *apiSettings
	var catalogs []apiCatalog
	if p.Status == "ok" {
		set = fetchSettings(c, name)
		var m struct {
			Catalogs []apiCatalog `json:"catalogs"`
		}
		if c.call("GET", "/api/hosts/"+url.PathEscape(name)+"/models", nil, nil, &m) == nil {
			catalogs = m.Catalogs
		}
	}
	if asJSON {
		return jsonOut(e, map[string]any{"peer": p, "settings": set, "models": catalogs, "hub_version": r.Version})
	}
	w := e.Stdout
	ver := dash(p.Version)
	if p.VersionMismatch {
		ver += " (differs from the hub's " + r.Version + ")"
	}
	fmt.Fprintf(w, "Peer %s (%s)\n  status:   %s, %d agents\n  version:  %s\n  address:  %s\n", p.Name, p.Source, p.Status, p.Agents, ver, strings.Join(p.URLs, " | "))
	if p.Error != "" {
		fmt.Fprintf(w, "  error:    %s\n", p.Error)
	}
	switch {
	case p.Status != "ok":
		fmt.Fprintln(w, "  (settings and models are read from the peer itself, which is not answering)")
	case set == nil:
		fmt.Fprintln(w, "  settings: not available: this peer is too old to have host settings (upgrade it)")
	default:
		printSettings(e, *set)
	}
	for _, ct := range catalogs {
		def := "the agent's own default"
		if ct.Default != "" {
			def = ct.Default
		}
		fmt.Fprintf(w, "  %s models: %d (default %s)\n", ct.Kind, len(ct.Models), def)
	}
	return nil
}

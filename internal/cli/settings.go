package cli

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ---- settings ----

// apiSettings is a host's settings as GET /api/settings answers.
type apiSettings struct {
	Host     string `json:"host"`
	Version  string `json:"version"`
	Settings struct {
		DefaultKind  string                 `json:"default_kind"`
		AllowedKinds []string               `json:"allowed_kinds"`
		Kinds        map[string]apiKindConf `json:"kinds"`
		MaxWorkers   *int                   `json:"max_workers_per_repo"`
		Rev          int                    `json:"rev"`
		UpdatedBy    string                 `json:"updated_by"`
		UpdatedAt    *time.Time             `json:"updated_at"`
	} `json:"settings"`
	Effective struct {
		DefaultKind   string   `json:"default_kind"`
		DefaultSource string   `json:"default_source"`
		AllowedKinds  []string `json:"allowed_kinds"`
		AllowedSource string   `json:"allowed_source"`
		MaxWorkers    *int     `json:"max_workers_per_repo"`
		MaxSource     string   `json:"max_workers_per_repo_source"`
		Notice        string   `json:"notice"`
	} `json:"effective"`
	InstalledKinds []string `json:"installed_kinds"`
	Launch         struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	} `json:"launch"`
}

type apiKindConf struct {
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
}

func settingsPath(host string) string {
	if host == "" {
		return "/api/settings"
	}
	return "/api/hosts/" + url.PathEscape(host) + "/settings"
}

// settingsSource labels where an effective value comes from.
func settingsSource(src string) string {
	switch src {
	case "setting":
		return "set in the app or with `comebye settings set`"
	case "flag":
		return "from -default-kind / -allowed-kinds"
	case "auto":
		return "a guess: the kind most running agents use"
	}
	return ""
}

func printSettings(e *Env, s apiSettings) {
	w := e.Stdout
	fmt.Fprintf(w, "Settings on %s", dash(s.Host))
	if s.Settings.Rev > 0 {
		fmt.Fprintf(w, " (revision %d", s.Settings.Rev)
		if s.Settings.UpdatedBy != "" {
			fmt.Fprintf(w, ", by %s", s.Settings.UpdatedBy)
		}
		if s.Settings.UpdatedAt != nil {
			fmt.Fprintf(w, ", %s", s.Settings.UpdatedAt.Format("2006-01-02"))
		}
		fmt.Fprint(w, ")")
	}
	fmt.Fprintln(w)
	def := s.Effective.DefaultKind
	if def == "" {
		def = "(none set: " + settingsSource("auto") + ")"
	} else {
		def += "  (" + settingsSource(s.Effective.DefaultSource) + ")"
	}
	fmt.Fprintf(w, "  default agent: %s\n", def)
	if len(s.Effective.AllowedKinds) == 0 {
		fmt.Fprintln(w, "  allowed:       any installed kind")
	} else {
		fmt.Fprintf(w, "  allowed:       %s  (%s)\n", strings.Join(s.Effective.AllowedKinds, ", "), settingsSource(s.Effective.AllowedSource))
	}
	if s.Effective.MaxWorkers != nil {
		if n := *s.Effective.MaxWorkers; n == 0 {
			fmt.Fprintf(w, "  worker cap:    none  (%s)\n", capSource(s.Effective.MaxSource))
		} else {
			fmt.Fprintf(w, "  worker cap:    %d running per repository; more wait  (%s)\n", n, capSource(s.Effective.MaxSource))
		}
	}
	fmt.Fprintf(w, "  installed:     %s\n", dash(strings.Join(s.InstalledKinds, ", ")))
	var kinds []string
	for k := range s.Settings.Kinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(w, "  %-14s %s\n", k+":", modelLabel(s.Settings.Kinds[k].Model, s.Settings.Kinds[k].Thinking))
	}
	if s.Launch.Enabled {
		fmt.Fprintln(w, "  launching:     on")
	} else {
		fmt.Fprintf(w, "  launching:     off (%s)\n", dash(s.Launch.Reason))
	}
	if s.Effective.Notice != "" {
		fmt.Fprintf(w, "  warning:       %s\n", s.Effective.Notice)
	}
}

func capSource(src string) string {
	if src == "setting" {
		return "set in the app or with `comebye settings set`"
	}
	return "the default"
}

// kindSourceLabel says in words where a started agent's kind came from.
func kindSourceLabel(src string) string {
	switch src {
	case "request":
		return "asked for"
	case "setting":
		return "the host's default"
	case "flag":
		return "the host's default, from -default-kind"
	case "auto":
		return "the host's guess: no default is set"
	}
	return src
}

// defaultLabel is a host's default agent as one cell: the kind and its model.
func defaultLabel(s *apiSettings) string {
	if s == nil {
		return "?"
	}
	k := s.Effective.DefaultKind
	if k == "" {
		return "-"
	}
	if m := s.Settings.Kinds[k].Model; m != "" {
		return k + " (" + m + ")"
	}
	return k
}

// kvList collects KIND=VALUE flags.
type kvList map[string]string

func (l *kvList) String() string { return "" }
func (l *kvList) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	k = strings.ToLower(strings.TrimSpace(k))
	if !ok || k == "" {
		return fmt.Errorf("want KIND=VALUE, e.g. pi=high")
	}
	if *l == nil {
		*l = kvList{}
	}
	(*l)[k] = strings.TrimSpace(val)
	return nil
}

func cmdSettings(e *Env, args []string) error {
	fs := flag.NewFlagSet("settings", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	host := fs.String("host", "", "the host (default: this one)")
	login := fs.String("login", "", "set, clear: the Tailscale login to act as (default $COMEBYE_LOGIN, else the first of $COMEBYE_TS_ALLOW_LOGIN)")
	defKind := fs.String("default-kind", "", "set: the agent kind a start uses when the request names none")
	allow := fs.String("allow", "", "set: the only kinds this host starts (comma-separated; 'any' lifts the limit)")
	var models, thinkings kvList
	fs.Var(&models, "model", "set: KIND=MODEL, the model that kind starts with (KIND= clears it; repeatable)")
	fs.Var(&thinkings, "thinking", "set: KIND=LEVEL, pi's thinking level (KIND= clears it; repeatable)")
	maxWorkers := fs.Int("max-workers", -1, "set: the most workers running at once per repository on the host; more wait (0 = no cap)")
	field := fs.String("field", "", "clear: only this one of default-kind, allowed, models, max-workers (default: everything)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	verb := "show"
	if len(pos) > 0 {
		verb, pos = pos[0], pos[1:]
	}
	if len(pos) > 0 || (verb != "show" && verb != "set" && verb != "clear") {
		return usageErr("want settings, settings set ... or settings clear")
	}
	switch {
	case verb == "set" && *defKind == "" && *allow == "" && len(models) == 0 && len(thinkings) == 0 && *maxWorkers < 0:
		return usageErr("settings set wants --default-kind, --allow, --model, --thinking or --max-workers")
	case verb != "clear" && *field != "":
		return usageErr("--field goes with settings clear")
	case verb == "clear" && *field != "" && *field != "default-kind" && *field != "allowed" && *field != "models" && *field != "max-workers":
		return usageErr("--field is default-kind, allowed, models or max-workers")
	case verb != "set" && (*defKind != "" || *allow != "" || len(models) > 0 || len(thinkings) > 0 || *maxWorkers >= 0):
		return usageErr("--default-kind, --allow, --model, --thinking and --max-workers go with settings set")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	if verb != "show" {
		// Writing is the user's alone: an agent token is refused, so the
		// call goes out as the user's Tailscale login, like `peer add`.
		if c.user = peerLogin(e, *login); c.user == "" {
			return usageErr("settings %s needs the user's login: pass --login you@example.com (or set COMEBYE_LOGIN)", verb)
		}
	} else if *login != "" {
		c.user = peerLogin(e, *login)
	}
	path := settingsPath(*host)
	var cur apiSettings
	if err := c.call("GET", path, nil, nil, &cur); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.status == 404 && *host != "" {
			return fmt.Errorf("%v (the host may be too old to have settings: `comebye peer show %s` says its version)", err, *host)
		}
		return err
	}
	if verb == "show" {
		if *asJSON {
			return jsonOut(e, cur)
		}
		printSettings(e, cur)
		return nil
	}

	// set and clear change a copy of what the host has and send it back
	// with its revision, so two editors cannot overwrite each other.
	body := map[string]any{"rev": cur.Settings.Rev}
	kinds := map[string]apiKindConf{}
	for k, v := range cur.Settings.Kinds {
		kinds[k] = v
	}
	dk, allowed := cur.Settings.DefaultKind, cur.Settings.AllowedKinds
	capSet := cur.Settings.MaxWorkers
	switch verb {
	case "set":
		if *defKind != "" {
			dk = strings.ToLower(*defKind)
		}
		if *allow != "" {
			allowed = nil
			if a := strings.ToLower(*allow); a != "any" {
				for _, k := range strings.Split(a, ",") {
					if k = strings.TrimSpace(k); k != "" {
						allowed = append(allowed, k)
					}
				}
			}
		}
		if *maxWorkers >= 0 {
			capSet = maxWorkers
		}
		for k, m := range models {
			v := kinds[k]
			v.Model = m
			kinds[k] = v
		}
		for k, t := range thinkings {
			v := kinds[k]
			v.Thinking = t
			kinds[k] = v
		}
	case "clear":
		switch *field {
		case "":
			dk, allowed, kinds, capSet = "", nil, map[string]apiKindConf{}, nil
		case "default-kind":
			dk = ""
		case "allowed":
			allowed = nil
		case "models":
			kinds = map[string]apiKindConf{}
		case "max-workers":
			capSet = nil
		}
	}
	body["default_kind"], body["allowed_kinds"], body["kinds"] = dk, allowed, kinds
	body["max_workers_per_repo"] = capSet
	var out apiSettings
	if err := c.call("PUT", path, nil, body, &out); err != nil {
		return err
	}
	if *asJSON {
		return jsonOut(e, out)
	}
	printSettings(e, out)
	return nil
}

// fetchSettings reads a host's settings as the caller; nil when the host
// does not answer (a peer too old to have them, or down).
func fetchSettings(c *client, host string) *apiSettings {
	var s apiSettings
	if err := c.call("GET", settingsPath(host), nil, nil, &s); err != nil {
		return nil
	}
	return &s
}

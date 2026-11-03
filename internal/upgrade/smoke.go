package upgrade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

func systemctl(o *Options, args ...string) error {
	cmd := exec.Command(o.Systemctl, append([]string{"--user"}, args...)...)
	cmd.Env = o.Env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// waitActive polls "systemctl --user is-active" until the unit is active.
func waitActive(o *Options) error {
	var last string
	for i := 0; i < o.SmokeTries; i++ {
		cmd := exec.Command(o.Systemctl, "--user", "is-active", o.Service)
		cmd.Env = o.Env
		out, _ := cmd.Output()
		last = strings.TrimSpace(string(out))
		if last == "active" {
			return nil
		}
		o.Sleep(o.SmokeWait)
	}
	return fmt.Errorf("%s is %q, not active", o.Service, last)
}

// indexTag is the ETag of the local server's index.html (the served build);
// "" when the server does not answer.
func indexTag(o *Options) string {
	resp, err := o.HTTP.Get(strings.TrimRight(o.LocalURL, "/") + "/")
	if err != nil {
		return ""
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	if t := resp.Header.Get("ETag"); t != "" {
		return t
	}
	return "served"
}

// run runs the NEW binary (o.Bin) with a subcommand.
func runBin(o *Options, args ...string) (string, string, error) {
	cmd := exec.Command(o.Bin, args...)
	cmd.Env = o.Env
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	return so.String(), strings.TrimSpace(se.String()), err
}

var netWords = regexp.MustCompile(`(?i)unreachable|timeout|timed out|connection|dial|no route|refused|offline|deadline`)

// smoke checks the restarted service. It depends on this host only: a peer
// or hub that cannot be reached is a warning, never a failure.
//
//   - the unit is active;
//   - the new binary reaches the local server (whoami) and lists this
//     host's own agents;
//   - the local server serves its index.html;
//   - on a hub, ledger, queue and sweep --dry-run exit 0 (a sweep that
//     fails only because a host is unreachable is a warning); on a peer
//     they go through the hub, so a failure is a warning;
//   - the journal since the restart has no error-level lines.
func smoke(o *Options, since time.Time, beforeTag string) error {
	if err := waitActive(o); err != nil {
		return err
	}
	o.printf("smoke: %s is active\n", o.Service)

	// The server needs a moment after "active"; retry the first call.
	var who struct {
		Host   string `json:"host"`
		Hub    string `json:"hub"`
		Ledger bool   `json:"ledger"`
	}
	var err error
	for i := 0; i < o.SmokeTries; i++ {
		var out, serr string
		out, serr, err = runBin(o, "whoami", "--json")
		if err == nil {
			err = json.Unmarshal([]byte(out), &who)
		} else if serr != "" {
			err = fmt.Errorf("%v: %s", err, serr)
		}
		if err == nil {
			break
		}
		o.Sleep(o.SmokeWait)
	}
	if err != nil {
		return fmt.Errorf("the new binary cannot reach the local server (whoami): %w", err)
	}
	o.printf("smoke: whoami ok (host %s)\n", who.Host)

	tag := indexTag(o)
	if tag == "" {
		return fmt.Errorf("the local server at %s does not serve index.html", o.LocalURL)
	}
	if beforeTag != "" && beforeTag != tag {
		o.printf("smoke: index.html changed (%s -> %s): the new build is served\n", beforeTag, tag)
	} else {
		o.printf("smoke: index.html served (%s)\n", tag)
	}

	// Only this host's agents: other hosts may be offline.
	out, serr, err := runBin(o, "agents", "--host", who.Host, "--json")
	if err != nil {
		return fmt.Errorf("agents --host %s: %v: %s", who.Host, err, serr)
	}
	var ag struct {
		Agents []struct {
			Host string `json:"host"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(out), &ag); err != nil {
		return fmt.Errorf("agents --host %s: not JSON: %w", who.Host, err)
	}
	n := 0
	for _, a := range ag.Agents {
		if a.Host == who.Host {
			n++
		}
	}
	if n == 0 {
		fmt.Fprintf(o.Err, "warning: agents lists no agents on %s (none running?)\n", who.Host)
	} else {
		o.printf("smoke: agents lists %d on %s\n", n, who.Host)
	}

	isHub := who.Hub == "self" || who.Ledger
	for _, c := range [][]string{{"ledger"}, {"queue"}, {"sweep", "--dry-run"}} {
		_, serr, err := runBin(o, c...)
		name := strings.Join(c, " ")
		switch {
		case err == nil:
			o.printf("smoke: %s ok\n", name)
		case !isHub:
			fmt.Fprintf(o.Err, "warning: %s failed on a peer (the hub may be down or restarting): %v: %s\n", name, err, serr)
		case c[0] == "sweep" && netWords.MatchString(serr):
			fmt.Fprintf(o.Err, "warning: %s failed, apparently an unreachable host: %s\n", name, serr)
		default:
			return fmt.Errorf("%s: %v: %s", name, err, serr)
		}
	}

	jc := exec.Command(o.Journalctl, "--user", "-u", o.Service, "--since", since.Format("2006-01-02 15:04:05"), "-p", "err", "-q", "--no-pager")
	jc.Env = o.Env
	jout, jerr := jc.Output()
	switch {
	case jerr != nil:
		fmt.Fprintf(o.Err, "warning: could not read the journal: %v\n", jerr)
	case strings.TrimSpace(string(jout)) != "":
		return fmt.Errorf("the journal has errors since the restart:\n%s", strings.TrimSpace(string(jout)))
	default:
		o.printf("smoke: journal has no errors since the restart\n")
	}
	return nil
}

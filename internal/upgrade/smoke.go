package upgrade

import (
	"bytes"
	"encoding/json"
	"errors"
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

// lastWithoutVersionHeader is the newest release whose server does not
// send X-Comebye-Version; anything newer is expected to.
const lastWithoutVersionHeader = "v0.2.0"

// checkServedVersion asks the local server which binary version it runs
// (the X-Comebye-Version header of any /api response, a 401 included) and
// requires it to be the target. A target that predates the header cannot
// be checked: that passes with a note.
func checkServedVersion(o *Options, target string) error {
	got := ""
	resp, err := o.HTTP.Get(strings.TrimRight(o.LocalURL, "/") + "/api/version")
	if err == nil {
		resp.Body.Close()
		got = resp.Header.Get("X-Comebye-Version")
	}
	switch {
	case got == target:
		o.printf("smoke: the server reports version %s\n", got)
	case got == "" && compareVersions(target, lastWithoutVersionHeader) <= 0:
		o.printf("note: %s predates X-Comebye-Version, so the served version cannot be checked\n", target)
	case got == "":
		// No answer or no header: the server may still be starting.
		return fmt.Errorf("the server sends no X-Comebye-Version (%v); expected %s", orErr(err), target)
	default:
		return fatal(fmt.Errorf("the server reports version %s, expected %s: the restart did not pick up the new binary", got, target))
	}
	return nil
}

func orErr(err error) string {
	if err != nil {
		return err.Error()
	}
	return "header missing"
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

// retryDelays are the waits between the attempts of a transient smoke step:
// 5 attempts, 2+4+8+16 = 30s in all. A freshly started peer has to announce
// itself and be registered by the hub before the hub will answer it; that
// takes a few seconds (the worker tool allows the hub 90s to list a new
// node), so the waits double from 2s and stop at 30s in total.
var retryDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}

// errFatal marks an error that waiting cannot fix.
type errFatal struct{ err error }

func (e errFatal) Error() string { return e.err.Error() }
func (e errFatal) Unwrap() error { return e.err }

func fatal(err error) error { return errFatal{err} }

// retry runs fn up to len(delays)+1 times, sleeping between attempts, and
// logs each retry on one line. It stops at once on an errFatal and returns
// the last error after the final attempt.
func retry(o *Options, what string, delays []time.Duration, fn func() error) error {
	var err error
	for i := 0; ; i++ {
		if err = fn(); err == nil {
			return nil
		}
		var f errFatal
		if errors.As(err, &f) || i >= len(delays) {
			return err
		}
		o.printf("smoke: %s: attempt %d/%d failed (%s); retrying in %s\n", what, i+1, len(delays)+1, oneLine(err), delays[i])
		o.Sleep(delays[i])
	}
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
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
func smoke(o *Options, since time.Time, beforeTag, target string) error {
	if err := waitUp(o); err != nil {
		return err
	}
	if o.exitMode() {
		o.printf("smoke: the local server answers again\n")
	} else if o.launchdMode() {
		o.printf("smoke: %s is running\n", o.launchdTarget())
	} else {
		o.printf("smoke: %s is active\n", o.Service)
	}

	// The server needs a moment after "active"; retry the first call.
	var who struct {
		Host   string `json:"host"`
		Hub    string `json:"hub"`
		Ledger bool   `json:"ledger"`
	}
	err := retry(o, "whoami", retryDelays, func() error {
		out, serr, err := runBin(o, "whoami", "--json")
		if err != nil {
			if serr != "" {
				return fmt.Errorf("%v: %s", err, serr)
			}
			return err
		}
		return json.Unmarshal([]byte(out), &who)
	})
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

	if err := retry(o, "served version", retryDelays, func() error { return checkServedVersion(o, target) }); err != nil {
		return err
	}

	// Only this host's agents: other hosts may be offline.
	var out string
	err = retry(o, "agents", retryDelays, func() error {
		var serr string
		var err error
		out, serr, err = runBin(o, "agents", "--host", who.Host, "--json")
		if err != nil {
			err = fmt.Errorf("agents --host %s: %v: %s", who.Host, err, serr)
			if !transientAgents(serr) {
				return fatal(err)
			}
		}
		return err
	})
	if err != nil {
		return err
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

	if o.exitMode() {
		o.printf("smoke: no journal check (exit-restart mode: the supervisor keeps the server's log)\n")
		return nil
	}
	if o.launchdMode() {
		o.printf("smoke: no journal check (launchd keeps the server's log in the plist's StandardOutPath)\n")
		return nil
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

// transientAgents reports whether a failed "agents" call may succeed
// shortly: the peer is not registered with the hub yet (it announces itself
// after it starts), or the server or hub is not answering yet. A rejection
// for a reason that will not change (a tag mismatch, auth refused) is not.
func transientAgents(stderr string) bool {
	l := strings.ToLower(stderr)
	if strings.Contains(l, "tag_mismatch") || strings.Contains(l, "no longer carries the tag") || strings.Contains(l, "rejected this peer") {
		return false
	}
	return strings.Contains(l, "not a registered peer") || strings.Contains(l, "not_a_peer") || netWords.MatchString(l) || strings.Contains(l, "starting")
}

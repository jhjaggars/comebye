package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// signinSnap is the sign-in's state as the host answers it.
type signinSnap struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	State   string `json:"state"`
	URL     string `json:"url"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Method  string `json:"method"`
}

// cmdSignin is a thin client of POST/GET/DELETE /api/hosts/{host}/signin.
// The code is read from stdin, sent once and dropped: it is not printed,
// logged or put in an error.
func cmdSignin(e *Env, args []string) error {
	fs := flag.NewFlagSet("signin", flag.ContinueOnError)
	host := fs.String("host", "", "the host to sign in on (default: this host)")
	asJSON := fs.Bool("json", false, "machine-readable result")
	login := fs.String("login", "", "the Tailscale login to act as (default $COMEBYE_LOGIN, else the first of $COMEBYE_TS_ALLOW_LOGIN)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] != "claude" {
		return usageErr("want: comebye signin claude [--host H]")
	}
	c, err := newClient(e)
	if err != nil {
		return err
	}
	// Signing in is the user's alone: the call goes out as the user's login.
	if c.user = peerLogin(e, *login); c.user == "" {
		return usageErr("signin needs the user's login: pass --login you@example.com (or set COMEBYE_LOGIN)")
	}
	h := *host
	if h == "" {
		w, err := c.getWhoami()
		if err != nil {
			return err
		}
		h = w.Host
	}
	base := "/api/hosts/" + url.PathEscape(h) + "/signin"

	var snap signinSnap
	err = c.call("POST", base, nil, map[string]any{"agent": "claude"}, &snap)
	var ae *apiError
	if errors.As(err, &ae) && ae.status == 409 && ae.code == "busy" {
		// One is already running (another tab, or an earlier try): attach to it.
		var b struct {
			Session signinSnap `json:"session"`
		}
		if json.Unmarshal(ae.body, &b) != nil || b.Session.ID == "" {
			return err
		}
		snap = b.Session
		if snap.State != "waiting_code" {
			return fmt.Errorf("a sign-in of claude on %s is already running (%s); wait for it to end or cancel it", h, snap.State)
		}
		fmt.Fprintf(e.Stderr, "A sign-in of claude on %s is already running; continuing it.\n", h)
		err = nil
	}
	if err != nil {
		return err
	}
	return signinFlow(e, c, base, h, snap, *asJSON)
}

func signinFlow(e *Env, c *client, base, host string, snap signinSnap, asJSON bool) error {
	path := base + "/" + url.PathEscape(snap.ID)
	cancel := func() { _ = c.call("DELETE", path, nil, nil, nil) }
	if !asJSON {
		fmt.Fprintf(e.Stderr, "Signing claude in on %s. Open this link, sign in, and copy the code the page shows:\n\n%s\n\n", host, snap.URL)
		fmt.Fprint(e.Stderr, "Paste the code here: ")
	}
	line, rerr := bufio.NewReader(e.Stdin).ReadString('\n')
	code := strings.TrimSpace(line)
	if code == "" {
		cancel()
		if rerr != nil {
			return fmt.Errorf("no code was given; the sign-in was cancelled")
		}
		return fmt.Errorf("an empty code; the sign-in was cancelled")
	}
	err := c.call("POST", path+"/code", nil, map[string]any{"code": code}, &snap)
	code = ""
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.code == "bad_code" {
			cancel()
			return fmt.Errorf("that is not a code: %s; the sign-in was cancelled", ae.message)
		}
		cancel()
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for snap.State == "checking" {
		if time.Now().After(deadline) {
			cancel()
			return fmt.Errorf("the sign-in did not finish")
		}
		sleep(e, time.Second)
		if err := c.call("GET", path, nil, nil, &snap); err != nil {
			return err
		}
	}
	if asJSON {
		return rawJSONValue(e, snap)
	}
	if snap.State == "signed_in" {
		m := snap.Method
		if m == "" {
			m = "signed in"
		}
		fmt.Fprintf(e.Stdout, "claude is signed in on %s (%s).\n", host, m)
		return nil
	}
	msg := snap.Message
	if msg == "" {
		msg = "the sign-in failed"
	}
	return fmt.Errorf("claude is not signed in on %s: %s", host, msg)
}

func sleep(e *Env, d time.Duration) {
	if e.Sleep != nil {
		e.Sleep(d)
		return
	}
	time.Sleep(d)
}

func rawJSONValue(e *Env, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(e.Stdout, string(b))
	return nil
}

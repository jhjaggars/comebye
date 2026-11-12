package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// `worker preflight` answers one question for a caller that has not built
// anything yet: can this node reach the clouds it would build in? It is
// read-only (aws sts get-caller-identity, gcloud's local account list, the
// OAuth token exchange, a sandbox listing) and it never prints a key, a token
// or an account id: a note is a profile or gateway name or a fixed phrase.
//
//	{"v":1,"aws":{"state":"ok","note":"profile comebye-provision"}, "gcp":{...},
//	 "tailnet":{...}, "openshell":{"state":"ok","note":"...","gateway":"homelab"}, "ssh":{...}}
//
// state is ok, missing (nothing there to use) or expired (it was there; log in
// again), or error (it failed in some other way).

type pfCheck struct {
	State   string `json:"state"`
	Note    string `json:"note,omitempty"`
	Gateway string `json:"gateway,omitempty"` // openshell only
	// What a sandbox would be made from (settings, not credentials; openshell
	// only): the app shows them in place of a size picker.
	Workspace string `json:"workspace,omitempty"`
	Image     string `json:"image,omitempty"`
	Policy    string `json:"policy,omitempty"` // the policy file's name, not its path
	CPU       string `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
}

type pfDoc struct {
	V         int     `json:"v"`
	CheckedAt string  `json:"checked_at"`
	AWS       pfCheck `json:"aws"`
	GCP       pfCheck `json:"gcp"`
	Tailnet   pfCheck `json:"tailnet"`
	OpenShell pfCheck `json:"openshell"`
	SSH       pfCheck `json:"ssh"`
	// What this node is set up to build in (settings, not credentials): the
	// config's aws_region, and the regions a caller may pick.
	Region  string   `json:"region,omitempty"`
	Regions []string `json:"regions,omitempty"`
}

var (
	reNoCreds   = regexp.MustCompile(`(?i)unable to locate credentials|could not be found|profile .* not found|no credentials|executable file not found|not found in \$PATH|no such file`)
	reOSExpired = regexp.MustCompile(`(?i)unauthenticated|unauthorized|\b401\b|token.*(expired|invalid)|gateway login|re-?authenticat`)
)

// lookPath is exec.LookPath; tests replace it.
var lookPath = exec.LookPath

var defaultLookPath = exec.LookPath

func (e *env) preflight(ctx context.Context, asJSON bool) error {
	d := &pfDoc{V: 1, CheckedAt: e.now().UTC().Format(time.RFC3339)}
	d.AWS = e.pfAWS(ctx)
	d.GCP = e.pfGCP(ctx)
	d.Tailnet = e.pfTailnet(ctx)
	d.OpenShell = e.pfOpenShell(ctx)
	d.SSH = e.pfSSH()
	d.Region, d.Regions = e.Cfg.AWSRegion, e.Cfg.RegionsAllowed()
	if asJSON {
		b := marshalJSON(d)
		e.printf("%s\n", b)
		return nil
	}
	for _, r := range []struct {
		n string
		c pfCheck
	}{{"aws", d.AWS}, {"gcp", d.GCP}, {"tailnet", d.Tailnet}, {"openshell", d.OpenShell}, {"ssh", d.SSH}} {
		e.printf("%-10s %-8s %s\n", r.n, r.c.State, r.c.Note)
	}
	return nil
}

func (e *env) pfAWS(ctx context.Context) pfCheck {
	who := "default credentials"
	args := []string{"sts", "get-caller-identity", "--query", "Account", "--output", "text", "--no-cli-pager"}
	if p := e.Cfg.AWSProfile; p != "" {
		who = "profile " + p
		args = append(args, "--profile", p)
	}
	// The answer (the account id) is dropped: only success matters.
	_, errOut, err := e.Run.Run(ctx, "aws", args, nil)
	if err == nil {
		return pfCheck{State: "ok", Note: who}
	}
	msg := err.Error() + " " + errOut
	switch {
	case reNoCreds.MatchString(msg):
		return pfCheck{State: "missing", Note: who + ": no credentials"}
	case reExpired.MatchString(msg):
		return pfCheck{State: "expired", Note: who + ": the login expired; run aws login"}
	}
	return pfCheck{State: "error", Note: who + ": the identity check failed"}
}

func (e *env) pfGCP(ctx context.Context) pfCheck {
	out, errOut, err := e.Run.Run(ctx, "gcloud", []string{"auth", "list", "--filter=status:ACTIVE", "--format=value(account)"}, nil)
	if err != nil {
		if reNoCreds.MatchString(err.Error() + errOut) {
			return pfCheck{State: "missing", Note: "gcloud is not installed"}
		}
		if reExpired.MatchString(err.Error() + errOut) {
			return pfCheck{State: "expired", Note: "the login expired; run gcloud auth login"}
		}
		return pfCheck{State: "error", Note: "gcloud auth list failed"}
	}
	if strings.TrimSpace(out) == "" {
		return pfCheck{State: "missing", Note: "no active gcloud account"}
	}
	// The account (an email) is not echoed.
	if e.Cfg.Project != "" {
		return pfCheck{State: "ok", Note: "project " + e.Cfg.Project}
	}
	return pfCheck{State: "ok", Note: "an account is active; no project in the config"}
}

func (e *env) pfTailnet(ctx context.Context) pfCheck {
	id, secret, err := LoadOAuth(e.Cfg.TSOAuthFile)
	switch {
	case err != nil && os.IsNotExist(err):
		return pfCheck{State: "missing", Note: "no OAuth client file (a hand-minted key is not used by a provisioner)"}
	case err != nil:
		return pfCheck{State: "error", Note: "the OAuth client file is unusable (mode 0600, client_id and client_secret)"}
	}
	ts := &tsClient{base: e.Cfg.TSAPIBase, http: e.HTTP, id: id, secret: secret}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := ts.Devices(cctx); err != nil {
		return pfCheck{State: "error", Note: "the OAuth client was not accepted, or the Tailscale API is unreachable"}
	}
	return pfCheck{State: "ok", Note: "OAuth client accepted for " + e.Cfg.Tag}
}

func (e *env) pfOpenShell(ctx context.Context) pfCheck {
	pf := e.pfOpenShellState(ctx)
	c := e.Cfg
	pf.Workspace, pf.Image, pf.CPU, pf.Memory = c.OpenShellWorkspace, c.SandboxImage, c.SandboxCPU, c.SandboxMemory
	if c.SandboxPolicy != "" {
		pf.Policy = filepath.Base(c.SandboxPolicy)
	}
	return pf
}

func (e *env) pfOpenShellState(ctx context.Context) pfCheck {
	c := e.Cfg
	gw := c.OpenShellGateway
	if c.OpenShellEndpoint != "" {
		gw = "endpoint"
	}
	if c.OpenShellWorkspace == "" || c.SandboxImage == "" || (c.OpenShellGateway == "" && c.OpenShellEndpoint == "") {
		return pfCheck{State: "missing", Note: "no openshell gateway, workspace and image in the config", Gateway: gw}
	}
	cli := &osCLI{r: e.Run, cfg: c}
	if _, err := cli.list(ctx, ""); err != nil {
		switch {
		case reNoCreds.MatchString(err.Error()):
			return pfCheck{State: "missing", Note: "the openshell CLI is not installed", Gateway: gw}
		case reOSExpired.MatchString(err.Error()):
			return pfCheck{State: "expired", Note: "the gateway login expired; run openshell gateway login", Gateway: gw}
		}
		return pfCheck{State: "error", Note: "the gateway did not answer a sandbox listing", Gateway: gw}
	}
	return pfCheck{State: "ok", Note: "gateway answers", Gateway: gw}
}

func (e *env) pfSSH() pfCheck {
	for _, b := range []string{"ssh", "ssh-keygen"} {
		if _, err := lookPath(b); err != nil {
			return pfCheck{State: "missing", Note: b + " is not installed"}
		}
	}
	if _, err := os.Stat(e.Cfg.KeyPath()); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pfCheck{State: "ok", Note: "no ssh key yet: create makes one"}
		}
		return pfCheck{State: "error", Note: "the ssh key is unreadable"}
	}
	return pfCheck{State: "ok", Note: "ssh key present"}
}

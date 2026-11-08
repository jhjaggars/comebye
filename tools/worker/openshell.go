package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// The openshell provider: a worker is a sandbox on an OpenShell gateway, not a
// VM. The gateway (signed in as the operator through the `openshell` CLI's own
// OIDC login) creates, execs into, stops and deletes it. The tailnet join, the
// hub registration, the archive pull and the order of destroy are the same code
// as for a VM; only the resource steps and the Transport differ.
//
// This tool never stores, reads or prints the gateway credential: it shells out
// to the CLI, which holds the login. The sandbox never holds one either.

// osCLI runs the openshell CLI for one gateway and workspace.
type osCLI struct {
	r   Runner
	cfg *Config
}

func (e *env) osCLI() *osCLI { return &osCLI{r: e.Run, cfg: e.Cfg} }

func (c *osCLI) bin() string {
	if c.cfg.OpenShellBin != "" {
		return c.cfg.OpenShellBin
	}
	return "openshell"
}

// scope selects the gateway and workspace. The CLI takes these after the
// subcommand as well as before it.
func (c *osCLI) scope() []string {
	var a []string
	switch {
	case c.cfg.OpenShellEndpoint != "":
		a = append(a, "--gateway-endpoint", c.cfg.OpenShellEndpoint)
	case c.cfg.OpenShellGateway != "":
		a = append(a, "-g", c.cfg.OpenShellGateway)
	}
	return append(a, "--workspace", c.cfg.OpenShellWorkspace)
}

// args puts the gateway scope after the subcommand words: args("sandbox", "list")...
func (c *osCLI) args(sub []string, rest ...string) []string {
	return append(append(append([]string{}, sub...), c.scope()...), rest...)
}

func (c *osCLI) Run(ctx context.Context, args []string, stdin []byte) (string, string, error) {
	out, errOut, err := c.r.Run(ctx, c.bin(), args, stdin)
	if err != nil {
		err = c.explain(err, errOut)
	}
	return out, errOut, err
}

// explain adds the fix when the CLI refuses for lack of a login.
func (c *osCLI) explain(err error, errOut string) error {
	low := strings.ToLower(err.Error() + " " + errOut)
	if strings.Contains(low, "unauthenticated") || strings.Contains(low, "token expired") || strings.Contains(low, "not logged in") ||
		strings.Contains(low, "authentication failed") || strings.Contains(low, "login required") {
		gw := c.cfg.OpenShellGateway
		if gw == "" {
			gw = "NAME"
		}
		return fmt.Errorf("%w\nthe gateway wants a login: run `%s gateway login %s` (on a user-owned tailnet node; see docs/workers.md)", err, c.bin(), gw)
	}
	return err
}

// execArgs is `sandbox exec` running script inside a sandbox. The script (a
// shell string this program composed) is one argument; secrets are on stdin.
func (c *osCLI) execArgs(name string, tty bool, script string) []string {
	t := "--no-tty"
	if tty {
		t = "--tty"
	}
	a := c.args([]string{"sandbox", "exec"}, "-n", name, t, "--no-login-shell", "--")
	return append(a, sandboxScript(script)...)
}

// ---- sandbox JSON ----

// osSandbox is the CLI's `-o json` view of a sandbox.
type osSandbox struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	Phase     string            `json:"phase"`
	CreatedAt string            `json:"created_at"` // "YYYY-MM-DD HH:MM:SS" UTC
	ExitCode  *int              `json:"exit_code"`
}

func parseSandboxList(b string) ([]osSandbox, string, error) {
	var r struct {
		Sandboxes []osSandbox `json:"sandboxes"`
		Next      string      `json:"next_page_token"`
	}
	if err := json.Unmarshal([]byte(b), &r); err != nil {
		return nil, "", fmt.Errorf("openshell sandbox list: not JSON: %v", err)
	}
	return r.Sandboxes, r.Next, nil
}

// list returns every sandbox of the workspace, selector narrowing it.
func (c *osCLI) list(ctx context.Context, selector string) ([]osSandbox, error) {
	var all []osSandbox
	tok := ""
	for page := 0; page < 50; page++ {
		rest := []string{"-o", "json"}
		if selector != "" {
			rest = append(rest, "--selector", selector)
		}
		if tok != "" {
			rest = append(rest, "--page-token", tok)
		}
		out, _, err := c.Run(ctx, c.args([]string{"sandbox", "list"}, rest...), nil)
		if err != nil {
			return nil, err
		}
		sbs, next, err := parseSandboxList(out)
		if err != nil {
			return nil, err
		}
		all = append(all, sbs...)
		if next == "" {
			return all, nil
		}
		tok = next
	}
	return nil, fmt.Errorf("openshell sandbox list: more than 50 pages")
}

// phaseState maps a sandbox phase onto the states the shared commands know.
// Completed is the init's idle exit: the sandbox is kept and `start` brings it back.
func phaseState(phase string) (state, note string) {
	switch phase {
	case "Ready":
		return stateRunning, ""
	case "Stopped":
		return stateStopped, ""
	case "Completed":
		return stateStopped, "completed (the init exited after its idle time); start resumes it"
	}
	return strings.ToUpper(phase), ""
}

func (s osSandbox) inst(c *Config) Inst {
	st, note := phaseState(s.Phase)
	i := Inst{Name: s.Name, ID: s.ID, State: st, Type: c.OpenShellWorkspace, Managed: s.Labels[labelKey] == labelVal, Note: note}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s.CreatedAt, time.UTC); err == nil {
		i.Created = t
	}
	return i
}

// ---- Provider ----

// openshellProv is the Provider of a sandbox. The cloud steps (network, data
// disk, host key) do not exist: the gateway creates the volume with the sandbox.
type openshellProv struct {
	cli   *osCLI
	cfg   *Config
	Image string    // set by create once the comebye version is known
	wait  func(int) // seconds; the env's, so tests do not sleep
}

func (p *openshellProv) Kind() string { return "openshell" }

func (p *openshellProv) Describe(ctx context.Context, name string) (*Inst, error) {
	sbs, err := p.cli.list(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, s := range sbs {
		if s.Name == name && s.Phase != "Deleting" {
			i := s.inst(p.cfg)
			return &i, nil
		}
	}
	return nil, nil
}

func (p *openshellProv) Instances(ctx context.Context) ([]Inst, error) {
	sbs, err := p.cli.list(ctx, labelKey+"="+labelVal)
	if err != nil {
		return nil, err
	}
	var out []Inst
	for _, s := range sbs {
		if s.Phase != "Deleting" {
			out = append(out, s.inst(p.cfg))
		}
	}
	return out, nil
}

func (p *openshellProv) EnsureNetwork(context.Context, logf) error              { return nil }
func (p *openshellProv) EnsureDataDisk(context.Context, string, logf) error     { return nil }
func (p *openshellProv) Attach(context.Context, string) error                   { return nil }
func (p *openshellProv) DeleteDataDisk(context.Context, string) error           { return nil }
func (p *openshellProv) HostKey(context.Context, string, *Inst) (string, error) { return "", nil }
func (p *openshellProv) CleanupNetwork(context.Context, logf) error             { return nil }

// createArgs is the `sandbox create` command line. The init is the sandbox's
// main process (--detach: we do not attach to it).
func (p *openshellProv) createArgs(name string) []string {
	c := p.cfg
	a := []string{"--name", name, "--from", p.Image, "--label", labelKey + "=" + labelVal, "--detach", "--no-tty", "--no-auto-providers"}
	if c.SandboxPolicy != "" {
		a = append(a, "--policy", c.SandboxPolicy)
	}
	for _, pr := range c.SandboxProviders {
		a = append(a, "--provider", pr)
	}
	if c.SandboxCPU != "" {
		a = append(a, "--cpu", c.SandboxCPU)
	}
	if c.SandboxMemory != "" {
		a = append(a, "--memory", c.SandboxMemory)
	}
	a = append(a, "-o", "json", "--", "comebye-sandbox-init")
	return p.cli.args([]string{"sandbox", "create"}, a...)
}

func (p *openshellProv) Create(ctx context.Context, name, _ string) error {
	if p.Image == "" {
		return fmt.Errorf("no sandbox image")
	}
	_, _, err := p.cli.Run(ctx, p.createArgs(name), nil)
	return err
}

func (p *openshellProv) Start(ctx context.Context, name string) error {
	_, _, err := p.cli.Run(ctx, p.cli.args([]string{"sandbox", "start"}, name), nil)
	return err
}

func (p *openshellProv) Stop(ctx context.Context, name string) error {
	_, _, err := p.cli.Run(ctx, p.cli.args([]string{"sandbox", "stop"}, name), nil)
	return err
}

// Delete removes the sandbox and its volume, then waits until it is gone.
func (p *openshellProv) Delete(ctx context.Context, name string) error {
	if _, _, err := p.cli.Run(ctx, p.cli.args([]string{"sandbox", "delete"}, name), nil); err != nil {
		return err
	}
	for i := 0; i < 36; i++ {
		inst, err := p.Describe(ctx, name)
		if err != nil {
			return err
		}
		if inst == nil {
			return nil
		}
		p.wait(5)
	}
	return fmt.Errorf("sandbox %s is still there after 3 minutes", name)
}

func (p *openshellProv) List(ctx context.Context, w logf) error {
	insts, err := p.Instances(ctx)
	if err != nil {
		return err
	}
	w("sandboxes (workspace %s, %s=%s):\n", p.cfg.OpenShellWorkspace, labelKey, labelVal)
	for _, i := range insts {
		w("  %-24s %-9s %s\n", i.Name, i.State, i.ID)
	}
	if len(insts) == 0 {
		w("  (none)\n")
	}
	return nil
}

func (p *openshellProv) Plan(name, _ string, w logf) {
	w("   %s\n   (the gateway creates the sandbox with its volume; no network, disk or ssh key)\n", cmdLine(p.cli.bin(), p.createArgs(name)))
}

// openshellProvider is the typed provider of the current config.
func (e *env) openshellProvider() *openshellProv {
	if p, ok := e.prov().(*openshellProv); ok {
		return p
	}
	return nil
}

func sandboxImage(c *Config, version string) string {
	return strings.ReplaceAll(c.SandboxImage, "{version}", version)
}

// ---- guest paths and the env file ----

// sandboxEnvFile is the file the init waits for. It is KEY=VALUE lines the init
// sources: COMEBYE_* for comebye and SBX_* for the init itself.
func sandboxEnvFile(c *Config, name, home string) (string, error) {
	lines := [][2]string{
		{"COMEBYE_HOST_NAME", name},
		{"COMEBYE_TS_ALLOW_LOGIN", c.TSAllowLogin},
		{"COMEBYE_PUSH", "false"},
		{"COMEBYE_HUB", c.HubPeerURL},
		{"COMEBYE_ANNOUNCE", "1"},
		{"COMEBYE_LAUNCH_ROOTS", home + "/code"},
		{"COMEBYE_WORKTREE_ROOT", home + "/worktrees"},
		{"SBX_IDLE_MINUTES", fmt.Sprint(c.IdleMinutes)},
	}
	var b strings.Builder
	for _, kv := range lines {
		if !shellSafe.MatchString(kv[1]) {
			return "", fmt.Errorf("refusing %s=%q: characters outside the safe set", kv[0], kv[1])
		}
		fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
	}
	return b.String(), nil
}

// writeEnvScript installs stdin as ~/.config/comebye/env, mode 600, atomically
// (the init polls for the file and must not see half of it).
const writeEnvScript = `set -e; umask 077; d="$HOME/.config/comebye"; mkdir -p "$d"; cat >"$d/env.tmp"; mv "$d/env.tmp" "$d/env"`

// sandboxTS runs the tailscale CLI against the sandbox's own tailscaled.
const sandboxTS = `tailscale --socket="$HOME/.ts/tailscaled.sock"`

// ---- create ----

// ready waits for the sandbox's phase to be Ready, then for exec to answer.
func (e *env) sandboxReady(ctx context.Context, name string) error {
	pv := e.prov()
	err := e.poll(72, 5, "the sandbox to be Ready", func() (bool, error) {
		inst, err := pv.Describe(ctx, name)
		if err != nil {
			return false, err
		}
		if inst == nil {
			return false, fmt.Errorf("sandbox %s does not exist", name)
		}
		if inst.State == "ERROR" {
			return false, fmt.Errorf("sandbox %s is in the Error phase: see `%s logs %s`", name, e.osCLI().bin(), name)
		}
		return inst.State == stateRunning, nil
	})
	if err != nil {
		return err
	}
	return e.poll(24, 5, "exec to answer in the sandbox", func() (bool, error) {
		_, err := e.sh(ctx, name, "true", nil)
		return err == nil, nil
	})
}

func (e *env) createSandbox(ctx context.Context, name string, o createOpts) error {
	pv := e.openshellProvider()
	w := e.printf
	version, err := e.resolveVersion(ctx, o.Version)
	if err != nil {
		return err
	}
	pv.Image = sandboxImage(e.Cfg, version)
	w("worker %s (openshell): comebye %s, image %s\n", name, version, pv.Image)

	w("1/4 sandbox\n")
	inst, err := pv.Describe(ctx, name)
	if err != nil {
		return err
	}
	switch {
	case inst == nil:
		if err := pv.Create(ctx, name, ""); err != nil {
			return err
		}
	case checkOwned(inst) != nil:
		return checkOwned(inst)
	case inst.State == stateStopped:
		w("  sandbox %s: stopped, starting it\n", name)
		if err := pv.Start(ctx, name); err != nil {
			return err
		}
	default:
		w("  sandbox %s: %s\n", name, inst.State)
	}
	if err := e.sandboxReady(ctx, name); err != nil {
		return err
	}

	w("2/4 tailnet\n")
	if err := e.enrollSandbox(ctx, name); err != nil {
		return err
	}

	w("3/4 comebye settings\n")
	home, err := e.sh(ctx, name, `printf %s "$HOME"`, nil)
	if err != nil {
		return err
	}
	home = strings.TrimSpace(home)
	if !strings.HasPrefix(home, "/") || !shellSafe.MatchString(home) {
		return fmt.Errorf("the sandbox reports HOME=%q", home)
	}
	envf, err := sandboxEnvFile(e.Cfg, name, home)
	if err != nil {
		return err
	}
	if _, err := e.sh(ctx, name, writeEnvScript, []byte(envf)); err != nil {
		return fmt.Errorf("writing the settings file: %w", err)
	}

	w("4/4 hub\n")
	return e.awaitJoin(ctx, name)
}

// enrollSandbox joins the sandbox to the tailnet. tailscaled and Serve are the
// init's; this runs `tailscale up` with the key on stdin. The key is single
// use: it is minted only once tailscaled's socket exists.
func (e *env) enrollSandbox(ctx context.Context, name string) error {
	if out, err := e.sh(ctx, name, sandboxTS+" ip -4", nil); err == nil && strings.HasPrefix(strings.TrimSpace(out), "100.") {
		e.printf("  already on the tailnet\n")
		return nil
	}
	if err := e.poll(36, 5, "tailscaled in the sandbox", func() (bool, error) {
		_, err := e.sh(ctx, name, `test -S "$HOME/.ts/tailscaled.sock"`, nil)
		return err == nil, nil
	}); err != nil {
		return err
	}
	key, usedFile, err := e.authKey(ctx, name)
	if err != nil {
		return err
	}
	if usedFile != "" {
		defer os.Remove(usedFile)
	}
	up := fmt.Sprintf("%s up --auth-key=file:/dev/stdin --hostname=%s --advertise-tags=%s", sandboxTS, shellQuote(name), shellQuote(e.Cfg.Tag))
	if _, err := e.sh(ctx, name, up, []byte(key)); err != nil {
		return fmt.Errorf("tailscale up: %w", redact(err, key))
	}
	return nil
}

func (e *env) createSandboxPlan(name string, o createOpts) error {
	c := e.Cfg
	version := o.Version
	if version == "" {
		version = c.HubVersion
	}
	if version == "" {
		version = "<the hub's version, read from " + c.HubURL + "/api/whoami>"
	}
	pv := e.openshellProvider()
	pv.Image = sandboxImage(c, version)
	gw := c.OpenShellGateway
	if c.OpenShellEndpoint != "" {
		gw = c.OpenShellEndpoint
	}
	e.printf("DRY RUN: nothing is created, no key is minted, no host is contacted.\n\n")
	e.printf("worker      %s\nprovider    openshell (gateway %s, workspace %s)\nimage       %s\ncomebye     %s\ntailnet tag %s   hub %s\n\n",
		name, orDash(gw), c.OpenShellWorkspace, pv.Image, version, c.Tag, c.HubPeerURL)
	e.printf("1. the sandbox, created by the gateway with its volume:\n")
	pv.Plan(name, "", e.printf)
	e.printf("2. tailnet: the image's init runs tailscaled (userspace) and Tailscale Serve.\n")
	if _, err := os.Stat(c.TSOAuthFile); err == nil {
		e.printf("   OAuth client %s: delete an offline %s device named %s (never an untagged one), mint a single-use\n   pre-authorized %s key valid 10 minutes\n", c.TSOAuthFile, c.Tag, name, c.Tag)
	} else {
		e.printf("   no OAuth client at %s: a hidden prompt asks for a single-use key tagged %s\n", c.TSOAuthFile, c.Tag)
	}
	e.printf("   openshell sandbox exec: tailscale up --auth-key=file:/dev/stdin --hostname=%s --advertise-tags=%s   (key on stdin: <not shown>)\n", name, c.Tag)
	e.printf("3. ~/.config/comebye/env, over exec stdin (mode 600):\n")
	env, _ := sandboxEnvFile(c, name, "/sandbox")
	for _, l := range strings.Split(strings.TrimSpace(env), "\n") {
		e.printf("     %s\n", l)
	}
	e.printf("4. wait for the hub (%s) to list %s; if it does not, print:\n   comebye peer add %s %s --tag %s\n", c.HubURL, name, name, peerURL(c, name), c.Tag)
	e.printf("\nNo repo is cloned and no agent is signed in; sign in with `worker login %s claude|codex`.\n", name)
	return nil
}

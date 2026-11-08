package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/archive"
)

func (e *env) printf(format string, a ...any) { fmt.Fprintf(e.Out, format, a...) }

func (e *env) hub() *hubClient { return &hubClient{base: e.Cfg.HubURL, http: e.HTTP} }

func (e *env) wait(secs int) {
	if e.Sleep != nil {
		e.Sleep(secs)
		return
	}
	time.Sleep(time.Duration(secs) * time.Second)
}

// poll calls f every few seconds until it returns true, an error, or the
// attempts run out.
func (e *env) poll(tries, every int, what string, f func() (bool, error)) error {
	for i := 0; i < tries; i++ {
		ok, err := f()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		e.wait(every)
	}
	return fmt.Errorf("timed out waiting for %s", what)
}

func checkName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("worker name %q: want a lowercase DNS label (letters, digits, hyphens; at most 30 characters)", name)
	}
	return nil
}

// resolveVersion is the comebye release the worker gets: the flag, else the
// config pin, else whatever the hub runs. A hub on an unreleased build
// reports something that is not a release tag; that is refused, since the
// worker installs from a release tarball.
func (e *env) resolveVersion(ctx context.Context, flagVal string) (string, error) {
	v := flagVal
	src := "--comebye-version"
	if v == "" {
		v, src = e.Cfg.HubVersion, "hub_version"
	}
	if v == "" {
		var err error
		if v, err = e.hub().Version(ctx); err != nil {
			return "", err
		}
		src = "the hub's whoami"
	}
	if !versionRe.MatchString(v) {
		return "", fmt.Errorf("comebye version %q (from %s) is not a release tag like v0.3.0: pass --comebye-version for a release, or upgrade the hub to one", v, src)
	}
	return v, nil
}

// ---- create ----

type createOpts struct {
	Version string
}

func (e *env) create(ctx context.Context, name string, o createOpts) error {
	if err := checkName(name); err != nil {
		return err
	}
	if e.Cfg.Provider == "openshell" {
		if e.DryRun {
			return e.createSandboxPlan(name, o)
		}
		return e.createSandbox(ctx, name, o)
	}
	if e.DryRun {
		return e.createPlan(name, o)
	}
	pv := e.prov()
	w := e.printf

	// Version first: nothing is created if the hub's answer is unusable.
	version, err := e.resolveVersion(ctx, o.Version)
	if err != nil {
		return err
	}
	w("worker %s (%s): comebye %s\n", name, pv.Kind(), version)
	if err := e.llmPreflight(ctx); err != nil {
		return err
	}

	w("1/8 network\n")
	if err := pv.EnsureNetwork(ctx, w); err != nil {
		return err
	}

	w("2/8 ssh key\n")
	pub, err := ensureKey(ctx, e)
	if err != nil {
		return err
	}

	w("3/8 data disk\n")
	if err := pv.EnsureDataDisk(ctx, name, w); err != nil {
		return err
	}

	w("4/8 instance\n")
	inst, err := pv.Describe(ctx, name)
	if err != nil {
		return err
	}
	switch {
	case inst == nil:
		if err := pv.Create(ctx, name, pub); err != nil {
			return err
		}
	case checkOwned(inst) != nil:
		return checkOwned(inst)
	case inst.State == stateStopped:
		w("  instance %s: stopped, starting it\n", name)
		if err := pv.Start(ctx, name); err != nil {
			return err
		}
	default:
		w("  instance %s: %s\n", name, inst.State)
	}
	if err := pv.Attach(ctx, name); err != nil {
		return err
	}

	w("5/8 verified ssh\n")
	if err := e.connect(ctx, name); err != nil {
		return err
	}

	w("6/8 packages, tailscale, idle timer\n")
	rs, err := rootScript(e.Cfg)
	if err != nil {
		return err
	}
	if _, err := e.sh(ctx, name, "sudo bash -s", []byte(rs)); err != nil {
		return fmt.Errorf("root bootstrap: %w", err)
	}
	if _, err := e.sh(ctx, name, "sudo install -m 0755 /dev/stdin /usr/local/sbin/comebye-idle-check", []byte(idleCheck)); err != nil {
		return fmt.Errorf("installing the idle check: %w", err)
	}

	w("7/8 tailnet\n")
	if err := e.enroll(ctx, name); err != nil {
		return err
	}

	w("8/8 herdr, agent CLIs, comebye %s\n", version)
	us, err := userScript(e.Cfg, name, version)
	if err != nil {
		return err
	}
	if _, err := e.sh(ctx, name, "bash -s", []byte(us)); err != nil {
		return fmt.Errorf("user bootstrap: %w", err)
	}
	if err := e.provisionLLM(ctx, name); err != nil {
		return err
	}

	return e.awaitJoin(ctx, name)
}

// connect makes the worker reachable through its transport. For a sandbox
// that is only a check that it is Ready: the gateway authenticates every exec.
func (e *env) connect(ctx context.Context, name string) error {
	if e.Cfg.Provider == "openshell" {
		inst, err := e.prov().Describe(ctx, name)
		if err != nil {
			return err
		}
		if inst == nil {
			return fmt.Errorf("sandbox %s does not exist", name)
		}
		if inst.State != stateRunning {
			return fmt.Errorf("sandbox %s is %s", name, inst.State)
		}
		return nil
	}
	return e.connectVM(ctx, name)
}

// connectVM reads the VM's host key from guest attributes, writes the private
// ssh config and waits until ssh answers.
func (e *env) connectVM(ctx context.Context, name string) error {
	pv := e.prov()
	var ip, key string
	err := e.poll(72, 5, "the VM's ssh host key from the cloud API", func() (bool, error) {
		inst, err := pv.Describe(ctx, name)
		if err != nil {
			return false, err
		}
		if inst == nil {
			return false, fmt.Errorf("instance %s does not exist", name)
		}
		if inst.State != stateRunning || inst.IP == "" {
			return false, nil
		}
		ip = inst.IP
		k, err := pv.HostKey(ctx, name, inst)
		if err != nil || k == "" {
			return false, nil
		}
		key = k
		return true, nil
	})
	if err != nil {
		return err
	}
	if _, err := writeSSHConfig(e.Cfg, name, ip, key); err != nil {
		return err
	}
	e.printf("  host key verified through the %s API for %s\n", pv.Kind(), ip)
	err = e.poll(60, 5, "ssh", func() (bool, error) {
		_, err := e.sh(ctx, name, "true", nil)
		return err == nil, nil
	})
	if err != nil {
		return fmt.Errorf("%w\n%s", err, sshTimeoutHint(e.Cfg))
	}
	return nil
}

// sshTimeoutHint explains the usual cause of an ssh timeout on a VM that is
// running with a verified host key: the firewall admits ssh only from
// ssh_source_cidr, and a home or office address changes. The rule is made
// once and not rewritten from the config, so the fix has two parts.
func sshTimeoutHint(c *Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "the VM is running, but ssh did not answer. The firewall admits ssh only from ssh_source_cidr = %s.\n", c.SSHSourceCIDR)
	b.WriteString("If your public IP changed since create, find the new one (curl -s https://checkip.amazonaws.com), then\n")
	fmt.Fprintf(&b, "  1. set ssh_source_cidr=<new-ip>/32 in %s\n", DefaultConfigPath())
	switch c.Provider {
	case "aws":
		b.WriteString("  2. in the security group comebye-worker-ssh (tcp 22): add the new /32 and remove the old one\n")
	default:
		fmt.Fprintf(&b, "  2. gcloud compute firewall-rules update %s-ssh --source-ranges=<new-ip>/32\n", c.Network)
	}
	b.WriteString("then run the same command again (it resumes).")
	return b.String()
}

// enrolled reports whether tailscale on the VM is up already.
func (e *env) enrolled(ctx context.Context, name string) bool {
	out, err := e.sh(ctx, name, e.guest().TSIP, nil)
	return err == nil && strings.HasPrefix(strings.TrimSpace(out), "100.")
}

// readKeyFile reads a hand-minted key from a file that only we may read.
func readKeyFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is mode %o: chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if !strings.HasPrefix(k, "tskey-") || strings.ContainsAny(k, " \t\r\n") {
		return "", fmt.Errorf("%s does not hold a Tailscale key (tskey-...)", path)
	}
	return k, nil
}

// authKey returns a one-time tagged key: minted through the OAuth client
// (after clearing a stale offline device of the same name and tag); else read
// from the key file; else read at a hidden prompt; else, with no terminal,
// waited for as a key file. usedFile is set when the key came from a file,
// so the caller can delete it once the key has been used.
func (e *env) authKey(ctx context.Context, name string) (key, usedFile string, err error) {
	id, secret, oerr := LoadOAuth(e.Cfg.TSOAuthFile)
	if oerr != nil {
		if !os.IsNotExist(oerr) {
			return "", "", oerr
		}
		kf := e.Cfg.TSAuthKeyFile
		if k, err := readKeyFile(kf); err == nil {
			return k, kf, nil
		} else if !os.IsNotExist(err) {
			return "", "", err
		}
		e.printf("  no OAuth client at %s: a single-use key tagged %s, minted by hand in the admin console, is needed\n", e.Cfg.TSOAuthFile, e.Cfg.Tag)
		if k, err := e.ReadSecret("Tailscale auth key: "); err == nil && strings.TrimSpace(k) != "" {
			return strings.TrimSpace(k), "", nil
		}
		e.printf("\n  >>> WAITING FOR A TAILSCALE KEY (no terminal here for a prompt).\n"+
			"  >>> In your own shell, with the key hidden, run:\n"+
			"  >>>   umask 077; read -rs k; printf %%s \"$k\" > %s; unset k\n"+
			"  >>> It is read once and then deleted. Waiting up to 20 minutes.\n\n", kf)
		var k string
		err := e.poll(240, 5, "a Tailscale key in "+kf, func() (bool, error) {
			var err error
			k, err = readKeyFile(kf)
			if err != nil && !os.IsNotExist(err) {
				return false, err
			}
			return err == nil, nil
		})
		if err != nil {
			return "", "", err
		}
		return k, kf, nil
	}
	ts := &tsClient{base: e.Cfg.TSAPIBase, http: e.HTTP, id: id, secret: secret}
	devs, err := ts.Devices(ctx)
	if err != nil {
		return "", "", err
	}
	for _, d := range staleDevices(devs, name, e.Cfg.Tag) {
		e.printf("  removing the stale offline device %s (%s)\n", d.Name, d.ID)
		if err := ts.DeleteDevice(ctx, d.ID); err != nil {
			return "", "", err
		}
	}
	kid, key, err := ts.MintKeyOpts(ctx, e.Cfg.Tag, "comebye-worker "+name, e.Cfg.TSKeyEphemeral, e.Cfg.TSKeyMinutes)
	if err != nil {
		return "", "", err
	}
	eph := ""
	if e.Cfg.TSKeyEphemeral {
		eph = ", ephemeral"
	}
	e.printf("  minted a single-use %s key (id %s, valid %d minutes%s)\n", e.Cfg.Tag, kid, e.Cfg.TSKeyMinutes, eph)
	return key, "", nil
}

func (e *env) enroll(ctx context.Context, name string) error {
	if e.enrolled(ctx, name) {
		e.printf("  already on the tailnet\n")
	} else {
		key, usedFile, err := e.authKey(ctx, name)
		if err != nil {
			return err
		}
		if usedFile != "" {
			// A single-use key is spent by the first `up`, success or not;
			// never leave it on disk.
			defer os.Remove(usedFile)
		}
		// The key goes over stdin: never argv, a file, or a log line.
		up := fmt.Sprintf("sudo tailscale up --auth-key=file:/dev/stdin --hostname=%s --advertise-tags=%s",
			shellQuote(name), shellQuote(e.Cfg.Tag))
		if _, err := e.sh(ctx, name, up, []byte(key)); err != nil {
			return fmt.Errorf("tailscale up: %w", redact(err, key))
		}
	}
	if _, err := e.sh(ctx, name, "sudo tailscale set --operator="+shellQuote(e.Cfg.SSHUser), nil); err != nil {
		return err
	}
	if _, err := e.sh(ctx, name, "tailscale serve --bg 8741", nil); err != nil {
		return fmt.Errorf("tailscale serve: %w (are HTTPS certificates enabled for the tailnet?)", err)
	}
	return nil
}

func redact(err error, secret string) error {
	if secret == "" || err == nil {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), secret, "[redacted]"))
}

// awaitJoin waits for the hub to list the worker (self-registration), else
// prints the fallback the user runs on the hub.
func (e *env) awaitJoin(ctx context.Context, name string) error {
	e.printf("waiting for the hub to register %s ...\n", name)
	var p *hubPeer
	err := e.poll(18, 5, "the hub to list "+name, func() (bool, error) {
		var err error
		p, err = e.hub().Peer(ctx, name)
		if err != nil {
			return false, nil
		}
		return p != nil, nil
	})
	if err != nil {
		e.printf("The hub has not listed %s. If it is not set up for self-registration, add it by hand on the hub, as the user:\n\n  comebye peer add %s %s --tag %s\n\n",
			name, name, peerURL(e.Cfg, name), e.Cfg.Tag)
		return fmt.Errorf("worker %s is built and on the tailnet, but not yet a peer", name)
	}
	e.printf("hub: %s is %s (%d agents, %s)\n", name, p.Status, p.Agents, orDash(p.Version))
	// The hub ties ledger entries to this machine, not the name. A failure
	// leaves the hub's own id for the peer: still one per registration.
	if inst, err := e.prov().Describe(ctx, name); err == nil && inst != nil && inst.ID != "" {
		if err := e.hub().SetInstance(ctx, name, inst.ID); err != nil {
			e.printf("hub: could not record %s's instance id (%v); entries still follow this registration, but an old %s would not be told apart\n", name, err, name)
		} else {
			e.printf("hub: %s is instance %s\n", name, inst.ID)
		}
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- dry-run ----

func (e *env) createPlan(name string, o createOpts) error {
	c := e.Cfg
	version := o.Version
	if version == "" {
		version = c.HubVersion
	}
	if version == "" {
		version = "<the hub's version, read from " + c.HubURL + "/api/whoami>"
	}
	pub := "<contents of " + c.KeyPath() + ".pub>"
	if b, err := os.ReadFile(c.KeyPath() + ".pub"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 2 {
			pub = f[0] + " " + f[1] + " comebye-worker"
		}
	}
	e.printf("DRY RUN: nothing is created, no key is minted, no host is contacted.\n\n")
	e.printf("worker      %s\nprovider    %s\nmachine     %s, boot %d GB, data %d GB %s\ncomebye     %s\ntailnet tag %s   hub %s\n\n",
		name, c.Provider, c.MachineType, c.BootDiskGB, c.DataDiskGB, c.DiskType, version, c.Tag, c.HubPeerURL)
	e.printf("1. cloud resources (%s)\n", c.Provider)
	e.prov().Plan(name, pub, e.printf)
	e.printf("2. ssh key: ssh-keygen -t ed25519 at %s, only if absent; host key verified, known_hosts written to %s/known_hosts, StrictHostKeyChecking yes\n", c.KeyPath(), c.WorkerDir(name))
	e.printf("3. over ssh as root: apt packages (git jq bubblewrap socat curl ...), tailscale from its apt repo,\n   linger, /data/code, /data/worktrees, idle-poweroff timer (%d min, no interactive ssh session and no working agent)%s\n", c.IdleMinutes, maxRunNote(c))
	e.printf("4. tailnet:\n")
	if _, err := os.Stat(c.TSOAuthFile); err == nil {
		e.printf("   OAuth client %s: delete an offline %s device named %s (never an untagged one), mint a single-use\n   pre-authorized %s key valid 10 minutes\n", c.TSOAuthFile, c.Tag, name, c.Tag)
	} else {
		e.printf("   no OAuth client at %s: a hidden prompt asks for a single-use key tagged %s\n", c.TSOAuthFile, c.Tag)
	}
	e.printf("   ssh %s 'sudo tailscale up --auth-key=file:/dev/stdin --hostname=%s --advertise-tags=%s'   (key on stdin: <not shown>)\n", name, name, c.Tag)
	e.printf("   ssh %s 'sudo tailscale set --operator=%s'; ssh %s 'tailscale serve --bg 8741'\n", name, c.SSHUser, name)
	e.printf("5. over ssh as %s: herdr (%s%s) as a user service; agent CLIs installed, not signed in: %s (pi pinned at %s);\n   comebye %s from %s/download/<tag>/comebye_linux_<arch>.tar.gz, SHA256SUMS checked, then `comebye upgrade --version`\n   ~/.config/comebye/env:\n",
		c.SSHUser, c.HerdrInstall, map[bool]string{true: ", sha256 pinned", false: ", NOT pinned"}[c.HerdrSHA256 != ""], strings.Join(c.Agents, ", "), c.PiVersion, version, c.ReleasesURL)
	for _, l := range []string{
		"COMEBYE_HOST_NAME=" + name, "COMEBYE_TS_ALLOW_LOGIN=" + c.TSAllowLogin, "COMEBYE_PUSH=false",
		"COMEBYE_HUB=" + c.HubPeerURL, "COMEBYE_ANNOUNCE=1", "COMEBYE_LAUNCH_ROOTS=/data/code", "COMEBYE_WORKTREE_ROOT=/data/worktrees",
	} {
		e.printf("     %s\n", l)
	}
	if c.LLMGatewayURL != "" {
		e.printf("   LLM gateway %s (checked first, from this machine: /health/liveliness; nothing is created if it is down):\n"+
			"     POST /key/generate with the provisioning key from %s (never printed): alias %s, user_id %s,\n"+
			"       models %s, max_budget $%g per %s, rpm_limit %d; no expiry (revoked at destroy)\n"+
			"     the new key travels over ssh stdin into ~/.pi/agent/auth.json (mode 600, provider %s); never argv, user-data or a log\n"+
			"     ~/.pi/agent/models.json: provider %s -> %s/v1 (openai-completions), no maxTokens; settings.json: default model %s\n"+
			"     destroy deletes the key first (by alias) and checks the gateway refuses it\n",
			c.LLMGatewayURL, c.LLMProvisionerKeyFile, llmAlias(name), llmProvisionerUser,
			strings.Join(c.LLMModels, ", "), c.LLMMaxBudget, c.LLMBudgetDuration, c.LLMRPM,
			llmProvider, llmProvider, c.LLMGatewayURL, c.LLMModels[0])
	}
	e.printf("6. wait for the hub (%s) to list %s; if it does not, print:\n   comebye peer add %s %s --tag %s\n", c.HubURL, name, name, peerURL(c, name), c.Tag)
	e.printf("\nNo repo is cloned and no agent is signed in; sign in with `worker login %s claude|codex`.\n", name)
	return nil
}

// ---- status ----

func (e *env) status(ctx context.Context, name string) error {
	if name == "" {
		return e.statusAll(ctx)
	}
	if err := checkName(name); err != nil {
		return err
	}
	inst, err := e.prov().Describe(ctx, name)
	if err != nil {
		return err
	}
	if inst == nil {
		e.printf("%s: no such instance (%s)\n", name, e.Cfg.Provider)
		return nil
	}
	e.printf("%-11s %s (%s)  %s\n", e.Cfg.Provider, inst.State, inst.Type, orDash(inst.IP))
	if inst.Note != "" {
		e.printf("            %s\n", inst.Note)
	}
	e.printf("archive     host %s\n", archive.WorkerHost(name, inst.ID))
	if inst.State == stateRunning {
		if err := e.connect(ctx, name); err != nil {
			e.printf("%-11s unreachable: %v\n", e.reachWord(), err)
		} else {
			g := e.guest()
			ip, _ := e.sh(ctx, name, g.TSIP, nil)
			e.printf("tailnet     %s\n", orDash(strings.TrimSpace(ip)))
			v, _ := e.sh(ctx, name, g.Version, nil)
			active, _ := e.sh(ctx, name, g.Units, nil)
			e.printf("comebye     %s   %s: %s\n", orDash(strings.TrimSpace(v)), g.UnitsLabel, orDash(strings.TrimSpace(active)))
			integ, _ := e.sh(ctx, name, g.Integrations, nil)
			e.printf("herdr       integrations: %s\n", orDash(strings.ReplaceAll(strings.TrimSpace(integ), ";", ";  ")))
		}
	}
	e.tailnetLine(ctx, name)
	if p, err := e.hub().Peer(ctx, name); err != nil {
		e.printf("hub         unavailable: %v\n", err)
	} else if p == nil {
		e.printf("hub         not a peer\n")
	} else {
		hv, _ := e.hub().Version(ctx)
		mark := ""
		if hv != "" && p.Version != "" && hv != p.Version {
			mark = "   VERSION MISMATCH (hub " + hv + "): run `comebye upgrade` on the worker"
		}
		e.printf("hub         %s, %d agents, %s%s %s\n", p.Status, p.Agents, orDash(p.Version), mark, p.Error)
	}
	return nil
}

func shortType(s string) string { return filepath.Base(s) }

// maxRunNote describes the guest-side timer used where the cloud has no cap.
func maxRunNote(c *Config) string {
	if c.Provider == "aws" && c.MaxRunDuration != "" {
		return "; advisory guest timer powers off " + c.MaxRunDuration + " after boot"
	}
	return ""
}

func (e *env) tsClientIfConfigured() *tsClient {
	id, secret, err := LoadOAuth(e.Cfg.TSOAuthFile)
	if err != nil {
		return nil
	}
	return &tsClient{base: e.Cfg.TSAPIBase, http: e.HTTP, id: id, secret: secret}
}

func (e *env) tailnetLine(ctx context.Context, name string) {
	ts := e.tsClientIfConfigured()
	if ts == nil {
		e.printf("tailnet api not configured (no OAuth client)\n")
		return
	}
	devs, err := ts.Devices(ctx)
	if err != nil {
		e.printf("tailnet api %v\n", err)
		return
	}
	own := ownDevices(devs, name, e.Cfg.Tag)
	if len(own) == 0 {
		e.printf("tailnet api no %s device named %s\n", e.Cfg.Tag, name)
	}
	for _, d := range own {
		state := "offline since " + d.LastSeen
		if d.ConnectedToControl {
			state = "online"
		}
		e.printf("tailnet api %s %s\n", d.Name, state)
	}
}

func (e *env) statusAll(ctx context.Context) error {
	if err := e.prov().List(ctx, e.printf); err != nil {
		return err
	}
	if ts := e.tsClientIfConfigured(); ts != nil {
		devs, err := ts.Devices(ctx)
		if err != nil {
			return err
		}
		e.printf("tailnet devices tagged %s:\n", e.Cfg.Tag)
		for _, d := range devs {
			if hasTag(d, e.Cfg.Tag) {
				e.printf("  %-24s %v\n", d.Hostname, map[bool]string{true: "online", false: "offline"}[d.ConnectedToControl])
			}
		}
	}
	return nil
}

// ---- stop / start ----

func (e *env) stop(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := e.owned(ctx, name); err != nil {
		return err
	}
	if err := e.prov().Stop(ctx, name); err != nil {
		return err
	}
	e.printf("%s stopped. The tailnet node and the hub's peer entry are kept (the hub shows it unreachable). %s\n", name, e.stopNote())
	return nil
}

func (e *env) owned(ctx context.Context, name string) error {
	inst, err := e.prov().Describe(ctx, name)
	if err != nil {
		return err
	}
	if inst == nil {
		return fmt.Errorf("no instance %s (%s)", name, e.Cfg.Provider)
	}
	return checkOwned(inst)
}

func (e *env) start(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := e.owned(ctx, name); err != nil {
		return err
	}
	if err := e.prov().Start(ctx, name); err != nil {
		return err
	}
	// The external IP changes across a stop/start: rewrite the ssh config
	// from GCE again, with the host key GCE vouches for. A sandbox takes a
	// while to be Ready again.
	if e.Cfg.Provider == "openshell" {
		if err := e.sandboxReady(ctx, name); err != nil {
			return err
		}
	} else if err := e.connect(ctx, name); err != nil {
		return err
	}
	g := e.guest()
	if err := e.poll(24, 5, "comebye to be active", func() (bool, error) {
		out, _ := e.sh(ctx, name, g.Active, nil)
		return strings.TrimSpace(out) == "active", nil
	}); err != nil {
		return err
	}
	v, _ := e.sh(ctx, name, g.Version, nil)
	e.printf("%s is up, comebye %s\n", name, orDash(strings.TrimSpace(v)))
	if hv, err := e.hub().Version(ctx); err == nil && !strings.Contains(v, hv) {
		e.printf("warning: the hub runs %s; run `comebye upgrade --version %s` on %s\n", hv, hv, name)
	}
	return nil
}

// ---- destroy ----

type destroyOpts struct {
	DeleteDataDisk bool
	NoArchive      bool // lose the worker's sessions rather than save them first
}

func (e *env) destroy(ctx context.Context, name string, o destroyOpts) error {
	if err := checkName(name); err != nil {
		return err
	}
	if o.DeleteDataDisk && e.Cfg.Provider == "openshell" {
		return fmt.Errorf("--delete-data-disk: a sandbox has no separate data disk; its volume is deleted with it")
	}
	inst, err := e.prov().Describe(ctx, name)
	if err != nil {
		return err
	}
	if inst != nil {
		if err := checkOwned(inst); err != nil {
			return err
		}
	}
	archiving := !o.NoArchive && inst != nil
	if archiving && inst.State != stateRunning {
		return fmt.Errorf("destroy refused, nothing was changed: %s is %s and its sessions are on its %s. Run `worker start %s` first, or pass --no-archive to lose them", name, inst.State, e.storeWord(), name)
	}
	disk := "kept (" + dataDiskName(name) + ")"
	if o.DeleteDataDisk {
		disk = "DELETED (" + dataDiskName(name) + ")"
	}
	if e.Cfg.Provider == "openshell" {
		e.printf("This removes worker %s: the sandbox and its volume, its tailnet node, and the hub's peer entry.\nType the worker's name to confirm: ", name)
	} else {
		e.printf("This removes worker %s: the VM, its tailnet node, and the hub's peer entry. Data disk: %s.\nType the worker's name to confirm: ", name, disk)
	}
	line, _ := bufio.NewReader(e.In).ReadString('\n')
	if strings.TrimSpace(line) != name {
		return fmt.Errorf("confirmation did not match: nothing was changed")
	}
	switch {
	case archiving:
		if err := e.archiveBeforeDestroy(ctx, inst); err != nil {
			return err
		}
	case inst == nil:
		e.printf("  no %s exists, so there are no sessions to save\n", e.noun())
	default:
		e.printf("  --no-archive: this worker's sessions are NOT being saved\n")
	}
	var failed []string
	// The worker's LLM key goes first: it is the one thing here that is
	// useful to anyone who has it.
	if p := e.revokeLLM(ctx, name, inst != nil && inst.State == stateRunning); p != "" {
		failed = append(failed, p)
	}
	step := func(what string, err error) {
		if err != nil {
			failed = append(failed, what)
			e.printf("  %s: %v\n", what, err)
			return
		}
		e.printf("  %s: ok\n", what)
	}

	if inst != nil && inst.State == stateRunning {
		if err := e.connect(ctx, name); err != nil {
			step("reach the "+e.noun(), err)
		} else {
			// Best effort: sign the agents out and stop services, then leave the tailnet.
			g := e.guest()
			e.sh(ctx, name, g.StopServices, nil)
			e.sh(ctx, name, g.AgentLogout, nil)
			_, err := e.sh(ctx, name, g.TSLogout, nil)
			step("tailscale logout", err)
		}
	} else if inst != nil {
		e.printf("  the %s is %s: skipping the on-host sign-out and logout (the node is removed through the API below)\n", e.noun(), inst.State)
	}

	if ts := e.tsClientIfConfigured(); ts != nil {
		devs, err := ts.Devices(ctx)
		if err == nil {
			for _, d := range ownDevices(devs, name, e.Cfg.Tag) {
				step("delete tailnet device "+d.Name, ts.DeleteDevice(ctx, d.ID))
			}
		} else {
			step("list tailnet devices", err)
		}
	} else {
		e.printf("  no OAuth client: remove the node %s from https://login.tailscale.com/admin/machines if it is still listed\n", name)
	}

	if err := e.hub().RemovePeer(ctx, name); err != nil {
		e.printf("  hub peer entry: %v\n  remove it by hand on the hub: comebye peer remove %s\n", err, name)
		failed = append(failed, "hub peer entry")
	} else {
		e.printf("  hub peer entry: removed\n")
	}

	if inst != nil {
		step("delete instance", e.prov().Delete(ctx, name))
	}
	if o.DeleteDataDisk {
		step("delete data disk", e.prov().DeleteDataDisk(ctx, name))
	}
	step("network cleanup", e.prov().CleanupNetwork(ctx, e.printf))
	os.RemoveAll(e.Cfg.WorkerDir(name))
	if len(failed) > 0 {
		return fmt.Errorf("destroy finished with problems: %s", strings.Join(failed, ", "))
	}
	if archiving {
		e.reindex(ctx)
	}
	e.printf("%s destroyed.\n", name)
	return nil
}

// reindex starts the archive service now, so that the worker's sessions are
// searchable in minutes rather than at the next hourly run. The saved sessions are
// safe either way, so a host without the service only gets a note.
func (e *env) reindex(ctx context.Context) {
	if _, _, err := e.Run.Run(ctx, "systemctl", []string{"--user", "start", "--no-block", "comebye-archive.service"}, nil); err != nil {
		e.printf("  index: not started (%v); the next `comebye archive index` finds this worker's sessions\n", err)
		return
	}
	e.printf("  index: rebuilding in the background, for search\n")
}

// ---- login ----

func (e *env) login(ctx context.Context, name, agent string) error {
	if err := checkName(name); err != nil {
		return err
	}
	var cmd string
	switch agent {
	case "claude":
		cmd = "claude auth login"
	case "codex":
		cmd = "codex login"
	case "pi":
		cmd = "pi"
	default:
		return fmt.Errorf("agent %q: want pi, claude or codex", agent)
	}
	if err := e.owned(ctx, name); err != nil {
		return err
	}
	if err := e.connect(ctx, name); err != nil {
		return err
	}
	e.printf("Signing in to %s on %s. Open the URL it prints in your own browser; the credential is created on the worker and copies nothing from this machine.\n", agent, name)
	if agent == "pi" {
		e.printf("pi has no login command: it opens now; type /login, choose a provider, and /quit when done. (Workers on the LLM gateway need no login: their key is delivered by create.)\n")
	}
	remote := `PATH="$HOME/.local/bin:$PATH"; ` + cmd
	return e.tr().Interactive(ctx, name, remote)
}

package main

import (
	"context"
	"io"
)

// Transport is how the tool runs commands on a worker once it exists: ssh to
// a VM, or `openshell sandbox exec` into a sandbox. Everything after the
// resource steps (tailnet join, bootstrap, archive pull, sign-out) goes
// through it, so those steps are the same for every provider. script is a
// shell command string this program composed, never the user's input; secrets
// go in stdin only.
type Transport interface {
	// Exec runs script and returns its stdout.
	Exec(ctx context.Context, name, script string, stdin []byte) (string, error)
	// Stream runs script with its stdout going to w (output too large to hold).
	Stream(ctx context.Context, name, script string, w io.Writer) error
	// Interactive runs script attached to this terminal.
	Interactive(ctx context.Context, name, script string) error
}

// sshTransport reaches a VM over ssh with the private, host-key-pinned config
// that connect wrote.
type sshTransport struct{ e *env }

func (t sshTransport) Exec(ctx context.Context, name, script string, stdin []byte) (string, error) {
	return ssh(ctx, t.e, name, script, stdin)
}

func (t sshTransport) Stream(ctx context.Context, name, script string, w io.Writer) error {
	return runStream(ctx, t.e.Run, "ssh", sshArgs(t.e.Cfg, name, false, script), w)
}

func (t sshTransport) Interactive(ctx context.Context, name, script string) error {
	return t.e.Run.Interactive(ctx, "ssh", sshArgs(t.e.Cfg, name, true, script))
}

// osTransport reaches a sandbox through the gateway: the operator's own OIDC
// login (kept by the openshell CLI) authenticates every call.
type osTransport struct{ cli *osCLI }

// sandboxScript runs script under bash with the user's own bin first on PATH
// (an upgraded comebye lives in ~/.local/bin).
func sandboxScript(script string) []string {
	return []string{"bash", "-c", `export PATH="$HOME/.local/bin:$PATH"; ` + script}
}

func (t osTransport) Exec(ctx context.Context, name, script string, stdin []byte) (string, error) {
	out, _, err := t.cli.Run(ctx, t.cli.execArgs(name, false, script), stdin)
	return out, err
}

func (t osTransport) Stream(ctx context.Context, name, script string, w io.Writer) error {
	return runStream(ctx, t.cli.r, t.cli.bin(), t.cli.execArgs(name, false, script), w)
}

func (t osTransport) Interactive(ctx context.Context, name, script string) error {
	return t.cli.r.Interactive(ctx, t.cli.bin(), t.cli.execArgs(name, true, script))
}

// tr is the transport for the configured provider.
func (e *env) tr() Transport {
	if e.Cfg.Provider == "openshell" {
		return osTransport{cli: e.osCLI()}
	}
	return sshTransport{e}
}

// sh runs script on the worker through its transport.
func (e *env) sh(ctx context.Context, name, script string, stdin []byte) (string, error) {
	return e.tr().Exec(ctx, name, script, stdin)
}

// guest holds the few commands that differ between a VM (systemd, sudo) and a
// sandbox (an init script, no root, tailscaled's socket under ~/.ts).
type guest struct {
	StopServices  string // stop comebye and herdr (agents stop writing sessions)
	StartServices string // bring them back; a failed archive uses it
	TSIP          string // prints the node's tailnet IPv4
	TSLogout      string
	AgentLogout   string // best effort sign-out of claude and codex
	Version       string // prints comebye's version
	Active        string // prints "active" when comebye runs
	// status lines
	Units, UnitsLabel string // what runs: prints a short list
	Integrations      string // herdr integration status: "pi: current;  claude: ..."
}

func (e *env) guest() guest {
	if e.Cfg.Provider == "openshell" {
		ts := `tailscale --socket="$HOME/.ts/tailscaled.sock"`
		return guest{
			// The init restarts what dies, so "stop" is herdr server stop (ends the panes and their
			// agents) and "start" waits for the init to bring herdr back.
			StopServices:  `herdr server stop`,
			StartServices: `for i in $(seq 1 30); do [ -S "$HOME/.config/herdr/herdr.sock" ] && exit 0; sleep 1; done; exit 1`,
			TSIP:          ts + " ip -4",
			TSLogout:      ts + " logout",
			AgentLogout:   `claude auth logout; codex logout; true`,
			Version:       "comebye -version",
			Active:        `pgrep -x comebye >/dev/null && echo active`,
			Units:         `for p in tailscaled herdr comebye; do pgrep -x "$p" >/dev/null && printf '%s ' "$p"; done`,
			UnitsLabel:    "running",
			Integrations:  herdrIntegrations,
		}
	}
	return guest{
		StopServices:  "systemctl --user stop comebye herdr",
		StartServices: "systemctl --user start herdr comebye",
		TSIP:          "tailscale ip -4",
		TSLogout:      "sudo tailscale logout",
		AgentLogout:   `PATH="$HOME/.local/bin:$PATH"; claude auth logout; codex logout; true`,
		Version:       "$HOME/.local/bin/comebye -version",
		Active:        "systemctl --user is-active comebye",
		Units:         "systemctl --user is-active comebye herdr | paste -sd' '",
		UnitsLabel:    "units (comebye herdr)",
		Integrations:  herdrIntegrations,
	}
}

// herdrIntegrations lists herdr's integrations (pi, claude, codex), one line.
const herdrIntegrations = `PATH="$HOME/.local/bin:$PATH"; herdr integration status 2>/dev/null | grep -E '^(pi|claude|codex):' | sed 's/ (\/.*//' | paste -sd';'`

// reachWord names how status reaches the worker.
func (e *env) reachWord() string {
	if e.Cfg.Provider == "openshell" {
		return "exec"
	}
	return "ssh"
}

func (e *env) noun() string {
	if e.Cfg.Provider == "openshell" {
		return "sandbox"
	}
	return "VM"
}

func (e *env) storeWord() string {
	if e.Cfg.Provider == "openshell" {
		return "volume"
	}
	return "boot disk"
}

func (e *env) stopNote() string {
	if e.Cfg.Provider == "openshell" {
		return "The volume is kept."
	}
	return "Disks are still billed."
}

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The host key is published by the VM to GCE guest attributes at boot and
// read here through the authenticated GCE API. known_hosts is written from
// it, so the first ssh connection never trusts the network.

var hostKeyRe = regexp.MustCompile(`^ssh-ed25519 [A-Za-z0-9+/]+={0,2}( [^\r\n]*)?$`)

// parseHostKey keeps only "ssh-ed25519 <base64>" from a guest attribute.
func parseHostKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !hostKeyRe.MatchString(s) {
		return "", fmt.Errorf("guest attribute is not an ssh-ed25519 host key")
	}
	f := strings.Fields(s)
	return f[0] + " " + f[1], nil
}

func writeKnownHosts(dir, ip, key string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "known_hosts")
	return p, os.WriteFile(p, []byte(ip+" "+key+"\n"), 0o600)
}

// sshConfig is a private ssh_config for one worker: strict host key
// checking against the file above, our key only, no agent, no other
// known_hosts.
func sshConfig(c *Config, name, ip, knownHosts string) string {
	return fmt.Sprintf(`Host %[1]s
  HostName %[2]s
  User %[3]s
  IdentityFile %[4]s
  IdentitiesOnly yes
  IdentityAgent none
  UserKnownHostsFile %[5]s
  GlobalKnownHostsFile /dev/null
  StrictHostKeyChecking yes
  HostKeyAlgorithms ssh-ed25519
  PasswordAuthentication no
  ServerAliveInterval 30
  ConnectTimeout 15
`, name, ip, c.SSHUser, c.KeyPath(), knownHosts)
}

func writeSSHConfig(c *Config, name, ip, key string) (string, error) {
	dir := c.WorkerDir(name)
	kh, err := writeKnownHosts(dir, ip, key)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "ssh_config")
	return p, os.WriteFile(p, []byte(sshConfig(c, name, ip, kh)), 0o600)
}

// ensureKey makes the provisioner's ed25519 key if there is none, and
// returns the public half.
func ensureKey(ctx context.Context, e *env) (string, error) {
	kp := e.Cfg.KeyPath()
	if _, err := os.Stat(kp); err != nil {
		if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
			return "", err
		}
		if _, _, err := e.Run.Run(ctx, "ssh-keygen", []string{"-q", "-t", "ed25519", "-N", "", "-C", "comebye-worker", "-f", kp}, nil); err != nil {
			return "", err
		}
	}
	b, err := os.ReadFile(kp + ".pub")
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(b))
	if len(f) < 2 || f[0] != "ssh-ed25519" {
		return "", fmt.Errorf("%s.pub is not an ed25519 public key", kp)
	}
	return f[0] + " " + f[1] + " comebye-worker", nil
}

// sshArgs is the ssh command line for running remote (a command string) on
// the worker. The remote string is composed by this program, never by the
// user's input.
func sshArgs(c *Config, name string, tty bool, remote string) []string {
	a := []string{"-F", filepath.Join(c.WorkerDir(name), "ssh_config")}
	if tty {
		a = append(a, "-t")
	} else {
		a = append(a, "-o", "BatchMode=yes")
	}
	return append(a, name, remote)
}

func ssh(ctx context.Context, e *env, name, remote string, stdin []byte) (string, error) {
	out, _, err := e.Run.Run(ctx, "ssh", sshArgs(e.Cfg, name, false, remote), stdin)
	return out, err
}

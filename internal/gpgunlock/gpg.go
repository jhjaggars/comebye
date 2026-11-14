// Package gpgunlock is the host side of GPG unlock with a passkey.
//
// The user's GPG passphrase is stored on the host only as an AES-GCM blob
// under a key K that exists only after a passkey assertion (WebAuthn PRF, in
// the browser). To unlock, the host makes a signed request, the browser
// answers it (assertion plus K sealed to the request's one-use ECDH key), and
// the host verifies the assertion itself against the passkeys it pinned at
// setup, opens K, decrypts the blob and presets the passphrase into
// gpg-agent. A timer ends the unlock after a fixed lifetime.
//
// Formats (the browser builds the first two; see sealed.go and blob.go):
//
//	request   canonical JSON object, signed by the host key (request.go)
//	challenge SHA-256("comebye-approval-v1\0" || request bytes)
//	PRF salt  SHA-256("comebye-prf-v1\0" || host || "\0" || keygrips joined by ",")
//	K         HKDF-SHA256(PRF output, info "comebye-gpg-k-v1"), 32 bytes
//	sealed    base64url(JSON{v,curve,epk,iv,ct}): K sealed to the request's pub
//	blob      AES-256-GCM of the passphrase under K, host and keygrips as AAD
package gpgunlock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GPG runs the gpg tools against one GNUPGHOME ("" is the user's default).
// Tests set Home to a temp directory: nothing here ever names ~/.gnupg.
type GPG struct {
	Home string
	// Bin, ConnectBin and ConfBin default to gpg, gpg-connect-agent, gpgconf.
	Bin, ConnectBin, ConfBin string
	// PresetBin is gpg-preset-passphrase; found by Preset when empty.
	PresetBin string
}

func (g *GPG) bin(name, def string) string {
	if name != "" {
		return name
	}
	return def
}

func (g *GPG) run(stdin []byte, bin string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ()
	if g.Home != "" {
		cmd.Env = append(cmd.Env, "GNUPGHOME="+g.Home)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), fmt.Errorf("%s: %s", filepath.Base(bin), msg)
	}
	return out.Bytes(), nil
}

// Available reports whether gpg and its agent tools can be run.
func (g *GPG) Available() bool {
	for _, b := range []string{g.bin(g.Bin, "gpg"), g.bin(g.ConnectBin, "gpg-connect-agent"), g.bin(g.ConfBin, "gpgconf")} {
		if _, err := exec.LookPath(b); err != nil {
			return false
		}
	}
	return true
}

// KeyInfo is one line of the agent's KEYINFO --list.
type KeyInfo struct {
	Grip   string
	Cached bool
}

// ParseKeyInfo reads `gpg-connect-agent 'KEYINFO --list' /bye`. Each key is
// "S KEYINFO <grip> <type> <serialno> <idstr> <cached> ...": the seventh
// field is 1 when the passphrase is cached and - when it is not.
func ParseKeyInfo(out string) []KeyInfo {
	var keys []KeyInfo
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) < 7 || f[0] != "S" || f[1] != "KEYINFO" {
			continue
		}
		keys = append(keys, KeyInfo{Grip: strings.ToUpper(f[2]), Cached: f[6] == "1"})
	}
	return keys
}

// KeyInfo lists the agent's keys and whether each passphrase is cached.
func (g *GPG) KeyInfo() ([]KeyInfo, error) {
	out, err := g.run(nil, g.bin(g.ConnectBin, "gpg-connect-agent"), "KEYINFO --list", "/bye")
	if err != nil {
		return nil, err
	}
	return ParseKeyInfo(string(out)), nil
}

// Locked reports whether any of the keygrips has no cached passphrase. A
// keygrip the agent does not list counts as locked.
func (g *GPG) Locked(grips []string) (bool, error) {
	info, err := g.KeyInfo()
	if err != nil {
		return false, err
	}
	cached := map[string]bool{}
	for _, k := range info {
		cached[k.Grip] = k.Cached
	}
	for _, gr := range grips {
		if !cached[strings.ToUpper(gr)] {
			return true, nil
		}
	}
	return false, nil
}

// ParseKeygrips reads `gpg --list-secret-keys --with-keygrip --with-colons`
// and returns the keygrips of the primary key and subkeys that can sign.
func ParseKeygrips(colons string) []string {
	var grips []string
	signs := false
	for _, ln := range strings.Split(colons, "\n") {
		f := strings.Split(ln, ":")
		switch f[0] {
		case "sec", "ssb":
			signs = len(f) > 11 && strings.Contains(f[11], "s")
		case "grp":
			if signs && len(f) > 9 && f[9] != "" {
				grips = append(grips, strings.ToUpper(f[9]))
			}
			signs = false
		}
	}
	return grips
}

// Keygrips returns the signing keygrips of the secret key fpr.
func (g *GPG) Keygrips(fpr string) ([]string, error) {
	out, err := g.run(nil, g.bin(g.Bin, "gpg"), "--batch", "--list-secret-keys", "--with-keygrip", "--with-colons", fpr)
	if err != nil {
		return nil, fmt.Errorf("no secret key %s here: %v", fpr, err)
	}
	grips := ParseKeygrips(string(out))
	if len(grips) == 0 {
		return nil, fmt.Errorf("the secret key %s has no signing key", fpr)
	}
	return grips, nil
}

// presetTool finds gpg-preset-passphrase: it is not on PATH on Fedora, so
// look in gpgconf's libexecdir and the usual places.
func (g *GPG) presetTool() (string, error) {
	if g.PresetBin != "" {
		return g.PresetBin, nil
	}
	if p, err := exec.LookPath("gpg-preset-passphrase"); err == nil {
		return p, nil
	}
	var dirs []string
	if out, err := g.run(nil, g.bin(g.ConfBin, "gpgconf"), "--list-dirs", "libexecdir"); err == nil {
		dirs = append(dirs, strings.TrimSpace(string(out)))
	}
	dirs = append(dirs, "/usr/libexec", "/usr/lib/gnupg", "/usr/lib/gnupg2", "/usr/libexec/gnupg")
	for _, d := range dirs {
		p := filepath.Join(d, "gpg-preset-passphrase")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("gpg-preset-passphrase not found (it ships with gnupg, in gpgconf's libexecdir)")
}

// ErrBadPassphrase is a passphrase the key rejects.
var ErrBadPassphrase = errors.New("that passphrase does not unlock the key")

// Preset caches the passphrase for a keygrip. It goes to the tool on stdin,
// never argv. The agent does not check it: use TestSign.
func (g *GPG) Preset(grip string, pass []byte) error {
	if bytes.ContainsAny(pass, "\r\n") || len(pass) == 0 {
		return errors.New("the passphrase must be one line")
	}
	tool, err := g.presetTool()
	if err != nil {
		return err
	}
	_, err = g.run(append(append([]byte(nil), pass...), '\n'), tool, "--preset", grip)
	return err
}

// Forget drops a keygrip's cached passphrase at once. gpg-preset-passphrase
// --forget works but, on gnupg 2.4.9, exits 2 with "No inquire callback in
// IPC" after clearing, so its status is not trusted: the agent is asked, and
// a key still cached is cleared through the agent's own CLEAR_PASSPHRASE.
func (g *GPG) Forget(grip string) error {
	tool, err := g.presetTool()
	if err != nil {
		return err
	}
	_, toolErr := g.run(nil, tool, "--forget", grip)
	if !g.cached(grip) {
		return nil
	}
	_, _ = g.run(nil, g.bin(g.ConnectBin, "gpg-connect-agent"), "CLEAR_PASSPHRASE --mode=normal "+grip, "/bye")
	if g.cached(grip) {
		if toolErr != nil {
			return toolErr
		}
		return errors.New("the passphrase is still cached after --forget")
	}
	return nil
}

func (g *GPG) cached(grip string) bool {
	info, err := g.KeyInfo()
	if err != nil {
		return true
	}
	for _, k := range info {
		if k.Grip == strings.ToUpper(grip) {
			return k.Cached
		}
	}
	return false
}

// ForgetAll drops every grip, returning the first error after trying all.
func (g *GPG) ForgetAll(grips []string) error {
	var first error
	for _, gr := range grips {
		if err := g.Forget(gr); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// TestSign signs a few bytes with the key without ever asking for a
// passphrase (--pinentry-mode error): it works only when the cache is set.
func (g *GPG) TestSign(fpr string) error {
	_, err := g.run([]byte("comebye gpg unlock check\n"), g.bin(g.Bin, "gpg"),
		"--batch", "--yes", "--pinentry-mode", "error", "--local-user", fpr, "--detach-sign", "--output", "-")
	return err
}

// ConfigureAgent makes gpg-agent accept presets and caps their life: it adds
// allow-preset-passphrase and sets max-cache-ttl to lifetime in
// gpg-agent.conf (other lines, pinentry-program included, stay), then reloads
// the agent. A preset passphrase ignores default-cache-ttl and ends at
// max-cache-ttl, so this is the backstop under the lifetime timer.
func (g *GPG) ConfigureAgent(lifetime time.Duration) error {
	home := g.Home
	if home == "" {
		out, err := g.run(nil, g.bin(g.ConfBin, "gpgconf"), "--list-dirs", "homedir")
		if err != nil {
			return err
		}
		home = strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	path := filepath.Join(home, "gpg-agent.conf")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out := SetAgentConf(string(old), lifetime)
	if out != string(old) {
		if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
			return err
		}
	}
	_, _ = g.run(nil, g.bin(g.ConfBin, "gpgconf"), "--reload", "gpg-agent")
	return nil
}

// SetAgentConf returns conf with allow-preset-passphrase and max-cache-ttl
// set, changing nothing else.
func SetAgentConf(conf string, lifetime time.Duration) string {
	ttl := fmt.Sprintf("max-cache-ttl %d", int(lifetime/time.Second))
	var lines []string
	haveAllow, haveTTL := false, false
	for _, ln := range strings.Split(strings.TrimRight(conf, "\n"), "\n") {
		f := strings.Fields(ln)
		switch {
		case len(f) > 0 && f[0] == "allow-preset-passphrase":
			haveAllow = true
		case len(f) > 0 && f[0] == "max-cache-ttl":
			haveTTL = true
			ln = ttl
		}
		lines = append(lines, ln)
	}
	if conf == "" {
		lines = nil
	}
	if !haveAllow {
		lines = append(lines, "allow-preset-passphrase")
	}
	if !haveTTL {
		lines = append(lines, ttl)
	}
	return strings.Join(lines, "\n") + "\n"
}

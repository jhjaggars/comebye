// Package gpgtest makes a throwaway GNUPGHOME with a passphrase-protected
// signing key for tests of the GPG unlock. It never names the user's own
// ~/.gnupg: the home is a temp directory, removed (its agent killed) at the
// end of the test.
package gpgtest

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/gpgunlock"
)

// Key is a throwaway key in its own GNUPGHOME.
type Key struct {
	GPG  *gpgunlock.GPG
	Home string
	FPR  string
	Pass string
}

// New makes the home and the key, or skips the test when gpg (or its preset
// tool) is not installed. The agent is set up as `comebye gpg setup` does it,
// with the given preset lifetime.
func New(t *testing.T, lifetime time.Duration) *Key {
	t.Helper()
	g := &gpgunlock.GPG{}
	if !g.Available() {
		t.Skip("gpg is not installed")
	}
	if err := g.PresetAvailable(); err != nil {
		t.Skip(err.Error())
	}
	home, err := os.MkdirTemp("", "gu")
	if err != nil {
		t.Fatal(err)
	}
	g.Home = home
	t.Cleanup(func() {
		c := exec.Command("gpgconf", "--kill", "gpg-agent")
		c.Env = append(os.Environ(), "GNUPGHOME="+home)
		_ = c.Run()
		os.RemoveAll(home)
	})
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := g.ConfigureAgent(lifetime); err != nil {
		t.Fatal(err)
	}
	k := &Key{GPG: g, Home: home, Pass: "correct horse battery"}
	run := func(args ...string) string {
		cmd := exec.Command("gpg", args...)
		cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gpg %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("--batch", "--pinentry-mode", "loopback", "--passphrase", k.Pass, "--quick-gen-key", "Test <t@example.invalid>", "ed25519", "sign", "never")
	for _, ln := range strings.Split(run("--batch", "--list-secret-keys", "--with-colons"), "\n") {
		if f := strings.Split(ln, ":"); f[0] == "fpr" {
			k.FPR = f[9]
			break
		}
	}
	return k
}

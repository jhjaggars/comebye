package upgrade

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake launchctl: print shows a running agent, kickstart is logged and
// fails once when STUB_STATE/kickstart-fail-first exists.
const fakeLaunchctl = `#!/bin/sh
echo "$@" >> "$STUB_STATE/launchctl.log"
case "$1" in
print)
  [ -e "$STUB_STATE/not-loaded" ] && { echo "Could not find service" >&2; exit 113; }
  if grep -q BROKEN "$STUB_BIN"; then printf 'x = {\n\tstate = spawn scheduled\n}\n'; exit 0; fi
  printf 'gui/501/x = {\n\tstate = running\n}\n'; exit 0;;
kickstart)
  if [ -e "$STUB_STATE/kickstart-fail-first" ]; then
    rm "$STUB_STATE/kickstart-fail-first"; echo "boom" >&2; exit 1
  fi
  exit 0;;
esac
exit 0
`

func launchdOpts(e *env) Options {
	o := e.opts()
	o.Launchctl = filepath.Join(e.state, "tools", "launchctl")
	os.WriteFile(o.Launchctl, []byte(fakeLaunchctl), 0o755)
	o.HostOS = "darwin"
	o.UID = 501
	o.LaunchAgentsDir = filepath.Join(e.home, "LaunchAgents")
	o.Systemctl = filepath.Join(e.state, "no-such-systemctl") // a Mac has none
	o.Kill = func(int) error { panic("signalled in launchd mode") }
	return o
}

func launchctlLog(e *env) string {
	b, _ := os.ReadFile(filepath.Join(e.state, "launchctl.log"))
	return string(b)
}

func writePlist(t *testing.T, o Options, name, label, bin string) {
	t.Helper()
	os.MkdirAll(o.LaunchAgentsDir, 0o755)
	body := `<?xml version="1.0"?><plist><dict><key>Label</key><string>` + label + `</string>` +
		`<key>ProgramArguments</key><array><string>` + bin + `</string></array></dict></plist>`
	os.WriteFile(filepath.Join(o.LaunchAgentsDir, name), []byte(body), 0o644)
}

func TestLaunchdUpgrade(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	o := launchdOpts(e)
	writePlist(t, o, "other.plist", "com.example.other", "/usr/bin/other")
	writePlist(t, o, "mine.plist", "org.example.mycomebye", e.bin)
	if err := Run(o); err != nil {
		t.Fatalf("%v\n%s\n%s", err, e.out.String(), e.err.String())
	}
	if got := e.version(e.bin); got != "comebye v0.2.0" {
		t.Fatalf("installed: %s", got)
	}
	if got := e.version(e.bin + ".prev"); got != "comebye v0.1.0" {
		t.Fatalf(".prev: %s", got)
	}
	log := launchctlLog(e)
	if !strings.Contains(log, "kickstart -k gui/501/org.example.mycomebye") {
		t.Fatalf("launchctl calls:\n%s", log)
	}
	if e.restarts() != 0 {
		t.Fatal("systemctl used")
	}
	for _, want := range []string{"restart mode: launchd", "kickstart -k", "smoke: gui/501/org.example.mycomebye is running", "smoke: whoami ok", "no journal check", "upgraded v0.1.0 -> v0.2.0", "launchctl kickstart -k gui/501/org.example.mycomebye"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, e.out.String())
		}
	}
}

func TestLaunchdLabelOverrideAndDefault(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	o := launchdOpts(e)
	o.LaunchdLabel = "my.label"
	writePlist(t, o, "mine.plist", "ignored", e.bin)
	if err := resolveMode(&o); err != nil || o.Restart != RestartLaunchd || o.LaunchdLabel != "my.label" {
		t.Fatalf("%q %q %v", o.Restart, o.LaunchdLabel, err)
	}
	o = launchdOpts(e) // no plist: the default label
	o.LaunchAgentsDir = t.TempDir()
	if err := resolveMode(&o); err != nil || o.Restart != RestartLaunchd || o.LaunchdLabel != DefaultLaunchdLabel {
		t.Fatalf("%q %q %v", o.Restart, o.LaunchdLabel, err)
	}
}

func TestAutoModeLaunchdOnlyOnADarwinHostWithTheAgentLoaded(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	o := launchdOpts(e)
	o.HostOS = "linux"
	if err := resolveMode(&o); err != nil || o.Restart != RestartExit {
		t.Fatalf("linux: %q %v", o.Restart, err)
	}
	o = launchdOpts(e)
	e.set("not-loaded", "")
	if err := resolveMode(&o); err != nil || o.Restart != RestartExit {
		t.Fatalf("agent not loaded: %q %v", o.Restart, err)
	}
	os.Remove(filepath.Join(e.state, "not-loaded"))
	o = launchdOpts(e)
	o.Restart = RestartAuto
	if err := resolveMode(&o); err != nil || o.Restart != RestartLaunchd {
		t.Fatalf("loaded: %q %v", o.Restart, err)
	}
	// systemd still wins when its unit exists.
	o = launchdOpts(e)
	o.Systemctl = e.tool("systemctl")
	if err := resolveMode(&o); err != nil || o.Restart != RestartSystemd {
		t.Fatalf("systemd: %q %v", o.Restart, err)
	}
}

func TestLaunchdSmokeFailureRollsBack(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	e.release("v0.2.0", "BROKEN")
	err := Run(launchdOpts(e))
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("err = %v\n%s", err, e.err.String())
	}
	if got := e.version(e.bin); got != "comebye v0.1.0" {
		t.Fatalf("after rollback: %s", got)
	}
	if n := strings.Count(launchctlLog(e), "kickstart -k"); n != 2 {
		t.Fatalf("kickstarts = %d, want 2 (install, restore)", n)
	}
}

func TestLaunchdRestartFailureRollsBack(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	e.set("kickstart-fail-first", "")
	err := Run(launchdOpts(e))
	if !errors.Is(err, ErrRolledBack) || !strings.Contains(err.Error(), "launchctl kickstart") {
		t.Fatalf("err = %v\n%s", err, e.err.String())
	}
	if got := e.version(e.bin); got != "comebye v0.1.0" {
		t.Fatalf("after rollback: %s", got)
	}
}

func TestLaunchdNotRunningFailsTheWait(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	o := launchdOpts(e)
	o.Restart = RestartLaunchd
	o.LaunchdLabel = "x"
	e.set("not-loaded", "")
	if err := waitLaunchd(&o); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("err = %v", err)
	}
}

func TestLaunchdState(t *testing.T) {
	if got := launchdState("a = {\n\tstate = waiting\n\tstate = running\n}"); got != "waiting" {
		t.Fatal(got)
	}
	if got := launchdState("nothing"); got != "unknown" {
		t.Fatal(got)
	}
}

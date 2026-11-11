package server

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

// defaultUpgradeRunner picks how the app starts an upgrade on this host: a
// transient systemd user unit on Linux, a one-shot LaunchAgent on macOS.
func defaultUpgradeRunner(bin, url string) UpgradeRunner {
	if runtime.GOOS == "darwin" {
		return &launchdRunner{Bin: bin, URL: url}
	}
	return &systemdRunner{Bin: bin, URL: url}
}

// upgradeBinary is the installed binary `comebye upgrade` replaces: bin, or
// this process's own executable.
func upgradeBinary(bin string) (string, error) {
	if bin != "" {
		return bin, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(exe, " (deleted)"), nil // the binary was replaced while this server ran
}

// upgradeArgs is the `comebye upgrade ...` command line for one run.
func upgradeArgs(bin, path, version string, rollback, dry bool) []string {
	args := []string{"upgrade", "--bin", bin, "--record", path}
	if rollback {
		args = append(args, "--rollback")
	} else {
		args = append(args, "--version", version)
	}
	if dry {
		args = append(args, "--dry-run")
	}
	return args
}

// startDetachedDry runs a dry upgrade as a plain child: it installs and
// restarts nothing, so it needs no unit that outlives this server.
func startDetachedDry(bin, url string, args []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "COMEBYE_URL="+url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// launchdRunner starts `comebye upgrade` on a Mac as a one-shot LaunchAgent
// in the user's GUI domain. It is a separate launchd job from comebye's own,
// so the `launchctl kickstart -k` the upgrade ends with does not take it
// down. The job removes itself (and its plist) when the run is over.
type launchdRunner struct {
	Bin, URL string
	// Dir holds the job's plist and log (default: next to the record).
	Dir string
	// UID is the GUI domain's user (default: this process's).
	UID int
	// Launchctl is the program (default: launchctl found on PATH).
	Launchctl string
	// run executes a command (default: exec; tests replace it).
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// upgradeJobLabel names the one-shot job of a run.
func upgradeJobLabel(id string) string { return "com.jhjaggars.comebye-upgrade." + id }

// upgradeJobScript runs the upgrade, then unloads the job and deletes its
// plist (first: bootout ends this shell). $1 is the binary and the rest its arguments; the job's own target
// and plist come in the environment.
const upgradeJobScript = `bin="$1"; shift; "$bin" "$@"; rc=$?; rm -f "$COMEBYE_JOB_PLIST"; launchctl bootout "$COMEBYE_JOB_TARGET" >/dev/null 2>&1; exit $rc`

// upgradeJobPlist renders the LaunchAgent plist for a run.
func upgradeJobPlist(label, target, plistPath, logPath, url, bin string, args []string) []byte {
	var b bytes.Buffer
	esc := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return e.String()
	}
	str := func(s string) { fmt.Fprintf(&b, "\t\t<string>%s</string>\n", esc(s)) }
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
`)
	fmt.Fprintf(&b, "\t<string>%s</string>\n\t<key>ProgramArguments</key>\n\t<array>\n", esc(label))
	for _, a := range append([]string{"/bin/sh", "-c", upgradeJobScript, "comebye-upgrade", bin}, args...) {
		str(a)
	}
	b.WriteString("\t</array>\n\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	for _, kv := range [][2]string{{"COMEBYE_URL", url}, {"COMEBYE_JOB_TARGET", target}, {"COMEBYE_JOB_PLIST", plistPath},
		{"PATH", "/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"}} {
		fmt.Fprintf(&b, "\t\t<key>%s</key>\n", esc(kv[0]))
		str(kv[1])
	}
	b.WriteString("\t</dict>\n\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<false/>\n\t<key>AbandonProcessGroup</key>\n\t<true/>\n")
	fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", esc(logPath), esc(logPath))
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func (l *launchdRunner) Start(ctx context.Context, rec upgrade.Record, path string, rollback, dry bool) error {
	bin, err := upgradeBinary(l.Bin)
	if err != nil {
		return err
	}
	args := upgradeArgs(bin, path, rec.Version, rollback, dry)
	if dry && !rollback {
		return startDetachedDry(bin, l.URL, args)
	}
	lc := l.Launchctl
	if lc == "" {
		if lc, err = exec.LookPath("launchctl"); err != nil {
			return errors.New("launchctl is needed to upgrade from the app (run `comebye upgrade` on the host)")
		}
	}
	dir := l.Dir
	if dir == "" {
		dir = filepath.Dir(path)
	}
	uid := l.UID
	if uid <= 0 {
		uid = os.Getuid()
	}
	label := upgradeJobLabel(rec.ID)
	domain := "gui/" + strconv.Itoa(uid)
	plistPath := filepath.Join(dir, label+".plist")
	body := upgradeJobPlist(label, domain+"/"+label, plistPath, filepath.Join(dir, label+".log"), l.URL, bin, args)
	if err := os.WriteFile(plistPath, body, 0o600); err != nil {
		return err
	}
	run := l.run
	if run == nil {
		run = func(ctx context.Context, name string, a ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, a...).CombinedOutput()
		}
	}
	if out, err := run(ctx, lc, "bootstrap", domain, plistPath); err != nil {
		_ = os.Remove(plistPath)
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, oneLine(string(out), 200))
	}
	return nil
}

package server

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/upgrade"
)

func TestUpgradeJobPlistIsWellFormed(t *testing.T) {
	args := upgradeArgs("/Users/me/.local/bin/comebye", "/state/release/upgrade.json", "v1.2.3", false, false)
	body := upgradeJobPlist("com.jhjaggars.comebye-upgrade.ab12", "gui/501/com.jhjaggars.comebye-upgrade.ab12",
		"/state/x.plist", "/state/x.log", "http://127.0.0.1:8741", "/Users/me/a&b/comebye", args)
	// Parses as XML; the & is escaped.
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	dec.Strict = false
	for {
		if _, err := dec.Token(); err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("plist is not well formed: %v\n%s", err, body)
			}
			break
		}
	}
	s := string(body)
	for _, want := range []string{"<string>/bin/sh</string>", "a&amp;b/comebye", "<string>--version</string>", "<string>v1.2.3</string>",
		"<key>COMEBYE_URL</key>", "<key>KeepAlive</key>\n\t<false/>", "<key>RunAtLoad</key>", "COMEBYE_JOB_TARGET"} {
		if !strings.Contains(s, want) {
			t.Errorf("plist lacks %q:\n%s", want, s)
		}
	}
}

func TestUpgradeArgs(t *testing.T) {
	got := strings.Join(upgradeArgs("/b", "/r.json", "v1.0.0", false, true), " ")
	if got != "upgrade --bin /b --record /r.json --version v1.0.0 --dry-run" {
		t.Errorf("args = %q", got)
	}
	got = strings.Join(upgradeArgs("/b", "/r.json", "v1.0.0", true, false), " ")
	if got != "upgrade --bin /b --record /r.json --rollback" {
		t.Errorf("rollback args = %q", got)
	}
}

func TestLaunchdRunnerBootstrapsOneShotJob(t *testing.T) {
	dir := t.TempDir()
	var calls [][]string
	l := &launchdRunner{Bin: "/bin/comebye", URL: "http://127.0.0.1:1", Dir: dir, UID: 501, Launchctl: "launchctl",
		run: func(_ context.Context, name string, a ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, a...))
			return nil, nil
		}}
	rec := upgrade.Record{ID: "ab12", Version: "v1.2.3"}
	if err := l.Start(context.Background(), rec, filepath.Join(dir, "upgrade.json"), false, false); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(dir, "com.jhjaggars.comebye-upgrade.ab12.plist")
	if len(calls) != 1 || strings.Join(calls[0], " ") != "launchctl bootstrap gui/501 "+plist {
		t.Fatalf("calls = %v", calls)
	}
	b, err := os.ReadFile(plist)
	if err != nil || !strings.Contains(string(b), "<string>v1.2.3</string>") {
		t.Fatalf("plist not written: %v %s", err, b)
	}
}

func TestLaunchdRunnerCleansUpWhenBootstrapFails(t *testing.T) {
	dir := t.TempDir()
	l := &launchdRunner{Bin: "/bin/comebye", Dir: dir, UID: 501, Launchctl: "launchctl",
		run: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("Bootstrap failed: 5"), os.ErrPermission
		}}
	err := l.Start(context.Background(), upgrade.Record{ID: "ab12", Version: "v1.2.3"}, filepath.Join(dir, "upgrade.json"), false, false)
	if err == nil || !strings.Contains(err.Error(), "launchctl bootstrap") {
		t.Fatalf("err = %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.plist")); len(m) != 0 {
		t.Errorf("plist left behind: %v", m)
	}
}

func TestLaunchdRunnerWithoutLaunchctl(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	l := &launchdRunner{Bin: "/bin/comebye", Dir: t.TempDir()}
	err := l.Start(context.Background(), upgrade.Record{ID: "x", Version: "v1.2.3"}, "/r.json", false, false)
	if err == nil || !strings.Contains(err.Error(), "launchctl is needed") {
		t.Fatalf("err = %v", err)
	}
}

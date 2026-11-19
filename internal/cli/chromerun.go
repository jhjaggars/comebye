package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Running headless Chrome for the publish-time checks (layout, thumbnails).
// Chrome is a courtesy here, so it must never hold a publish up: it gets no
// network but the one CDN the pages guide allows (a page's other scripts and
// fonts would only hang the render), it runs in its own process group, and
// the whole group is killed when the time is up (Chrome's helper processes
// outlive the main one and keep its output open).

// chromePaths are where a browser lives when it is not on PATH: macOS apps,
// Google's own Linux packages, snaps. Tests empty it.
var chromePaths = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
	"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
	"/opt/google/chrome/chrome",
	"/opt/chromium.org/chromium/chromium",
	"/usr/lib/chromium/chromium",
	"/usr/lib/chromium-browser/chromium-browser",
	"/snap/bin/chromium",
	"/usr/bin/microsoft-edge",
}

// chromeNoNetwork routes every request but the CDN's to a closed port, so a
// page that loads from anywhere else fails at once instead of waiting for a
// host that never answers (IP literals included, which a resolver rule would
// miss). file:// pages do not use a proxy.
const chromeNoNetwork = `--proxy-pac-url=data:application/x-ns-proxy-autoconfig;charset=utf-8,` +
	`function FindProxyForURL(u,h){return h=="cdnjs.cloudflare.com"?"DIRECT":"PROXY 127.0.0.1:9"}`

// chromeBase are the flags every headless run shares; profile is a fresh
// user-data directory, never the user's.
func chromeBase(profile string) []string {
	return []string{"--headless", "--no-sandbox", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--hide-scrollbars", "--disable-dev-shm-usage", "--disable-background-networking", "--disable-component-update",
		"--disable-sync", "--mute-audio", "--user-data-dir=" + profile, chromeNoNetwork}
}

var errChromeTimeout = errors.New("timed out")

// runChrome runs chrome with args to completion or until ctx is done, then
// kills its whole process group. stdout may be nil. The error says why in a
// few words: the timeout, or Chrome's own complaint (last stderr line).
func runChrome(ctx context.Context, chrome string, args []string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, chrome, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	cmd.Stdout = stdout
	var se lastLines
	cmd.Stderr = &se
	err := cmd.Run()
	// Whatever ended it, nothing of Chrome may stay behind.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if ctx.Err() != nil {
		return errChromeTimeout
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if l := se.last(); l != "" {
				return errors.New("Chrome exited " + ee.ProcessState.String() + ": " + l)
			}
			return errors.New("Chrome exited " + ee.ProcessState.String())
		}
		return err
	}
	return nil
}

// lastLines keeps the tail of what Chrome wrote to stderr.
type lastLines struct{ b []byte }

func (l *lastLines) Write(p []byte) (int, error) {
	l.b = append(l.b, p...)
	if len(l.b) > 4096 {
		l.b = l.b[len(l.b)-4096:]
	}
	return len(p), nil
}

func (l *lastLines) last() string {
	lines := strings.Split(strings.TrimSpace(string(bytes.ToValidUTF8(l.b, nil))), "\n")
	s := strings.TrimSpace(lines[len(lines)-1])
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

func fileExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode()&0o111 != 0
}

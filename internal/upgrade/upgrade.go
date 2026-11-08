// Package upgrade installs a new comebye binary over the running one and
// restarts the user service, with a smoke check and an automatic rollback.
//
// The binary that runs the upgrade is the old one: it fetches (or builds)
// the new binary next to the target, renames it into place (atomic; the
// running process keeps its old inode), restarts the unit through
// systemctl and checks the result by running the NEW binary. The unit file
// and its drop-ins are never touched.
package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultReleases is where release archives live.
const DefaultReleases = "https://github.com/jhjaggars/comebye/releases"

// Options is one upgrade (or rollback). Zero values are real defaults,
// except Bin, which the caller must set.
type Options struct {
	Bin string // the installed binary, e.g. ~/.local/bin/comebye

	Version    string // a tag (v0.2.0) or "latest" (the default)
	FromSource bool   // build from a checkout instead of a release
	Ref        string // with FromSource: commit, tag or branch (default origin/main)
	RepoDir    string // with FromSource: the checkout (default the current directory)
	SkipChecks bool   // with FromSource: skip go vet and go test

	DryRun   bool // fetch and verify, print the plan, install nothing
	Force    bool // allow a downgrade, or reinstall the same version
	Rollback bool // restore Bin.prev instead of upgrading

	// Restart is how the server is restarted: "systemd" (the user unit),
	// "exit" (SIGTERM to the running server, restarted by a supervisor such
	// as a sandbox's init loop) or "auto" (the default: systemd when the
	// unit exists). PID names the server to stop in exit mode when there is
	// more than one.
	Restart string
	PID     int
	// ProcRoot, Kill and Alive replace /proc and signals (tests).
	ProcRoot string
	Kill     func(pid int) error
	Alive    func(pid int) bool

	Releases   string // release base URL (default DefaultReleases)
	OS, Arch   string // default the running ones
	Service    string // unit name (default comebye)
	Systemctl  string // default "systemctl"
	Journalctl string // default "journalctl"
	Git, Go    string // default "git", "go"
	LocalURL   string // the local server (default http://127.0.0.1:8741)
	Env        []string
	HTTP       *http.Client

	Out, Err io.Writer
	Sleep    func(time.Duration)
	Now      func() time.Time
	// SmokeTries and SmokeWait bound the wait for the restarted service.
	SmokeTries int
	SmokeWait  time.Duration
}

func (o *Options) defaults() {
	if o.Version == "" {
		o.Version = "latest"
	}
	if o.Releases == "" {
		o.Releases = DefaultReleases
	}
	if o.OS == "" {
		o.OS = runtime.GOOS
	}
	if o.Arch == "" {
		o.Arch = runtime.GOARCH
	}
	if o.Service == "" {
		o.Service = "comebye"
	}
	if o.Systemctl == "" {
		o.Systemctl = "systemctl"
	}
	if o.Journalctl == "" {
		o.Journalctl = "journalctl"
	}
	if o.Git == "" {
		o.Git = "git"
	}
	if o.Go == "" {
		o.Go = "go"
	}
	if o.LocalURL == "" {
		o.LocalURL = "http://127.0.0.1:8741"
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{
			Timeout: 5 * time.Minute,
			// Redirects are followed by hand where they matter (latest).
		}
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Err == nil {
		o.Err = io.Discard
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SmokeTries <= 0 {
		o.SmokeTries = 10
	}
	if o.SmokeWait <= 0 {
		o.SmokeWait = time.Second
	}
}

func (o *Options) printf(format string, a ...any) { fmt.Fprintf(o.Out, format, a...) }

// ErrRolledBack means the upgrade failed its smoke check and the previous
// binary was restored.
var ErrRolledBack = errors.New("upgrade failed its smoke check; rolled back to the previous binary")

// Run does the upgrade (or rollback) described by o.
func Run(o Options) error {
	o.defaults()
	if o.Bin == "" {
		return errors.New("no binary path")
	}
	if err := resolveMode(&o); err != nil {
		return err
	}
	o.printf("restart mode: %s\n", o.Restart)
	if o.Rollback {
		return rollback(&o)
	}
	cur, curErr := binVersion(&o, o.Bin)
	if curErr != nil {
		o.printf("current version: unknown (%v)\n", curErr)
	} else {
		o.printf("current version: %s\n", cur)
	}

	tmp, err := os.MkdirTemp(filepath.Dir(o.Bin), ".comebye-upgrade-")
	if err != nil {
		return fmt.Errorf("cannot stage next to %s: %w", o.Bin, err)
	}
	defer os.RemoveAll(tmp)

	var target, staged string
	if o.FromSource {
		target, staged, err = stageSource(&o, tmp, cur)
	} else {
		target, staged, err = stageRelease(&o, tmp, cur)
	}
	if err != nil {
		return err
	}
	if target == "" { // no-op, already said so
		return nil
	}

	if curErr == nil {
		switch c := compareVersions(target, cur); {
		case c == 0 && !o.Force:
			o.printf("already at %s: nothing to do\n", cur)
			return nil
		case c < 0 && !o.Force:
			return fmt.Errorf("%s is older than the installed %s: refusing a downgrade (use --force, or `comebye upgrade --rollback`)", target, cur)
		}
	}

	prev := o.Bin + ".prev"
	if o.DryRun {
		o.printf("dry run: would install %s over %s (was %s)\n", target, o.Bin, orUnknown(cur, curErr))
		o.printf("dry run: would keep the old binary as %s and restart %s\n", prev, o.restartTarget())
		o.printf("dry run: rollback would be: cp %s %s && %s\n", prev, o.Bin, restartCmdHint(&o))
		return nil
	}

	// Keep the old binary, unless it is the one being (re)installed.
	haveOld := curErr == nil
	if haveOld && cur != target {
		if err := copyFile(o.Bin, prev); err != nil {
			return fmt.Errorf("back up %s: %w", o.Bin, err)
		}
		o.printf("kept the old binary as %s\n", prev)
	} else if _, err := os.Stat(prev); err == nil {
		haveOld = true
	}
	before := indexTag(&o)

	if err := os.Rename(staged, o.Bin); err != nil {
		return fmt.Errorf("install %s: %w", o.Bin, err)
	}
	o.printf("installed %s at %s\n", target, o.Bin)
	if err := restartServer(&o); err != nil {
		return failAndRestore(&o, haveOld, prev, fmt.Errorf("restart: %w", err))
	}
	start := o.Now().Add(-2 * time.Second)
	if err := smoke(&o, start, before, target); err != nil {
		return failAndRestore(&o, haveOld, prev, err)
	}
	after, _ := binVersion(&o, o.Bin)
	o.printf("upgraded %s -> %s\n", orUnknown(cur, curErr), after)
	if haveOld {
		o.printf("to roll back: comebye upgrade --rollback   (or: cp %s %s && %s)\n", prev, o.Bin, restartCmdHint(&o))
	}
	return nil
}

// restartTarget names what is restarted, for messages.
func (o *Options) restartTarget() string {
	if o.exitMode() {
		return "the running server (SIGTERM; a supervisor starts it again)"
	}
	return o.Service
}

func orUnknown(v string, err error) string {
	if err != nil {
		return "unknown"
	}
	return v
}

// failAndRestore puts the previous binary back after a failed install.
func failAndRestore(o *Options, haveOld bool, prev string, cause error) error {
	fmt.Fprintf(o.Err, "FAILED: %v\n", cause)
	if !haveOld {
		return fmt.Errorf("%w (no previous binary to restore)", cause)
	}
	if err := copyFile(prev, o.Bin); err != nil {
		return fmt.Errorf("%v; and restoring %s failed: %w", cause, prev, err)
	}
	if err := restartServer(o); err != nil {
		return fmt.Errorf("%v; restored %s but the restart failed: %w", cause, o.Bin, err)
	}
	if err := waitUp(o); err != nil {
		return fmt.Errorf("%v; restored %s but the service is not active: %w", cause, o.Bin, err)
	}
	v, _ := binVersion(o, o.Bin)
	fmt.Fprintf(o.Err, "rolled back to %s (%s is active again)\n", v, o.Service)
	return fmt.Errorf("%w: %v", ErrRolledBack, cause)
}

func rollback(o *Options) error {
	prev := o.Bin + ".prev"
	pv, err := binVersion(o, prev)
	if err != nil {
		return fmt.Errorf("no usable previous binary at %s: %w", prev, err)
	}
	cur, _ := binVersion(o, o.Bin)
	o.printf("current: %s; restoring %s from %s\n", cur, pv, prev)
	if o.DryRun {
		o.printf("dry run: nothing changed\n")
		return nil
	}
	before := indexTag(o)
	// Through a temp file and a rename, so the swap is atomic.
	if err := copyFile(prev, o.Bin); err != nil {
		return err
	}
	if err := restartServer(o); err != nil {
		return err
	}
	if err := smoke(o, o.Now().Add(-2*time.Second), before, pv); err != nil {
		return fmt.Errorf("rolled back, but the smoke check failed: %w", err)
	}
	o.printf("rolled back to %s\n", pv)
	return nil
}

// ---- version handling ----

var verRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-(\d+)-g[0-9a-f]+)?`)

// compareVersions orders vX.Y.Z[-N-gSHA] strings. Anything else
// (dev, a pseudo-version) is treated as older than a release, so
// installing a release over it is an upgrade; two unparseable versions
// compare equal only when identical, else as an upgrade.
func compareVersions(a, b string) int {
	pa, oka := parseVer(a)
	pb, okb := parseVer(b)
	switch {
	case a == b:
		return 0
	case oka && okb:
		for i := range pa {
			if pa[i] != pb[i] {
				if pa[i] < pb[i] {
					return -1
				}
				return 1
			}
		}
		return 0
	case oka:
		return 1
	case okb:
		return -1
	}
	return 1
}

func parseVer(s string) ([4]int, bool) {
	var v [4]int
	m := verRe.FindStringSubmatch(s)
	if m == nil {
		return v, false
	}
	for i := 1; i <= 4; i++ {
		v[i-1], _ = strconv.Atoi(m[i]) // empty (no -N- part) is 0
	}
	return v, true
}

// binVersion runs "<bin> -version" ("comebye v0.2.0").
func binVersion(o *Options, bin string) (string, error) {
	cmd := exec.Command(bin, "-version")
	cmd.Env = o.Env
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", errors.New("empty -version output")
	}
	return f[len(f)-1], nil
}

// ---- release source ----

func stageRelease(o *Options, tmp, cur string) (target, staged string, err error) {
	tag := o.Version
	if tag == "latest" {
		if tag, err = latestTag(o); err != nil {
			return "", "", err
		}
	}
	if !verRe.MatchString(tag) {
		return "", "", fmt.Errorf("%q is not a release tag like v0.2.0", tag)
	}
	// Already there: skip the download.
	if cur == tag && !o.Force {
		o.printf("already at %s: nothing to do\n", cur)
		return "", "", nil
	}
	name := fmt.Sprintf("comebye_%s_%s.tar.gz", o.OS, o.Arch)
	base := strings.TrimRight(o.Releases, "/") + "/download/" + url.PathEscape(tag) + "/"
	o.printf("target: %s (%s)\n", tag, base+name)
	sums, err := httpGet(o, base+"SHA256SUMS")
	if err != nil {
		return "", "", err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = f[0]
		}
	}
	if want == "" {
		return "", "", fmt.Errorf("SHA256SUMS for %s has no entry for %s", tag, name)
	}
	archive, err := httpGet(o, base+name)
	if err != nil {
		return "", "", err
	}
	got := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return "", "", fmt.Errorf("checksum mismatch for %s: want %s, got %x", name, want, got)
	}
	o.printf("checksum ok (%s)\n", want[:12])
	staged = filepath.Join(tmp, "comebye.new")
	if err := extractBinary(archive, staged); err != nil {
		return "", "", err
	}
	v, err := binVersion(o, staged)
	if err != nil {
		return "", "", fmt.Errorf("the downloaded binary does not run: %w", err)
	}
	if v != tag {
		return "", "", fmt.Errorf("the downloaded binary reports %s, expected %s", v, tag)
	}
	return tag, staged, nil
}

// latestTag follows releases/latest by hand: GitHub redirects it to
// releases/tag/<tag>.
func latestTag(o *Options) (string, error) {
	c := *o.HTTP
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Get(strings.TrimRight(o.Releases, "/") + "/latest")
	if err != nil {
		return "", fmt.Errorf("find the latest release: %w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || loc == "" {
		return "", fmt.Errorf("find the latest release: HTTP %d without a redirect", resp.StatusCode)
	}
	tag := path.Base(strings.TrimRight(loc, "/"))
	if !verRe.MatchString(tag) {
		return "", fmt.Errorf("latest release redirects to %q, not a tag", loc)
	}
	return tag, nil
}

const maxDownload = 256 << 20

func httpGet(o *Options, u string) ([]byte, error) {
	resp, err := o.HTTP.Get(u)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download %s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	if len(b) > maxDownload {
		return nil, fmt.Errorf("download %s: larger than %d MB", u, maxDownload>>20)
	}
	return b, nil
}

// extractBinary writes the archive's "comebye" file (any directory) to dst.
func extractBinary(archive []byte, dst string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return errors.New("the archive has no comebye binary")
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != "comebye" {
			continue
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, io.LimitReader(tr, maxDownload)); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
}

// ---- source ----

func stageSource(o *Options, tmp, cur string) (target, staged string, err error) {
	repo := o.RepoDir
	if repo == "" {
		repo = "."
	}
	ref := o.Ref
	if ref == "" {
		ref = "origin/main"
	}
	run := func(dir, name string, args ...string) (string, error) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = o.Env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return strings.TrimSpace(out.String()), err
	}
	if out, err := run(repo, o.Git, "fetch", "--tags", "--quiet"); err != nil {
		fmt.Fprintf(o.Err, "warning: git fetch failed (building what is already here): %s\n", out)
	}
	sha, err := run(repo, o.Git, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", "", fmt.Errorf("%s is not a commit in %s: %s", ref, repo, sha)
	}
	ver, err := run(repo, o.Git, "describe", "--tags", "--always", sha)
	if err != nil {
		return "", "", fmt.Errorf("git describe: %s", ver)
	}
	o.printf("target: %s at %s (built from source)\n", ver, sha[:12])
	if cur == ver && !o.Force {
		o.printf("already at %s: nothing to do\n", cur)
		return "", "", nil
	}
	wt := filepath.Join(tmp, "src")
	if out, err := run(repo, o.Git, "worktree", "add", "--detach", wt, sha); err != nil {
		return "", "", fmt.Errorf("git worktree add: %s", out)
	}
	defer run(repo, o.Git, "worktree", "remove", "--force", wt)
	if !o.SkipChecks {
		if out, err := run(wt, o.Go, "vet", "./..."); err != nil {
			return "", "", fmt.Errorf("go vet failed:\n%s", out)
		}
		if out, err := run(wt, o.Go, "test", "-count=1", "./..."); err != nil {
			return "", "", fmt.Errorf("go test failed:\n%s", out)
		}
		o.printf("go vet and go test pass\n")
	}
	staged = filepath.Join(tmp, "comebye.new")
	if out, err := run(wt, o.Go, "build", "-ldflags", "-X main.version="+ver, "-o", staged, "."); err != nil {
		return "", "", fmt.Errorf("go build failed:\n%s", out)
	}
	v, err := binVersion(o, staged)
	if err != nil || v != ver {
		return "", "", fmt.Errorf("the built binary reports %q (%v), expected %s", v, err, ver)
	}
	return ver, staged, nil
}

// ---- files ----

// copyFile copies src over dst through a temp file and a rename, keeping
// the mode, so a reader never sees a half-written binary.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(dst), ".comebye-copy-")
	if err != nil {
		return err
	}
	name := out.Name()
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(name)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, st.Mode().Perm()|0o100); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

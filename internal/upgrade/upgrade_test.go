package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A stub comebye: a shell script that answers -version and the smoke
// subcommands, steered by files in $STUB_STATE.
const stubScript = `#!/bin/sh
# %s
V=%s
S="$STUB_STATE"
case "$1" in
-version) echo "comebye $V"; exit 0;;
whoami) if [ -f "$S/peer" ]; then echo '{"host":"h1","hub":"https://hub.example","ledger":false}'; else echo '{"host":"h1","hub":"self","ledger":true}'; fi; exit 0;;
agents) echo '{"agents":[{"host":"h1","id":"h1/w1:p1"}]}'; exit 0;;
*) if [ -f "$S/fail_$1" ]; then cat "$S/fail_$1" >&2; exit 1; fi; echo ok; exit 0;;
esac
`

const fakeSystemctl = `#!/bin/sh
echo "$@" >> "$STUB_STATE/systemctl.log"
case "$2" in
restart) [ -f "$STUB_STATE/restart_fail" ] && exit 1; exit 0;;
is-active) if grep -q BROKEN "$STUB_BIN"; then echo failed; exit 3; fi; echo active; exit 0;;
esac
exit 0
`

const fakeJournalctl = `#!/bin/sh
if grep -q JOURNALERR "$STUB_BIN"; then echo "Nov 02 comebye[1]: boom"; fi
exit 0
`

type env struct {
	t        *testing.T
	home     string
	bin      string
	state    string
	out, err bytes.Buffer
	rel      *httptest.Server
	local    *httptest.Server
	archHits atomic.Int32
	tags     map[string][]byte // tag -> tarball
	badSum   bool
}

func stub(version, marker string) string {
	return fmt.Sprintf(stubScript, marker, version)
}

func tarball(t *testing.T, script string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "comebye_linux_amd64/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "comebye_linux_amd64/README.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: "comebye_linux_amd64/comebye", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(script))})
	tw.Write([]byte(script))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func newEnv(t *testing.T, installed string) *env {
	t.Helper()
	e := &env{t: t, tags: map[string][]byte{}}
	e.home = t.TempDir()
	e.state = t.TempDir()
	bindir := filepath.Join(e.home, ".local", "bin")
	os.MkdirAll(bindir, 0o755)
	e.bin = filepath.Join(bindir, "comebye")
	if installed != "" {
		os.WriteFile(e.bin, []byte(stub(installed, "OLD")), 0o755)
	}
	e.local = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		fmt.Fprint(w, "<html>")
	}))
	e.rel = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == "/latest":
			http.Redirect(w, r, "/tag/v0.2.0", http.StatusFound)
		case strings.HasSuffix(p, "/SHA256SUMS"):
			tag := strings.Split(p, "/")[2]
			tb, ok := e.tags[tag]
			if !ok {
				http.NotFound(w, r)
				return
			}
			sum := sha256.Sum256(tb)
			if e.badSum {
				sum[0] ^= 0xff
			}
			fmt.Fprintf(w, "%x  comebye_linux_amd64.tar.gz\n%x  comebye_darwin_arm64.tar.gz\n", sum, sum)
		case strings.HasSuffix(p, "comebye_linux_amd64.tar.gz"):
			e.archHits.Add(1)
			tb, ok := e.tags[strings.Split(p, "/")[2]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Write(tb)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(e.rel.Close)
	t.Cleanup(e.local.Close)
	e.release("v0.2.0", "NEW")
	return e
}

func (e *env) release(tag, marker string) {
	e.tags[tag] = tarball(e.t, stub(tag, marker))
}

func (e *env) opts() Options {
	return Options{
		Bin: e.bin, Releases: e.rel.URL, OS: "linux", Arch: "amd64",
		Systemctl: e.tool("systemctl"), Journalctl: e.tool("journalctl"),
		LocalURL: e.local.URL, HTTP: e.rel.Client(),
		Env: append(os.Environ(), "STUB_STATE="+e.state, "STUB_BIN="+e.bin),
		Out: &e.out, Err: &e.err,
		Sleep: func(time.Duration) {}, SmokeTries: 2,
	}
}

// tool writes the fake tool once per env (they are in the same temp dir).
func (e *env) tool(name string) string {
	dir := filepath.Join(e.state, "tools")
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, name)
	body := fakeSystemctl
	if name == "journalctl" {
		body = fakeJournalctl
	}
	os.WriteFile(p, []byte(body), 0o755)
	return p
}

func (e *env) version(path string) string {
	out, err := exec.Command(path, "-version").Output()
	if err != nil {
		e.t.Fatalf("%s -version: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) restarts() int {
	b, _ := os.ReadFile(filepath.Join(e.state, "systemctl.log"))
	return strings.Count(string(b), "restart")
}

func (e *env) set(file, content string) {
	os.WriteFile(filepath.Join(e.state, file), []byte(content), 0o644)
}

func TestUpgradeRelease(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	if err := Run(e.opts()); err != nil {
		t.Fatalf("%v\n%s\n%s", err, e.out.String(), e.err.String())
	}
	if got := e.version(e.bin); got != "comebye v0.2.0" {
		t.Fatalf("installed: %s", got)
	}
	if got := e.version(e.bin + ".prev"); got != "comebye v0.1.0" {
		t.Fatalf(".prev: %s", got)
	}
	if e.restarts() != 1 {
		t.Fatalf("restarts = %d", e.restarts())
	}
	for _, want := range []string{"checksum ok", "upgraded v0.1.0 -> v0.2.0", "to roll back: comebye upgrade --rollback", "cp " + e.bin + ".prev"} {
		if !strings.Contains(e.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, e.out.String())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(e.bin))
	for _, f := range entries {
		if strings.HasPrefix(f.Name(), ".comebye") {
			t.Errorf("staging leftover %s", f.Name())
		}
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	o := e.opts()
	o.DryRun = true
	if err := Run(o); err != nil {
		t.Fatal(err)
	}
	if e.version(e.bin) != "comebye v0.1.0" || e.restarts() != 0 {
		t.Fatal("dry run changed things")
	}
	if _, err := os.Stat(e.bin + ".prev"); err == nil {
		t.Fatal("dry run wrote .prev")
	}
	if !strings.Contains(e.out.String(), "dry run: would install v0.2.0") || e.archHits.Load() != 1 {
		t.Fatalf("plan/verify missing (archive hits %d):\n%s", e.archHits.Load(), e.out.String())
	}
}

func TestSameVersionIsNoop(t *testing.T) {
	e := newEnv(t, "v0.2.0")
	if err := Run(e.opts()); err != nil {
		t.Fatal(err)
	}
	if e.restarts() != 0 || e.archHits.Load() != 0 {
		t.Fatalf("restarts %d, downloads %d", e.restarts(), e.archHits.Load())
	}
	if !strings.Contains(e.out.String(), "nothing to do") {
		t.Fatal(e.out.String())
	}
	if _, err := os.Stat(e.bin + ".prev"); err == nil {
		t.Fatal("a no-op wrote .prev")
	}
}

func TestDowngradeNeedsForce(t *testing.T) {
	e := newEnv(t, "v0.3.0")
	err := Run(e.opts())
	if err == nil || !strings.Contains(err.Error(), "refusing a downgrade") {
		t.Fatalf("err = %v", err)
	}
	if e.version(e.bin) != "comebye v0.3.0" || e.restarts() != 0 {
		t.Fatal("refused downgrade still changed things")
	}
	o := e.opts()
	o.Force = true
	if err := Run(o); err != nil {
		t.Fatal(err)
	}
	if e.version(e.bin) != "comebye v0.2.0" {
		t.Fatal("forced downgrade did not install")
	}
}

func TestChecksumMismatch(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	e.badSum = true
	err := Run(e.opts())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if e.version(e.bin) != "comebye v0.1.0" || e.restarts() != 0 {
		t.Fatal("bad download was installed")
	}
}

func TestWrongVersionInArchive(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	e.tags["v0.2.0"] = tarball(t, stub("v0.9.9", "NEW"))
	err := Run(e.opts())
	if err == nil || !strings.Contains(err.Error(), "expected v0.2.0") {
		t.Fatalf("err = %v", err)
	}
}

func TestSmokeFailureRollsBack(t *testing.T) {
	for name, marker := range map[string]string{"not active": "BROKEN", "journal errors": "JOURNALERR"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, "v0.1.0")
			e.release("v0.2.0", marker)
			err := Run(e.opts())
			if !errors.Is(err, ErrRolledBack) {
				t.Fatalf("err = %v\n%s", err, e.err.String())
			}
			if got := e.version(e.bin); got != "comebye v0.1.0" {
				t.Fatalf("after rollback: %s", got)
			}
			if e.restarts() != 2 {
				t.Fatalf("restarts = %d, want 2 (install, restore)", e.restarts())
			}
			if !strings.Contains(e.err.String(), "rolled back to v0.1.0") {
				t.Fatal(e.err.String())
			}
		})
	}
}

func TestRestartFailureRollsBack(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	e.set("restart_fail", "")
	err := Run(e.opts())
	// the restore's own restart fails too, and that is reported
	if err == nil || e.version(e.bin) != "comebye v0.1.0" {
		t.Fatalf("err=%v version=%s", err, e.version(e.bin))
	}
}

// The other host being unreachable must not roll an upgrade back.
func TestUnreachableOtherHostIsOnlyAWarning(t *testing.T) {
	t.Run("peer, hub down", func(t *testing.T) {
		e := newEnv(t, "v0.1.0")
		e.set("peer", "")
		for _, c := range []string{"ledger", "queue", "sweep"} {
			e.set("fail_"+c, "hub https://hub.example: dial tcp: connection refused")
		}
		if err := Run(e.opts()); err != nil {
			t.Fatalf("%v\n%s", err, e.err.String())
		}
		if e.version(e.bin) != "comebye v0.2.0" {
			t.Fatal("not upgraded")
		}
		if strings.Count(e.err.String(), "warning:") != 3 {
			t.Fatalf("warnings:\n%s", e.err.String())
		}
	})
	t.Run("hub, a peer down breaks only sweep", func(t *testing.T) {
		e := newEnv(t, "v0.1.0")
		e.set("fail_sweep", "host laharl: dial tcp: i/o timeout")
		if err := Run(e.opts()); err != nil {
			t.Fatalf("%v\n%s", err, e.err.String())
		}
		if !strings.Contains(e.err.String(), "warning: sweep --dry-run failed") {
			t.Fatal(e.err.String())
		}
	})
	t.Run("hub, own ledger broken is a real failure", func(t *testing.T) {
		e := newEnv(t, "v0.1.0")
		e.set("fail_ledger", "boom")
		if err := Run(e.opts()); !errors.Is(err, ErrRolledBack) {
			t.Fatalf("err = %v", err)
		}
		if e.version(e.bin) != "comebye v0.1.0" {
			t.Fatal("not rolled back")
		}
	})
}

func TestLocalServerDownRollsBack(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	e.local.Close()
	if err := Run(e.opts()); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("err = %v", err)
	}
}

func TestRollbackCommand(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	if err := Run(e.opts()); err != nil {
		t.Fatal(err)
	}
	o := e.opts()
	o.Rollback = true
	if err := Run(o); err != nil {
		t.Fatalf("%v\n%s", err, e.err.String())
	}
	if e.version(e.bin) != "comebye v0.1.0" {
		t.Fatal("not restored")
	}
	// no .prev: refused
	e2 := newEnv(t, "v0.1.0")
	o2 := e2.opts()
	o2.Rollback = true
	if err := Run(o2); err == nil {
		t.Fatal("rollback without .prev succeeded")
	}
}

func TestExplicitTagAndBadTag(t *testing.T) {
	e := newEnv(t, "v0.1.0")
	o := e.opts()
	o.Version = "v0.2.0"
	if err := Run(o); err != nil {
		t.Fatal(err)
	}
	o.Version = "main"
	if err := Run(o); err == nil {
		t.Fatal("a branch name was accepted as a tag")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v0.2.0", "v0.1.0", 1}, {"v0.1.0", "v0.2.0", -1}, {"v0.2.0", "v0.2.0", 0},
		{"v0.10.0", "v0.9.0", 1}, {"v0.2.0-3-gabc1234", "v0.2.0", 1},
		{"v0.2.0", "v0.2.0-3-gabc1234", -1}, {"v0.2.0", "dev", 1}, {"dev", "v0.2.0", -1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestFromSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module stub\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "main.go"), []byte(`package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	switch os.Args[1] {
	case "-version":
		fmt.Println("comebye", version)
	case "whoami":
		fmt.Println(`+"`"+`{"host":"h1","hub":"self","ledger":true}`+"`"+`)
	case "agents":
		fmt.Println(`+"`"+`{"agents":[{"host":"h1"}]}`+"`"+`)
	}
}
`), 0o644)
	git("init", "-q", "-b", "main")
	git("add", "go.mod", "main.go")
	git("commit", "-q", "-m", "x")
	git("tag", "v0.3.0")

	e := newEnv(t, "v0.2.0")
	o := e.opts()
	o.FromSource, o.RepoDir, o.Ref = true, repo, "HEAD"
	if err := Run(o); err != nil {
		t.Fatalf("%v\n%s\n%s", err, e.out.String(), e.err.String())
	}
	if got := e.version(e.bin); got != "comebye v0.3.0" {
		t.Fatalf("installed %s", got)
	}
	if !strings.Contains(e.out.String(), "go vet and go test pass") {
		t.Fatal(e.out.String())
	}
	// the worktree was cleaned up
	out, _ := exec.Command("git", "-C", repo, "worktree", "list").Output()
	if strings.Count(string(out), "\n") != 1 {
		t.Fatalf("worktree left behind:\n%s", out)
	}
	// same again: no-op
	e.out.Reset()
	if err := Run(o); err != nil || !strings.Contains(e.out.String(), "nothing to do") {
		t.Fatalf("%v\n%s", err, e.out.String())
	}
}

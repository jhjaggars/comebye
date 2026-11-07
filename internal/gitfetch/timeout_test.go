package gitfetch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGit puts a script in place of git for fetches; other commands go to the
// real one. body is what a fetch runs.
func fakeGit(t *testing.T, body string) {
	t.Helper()
	real, err := execLookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	p := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = fetch ] && { " + body + "; }; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := gitBin
	gitBin = p
	t.Cleanup(func() { gitBin = old })
}

// A fetch that runs past its timeout gets SIGTERM, so that git can remove its
// temporary pack, not SIGKILL.
func TestATimedOutFetchGetsSIGTERM(t *testing.T) {
	w := newWorld(t, 0)
	mark := filepath.Join(t.TempDir(), "signal")
	fakeGit(t, `sleep 30 & trap 'echo TERM > `+mark+`; kill $!; exit 143' TERM; wait; exit 0`)
	t0 := time.Now()
	res := Once(context.Background(), w.wt1, time.Second, 300*time.Millisecond)
	if res.Reason != "timed out after 300ms" {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(mark); strings.TrimSpace(string(b)) != "TERM" {
		t.Errorf("did not get SIGTERM: %q", b)
	}
	if time.Since(t0) > 5*time.Second {
		t.Errorf("took %s", time.Since(t0))
	}
}

// One that ignores SIGTERM is killed after the grace period.
func TestAFetchIgnoringSIGTERMIsKilledAfterTheGrace(t *testing.T) {
	w := newWorld(t, 0)
	old := TermGrace
	TermGrace = 500 * time.Millisecond
	t.Cleanup(func() { TermGrace = old })
	fakeGit(t, `trap '' TERM; sleep 30 </dev/null >/dev/null 2>&1; exit 0`)
	t0 := time.Now()
	res := Once(context.Background(), w.wt1, time.Second, 300*time.Millisecond)
	took := time.Since(t0)
	if !strings.HasPrefix(res.Reason, "timed out") {
		t.Fatalf("%+v", res)
	}
	if took < 700*time.Millisecond || took > 10*time.Second {
		t.Errorf("took %s: not TERM, wait, then KILL", took)
	}
}

// A branch deleted on origin loses its remote-tracking ref at the next fetch.
func TestFetchPrunesBranchesDeletedUpstream(t *testing.T) {
	w := newWorld(t, 0)
	gitDo(t, w.clone, "push", "-q", "origin", "HEAD:refs/heads/pull-request/1")
	if r := Once(context.Background(), w.wt1, time.Second, 30*time.Second); !r.Fetched {
		t.Fatalf("%+v", r)
	}
	gitDo(t, w.clone, "rev-parse", "--verify", "refs/remotes/origin/pull-request/1")
	gitDo(t, w.origin, "update-ref", "-d", "refs/heads/pull-request/1")
	if r := Once(context.Background(), w.wt1, time.Second, 30*time.Second); !r.Fetched {
		t.Fatalf("%+v", r)
	}
	if out, err := execCommand("git", "-C", w.clone, "rev-parse", "--verify", "-q", "refs/remotes/origin/pull-request/1").Output(); err == nil {
		t.Errorf("still there: %s", out)
	}
}

// A pending fetch: the caller goes on after Wait, and the fetch finishes on its own.
func TestACallerDoesNotWaitForASlowFetch(t *testing.T) {
	w := newWorld(t, time.Second)
	f := &Fetcher{Wait: 200 * time.Millisecond, Timeout: 30 * time.Second}
	t0 := time.Now()
	if r := f.Fetch(context.Background(), w.wt1); !r.Pending {
		t.Fatalf("%+v", r)
	}
	if time.Since(t0) > 800*time.Millisecond {
		t.Errorf("waited %s", time.Since(t0))
	}
	// The next caller waits on the same fetch, and gets its result.
	f.Wait = 10 * time.Second
	if r := f.Fetch(context.Background(), w.wt2); !r.Fetched {
		t.Errorf("%+v", r)
	}
	if n, _ := w.served(t); n != 1 {
		t.Errorf("%d fetches", n)
	}
}

func TestDamageAndBrokenRef(t *testing.T) {
	for in, want := range map[string]string{
		"fatal: missing object a0b27ba6df8f61c2715e78feda483ea2867894a0 for refs/remotes/origin/pull-request/4789\n":    "refs/remotes/origin/pull-request/4789",
		"error: refs/remotes/origin/pull-request/4789: invalid sha1 pointer a0b27ba6df8f61c2715e78feda483ea2867894a0\n": "refs/remotes/origin/pull-request/4789",
		"error: refs/remotes/origin/x does not point to a valid object!\n":                                              "refs/remotes/origin/x",
		"missing tree 2104d49fbb6801abbea9fbbc49d55c05e6bab4ff\n":                                                       "",
		"broken link from    tree 5dc5554030de9f55ccaaf96fe152f7b38cfadeac\n":                                           "",
	} {
		if !IsDamage(in) {
			t.Errorf("not damage: %q", in)
		}
		if got := BrokenRef(in); got != want {
			t.Errorf("%q: ref %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		"fatal: unable to access 'https://h/x': Could not resolve host: h\n",
		"git@h: Permission denied (publickey).\n",
		"fatal: refusing to fetch into branch 'refs/heads/main' checked out\n",
	} {
		if IsDamage(in) {
			t.Errorf("damage: %q", in)
		}
	}
}

var (
	execLookPath = execLook
	execCommand  = execCmd
)

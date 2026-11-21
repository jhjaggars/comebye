package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fetchWorld is a worker VM and a GitHub stand-in, all local: the VM's
// /data/code is a directory of bare repositories reached through a fake
// ssh on PATH, origin is a bare repository, and the local repository is a
// clone of origin.
type fetchWorld struct {
	t      *testing.T
	root   string
	vm     string // the worker's repository (a bare repo under the fake /data/code)
	origin string
	local  string
	dev    string // a scratch clone that plays the worker's agent
	sshLog string
	cfg    *Config
}

func newFetchWorld(t *testing.T) *fetchWorld {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	w := &fetchWorld{t: t, root: root,
		vm: filepath.Join(root, "vm", "code", "comebye"), origin: filepath.Join(root, "origin.git"),
		local: filepath.Join(root, "code", "comebye"), dev: filepath.Join(root, "dev"),
		sshLog: filepath.Join(root, "ssh.log")}
	// No user or system git config: no signing, no hooks, a fixed identity.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// An inherited GIT_SSH_COMMAND (go test sets one) must not decide how
	// the worker is reached.
	t.Setenv("GIT_SSH_COMMAND", "false")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	os.WriteFile(filepath.Join(root, "gitconfig"), []byte("[init]\n\tdefaultBranch = main\n"), 0o600)

	// The fake ssh: it insists on the provisioner's config and the worker's
	// name, logs its arguments, and runs the remote command against the
	// fake /data/code. It never reaches a network.
	w.cfg = testCfg(t)
	cfgPath := filepath.Join(w.cfg.WorkerDir("kv1"), "ssh_config")
	os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	os.WriteFile(cfgPath, []byte("Host kv1\n  HostName 127.0.0.1\n"), 0o600)
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + shellQuote(w.sshLog) + `
[ "$1" = "-F" ] && [ "$2" = ` + shellQuote(cfgPath) + ` ] || { echo "fake ssh: not the provisioner's config: $*" >&2; exit 255; }
host=""; for a in "$@"; do last="$a"; done
for a in "$@"; do [ "$a" = kv1 ] && host=kv1; done
[ "$host" = kv1 ] || { echo "fake ssh: unknown host" >&2; exit 255; }
cmd=$(printf '%s' "$last" | sed "s#/data/code/#` + filepath.Join(root, "vm", "code") + `/#g")
exec sh -c "$cmd"
`
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	w.git(root, "init", "--bare", "-q", w.origin)
	w.git(root, "init", "--bare", "-q", w.vm)
	w.git(root, "clone", "-q", w.origin, w.dev)
	w.commit("base")
	w.git(w.dev, "push", "-q", "origin", "main")
	w.git(w.dev, "push", "-q", w.vm, "main")
	w.git(root, "clone", "-q", w.origin, w.local)
	return w
}

func (w *fetchWorld) git(dir string, args ...string) string {
	w.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit makes a commit in the dev clone and returns its sha.
func (w *fetchWorld) commit(msg string) string {
	w.git(w.dev, "commit", "-q", "--allow-empty", "-m", msg)
	return w.git(w.dev, "rev-parse", "HEAD")
}

// workerBranch puts a branch with one new commit on the worker.
func (w *fetchWorld) workerBranch(branch, msg string) string {
	w.git(w.dev, "checkout", "-q", "-B", branch)
	sha := w.commit(msg)
	w.git(w.dev, "push", "-q", "-f", w.vm, branch)
	w.git(w.dev, "checkout", "-q", "main")
	return sha
}

func (w *fetchWorld) ref(repo, ref string) string {
	out, _ := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", ref).Output()
	return strings.TrimSpace(string(out))
}

func (w *fetchWorld) run(branch string, o fetchOpts) (string, error) {
	var out, errb bytes.Buffer
	e := &env{Out: &out, Err: &errb, Run: osRunner{}, Cfg: w.cfg}
	if o.Repo == "" {
		o.Repo = w.local
	}
	err := e.fetch(context.Background(), "kv1", branch, o)
	return out.String(), err
}

func TestFetchCreatesThenPushes(t *testing.T) {
	w := newFetchWorld(t)
	tip := w.workerBranch("docs/bg-processes-ref", "docs")

	out, err := w.run("docs/bg-processes-ref", fetchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.ref(w.local, "refs/heads/docs/bg-processes-ref"); got != tip {
		t.Fatalf("local branch %q, want %q\n%s", got, tip, out)
	}
	if got := w.ref(w.origin, "refs/heads/docs/bg-processes-ref"); got != "" {
		t.Fatalf("pushed without --push")
	}
	if !strings.Contains(out, "not pushed") {
		t.Errorf("output does not say it was not pushed:\n%s", out)
	}
	if w.ref(w.local, fetchStageRef("kv1", "docs/bg-processes-ref")) != "" {
		t.Errorf("the staging ref was left behind")
	}
	log, _ := os.ReadFile(w.sshLog)
	if !strings.Contains(string(log), "BatchMode=yes") || !strings.Contains(string(log), "/data/code/comebye") {
		t.Errorf("ssh was not called as expected:\n%s", log)
	}

	if out, err = w.run("docs/bg-processes-ref", fetchOpts{Push: true}); err != nil {
		t.Fatal(err)
	}
	if got := w.ref(w.origin, "refs/heads/docs/bg-processes-ref"); got != tip {
		t.Fatalf("origin %q, want %q\n%s", got, tip, out)
	}
	if got := w.ref(w.origin, "refs/heads/main"); got == tip {
		t.Fatal("main moved")
	}

	// The worker adds a commit: both fast-forward.
	w.git(w.dev, "checkout", "-q", "docs/bg-processes-ref")
	tip2 := w.commit("more")
	w.git(w.dev, "push", "-q", w.vm, "docs/bg-processes-ref")
	if out, err = w.run("docs/bg-processes-ref", fetchOpts{Push: true}); err != nil {
		t.Fatal(err)
	}
	if w.ref(w.local, "refs/heads/docs/bg-processes-ref") != tip2 || w.ref(w.origin, "refs/heads/docs/bg-processes-ref") != tip2 {
		t.Fatalf("not fast-forwarded:\n%s", out)
	}
	// Again: nothing to do, and it says so.
	if out, err = w.run("docs/bg-processes-ref", fetchOpts{Push: true}); err != nil || !strings.Contains(out, "already there") {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
}

func TestFetchFromAWorktree(t *testing.T) {
	w := newFetchWorld(t)
	tip := w.workerBranch("feat/x", "x")
	wt := filepath.Join(w.root, "wt")
	w.git(w.local, "worktree", "add", "-q", "-b", "other", wt)
	if out, err := w.run("feat/x", fetchOpts{Repo: wt}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if w.ref(w.local, "refs/heads/feat/x") != tip {
		t.Fatal("not fetched into the repository the worktree belongs to")
	}
}

func TestFetchRefusesUnknownWorker(t *testing.T) {
	w := newFetchWorld(t)
	w.workerBranch("feat/x", "x")
	var out bytes.Buffer
	e := &env{Out: &out, Err: &out, Run: osRunner{}, Cfg: w.cfg}
	for _, name := range []string{"kv9", "../kv1", "KV1", "kv1;id"} {
		err := e.fetch(context.Background(), name, "feat/x", fetchOpts{Repo: w.local, Push: true})
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A directory with no ssh_config in it is not a worker either.
	os.MkdirAll(w.cfg.WorkerDir("pull"), 0o700)
	if err := e.fetch(context.Background(), "pull", "feat/x", fetchOpts{Repo: w.local}); err == nil || !strings.Contains(err.Error(), "not a worker this provisioner knows") {
		t.Errorf("empty state dir: %v", err)
	}
	if b, _ := os.ReadFile(w.sshLog); len(b) != 0 {
		t.Errorf("ssh ran for a worker that was refused:\n%s", b)
	}
}

func TestFetchRefusesDefaultAndOddBranches(t *testing.T) {
	w := newFetchWorld(t)
	// origin's default is "trunk" here, as well as main and master.
	w.git(w.dev, "push", "-q", "origin", "main:trunk")
	w.git(w.root, "-C", w.origin, "symbolic-ref", "HEAD", "refs/heads/trunk")
	for _, b := range []string{"main", "master", "trunk", "-x", "--upload-pack=id", "refs/heads/x", "a:b", "+x", "a b", "x..y", ""} {
		if _, err := w.run(b, fetchOpts{Push: true}); err == nil {
			t.Errorf("branch %q: accepted", b)
		}
	}
	if b, _ := os.ReadFile(w.sshLog); len(b) != 0 {
		t.Errorf("ssh ran for a refused branch:\n%s", b)
	}
	if _, err := w.run("x", fetchOpts{RemoteRepo: "../etc"}); err == nil {
		t.Error("remote repo ../etc accepted")
	}
}

func TestFetchRefusesToOverwriteLocal(t *testing.T) {
	w := newFetchWorld(t)
	tip := w.workerBranch("feat/x", "worker")
	// The local branch holds a commit the worker's does not have.
	w.git(w.local, "branch", "feat/x", "main")
	w.git(w.local, "checkout", "-q", "feat/x")
	w.git(w.local, "commit", "-q", "--allow-empty", "-m", "local only")
	mine := w.ref(w.local, "refs/heads/feat/x")
	w.git(w.local, "checkout", "-q", "main")

	_, err := w.run("feat/x", fetchOpts{Push: true})
	if err == nil || !strings.Contains(err.Error(), "not an ancestor") {
		t.Fatalf("diverged local accepted: %v", err)
	}
	if w.ref(w.local, "refs/heads/feat/x") != mine || w.ref(w.origin, "refs/heads/feat/x") != "" {
		t.Fatal("something moved on a refusal")
	}
	if _, err := w.run("feat/x", fetchOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if w.ref(w.local, "refs/heads/feat/x") != tip {
		t.Fatal("--force did not replace the local branch")
	}
}

func TestFetchRefusesCheckedOutBranch(t *testing.T) {
	w := newFetchWorld(t)
	w.workerBranch("feat/x", "worker")
	w.git(w.local, "checkout", "-q", "-b", "feat/x") // at main, an ancestor
	before := w.ref(w.local, "HEAD")
	_, err := w.run("feat/x", fetchOpts{Force: true})
	if err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("checked-out branch moved: %v", err)
	}
	if w.ref(w.local, "HEAD") != before {
		t.Fatal("HEAD moved")
	}
}

func TestFetchRefusesToOverwriteOrigin(t *testing.T) {
	w := newFetchWorld(t)
	// origin already has feat/x at a commit the worker does not have.
	w.git(w.dev, "checkout", "-q", "-b", "feat/x")
	theirs := w.commit("someone else")
	w.git(w.dev, "push", "-q", "origin", "feat/x")
	w.git(w.dev, "checkout", "-q", "main")
	w.git(w.dev, "branch", "-q", "-D", "feat/x")
	tip := w.workerBranch("feat/x", "worker")

	_, err := w.run("feat/x", fetchOpts{Push: true})
	if err == nil || !strings.Contains(err.Error(), "origin feat/x") {
		t.Fatalf("diverged origin accepted: %v", err)
	}
	if w.ref(w.origin, "refs/heads/feat/x") != theirs {
		t.Fatal("origin moved on a refusal")
	}
	if w.ref(w.local, "refs/heads/feat/x") != tip {
		t.Fatal("the fetch itself should have landed locally")
	}
	if _, err := w.run("feat/x", fetchOpts{Push: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	if w.ref(w.origin, "refs/heads/feat/x") != tip {
		t.Fatal("--force did not replace origin's branch")
	}
}

func TestFetchMissingBranchOnWorker(t *testing.T) {
	w := newFetchWorld(t)
	if _, err := w.run("feat/nope", fetchOpts{Push: true}); err == nil {
		t.Fatal("a branch the worker does not have was accepted")
	}
	if w.ref(w.local, "refs/heads/feat/nope") != "" {
		t.Fatal("made a branch from nothing")
	}
}

func TestFetchFlagsOnlyForFetch(t *testing.T) {
	var out, errb bytes.Buffer
	e := &env{Out: &out, Err: &errb, Run: &fakeRunner{handle: func(c call, f *fakeRunner) (string, string, error) { return "", "", nil }}}
	cfgPath := filepath.Join(t.TempDir(), "config")
	os.WriteFile(cfgPath, []byte("project=comebye-test-0001\nzone=us-east1-b\nssh_source_cidr=198.51.100.1/32\nts_allow_login=a@b.co\nhub_url=https://h.t.ts.net\nhub_peer_url=https://h.t.ts.net\nstate_dir="+t.TempDir()+"\n"), 0o600)
	if code := run([]string{"-config", cfgPath, "status", "--push"}, e); code != 2 {
		t.Errorf("--push on status: exit %d", code)
	}
	if code := run([]string{"-config", cfgPath, "fetch", "kv1"}, e); code != 2 {
		t.Errorf("fetch without a branch: exit %d", code)
	}
	if code := run([]string{"-config", cfgPath, "fetch", "kv1", "feat/x", "--push"}, e); code != 1 || !strings.Contains(errb.String(), "not a worker this provisioner knows") {
		t.Errorf("unknown worker: exit %d\n%s", code, errb.String())
	}
}

package release

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rig builds a throwaway "origin" (a bare repository in a temp directory,
// never GitHub) with one commit on main, a clone of it, and a fake gh that
// answers the two queries the script makes. Nothing leaves the temp dir.
type rig struct {
	t             *testing.T
	origin, clone string
	sha, ghState  string
	script        *Script
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRig(t *testing.T) *rig {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src, err := os.ReadFile("../../tools/release/tag")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	r := &rig{t: t, origin: filepath.Join(root, "origin.git"), clone: filepath.Join(root, "clone")}
	git(t, root, "init", "--bare", "-b", "main", r.origin)
	git(t, root, "clone", r.origin, r.clone)
	git(t, r.clone, "checkout", "-b", "main")
	for _, f := range []string{"a", "b"} {
		os.WriteFile(filepath.Join(r.clone, f), []byte(f), 0o644)
		git(t, r.clone, "add", f)
		git(t, r.clone, "commit", "-m", "add "+f)
	}
	git(t, r.clone, "tag", "-a", "-m", "v0.1.0", "v0.1.0", "HEAD~1")
	git(t, r.clone, "push", "origin", "main", "v0.1.0")
	r.sha = git(t, r.clone, "rev-parse", "HEAD")
	r.ghState = "completed success"
	bin := filepath.Join(root, "bin")
	os.Mkdir(bin, 0o755)
	gh := `#!/bin/sh
case "$*" in
  *"--workflow CI"*) echo "42 $GH_CI https://example.invalid/ci/42" ;;
  *"--workflow Release"*"--json url"*) echo "https://example.invalid/release/$GH_TAG" ;;
  *"--workflow Release"*) echo "completed success https://example.invalid/release/$GH_TAG" ;;
esac
`
	os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.invalid")
	t.Setenv("GH_CI", "completed success")
	t.Setenv("GH_TAG", "v0.2.0")
	r.script = &Script{Source: string(src), Repo: r.clone, Now: func() time.Time { return time.Unix(0, 0) }}
	return r
}

func TestCheckPassesAndListsTheChanges(t *testing.T) {
	r := newRig(t)
	c, err := r.script.Check(context.Background(), "v0.2.0", r.sha)
	if err != nil {
		t.Fatal(err)
	}
	if !c.OK || c.Problem != "" {
		t.Fatalf("not ok: %+v", c)
	}
	if c.Previous != "v0.1.0" || c.CIConclusion != "success" || c.CIURL == "" || c.Commit != r.sha || len(c.Changes) != 1 || !strings.Contains(c.Changes[0], "add b") {
		t.Fatalf("check = %+v", c)
	}
}

func TestCheckRefusals(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	cases := []struct {
		name, version, want string
		setup               func()
	}{
		{"CI pending", "v0.2.0", "still in_progress", func() { t.Setenv("GH_CI", "in_progress ") }},
		{"CI failed", "v0.2.0", "finished with failure", func() { t.Setenv("GH_CI", "completed failure") }},
		{"tag exists", "v0.1.0", "already exists", nil},
		{"not newer", "v0.0.9", "not greater", nil},
		{"bad version", "0.2", "not vMAJOR", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.setup != nil {
				c.setup()
			}
			got, err := r.script.Check(ctx, c.version, r.sha)
			if c.name == "bad version" {
				if err == nil {
					t.Fatal("a bad version must fail validation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.OK || !strings.Contains(got.Problem, c.want) {
				t.Fatalf("problem = %q (ok %v), want %q", got.Problem, got.OK, c.want)
			}
		})
	}
	t.Run("commit off main", func(t *testing.T) {
		os.WriteFile(filepath.Join(r.clone, "c"), []byte("c"), 0o644)
		git(t, r.clone, "add", "c")
		git(t, r.clone, "commit", "-m", "unpushed")
		off := git(t, r.clone, "rev-parse", "HEAD")
		got, err := r.script.Check(ctx, "v0.2.0", off)
		if err != nil {
			t.Fatal(err)
		}
		if got.OK || !strings.Contains(got.Problem, "not on origin/main") {
			t.Fatalf("problem = %q", got.Problem)
		}
	})
}

func TestTagDryRunPushesNothing(t *testing.T) {
	r := newRig(t)
	res, err := r.script.Tag(context.Background(), "v0.2.0", r.sha, true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || res.Pushed || !strings.Contains(res.Output, "Dry run") {
		t.Fatalf("res = %+v", res)
	}
	if out := git(t, r.origin, "tag", "--list", "v0.2.0"); out != "" {
		t.Fatalf("a dry run pushed %q", out)
	}
}

func TestTagPushesOnlyTheTagToTheLocalOrigin(t *testing.T) {
	r := newRig(t)
	res, err := r.script.Tag(context.Background(), "v0.2.0", r.sha, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pushed || res.RunURL != "https://example.invalid/release/v0.2.0" {
		t.Fatalf("res = %+v", res)
	}
	if got := git(t, r.origin, "rev-parse", "v0.2.0^{commit}"); got != r.sha {
		t.Fatalf("tag points at %s, want %s", got, r.sha)
	}
	// A second go is refused by the script itself.
	if _, err := r.script.Tag(context.Background(), "v0.2.0", r.sha, false); err == nil {
		t.Fatal("tagging an existing version must fail")
	}
}

func TestTagRefusesWhenCIIsNotGreen(t *testing.T) {
	r := newRig(t)
	t.Setenv("GH_CI", "completed failure")
	if _, err := r.script.Tag(context.Background(), "v0.2.0", r.sha, false); err == nil {
		t.Fatal("tagged a commit whose CI failed")
	}
	if out := git(t, r.origin, "tag", "--list", "v0.2.0"); out != "" {
		t.Fatalf("pushed %q", out)
	}
}

func TestBuildAndParse(t *testing.T) {
	r := newRig(t)
	b, err := r.script.Build(context.Background(), "v0.2.0")
	if err != nil || b.State != "success" {
		t.Fatalf("build = %+v, %v", b, err)
	}
	now := time.Now()
	for in, want := range map[string]string{"": "none", "completed success https://u": "success", "completed failure https://u": "failure", "in_progress  https://u": "in_progress", "queued  https://u": "queued"} {
		if got := ParseBuild(in, now).State; got != want {
			t.Errorf("ParseBuild(%q) = %s, want %s", in, got, want)
		}
	}
}

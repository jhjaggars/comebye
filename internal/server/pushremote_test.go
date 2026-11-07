package server

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestRemoteOwner(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:jhjaggars/comebye.git":       "jhjaggars",
		"https://github.com/NVIDIA/OpenShell":        "NVIDIA",
		"https://tok@github.com/openclaw/x.git":      "openclaw",
		"ssh://git@github.com/jhjaggars/comebye.git": "jhjaggars",
		"/srv/git/repo.git":                          "",
		"justaname":                                  "",
	} {
		if got := remoteOwner(url); got != want {
			t.Errorf("remoteOwner(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestPickPushRemote(t *testing.T) {
	cases := []struct {
		name    string
		remotes map[string]string
		want    string
	}{
		{"user fork present", map[string]string{"origin": "git@github.com:NVIDIA/OpenShell.git", "fork": "git@github.com:jhjaggars/OpenShell.git"}, "fork"},
		{"origin is the user's", map[string]string{"origin": "https://github.com/JHJaggars/comebye.git"}, "origin"},
		{"origin and a fork both the user's", map[string]string{"origin": "git@github.com:jhjaggars/a.git", "fork": "git@github.com:jhjaggars/b.git"}, "origin"},
		{"only upstream", map[string]string{"origin": "git@github.com:openclaw/openclaw-enterprise.git", "upstream": "git@github.com:openclaw/x.git"}, ""},
		{"no remotes", nil, ""},
	}
	for _, c := range cases {
		if got := pickPushRemote(c.remotes, "jhjaggars"); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := pickPushRemote(map[string]string{"origin": "git@github.com:jhjaggars/a.git"}, ""); got != "" {
		t.Errorf("unknown login picked %q", got)
	}
}

func TestPushRemoteForRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	old := userLogin
	userLogin = func(context.Context) string { return "jhjaggars" }
	defer func() { userLogin = old; loginCache = "" }()
	loginCache = ""
	loginAt = loginAt.Add(-48 * 3600e9)

	dir := t.TempDir()
	run := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "git@github.com:NVIDIA/OpenShell.git")
	if got := pushRemoteFor(context.Background(), dir); !strings.Contains(got, "none was found") || !strings.Contains(got, "do not push") {
		t.Errorf("only upstream: %q", got)
	}
	run("remote", "add", "fork", "git@github.com:jhjaggars/OpenShell.git")
	if got := pushRemoteFor(context.Background(), dir); !strings.HasPrefix(got, "`fork`") {
		t.Errorf("with a fork: %q", got)
	}
}

func TestDefaultTemplatePushRules(t *testing.T) {
	for _, want := range []string{
		"{{push_remote}}", "push your branch early", "after each commit", "to the user's own remote only",
		"Never push to a shared upstream", "NVIDIA/OpenShell", "never force-push", "never push to " + "`main`" + " or the default branch",
		"never open a PR or merge", "fast-forward pushes only", "never rewrite pushed history",
	} {
		if !strings.Contains(defaultHandoffTemplate, want) {
			t.Errorf("the handoff template lacks %q", want)
		}
	}
	if strings.Contains(defaultHandoffTemplate, "When your task lets you push") {
		t.Error("the old conditional push rule is still in the template")
	}
}

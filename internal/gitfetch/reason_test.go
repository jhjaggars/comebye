package gitfetch

import (
	"strings"
	"testing"
	"time"
)

func TestRemoteHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/jhjaggars/OpenShell.git":                 "github.com",
		"https://user:tok@gitlab.cee.redhat.com/service/app.git":     "gitlab.cee.redhat.com",
		"https://x-access-token:ghp_abc@github.com:443/o/r":          "github.com",
		"ssh://git@github.com:22/NVIDIA/OpenShell.git":               "github.com",
		"git@gitlab.cee.redhat.com:service/uhc-clusters-service.git": "gitlab.cee.redhat.com",
		"github.com:o/r.git":          "github.com",
		"/srv/git/widgets.git":        "a local path",
		"../widgets":                  "a local path",
		"file:///srv/git/widgets.git": "a local path",
	} {
		if got := RemoteHost(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// Real stderr from git and ssh, with tokens and paths in it.
func TestFetchReason(t *testing.T) {
	const h = "gitlab.example.com"
	for name, c := range map[string]struct {
		stderr, want string
		timedOut     bool
	}{
		"https dns": {stderr: "fatal: unable to access 'https://u:tok@gitlab.example.com/a/b.git/': Could not resolve host: gitlab.example.com\n", want: "could not resolve host " + h},
		"ssh dns":   {stderr: "ssh: Could not resolve hostname gitlab.example.com: Name or service not known\nfatal: Could not read from remote repository.\n", want: "could not resolve host " + h},
		"ssh key":   {stderr: "git@gitlab.example.com: Permission denied (publickey).\nfatal: Could not read from remote repository.\n", want: "ssh refused by " + h},
		"no creds":  {stderr: "fatal: could not read Username for 'https://gitlab.example.com': terminal prompts disabled\n", want: "no credentials for " + h},
		"gone":      {stderr: "remote: Repository not found.\nfatal: repository 'https://gitlab.example.com/a/b.git/' not found\n", want: "no repository at origin on " + h},
		"refused":   {stderr: "fatal: unable to access 'https://gitlab.example.com/a/': Failed to connect to gitlab.example.com port 443: Connection refused\n", want: "could not connect to " + h},
		"damaged":   {stderr: "error: Could not read a0b27ba6df8f61c2715e78feda483ea2867894a0\nfatal: missing object a0b27ba6df8f61c2715e78feda483ea2867894a0 for refs/remotes/origin/pull-request/4789\n", want: "the local repository has missing or damaged objects"},
		"timeout":   {stderr: "", timedOut: true, want: "timed out after 2m0s"},
		"other":     {stderr: "warning: x\nfatal: refusing to fetch into branch 'refs/heads/main' checked out at '/home/u/x' via https://u:tok@gitlab.example.com/a\n", want: "refusing to fetch into branch refs/heads/main checked out at /home/u/x via " + h},
		"nothing":   {stderr: "", want: "git gave no reason"},
	} {
		got := Reason(c.stderr, h, c.timedOut, 2*time.Minute)
		if got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
		if strings.Contains(got, "tok") || strings.Contains(got, "\n") || len(got) > 110 {
			t.Errorf("%s: %q", name, got)
		}
	}
}

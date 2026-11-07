package server

import (
	"context"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Workers push their branch early as a backup, but only to a remote the user
// owns. The worker's host works out which one when it starts the worker, and
// puts it in the brief ({{push_remote}}), so the worker does not have to guess
// between a fork and a shared upstream.

var remoteOwnerRE = regexp.MustCompile(`^(?:[a-z+]+://)?(?:[^@/]+@)?[^/:]+[:/]+([^/]+)/[^/]+$`)

// remoteOwner is the owner (user or organisation) in a remote URL such as
// git@github.com:owner/repo.git or https://github.com/owner/repo; "" if the URL
// has no owner/repo shape.
func remoteOwner(url string) string {
	m := remoteOwnerRE.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return ""
	}
	return m[1]
}

// pickPushRemote chooses, from remotes (name -> URL), one whose owner is login:
// origin if it is the user's, else the first by name. It returns "" if none is.
func pickPushRemote(remotes map[string]string, login string) string {
	if login == "" {
		return ""
	}
	var names []string
	for n, u := range remotes {
		if strings.EqualFold(remoteOwner(u), login) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	for _, n := range names {
		if n == "origin" {
			return n
		}
	}
	return names[0]
}

// pushRemoteText is what {{push_remote}} becomes in the brief.
func pushRemoteText(remote, login string) string {
	if remote == "" {
		return "none was found (no remote of this repository belongs to the user" + loginNote(login) + "); do not push, and say so in your report"
	}
	return "`" + remote + "`, a remote that belongs to the user"
}

func loginNote(login string) string {
	if login == "" {
		return ", and the user's GitHub login could not be determined"
	}
	return " " + login
}

var (
	loginMu    sync.Mutex
	loginCache string
	loginAt    time.Time
	// userLogin is replaced in tests.
	userLogin = ghLogin
)

// ghLogin is the user's GitHub login: `gh api user`, else git's github.user.
func ghLogin(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "gh", "api", "user", "--jq", ".login").Output(); err == nil {
		if l := strings.TrimSpace(string(out)); l != "" {
			return l
		}
	}
	if out, err := exec.CommandContext(ctx, "git", "config", "--global", "github.user").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// cachedLogin remembers a found login for an hour (an empty one for a minute).
func cachedLogin(ctx context.Context) string {
	loginMu.Lock()
	defer loginMu.Unlock()
	ttl := time.Hour
	if loginCache == "" {
		ttl = time.Minute
	}
	if !loginAt.IsZero() && time.Since(loginAt) < ttl {
		return loginCache
	}
	loginCache, loginAt = userLogin(ctx), time.Now()
	return loginCache
}

// listRemotes is the repository's remotes (name -> fetch URL) as `git remote -v`
// shows them.
func listRemotes(ctx context.Context, dir string) map[string]string {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "remote", "-v").Output()
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && (len(f) < 3 || f[2] == "(fetch)") {
			m[f[0]] = f[1]
		}
	}
	return m
}

// pushRemoteFor is the {{push_remote}} text for the worktree in dir.
func pushRemoteFor(ctx context.Context, dir string) string {
	login := cachedLogin(ctx)
	return pushRemoteText(pickPushRemote(listRemotes(ctx, dir), login), login)
}

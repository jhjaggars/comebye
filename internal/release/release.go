// Package release runs the release steps the app's release page offers:
// checking a commit, tagging it and watching the release build. It does not
// reimplement tools/release/tag: the server hands that very script (embedded
// in the binary, so a checkout cannot swap it) to bash in the checkout that
// owns the repository, and reads its `--check` output.
package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	// VersionRE is a release tag, vMAJOR.MINOR.PATCH (as tools/release/tag).
	VersionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	// CommitRE is a full commit id. Release items carry the whole id so the
	// page and the tag can never mean two different commits.
	CommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Check is what tools/release/tag --check found out about a version and a
// commit. Problem is empty when every precondition holds (OK); fields known
// before the first failing check are still set, so the page can show the
// commit and a CI run that has not finished.
type Check struct {
	Version      string   `json:"version"`
	Commit       string   `json:"commit"`
	Short        string   `json:"short,omitempty"`
	Subject      string   `json:"subject,omitempty"`
	Previous     string   `json:"previous,omitempty"`
	CIID         string   `json:"ci_id,omitempty"`
	CIStatus     string   `json:"ci_status,omitempty"`
	CIConclusion string   `json:"ci_conclusion,omitempty"`
	CIURL        string   `json:"ci_url,omitempty"`
	Changes      []string `json:"changes,omitempty"`
	// TagOnOrigin is the commit the version's tag already points at on
	// origin (empty: no such tag).
	TagOnOrigin string    `json:"tag_on_origin,omitempty"`
	OK          bool      `json:"ok"`
	Problem     string    `json:"problem,omitempty"`
	At          time.Time `json:"at"`
}

// TagResult is the outcome of a tagging.
type TagResult struct {
	Pushed bool   `json:"pushed"`
	DryRun bool   `json:"dry_run,omitempty"`
	RunURL string `json:"run_url,omitempty"`
	Output string `json:"output,omitempty"`
}

// Build is the state of the Release workflow run for a tag. State is
// "none" (no run yet), "queued", "in_progress", "success" or "failure".
type Build struct {
	State string    `json:"state"`
	URL   string    `json:"url,omitempty"`
	At    time.Time `json:"at"`
}

// Tool is what the server needs from a release implementation.
type Tool interface {
	Check(ctx context.Context, version, commit string) (Check, error)
	// Tag tags commit as version and pushes the tag. With dry it does every
	// check and changes nothing.
	Tag(ctx context.Context, version, commit string, dry bool) (TagResult, error)
	Build(ctx context.Context, version string) (Build, error)
}

// Exec runs a command and returns its combined output.
type Exec func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)

func defaultExec(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.Bytes(), err
}

// Script is the real Tool: the tag script, run in Repo.
type Script struct {
	Source string // the tools/release/tag script text
	Repo   string // the checkout that owns the repository (a dedicated clone: see docs/releasing.md)
	// Slug pins gh and origin to this GitHub repository (default
	// jhjaggars/comebye); Origin, for tests only, is one more URL accepted
	// as origin (a local stand-in for GitHub).
	Slug, Origin string
	Exec         Exec // default: os/exec
	Now          func() time.Time
}

func (s *Script) exec() Exec {
	if s.Exec != nil {
		return s.Exec
	}
	return defaultExec
}

func (s *Script) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Script) run(ctx context.Context, args ...string) ([]byte, error) {
	// bash -c SCRIPT NAME ARGS...: the script comes from the binary, and
	// stdin is not the script, so git cannot swallow it.
	a := append([]string{"-c", s.Source, "tag"}, args...)
	return s.exec()(ctx, s.Repo, s.env(), "bash", a...)
}

func (s *Script) env() []string {
	e := []string{"COMEBYE_RELEASE_REPO=" + s.Repo, "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1"}
	if s.Slug != "" {
		e = append(e, "COMEBYE_RELEASE_SLUG="+s.Slug)
	}
	if s.Origin != "" {
		e = append(e, "COMEBYE_RELEASE_ORIGIN="+s.Origin)
	}
	return e
}

func validate(version, commit string) error {
	if !VersionRE.MatchString(version) {
		return fmt.Errorf("%q is not vMAJOR.MINOR.PATCH", version)
	}
	if !CommitRE.MatchString(commit) {
		return errors.New("the commit must be a full 40-character id")
	}
	return nil
}

// Check runs the script with --check.
func (s *Script) Check(ctx context.Context, version, commit string) (Check, error) {
	if err := validate(version, commit); err != nil {
		return Check{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := s.run(ctx, version, commit, "--check")
	c := ParseCheck(string(out))
	c.Version, c.At = version, s.now()
	if err != nil {
		return c, fmt.Errorf("release check: %w: %s", err, tail(string(out), 400))
	}
	if !c.OK && c.Problem == "" {
		c.Problem = "the check printed no verdict: " + tail(string(out), 300)
	}
	// The script resolved the id itself; refuse a check about another commit.
	if c.Commit != "" && c.Commit != commit {
		return c, fmt.Errorf("release check resolved %s, not %s", c.Commit, commit)
	}
	if c.Commit == "" {
		c.Commit = commit
	}
	return c, nil
}

// ParseCheck reads --check's key=value lines.
func ParseCheck(out string) Check {
	var c Check
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		switch k {
		case "commit":
			c.Commit = v
		case "short":
			c.Short = v
		case "subject":
			c.Subject = v
		case "previous":
			c.Previous = v
		case "ci_id":
			c.CIID = v
		case "ci_status":
			c.CIStatus = v
		case "ci_conclusion":
			c.CIConclusion = v
		case "ci_url":
			c.CIURL = v
		case "tag_on_origin":
			if CommitRE.MatchString(v) {
				c.TagOnOrigin = v
			}
		case "change":
			c.Changes = append(c.Changes, v)
		case "problem":
			c.Problem, c.OK = v, false
		case "ok":
			c.OK = v == "1" && c.Problem == ""
		}
	}
	return c
}

// The script's own lines start in column 0; commit subjects it lists are
// indented, so text in a commit message cannot pass for them.
var (
	runURLRE = regexp.MustCompile(`(?m)^Release workflow: (https://\S+)`)
	pushedRE = regexp.MustCompile(`(?m)^Pushed (v\S+) `)
)

// Tag runs the script with --yes (or --dry-run). The script re-checks
// everything itself before it tags.
func (s *Script) Tag(ctx context.Context, version, commit string, dry bool) (TagResult, error) {
	if err := validate(version, commit); err != nil {
		return TagResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	mode := "--yes"
	if dry {
		mode = "--dry-run"
	}
	out, err := s.run(ctx, version, commit, mode)
	res := TagResult{DryRun: dry, Output: tail(string(out), 4000)}
	if err != nil {
		return res, fmt.Errorf("%w: %s", err, tail(string(out), 400))
	}
	if all := runURLRE.FindAllStringSubmatch(string(out), -1); len(all) > 0 {
		res.RunURL = all[len(all)-1][1]
	}
	for _, m := range pushedRE.FindAllStringSubmatch(string(out), -1) {
		if m[1] == version {
			res.Pushed = !dry
		}
	}
	if !dry && !res.Pushed {
		return res, errors.New("the script ended without saying it pushed the tag: " + tail(string(out), 300))
	}
	return res, nil
}

// Build asks GitHub for the Release workflow run of the tag.
func (s *Script) Build(ctx context.Context, version string) (Build, error) {
	if !VersionRE.MatchString(version) {
		return Build{}, errors.New("bad version")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := s.exec()(ctx, s.Repo, s.ghEnv(), "gh", "run", "list", "--workflow", "Release", "--branch", version,
		"--limit", "1", "--json", "status,conclusion,url", "--jq", `.[] | "\(.status) \(.conclusion) \(.url)"`)
	if err != nil {
		return Build{}, fmt.Errorf("gh run list: %w: %s", err, tail(string(out), 200))
	}
	return ParseBuild(string(out), s.now()), nil
}

func (s *Script) ghEnv() []string {
	slug := s.Slug
	if slug == "" {
		slug = "jhjaggars/comebye"
	}
	return []string{"GH_PROMPT_DISABLED=1", "GH_REPO=" + slug}
}

// ParseBuild reads one "status conclusion url" line of gh's output.
func ParseBuild(out string, now time.Time) Build {
	// The conclusion is empty while a run is going, so the fields cannot
	// be split on whitespace: status first, the URL last.
	f := strings.Fields(out)
	if len(f) < 2 || !strings.HasPrefix(f[len(f)-1], "https://") {
		return Build{State: "none", At: now}
	}
	status, conclusion := f[0], ""
	if len(f) > 2 {
		conclusion = f[1]
	}
	b := Build{URL: f[len(f)-1], At: now}
	switch {
	case status == "completed" && conclusion == "success":
		b.State = "success"
	case status == "completed":
		b.State = "failure"
	case status == "in_progress":
		b.State = "in_progress"
	default:
		b.State = "queued"
	}
	return b
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}

// Simulated is a Tool that runs nothing: canned answers for demos and the
// browser tests (-release-simulate). It "tags" in memory only.
type Simulated struct {
	mu     sync.Mutex
	Tagged map[string]bool
	Now    func() time.Time
}

func (s *Simulated) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Simulated) Check(_ context.Context, version, commit string) (Check, error) {
	if err := validate(version, commit); err != nil {
		return Check{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := Check{Version: version, Commit: commit, Short: commit[:7], Subject: "simulated commit", Previous: "v0.0.0",
		CIID: "1", CIStatus: "completed", CIConclusion: "success", CIURL: "https://example.invalid/ci/1",
		Changes: []string{commit[:7] + " simulated change", "0000000 an earlier simulated change"}, OK: true, At: s.now()}
	if s.Tagged[version] {
		c.OK, c.Problem = false, "tag "+version+" already exists locally"
	}
	return c, nil
}

func (s *Simulated) Tag(_ context.Context, version, commit string, dry bool) (TagResult, error) {
	if err := validate(version, commit); err != nil {
		return TagResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if dry {
		return TagResult{DryRun: true, Output: "Dry run (simulated). Would run:\n  git tag -a -m " + version + " " + version + " " + commit}, nil
	}
	if s.Tagged == nil {
		s.Tagged = map[string]bool{}
	}
	s.Tagged[version] = true
	return TagResult{Pushed: true, RunURL: "https://example.invalid/release/" + version, Output: "Pushed " + version + " (simulated)."}, nil
}

func (s *Simulated) Build(_ context.Context, version string) (Build, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Tagged[version] {
		return Build{State: "success", URL: "https://example.invalid/release/" + version, At: s.now()}, nil
	}
	return Build{State: "none", At: s.now()}, nil
}

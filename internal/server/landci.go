package server

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/ledger"
)

// ghRuns lists a commit's workflow runs as JSON (`gh run list --commit`),
// run in dir (a checkout of the repository). Tests replace it.
var ghRuns = func(ctx context.Context, dir, sha string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "run", "list", "--commit", sha, "--limit", "30", "--json", "status,conclusion,url,name")
	cmd.Dir = dir
	return cmd.Output()
}

// ciOf reads the CI state of a commit: running while any run has not
// completed, failure when a completed run failed (or was cancelled or timed
// out), success when every run completed well, none when there is no run.
// nil when gh cannot say (not installed, not logged in, no network): the
// card then shows nothing rather than a guess.
func ciOf(ctx context.Context, dir, sha string) *ledger.CI {
	if sha == "" || dir == "" {
		return nil
	}
	out, err := ghRuns(ctx, dir, sha)
	if err != nil {
		return nil
	}
	return ciFromRuns(out, sha)
}

// ciFromRuns folds `gh run list --json status,conclusion,url,name`.
func ciFromRuns(out []byte, sha string) *ledger.CI {
	var runs []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		URL        string `json:"url"`
		Name       string `json:"name"`
	}
	if json.Unmarshal(out, &runs) != nil {
		return nil
	}
	ci := &ledger.CI{State: ledger.CINone, SHA: sha, Runs: len(runs), At: time.Now().UTC()}
	if len(runs) == 0 {
		return ci
	}
	running, failed, link := false, false, ""
	for _, r := range runs {
		if link == "" {
			link = r.URL
		}
		if strings.ToLower(r.Status) != "completed" {
			running = true
			continue
		}
		switch strings.ToLower(r.Conclusion) {
		case "success", "skipped", "neutral":
		default:
			failed = true
			link = r.URL // the failing run is the one to look at
		}
	}
	ci.URL = link
	switch {
	case failed:
		ci.State = ledger.CIFailure // as soon as one run fails, even while others run
	case running:
		ci.State = ledger.CIRunning
	default:
		ci.State = ledger.CISuccess
	}
	return ci
}

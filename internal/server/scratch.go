package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/scratch"
)

// Scratch space: what agent sessions leave in the host's temporary directory
// (see package scratch). Removing a session cleans up after it; a periodic
// prune and `comebye scratch` catch what that missed.

const (
	scratchFirstRun = 2 * time.Minute
	// DefaultScratchEvery is how often the prune runs.
	DefaultScratchEvery = time.Hour
)

func (s *Server) scratchOpts(dry bool) scratch.Options {
	return scratch.Options{TmpDir: s.cfg.ScratchDir, DryRun: dry, Logf: log.Printf}
}

// TmpUsage reports how full the temporary directory is, for the host status.
func (s *Server) TmpUsage() *scratch.Usage {
	if s.cfg.ScratchDir == "" {
		return nil
	}
	return scratch.UsageOf(s.cfg.ScratchDir)
}

// cleanRemoved cleans up after a worktree that was just removed: its
// scratchpad, and processes still running in it. A failure is logged and
// never fails the removal. closed is the agent the removal closed.
func (s *Server) cleanRemoved(wt string, live []herdr.AgentInfo, closed *herdr.AgentInfo) *scratch.Report {
	if s.cfg.ScratchDir == "" {
		return nil
	}
	var cwds []string
	for _, a := range live {
		if closed != nil && a.PaneID == closed.PaneID {
			continue
		}
		cwds = append(cwds, a.CWD)
		if r, err := filepath.EvalSymlinks(a.CWD); err == nil {
			cwds = append(cwds, r)
		}
	}
	rep := scratch.CleanWorktree(s.scratchOpts(false), wt, cwds)
	return &rep
}

// livePaths are the directories whose scratchpads must stay: the working
// directories of every live agent, every open workspace's checkout, and all
// the worktrees and primary checkouts of the repositories under the launch
// roots. An error means the answer is incomplete, and nothing may be pruned.
func (s *Server) livePaths(ctx context.Context) ([]string, error) {
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		out = append(out, p)
		if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
			out = append(out, r)
		}
	}
	agents, err := s.client.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		add(a.CWD)
	}
	wss, err := s.client.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range wss {
		if w.Worktree != nil {
			add(w.Worktree.CheckoutPath)
		}
	}
	for _, repo := range repoDirs(s.checkRoots()) {
		add(repo)
		wts, err := s.client.ListWorktrees(ctx, repo)
		if err != nil {
			return nil, err
		}
		for _, wt := range wts {
			add(wt.Path)
		}
	}
	return out, nil
}

// repoDirs lists the git repositories in roots (a root itself, its children
// and grandchildren), the same walk as the repository picker.
func repoDirs(roots []string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if len(out) >= maxRepos || seen[dir] {
			return
		}
		seen[dir] = true
		if fi, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
			out = append(out, dir)
			return
		}
		if depth == 2 {
			return
		}
		list, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range list {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				walk(filepath.Join(dir, e.Name()), depth+1)
			}
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return out
}

// pruneScratch runs one prune.
func (s *Server) pruneScratch(ctx context.Context, dry bool) (scratch.Report, error) {
	live, err := s.livePaths(ctx)
	if err != nil {
		return scratch.Report{DryRun: dry}, err
	}
	return scratch.Prune(s.scratchOpts(dry), scratch.PruneParams{Live: live, TmpMaxAge: s.cfg.TmpMaxAge}), nil
}

// RunScratchPrune prunes the temporary directory now and then.
func (s *Server) RunScratchPrune(ctx context.Context) {
	if s.cfg.ScratchDir == "" || s.cfg.ScratchEvery <= 0 || s.client == nil {
		return
	}
	t := time.NewTimer(scratchFirstRun)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		rep, err := s.pruneScratch(cctx, false)
		cancel()
		switch {
		case err != nil:
			log.Printf("scratch: prune skipped: %v", err)
		case !rep.Empty():
			log.Printf("scratch: prune: %s", rep.Summary())
		}
		t.Reset(s.cfg.ScratchEvery)
	}
}

// handleScratch prunes now: POST {dry_run}.
func (s *Server) handleScratch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DryRun bool `json:"dry_run"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if s.cfg.ScratchDir == "" {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "scratch_disabled", "message": "scratch cleanup is not configured on this host"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	rep, err := s.pruneScratch(ctx, body.DryRun)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"code": "live_unknown", "message": "could not tell which worktrees are live, so nothing was pruned: " + errMessage(err)})
		return
	}
	writeJSON(w, map[string]any{"report": rep, "usage": s.TmpUsage()})
}

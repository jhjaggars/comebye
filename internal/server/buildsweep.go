package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jhjaggars/comebye/internal/buildout"
)

// buildSweepMin is the build output a worktree must hold to be listed (1 GB).
const buildSweepMin = int64(1_000_000_000)

type buildSweepBody struct {
	Clear    bool  `json:"clear"`
	MinBytes int64 `json:"min_bytes,omitempty"`
}

type buildSweepRow struct {
	Worktree string         `json:"worktree"`
	Parked   string         `json:"parked,omitempty"` // the parked agent's name, if one can resume there
	Bytes    int64          `json:"bytes"`
	Dirs     []buildout.Dir `json:"dirs"`
	Cleared  bool           `json:"cleared,omitempty"`
	Error    string         `json:"error,omitempty"`
}

type buildSweepResult struct {
	Host    string          `json:"host"`
	Clear   bool            `json:"clear"`
	Rows    []buildSweepRow `json:"rows"`
	Total   int64           `json:"total_bytes"`
	Freed   int64           `json:"freed_bytes"`
	Skipped int             `json:"skipped_live"` // worktrees with a live agent, left alone
}

// handleBuildSweep is POST /api/build-sweep: the worktrees on this host with no live
// agent whose build output exceeds 1 GB (a dry run), and with clear:true it
// deletes that build output.
func (s *Server) handleBuildSweep(w http.ResponseWriter, r *http.Request) {
	var b buildSweepBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&b); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	res, lerr := s.buildSweep(ctx, b)
	if lerr != nil {
		writeLaunchError(w, lerr)
		return
	}
	writeJSON(w, res)
}

func (s *Server) buildSweep(ctx context.Context, b buildSweepBody) (*buildSweepResult, *launchError) {
	root := s.worktreeRoot()
	if !filepath.IsAbs(root) {
		return nil, errLaunchDisabled
	}
	live, err := s.client.ListAgents(ctx)
	if err != nil {
		return nil, herdrFailure("list agents", err) // never clear without knowing who is live
	}
	min := b.MinBytes
	if min <= 0 {
		min = buildSweepMin
	}
	parked := map[string]string{}
	if ps := s.parkStore(); ps != nil {
		for _, r := range ps.List(false) {
			if r.Parked() && r.Worktree != "" {
				parked[filepath.Clean(r.Worktree)] = r.Name
			}
		}
	}
	res := &buildSweepResult{Host: s.fleet.LocalName(), Clear: b.Clear, Rows: []buildSweepRow{}}
	for _, wt := range linkedWorktrees(root) {
		if ctx.Err() != nil {
			break
		}
		inUse := false
		for _, a := range live {
			if cwd, err := filepath.EvalSymlinks(a.CWD); err == nil && within(cwd, wt) {
				inUse = true
				break
			}
		}
		if inUse {
			res.Skipped++
			continue
		}
		dirs, err := buildout.Find(ctx, wt)
		if err != nil || buildout.Total(dirs) < min {
			continue
		}
		row := buildSweepRow{Worktree: wt, Parked: parked[filepath.Clean(wt)], Bytes: buildout.Total(dirs), Dirs: dirs}
		res.Total += row.Bytes
		if b.Clear {
			freed, _, err := buildout.Clear(ctx, wt)
			res.Freed += freed
			row.Cleared = err == nil
			if err != nil {
				row.Error = err.Error()
			}
		}
		res.Rows = append(res.Rows, row)
	}
	sort.Slice(res.Rows, func(i, j int) bool { return res.Rows[i].Bytes > res.Rows[j].Bytes })
	return res, nil
}

// linkedWorktrees lists the linked git worktrees (a .git file) one or two
// levels under root: <root>/<repo>/<name> and <root>/<name>.
func linkedWorktrees(root string) []string {
	var out []string
	isWT := func(p string) bool {
		fi, err := os.Lstat(filepath.Join(p, ".git"))
		return err == nil && fi.Mode().IsRegular()
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(root, e.Name())
		if isWT(p) {
			out = append(out, p)
			continue
		}
		subs, _ := os.ReadDir(p)
		for _, sd := range subs {
			if q := filepath.Join(p, sd.Name()); sd.IsDir() && isWT(q) {
				out = append(out, q)
			}
		}
	}
	return out
}

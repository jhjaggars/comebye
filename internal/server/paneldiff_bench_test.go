package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Benchmarks for the Diff panel's host op (panelDiffOf), the work behind
// GET /api/panel/diff. They exist to show where the time goes:
//
//	go test ./internal/server -run '^$' -bench PanelDiff -benchtime 5x
//	COMEBYE_BENCH_DIR=/path/to/checkout go test ./internal/server -run '^$' -bench PanelDiffDir -benchtime 5x
//
// Each run also prints the git processes one request starts (count and wall
// time per command), read from git's trace2 event stream.

// benchRepo builds a repository with nFiles tracked files of ~60 lines, a
// "main" branch, a feature branch with commits, then nDirty uncommitted
// edits, nStaged staged edits and nUntracked new files.
func benchRepo(tb testing.TB, nFiles, nCommits, nDirty, nStaged, nUntracked int) string {
	tb.Helper()
	dir := tb.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=b", "GIT_AUTHOR_EMAIL=b@x", "GIT_COMMITTER_NAME=b", "GIT_COMMITTER_EMAIL=b@x")
		if out, err := cmd.CombinedOutput(); err != nil {
			tb.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	body := func(i, rev int) string {
		var sb strings.Builder
		for l := 0; l < 60; l++ {
			fmt.Fprintf(&sb, "line %d of file %d rev %d: the quick brown fox jumps over the lazy dog\n", l, i, rev)
		}
		return sb.String()
	}
	write := func(rel, s string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	for i := 0; i < nFiles; i++ {
		write(fmt.Sprintf("pkg%d/f%d.go", i%40, i), body(i, 0))
	}
	git("add", ".")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "feature")
	for c := 0; c < nCommits; c++ {
		write(fmt.Sprintf("pkg%d/f%d.go", c%40, c), body(c, 100+c))
		git("commit", "-q", "-am", fmt.Sprintf("commit %d", c))
	}
	for i := 0; i < nDirty; i++ {
		k := nCommits + i
		write(fmt.Sprintf("pkg%d/f%d.go", k%40, k), body(k, 200))
	}
	for i := 0; i < nStaged; i++ {
		k := nCommits + nDirty + i
		write(fmt.Sprintf("pkg%d/f%d.go", k%40, k), body(k, 300))
		git("add", fmt.Sprintf("pkg%d/f%d.go", k%40, k))
	}
	for i := 0; i < nUntracked; i++ {
		write(fmt.Sprintf("new/u%d.txt", i), body(i, 400))
	}
	return dir
}

type gitCmdStat struct {
	argv  string
	n     int
	total time.Duration
}

// traceGit runs f with git's trace2 event output on and returns the git
// processes it started (the top-level ones only), slowest total first.
func traceGit(tb testing.TB, f func()) []gitCmdStat {
	tb.Helper()
	tf := filepath.Join(tb.TempDir(), "trace2")
	tb.Setenv("GIT_TRACE2_EVENT", tf)
	f()
	tb.Setenv("GIT_TRACE2_EVENT", "")
	data, _ := os.ReadFile(tf)
	type ev struct {
		Event string   `json:"event"`
		Sid   string   `json:"sid"`
		Argv  []string `json:"argv"`
		TAbs  float64  `json:"t_abs"`
	}
	argvOf := map[string]string{}
	var stats []gitCmdStat
	idx := map[string]int{}
	for _, l := range strings.Split(string(data), "\n") {
		var e ev
		if json.Unmarshal([]byte(l), &e) != nil || strings.Contains(e.Sid, "/") {
			continue // child processes of git have a "parent/child" sid
		}
		switch e.Event {
		case "start":
			a := e.Argv
			if len(a) > 0 {
				a[0] = "git"
			}
			// drop the leading -C dir -c k=v noise
			var keep []string
			for i := 0; i < len(a); i++ {
				if a[i] == "-C" || a[i] == "-c" {
					i++
					continue
				}
				keep = append(keep, a[i])
			}
			argvOf[e.Sid] = strings.Join(keep, " ")
		case "atexit":
			k := argvOf[e.Sid]
			if len(k) > 60 {
				k = k[:60]
			}
			if i, ok := idx[k]; ok {
				stats[i].n++
				stats[i].total += time.Duration(e.TAbs * float64(time.Second))
			} else {
				idx[k] = len(stats)
				stats = append(stats, gitCmdStat{k, 1, time.Duration(e.TAbs * float64(time.Second))})
			}
		}
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].total > stats[j].total })
	return stats
}

func benchPanelDiff(b *testing.B, dir string) {
	ctx := context.Background()
	d := panelDiffOf(ctx, dir, "") // warm the page cache; also the size of the answer
	raw, _ := json.Marshal(d)
	gz := gzipLen(raw)
	stats := traceGit(b, func() { panelDiffOf(ctx, dir, "") })
	var procs int
	var sum time.Duration
	var sb strings.Builder
	for _, s := range stats {
		procs += s.n
		sum += s.total
		fmt.Fprintf(&sb, "\n    %2dx %8.1fms  %s", s.n, float64(s.total)/1e6, s.argv)
	}
	b.Logf("files=%d add=%d del=%d json=%dKB gzip=%dKB git procs=%d (sum of git wall %.0fms)%s",
		d.Files, d.Add, d.Del, len(raw)>>10, gz>>10, procs, float64(sum)/1e6, sb.String())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		panelDiffOf(ctx, dir, "")
	}
}

func BenchmarkPanelDiffSmall(b *testing.B) {
	benchPanelDiff(b, benchRepo(b, 200, 3, 5, 2, 3))
}

func BenchmarkPanelDiffLarge(b *testing.B) {
	benchPanelDiff(b, benchRepo(b, 5000, 20, 60, 20, 30))
}

// BenchmarkPanelDiffClean is a checkout with nothing changed: the common case
// for a poll that finds nothing new.
func BenchmarkPanelDiffClean(b *testing.B) {
	benchPanelDiff(b, benchRepo(b, 5000, 0, 0, 0, 0))
}

func BenchmarkPanelDiffDir(b *testing.B) {
	dir := os.Getenv("COMEBYE_BENCH_DIR")
	if dir == "" {
		b.Skip("set COMEBYE_BENCH_DIR to a checkout")
	}
	benchPanelDiff(b, dir)
}

func gzipLen(b []byte) int {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Len()
}

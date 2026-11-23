package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// apiHub is a throwaway hub for `comebye api run` tests: the built binary,
// its own state dir and a fake herdr with the dev fixtures.
type apiHub struct {
	t       *testing.T
	root    string // the repository
	comebye string // the built binary
	env     []string
}

// hubAgo is a time h hours ago, as the ledger writes it.
func hubAgo(h float64) string {
	return time.Now().UTC().Add(-time.Duration(h * float64(time.Hour))).Format(time.RFC3339)
}

// hubEntry is a ledger entry on host devbox for the comebye repo; kv sets more fields.
func hubEntry(i int, name, pane, status string, kv ...any) map[string]any {
	e := map[string]any{"id": fmt.Sprintf("h%011x", 0xabc000+i), "host": "devbox", "repo": "/home/u/code/comebye", "repo_name": "comebye",
		"branch": "feat/" + name, "kind": "claude", "agent_name": name, "agent_id": "devbox/" + pane, "pane_id": pane, "brief": "do " + name,
		"status": status, "created_at": hubAgo(30)}
	for i := 0; i+1 < len(kv); i += 2 {
		e[kv[i].(string)] = kv[i+1]
	}
	return e
}

// startAPIHub writes the ledger (and the to-dos, when todos is not nil)
// into a new state dir and starts the hub on it.
func startAPIHub(t *testing.T, entries []any, todos []any) *apiHub {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and starts comebye")
	}
	needPython(t)
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	d, err := os.MkdirTemp("", "cbapi") // short: the herdr socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	bin := func(name, pkg string) string {
		out := filepath.Join(d, name)
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
		return out
	}
	comebye, herdr := bin("comebye", "."), bin("fake-herdr", "./tools/devserver")

	state := filepath.Join(d, "state")
	os.MkdirAll(state, 0o700)
	write := func(name string, v any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(state, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ledger.json", map[string]any{"version": 1, "marks": []any{}, "entries": entries})
	if todos != nil {
		write("todo.json", map[string]any{"version": 1, "items": todos})
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	baseEnv := []string{"HOME=" + filepath.Join(d, "home"), "PATH=" + os.Getenv("PATH"), "COMEBYE_SCRATCH_DIR=" + filepath.Join(d, "scratch")}
	start := func(name, bin string, args ...string) {
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Env = baseEnv
		out := &lockedBuf{}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cancel()
			cmd.Wait()
			if t.Failed() {
				t.Logf("%s log:\n%s", name, out.String())
			}
		})
	}
	waitFor := func(what string, ok func() bool) {
		for i := 0; i < 200; i++ {
			if ok() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	sock := filepath.Join(d, "h.sock")
	start("herdr", herdr, "-sock", sock, "-dir", filepath.Join(d, "tx"), "-fixtures")
	waitFor("the fake herdr", func() bool { _, err := os.Stat(sock); return err == nil })
	start("hub", comebye, "-sock", sock, "-addr", addr, "-insecure-no-auth", "-test-mode", "-push=false", "-state-dir", state, "-host-name", "devbox")
	waitFor("the hub", func() bool {
		r, err := http.Get("http://" + addr + "/api/agents")
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
	// The CLI, as an agent with no pane of its own, aimed at the hub.
	return &apiHub{t: t, root: root, comebye: comebye, env: append(baseEnv, "COMEBYE_URL=http://"+addr, "COMEBYE_STATE_DIR="+state)}
}

// cli runs the CLI in dir (relative to the repository) with stdin and extra env.
func (h *apiHub) cli(dir, stdin string, env []string, args ...string) (int, string, string) {
	cmd := exec.Command(h.comebye, args...)
	cmd.Dir = filepath.Join(h.root, dir)
	cmd.Env = append(append([]string{}, h.env...), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatal(err)
	}
	return code, out.String(), errb.String()
}

func (h *apiHub) must(dir, stdin string, args ...string) {
	h.t.Helper()
	if code, out, errs := h.cli(dir, stdin, nil, args...); code != 0 {
		h.t.Fatalf("comebye %v: exit %d\n%s%s", args, code, out, errs)
	}
}

// `comebye api run` with the built binary against a throwaway hub: the
// module's hub tests (internal/apipy/tests/hub_check.py) and the guide's
// example script.
func TestAPIRunAgainstAHub(t *testing.T) {
	ago := hubAgo
	h := startAPIHub(t, []any{
		hubEntry(1, "fix-detail-menu", "w3:p1", "running", "progress", map[string]any{"text": "running the browser tests", "at": ago(0.05)}),
		hubEntry(2, "old-worker", "w7:p1", "finished", "final_report", "## Summary\nall done", "report_source", "explicit", "report_seq", 1, "report_at", ago(50), "finished_at", ago(50)),
		hubEntry(3, "ghost-worker", "w99:p1", "finished", "finished_at", ago(3)),
		hubEntry(4, "pr-watcher", "w4:p1", "running", "loop", "watch PRs", "final_report", "pass 1: quiet", "report_source", "explicit", "report_seq", 1,
			"report_at", ago(26), "last_pass_report", "pass 9: PR #12 green", "last_pass_report_at", ago(0.5)),
	}, nil)
	const dir = "internal/apipy"
	h.must(dir, "", "todo", "add", "Land PR #12?", "--kind", "followup")
	h.must(dir, "", "prompt", "needs-input", "please check the flaky test", "--queue")
	h.must(dir, "# Hello\n", "publish", "-", "--title", "API demo page", "--kind", "md")

	code, out, errs := h.cli(dir, "", nil, "api", "run", "tests/hub_check.py")
	if code != 0 || !strings.Contains(errs, "OK") {
		t.Fatalf("hub_check.py: exit %d\n%s%s", code, out, errs)
	}
	t.Logf("hub_check.py:\n%s", errs)

	code, out, errs = h.cli(dir, helpExample(t), nil, "api", "run", "-")
	if code != 0 {
		t.Fatalf("the guide's example: exit %d\n%s%s", code, out, errs)
	}
	for _, want := range []string{
		"hub devbox, as agent",
		"running   fix-detail-menu          progress ", "m ago: running the browser tests",
		"finished  old-worker               its newest reply is 2d ago",
		"finished  ghost-worker             finished without a report; its agent is gone",
		"running   pr-watcher               ok",
		"1 queued prompts, 0 held",
		"removals: the user's to review",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide's example: no %q in\n%s%s", want, out, errs)
		}
	}
}

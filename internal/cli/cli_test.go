package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/agenttoken"
)

// fakeComebye is a stand-in local server: it checks the agent token and
// answers with canned JSON, recording each request.
type fakeComebye struct {
	t     *testing.T
	token string
	mu    sync.Mutex
	reqs  []recorded
	// answers by "METHOD /path": status and body.
	answers map[string]answer
}

type recorded struct {
	method, path, query, body string
	hdr                       http.Header
}

type answer struct {
	code int
	body string
}

func (f *fakeComebye) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Clone()})
	a, ok := f.answers[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get(headerToken) != f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == "POST" && r.Header.Get("Content-Type") != "application/json" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"code":"not_found","message":"no route `+r.URL.Path+`"}`)
		return
	}
	if a.code == 0 {
		a.code = 200
	}
	w.WriteHeader(a.code)
	io.WriteString(w, a.body)
}

func (f *fakeComebye) find(method, path string) *recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i].method == method && f.reqs[i].path == path {
			r := f.reqs[i]
			return &r
		}
	}
	return nil
}

const agentsJSON = `{"agents":[
 {"id":"laharl/w1:p1","host":"laharl","pane_id":"w1:p1","name":"coordinator","agent":"claude","agent_status":"idle","cwd":"/home/u"},
 {"id":"laharl/w2:p1","host":"laharl","pane_id":"w2:p1","name":"home","agent":"pi","agent_status":"working","cwd":"/home/u/code/proj","worktree":{"repo":"proj","checkout":"/home/u/code/proj","linked":false,"branch":"main"}},
 {"id":"laharl/w3:p1","host":"laharl","pane_id":"w3:p1","name":"fix-a","agent":"pi","agent_status":"idle","cwd":"/home/u/.worktrees/proj/fix-a"},
 {"id":"kube/w3:p1","host":"kube","pane_id":"w3:p1","name":"fix-a","agent":"claude","agent_status":"blocked","cwd":"/home/k/code/other"},
 {"id":"kube/w5:p2","host":"kube","pane_id":"w5:p2","name":"solo","agent":"codex","agent_status":"idle","cwd":"/home/k/code/other"}
],"hosts":[{"name":"laharl","local":true,"status":"ok"},{"name":"kube","status":"ok"},{"name":"bee","status":"down","error":"unreachable"}]}`

const ledgerJSON = `{"entries":[
 {"id":"h1","host":"laharl","repo":"/home/u/code/proj","repo_name":"proj","branch":"fix/a","base":"main","agent_id":"laharl/w3:p1","pane_id":"w3:p1","agent_name":"fix-a","worktree":"/home/u/.worktrees/proj/fix-a","status":"finished","final_report":"## Summary\nFixed A.","live":{"present":true,"agent_status":"idle"},"candidate":true},
 {"id":"h2","host":"kube","repo":"/home/k/code/other","repo_name":"other","branch":"feat/b","agent_id":"kube/w9:p9","agent_name":"feat-b","status":"running","live":{"present":false}},
 {"id":"h3","host":"kube","repo":"/home/k/code/other","repo_name":"other","branch":"feat/c","status":"failed","error":"bad_base: no such base"}
],"marks":[{"agent_id":"kube/w5:p2","name":"solo","by":"me","reason":"stale","at":"2026-10-22T08:00:00Z"}],
"roles":{"laharl/w1:p1":{"role":"coordinator","name":"coordinator","at":"2026-10-22T08:00:00Z"}}}`

func newFake(t *testing.T) (*fakeComebye, *Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	tok, err := agenttoken.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeComebye{t: t, token: tok, answers: map[string]answer{
		"GET /api/agents": {body: agentsJSON},
		"GET /api/whoami": {body: `{"host":"laharl","role":"agent","agent":"laharl/w1:p1","by":"coordinator (laharl/w1:p1)","hub":"self","ledger":true}`},
		"GET /api/ledger": {body: ledgerJSON},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	env := map[string]string{"COMEBYE_URL": srv.URL, "COMEBYE_STATE_DIR": dir, "HERDR_PANE_ID": "w1:p1"}
	var out, errb bytes.Buffer
	e := &Env{
		Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""),
		Getenv:   func(k string) string { return env[k] },
		Hostname: func() (string, error) { return "laharl.local", nil },
		Now:      func() time.Time { return time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC) },
	}
	return f, e, &out, &errb
}

func run(e *Env, args ...string) int { return Run(args, e) }

func newFS() (*flag.FlagSet, *int) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	return fs, fs.Int("lines", 0, "")
}

func TestParseInterspersed(t *testing.T) {
	for _, args := range [][]string{
		{"x", "--lines", "5"}, {"--lines", "5", "x"}, {"--lines=5", "x"},
	} {
		fs, lines := newFS()
		pos, err := parse(fs, args)
		if err != nil || len(pos) != 1 || pos[0] != "x" || *lines != 5 {
			t.Errorf("%v: %v %v %d", args, pos, err, *lines)
		}
	}
	fs, _ := newFS()
	if pos, err := parse(fs, []string{"a", "-", "--", "--lines", "b"}); err != nil || strings.Join(pos, "|") != "a|-|--lines|b" {
		t.Errorf("-- and -: %v %v", pos, err)
	}
	fs, _ = newFS()
	if _, err := parse(fs, []string{"--nope"}); err == nil {
		t.Error("unknown flag accepted")
	}
}

func TestUsageAndExitCodes(t *testing.T) {
	_, e, out, errb := newFake(t)
	if run(e, "help") != 0 || !strings.Contains(out.String(), "comebye handoff --host H") {
		t.Fatalf("help: %s", out)
	}
	for _, args := range [][]string{
		{"read"}, {"read", "a", "b"}, {"read", "x", "--lines", "0"}, {"mark", "home"}, {"handoff", "--host", "laharl"},
		{"handoff", "--host", "h", "--repo", "rel", "--branch", "b", "--brief", "-"}, {"prompt", "home"},
		{"brief"}, {"brief", "frob"}, {"playbook", "--harness", "emacs"}, {"presets"}, {"agents", "extra"}, {"nosuch"},
	} {
		errb.Reset()
		if code := run(e, args...); code != 2 {
			t.Errorf("%v: exit %d (%s), want 2", args, code, errb)
		}
	}
	if !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("unknown command message: %s", errb)
	}
	out.Reset()
	if code := run(e, "read", "-h"); code != 0 || !strings.Contains(out.String(), "usage: comebye read AGENT") {
		t.Errorf("read -h: %d %s", code, out)
	}
}

func TestResolve(t *testing.T) {
	_, e, _, _ := newFake(t)
	c, err := newClient(e)
	if err != nil {
		t.Fatal(err)
	}
	for arg, want := range map[string]string{
		"kube/w5:p2":   "kube/w5:p2",   // host-qualified
		"laharl/w1:p1": "laharl/w1:p1", //
		"w2:p1":        "laharl/w2:p1", // bare pane: the local host
		"w3:p1":        "laharl/w3:p1", // the local one, though kube has one too
		"solo":         "kube/w5:p2",   // a unique name on another host
		"home":         "laharl/w2:p1",
		"kube/gone:p1": "kube/gone:p1", // not running: passed through
	} {
		got, err := c.resolve(arg)
		if err != nil || got.ID != want {
			t.Errorf("resolve(%q) = %+v, %v; want %s", arg, got, err, want)
		}
		if got.Host == "" || got.Pane == "" {
			t.Errorf("resolve(%q): host/pane not split: %+v", arg, got)
		}
	}
	if _, err := c.resolve("fix-a"); err == nil || !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), "laharl/w3:p1") || !strings.Contains(err.Error(), "kube/w3:p1") {
		t.Errorf("ambiguous name: %v", err)
	}
	if _, err := c.resolve("w5:p2"); err == nil || !strings.Contains(err.Error(), "did you mean kube/w5:p2") {
		t.Errorf("pane on another host: %v", err)
	}
	if _, err := c.resolve("nobody"); err == nil || !strings.Contains(err.Error(), "comebye agents") {
		t.Errorf("unknown: %v", err)
	}
	if _, err := c.resolve("/x"); err == nil {
		t.Error("bad id accepted")
	}
}

func TestAgentsCommand(t *testing.T) {
	f, e, out, _ := newFake(t)
	if code := run(e, "agents"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	s := out.String()
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if !strings.HasPrefix(lines[0], "AGENT") || !strings.HasPrefix(lines[1], "laharl/w1:p1") ||
		!strings.Contains(lines[1], "coordinator (you)") {
		t.Fatalf("coordinator first, marked as you:\n%s", s)
	}
	for _, want := range []string{"proj", "fix-a", "worker (finished)", "home", "main", "bee is down (unreachable)"} {
		if !strings.Contains(s, want) {
			t.Errorf("agents lacks %q:\n%s", want, s)
		}
	}
	r := f.find("GET", "/api/agents")
	if r.hdr.Get(headerToken) != f.token || r.hdr.Get(headerAgent) != "w1:p1" || r.hdr.Get(headerAgentHost) != "laharl.local" {
		t.Fatalf("headers = %v", r.hdr)
	}
	out.Reset()
	run(e, "agents", "--json", "--host", "kube")
	var v struct {
		Agents []agentRow `json:"agents"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || len(v.Agents) != 2 || v.Agents[0].Host != "kube" {
		t.Fatalf("json: %v %s", err, out)
	}
}

func TestLedgerAndRead(t *testing.T) {
	f, e, out, errb := newFake(t)
	if code := run(e, "ledger"); code != 0 {
		t.Fatalf("exit %d %s", code, errb)
	}
	s := out.String()
	for _, want := range []string{"h1", "finished", "candidate", "agent gone", "error: bad_base", "Other sessions marked", "kube/w5:p2", "stale"} {
		if !strings.Contains(s, want) {
			t.Errorf("ledger lacks %q:\n%s", want, s)
		}
	}
	out.Reset()
	run(e, "ledger", "--active")
	if strings.Contains(out.String(), "h3") {
		t.Errorf("--active shows failed:\n%s", out)
	}

	// --report: by agent name, entry id or branch.
	for _, arg := range []string{"laharl/w3:p1", "h1", "fix/a"} {
		out.Reset()
		if code := run(e, "read", arg, "--report"); code != 0 || !strings.Contains(out.String(), "Fixed A.") {
			t.Errorf("report %s: %d %s %s", arg, code, out, errb)
		}
	}
	errb.Reset()
	if code := run(e, "read", "--report", "h2"); code != 1 || !strings.Contains(errb.String(), "no final report yet") {
		t.Errorf("no report: %d %s", code, errb)
	}

	f.answers["GET /api/hosts/laharl/agents/w2:p1/transcript"] = answer{body: `{"messages":[
		{"id":"1","kind":"user","text":"do it","time":"2026-10-22T08:00:00Z"},
		{"id":"2","kind":"tool","tool":"bash","args":"go test ./...","result":"ok  pkg\nmore","ms":1200},
		{"id":"3","kind":"assistant","text":"Done."}]}`}
	out.Reset()
	if code := run(e, "read", "home", "--lines", "3"); code != 0 {
		t.Fatalf("read: %d %s", code, errb)
	}
	if r := f.find("GET", "/api/hosts/laharl/agents/w2:p1/transcript"); r.query != "limit=3" {
		t.Errorf("query %q", r.query)
	}
	for _, want := range []string{"home (laharl/w2:p1) · working", "── user", "do it", "· bash go test ./... → ok: ok  pkg", "── assistant", "Done."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("read lacks %q:\n%s", want, out)
		}
	}
	// --report for an agent that is not a ledger worker: its last reply.
	out.Reset()
	if code := run(e, "read", "home", "--report"); code != 0 || !strings.Contains(out.String(), "not a worker in the ledger; its last reply") || !strings.Contains(out.String(), "Done.") {
		t.Errorf("last reply: %d %s %s", code, out, errb)
	}
	// No session: the terminal's output.
	f.answers["GET /api/hosts/kube/agents/w5:p2/transcript"] = answer{code: 404, body: `{"code":"no_session","message":"none"}`}
	f.answers["GET /api/hosts/kube/agents/w5:p2/output"] = answer{body: `{"text":"$ codex\nworking..."}`}
	out.Reset()
	if code := run(e, "read", "solo"); code != 0 || !strings.Contains(out.String(), "recent terminal output") || !strings.Contains(out.String(), "working...") {
		t.Errorf("fallback: %d %s", code, out)
	}
}

func TestHandoffCommand(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/handoffs"] = answer{body: `{"entry":{"id":"h9","host":"kube","repo_name":"other","branch":"feat/x","base":"main","agent_id":"kube/w7:p1","agent_name":"feat-x","worktree":"/home/k/.worktrees/other/feat-x","status":"running"}}`}
	brief := filepath.Join(t.TempDir(), "brief.md")
	os.WriteFile(brief, []byte("## Goal\nDo X."), 0o600)
	code := run(e, "handoff", "--host", "kube", "--repo", "/home/k/code/other", "--branch", "feat/x", "--kind", "claude", "--brief", brief)
	if code != 0 || !strings.Contains(out.String(), "Handed off feat/x on kube to feat-x (kube/w7:p1)") {
		t.Fatalf("handoff: %d %s %s", code, out, errb)
	}
	var body map[string]string
	json.Unmarshal([]byte(f.find("POST", "/api/handoffs").body), &body)
	if body["host"] != "kube" || body["repo"] != "/home/k/code/other" || body["branch"] != "feat/x" || body["kind"] != "claude" ||
		body["brief"] != "## Goal\nDo X." || body["base"] != "" {
		t.Fatalf("body = %v", body)
	}
	// From stdin; an existing worker is reported as such.
	f.answers["POST /api/handoffs"] = answer{body: `{"entry":{"id":"h9","host":"kube","branch":"feat/x","agent_id":"kube/w7:p1","agent_name":"feat-x","status":"finished"},"existing":true}`}
	e.Stdin = strings.NewReader("brief from stdin")
	out.Reset()
	if code := run(e, "handoff", "--host", "kube", "--repo", "/r", "--branch", "feat/x", "--brief", "-"); code != 0 || !strings.Contains(out.String(), "Already handed off") {
		t.Fatalf("existing: %d %s", code, out)
	}
	json.Unmarshal([]byte(f.find("POST", "/api/handoffs").body), &body)
	if body["brief"] != "brief from stdin" {
		t.Fatalf("stdin brief = %q", body["brief"])
	}
	// A refused handoff: exit 1 with the server's message.
	f.answers["POST /api/handoffs"] = answer{code: 400, body: `{"code":"bad_base","message":"no such base branch: nope","entry":{}}`}
	errb.Reset()
	if code := run(e, "handoff", "--host", "kube", "--repo", "/r", "--branch", "b", "--brief", brief); code != 1 || !strings.Contains(errb.String(), "no such base branch: nope") {
		t.Fatalf("refused: %d %s", code, errb)
	}
}

func TestPromptMarkBrief(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/hosts/kube/agents/w5:p2/prompt"] = answer{body: `{"agent":{}}`}
	if code := run(e, "prompt", "solo", "please", "add", "tests"); code != 0 {
		t.Fatalf("prompt: %d %s", code, errb)
	}
	if b := f.find("POST", "/api/hosts/kube/agents/w5:p2/prompt").body; b != `{"text":"please add tests"}` {
		t.Fatalf("prompt body %s", b)
	}
	errb.Reset()
	if code := run(e, "prompt", "coordinator", "hi"); code != 1 || !strings.Contains(errb.String(), "yourself") {
		t.Fatalf("self prompt: %d %s", code, errb)
	}

	// A TEXT that is an existing file is refused (the agent would get the
	// path); stdin sends the file's contents.
	file := filepath.Join(t.TempDir(), "reboot-prompt.md")
	if err := os.WriteFile(file, []byte("resume work"), 0o600); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	n := len(f.reqs)
	if code := run(e, "prompt", "solo", file); code == 0 || !strings.Contains(errb.String(), "is a file") || !strings.Contains(errb.String(), "- <") {
		t.Fatalf("file text: %d %s", code, errb)
	}
	if len(f.reqs) != n {
		t.Fatalf("a refused prompt reached the hub: %v", f.reqs[n:])
	}

	f.answers["POST /api/marks"] = answer{body: `{"mark":{}}`}
	if code := run(e, "mark", "w2:p1", "--reason", "merged"); code != 0 {
		t.Fatalf("mark: %d %s", code, errb)
	}
	if b := f.find("POST", "/api/marks").body; b != `{"agent_id":"laharl/w2:p1","discard":false,"no_ask":false,"reason":"merged"}` {
		t.Fatalf("mark body %s", b)
	}
	out.Reset()
	if code := run(e, "mark", "w2:p1", "--reason", "superseded", "--discard"); code != 0 || !strings.Contains(out.String(), "Proposed discarding home") {
		t.Fatalf("mark --discard: %d %s %s", code, out, errb)
	}
	if b := f.find("POST", "/api/marks").body; b != `{"agent_id":"laharl/w2:p1","discard":true,"no_ask":false,"reason":"superseded"}` {
		t.Fatalf("discard body %s", b)
	}
	f.answers["POST /api/marks/clear"] = answer{body: `{"cleared":true}`}
	out.Reset()
	if code := run(e, "unmark", "laharl/w2:p1"); code != 0 || !strings.Contains(out.String(), "Unmarked home") {
		t.Fatalf("unmark: %d %s", code, out)
	}
	// The server's refusals reach the user.
	f.answers["POST /api/marks"] = answer{code: 409, body: `{"code":"coordinator","message":"agents cannot mark a coordinator; only the user can remove one"}`}
	errb.Reset()
	if code := run(e, "mark", "coordinator", "--reason", "x"); code != 1 || !strings.Contains(errb.String(), "cannot mark a coordinator") {
		t.Fatalf("refused mark: %d %s", code, errb)
	}
	f.answers["GET /api/removals"] = answer{code: 403, body: `{"code":"agent_forbidden","message":"left to the user"}`}

	f.answers["POST /api/brief"] = answer{body: `{"brief":{}}`}
	e.Stdin = strings.NewReader("## proj\n**Finished:** x")
	if code := run(e, "brief", "set", "-"); code != 0 {
		t.Fatalf("brief set: %d %s", code, errb)
	}
	if b := f.find("POST", "/api/brief").body; b != `{"markdown":"## proj\n**Finished:** x"}` {
		t.Fatalf("brief body %s", b)
	}
	f.answers["GET /api/brief"] = answer{body: `{"brief":{"markdown":"## proj","by":"coordinator (laharl/w1:p1)","at":"2026-10-22T08:00:00Z"}}`}
	out.Reset()
	if code := run(e, "brief", "get"); code != 0 || !strings.Contains(out.String(), "Briefing from coordinator (laharl/w1:p1), 1h ago") {
		t.Fatalf("brief get: %d %s", code, out)
	}
	f.answers["GET /api/brief"] = answer{body: `{"brief":null}`}
	out.Reset()
	run(e, "brief", "get")
	if !strings.Contains(out.String(), "no briefing") {
		t.Fatalf("no brief: %s", out)
	}
}

func TestBriefSchedule(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["GET /api/brief/schedule"] = answer{body: `{"schedule":{"enabled":false,"state":"off"}}`}
	if code := run(e, "brief", "schedule"); code != 0 || !strings.Contains(out.String(), "Scheduled briefings are off.") {
		t.Fatalf("show off: %d %s %s", code, out, errb)
	}
	// The first time needs --every; flags belong to schedule.
	if code := run(e, "brief", "schedule", "--at", "08:00"); code != 2 || !strings.Contains(errb.String(), "--every is required") {
		t.Fatalf("no every: %d %s", code, errb)
	}
	if code := run(e, "brief", "get", "--off"); code != 2 {
		t.Fatalf("get --off: %d", code)
	}
	f.answers["POST /api/brief/schedule"] = answer{body: `{"schedule":{}}`}
	if code := run(e, "brief", "schedule", "--every", "24h", "--at", "08:00", "--tz", "UTC"); code != 0 {
		t.Fatalf("set: %d %s", code, errb)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.find("POST", "/api/brief/schedule").body), &body)
	if body["enabled"] != true || body["every"] != "24h" || body["at"] != "08:00" || body["tz"] != "UTC" {
		t.Fatalf("set body = %v", body)
	}
	f.answers["GET /api/brief/schedule"] = answer{body: `{"schedule":{"enabled":true,"every":"24h","at":"08:00","tz":"UTC","by":"me@example.com","updated":"2026-10-22T08:00:00Z",
		"state":"ok","next":"2026-10-23T08:00:00Z","missed":{"id":"2026-10-22T08:00:00Z","since":"2026-10-22T08:00:00Z","count":2,"reason":"no_coordinator","dismissed":false}}}`}
	out.Reset()
	if code := run(e, "brief", "schedule"); code != 0 ||
		!strings.Contains(out.String(), "every 24h at 08:00 UTC, set by me@example.com 1h ago") ||
		!strings.Contains(out.String(), "Next: Fri Oct 23 08:00 UTC") ||
		!strings.Contains(out.String(), "Missed 2 scheduled briefing(s) since Thu Oct 22 08:00 UTC: no coordinator was running") {
		t.Fatalf("show: %d %s", code, out)
	}
	f.answers["POST /api/brief/schedule/dismiss"] = answer{body: `{"schedule":{}}`}
	if code := run(e, "brief", "schedule", "--dismiss"); code != 0 || f.find("POST", "/api/brief/schedule/dismiss").body != `{"id":"2026-10-22T08:00:00Z"}` {
		t.Fatalf("dismiss: %d %s", code, errb)
	}
}

func TestPlaybookPresetsWhoami(t *testing.T) {
	_, e, out, errb := newFake(t)
	for h, want := range map[string]string{"pi": "name: coordinator", "claude": "(Claude Code)", "generic": "# Comebye coordinator playbook"} {
		out.Reset()
		if code := run(e, "playbook", "--harness", h); code != 0 || !strings.Contains(out.String(), want) {
			t.Errorf("playbook %s: %d", h, code)
		}
	}
	out.Reset()
	errb.Reset()
	if code := run(e, "presets", "claude"); code != 0 || !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "Bash(comebye:*)") || !strings.Contains(errb.String(), "settings.json") {
		t.Errorf("presets claude: %d %s %s", code, out, errb)
	}
	out.Reset()
	if code := run(e, "presets", "pi"); code != 0 || !strings.Contains(out.String(), "permission-gate") {
		t.Errorf("presets pi: %d", code)
	}
	out.Reset()
	if code := run(e, "whoami"); code != 0 || !strings.Contains(out.String(), "You are coordinator (laharl/w1:p1), with the agent role, on host laharl") ||
		!strings.Contains(out.String(), "is the hub") {
		t.Errorf("whoami: %d %s", code, out)
	}
}

func TestTransportErrors(t *testing.T) {
	f, e, _, errb := newFake(t)
	// No token file.
	env := map[string]string{"COMEBYE_STATE_DIR": t.TempDir(), "COMEBYE_URL": "http://127.0.0.1:1"}
	e.Getenv = func(k string) string { return env[k] }
	if code := run(e, "agents"); code != 1 || !strings.Contains(errb.String(), "no agent token at") {
		t.Fatalf("no token: %d %s", code, errb)
	}
	// Token, but nothing listening.
	agenttoken.LoadOrCreate(env["COMEBYE_STATE_DIR"])
	errb.Reset()
	if code := run(e, "agents"); code != 1 || !strings.Contains(errb.String(), "cannot reach Comebye at http://127.0.0.1:1") {
		t.Fatalf("unreachable: %d %s", code, errb)
	}
	// A server that does not know this token.
	f.token = "something else"
	srv := httptest.NewServer(f)
	defer srv.Close()
	env["COMEBYE_URL"] = srv.URL
	errb.Reset()
	if code := run(e, "agents"); code != 1 || !strings.Contains(errb.String(), "did not accept the agent token") {
		t.Fatalf("wrong token: %d %s", code, errb)
	}
	env["COMEBYE_URL"] = "ftp://x"
	errb.Reset()
	if code := run(e, "agents"); code != 1 || !strings.Contains(errb.String(), "COMEBYE_URL") {
		t.Fatalf("bad url: %d %s", code, errb)
	}
	// Forbidden for agents.
	f3, e3, _, errb3 := newFake(t)
	f3.answers["GET /api/agents"] = answer{code: 403, body: `{"code":"agent_forbidden","message":"left to the user"}`}
	if code := run(e3, "agents"); code != 1 || !strings.Contains(errb3.String(), "not allowed for agents: left to the user") {
		t.Fatalf("forbidden: %d %s", code, errb3)
	}
}

func TestStateDir(t *testing.T) {
	env := map[string]string{"HOME": "/home/u"}
	get := func(k string) string { return env[k] }
	if d := StateDir(get); d != "/home/u/.local/state/comebye" {
		t.Errorf("default %s", d)
	}
	env["XDG_STATE_HOME"] = "/x/state"
	if d := StateDir(get); d != "/x/state/comebye" {
		t.Errorf("xdg %s", d)
	}
	env["COMEBYE_STATE_DIR"] = "~/s"
	if d := StateDir(get); d != "/home/u/s" {
		t.Errorf("explicit %s", d)
	}
}

// The answer to "is it safe to remove you?": printed when known, waited
// for with --wait, and shown in the ledger.
func TestMarkAsk(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["POST /api/marks"] = answer{body: `{"mark":{"agent_id":"kube/w5:p2","at":"2026-10-22T09:00:00Z","ask":{"state":"asking"}}}`}
	if code := run(e, "mark", "solo", "--reason", "merged"); code != 0 || !strings.Contains(out.String(), "Asked the session whether it is safe") {
		t.Fatalf("mark: %d %s %s", code, out, errb)
	}
	asking := `{"entries":[],"marks":[{"agent_id":"kube/w5:p2","at":"2026-10-22T09:00:00Z","ask":{"state":"asking"}}]}`
	f.answers["GET /api/ledger"] = answer{body: asking}
	sleeps := 0
	e.Sleep = func(time.Duration) {
		sleeps++
		f.mu.Lock()
		f.answers["GET /api/ledger"] = answer{body: strings.Replace(asking, `{"state":"asking"}`, `{"state":"unsafe","reason":"watching PR #12"}`, 1)}
		f.mu.Unlock()
	}
	out.Reset()
	if code := run(e, "mark", "solo", "--reason", "merged", "--wait"); code != 0 || sleeps != 1 ||
		!strings.Contains(out.String(), "The session says UNSAFE: watching PR #12.") {
		t.Fatalf("mark --wait: %d %d %s %s", code, sleeps, out, errb)
	}
	out.Reset()
	if code := run(e, "mark", "solo", "--reason", "merged", "--wait", "--json"); code != 0 || !strings.Contains(out.String(), `"state": "unsafe"`) {
		t.Fatalf("mark --wait --json: %d %s", code, out)
	}
	if code := run(e, "mark", "solo", "--reason", "merged", "--no-ask"); code != 0 || !strings.Contains(f.find("POST", "/api/marks").body, `"no_ask":true`) {
		t.Fatalf("mark --no-ask: %d %s", code, errb)
	}
	out.Reset()
	if code := run(e, "ledger"); code != 0 || !strings.Contains(out.String(), "the session says UNSAFE: watching PR #12") {
		t.Fatalf("ledger: %d %s", code, out)
	}
	// A mark replaced meanwhile ends the wait.
	f.answers["GET /api/ledger"] = answer{body: strings.Replace(asking, "09:00:00", "09:00:01", 1)}
	errb.Reset()
	if code := run(e, "mark", "solo", "--reason", "merged", "--wait"); code != 1 || !strings.Contains(errb.String(), "replaced") {
		t.Fatalf("replaced: %d %s", code, errb)
	}
}

// Pushed is not merged: inspect says which.
func TestInspectPushedNotMerged(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["GET /api/inspect"] = answer{body: `{"agent_id":"kube/w5:p2","host":"kube","name":"solo","status":"idle","checks":{
		"path":"/w","exists":true,"branch":"fix","unmerged_commits":2,"base_used":"origin/main","tip":"abc",
		"detail":{"status":[],"commits":[],"upstream":"origin/fix","unpushed":0}}}`}
	if code := run(e, "inspect", "solo"); code != 0 {
		t.Fatalf("inspect: %d %s", code, errb)
	}
	for _, want := range []string{"merged:   NO · 2 commits not in origin/main", "a squash-merged PR whose patch differs still counts",
		"pushed:   upstream origin/fix · every unmerged commit is on a remote, but pushed is not merged"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("inspect lacks %q:\n%s", want, out)
		}
	}
	f.answers["GET /api/inspect"] = answer{body: strings.Replace(f.answers["GET /api/inspect"].body, `"unmerged_commits":2`, `"unmerged_commits":0`, 1)}
	out.Reset()
	if code := run(e, "inspect", "solo"); code != 0 || !strings.Contains(out.String(), "merged:   yes · every commit is in origin/main") ||
		!strings.Contains(out.String(), "nothing only here") {
		t.Fatalf("merged: %d %s", code, out)
	}
}

func TestInspectCommand(t *testing.T) {
	f, e, out, errb := newFake(t)
	f.answers["GET /api/inspect"] = answer{body: `{"agent_id":"kube/w5:p2","host":"kube","name":"solo","status":"idle","checks":{
		"path":"/home/k/.worktrees/other/fix","exists":true,"primary":false,"branch":"fix","dirty":true,"unmerged_commits":1,"base_used":"main",
		"tip":"5873ecd0123456789","in_progress":"rebase",
		"detail":{"status":["M  README.md","?? new.ts"],"commits":[{"sha":"5873ecd01234","subject":"herdr: workspace spawn"}],
		"diff_stat":" README.md | 2 +-","upstream":"","unpushed":1,"diff":"diff --git a/README.md b/README.md\n+x\n"}}}`}
	if code := run(e, "inspect", "solo", "--diff"); code != 0 {
		t.Fatalf("inspect: %d %s", code, errb)
	}
	if r := f.find("GET", "/api/inspect"); r.query != "agent=kube%2Fw5%3Ap2&diff=1" {
		t.Fatalf("query %q", r.query)
	}
	for _, want := range []string{"solo (kube/w5:p2) · idle · on kube", "worktree: /home/k/.worktrees/other/fix", "tip 5873ecd01234",
		"a rebase is in progress", "merged:   NO · 1 commits not in main", "no upstream · 1 commits on no remote (only here)",
		"5873ecd01234 herdr: workspace spawn", "?? new.ts", "── diff: the branch against its base", "+x"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("inspect lacks %q:\n%s", want, out)
		}
	}
}

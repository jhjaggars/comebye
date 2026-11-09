package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// panelHome is a HOME with a worktree under a dot-directory (the way
// ~/.worktrees is), on a feature branch one commit past main, holding every
// kind of change the Diff panel shows.
func panelHome(t *testing.T) (home, wt string) {
	t.Helper()
	home, _ = filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	wt = filepath.Join(home, ".worktrees", "proj", "feat")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "init", "-q", "-b", "main")
	put(t, filepath.Join(wt, "a.go"), "package a\n\nfunc A() {}\n")
	put(t, filepath.Join(wt, "b.go"), "package b\n")
	put(t, filepath.Join(wt, "README.md"), "# proj\n\n## Plan\n\nText.\n\n## Rollout\n\nMore.\n")
	gitIn(t, wt, "add", ".")
	gitIn(t, wt, "commit", "-q", "-m", "base")
	gitIn(t, wt, "checkout", "-q", "-b", "feat")
	put(t, filepath.Join(wt, "c.go"), "package c\n")
	gitIn(t, wt, "add", "c.go")
	gitIn(t, wt, "commit", "-q", "-m", "add c")
	put(t, filepath.Join(wt, "a.go"), "package a\n\nfunc A() { println(1) }\n") // unstaged
	put(t, filepath.Join(wt, "b.go"), "package b\n\nvar B = 1\n")               // staged
	gitIn(t, wt, "add", "b.go")
	put(t, filepath.Join(wt, "new.txt"), "one\ntwo\n") // untracked
	return home, wt
}

func panelServer(t *testing.T) *Server {
	t.Helper()
	return New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "desk", StateDir: t.TempDir(), AgentToken: testToken}, nil, nil)
}

func runDiff(t *testing.T, s *Server, dir, since string) panelDiff {
	t.Helper()
	status, out := s.panelRun(context.Background(), "diff", url.Values{"dir": {dir}, "since": {since}})
	if status != 200 {
		t.Fatalf("diff: %d %v", status, out)
	}
	b, _ := json.Marshal(out)
	var d panelDiff
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func sectionFile(d panelDiff, section, path string) *panelFile {
	for _, s := range d.Sections {
		if s.Key != section {
			continue
		}
		for i := range s.Files {
			if s.Files[i].Path == path {
				return &s.Files[i]
			}
		}
	}
	return nil
}

func TestPanelDiffSections(t *testing.T) {
	_, wt := panelHome(t)
	d := runDiff(t, panelServer(t), wt, "")
	if d.Branch != "feat" || d.Base != "main" || len(d.Commits) != 1 || d.Commits[0].Subject != "add c" {
		t.Fatalf("branch/base/commits: %+v", d)
	}
	for _, c := range []struct{ sec, path, status, has string }{
		{"unstaged", "a.go", "modified", "+func A() { println(1) }"},
		{"staged", "b.go", "modified", "+var B = 1"},
		{"untracked", "new.txt", "untracked", "+two"},
		{"commits", "c.go", "added", "+package c"},
	} {
		f := sectionFile(d, c.sec, c.path)
		if f == nil || f.Status != c.status || !strings.Contains(f.Diff, c.has) || !strings.HasPrefix(f.Diff, "@@") {
			t.Errorf("%s %s: %+v", c.sec, c.path, f)
		}
	}
	if f := sectionFile(d, "untracked", "new.txt"); f != nil && (f.Add != 2 || !strings.HasPrefix(f.Diff, "@@ -0,0 +1,2 @@")) {
		t.Errorf("untracked counts: %+v", f)
	}
	if d.Files != 4 || d.Add < 5 {
		t.Errorf("totals: files %d add %d", d.Files, d.Add)
	}
}

func TestPanelDiffUnchangedAndLive(t *testing.T) {
	_, wt := panelHome(t)
	s := panelServer(t)
	d := runDiff(t, s, wt, "")
	if again := runDiff(t, s, wt, d.Sig); !again.Unchanged || again.Sig != d.Sig || len(again.Sections) != 0 {
		t.Fatalf("same state, same sig: %+v", again)
	}
	put(t, filepath.Join(wt, "a.go"), "package a\n\nfunc A() { println(2) }\n")
	if next := runDiff(t, s, wt, d.Sig); next.Unchanged || next.Sig == d.Sig || sectionFile(next, "unstaged", "a.go") == nil {
		t.Fatalf("a changed file changes the sig: %+v", next)
	}
}

func TestPanelDiffCaps(t *testing.T) {
	_, wt := panelHome(t)
	// A tracked file with a huge change, and a huge untracked one.
	big := strings.Repeat("line of text that is long enough\n", 4000)
	put(t, filepath.Join(wt, "b.go"), big)
	put(t, filepath.Join(wt, "huge.txt"), big)
	// Many small untracked files push past the total cap.
	for i := 0; i < 60; i++ {
		put(t, filepath.Join(wt, "many", fmt.Sprintf("f%02d.txt", i)), strings.Repeat("0123456789abcdef\n", 1000))
	}
	d := runDiff(t, panelServer(t), wt, "")
	if f := sectionFile(d, "unstaged", "b.go"); f == nil || !f.TooBig || f.Diff != "" || f.Lines < 4000 {
		t.Errorf("a big tracked diff is a placeholder with its line count: %+v", f)
	}
	if f := sectionFile(d, "untracked", "huge.txt"); f == nil || !f.TooBig || f.Lines != 4000 || f.Diff != "" {
		t.Errorf("a big untracked file is a placeholder: %+v", f)
	}
	text, over := 0, 0
	for _, s := range d.Sections {
		for _, f := range s.Files {
			text += len(f.Diff)
			if f.TooBig {
				over++
			}
		}
	}
	if text > panelTotalCap {
		t.Errorf("diff text %d is over the total cap %d", text, panelTotalCap)
	}
	if d.Note == "" || over < 10 {
		t.Errorf("the total cap leaves placeholders and a note: over %d, note %q", over, d.Note)
	}
}

func TestPanelDiffRedactsAndWithholds(t *testing.T) {
	_, wt := panelHome(t)
	const akia, ghp = "AKIAABCDEFGHIJKLMNOP", "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	put(t, filepath.Join(wt, "cfg.go"), "package cfg\n\nconst key = \""+akia+"\"\nvar tok = \""+ghp+"\"\nvar password = \"hunter2hunter2\"\nvar ok = \"fine\"\n")
	put(t, filepath.Join(wt, "key.txt"), "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n-----END RSA PRIVATE KEY-----\n")
	put(t, filepath.Join(wt, ".env"), "DB_PASSWORD=supersecretvalue\nOTHER=1\n")
	put(t, filepath.Join(wt, "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n-----END OPENSSH PRIVATE KEY-----\n")
	put(t, filepath.Join(wt, "certs", "server.pem"), "-----BEGIN CERTIFICATE-----\nMIIDxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n-----END CERTIFICATE-----\n")
	put(t, filepath.Join(wt, ".env.example"), "DB_PASSWORD=changeme-changeme\n")
	// a tracked secret file, staged
	put(t, filepath.Join(wt, "staged.pem"), "-----BEGIN PRIVATE KEY-----\nMIIBVQIBADANBgkqhkiG9w0BAQEFAASCAT8wggE7AgEAAkEAxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n-----END PRIVATE KEY-----\n")
	gitIn(t, wt, "add", "-f", "staged.pem")

	d := runDiff(t, panelServer(t), wt, "")
	b, _ := json.Marshal(d)
	all := string(b)
	for _, leak := range []string{akia, ghp, "hunter2hunter2", "MIIEowIBAAKC", "supersecretvalue", "b3BlbnNzaC1r", "MIIDxxxx", "MIIBVQIBAD"} {
		if strings.Contains(all, leak) {
			t.Errorf("the answer leaks %q", leak)
		}
	}
	if f := sectionFile(d, "untracked", "cfg.go"); f == nil || f.Redacted < 3 || !strings.Contains(f.Diff, "var ok = \"fine\"") || !strings.Contains(f.Diff, "var password = \""+redactedMark) {
		t.Errorf("secrets replaced, the rest kept: %+v", f)
	}
	if f := sectionFile(d, "untracked", "key.txt"); f == nil || f.Redacted == 0 || strings.Contains(f.Diff, "xxxxxxxx") {
		t.Errorf("a key block is replaced whole: %+v", f)
	}
	for _, c := range []struct{ sec, path string }{{"untracked", ".env"}, {"untracked", "id_ed25519"}, {"untracked", "certs/server.pem"}, {"staged", "staged.pem"}} {
		if f := sectionFile(d, c.sec, c.path); f == nil || f.Withheld == "" || f.Diff != "" || f.Lines == 0 {
			t.Errorf("%s withheld with a line count: %+v", c.path, f)
		}
	}
	if f := sectionFile(d, "untracked", ".env.example"); f == nil || f.Withheld != "" {
		t.Errorf(".env.example is an example, not a secret: %+v", f)
	}
	if d.Redacted == 0 {
		t.Error("redactions are counted")
	}
}

func TestPanelDiffNotGit(t *testing.T) {
	home, _ := panelHome(t)
	plain := filepath.Join(home, "plain")
	os.MkdirAll(plain, 0o755)
	if d := runDiff(t, panelServer(t), plain, ""); !d.NotGit {
		t.Fatalf("a plain directory: %+v", d)
	}
	// Outside home and the roots: refused.
	if st, _ := panelServer(t).panelRun(context.Background(), "diff", url.Values{"dir": {t.TempDir()}}); st != http.StatusNotFound {
		t.Fatalf("outside home: %d", st)
	}
}

func fileOf(t *testing.T, s *Server, dir, pin string, q url.Values) (int, map[string]any) {
	t.Helper()
	q.Set("dir", dir)
	if pin != "" {
		q.Set("pin", pin)
	}
	return s.panelRun(context.Background(), "file", q)
}

func TestPanelFilePathRules(t *testing.T) {
	home, wt := panelHome(t)
	s := panelServer(t)
	outside := filepath.Join(home, "notes", "outside.md") // in home, not in the worktree
	put(t, outside, "# outside\n")
	put(t, filepath.Join(home, "notes", "other.md"), "# other\n")
	put(t, filepath.Join(wt, "docs", "plan.md"), "# Plan\n\n## 1. Goal\n\nx\n")
	put(t, filepath.Join(wt, "docs", "page.html"), "<h1>hi</h1>")
	put(t, filepath.Join(wt, ".github", "x.md"), "# hidden dir\n")
	put(t, filepath.Join(wt, "docs", ".draft.md"), "# hidden file\n")
	put(t, filepath.Join(wt, "big.md"), strings.Repeat("a", panelDocMax+1))
	put(t, filepath.Join(wt, "tool.go"), "package x\n")
	put(t, filepath.Join(wt, "bin.md"), "abc\x00def\xff")
	put(t, filepath.Join(wt, "id_rsa.txt"), "secret\n")
	put(t, filepath.Join(wt, "sub", "link-target.md"), "# in worktree\n")
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.md") // outside home
	put(t, elsewhere, "# elsewhere\n")
	if err := os.Symlink(outside, filepath.Join(wt, "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(wt, "escape2.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "notes"), filepath.Join(wt, "notes-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(wt, "sub", "link-target.md"), filepath.Join(wt, "inside.md")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, path, pin string
		want            int
		code            string
	}{
		{"relative file", "docs/plan.md", "", 200, ""},
		{"absolute file inside", filepath.Join(wt, "docs", "plan.md"), "", 200, ""},
		{"html is read (shown as source for now)", "docs/page.html", "", 200, ""},
		{"a link that stays inside", "inside.md", "", 200, ""},
		{"traversal out of the worktree", "../../../notes/outside.md", "", 403, "outside_allowed"},
		{"traversal with a dot-dot inside the tree", "docs/../docs/plan.md", "", 200, ""},
		{"a file in home outside the worktree", outside, "", 403, "outside_allowed"},
		{"a link out to home", "escape.md", "", 403, "outside_allowed"},
		{"a link out of home", "escape2.md", "", 403, "outside_allowed"},
		{"a directory link out", "notes-link/other.md", "", 403, "outside_allowed"},
		{"outside home altogether", elsewhere, "", 403, "outside_allowed"},
		{"/etc/passwd", "/etc/passwd", "", 403, "outside_allowed"},
		{"a dot-directory", ".github/x.md", "", 403, "dot_dir"},
		{"a dot-file", "docs/.draft.md", "", 403, "dot_dir"},
		{"over 1 MB", "big.md", "", 413, "too_big"},
		{"not an allowed type", "tool.go", "", 415, "bad_type"},
		{"binary under a text name", "bin.md", "", 415, "bad_type"},
		{"a secret by name", "id_rsa.txt", "", 403, "secret_file"},
		{"missing", "docs/none.md", "", 404, "not_found"},
		{"missing outside home reveals nothing", "/nonexistent/x.md", "", 403, "outside_allowed"},
		{"the declared file, outside the worktree", outside, outside, 200, ""},
		{"another file beside the declared one", filepath.Join(home, "notes", "other.md"), outside, 403, "outside_allowed"},
		{"the declared file outside home", elsewhere, elsewhere, 403, "outside_allowed"},
		{"a declared dot-dir file", filepath.Join(home, ".config", "x.md"), filepath.Join(home, ".config", "x.md"), 404, "not_found"},
		{"no path", "", "", 400, "bad_path"},
	}
	put(t, filepath.Join(home, ".config", "y.md"), "# dot\n")
	for _, c := range cases {
		st, out := fileOf(t, s, wt, c.pin, url.Values{"path": {c.path}})
		if st != c.want || (c.code != "" && out["code"] != c.code) {
			t.Errorf("%s: %d %v, want %d %s", c.name, st, out, c.want, c.code)
		}
	}
	// A declared file under a dot-directory of home is refused as such.
	dot := filepath.Join(home, ".config", "y.md")
	if st, out := fileOf(t, s, wt, dot, url.Values{"path": {dot}}); st != 403 || out["code"] != "dot_dir" {
		t.Errorf("declared dot-dir file: %d %v", st, out)
	}
	// The content and the signature come back, and since= answers "unchanged".
	st, out := fileOf(t, s, wt, "", url.Values{"path": {"docs/plan.md"}})
	doc, _ := out["doc"].(map[string]any)
	if st != 200 || doc["kind"] != "markdown" || !strings.Contains(fmt.Sprint(doc["content"]), "## 1. Goal") || out["rel"] != "docs/plan.md" {
		t.Fatalf("read: %d %v", st, out)
	}
	_, again := fileOf(t, s, wt, "", url.Values{"path": {"docs/plan.md"}, "since": {fmt.Sprint(doc["sig"])}})
	if d2, _ := again["doc"].(map[string]any); d2["unchanged"] != true || d2["content"] != nil {
		t.Errorf("unchanged: %v", again)
	}
	// A section the file has, and one it lacks.
	_, found := fileOf(t, s, wt, "", url.Values{"path": {"docs/plan.md"}, "section": {"1. Goal"}, "meta": {"1"}})
	_, lost := fileOf(t, s, wt, "", url.Values{"path": {"docs/plan.md"}, "section": {"Nope"}, "meta": {"1"}})
	f1, _ := found["doc"].(map[string]any)
	f2, _ := lost["doc"].(map[string]any)
	if f1["section_found"] != true || f2["section_found"] != false || f1["content"] != nil {
		t.Errorf("section check: %v / %v", f1, f2)
	}
}

func TestPanelFileList(t *testing.T) {
	_, wt := panelHome(t)
	put(t, filepath.Join(wt, "docs", "plan.md"), "# Plan\n")
	put(t, filepath.Join(wt, ".github", "x.md"), "# x\n")
	put(t, filepath.Join(wt, "notes.txt"), "n\n")
	put(t, filepath.Join(wt, "secret.pem"), "k\n")
	put(t, filepath.Join(wt, "main.go"), "package main\n")
	_, out := fileOf(t, panelServer(t), wt, "", url.Values{"list": {"1"}})
	b, _ := json.Marshal(out["files"])
	got := string(b)
	for _, want := range []string{`"README.md"`, `"docs/plan.md"`, `"notes.txt"`} {
		if !strings.Contains(got, want) {
			t.Errorf("list lacks %s: %s", want, got)
		}
	}
	for _, no := range []string{".github", "secret.pem", "main.go"} {
		if strings.Contains(got, no) {
			t.Errorf("list has %s: %s", no, got)
		}
	}
}

// ---- the routes: auth matrix, show, and a peer ----

type panelRig struct {
	s    *Server
	fh   *fakeLaunchHerdr
	wt   string
	home string
}

func newPanelRig(t *testing.T) *panelRig {
	t.Helper()
	home, wt := panelHome(t)
	fh, client := startFakeLaunchHerdr(t)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = []herdr.AgentInfo{
			{Name: "w", PaneID: "w9:p1", Agent: "pi", Status: herdr.StatusIdle, CWD: wt},
			{Name: "other", PaneID: "w8:p1", Agent: "pi", Status: herdr.StatusIdle, CWD: t.TempDir()},
		}
	})
	m := managerWith(t, client, "desk", 2)
	s := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, HostName: "desk", StateDir: t.TempDir(), AgentToken: testToken}, client, m)
	return &panelRig{s: s, fh: fh, wt: wt, home: home}
}

func (r *panelRig) req(method, path, body string, hdr map[string]string) (int, string) {
	w := do(r.s, method, path, hdr, body)
	return w.Code, w.Body.String()
}

func TestPanelAuthMatrix(t *testing.T) {
	r := newPanelRig(t)
	put(t, filepath.Join(r.wt, "docs", "plan.md"), "# Plan\n")
	routes := []string{
		"/api/agents/w9:p1/diff", "/api/agents/w9:p1/file?path=docs/plan.md", "/api/agents/w9:p1/file?list=1",
		"/api/hosts/desk/agents/w9:p1/diff", "/api/hosts/desk/agents/w9:p1/file?path=docs/plan.md",
	}
	other := "/api/agents/w8:p1/diff"
	for _, p := range routes {
		if st, b := r.req("GET", p, "", userHdr); st != 200 {
			t.Errorf("the user %s: %d %s", p, st, b)
		}
		if st, b := r.req("GET", p, "", agentHdr("w9:p1")); st != 200 {
			t.Errorf("the agent itself %s: %d %s", p, st, b)
		}
		if st, _ := r.req("GET", p, "", agentHdr("w8:p1")); st != 403 {
			t.Errorf("another agent %s: %d", p, st)
		}
		if st, _ := r.req("GET", p, "", agentHdr("")); st != 403 {
			t.Errorf("an agent of unknown pane %s: %d", p, st)
		}
		if st, _ := r.req("GET", p, "", nil); st != 401 {
			t.Errorf("no identity %s: %d", p, st)
		}
		if st, _ := r.req("GET", p, "", map[string]string{HeaderAgentToken: strings.Repeat("f", 64), HeaderAgent: "w9:p1"}); st != 401 {
			t.Errorf("a wrong token %s: %d", p, st)
		}
	}
	if st, _ := r.req("GET", other, "", userHdr); st != 200 && st != 404 {
		t.Errorf("the user reads any agent: %d", st)
	}
	if st, _ := r.req("GET", "/api/agents/nobody/diff", "", userHdr); st != 404 {
		t.Errorf("unknown agent: %d", st)
	}
	// The host side is the hub's call only: not an agent's, not a browser's.
	for _, p := range []string{"/api/panel/diff?dir=" + r.wt, "/api/panel/file?dir=" + r.wt + "&path=docs/plan.md"} {
		if st, _ := r.req("GET", p, "", userHdr); st != 403 {
			t.Errorf("a browser on %s: %d", p, st)
		}
		if st, _ := r.req("GET", p, "", agentHdr("w9:p1")); st != 403 {
			t.Errorf("an agent on %s: %d", p, st)
		}
		if st, b := r.req("GET", p, "", map[string]string{"Tailscale-User-Login": "me@example.com", fleet.HeaderHubCall: "1"}); st != 200 {
			t.Errorf("the hub on %s: %d %s", p, st, b)
		}
	}
	// show: an agent declares its own; another's is refused; the user may.
	body := `{"agent_id":"desk/w9:p1","path":"` + filepath.Join(r.wt, "docs", "plan.md") + `"}`
	if st, b := r.req("POST", "/api/show", body, agentHdr("w8:p1")); st != 403 {
		t.Errorf("show for another agent: %d %s", st, b)
	}
	if st, b := r.req("POST", "/api/show", body, agentHdr("w9:p1")); st != 200 {
		t.Errorf("show for itself: %d %s", st, b)
	}
	if st, _ := r.req("POST", "/api/show", body, nil); st != 401 {
		t.Errorf("show with no identity: %d", st)
	}
	if st, b := r.req("POST", "/api/show", body, userHdr); st != 200 {
		t.Errorf("show by the user: %d %s", st, b)
	}
}

func TestShowPinsClearsAndServesTheDoc(t *testing.T) {
	r := newPanelRig(t)
	plan := filepath.Join(r.wt, "docs", "plan.md")
	put(t, plan, "# Plan\n\n## 1. Goal\n\nx\n\n## 2. Rollout\n\ny\n")
	st, b := r.req("GET", "/api/agents/w9:p1/file", "", userHdr)
	if st != 200 || !strings.Contains(b, `"pin":null`) {
		t.Fatalf("no pin yet: %d %s", st, b)
	}
	st, b = r.req("POST", "/api/show", `{"agent_id":"desk/w9:p1","path":"`+plan+`","section":"2. Rollout"}`, agentHdr("w9:p1"))
	if st != 200 || strings.Contains(b, "warning") {
		t.Fatalf("show: %d %s", st, b)
	}
	st, b = r.req("GET", "/api/agents/w9:p1/file", "", agentHdr("w9:p1"))
	var got struct {
		Pin *docPin        `json:"pin"`
		Doc map[string]any `json:"doc"`
	}
	if err := json.Unmarshal([]byte(b), &got); err != nil || st != 200 || got.Pin == nil || got.Pin.Section != "2. Rollout" || !strings.Contains(fmt.Sprint(got.Doc["content"]), "## 2. Rollout") {
		t.Fatalf("pinned doc: %d %s", st, b)
	}
	// The pin survives a restart of the store.
	if p, ok := openPins(pinsPath(r.s.cfg.StateDir)).get("desk/w9:p1"); !ok || p.Path != plan {
		t.Fatalf("persisted pin: %+v %v", p, ok)
	}
	// A section the file lacks: still pinned, with a warning.
	st, b = r.req("POST", "/api/show", `{"agent_id":"desk/w9:p1","path":"`+plan+`","section":"Missing"}`, agentHdr("w9:p1"))
	if st != 200 || !strings.Contains(b, "warning") {
		t.Fatalf("missing section: %d %s", st, b)
	}
	// Refused files are not pinned.
	for _, p := range []string{filepath.Join(r.wt, "a.go"), "/etc/passwd", filepath.Join(r.wt, "docs", "nope.md")} {
		if st, _ := r.req("POST", "/api/show", `{"agent_id":"desk/w9:p1","path":"`+p+`"}`, agentHdr("w9:p1")); st < 400 {
			t.Errorf("show %s: %d", p, st)
		}
	}
	if pin, _ := r.s.pins.get("desk/w9:p1"); pin.Section != "Missing" {
		t.Errorf("a refused show keeps the old pin: %+v", pin)
	}
	// A path box: the agent's pin does not widen the user's reads.
	if st, _ := r.req("GET", "/api/agents/w9:p1/file?path=../../../x.md", "", userHdr); st != 403 && st != 404 {
		t.Errorf("traversal: %d", st)
	}
	// Clear.
	if st, _ := r.req("POST", "/api/show", `{"agent_id":"desk/w9:p1","clear":true}`, agentHdr("w9:p1")); st != 200 {
		t.Fatalf("clear: %d", st)
	}
	if _, b := r.req("GET", "/api/agents/w9:p1/file", "", userHdr); !strings.Contains(b, `"pin":null`) {
		t.Errorf("cleared: %s", b)
	}
}

// A forged pin or dir in the browser's query is not believed: the hub says
// what the worktree is and what the agent declared.
func TestPanelQueryCannotWidenTheRules(t *testing.T) {
	r := newPanelRig(t)
	secret := filepath.Join(r.home, "notes", "private.md")
	put(t, secret, "# private\n")
	for _, q := range []string{"path=" + secret + "&pin=" + secret, "path=" + secret + "&dir=" + filepath.Dir(secret)} {
		if st, b := r.req("GET", "/api/agents/w9:p1/file?"+q, "", userHdr); st != 403 || strings.Contains(b, "# private") {
			t.Errorf("%s: %d %s", q, st, b)
		}
		if st, b := r.req("GET", "/api/agents/w9:p1/file?"+q, "", agentHdr("w9:p1")); st != 403 || strings.Contains(b, "# private") {
			t.Errorf("agent %s: %d %s", q, st, b)
		}
	}
}

// panelPeer is a peer host: it announces one agent in a worktree and answers
// the hub's panel calls with a real server's host side.
type panelPeer struct {
	host http.Handler
	cwd  string
}

func (p *panelPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/events":
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: agents\ndata: %s\n\n", fmt.Sprintf(`{"type":"agents","version":"v","agents":[{"pane_id":"wS:p1","name":"remote","agent_status":"idle","cwd":%q}]}`, p.cwd))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case strings.HasPrefix(r.URL.Path, "/api/panel/"):
		p.host.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func TestPanelThroughAPeer(t *testing.T) {
	_, wt := panelHome(t)
	put(t, filepath.Join(wt, "docs", "plan.md"), "# Plan\n\n## 1. Goal\n")
	peerSrv := New(Config{ReadLines: 100, HostName: "kube", StateDir: t.TempDir()}, nil, nil)
	pp := &panelPeer{host: peerSrv.Handler(), cwd: wt}
	ps := httptest.NewServer(pp)
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fl.Run(ctx)
	hub := New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, Fleet: fl, AgentToken: testToken, StateDir: t.TempDir()}, nil, nil)
	for deadline := time.Now().Add(5 * time.Second); len(fl.Agents()) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the peer's agent never showed up")
		}
	}
	get := func(path string, hdr map[string]string) (int, string) {
		w := do(hub, "GET", path, hdr, "")
		return w.Code, w.Body.String()
	}
	st, b := get("/api/hosts/kube/agents/wS:p1/diff", userHdr)
	if st != 200 || !strings.Contains(b, `"new.txt"`) || !strings.Contains(b, `"branch":"feat"`) {
		t.Fatalf("diff through the peer: %d %s", st, b)
	}
	// The peer agent itself (forwarded by its host), but not a neighbour.
	if st, _ := get("/api/hosts/kube/agents/wS:p1/diff", forwarded("kube", "wS:p1")); st != 200 {
		t.Errorf("own panel via forward: %d", st)
	}
	if st, _ := get("/api/hosts/kube/agents/wS:p1/diff", forwarded("kube", "wZ:p1")); st != 403 {
		t.Errorf("a neighbour: %d", st)
	}
	// show goes to the peer for the judgement; the doc comes back through it.
	plan := filepath.Join(wt, "docs", "plan.md")
	if st, b := func() (int, string) {
		w := do(hub, "POST", "/api/show", forwarded("kube", "wS:p1"), `{"agent_id":"kube/wS:p1","path":"`+plan+`","section":"1. Goal"}`)
		return w.Code, w.Body.String()
	}(); st != 200 {
		t.Fatalf("show on a peer's agent: %d %s", st, b)
	}
	st, b = get("/api/hosts/kube/agents/wS:p1/file", userHdr)
	if st != 200 || !strings.Contains(b, "## 1. Goal") || !strings.Contains(b, `"section":"1. Goal"`) {
		t.Fatalf("doc through the peer: %d %s", st, b)
	}
	if st, b := get("/api/hosts/kube/agents/wS:p1/file?path=../../../../x.md", userHdr); st != 403 {
		t.Errorf("traversal through the peer: %d %s", st, b)
	}
	if st, _ := get("/api/hosts/kube/agents/wS:p1/file?path=/etc/passwd", userHdr); st != 403 {
		t.Errorf("absolute path through the peer: %d", st)
	}
	// A browser cannot have the hub forward the host-side route.
	if st, _ := get("/api/hosts/kube/panel/diff", userHdr); st != 404 {
		t.Errorf("proxying the host op: %d", st)
	}
}

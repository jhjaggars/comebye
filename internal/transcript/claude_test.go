package transcript

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// Synthetic Claude Code records (shapes observed in real transcripts; no
// real content).
const (
	cMode     = `{"type":"permission-mode","permissionMode":"default","sessionId":"s1"}`
	cSnapshot = `{"type":"file-history-snapshot","messageId":"x","snapshot":{},"isSnapshotUpdate":false}`
	cTitle    = `{"type":"ai-title","aiTitle":"t","sessionId":"s1"}`
	cAttach   = `{"parentUuid":"u1","isSidechain":false,"type":"attachment","attachment":{"type":"date"},"uuid":"at1","timestamp":"2026-10-20T10:00:00Z","sessionId":"s1"}`
	cCaveat   = `{"parentUuid":null,"isSidechain":false,"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>Caveat</local-command-caveat>"},"uuid":"m0","timestamp":"2026-10-20T09:59:00Z","sessionId":"s1"}`
	cUser     = `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"list files"},"uuid":"u1","timestamp":"2026-10-20T10:00:00Z","sessionId":"s1"}`
	cThink    = `{"parentUuid":"u1","isSidechain":false,"type":"assistant","message":{"id":"msg1","model":"claude-test","role":"assistant","content":[{"type":"thinking","thinking":"should run ls","signature":"sig"}]},"uuid":"a1","timestamp":"2026-10-20T10:00:01Z","sessionId":"s1"}`
	cText     = `{"parentUuid":"a1","isSidechain":false,"type":"assistant","message":{"id":"msg1","model":"claude-test","role":"assistant","content":[{"type":"text","text":"Listing."}]},"uuid":"a2","timestamp":"2026-10-20T10:00:02Z","sessionId":"s1"}`
	cTool     = `{"parentUuid":"a2","isSidechain":false,"type":"assistant","message":{"id":"msg1","model":"claude-test","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls -la","description":"list"},"caller":{"type":"direct"}}]},"uuid":"a3","timestamp":"2026-10-20T10:00:02.500Z","sessionId":"s1"}`
	cResult   = `{"parentUuid":"a3","isSidechain":false,"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"file1\nfile2","is_error":false}]},"toolUseResult":{"stdout":"file1\nfile2"},"uuid":"r1","timestamp":"2026-10-20T10:00:04Z","sessionId":"s1"}`
	cSide     = `{"parentUuid":"r1","isSidechain":true,"type":"assistant","message":{"id":"msgS","role":"assistant","content":[{"type":"text","text":"subagent chatter"}]},"uuid":"sc1","timestamp":"2026-10-20T10:00:04Z","sessionId":"s1"}`
	cDur      = `{"parentUuid":"r1","isSidechain":false,"type":"system","subtype":"turn_duration","durationMs":4000,"uuid":"d1","timestamp":"2026-10-20T10:00:05Z","isMeta":false}`
	cFinal    = `{"parentUuid":"r1","isSidechain":false,"type":"assistant","message":{"id":"msg2","model":"claude-test","role":"assistant","content":[{"type":"text","text":"**Done.** Two files."}]},"uuid":"a4","timestamp":"2026-10-20T10:00:05Z","sessionId":"s1"}`
)

func TestSniffFormat(t *testing.T) {
	for _, tc := range []struct {
		lines []string
		want  string
	}{
		{[]string{`{"type":"session","version":3,"id":"s1"}`}, FormatPi},
		{[]string{cMode, cUser}, FormatClaude},
		{[]string{cSnapshot}, FormatClaude},
		{[]string{`{"type":"summary","summary":"old title","leafUuid":"x"}`}, FormatClaude},
		{nil, FormatPi},
	} {
		f, err := os.Open(writeSession(t, tc.lines...))
		if err != nil {
			t.Fatal(err)
		}
		if got := sniffFormat(f); got != tc.want {
			t.Errorf("%v: got %s want %s", tc.lines, got, tc.want)
		}
		f.Close()
	}
}

func TestClaudeReadBasics(t *testing.T) {
	path := writeSession(t, cMode, cSnapshot, cTitle, cCaveat, cUser, cAttach, cThink, cText, cTool, cResult, cSide, cDur, cFinal)
	tr, err := Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Format != FormatClaude {
		t.Fatalf("format %q", tr.Format)
	}
	var kinds []string
	for _, m := range tr.Messages {
		kinds = append(kinds, m.Kind)
	}
	if got := strings.Join(kinds, ","); got != "user,assistant,tool,assistant" {
		t.Fatalf("kinds %s", got)
	}
	u, a, tool, fin := tr.Messages[0], tr.Messages[1], tr.Messages[2], tr.Messages[3]
	if u.Text != "list files" || u.ID != "u1" || u.Time == "" {
		t.Errorf("user %+v", u)
	}
	if a.Text != "Listing." || a.Think != "should run ls" || a.Model != "claude-test" {
		t.Errorf("assistant %+v", a)
	}
	if tool.ID != "toolu_1" || tool.Tool != "Bash" || tool.Args != "ls -la" || tool.Result != "file1\nfile2" || tool.Err || tool.Ms != 1500 {
		t.Errorf("tool %+v", tool)
	}
	if fin.Think != "" || !strings.Contains(fin.Text, "Two files") {
		t.Errorf("final %+v", fin)
	}
	if tr.Ask != nil || tr.Total != 4 || tr.More {
		t.Errorf("ask=%v total=%d more=%v", tr.Ask, tr.Total, tr.More)
	}
}

func TestClaudeUserVariantsAndSystem(t *testing.T) {
	lines := []string{
		cMode,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":"<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>opus</command-args>"},"uuid":"c1","timestamp":"t"}`,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":"<local-command-stdout>Set model to \u001b[1mopus\u001b[22m</local-command-stdout>"},"uuid":"c2","timestamp":"t"}`,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":"<local-command-stdout></local-command-stdout>"},"uuid":"c3","timestamp":"t"}`,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":"<bash-input>git status</bash-input>"},"uuid":"b1","timestamp":"t"}`,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":"<bash-stdout>clean</bash-stdout><bash-stderr></bash-stderr>"},"uuid":"b2","timestamp":"t"}`,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":"<task-notification>\n<task-id>x</task-id>\n<status>completed</status>\n<summary>Background build finished</summary>\n</task-notification>"},"uuid":"tn","timestamp":"t"}`,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]},"uuid":"i1","timestamp":"t"}`,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"AAAA"}},{"type":"text","text":"what is this?"}]},"uuid":"im1","timestamp":"t"}`,
		`{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"AAAA"}}]},"uuid":"im2","timestamp":"t"}`,
		`{"type":"user","isSidechain":false,"isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"injected skill body"}]},"uuid":"meta","timestamp":"t"}`,
		`{"type":"system","subtype":"away_summary","content":"While you were away: tests pass.","isSidechain":false,"uuid":"aw","timestamp":"t"}`,
		`{"type":"system","subtype":"bridge_status","content":"connected","isSidechain":false,"uuid":"bs","timestamp":"t"}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>Compacted</local-command-stdout>","isSidechain":false,"uuid":"lc","timestamp":"t"}`,
		`{"type":"assistant","isSidechain":false,"isApiErrorMessage":true,"message":{"id":"e1","model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"API Error: overloaded"}]},"uuid":"err","timestamp":"t"}`,
	}
	tr, err := Read(writeSession(t, lines...), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range tr.Messages {
		got = append(got, m.Kind+":"+m.Text+m.Note)
	}
	want := []string{
		"user:/model opus",
		"note:Set model to opus",
		"user:! git status",
		"note:clean",
		"note:Background build finished",
		"note:Request interrupted by user",
		"user:what is this?",
		"user:[image]",
		"note:While you were away: tests pass.",
		"note:Compacted",
		"assistant:API Error: overloaded",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if m := tr.Messages[len(tr.Messages)-1]; m.Model != "" {
		t.Errorf("synthetic model should be blank, got %q", m.Model)
	}
}

func TestClaudeCompaction(t *testing.T) {
	lines := []string{
		cUser,
		`{"parentUuid":null,"logicalParentUuid":"u1","isSidechain":false,"type":"system","subtype":"compact_boundary","content":"Conversation compacted","compactMetadata":{"trigger":"manual"},"uuid":"cb","timestamp":"t"}`,
		`{"parentUuid":"cb","isSidechain":false,"type":"user","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"role":"user","content":"This session is being continued. Summary: did things."},"uuid":"cs","timestamp":"t"}`,
		// A summary without a boundary still renders as compaction.
		`{"isSidechain":false,"type":"user","isCompactSummary":true,"message":{"role":"user","content":[{"type":"text","text":"Second summary."}]},"uuid":"cs2","timestamp":"t"}`,
		`{"type":"summary","summary":"Session title","leafUuid":"u1"}`,
	}
	tr, err := Read(writeSession(t, lines...), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Messages) != 3 {
		t.Fatalf("messages %+v", tr.Messages)
	}
	if c := tr.Messages[1]; c.Kind != "compaction" || c.ID != "cb" || !strings.Contains(c.Note, "did things") {
		t.Errorf("compaction %+v", c)
	}
	if c := tr.Messages[2]; c.Kind != "compaction" || c.Note != "Second summary." {
		t.Errorf("compaction 2 %+v", c)
	}
}

func askRecord(uuid, toolID string) string {
	return `{"type":"assistant","isSidechain":false,"message":{"id":"m-` + uuid + `","model":"claude-test","role":"assistant","content":[{"type":"tool_use","id":"` + toolID + `","name":"AskUserQuestion","input":{"questions":[{"question":"Which DB?","header":"DB","multiSelect":false,"options":[{"label":"Postgres","description":"relational"},{"label":"SQLite","description":"embedded","preview":"x"}]}]}}]},"uuid":"` + uuid + `","timestamp":"2026-10-20T10:00:00Z"}`
}

func TestClaudeAsk(t *testing.T) {
	path := writeSession(t, cUser, askRecord("q1", "toolu_ask"))
	tr, err := Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Ask == nil || tr.Ask.ToolCallID != "toolu_ask" || len(tr.Ask.Questions) != 1 {
		t.Fatalf("ask %+v", tr.Ask)
	}
	q := tr.Ask.Questions[0]
	if q.Question != "Which DB?" || q.Header != "DB" || len(q.Options) != 2 || !q.Options[1].HasPreview || q.Options[0].Description != "relational" {
		t.Errorf("question %+v", q)
	}
	// The answer arrives (appended to the same file; the cache picks it up).
	appendLines(t, path, `{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"tool_use_id":"toolu_ask","type":"tool_result","content":"User has answered your questions: \"Which DB?\"=\"Postgres\"."}]},"uuid":"qa","timestamp":"2026-10-20T10:00:09Z"}`)
	tr, err = Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Ask != nil {
		t.Fatalf("answered ask still pending: %+v", tr.Ask)
	}
	if m := tr.Messages[len(tr.Messages)-1]; m.Kind != "tool" || !strings.Contains(m.Result, "Postgres") || m.Ms != 9000 {
		t.Errorf("ask card %+v", m)
	}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(join(lines...)); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeIncrementalAndPartialLine(t *testing.T) {
	path := writeSession(t, cUser, cThink)
	tr, _ := Read(path, 100, 0)
	if tr.Total != 1 {
		t.Fatalf("total %d", tr.Total)
	}
	// A half-written record is not consumed until its newline lands.
	half := cText[:40]
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(half)
	f.Close()
	if tr, _ = Read(path, 100, 0); tr.Total != 1 {
		t.Fatalf("partial: total %d", tr.Total)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(cText[40:] + "\n" + cTool + "\n")
	f.Close()
	tr, _ = Read(path, 100, 0)
	if tr.Total != 3 || tr.Messages[1].Think != "should run ls" || tr.Messages[2].Result != "" {
		t.Fatalf("after append: %+v", tr.Messages)
	}
	appendLines(t, path, cResult)
	if tr, _ = Read(path, 100, 0); tr.Messages[2].Result == "" {
		t.Fatal("result not patched")
	}
	// A rewritten (shorter) file is parsed from scratch.
	if err := os.WriteFile(path, []byte(join(cUser)), 0o600); err != nil {
		t.Fatal(err)
	}
	if tr, _ = Read(path, 100, 0); tr.Total != 1 {
		t.Fatalf("rewrite: total %d", tr.Total)
	}
}

func TestClaudeWindowing(t *testing.T) {
	var lines []string
	for i := 0; i < 250; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"user","isSidechain":false,"message":{"role":"user","content":"msg %d"},"uuid":"u%d","timestamp":"t"}`, i, i))
	}
	path := writeSession(t, lines...)
	tr, err := Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Total != 250 || !tr.More || len(tr.Messages) != 100 || tr.Messages[0].Text != "msg 150" || tr.Messages[99].Text != "msg 249" {
		t.Fatalf("window 1: total=%d more=%v first=%q", tr.Total, tr.More, tr.Messages[0].Text)
	}
	tr, _ = Read(path, 100, 200)
	if tr.More || len(tr.Messages) != 50 || tr.Messages[0].Text != "msg 0" {
		t.Fatalf("window 3: more=%v n=%d", tr.More, len(tr.Messages))
	}
	// Returned windows are copies: patching the cache must not alias them.
	tr.Messages[0].Text = "mutated"
	if tr2, _ := Read(path, 100, 200); tr2.Messages[0].Text != "msg 0" {
		t.Fatal("window aliases the cache")
	}
}

func TestClaudeLatest(t *testing.T) {
	// Parallel tool calls in one assistant message (separate records sharing
	// message.id); one has its result, the other waits on a permission prompt.
	path := writeSession(t, cUser, cThink, cText, cTool,
		`{"type":"assistant","isSidechain":false,"message":{"id":"msg1","role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Write","input":{"file_path":"/tmp/x"}}]},"uuid":"a5","timestamp":"t"}`,
		cResult,
		cSide,
	)
	l, err := LatestOf(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Text != "Listing." || len(l.Pending) != 1 || l.Pending[0].Tool != "Write" || l.Pending[0].Args != "/tmp/x" {
		t.Fatalf("latest %+v", l)
	}
	appendLines(t, path, `{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"tool_use_id":"toolu_2","type":"tool_result","content":"denied","is_error":true}]},"uuid":"r2","timestamp":"t"}`, cFinal)
	if l, _ = LatestOf(path); l.Text != "**Done.** Two files." || len(l.Pending) != 0 {
		t.Fatalf("latest after %+v", l)
	}
	// A new user prompt clears pending calls.
	appendLines(t, path, cTool, cUser)
	if l, _ = LatestOf(path); len(l.Pending) != 0 {
		t.Fatalf("pending after prompt %+v", l)
	}
}

func TestClaudeLatestTailWindow(t *testing.T) {
	// Larger than tailWindow so LatestOf seeks into the middle of a record.
	pad := strings.Repeat("x", 4096)
	var lines []string
	for i := 0; i*4096 < tailWindow+8192; i++ {
		lines = append(lines, `{"type":"user","isSidechain":false,"message":{"role":"user","content":[{"tool_use_id":"old","type":"tool_result","content":"`+pad+`"}]},"uuid":"p","timestamp":"t"}`)
	}
	lines = append(lines, cText)
	l, err := LatestOf(writeSession(t, append([]string{cMode}, lines...)...))
	if err != nil {
		t.Fatal(err)
	}
	if l.Text != "Listing." {
		t.Fatalf("latest %+v", l)
	}
}

func TestClaudeProjectSlug(t *testing.T) {
	for in, want := range map[string]string{
		"/home/me/code/comebye":                        "-home-me-code-comebye",
		"/home/me/code/x/.claude/worktrees/fix_clippy": "-home-me-code-x--claude-worktrees-fix-clippy",
		"/tmp/claude-1000/-home-me/a b":                "-tmp-claude-1000--home-me-a-b",
		"/home/me/ünï":                                 "-home-me--n-", "/x/😀": "-x---",
	} {
		if got := ClaudeProjectSlug(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

// projects builds a fake Claude projects dir with the given session files
// (relative paths) and mtimes in increasing order.
func projects(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	base := time.Now().Add(-time.Hour)
	for i, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(cUser+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func claudeAt(pane, cwd string, sess *herdr.AgentSessionInfo) herdr.AgentInfo {
	return herdr.AgentInfo{PaneID: pane, Agent: "claude", CWD: cwd, AgentSession: sess}
}

func TestResolveSessionPath(t *testing.T) {
	root := projects(t,
		"-work-a/old.jsonl",
		"-work-a/new.jsonl",
		"-work-a/new/subagents/agent-1.jsonl", // newer, but in a subdirectory
		"-work-b/b1.jsonl",
		"-work-b/b2.jsonl",
		"-elsewhere/sid-123.jsonl",
	)
	r := &Resolver{ProjectsDir: root}
	j := func(p string) string { return filepath.Join(root, p) }

	cases := []struct {
		name string
		a    herdr.AgentInfo
		live []herdr.AgentInfo
		want string
		err  error
	}{
		{"reported path wins", claudeAt("p1", "/work/a", &herdr.AgentSessionInfo{Kind: "path", Value: "/x/y.jsonl"}), nil, "/x/y.jsonl", nil},
		{"pi path", herdr.AgentInfo{PaneID: "p1", Agent: "pi", AgentSession: &herdr.AgentSessionInfo{Kind: "path", Value: "/pi.jsonl"}}, nil, "/pi.jsonl", nil},
		{"pi without session", herdr.AgentInfo{PaneID: "p1", Agent: "pi", CWD: "/work/a"}, nil, "", ErrNoSession},
		{"reported id, any project", claudeAt("p1", "/work/a", &herdr.AgentSessionInfo{Kind: "id", Agent: "claude", Value: "sid-123"}), nil, j("-elsewhere/sid-123.jsonl"), nil},
		{"reported id, bad id falls back", claudeAt("p1", "/work/a", &herdr.AgentSessionInfo{Kind: "id", Value: "../x"}), nil, j("-work-a/new.jsonl"), nil},
		{"cwd fallback picks newest", claudeAt("p1", "/work/a/", nil), nil, j("-work-a/new.jsonl"), nil},
		{"unknown cwd", claudeAt("p1", "/work/zzz", nil), nil, "", ErrNoSession},
		{"no cwd", claudeAt("p1", "", nil), nil, "", ErrNoSession},
		{
			"ambiguous cwd",
			claudeAt("p1", "/work/b", nil),
			[]herdr.AgentInfo{claudeAt("p1", "/work/b", nil), claudeAt("p2", "/work/b", nil)},
			"", ErrAmbiguousSession,
		},
		{
			"shared cwd, other reported its file",
			claudeAt("p1", "/work/b", nil),
			[]herdr.AgentInfo{claudeAt("p1", "/work/b", nil), claudeAt("p2", "/work/b", &herdr.AgentSessionInfo{Kind: "path", Value: j("-work-b/b2.jsonl")})},
			j("-work-b/b1.jsonl"), nil,
		},
		{
			"other agents elsewhere are fine",
			claudeAt("p1", "/work/b", nil),
			[]herdr.AgentInfo{claudeAt("p1", "/work/b", nil), claudeAt("p2", "/work/a", nil), {PaneID: "p3", Agent: "pi", CWD: "/work/b"}},
			j("-work-b/b2.jsonl"), nil,
		},
	}
	for _, tc := range cases {
		got, err := r.SessionPath(tc.a, tc.live)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

func TestResolveLongSlugAndConfigDir(t *testing.T) {
	long := "/" + strings.Repeat("d", 250)
	slug := ClaudeProjectSlug(long)
	root := projects(t, slug[:maxSlugLen]+"-abc123/s.jsonl")
	r := &Resolver{ProjectsDir: root}
	if got, err := r.SessionPath(claudeAt("p1", long, nil), nil); err != nil || got != filepath.Join(root, slug[:maxSlugLen]+"-abc123", "s.jsonl") {
		t.Fatalf("long slug: %q %v", got, err)
	}

	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if got := (&Resolver{}).projectsDir(); got != filepath.Join(cfg, "projects") {
		t.Fatalf("CLAUDE_CONFIG_DIR: %q", got)
	}
}

func TestClaudeConcurrentReads(t *testing.T) {
	path := writeSession(t, cUser, cThink, cText, cTool)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		appendLines(t, path, cResult, cFinal)
	}()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tr, err := Read(path, 100, 0); err == nil {
				_ = fmt.Sprint(tr.Messages) // read the window while others patch the cache
			}
		}()
	}
	wg.Wait()
	tr, err := Read(path, 100, 0)
	if err != nil || tr.Total != 4 || tr.Messages[2].Result == "" {
		t.Fatalf("final %+v err %v", tr, err)
	}
}

// A prompt sent while Claude was working is recorded as a queued_command
// attachment when Claude takes it up; it must show as a user message.
// Claude's own queued notifications and agent hand-backs must not.
func TestClaudeQueuedPrompt(t *testing.T) {
	path := writeSession(t, cMode, cUser, cTool,
		`{"type":"attachment","isSidechain":false,"attachment":{"type":"queued_command","commandMode":"prompt","prompt":"also check the README","humanTurn":true},"uuid":"q1","timestamp":"2026-10-20T10:00:02Z"}`,
		`{"type":"attachment","isSidechain":false,"attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"<task-notification>\n<task-id>x</task-id>\n</task-notification>"},"uuid":"q2","timestamp":"t"}`,
		`{"type":"attachment","isSidechain":false,"attachment":{"type":"queued_command","commandMode":"prompt","isMeta":true,"prompt":"<agent-message from=\"a1\">report</agent-message>"},"uuid":"q3","timestamp":"t"}`,
		`{"type":"attachment","isSidechain":true,"attachment":{"type":"queued_command","commandMode":"prompt","prompt":"subagent input"},"uuid":"q4","timestamp":"t"}`,
		cResult, cFinal)
	tr, err := Read(path, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range tr.Messages {
		got = append(got, m.Kind+":"+m.Text)
	}
	if len(tr.Messages) != 4 || tr.Messages[2].Kind != "user" || tr.Messages[2].Text != "also check the README" || tr.Messages[2].ID != "q1" {
		t.Fatalf("messages = %q", got)
	}
}

// Thinking that precedes a tool call (no bubble carries it) is surfaced as
// the transcript's latest thought, and a new user message clears it.
func TestClaudeLatestThinking(t *testing.T) {
	thinkThenTool := `{"type":"assistant","isSidechain":false,"message":{"id":"msg9","model":"m","role":"assistant","content":[{"type":"thinking","thinking":"need to check the tests first","signature":"s"}]},"uuid":"t9","timestamp":"2026-10-20T10:01:00Z"}` + "\n" +
		`{"type":"assistant","isSidechain":false,"message":{"id":"msg9","model":"m","role":"assistant","content":[{"type":"tool_use","id":"toolu_9","name":"Bash","input":{"command":"go test ./..."}}]},"uuid":"t10","timestamp":"2026-10-20T10:01:01Z"}`
	tr, err := Read(writeSession(t, cMode, cUser, thinkThenTool), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Thinking == nil || tr.Thinking.Text != "need to check the tests first" || tr.Thinking.Time != "2026-10-20T10:01:00Z" {
		t.Fatalf("thinking = %+v", tr.Thinking)
	}
	next := `{"type":"user","isSidechain":false,"message":{"role":"user","content":"now do something else"},"uuid":"u9","timestamp":"2026-10-20T10:02:00Z"}`
	if tr, _ := Read(writeSession(t, cMode, cUser, thinkThenTool, next), 100, 0); tr.Thinking != nil {
		t.Fatalf("a new user turn should clear thinking: %+v", tr.Thinking)
	}
}

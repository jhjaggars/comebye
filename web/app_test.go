package web

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// A page in the back/forward cache keeps an open EventSource (Chrome does),
// and each one pins one of the browser's six HTTP/1.1 connections to the
// origin, so a few cached copies of the app starve every later request.
// The app must close its stream on pagehide (pageshow -> resume() reopens
// it). There is no JS test runner, so guard the wiring at the source level.
func TestAppClosesEventSourceOnPagehide(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`addEventListener\("pagehide",[^\n]*suspendEvents\(\)`,
		`(?s)function suspendEvents\(\) \{.*?state\.es\.close\(\).*?\n\}`,
		`addEventListener\("pageshow",[^\n]*resume\(\)`,
	} {
		if !regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js does not match %s", re)
		}
	}
}

// A running worker whose brief was never delivered must be visible as
// such in the agent list, not just "running": the ledger chip shows
// "no brief" with the recorded error as its tooltip (the handoff answer
// already toasts it; this covers the later view).
func TestAppShowsUndeliveredBriefChip(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`e\.status === "running" && e\.error`,
		`lg-chip failed" title="\$\{esc\(e\.error\)\}">no brief`,
	} {
		if !regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js does not match %s", re)
		}
	}
}

// The vendored markdown libraries load before app.js, and each one matches
// the sha256 recorded in vendor/README.md (copied byte for byte from the
// npm tarball; see that file).
func TestVendoredLibraries(t *testing.T) {
	idx, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)<script src="vendor/marked\.umd\.js"></script>\s*<script src="vendor/purify\.min\.js"></script>\s*<script src="app\.js"></script>`)
	if !re.Match(idx) {
		t.Error("index.html must load vendor/marked.umd.js and vendor/purify.min.js before app.js")
	}
	readme, err := os.ReadFile("vendor/README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"marked.umd.js", "purify.min.js", "mermaid.min.js"} {
		b, err := FS.ReadFile("vendor/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		want := "`" + name + "` " + hex.EncodeToString(sum[:])
		if !strings.Contains(string(readme), want) {
			t.Errorf("vendor/README.md does not record %s", want)
		}
	}
	hl, err := FS.ReadDir("vendor/hljs")
	if err != nil || len(hl) < 2 {
		t.Fatalf("vendor/hljs: %v", err)
	}
	for _, e := range hl {
		b, _ := FS.ReadFile("vendor/hljs/" + e.Name())
		sum := sha256.Sum256(b)
		if want := "`" + e.Name() + "` " + hex.EncodeToString(sum[:]); !strings.Contains(string(readme), want) {
			t.Errorf("vendor/README.md does not record %s", want)
		}
	}
	for _, lic := range []string{"vendor/LICENSE-marked.txt", "vendor/LICENSE-dompurify.txt", "vendor/LICENSE-highlightjs.txt", "vendor/LICENSE-mermaid.txt"} {
		if _, err := FS.ReadFile(lic); err != nil {
			t.Errorf("missing %s: %v", lic, err)
		}
	}
}

// Agent text must only reach innerHTML through the sanitizing renderer.
// (Rendering itself is tested in a real browser: web/test/markdown.test.mjs.)
func TestAppMarkdownIsSanitized(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`lib\.P\.sanitize\(lib\.m\.parse\(src\), MD_PURIFY\)`,
		`ALLOWED_URI_REGEXP: /\^\(\?:\(\?:https\?:\|mailto:\)\|\\/\?#\\/todo\\/t\[0-9a-f\]\{10\}\$\)/i`,
		`"bubble md">\$\{renderMarkdown\(m\.text`,
	} {
		if !regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js does not match %s", re)
		}
	}
	if regexp.MustCompile(`\bmd\(`).Match(src) {
		t.Error("app.js still calls the old md() helper")
	}
}

// A request with no deadline can hang forever on a half-dead mobile
// connection, freezing the raw terminal view (stacked refreshOutput reads)
// and the send button (state.sending never clears). api() must time out,
// and refreshOutput must not overlap itself.
func TestAppRequestsTimeOutAndOutputReadsDoNotStack(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`(?s)async function api\(.*?AbortSignal\.timeout\(timeout\)`,
		`(?s)function resume\(\) \{.*?outputBusy = false;.*?refreshOutput\(\)`,
		`(?s)async function refreshOutput\([a-z]*\) \{.*?if \(outputBusy\) return;.*?finally \{\s*if \(outputAbort === ctl\) \{ outputAbort = null; outputBusy = false;`,
	} {
		if !regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js does not match %s", re)
		}
	}
}

// Opening an agent after one that fell back to the raw terminal view used
// to keep #chat hidden and #output showing the previous agent's text.
// Output reads are also tagged with their agent: a response for a pane that
// is no longer selected is dropped, and a switch aborts the read. The
// header's refresh button re-fetches the view from scratch.
func TestAppTerminalViewIsKeyedByAgent(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`(?s)function openDetail\(id\) \{.*?resetTerminalView\(\);`,
		`(?s)function closeDetail\(\) \{.*?resetTerminalView\(\);`,
		`(?s)function resetTerminalView\(\) \{.*?outputAbort\.abort\(\).*?\$\("output"\)\.textContent = "";.*?\$\("chat"\)\.style\.display = "";`,
		`(?s)async function refreshOutput\([a-z]*\) \{.*?const pane = state\.selected;.*?signal: ctl\.signal.*?if \(state\.selected !== pane \|\| !state\.chat \|\| !state\.chat\.terminal\) return;`,
		`\$\("d-refresh"\)\.addEventListener\("click", refreshView\)`,
		`(?s)function refreshView\(\) \{.*?outputBusy = false;.*?state\.sending = 0;`,
		`echoInOutput\(text\)`,
	} {
		if !regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js does not match %s", re)
		}
	}
	html, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="d-refresh"`) {
		t.Error("index.html has no d-refresh button")
	}
}

// After a send the reply must show up without re-selecting the agent: the
// poll keeps running for a while whatever status the (possibly lagging)
// snapshot reports, and the post-send raw read is not skipped behind a
// stuck one.
func TestAppPollsAfterSend(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`Date\.now\(\) < state\.pollUntil`,
		`(?s)async function postPrompt\(job\) \{.*?state\.pollUntil = Date\.now\(\) \+ POLL_AFTER_SEND_MS;.*?refreshOutput\(true\)`,
		`(?s)async function sendKeys\(.*?state\.pollUntil = Date\.now\(\) \+ POLL_AFTER_SEND_MS;`,
		`(?s)async function refreshOutput\(force\) \{.*?force === true`,
	} {
		if !regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js does not match %s", re)
		}
	}
}

// A peer the hub cannot reach shows the hub's diagnosis (dns, tcp, tls,
// http) and since when, not a bare error: the down row, and the toasts
// that name a host, all carry the class.
func TestAppShowsPeerFailureClass(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`h.since ? Date.parse(h.since) : NaN`,
		`what = "unreachable" + (h.class ? ` + "`: ${h.class}`" + ` : "")`,
		"` (since ${fmtAgo(since)} ago)`",
		`title="${esc(h.error || "")}"`,
		"is unreachable${h.class ? `: ${h.class}` : \"\"}",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("app.js does not contain %q", want)
		}
	}
}

// The to-do panel: Drop asks first (the point of the list is not losing
// follow-up work), Done is a single tap, agent text is escaped, and links
// are only ever http(s).
func TestAppTodoPanelWiring(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`(?s)\.td-drop"\)\) \{\s*if \(!confirm\(`,
		`\.td-done"\)\) \{\s*todoAct\(id, "done"\)`,
		`(?s)function todoLinkHref\(u\) \{.*?/\^https\?:`,
		`esc\(t\.question\)`,
		`renderMarkdown\(t\.context,`,
	} {
		if !regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js does not match %s", re)
		}
	}
}

// Transcripts hold anything an agent or a tool ever printed: output and snippets
// are text nodes, messages go through the sanitizer, and queries and text stay out
// of browser storage. A helper must not take the name of a vendored library's
// global, which would turn markdown off in the whole app.
func TestAppRecallWiring(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`m\.textContent = part`,
		`out\.append\(document\.createTextNode\(part\)\)`,
		`body\.innerHTML = renderMarkdown\(rcPlain\(m\.text\)\)`,
		`out\.append\(rcMarked\(result\.text\)\)`,
		`localStorage\.setItem\(RC_FILTERS_KEY, JSON\.stringify\(rc\.filters\)\)`,
	} {
		if !regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js does not match %s", re)
		}
	}
	for _, re := range []string{`(?m)^function marked\(`, `(?m)^function DOMPurify\(`, `localStorage\.setItem\([^)]*rc\.q`} {
		if regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js matches %s", re)
		}
	}
	// Filters are the only thing remembered, and a query is not among them.
	if strings.Contains(string(src), "rc.filters.q") {
		t.Error("the query went into the remembered filters")
	}
}

// Colours live in themes.js (one entry per theme): index.html must load it
// synchronously from <head> before the body (an external script, since the
// CSP forbids inline ones) and must not hard-code any colour itself, or a
// theme would leave that spot behind.
func TestThemesPlumbing(t *testing.T) {
	idx, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	head, _, ok := strings.Cut(string(idx), "</head>")
	if !ok || !strings.Contains(head, `<script src="themes.js"></script>`) {
		t.Error("index.html must load themes.js in <head>")
	}
	if _, err := FS.ReadFile("themes.js"); err != nil {
		t.Fatalf("themes.js is not embedded: %v", err)
	}
	if re := regexp.MustCompile(`[:( ,]#[0-9a-fA-F]{3,8}\b|rgba?\(|hsla?\(`); re.MatchString(string(idx[strings.Index(string(idx), "<style>"):])) {
		t.Errorf("index.html hard-codes a colour: %q (use a token from themes.js)", re.FindString(string(idx[strings.Index(string(idx), "<style>"):])))
	}
}

// The speech-to-text engine is the one vendored file that is built, not
// copied: its hash is recorded, its licences are shipped, and the worker
// that runs it can reach nothing but this origin.
func TestSpeechEngineFiles(t *testing.T) {
	readme, err := os.ReadFile("vendor/README.md")
	if err != nil {
		t.Fatal(err)
	}
	b, err := FS.ReadFile("vendor/transformers.bundle.js")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if want := "`transformers.bundle.js` " + hex.EncodeToString(sum[:]); !strings.Contains(string(readme), want) {
		t.Errorf("vendor/README.md does not record %s", want)
	}
	if _, err := FS.ReadFile("vendor/LICENSE-transformers-bundle.txt"); err != nil {
		t.Errorf("missing licence file: %v", err)
	}
	idx, _ := FS.ReadFile("index.html")
	if !strings.Contains(string(idx), `<script src="stt.js"></script>`) {
		t.Error("index.html does not load stt.js")
	}
	worker, err := FS.ReadFile("stt-worker.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`env\.allowRemoteModels = false`,
		`env\.useWasmCache = false`, // no blob: URL, so the CSP stays free of blob:
		`from "\./vendor/transformers\.bundle\.js"`,
	} {
		if !regexp.MustCompile(re).Match(worker) {
			t.Errorf("stt-worker.js does not match %s", re)
		}
	}
	for _, name := range []string{"stt.js", "stt-worker.js"} {
		src, _ := FS.ReadFile(name)
		if regexp.MustCompile(`https?://`).Match(src) {
			t.Errorf("%s names an absolute URL; everything must come from this origin", name)
		}
	}
}

// The keyboard layer loads after app.js (it uses its globals), ships in the
// embedded files and counts toward the app version.
func TestKeysScriptLoadsAfterApp(t *testing.T) {
	idx, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	a := strings.Index(string(idx), `<script src="app.js"></script>`)
	k := strings.Index(string(idx), `<script src="keys.js"></script>`)
	if a < 0 || k < a {
		t.Error("index.html must load keys.js after app.js")
	}
	src, err := FS.ReadFile("keys.js")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`https?://`).Match(src) {
		t.Error("keys.js names an absolute URL; everything must come from this origin")
	}
}

// canRestart decides whether the row menu offers "Restart…". The function is cut out of app.js and run in
// Node with the coordinator test stubbed, so the app's own code is what is checked.
func TestCanRestart(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	kinds := regexp.MustCompile(`(?m)^const RESTART_KINDS = .*;$`).Find(src)
	fn := regexp.MustCompile(`(?s)\nfunction canRestart\(a\) \{.*?\n\}\n`).Find(src)
	if kinds == nil || fn == nil {
		t.Fatal("RESTART_KINDS or canRestart not found in app.js")
	}
	js := string(kinds) + "\nconst isCoordinator = (a) => !!a && a.coord === true;\n" + string(fn) + `
const cases = [
  [{ agent: "claude", agent_status: "idle", name: "w" }, true],
  [{ agent: "claude", agent_status: "done", name: "w" }, true],
  [{ agent: "pi", agent_status: "idle", name: "w" }, true],
  [{ agent: "opencode", agent_status: "done", name: "w" }, true],
  [{ agent: "claude", agent_status: "idle" }, true], // unnamed: the hub names it for the moment
  [{ agent: "claude", agent_status: "working", name: "w" }, false],
  [{ agent: "claude", agent_status: "blocked", name: "w" }, false],
  [{ agent: "claude", agent_status: "unknown", name: "w" }, false],
  [{ agent: "codex", agent_status: "idle", name: "w" }, false],
  [{ agent: "", agent_status: "idle", name: "w" }, false],
  [{ agent: "claude", agent_status: "idle", name: "c", coord: true }, false],
  [null, false],
  [undefined, false],
];
const bad = cases.filter(([a, want]) => canRestart(a) !== want).map(([a]) => JSON.stringify(a));
console.log(JSON.stringify(bad));
`
	out, err := exec.Command(node, "-e", js).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "[]" {
		t.Fatalf("canRestart gave the wrong answer for: %s", out)
	}
}

// toolRunSummary is the grey line over a run of tool calls ("Ran 3 commands, edited 2 files"). The table and the
// function are cut out of app.js and run in Node, so the app's own code is what is checked.
func TestToolRunSummary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, re := range []string{
		`(?m)^function toolRunning\(m\) \{.*\}$`,
		`(?s)\nconst TOOL_KINDS = \{.*?\n\};\n`,
		`(?s)\nfunction toolKind\(name\) \{.*?\n\}\n`,
		`(?s)\nconst TOOL_PHRASES = \{.*?\n\};\n`,
		`(?s)\nfunction toolRowVerb\(m\) \{.*?\n\}\n`,
		`(?s)\nfunction toolRunSummary\(calls\) \{.*?\n\}\n`,
	} {
		b := regexp.MustCompile(re).Find(src)
		if b == nil {
			t.Fatalf("app.js has nothing matching %s", re)
		}
		parts = append(parts, string(b))
	}
	js := strings.Join(parts, "\n") + `
const t = (tool, extra) => Object.assign({ id: tool + Math.random(), kind: "tool", tool, args: "x", result: "ok", ms: 5 }, extra || {});
const cases = [
  // the example from the issue, in first-seen order
  [[t("Bash"), t("Bash"), t("Edit"), t("Read"), t("Edit"), t("Read"), t("Read"), t("Bash"), t("Read"), t("Read")], "Ran 3 commands, edited 2 files, read 5 files"],
  [[t("Bash")], "Ran a command"],
  [[t("Read")], "Read a file"],
  [[t("Write"), t("Write")], "Wrote 2 files"],
  [[t("Grep"), t("Glob"), t("Grep")], "Ran 3 searches"],
  [[t("WebSearch"), t("WebFetch"), t("WebFetch")], "Searched the web, fetched 2 pages"],
  [[t("Task")], "Ran a subagent"],
  [[t("TodoWrite"), t("TodoWrite")], "Updated the plan 2 times"],
  [[t("mcp__github__list_prs"), t("mcp__jira__get")], "Called 2 MCP tools"],
  // pi names
  [[t("bash"), t("read"), t("read"), t("edit"), t("write"), t("find"), t("ls")], "Ran a command, read 2 files, edited a file, wrote a file, ran 2 searches"],
  // unknown tools get a generic verb
  [[t("Frobnicate"), t("")], "Used 2 tools"],
  [[t("Read"), t("Frobnicate")], "Read a file, used a tool"],
];
const bad = cases.filter(([calls, want]) => toolRunSummary(calls).text !== want).map(([calls, want]) => want + " != " + toolRunSummary(calls).text);

// failures, a running call and the peek
let s = toolRunSummary([t("Bash", { err: true, result: "boom\nmore" }), t("Bash")]);
if (s.failed !== 1 || s.peek !== "") bad.push("an earlier failure is only a count: " + JSON.stringify(s));
s = toolRunSummary([t("Bash"), t("Bash", { err: true, result: "\n  exit status 2: no such file\nsecond line" })]);
if (s.failed !== 1 || s.peek !== "exit status 2: no such file") bad.push("the last failure peeks its first line: " + JSON.stringify(s));
s = toolRunSummary([t("Bash", { err: true, result: "" })]);
if (s.peek !== "failed") bad.push("a failure with no output still peeks: " + JSON.stringify(s));
s = toolRunSummary([t("Bash"), t("Read", { result: "", ms: 0 })]);
if (s.running !== 1 || s.failed !== 0 || s.peek !== "") bad.push("a call without a result is running: " + JSON.stringify(s));
if (toolRowVerb(t("Read", { result: "", ms: 0 })) !== "Reading" || toolRowVerb(t("Edit")) !== "Edited") bad.push("row verbs");
console.log(JSON.stringify(bad));
`
	out, err := exec.Command(node, "-e", js).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "[]" {
		t.Fatalf("toolRunSummary gave the wrong answer for: %s", out)
	}
}

// A stopped worker shows "Stopped" with a Start button (issue #66): the app
// reads the wake book, starts through the hub's route with the CSRF token,
// asks first for a host stopped at its budget cap, and shows progress.
func TestAppHasAStartButtonForStoppedWorkers(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`function wakeOf\(h\)`,
		"/api/provision/hosts/\\$\\{encodeURIComponent\\(name\\)\\}/start",
		`confirm_cap: !!confirmed`,
		`"/api/provision/wake"`,
		`data-wk-start=`,
		`Start again`,
		`Start anyway`,
		`wk\.wakes = ph\.data\.wakes`,
		`!h\.adopted \? ` + "`" + `<button type="button" class="btn danger" data-ph-destroy`,
	} {
		if !regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js does not match %s", re)
		}
	}
}

// Slice 4 of issue #66: the launch picker marks a stopped worker and can pick
// it, the rollout confirm says "stopped, will be started" (offline keeps its
// reason), a finished step says "woke it", the stopped session's chip does not
// look live, and the phone gets full-width buttons under 420 px.
func TestAppPolishesStoppedWorkers(t *testing.T) {
	js, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`const wx = wakeOf\(h\); // a stopped worker can be picked`,
		`class="ln-wk `,
		`function renderLaunchStopped\(host\)`,
		`if \(launchHostStopped\(host\)\) \{ renderLaunchStopped\(host\); return; \}`,
		`stopped, will be started \(about \$\{h\.wake_min \|\| 4\} min of runtime\)`,
		`h\.wake_note \?`,
		`"woke it"`,
		`ds\.textContent = "stopped"`,
	} {
		if !regexp.MustCompile(re).Match(js) {
			t.Errorf("app.js does not match %s", re)
		}
	}
	for _, re := range []string{
		`\.rl-tag-stop`,
		`@media \(max-width: 420px\) \{ \.wk-start \{ width: 100%; \} .*\.wk-launch \.btn \{ width: 100%; \}`,
		`\.rel-confirm \.td-acts button \{ width: 100%`,
	} {
		if !regexp.MustCompile(re).Match(html) {
			t.Errorf("index.html does not match %s", re)
		}
	}
}

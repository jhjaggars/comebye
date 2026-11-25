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

// A running provisioned worker has a Stop button on its page, beside where a
// stopped one has Start: it asks first, stops through the hub's route with the
// CSRF token and follows the job. Start and Stop name a node only when one node
// lists the host; with two, the hub chooses.
func TestAppHasAStopButtonForRunningWorkers(t *testing.T) {
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		"/api/provision/hosts/\\$\\{encodeURIComponent\\(name\\)\\}/stop",
		`body: JSON\.stringify\(\{ via: phVia\(name\) \}\)`,
		`body: JSON\.stringify\(\{ via: phVia\(name\), confirm_cap: !!confirmed \}\)`,
		`data-sp-stop=`,
		`data-sp-yes=`,
		`Its disks are kept, and Start brings it back`,
		`/api/provision/jobs/\$\{encodeURIComponent\(j\.id\)\}`,
		`const html = head \+ wsec \+ ssec \+ reach \+ body;`,
	} {
		if !regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js does not match %s", re)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	var js strings.Builder
	for _, name := range []string{"phHost", "phVia", "stopOf"} {
		b := regexp.MustCompile(`(?s)\nfunction ` + name + `\(.*?\n\}\n`).Find(src)
		if b == nil {
			b = regexp.MustCompile(`\nfunction ` + name + `\(.*\n`).Find(src)
		}
		if b == nil {
			t.Fatalf("app.js has no %s", name)
		}
		js.Write(b)
	}
	js.WriteString(`
const ph = { data: { hosts: [
  { name: "kv1", via: "desk", state: "running" }, { name: "kv1", via: "kube", state: "running" },
  { name: "demo1", via: "desk", state: "stopped" }, { name: "old", via: "kube", state: "destroyed" },
] } };
function peerStatusOf(h) { return h.status; }
const got = {
  vKv1: phVia("kv1"), vDemo: phVia("demo1"), vOld: phVia("old"), vNone: phVia("nope"),
  sUp: !!stopOf({ name: "kv1", status: "ok" }), sDown: !!stopOf({ name: "kv1", status: "down" }),
  sStopped: !!stopOf({ name: "demo1", status: "ok" }), sLocal: !!stopOf({ name: "kv1", status: "ok", local: true }),
};
console.log(JSON.stringify(got));
`)
	out, err := exec.Command(node, "-e", js.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	want := `{"vKv1":"","vDemo":"desk","vOld":"","vNone":"","sUp":true,"sDown":false,"sStopped":false,"sLocal":false}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
}

// The model chip beside the context chip shows a short name: Claude ids as
// "Opus 5.5", other models by their last path segment minus a date or build
// stamp. The full provider/model stays in the tooltip.
func TestShortModel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	b := regexp.MustCompile(`(?s)\nfunction shortModel\(id\) \{.*?\n\}\n`).Find(src)
	if b == nil {
		t.Fatal("app.js has no shortModel")
	}
	js := string(b) + `
const cases = [
  ["claude-opus-5-5", "Opus 5.5"], ["claude-sonnet-4-6", "Sonnet 4.6"], ["claude-haiku-5-5", "Haiku 5.5"],
  ["claude-fable-5-2", "Fable 5.2"], ["claude-opus-5", "Opus 5"], ["claude-sonnet-4-5-20250929", "Sonnet 4.5"],
  ["claude-opus-4-1-20250805", "Opus 4.1"], ["claude-3-5-sonnet-20241022", "Sonnet 3.5"], ["claude-opus-5-5[1m]", "Opus 5.5"],
  ["anthropic/claude-opus-5-5", "Opus 5.5"],
  ["enmaas/rits/deepseek-ai/DeepSeek-V4.1-Flash-1017", "DeepSeek-V4.1-Flash"],
  ["gpt-5", "gpt-5"], ["openai/gpt-4o-2024-08-06", "gpt-4o-2024-08-06"], ["m1", "m1"], ["a/b/", "b"], ["", ""], [null, ""], ["  ", ""],
];
console.log(JSON.stringify(cases.filter(([i, w]) => shortModel(i) !== w).map(([i, w]) => i + " -> " + shortModel(i) + ", wanted " + w)));
`
	out, err := exec.Command(node, "-e", js).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "[]" {
		t.Fatalf("shortModel gave the wrong answer for: %s", out)
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

// "Roll out to all" does everything in one click: the confirm lists the
// peers, the hub last, and after it the peers its deploy upgrades (with_hub,
// the in-cluster provisioner) as "with the hub", never as a blocker. A host
// the app may not upgrade is listed as skipped with why, and not counted. The
// fleet row of a with_hub peer has no Upgrade button, and a done rollout
// says its offline hosts are upgraded when they are back.
func TestAppRolloutToAllCoversTheHubAndItsPeers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	var js strings.Builder
	js.WriteString("const rls = { confirm: null, busy: false };\n")
	for _, name := range []string{"esc", "escAttr", "cpOut", "relShort", "relRecordText", "relDeployText", "relDeployOut", "relWhen",
		"roJob", "roLive", "RO_PHASE", "roAgain", "roSkipOfflineHtml", "roStepText", "roGatesHtml", "roBandHtml", "rlPreviewTag", "roConfirmHtml", "roButtonHtml", "relRowHtml"} {
		b := regexp.MustCompile(`\n(function ` + name + `\(.*\}|const ` + name + ` = .*;)\n`).Find(src) // a one-liner
		if b == nil {
			b = regexp.MustCompile(`(?s)\nfunction ` + name + `\(.*?\n\}\n`).Find(src)
		}
		if b == nil {
			t.Fatalf("app.js has no %s", name)
		}
		js.Write(b)
	}
	js.WriteString(`
const rl = { version: "v1.2.3", commit: "a".repeat(40), state: "tagged" };
const d = { current: true, todo: { id: "t1", release: rl }, build: { state: "success" }, rollout_enabled: true, rollout_hub_label: "Bump hub",
  deploy: { enabled: true, label: "Bump hub" }, release_host: "hub",
  hosts: [
    { name: "a", status: "ok", version: "v1.2.2", enabled: true },
    { name: "b", status: "ok", version: "v1.2.2", enabled: false },
    { name: "hub", status: "ok", version: "v1.2.2", hub: true, local: true },
    { name: "prov", status: "ok", version: "v1.2.2", with_hub: true },
  ],
  rollout_plan: [
    { host: "a", wave: "peers", phase: "pending" },
    { host: "b", wave: "peers", phase: "skipped", detail: "the app is not allowed to upgrade it (start comebye there with -release-upgrade)" },
    { host: "hub", wave: "hub", mode: "deploy", phase: "pending" },
    { host: "prov", wave: "hub", mode: "with_hub", phase: "pending" },
  ] };
rls.confirm = { k: "rollout", order: ["a", "b"], hub: true };
const confirm = roConfirmHtml(d);
rls.confirm = null;
const job = { id: "r1", todo: "t1", state: "done", plan: [
  { host: "a", wave: "peers", phase: "ok" }, { host: "c", wave: "peers", phase: "skipped", detail: "offline for 2m0s", catch_up: true },
  { host: "hub", wave: "hub", mode: "deploy", phase: "ok" }, { host: "prov", wave: "hub", mode: "with_hub", phase: "ok", detail: "upgraded with the hub" } ] };
const got = {
  button: /Roll out to all/.test(roButtonHtml(d, true)),
  withHubLi: /data-with-hub>[^]*with the hub[^]*upgraded with the hub by Bump hub/.test(confirm),
  skippedB: /data-host="b" data-skipped>[^]*skipped[^]*-release-upgrade/.test(confirm),
  count: /Confirm: roll out v1\.2\.3 to 3 hosts/.test(confirm),
  order: confirm.indexOf('data-host="hub"') < confirm.indexOf('data-host="prov"'),
  provRow: /upgraded with the hub \(Bump hub\)/.test(relRowHtml(d.hosts[3], d, null)) && !/data-rel="up"/.test(relRowHtml(d.hosts[3], d, null)),
  provOffline: /not reachable; upgraded with the hub/.test(relRowHtml({ ...d.hosts[3], offline: true, status: "down" }, d, null)),
  aRow: /data-rel="up"/.test(relRowHtml(d.hosts[0], d, null)),
  stepPending: roStepText({ host: "prov", mode: "with_hub", phase: "pending" }, { state: "running" }),
  stepOk: roStepText(job.plan[3], job),
  stepSkip: roStepText(job.plan[1], job),
  gates: /hub deployed/.test(roGatesHtml({ host: "prov", mode: "with_hub", phase: "verifying" }, "v1.2.3")),
  band: /data-catch-up>Offline hosts are upgraded by themselves when they are back: c\./.test(roBandHtml(d, job)),
  // Roll out again takes only what the plan can act on: b (not allowed) never.
  again: roAgain({ ...d, rollout_plan: [{ host: "c", wave: "peers", phase: "pending" }, d.rollout_plan[1]] }, { ...job, plan: [...job.plan, { host: "b", wave: "peers", phase: "skipped" }] }).join(),
  noAgain: roAgain(d, { ...job, plan: [{ host: "b", wave: "peers", phase: "skipped" }] }).length,
  // "Done, skip offline hosts" only when every host behind is offline.
  skipOff: /data-rel="done-skip-offline"[^]*mac is offline/.test(roSkipOfflineHtml({ todo: { id: "t1", state: "open" }, hosts: [{ name: "a", current: true }, { name: "mac", offline: true }] }, null)),
  noSkipOff: roSkipOfflineHtml(d, null) + roSkipOfflineHtml({ todo: { id: "t1", state: "done" }, hosts: [{ name: "mac", offline: true }] }, null),
};
console.log(JSON.stringify(got));
`)
	out, err := exec.Command(node, "-e", js.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	want := `{"button":true,"withHubLi":true,"skippedB":true,"count":true,"order":true,"provRow":true,"provOffline":true,"aRow":true,"stepPending":"upgraded with the hub, after it","stepOk":"upgraded with the hub","stepSkip":"skipped · offline for 2m0s · upgrades when it is back","gates":true,"band":true,"again":"c","noAgain":0,"skipOff":true,"noSkipOff":""}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
}

// A kept sign-in is dropped only on the host's own "no such sign-in" (#139):
// the phone's re-attach after a reload lost the sign-in on another 404 on the
// way. sinGone is cut out of app.js and run in Node.
func TestSigninGoneOnlyOnTheHostsOwnAnswer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?s)\nfunction sinGone\(r\) \{.*?\n\}\n`).Find(src)
	if fn == nil {
		t.Fatal("sinGone not found in app.js")
	}
	js := string(fn) + `
const cases = [
  [{ status: 404, body: { code: "no_signin", message: "no such sign-in" } }, true],
  [{ status: 404, body: { code: "not_found", message: "no such sign-in" } }, true], // a host from before no_signin
  [{ status: 404, body: { code: "unknown_host", message: "no such host: kube" } }, false],
  [{ status: 404, body: { code: "unknown_host", message: "kube was removed" } }, false],
  [{ status: 404, body: { code: "not_found", message: "not a proxied operation" } }, false],
  [{ status: 404, body: {} }, false],
  [{ status: 0, body: { message: "timed out" } }, false],
  [{ status: 502, body: { code: "host_unreachable" } }, false],
  [{ status: 504, body: { code: "host_timeout" } }, false],
  [{ status: 200, body: { id: "si_x", state: "waiting_code" } }, false],
  [null, false],
];
const bad = cases.filter(([r, want]) => sinGone(r) !== want).map(([r]) => JSON.stringify(r));
console.log(JSON.stringify(bad));
`
	out, err := exec.Command(node, "-e", js).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "[]" {
		t.Fatalf("sinGone gave the wrong answer for: %s", out)
	}
}

// patchHTML (#139 slice 4) redraws a panel by patching it: a child keeps its node when its key (or, with no key, its tag
// and place) matches, and only what came, went or moved is added, removed or moved. The helper is cut out of app.js and
// run in Node on a small stand-in DOM (no browser), so the app's own code is what is checked: reorder, insert, remove,
// identity kept (also for a row that moved to another group), the press layer's classes kept, and a typed value kept.
func TestPatchHTMLKeepsNodes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src, err := FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	var js strings.Builder
	for _, name := range []string{"PATCH_KEEP", "PATCH_KEYS", "patchKey", "patchPool", "patchKids", "patchRun", "patchNode"} {
		b := regexp.MustCompile(`\n(const ` + name + ` = .*;)\n`).Find(src)
		if b == nil {
			b = regexp.MustCompile(`(?s)\nfunction ` + name + `\(.*?\n\}\n`).Find(src)
		}
		if b == nil {
			t.Fatalf("app.js has no %s", name)
		}
		js.Write(b)
	}
	js.WriteString(`
// The stand-in DOM: just what patchKids and patchNode use. moves counts insertBefore calls on nodes already in a parent.
let moves = 0;
const doc = { activeElement: null };
class N {
  constructor(type, tag) { this.nodeType = type; this.localName = tag; this.childNodes = []; this.parentNode = null; this.ownerDocument = doc; this.attrs = new Map(); this.nodeValue = null; }
  get attributes() { return [...this.attrs].map(([name, value]) => ({ name, value })); }
  getAttribute(k) { return this.attrs.has(k) ? this.attrs.get(k) : null; }
  setAttribute(k, v) { this.attrs.set(k, String(v)); }
  removeAttribute(k) { this.attrs.delete(k); }
  hasAttribute(k) { return this.attrs.has(k); }
  get classList() { const c = (this.getAttribute("class") || "").split(/\s+/); return { contains: (x) => c.includes(x) }; }
  get textContent() { return this.nodeType === 3 ? this.nodeValue : this.childNodes.map((c) => c.textContent).join(""); }
  get isConnected() { return true; }
  contains(n) { for (; n; n = n.parentNode) if (n === this) return true; return false; }
  removeChild(n) { this.childNodes.splice(this.childNodes.indexOf(n), 1); n.parentNode = null; return n; }
  insertBefore(n, ref) {
    if (n.parentNode) { if (n.parentNode === this) moves++; n.parentNode.removeChild(n); }
    const i = ref ? this.childNodes.indexOf(ref) : this.childNodes.length;
    this.childNodes.splice(i, 0, n); n.parentNode = this; return n;
  }
}
const h = (tag, attrs, ...kids) => { const n = new N(1, tag); for (const k in attrs) n.setAttribute(k, attrs[k]); for (const c of kids) n.insertBefore(typeof c === "string" ? Object.assign(new N(3, "#text"), { nodeValue: c }) : c, null); moves = 0; return n; };
const row = (id, text, cls = "agent idle") => h("div", { class: cls, "data-id": id }, h("span", { class: "name" }, text));
const list = (...rows) => h("div", {}, ...rows);
const ids = (el) => el.childNodes.map((c) => c.getAttribute("data-id") || c.localName).join(",");
const got = {};

// Identity kept: a poll that changes one row's text and class keeps every row the same node.
{
  const el = list(row("a", "A"), row("b", "B"), row("c", "C"));
  const [a, b, c] = el.childNodes, bText = b.childNodes[0].childNodes[0];
  patchKids(el, list(row("a", "A"), row("b", "B2", "agent blocked"), row("c", "C")));
  got.identity = el.childNodes[0] === a && el.childNodes[1] === b && el.childNodes[2] === c && b.childNodes[0].childNodes[0] === bText;
  got.updated = b.textContent + "|" + b.getAttribute("class");
  got.identityMoves = moves;
}
// Reorder: the last row moves to the top; the others stay put, and only one node moves.
{
  const el = list(row("a", "A"), row("b", "B"), row("c", "C"), row("d", "D"));
  const before = [...el.childNodes];
  patchKids(el, list(row("d", "D"), row("a", "A"), row("b", "B"), row("c", "C")));
  got.reorder = ids(el);
  got.reorderSame = el.childNodes.every((n) => before.includes(n));
  got.reorderMoves = moves;
}
// Insert: a new row between two kept ones.
{
  const el = list(row("a", "A"), row("c", "C"));
  const [a, c] = el.childNodes;
  patchKids(el, list(row("a", "A"), row("b", "B"), row("c", "C")));
  got.insert = ids(el);
  got.insertKept = el.childNodes[0] === a && el.childNodes[2] === c && el.childNodes[1].textContent === "B";
}
// Remove: the gone row goes, the rest stay the same nodes.
{
  const el = list(row("a", "A"), row("b", "B"), row("c", "C"));
  const [a, , c] = el.childNodes;
  patchKids(el, list(row("a", "A"), row("c", "C")));
  got.remove = ids(el);
  got.removeKept = el.childNodes[0] === a && el.childNodes[1] === c;
}
// Rows with no key match by tag and place.
{
  const el = list(h("p", {}, "one"), h("p", {}, "two"));
  const [p1, p2] = el.childNodes;
  patchKids(el, list(h("p", {}, "uno"), h("p", {}, "dos"), h("ul", {})));
  got.loose = el.childNodes[0] === p1 && el.childNodes[1] === p2 && el.textContent === "unodos" && el.childNodes[2].localName === "ul";
}
// The same key with another tag is another node.
{
  const el = list(h("div", { "data-id": "x" }, "div"));
  const d = el.childNodes[0];
  patchKids(el, list(h("a", { "data-id": "x" }, "link")));
  got.tag = el.childNodes[0] !== d && el.childNodes[0].localName === "a";
}
// The press layer and the return mark are the app's: they stay on a kept row, and stale attributes go.
{
  const el = list(row("a", "A", "agent idle is-pressed was-open"));
  const a = el.childNodes[0];
  a.setAttribute("style", "animation-delay: -200ms"); a.setAttribute("aria-busy", "true");
  patchKids(el, list(row("a", "A", "agent working")));
  got.keep = a.getAttribute("class") + "|" + a.getAttribute("style") + "|" + a.hasAttribute("aria-busy");
}
// A typed value stays while the markup's value is the same, and follows it when the markup changes it.
{
  const el = list(h("input", { id: "q", value: "x" }));
  const q = el.childNodes[0];
  q.value = "typed";
  patchKids(el, list(h("input", { id: "q", value: "x" })));
  got.typed = q.value;
  patchKids(el, list(h("input", { id: "q", value: "y" })));
  got.changed = q.value + "|" + (el.childNodes[0] === q);
}
// A row that moves to another group (its status changed) is the same node in its new place.
{
  const sec = (key, ...rows) => h("section", { "data-key": key }, ...rows);
  const el = list(sec("idle", row("a", "A"), row("b", "B")), sec("blocked", row("c", "C")));
  const [a, b] = el.childNodes[0].childNodes, c = el.childNodes[1].childNodes[0];
  patchKids(el, list(sec("idle", row("a", "A")), sec("blocked", row("b", "B", "agent blocked"), row("c", "C"))), patchPool(el));
  const [s1, s2] = el.childNodes;
  got.across = ids(s1) + "/" + ids(s2) + "|" + (s1.childNodes[0] === a && s2.childNodes[0] === b && s2.childNodes[1] === c) + "|" + b.getAttribute("class");
  // and the other way round: the group it lands in comes first
  patchKids(el, list(sec("blocked", row("a", "A", "agent blocked"), row("b", "B", "agent blocked"), row("c", "C")), sec("idle")), patchPool(el));
  got.across2 = ids(el.childNodes[0]) + "|" + (el.childNodes[0].childNodes[0] === a && el.childNodes[0].childNodes[1] === b && el.childNodes[0].childNodes[2] === c);
}
// The longest run already in order stays.
got.run = patchRun([3, 0, 1, 4, 2, 5]).join(",");
console.log(JSON.stringify(got));
`)
	out, err := exec.Command(node, "-e", js.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	want := `{"identity":true,"updated":"B2|agent blocked","identityMoves":0,"reorder":"d,a,b,c","reorderSame":true,"reorderMoves":1,"insert":"a,b,c","insertKept":true,"remove":"a,c","removeKept":true,"loose":true,"tag":true,"keep":"agent working is-pressed was-open|animation-delay: -200ms|false","typed":"typed","changed":"y|true","across":"a/b,c|true|agent blocked","across2":"a,b,c|true","run":"0,1,2,5"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("patchHTML:\n got %s\nwant %s", out, want)
	}
}

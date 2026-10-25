package web

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
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
	for _, name := range []string{"marked.umd.js", "purify.min.js"} {
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
	for _, lic := range []string{"vendor/LICENSE-marked.txt", "vendor/LICENSE-dompurify.txt"} {
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
		`ALLOWED_URI_REGEXP: /\^\(\?:https\?:\|mailto:\)/i`,
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
		`(?s)async function refreshOutput\(\) \{.*?if \(outputBusy\) return;.*?finally \{\s*if \(outputAbort === ctl\) \{ outputAbort = null; outputBusy = false;`,
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
		`(?s)async function refreshOutput\(\) \{.*?const pane = state\.selected;.*?signal: ctl\.signal.*?if \(state\.selected !== pane \|\| !state\.chat \|\| !state\.chat\.terminal\) return;`,
		`\$\("d-refresh"\)\.addEventListener\("click", refreshView\)`,
		`(?s)function refreshView\(\) \{.*?outputBusy = false;.*?state\.sending = false;`,
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

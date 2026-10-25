package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/dialog"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/transcript"
)

// fakeIO serves a scripted visible screen and records send_keys calls.
type fakeIO struct {
	screens []string // successive reads; the last one repeats
	reads   int
	sent    [][]string
	reject  func(keys []string) error
}

func (f *fakeIO) ReadPane(_ context.Context, _, source string, _ int) (herdr.ReadResult, error) {
	if source != "visible" {
		panic("dialog reads must use the visible screen")
	}
	i := f.reads
	if i >= len(f.screens) {
		i = len(f.screens) - 1
	}
	f.reads++
	return herdr.ReadResult{Text: f.screens[i], Source: source}, nil
}

func (f *fakeIO) SendKeys(_ context.Context, _ string, keys []string) error {
	if f.reject != nil {
		if err := f.reject(keys); err != nil {
			return err
		}
	}
	f.sent = append(f.sent, keys)
	return nil
}

const gateScreen = `
  ⚠️ Permission Required

  Rule: bash.git.push

    git push origin feature

    Allow once
  ❯ Block

  ↑↓ choose  •  enter select  •  esc block
`

const permScreen = `
 Bash command
   rm -rf build
 Do you want to proceed?
 ❯ 1. Yes
   2. No, and tell Claude what to do differently (esc)
`

var (
	piBlocked     = herdr.AgentInfo{PaneID: "w:p1", Agent: "pi", Status: herdr.StatusBlocked}
	claudeBlocked = herdr.AgentInfo{PaneID: "w:p2", Agent: "claude", Status: herdr.StatusBlocked}
)

func fp(t *testing.T, screen, kind string) string {
	t.Helper()
	d := dialog.Detect(screen, kind)
	if d == nil {
		t.Fatal("fixture not detected")
	}
	return d.Fingerprint
}

func TestAnswerSendsKeysFromCurrentHighlight(t *testing.T) {
	io := &fakeIO{screens: []string{gateScreen}}
	// The highlight is on Block (index 1); "Allow once" is one row up.
	status, body := answerDialog(context.Background(), io, piBlocked, nil,
		answerBody{Action: "select", Index: 0, Fingerprint: fp(t, gateScreen, "pi")}, newAnswerDedupe(), time.Now())
	if status != http.StatusOK {
		t.Fatalf("status %d %v", status, body)
	}
	if want := [][]string{{"up", "enter"}}; !reflect.DeepEqual(io.sent, want) {
		t.Fatalf("sent %v, want %v (one send_keys call)", io.sent, want)
	}
}

func TestAnswerDialogChanged(t *testing.T) {
	// The phone rendered the gate for one command; the screen now asks about another.
	old := fp(t, gateScreen, "pi")
	changed := strings.Replace(gateScreen, "git push origin feature", "git push --force", 1)
	io := &fakeIO{screens: []string{changed}}
	status, body := answerDialog(context.Background(), io, piBlocked, nil,
		answerBody{Action: "select", Index: 0, Fingerprint: old}, newAnswerDedupe(), time.Now())
	m, _ := body.(map[string]any)
	if status != http.StatusConflict || m["code"] != "dialog_changed" {
		t.Fatalf("got %d %v", status, body)
	}
	if d, _ := m["dialog"].(*dialog.Dialog); d == nil || d.Fingerprint == old {
		t.Fatalf("409 must carry the fresh dialog: %v", m["dialog"])
	}
	if len(io.sent) != 0 {
		t.Fatalf("keys sent despite mismatch: %v", io.sent)
	}
	// Dialog gone entirely.
	io = &fakeIO{screens: []string{"all done\n❯ \n"}}
	status, body = answerDialog(context.Background(), io, piBlocked, nil,
		answerBody{Action: "select", Index: 0, Fingerprint: old}, newAnswerDedupe(), time.Now())
	if m, _ := body.(map[string]any); status != http.StatusConflict || m["code"] != "no_dialog" || len(io.sent) != 0 {
		t.Fatalf("no dialog: %d %v", status, body)
	}
}

func TestAnswerIdempotency(t *testing.T) {
	io := &fakeIO{screens: []string{gateScreen}}
	dd := newAnswerDedupe()
	now := time.Now()
	req := answerBody{Action: "select", Index: 1, Fingerprint: fp(t, gateScreen, "pi")}
	if s, b := answerDialog(context.Background(), io, piBlocked, nil, req, dd, now); s != http.StatusOK {
		t.Fatalf("first: %d %v", s, b)
	}
	// The screen still shows the dialog (not re-rendered yet): a double tap is refused.
	s, b := answerDialog(context.Background(), io, piBlocked, nil, req, dd, now.Add(500*time.Millisecond))
	if m, _ := b.(map[string]any); s != http.StatusConflict || m["code"] != "duplicate" {
		t.Fatalf("second: %d %v", s, b)
	}
	// Cancel within the window is refused too (same dialog).
	if s, _ := answerDialog(context.Background(), io, piBlocked, nil, answerBody{Action: "cancel", Fingerprint: req.Fingerprint}, dd, now.Add(time.Second)); s != http.StatusConflict {
		t.Fatalf("cancel during window: %d", s)
	}
	if len(io.sent) != 1 {
		t.Fatalf("sent %v, want exactly one call", io.sent)
	}
	// After the window the same dialog can be answered again.
	if s, b := answerDialog(context.Background(), io, piBlocked, nil, req, dd, now.Add(answerDedupeWindow+time.Millisecond)); s != http.StatusOK {
		t.Fatalf("after window: %d %v", s, b)
	}
	// A different pane with identical content is independent.
	other := piBlocked
	other.PaneID = "w:p9"
	if s, _ := answerDialog(context.Background(), io, other, nil, req, dd, now.Add(answerDedupeWindow+2*time.Millisecond)); s != http.StatusOK {
		t.Fatalf("other pane: %d", s)
	}
}

func TestAnswerSendFailureReleasesClaim(t *testing.T) {
	calls := 0
	io := &fakeIO{screens: []string{gateScreen}, reject: func([]string) error {
		calls++
		if calls == 1 {
			return &herdr.Error{Code: "server_unavailable", Message: "try again"}
		}
		return nil
	}}
	dd := newAnswerDedupe()
	req := answerBody{Action: "cancel", Fingerprint: fp(t, gateScreen, "pi")}
	if s, _ := answerDialog(context.Background(), io, piBlocked, nil, req, dd, time.Now()); s != http.StatusBadGateway {
		t.Fatalf("failed send: %d", s)
	}
	if s, _ := answerDialog(context.Background(), io, piBlocked, nil, req, dd, time.Now()); s != http.StatusOK {
		t.Fatalf("retry after failure: %d", s)
	}
	if !reflect.DeepEqual(io.sent, [][]string{{"esc"}}) {
		t.Fatalf("sent %v", io.sent)
	}
}

func TestAnswerClaudeDigitsWithArrowFallback(t *testing.T) {
	io := &fakeIO{screens: []string{permScreen}}
	req := answerBody{Action: "select", Index: 1, Fingerprint: fp(t, permScreen, "claude")}
	if s, b := answerDialog(context.Background(), io, claudeBlocked, nil, req, newAnswerDedupe(), time.Now()); s != http.StatusOK {
		t.Fatalf("%d %v", s, b)
	}
	if !reflect.DeepEqual(io.sent, [][]string{{"2"}}) {
		t.Fatalf("digit: sent %v", io.sent)
	}
	// herdr refuses the digit (validated before writing): arrows are used instead.
	io = &fakeIO{screens: []string{permScreen}, reject: func(k []string) error {
		if k[0] == "2" {
			return &herdr.Error{Code: "invalid_params", Message: "unsupported key 2"}
		}
		return nil
	}}
	if s, b := answerDialog(context.Background(), io, claudeBlocked, nil, req, newAnswerDedupe(), time.Now()); s != http.StatusOK {
		t.Fatalf("%d %v", s, b)
	}
	if !reflect.DeepEqual(io.sent, [][]string{{"down", "enter"}}) {
		t.Fatalf("fallback: sent %v", io.sent)
	}
}

func TestAnswerOnlyWhileWaiting(t *testing.T) {
	io := &fakeIO{screens: []string{permScreen}}
	idle := claudeBlocked
	idle.Status = herdr.StatusIdle
	s, b := answerDialog(context.Background(), io, idle, nil,
		answerBody{Action: "select", Index: 0, Fingerprint: fp(t, permScreen, "claude")}, newAnswerDedupe(), time.Now())
	if m, _ := b.(map[string]any); s != http.StatusConflict || m["code"] != "no_dialog" || io.reads != 0 || len(io.sent) != 0 {
		t.Fatalf("idle agent: %d %v reads=%d", s, b, io.reads)
	}
	// A working Pi agent with an open questionnaire is waiting.
	ask := &transcript.Ask{ToolCallID: "q", Questions: []transcript.AskQuestion{{Question: "Pick", Options: []transcript.AskOption{{Label: "LRU"}, {Label: "TTL"}}}}}
	screen := "\n Pick\n ❯ 1. LRU\n   2. TTL\n   3. Type something.\n"
	working := herdr.AgentInfo{PaneID: "w:p3", Agent: "pi", Status: herdr.StatusWorking}
	d, err := currentDialog(context.Background(), &fakeIO{screens: []string{screen}}, working, ask)
	if err != nil || d == nil || d.Source != "transcript+screen" {
		t.Fatalf("working+ask: %+v %v", d, err)
	}
	io = &fakeIO{screens: []string{screen}}
	if s, b := answerDialog(context.Background(), io, working, ask, answerBody{Action: "select", Index: 1, Fingerprint: d.Fingerprint}, newAnswerDedupe(), time.Now()); s != http.StatusOK {
		t.Fatalf("answer ask: %d %v", s, b)
	}
	if !reflect.DeepEqual(io.sent, [][]string{{"down", "enter"}}) {
		t.Fatalf("ask keys %v", io.sent)
	}
}

func TestAnswerValidation(t *testing.T) {
	io := &fakeIO{screens: []string{gateScreen}}
	good := fp(t, gateScreen, "pi")
	for _, b := range []answerBody{
		{Action: "type", Index: 0, Fingerprint: good},
		{Action: "select", Index: 0},
		{Action: "select", Index: 7, Fingerprint: good},
		{Action: "toggle", Index: 0, Fingerprint: good},
	} {
		if s, resp := answerDialog(context.Background(), io, piBlocked, nil, b, newAnswerDedupe(), time.Now()); s != http.StatusBadRequest {
			t.Errorf("%+v: got %d %v", b, s, resp)
		}
	}
	if len(io.sent) != 0 {
		t.Fatalf("sent %v", io.sent)
	}
	// Every key an answer can produce is in the herdr whitelist.
	for _, k := range []string{"up", "down", "enter", "space", "esc", "1", "9", "y", "n"} {
		if !herdr.ValidKey(k) {
			t.Errorf("answer key %q is not whitelisted", k)
		}
	}
}

func TestAnswerRequiresJSONAndSameOrigin(t *testing.T) {
	s := New(Config{AllowLogins: []string{"me@example.com"}, AllowHosts: testHosts, ReadLines: 100}, nil, nil)
	h := s.Handler()
	post := func(ct, site string) int {
		r := httptest.NewRequest("POST", "/api/agents/x/answer", strings.NewReader(`{"action":"cancel","fingerprint":"x"}`))
		r.Header.Set("Tailscale-User-Login", "me@example.com")
		r.Header.Set("Content-Type", ct)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
			r.Header.Set("Origin", "https://evil.example")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := post("text/plain", ""); c != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", c)
	}
	if c := post("application/json", "cross-site"); c != http.StatusForbidden {
		t.Errorf("cross-site: %d", c)
	}
	// Unauthenticated callers never reach the handler.
	r := httptest.NewRequest("POST", "/api/agents/x/answer", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no identity: %d", w.Code)
	}
	if w := do(s, "GET", "/api/agents/x/dialog", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("dialog without identity: %d", w.Code)
	}
}

func TestDialogHostRoutes(t *testing.T) {
	s := newTest()
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/hosts/nope/agents/x/dialog", ""},
		{"POST", "/api/hosts/nope/agents/x/answer", `{"action":"cancel","fingerprint":"f"}`},
	} {
		if w := do(s, c.method, c.path, nil, c.body); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "unknown_host") {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
}

const claudeMultiScreen = `
←  ☐ Investigate  ☐ Output  ✔ Submit  →

What should be investigated?

❯ 1. [ ] A
         Option A
  2. [ ] B
         Option B
  3. [ ] Type something
     Next
──────────────────────────────────────────────────────────────
  4. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
`

func multiAsk() *transcript.Ask {
	return &transcript.Ask{ToolCallID: "q", Questions: []transcript.AskQuestion{{
		Header: "Investigate", Question: "What should be investigated?", MultiSelect: true,
		Options: []transcript.AskOption{{Label: "A"}, {Label: "B"}},
	}}}
}

// The incident: herdr said "done" while a multi-select was open. The
// question must still be found, answerable, and must stop free text.
func TestClaudeMultiSelectWhileDone(t *testing.T) {
	done := herdr.AgentInfo{PaneID: "w:p1", Agent: "claude", Status: herdr.StatusDone}
	ask := multiAsk()
	io := &fakeIO{screens: []string{claudeMultiScreen}}
	d, err := currentDialog(context.Background(), io, done, ask)
	if err != nil || d == nil || !d.Multi || !d.Answerable {
		t.Fatalf("done+ask: %+v %v", d, err)
	}
	if err := promptGuard(context.Background(), io, done, ask); err != errDialogOpen {
		t.Fatalf("prompt not refused: %v", err)
	}
	// Toggle B, then commit with the keys computed from the highlight.
	if s, b := answerDialog(context.Background(), io, done, ask, answerBody{Action: "toggle", Index: 1, Fingerprint: d.Fingerprint}, newAnswerDedupe(), time.Now()); s != http.StatusOK {
		t.Fatalf("toggle: %d %v", s, b)
	}
	if s, b := answerDialog(context.Background(), io, done, ask, answerBody{Action: "submit", Fingerprint: d.Fingerprint}, newAnswerDedupe(), time.Now()); s != http.StatusOK {
		t.Fatalf("submit: %d %v", s, b)
	}
	want := [][]string{{"down", "space"}, {"down", "down", "down", "enter"}}
	if !reflect.DeepEqual(io.sent, want) {
		t.Fatalf("sent %v, want %v", io.sent, want)
	}
}

func TestPromptGuardAllowsStaleAndNoAsk(t *testing.T) {
	done := herdr.AgentInfo{PaneID: "w:p1", Agent: "claude", Status: herdr.StatusDone}
	// No transcript question: nothing is read, the prompt goes through.
	io := &fakeIO{screens: []string{claudeMultiScreen}}
	if err := promptGuard(context.Background(), io, done, nil); err != nil || io.reads != 0 {
		t.Fatalf("no ask: %v reads=%d", err, io.reads)
	}
	// A question the transcript calls open on a finished agent whose screen
	// shows only the prompt box is stale: do not wedge prompts forever.
	io = &fakeIO{screens: []string{"● Done.\n❯ \n"}}
	if err := promptGuard(context.Background(), io, done, multiAsk()); err != nil {
		t.Fatalf("stale ask refused prompts: %v", err)
	}
}

func TestResetOnlyLeavesInsertMode(t *testing.T) {
	done := herdr.AgentInfo{PaneID: "w:p1", Agent: "claude", Status: herdr.StatusDone}
	insert := strings.Replace(claudeMultiScreen, "❯ 1. [ ] A", "  1. [ ] A", 1)
	insert = strings.Replace(insert, "  3. [ ] Type something", "❯ 3. [ ] typed text", 1) + "\n-- INSERT --\n"
	io := &fakeIO{screens: []string{insert}}
	d, _ := currentDialog(context.Background(), io, done, multiAsk())
	if d == nil || !d.Insert {
		t.Fatalf("insert not seen: %+v", d)
	}
	if s, _ := answerDialog(context.Background(), io, done, multiAsk(), answerBody{Action: "toggle", Index: 0, Fingerprint: d.Fingerprint}, newAnswerDedupe(), time.Now()); s != http.StatusBadRequest || len(io.sent) != 0 {
		t.Fatalf("toggle in INSERT mode: %d sent=%v", s, io.sent)
	}
	if s, b := answerDialog(context.Background(), io, done, multiAsk(), answerBody{Action: "reset", Fingerprint: d.Fingerprint}, newAnswerDedupe(), time.Now()); s != http.StatusOK || !reflect.DeepEqual(io.sent, [][]string{{"esc"}}) {
		t.Fatalf("reset: %d %v sent=%v", s, b, io.sent)
	}
	// Out of INSERT mode a reset is refused: Esc would cancel the question.
	io = &fakeIO{screens: []string{claudeMultiScreen}}
	d, _ = currentDialog(context.Background(), io, done, multiAsk())
	if s, _ := answerDialog(context.Background(), io, done, multiAsk(), answerBody{Action: "reset", Fingerprint: d.Fingerprint}, newAnswerDedupe(), time.Now()); s != http.StatusBadRequest || len(io.sent) != 0 {
		t.Fatalf("reset outside INSERT: %d sent=%v", s, io.sent)
	}
}

package state

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/herdr-phone/internal/herdr"
	"github.com/jhjaggars/herdr-phone/internal/push"
	"github.com/jhjaggars/herdr-phone/internal/transcript"
)

func TestBuildNotificationBlockedShowsPendingTool(t *testing.T) {
	a := &herdr.AgentInfo{Name: "fixer", PaneID: "wS:p1", Status: herdr.StatusBlocked,
		Worktree: &herdr.WorktreeInfo{RepoName: "herdr-phone", IsLinked: true, Branch: "notify"}}
	latest := &transcript.Latest{Text: "Running tests.",
		Pending: []transcript.PendingTool{{Tool: "bash", Args: "go test ./..."}}}
	kind, msg := buildNotification(a, "", latest, 200)
	if kind != "blocked" || msg.Title != "fixer needs input" {
		t.Fatalf("kind=%q title=%q", kind, msg.Title)
	}
	if msg.Message != "herdr-phone ⑂ notify\nwants to run bash: go test ./..." {
		t.Fatalf("message = %q", msg.Message)
	}
}

func TestBuildNotificationDoneQuestionAndNoExcerpt(t *testing.T) {
	a := &herdr.AgentInfo{Name: "fixer", PaneID: "wS:p1", Status: herdr.StatusDone, CWD: "/home/u/code/repo"}
	latest := &transcript.Latest{Text: "All green.\n\nShould I push the branch?"}
	_, msg := buildNotification(a, "", latest, 200)
	if msg.Title != "fixer has a question" || msg.Message != "repo\nAll green. Should I push the branch?" {
		t.Fatalf("title=%q message=%q", msg.Title, msg.Message)
	}
	// Excerpts disabled: no message text at all.
	_, msg = buildNotification(a, "", nil, 0)
	if msg.Title != "fixer is done" || msg.Message != "repo\ncompleted work" {
		t.Fatalf("title=%q message=%q", msg.Title, msg.Message)
	}
}

func TestNotifyTransitionPostsJSONWithDeepLink(t *testing.T) {
	var mu sync.Mutex
	var got []ntfyMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m ntfyMessage
		if err := json.Unmarshal(body, &m); err != nil {
			t.Errorf("bad body %q: %v", body, err)
		}
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	defer srv.Close()

	session := filepath.Join(t.TempDir(), "s.jsonl")
	line := `{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"Done: 3 files changed."}]}}` + "\n"
	if err := os.WriteFile(session, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{dispatch: &Dispatcher{Ntfy: &Notifier{BaseURL: srv.URL, Topic: "t", ClickURL: "https://box.ts.net/",
		MinInterval: time.Minute, ExcerptChars: 200}}}
	a := &herdr.AgentInfo{Name: "fixer", PaneID: "wS:p1", Status: herdr.StatusDone, CWD: "/x/repo",
		AgentSession: &herdr.AgentSessionInfo{Kind: "path", Value: session}}
	m.notifyTransition(a)
	m.notifyTransition(a) // throttled

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("got %d posts, want 1", len(got))
	}
	g := got[0]
	if g.Topic != "t" || g.Click != "https://box.ts.net/#/agent/wS:p1" ||
		!strings.Contains(g.Message, "Done: 3 files changed.") {
		t.Fatalf("post = %+v", g)
	}
}

type fakePusher struct {
	mu   sync.Mutex
	keys []string
	msgs []push.Message
	opts []push.Options
}

func (f *fakePusher) Active() bool      { return true }
func (f *fakePusher) ExcerptChars() int { return 200 }
func (f *fakePusher) Notify(key string, msg push.Message, opts push.Options) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	f.msgs = append(f.msgs, msg)
	f.opts = append(f.opts, opts)
}

type fakeViewer map[string]bool

func (v fakeViewer) Viewing(pane string) bool { return v[pane] }

func TestNotifyTransitionWebPush(t *testing.T) {
	session := filepath.Join(t.TempDir(), "s.jsonl")
	line := `{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"Should I push?"}]}}` + "\n"
	if err := os.WriteFile(session, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	fp := &fakePusher{}
	m := &Manager{dispatch: &Dispatcher{}} // ntfy off: push alone must still fire
	m.SetPush(fp, fakeViewer{"wS:p2": true})
	done := &herdr.AgentInfo{Name: "fixer", PaneID: "wS:p1", Status: herdr.StatusDone, CWD: "/x/repo",
		AgentSession: &herdr.AgentSessionInfo{Kind: "path", Value: session}}
	m.notifyTransition(done)
	blocked := &herdr.AgentInfo{Name: "other", PaneID: "wS:p3", Status: herdr.StatusBlocked}
	m.notifyTransition(blocked)
	viewed := &herdr.AgentInfo{Name: "viewed", PaneID: "wS:p2", Status: herdr.StatusBlocked}
	m.notifyTransition(viewed) // suppressed: on screen
	idle := &herdr.AgentInfo{Name: "fixer", PaneID: "wS:p1", Status: herdr.StatusIdle}
	m.notifyTransition(idle) // not a notifying transition

	if len(fp.msgs) != 2 {
		t.Fatalf("got %d pushes, want 2: %+v", len(fp.msgs), fp.msgs)
	}
	g := fp.msgs[0]
	if fp.keys[0] != "wS:p1/done" || g.Title != "fixer has a question" || g.Body != "repo\nShould I push?" ||
		g.URL != "/#/agent/wS:p1" || g.Tag != "wS:p1" || g.Renotify || fp.opts[0].Urgency != "normal" {
		t.Fatalf("done push = %q %+v %+v", fp.keys[0], g, fp.opts[0])
	}
	if b := fp.msgs[1]; !b.Renotify || fp.opts[1].Urgency != "high" || b.Title != "other needs input" || fp.opts[1].Topic == "" {
		t.Fatalf("blocked push = %+v %+v", b, fp.opts[1])
	}
}

func TestForegroundSuppressionAppliesToNtfy(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
	}))
	defer srv.Close()
	m := &Manager{dispatch: &Dispatcher{Ntfy: &Notifier{BaseURL: srv.URL, Topic: "t", MinInterval: time.Minute}}}
	m.SetPush(nil, fakeViewer{"wS:p1": true})
	m.notifyTransition(&herdr.AgentInfo{Name: "a", PaneID: "wS:p1", Status: herdr.StatusDone})
	m.notifyTransition(&herdr.AgentInfo{Name: "b", PaneID: "wS:p9", Status: herdr.StatusDone})
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("got %d ntfy posts, want 1 (viewed pane suppressed)", n)
	}
}

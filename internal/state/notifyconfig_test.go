package state

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/transcript"
)

func TestParseKinds(t *testing.T) {
	k, err := ParseKinds(" Blocked, question ")
	if err != nil || len(k) != 2 || !k[KindBlocked] || !k[KindQuestion] {
		t.Fatalf("parse = %v %v", k, err)
	}
	if k, err := ParseKinds("none"); err != nil || len(k) != 0 {
		t.Fatalf("none = %v %v", k, err)
	}
	for _, bad := range []string{"", " , ", "blocked,finished"} {
		if _, err := ParseKinds(bad); err == nil {
			t.Errorf("ParseKinds(%q) accepted", bad)
		}
	}
}

// latestText is a LatestFunc answering text, counting its calls.
func latestText(text string, calls *int) LatestFunc {
	return func(context.Context) *transcript.Latest {
		*calls++
		return &transcript.Latest{Text: text}
	}
}

// noExcerptPusher is a pusher configured to send no message text.
type noExcerptPusher struct{ fakePusher }

func (*noExcerptPusher) ExcerptChars() int { return 0 }

// Muting "done" keeps "blocked" and questions; the question is told from
// the last message even with excerpts off, and a dropped "done" does not
// hold the agent.
func TestNotifyKinds(t *testing.T) {
	fp := &noExcerptPusher{}
	d := &Dispatcher{Push: fp, Kinds: map[string]bool{KindBlocked: true, KindQuestion: true}}
	n := 0
	done := &herdr.AgentInfo{Name: "w", ID: "desk/wS:p1", PaneID: "wS:p1", Status: herdr.StatusDone}
	d.Notify(done, LocalRef(done), latestText("All green, branch committed.", &n))
	if len(fp.msgs) != 0 || n != 1 {
		t.Fatalf("plain done sent %d (latest read %d times)", len(fp.msgs), n)
	}
	// Not held by the dropped one: a question right after still goes out,
	// long enough that an excerpt would have cut the question mark off.
	q := "Tests pass. " + strings.Repeat("More detail. ", 40) + "Should I push the branch?"
	d.Notify(done, LocalRef(done), latestText(q, &n))
	if len(fp.msgs) != 1 || fp.msgs[0].Title != "w has a question" || fp.msgs[0].Body != "wS:p1\nwaiting for your answer" {
		t.Fatalf("question = %+v", fp.msgs)
	}
	blocked := &herdr.AgentInfo{Name: "b", ID: "desk/wS:p2", PaneID: "wS:p2", Status: herdr.StatusBlocked}
	d.Notify(blocked, LocalRef(blocked), nil)
	if len(fp.msgs) != 2 || fp.msgs[1].Title != "b needs input" {
		t.Fatalf("blocked = %+v", fp.msgs)
	}

	// Only "done": blocked agents and questions are silent.
	fp2 := &fakePusher{}
	d2 := &Dispatcher{Push: fp2, Kinds: map[string]bool{KindDone: true}}
	if d2.Wants(blocked, LocalRef(blocked)) {
		t.Fatal("blocked wanted with only done on")
	}
	d2.Notify(done, LocalRef(done), latestText("Push?", &n))
	other := &herdr.AgentInfo{Name: "o", ID: "desk/wS:p3", PaneID: "wS:p3", Status: herdr.StatusDone}
	d2.Notify(other, LocalRef(other), latestText("Done.", &n))
	if len(fp2.msgs) != 1 || fp2.msgs[0].Title != "o is done" {
		t.Fatalf("done only = %+v", fp2.msgs)
	}

	// Nothing: no transition is even wanted.
	d3 := &Dispatcher{Push: &fakePusher{}, Kinds: map[string]bool{}}
	if d3.Wants(done, LocalRef(done)) || d3.Wants(blocked, LocalRef(blocked)) {
		t.Fatal("wanted with no kinds")
	}
}

// digestClock drives a Dispatcher's digest timer by hand.
type digestClock struct {
	mu    sync.Mutex
	fire  func()
	delay time.Duration
	armed int
}

func (c *digestClock) after(d time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fire, c.delay = f, d
	c.armed++
}

func (c *digestClock) flush(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	f := c.fire
	c.fire = nil
	c.mu.Unlock()
	if f == nil {
		t.Fatal("no digest pending")
	}
	f()
}

func TestDoneDigest(t *testing.T) {
	fp := &fakePusher{}
	viewed := seenViewer{}
	clk := &digestClock{}
	now := time.Unix(1_000_000, 0)
	d := &Dispatcher{Push: fp, Viewer: viewed, DoneDigest: 10 * time.Minute, ShowHost: true, after: clk.after, now: func() time.Time { return now }}
	agent := func(name, pane, repo string, st herdr.AgentStatus) *herdr.AgentInfo {
		return &herdr.AgentInfo{Name: name, ID: "kube/" + pane, Host: "kube", PaneID: pane, Status: st,
			Worktree: &herdr.WorktreeInfo{RepoName: repo}}
	}
	n := 0
	a := agent("fix-tests", "w1:p1", "comebye", herdr.StatusDone)
	b := agent("docs", "w2:p1", "comebye", herdr.StatusDone)
	c := agent("seen", "w3:p1", "other", herdr.StatusDone)
	e := agent("back-to-work", "w4:p1", "other", herdr.StatusDone)
	for _, x := range []*herdr.AgentInfo{a, b, c, e} {
		d.Notify(x, PeerRef(x), latestText("Done.", &n))
		now = now.Add(time.Second)
	}
	// Questions and blocked agents go out at once.
	q := agent("asker", "w5:p1", "comebye", herdr.StatusDone)
	d.Notify(q, PeerRef(q), latestText("Merge it?", &n))
	bl := agent("stuck", "w6:p1", "comebye", herdr.StatusBlocked)
	d.Notify(bl, PeerRef(bl), nil)
	if len(fp.msgs) != 2 || fp.msgs[0].Title != "asker has a question" || fp.msgs[1].Title != "stuck needs input" {
		t.Fatalf("immediate = %+v", fp.msgs)
	}
	if clk.armed != 1 || clk.delay != 10*time.Minute {
		t.Fatalf("timer armed %d times for %v, want once for 10m (the window does not slide)", clk.armed, clk.delay)
	}
	// Opened in the app, or back to work: left out.
	viewed["kube/w3:p1"] = now
	working := *e
	working.Status = herdr.StatusWorking
	d.Observe(&working, PeerRef(&working))
	clk.flush(t)
	if len(fp.msgs) != 3 {
		t.Fatalf("pushes = %d, want 3", len(fp.msgs))
	}
	g := fp.msgs[2]
	if g.Title != "2 agents finished" || g.Body != "fix-tests · kube · comebye\ndocs · kube · comebye" || g.URL != "/" || g.Tag != digestKey || fp.keys[2] != digestKey+"/done" || fp.opts[2].Urgency != "low" {
		t.Fatalf("digest = %q %+v %+v", fp.keys[2], g, fp.opts[2])
	}
	// Each agent stays held as usual: finishing again is not re-sent.
	d.Notify(a, PeerRef(a), latestText("Done.", &n))
	if clk.fire != nil {
		t.Fatal("a held agent was queued again")
	}

	// A digest of one is that agent's own notification.
	f := agent("solo", "w7:p1", "comebye", herdr.StatusDone)
	d.Notify(f, PeerRef(f), latestText("Finished the refactor.", &n))
	clk.flush(t)
	if s := fp.msgs[len(fp.msgs)-1]; s.Title != "solo is done" || s.URL != "/#/agent/kube/w7:p1" || !strings.Contains(s.Body, "Finished the refactor.") {
		t.Fatalf("single = %+v", s)
	}

	// Long digests are cut.
	var many []digestEntry
	for i := range 9 {
		many = append(many, digestEntry{a: *agent("w", "p", "r", herdr.StatusDone), at: now.Add(time.Duration(i))})
	}
	if m := buildDigest(many, false); m.Title != "9 agents finished" || strings.Count(m.Message, "\n") != maxDigestLines-1 || !strings.HasSuffix(m.Message, "and 4 more") {
		t.Fatalf("long digest = %+v", m)
	}
}

func TestDoneDigestNtfy(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	clk := &digestClock{}
	d := &Dispatcher{Ntfy: &Notifier{BaseURL: srv.URL, Topic: "t", ClickURL: "https://hub.ts.net", MinInterval: time.Minute},
		DoneDigest: time.Minute, after: clk.after}
	for _, p := range []string{"w1:p1", "w2:p1"} {
		a := &herdr.AgentInfo{Name: p, ID: "desk/" + p, PaneID: p, Status: herdr.StatusDone}
		d.Notify(a, LocalRef(a), nil)
	}
	clk.flush(t)
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"title":"2 agents finished"`) || !strings.Contains(bodies[0], `"click":"https://hub.ts.net/"`) {
		t.Fatalf("ntfy = %v", bodies)
	}
}

// timers collects the functions a Dispatcher schedules, to run by hand.
type timers struct {
	mu  sync.Mutex
	fns []func()
	ds  []time.Duration
}

func (c *timers) after(d time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fns, c.ds = append(c.fns, f), append(c.ds, d)
}

// fire runs every scheduled function.
func (c *timers) fire() {
	c.mu.Lock()
	fns := c.fns
	c.fns = nil
	c.mu.Unlock()
	for _, f := range fns {
		f()
	}
}

// A "needs input" waits out BlockedDelay and goes only if the agent is
// still blocked; questions and other kinds are not delayed.
func TestBlockedDelay(t *testing.T) {
	fp := &fakePusher{}
	viewing := fakeViewer{}
	clk := &timers{}
	d := &Dispatcher{Push: fp, Viewer: viewing, BlockedDelay: 15 * time.Second, after: clk.after}
	at := func(name string, st herdr.AgentStatus) *herdr.AgentInfo {
		return &herdr.AgentInfo{Name: name, ID: "desk/" + name, PaneID: name, Status: st}
	}
	transition := func(a *herdr.AgentInfo) {
		d.Observe(a, LocalRef(a))
		d.Notify(a, LocalRef(a), nil)
	}

	// Still blocked after the delay: sent then.
	transition(at("stuck", herdr.StatusBlocked))
	transition(at("stuck", herdr.StatusBlocked)) // a wait under way is not restarted
	if len(fp.msgs) != 0 || len(clk.ds) != 1 || clk.ds[0] != 15*time.Second {
		t.Fatalf("sent at once (%d) or timers %v", len(fp.msgs), clk.ds)
	}
	clk.fire()
	if len(fp.msgs) != 1 || fp.msgs[0].Title != "stuck needs input" {
		t.Fatalf("after the delay = %+v", fp.msgs)
	}

	// Worked through it, or finished, or went idle: nothing.
	for _, next := range []herdr.AgentStatus{herdr.StatusWorking, herdr.StatusDone, herdr.StatusIdle} {
		name := "brief-" + string(next)
		transition(at(name, herdr.StatusBlocked))
		d.Observe(at(name, next), LocalRef(at(name, next)))
	}
	clk.fire()
	if len(fp.msgs) != 1 {
		t.Fatalf("a block that ended was sent: %+v", fp.msgs)
	}

	// Blocked again after working: a new wait, which an older timer
	// cannot cut short.
	flip := at("flip", herdr.StatusBlocked)
	transition(flip)
	d.Observe(at("flip", herdr.StatusWorking), LocalRef(flip))
	stale := clk.fns
	clk.fns = nil
	transition(flip)
	for _, f := range stale {
		f()
	}
	if len(fp.msgs) != 1 {
		t.Fatal("the first wait's timer sent the second block early")
	}
	clk.fire()
	if len(fp.msgs) != 2 || fp.msgs[1].Title != "flip needs input" {
		t.Fatalf("second block = %+v", fp.msgs)
	}

	// Opened in the app during the wait: suppressed as usual.
	transition(at("looked", herdr.StatusBlocked))
	viewing["desk/looked"] = true
	clk.fire()
	if len(fp.msgs) != 2 {
		t.Fatal("sent while on screen")
	}

	// "done" is not delayed.
	transition(at("finished", herdr.StatusDone))
	if len(fp.msgs) != 3 || fp.msgs[2].Title != "finished is done" {
		t.Fatalf("done = %+v", fp.msgs)
	}
}

// Quiet agents (a session asked whether it is safe to remove) get no
// notification for answering.
func TestNotifyQuiet(t *testing.T) {
	fp := &fakePusher{}
	d := &Dispatcher{Push: fp, Quiet: func(key string) bool { return key == "desk/wS:p1" }}
	asked := &herdr.AgentInfo{Name: "a", ID: "desk/wS:p1", PaneID: "wS:p1", Status: herdr.StatusDone}
	other := &herdr.AgentInfo{Name: "o", ID: "desk/wS:p2", PaneID: "wS:p2", Status: herdr.StatusDone}
	if d.Wants(asked, LocalRef(asked)) || !d.Wants(other, LocalRef(other)) {
		t.Fatal("Quiet not applied to the asked agent only")
	}
}

// A plan's push is not held back by an unanswered notification for an
// agent, has its own key per plan, and follows the kinds switch.
func TestNotifyPlan(t *testing.T) {
	fp := &fakePusher{}
	d := &Dispatcher{Push: fp}
	w := &herdr.AgentInfo{Name: "w", ID: "desk/wS:p1", PaneID: "wS:p1", Status: herdr.StatusDone}
	n := 0
	d.Notify(w, LocalRef(w), latestText("Done.", &n)) // leaves the agent held
	d.NotifyPlan("hab", "Plan done: Add foo", "2 of 2 workers finished.")
	d.NotifyPlan("hcd", "Plan done: Other", "1 of 1 workers finished.")
	if len(fp.msgs) != 3 || fp.keys[1] != "plan/hab/plan" || fp.keys[2] != "plan/hcd/plan" || fp.msgs[1].Title != "Plan done: Add foo" {
		t.Fatalf("pushes = %v %+v", fp.keys, fp.msgs)
	}
	off := &fakePusher{}
	(&Dispatcher{Push: off, Kinds: map[string]bool{KindDone: true}}).NotifyPlan("hab", "t", "b")
	if len(off.msgs) != 0 {
		t.Fatal("plan push with the kind off")
	}
	if k, err := ParseKinds("done,plan"); err != nil || !k[KindPlan] {
		t.Fatalf("plan kind = %v %v", k, err)
	}
}

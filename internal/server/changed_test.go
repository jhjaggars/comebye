package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/park"
	"github.com/jhjaggars/comebye/internal/provision"
	"github.com/jhjaggars/comebye/internal/state"
)

// storeKinds is every Server field that keeps data on disk, with the
// "changed" kind its writes send (changed.go). A store the app never shows
// says why instead, starting with "-". TestChangedEveryStoreHasAKind fails
// for a new store missing here, so it is never silently left to a poll.
var storeKinds = map[string]string{
	"ledger":    state.ChangedLedger,
	"roles":     state.ChangedLedger, // GET /api/ledger carries the roles
	"events":    state.ChangedLedger, // the Status board\'s "the hub did" lane
	"todos":     state.ChangedTodos,
	"approvals": state.ChangedTodos, // a to-do shows its approval's state
	"pages":     state.ChangedPages,
	"pageCmts":  state.ChangedPages,
	"parks":     state.ChangedParked,
	"rollout":   state.ChangedReleases,
	"usage":     state.ChangedUsage,
	"pjobs":     state.ChangedJobPrefix + "*",
	"hjobs":     state.ChangedJobPrefix + "*",

	"health":   "-a probe file that checks the state directory is writable",
	"pq":       "-the prompt queue: read when an agent's detail opens, not on a screen that stays open",
	"passkeys": "-read once when Settings opens; only the user changes them, from that screen",
	"typed":    "-marks on transcript lines; the transcript's own kind covers what it shows",
	"sent":     "-the hub's memory of what it told whom",
	"sweeps":   "-the sweep loop's schedule, read with the host's settings",
	"pseen":    "-what the hub already filed about provisioned hosts",
	"phosts":   state.ChangedJobPrefix + provHostsJobID,
	"wake":     state.ChangedJobPrefix + wakeJobID,
	"chats":    "-a page's chat helper; the agent's own kinds cover what it shows",
	"resets":   "-the coordinator reset record, read by `comebye coordinator status`",
	"askers":   "-who asked what, for routing answers",
	"kinds":    "-the agent kinds a host learned",
	"unrec":    "-unrecognised dialogs, kept for debugging",
	"pins":     "-Doc panel pins; the panel's own stream follows them",
	"settings": "-a host's agent settings: read on the screen that changes them",
	"prop":     "-park removals already proposed",
	"seen":     "-what the hub has seen of each agent",
}

// keepsData reports whether a field's type keeps data on disk: a struct
// with a path or dir, or one holding such a store.
func keepsData(ty reflect.Type) bool {
	if ty.Kind() == reflect.Pointer {
		ty = ty.Elem()
	}
	if ty.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		if (f.Name == "path" || f.Name == "dir") && f.Type.Kind() == reflect.String {
			return true
		}
		if f.Name == "store" && f.Type.Kind() == reflect.Pointer && keepsData(f.Type) {
			return true
		}
	}
	return false
}

func TestChangedEveryStoreHasAKind(t *testing.T) {
	st := reflect.TypeOf(Server{})
	seen := map[string]bool{}
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if !keepsData(f.Type) {
			continue
		}
		seen[f.Name] = true
		kind, ok := storeKinds[f.Name]
		if !ok {
			t.Errorf("Server.%s (%s) keeps data but has no \"changed\" kind: wire its write to Changed in changed.go and add it to storeKinds, or say there why the app never shows it", f.Name, f.Type)
			continue
		}
		if !strings.HasPrefix(kind, "-") && !state.ValidChangedKind(strings.TrimSuffix(kind, "*")+map[bool]string{true: "x"}[strings.HasSuffix(kind, "*")]) {
			t.Errorf("Server.%s: %q is not a kind the app knows", f.Name, kind)
		}
	}
	for name := range storeKinds {
		if !seen[name] {
			t.Errorf("storeKinds lists %s, which is no longer a store of the Server", name)
		}
	}
}

// changedRecorder collects the kinds sent on the browsers' stream.
type changedRecorder struct {
	ch     <-chan state.Event
	cancel func()
}

func recordChanged(t *testing.T, s *Server) *changedRecorder {
	t.Helper()
	ch, cancel := s.fleet.Changes().Subscribe()
	t.Cleanup(cancel)
	return &changedRecorder{ch: ch, cancel: cancel}
}

// drain forgets what was sent so far.
func (r *changedRecorder) drain() {
	for {
		select {
		case <-r.ch:
		case <-time.After(400 * time.Millisecond): // longer than the debounce: a trailing event is in
			return
		}
	}
}

// want waits for kind (a trailing "*" matches any id).
func (r *changedRecorder) want(t *testing.T, what, kind string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	var got []string
	for {
		select {
		case ev := <-r.ch:
			if ev.Type != "changed" {
				t.Fatalf("%s: event type %q on the changes stream", what, ev.Type)
			}
			if ev.Kind == kind || (strings.HasSuffix(kind, "*") && strings.HasPrefix(ev.Kind, strings.TrimSuffix(kind, "*"))) {
				return
			}
			got = append(got, ev.Kind)
		case <-deadline:
			t.Fatalf("%s: no changed{%s} within 2 s (got %v)", what, kind, got)
		}
	}
}

// TestChangedWritesEmitKind writes each store once and checks its kind goes
// out on the stream the browsers read.
func TestChangedWritesEmitKind(t *testing.T) {
	dir := t.TempDir()
	s := newServer(t, Config{ReadLines: 100, HostName: "kube", StateDir: dir}, nil, nil)
	if s.pjobs == nil { // a provisioner's stores: this hub has no provision tool
		s.pjobs = openProvJobs(dir)
		s.phosts = provision.OpenHostBook(dir)
		s.wireChanged()
	}
	rec := recordChanged(t, s)

	cases := []struct {
		store, kind string
		write       func() error
	}{
		{"ledger", state.ChangedLedger, func() error {
			_, err := s.ledger.Add(ledger.Entry{Host: "kube", RepoName: "proj", Branch: "feat/x", AgentID: "kube/w1:p1", PaneID: "w1:p1", Status: ledger.Running})
			return err
		}},
		{"roles", state.ChangedLedger, func() error { return s.roles.set("kube/w1:p2", Role{Role: RoleCoordinator, Name: "c", At: time.Now()}) }},
		{"todos", state.ChangedTodos, func() error {
			_, _, err := s.todos.add(Todo{Question: "Ship it?", Kind: "decision", CreatedBy: "kube/w1:p1"})
			return err
		}},
		{"todos (a release item)", state.ChangedReleases, func() error {
			_, _, err := s.todos.add(Todo{Question: "Release v9.9.9?", Kind: "release", CreatedBy: "hub", Release: &TodoRelease{Version: "v9.9.9", State: relPending}})
			return err
		}},
		{"events", state.ChangedLedger, func() error {
			s.events.mu.Lock()
			defer s.events.mu.Unlock()
			s.events.saveLocked()
			return nil
		}},
		{"approvals", state.ChangedTodos, func() error {
			s.approvals.mu.Lock()
			defer s.approvals.mu.Unlock()
			return s.approvals.saveLocked()
		}},
		{"pages", state.ChangedPages, func() error {
			_, _, err := s.pages.publish(pageIn{Kind: "md", Title: "A page", Body: []byte("# a")}, pageWho{user: true, by: "me"})
			return err
		}},
		{"pageCmts", state.ChangedPages, func() error {
			cs := s.commentsStore()
			cs.mu.Lock()
			defer cs.mu.Unlock()
			return cs.saveLocked()
		}},
		{"parks", state.ChangedParked, func() error {
			return s.parkStore().Add(park.Record{ID: park.NewID(), Name: "w", Kind: "claude", Host: "kube", PaneID: "w5:p1", Worktree: "/wt/w", ParkedAt: time.Now()})
		}},
		{"rollout", state.ChangedReleases, func() error { return s.rollout.save(&Rollout{ID: "ro_1", Version: "v9.9.9"}) }},
		{"deploy record", state.ChangedReleases, func() error {
			s.writeDeployRecord(DeployRecord{Version: "v9.9.9", State: depRunning, Started: time.Now()})
			return nil
		}},
		{"usage", state.ChangedUsage, func() error {
			s.usage.store.PeerFailed("peer1", "down")
			return s.usage.store.Save()
		}},
		{"pjobs", state.ChangedJobPrefix + "pj_1", func() error {
			s.pjobs.mu.Lock()
			defer s.pjobs.mu.Unlock()
			j := &pjob{ID: "pj_1", State: jobRunning}
			s.pjobs.jobs[j.ID] = j
			s.pjobs.saveLocked(j)
			return nil
		}},
		{"phosts", state.ChangedJobPrefix + provHostsJobID, func() error {
			s.phosts.Add("vm9", "pj_1", provision.Spec{Name: "vm9"}, "us-east-1", nil, provision.Estimate{})
			return nil
		}},
		{"wake", state.ChangedJobPrefix + wakeJobID, func() error {
			s.wake.begin("vm1", "prov", "me", "tap")
			return nil
		}},
		{"hjobs", state.ChangedJobPrefix + "hj_0123456789abcdef", func() error {
			s.hjobs.mu.Lock()
			defer s.hjobs.mu.Unlock()
			s.hjobs.jobs["hj_0123456789abcdef"] = &hubJob{ID: "hj_0123456789abcdef", Via: "prov", PeerID: "pj_1", State: jobRunning}
			s.hjobs.saveLocked()
			return nil
		}},
	}
	covered := map[string]bool{}
	for _, c := range cases {
		rec.drain()
		if err := c.write(); err != nil {
			t.Fatalf("%s: write: %v", c.store, err)
		}
		rec.want(t, c.store, c.kind)
		covered[c.store] = true
	}
	for name, kind := range storeKinds {
		if !strings.HasPrefix(kind, "-") && !covered[name] {
			t.Errorf("storeKinds says %s sends %s, but no case here writes it", name, kind)
		}
	}

	// A provisioner's job, relayed by the fleet, is the hub job that follows it.
	rec.drain()
	s.peerChanged("prov", state.ChangedJobPrefix+"pj_1")
	rec.want(t, "relayed provisioner job", state.ChangedJobPrefix+"hj_0123456789abcdef")
}

// TestChangedDebouncesBursts: twenty writes in a row are one event at once
// and one at the end, not twenty.
func TestChangedDebouncesBursts(t *testing.T) {
	s := newServer(t, Config{ReadLines: 100, HostName: "kube", StateDir: t.TempDir()}, nil, nil)
	rec := recordChanged(t, s)
	for i := 0; i < 20; i++ {
		if _, _, err := s.todos.add(Todo{Question: "q" + string(rune('a'+i)) + "?", Kind: "followup", CreatedBy: "kube/w1:p1"}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	timeout := time.After(time.Second)
loop:
	for {
		select {
		case ev := <-rec.ch:
			if ev.Kind == state.ChangedTodos {
				n++
			}
		case <-timeout:
			break loop
		}
	}
	if n < 1 || n > 2 {
		t.Fatalf("20 to-do writes in a burst sent %d changed{todos}, want 1 or 2", n)
	}
}

// TestChangedReachesTheEventStream: a write shows on /api/events as an
// "event: changed" with its kind.
func TestChangedReachesTheEventStream(t *testing.T) {
	s := newServer(t, Config{ReadLines: 100, HostName: "kube", StateDir: t.TempDir()}, nil, nil)
	lines := openEventStream(t, s)
	if _, _, err := s.todos.add(Todo{Question: "Seen at once?", Kind: "decision", CreatedBy: "kube/w1:p1"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case l := <-lines:
			if l == `data: {"kind":"todos"}` {
				return
			}
		case <-deadline:
			t.Fatal("no changed{todos} on /api/events within 3 s")
		}
	}
}

// TestTranscriptWatch: a read of a transcript watches its file; a write to
// the file sends transcript:<agent id>; a watch nobody renews ends.
func TestTranscriptWatch(t *testing.T) {
	s := newServer(t, Config{ReadLines: 100, HostName: "kube", StateDir: t.TempDir()}, nil, nil)
	s.twatch.every, s.twatch.keep = 50*time.Millisecond, time.Second
	rec := recordChanged(t, s)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.watchTranscript(herdr.AgentInfo{PaneID: "w1:p1"}, path)
	rec.drain()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{\"more\":1}\n")
	f.Close()
	rec.want(t, "transcript write", state.ChangedTranscriptPrefix+"kube/w1:p1")
	time.Sleep(1500 * time.Millisecond) // past keep, unrenewed
	s.twatch.mu.Lock()
	n, running := len(s.twatch.m), s.twatch.running
	s.twatch.mu.Unlock()
	if n != 0 || running {
		t.Fatalf("after its keep the watch is still there (files %d, running %v)", n, running)
	}
}

// openEventStream opens /api/events as a browser does and sends its lines.
func openEventStream(t *testing.T, s *Server) <-chan string {
	t.Helper()
	srv := httptest.NewServer(s.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); resp.Body.Close(); srv.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/events: %s", resp.Status)
	}
	out := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			select {
			case out <- sc.Text():
			default:
			}
		}
	}()
	return out
}

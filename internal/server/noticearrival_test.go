package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jhjaggars/comebye/internal/fleet"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
)

// A notice is never delivered twice. herdr can fail agent.prompt after the
// text and Enter were written (a timeout, a dropped socket); the hub used
// to count that as a failed attempt and send the notice again 15 s later.
// The fake herdr here behaves that way: it appends the text to the target's
// session transcript, as the harness would, and then returns an error.

// arrivingThenError scripts agent.prompt: the text lands in the transcript
// file of the (single) target, and the call fails. Later calls succeed.
func arrivingThenError(fh *fakeLaunchHerdr, path string) {
	onPrompt(fh, map[int]promptAnswer{
		1: {err: &herdr.Error{Code: "internal_error", Message: "socket closed after write"}},
		0: {status: herdr.StatusWorking},
	}, func(f *fakeLaunchHerdr) {
		if len(f.calls) == 0 {
			return
		}
		last := f.calls[len(f.calls)-1]
		if last.Method != "agent.prompt" {
			return
		}
		text, _ := last.Params["text"].(string)
		fp, _ := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		defer fp.Close()
		fp.WriteString(`{"type":"user","message":{"content":"` + strings.ReplaceAll(text, `"`, `\"`) + `"}}` + "\n")
	})
}

func TestCallbackNotRepeatedWhenSendErrorsAfterArrival(t *testing.T) {
	for _, kind := range []string{"claude", "pi"} {
		t.Run(kind, func(t *testing.T) {
			s, fh, _ := coordServer(t)
			s.noticeArriveWait = 20 * time.Millisecond
			path := filepath.Join(t.TempDir(), "session.jsonl")
			os.WriteFile(path, []byte(`{"type":"user","message":{"content":"earlier"}}`+"\n"), 0o600)
			arrivingThenError(fh, path)
			coord := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
			coord.Agent = kind
			coord.AgentSession = &herdr.AgentSessionInfo{Kind: "path", Value: path}
			e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}, coord)

			s.deliverCallbacks([]herdr.AgentInfo{coord})
			waitFor(t, func() bool { return promptCount(fh) >= 1 })
			// The attempt errored but the text arrived: it is delivered.
			waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })

			// Later fleet updates (past the retry spacing) send nothing more.
			s.coord.mu.Lock()
			s.coord.calling = map[string]time.Time{}
			s.coord.mu.Unlock()
			s.deliverCallbacks([]herdr.AgentInfo{coord})
			time.Sleep(100 * time.Millisecond)
			if n := promptCount(fh); n != 1 {
				t.Fatalf("prompts = %d, want 1", n)
			}
		})
	}
}

// An error where the text did not arrive is still retried.
func TestCallbackRetriedWhenSendErrorsWithoutArrival(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.noticeArriveWait = 20 * time.Millisecond
	path := filepath.Join(t.TempDir(), "session.jsonl")
	os.WriteFile(path, []byte("{}\n"), 0o600)
	onPrompt(fh, map[int]promptAnswer{
		1: {err: &herdr.Error{Code: "internal_error", Message: "nope"}},
		0: {status: herdr.StatusWorking},
	}, nil)
	coord := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	coord.AgentSession = &herdr.AgentSessionInfo{Kind: "path", Value: path}
	e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}, coord)
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	waitFor(t, func() bool { return promptCount(fh) >= 1 })
	time.Sleep(100 * time.Millisecond)
	s.coord.mu.Lock()
	s.coord.calling = map[string]time.Time{}
	s.coord.mu.Unlock()
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	waitFor(t, func() bool { g, _ := s.ledger.Get(e.ID); return g.Callback.State == ledger.CallbackDelivered })
	if n := promptCount(fh); n != 2 {
		t.Fatalf("prompts = %d, want 2", n)
	}
}

var _ = context.Background

// A target on a peer: the hub asks the peer's noticeseen route, which
// answers from the peer's own transcript.
type noticePeer struct {
	mu         sync.Mutex
	transcript string
	arrive     bool // the first prompt's text lands, then the call errors
	prompts    int
	probes     int
}

func (p *noticePeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/events":
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: agents\ndata: {\"type\":\"agents\",\"agents\":[]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case "/api/agents/w2:p1/prompt":
		p.prompts++
		var b struct{ Text string }
		json.Unmarshal(body, &b)
		if p.arrive && p.prompts == 1 {
			p.transcript += b.Text + "\n"
			http.Error(w, `{"code":"internal_error"}`, http.StatusInternalServerError)
			return
		}
		if p.prompts == 1 {
			http.Error(w, `{"code":"internal_error"}`, http.StatusInternalServerError)
			return
		}
		p.transcript += b.Text + "\n"
		fmt.Fprint(w, `{"ok":true}`)
	case "/api/agents/w2:p1/noticeseen":
		p.probes++
		var b noticeSeenBody
		json.Unmarshal(body, &b)
		if b.Offset < 0 {
			fmt.Fprintf(w, `{"ok":true,"size":%d}`, len(p.transcript))
			return
		}
		fmt.Fprintf(w, `{"ok":true,"seen":%v,"size":%d}`, strings.Contains(p.transcript[b.Offset:], "[notice "+b.ID+"]"), len(p.transcript))
	default:
		http.NotFound(w, r)
	}
}

func noticeHub(t *testing.T, p *noticePeer) *Server {
	t.Helper()
	pu, _ := url.Parse(httptestServer(t, p))
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	s := New(Config{ReadLines: 100, Fleet: fl, StateDir: t.TempDir()}, nil, nil)
	s.noticeArriveWait = 20 * time.Millisecond
	return s
}

func TestNoticeToPeerArrivedAfterErrorIsNotRetried(t *testing.T) {
	p := &noticePeer{arrive: true, transcript: "earlier\n"}
	s := noticeHub(t, p)
	a := herdr.AgentInfo{ID: "kube/w2:p1", Host: "kube", PaneID: "w2:p1", Name: "coordinator", Agent: "claude", Status: herdr.StatusIdle}
	if err := s.promptNotice(context.Background(), a, "Comebye (hub notice): a worker finished."); err != nil {
		t.Fatalf("arrived but reported failed: %v", err)
	}
	if p.prompts != 1 {
		t.Fatalf("prompts = %d, want 1", p.prompts)
	}
}

func TestNoticeToPeerNotArrivedIsRetried(t *testing.T) {
	p := &noticePeer{transcript: "earlier\n"}
	s := noticeHub(t, p)
	a := herdr.AgentInfo{ID: "kube/w2:p1", Host: "kube", PaneID: "w2:p1", Name: "coordinator", Agent: "claude", Status: herdr.StatusIdle}
	text := "Comebye (hub notice): a worker finished."
	if err := s.promptNotice(context.Background(), a, text); err == nil {
		t.Fatal("a notice that did not arrive reported success")
	}
	if err := s.promptNotice(context.Background(), a, text); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if p.prompts != 2 || strings.Count(p.transcript, "[notice ") != 1 {
		t.Fatalf("prompts = %d, transcript:\n%s", p.prompts, p.transcript)
	}
}

// The peer's own route answers from a local transcript, bounded to one id.
func TestNoticeSeenRoute(t *testing.T) {
	s, _, _ := coordServer(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(path, []byte("aaaa [notice 0a1b2c] bbbb\n"), 0o600)
	probe := probeFile(path, "0a1b2c", 0)
	if !probe.OK || !probe.Seen || probe.Size == 0 {
		t.Fatalf("probe = %+v", probe)
	}
	if p := probeFile(path, "0a1b2c", probe.Size); p.Seen {
		t.Fatal("seen after the offset")
	}
	if w := do(s, "POST", "/api/agents/w1:p1/noticeseen", userHdr, `{"id":"../etc","offset":0}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", w.Code)
	}
}

// A worker that files several explicit reports gets a callback per report,
// and the notice text is the same each time ("handoff X finished"): the
// coordinator saw "[notice 7bb054]" twice, minutes apart, though no send had
// failed. The same text is not delivered to the same agent twice.
func TestSameNoticeTextIsDeliveredOncePerRecipient(t *testing.T) {
	s, fh, _ := coordServer(t)
	coord := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}, coord)
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	waitFor(t, func() bool {
		g, _ := s.ledger.Get(e.ID)
		return g.Callback != nil && g.Callback.State == ledger.CallbackDelivered
	})
	if n := promptCount(fh); n != 1 {
		t.Fatalf("first notice: prompts = %d, want 1", n)
	}
	// The worker reports again (a second round / a corrected report).
	if _, ok, err := s.ledger.Report("desk/w9:p1", "second report, different words", time.Now()); err != nil || !ok {
		t.Fatalf("report: %v %v", ok, err)
	}
	g, _ := s.ledger.Get(e.ID)
	if g.Callback == nil || g.Callback.State != ledger.CallbackPending || g.ReportSeq != 1 && g.Callback.Seq != g.ReportSeq {
		t.Fatalf("second report did not queue a notice: %+v seq %d", g.Callback, g.ReportSeq)
	}
	s.deliverCallbacks([]herdr.AgentInfo{coord})
	waitFor(t, func() bool {
		g, _ := s.ledger.Get(e.ID)
		return g.Callback != nil && g.Callback.State == ledger.CallbackDelivered && g.Callback.Seq == g.ReportSeq
	})
	if n := promptCount(fh); n != 1 {
		t.Fatalf("same notice text sent again: prompts = %d, want 1", n)
	}
}

func TestPromptNoticeOnceWindowAndRecipient(t *testing.T) {
	s, fh, _ := coordServer(t)
	a := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	b := cbAgent("w2:p1", "other", herdr.StatusIdle)
	ctx := context.Background()
	s.notices.window = 80 * time.Millisecond
	for _, c := range []struct {
		to   herdr.AgentInfo
		text string
		want int
	}{{a, "worker X finished", 1}, {a, "worker X finished", 1}, {b, "worker X finished", 2}, {a, "worker Y finished", 3}} {
		if err := s.promptNoticeOnce(ctx, c.to, c.text); err != nil {
			t.Fatal(err)
		}
		if n := promptCount(fh); n != c.want {
			t.Fatalf("after %q to %s: prompts = %d, want %d", c.text, c.to.ID, n, c.want)
		}
	}
	time.Sleep(120 * time.Millisecond)
	s.promptNoticeOnce(ctx, a, "worker X finished")
	if n := promptCount(fh); n != 4 {
		t.Fatalf("after the window: prompts = %d, want 4", n)
	}
}

// A failed send is not remembered as delivered.
func TestPromptNoticeOnceFailureIsRetried(t *testing.T) {
	s, fh, _ := coordServer(t)
	s.noticeArriveWait = 10 * time.Millisecond
	a := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	onPrompt(fh, map[int]promptAnswer{1: {err: &herdr.Error{Code: "internal_error", Message: "x"}}, 0: {status: herdr.StatusWorking}}, nil)
	if err := s.promptNoticeOnce(context.Background(), a, "worker Z finished"); err == nil {
		t.Fatal("first send should fail")
	}
	if err := s.promptNoticeOnce(context.Background(), a, "worker Z finished"); err != nil {
		t.Fatal(err)
	}
	if n := promptCount(fh); n != 2 {
		t.Fatalf("prompts = %d, want 2", n)
	}
}

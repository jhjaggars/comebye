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
// and the notice text is the same each time. A report with new content always
// notifies the coordinator; the same content never does, with no time window.
func TestCallbackOncePerReportContent(t *testing.T) {
	s, fh, _ := coordServer(t)
	coord := cbAgent("w1:p1", "coordinator", herdr.StatusIdle)
	e := cbFinished(t, s, &ledger.Ref{AgentID: "desk/w1:p1", Name: "coordinator"}, coord)
	settle := func(want int) {
		t.Helper()
		s.deliverCallbacks([]herdr.AgentInfo{coord})
		waitFor(t, func() bool {
			g, _ := s.ledger.Get(e.ID)
			return g.Callback != nil && g.Callback.State == ledger.CallbackDelivered && g.Callback.Seq == g.ReportSeq
		})
		time.Sleep(30 * time.Millisecond)
		if n := promptCount(fh); n != want {
			t.Fatalf("prompts = %d, want %d", n, want)
		}
	}
	settle(1)
	report := func(text string) {
		t.Helper()
		if _, ok, err := s.ledger.Report("desk/w9:p1", text, time.Now()); err != nil || !ok {
			t.Fatalf("report: %v %v", ok, err)
		}
	}
	// Two reports with different content within a minute: two notices.
	report("round two: done")
	settle(2)
	report("round three: done, landed")
	settle(3)
	// The same content again (the worker re-files it after round two's
	// text was replaced): already announced, no notice, and it settles.
	report("round two: done")
	settle(3)
	// And it survives a restart: the key is in the ledger.
	g, _ := s.ledger.Get(e.ID)
	if len(g.NoticeKeys) != 3 {
		t.Fatalf("notice keys = %v", g.NoticeKeys)
	}
}

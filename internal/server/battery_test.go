package server

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/state"
	"github.com/jhjaggars/comebye/internal/transcript"
)

func writeSession(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func piMsg(id, role, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "message", "id": id, "message": map[string]any{
		"role": role, "content": []map[string]string{{"type": "text", "text": text}}}})
	return string(b)
}

// Transcript polls: an unchanged file is a bodiless 304, and ?from= cuts
// the response down to that message and the ones after it.
func TestTranscriptConditionalAndPartial(t *testing.T) {
	s := New(Config{ReadLines: 100, HostName: "desk"}, nil, nil)
	path := writeSession(t, piMsg("u1", "user", "hi"), piMsg("a1", "assistant", "hello"), piMsg("u2", "user", "more"))
	get := func(query string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/agents/wS:p1/transcript"+query, nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		s.serveTranscript(w, r, path, transcript.Read)
		return w
	}
	ids := func(w *httptest.ResponseRecorder) (out []string, partial bool) {
		var body struct {
			Messages []struct{ ID string }
			Partial  bool
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		for _, m := range body.Messages {
			out = append(out, m.ID)
		}
		return out, body.Partial
	}

	w := get("?limit=100", nil)
	etag := w.Header().Get("ETag")
	if w.Code != 200 || etag == "" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("first: %d %v", w.Code, w.Header())
	}
	if got, partial := ids(w); len(got) != 3 || partial {
		t.Fatalf("full = %v %v", got, partial)
	}
	if w := get("?limit=100", map[string]string{"If-None-Match": etag}); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("unchanged: %d %q", w.Code, w.Body)
	}

	w = get("?limit=100&from=a1", nil)
	if got, partial := ids(w); strings.Join(got, ",") != "a1,u2" || !partial {
		t.Fatalf("from a1 = %v %v", got, partial)
	}
	if w.Header().Get("ETag") == etag {
		t.Fatal("from must change the ETag")
	}
	if got, partial := ids(get("?limit=100&from=gone", nil)); len(got) != 3 || partial {
		t.Fatalf("unknown from = %v %v", got, partial)
	}

	// The file grows: the old ETag no longer matches.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(piMsg("a2", "assistant", "done") + "\n")
	f.Close()
	if w := get("?limit=100", map[string]string{"If-None-Match": etag}); w.Code != 200 {
		t.Fatalf("changed: %d", w.Code)
	}
}

// A subagent's transcript is read from <session>/subagents/ only, for a
// well-formed id, and is served like a session (ETag, ?from=).
func TestSubagentTranscript(t *testing.T) {
	s := New(Config{ReadLines: 100, HostName: "desk"}, nil, nil)
	session := writeSession(t, `{"type":"user","isSidechain":false,"message":{"role":"user","content":"hi"},"uuid":"u1","timestamp":"2026-10-20T10:00:00Z"}`)
	dir := filepath.Join(strings.TrimSuffix(session, ".jsonl"), "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sub := `{"type":"user","isSidechain":true,"message":{"role":"user","content":"do the task"},"uuid":"s1","timestamp":"2026-10-20T10:00:01Z"}` + "\n" +
		`{"type":"assistant","isSidechain":true,"message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"SubagentHandback","input":{"message":"all done"}}]},"uuid":"s2","timestamp":"2026-10-20T10:00:02Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "agent-a1234567.jsonl"), []byte(sub), 0o600); err != nil {
		t.Fatal(err)
	}
	get := func(id, query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.serveSubagent(w, httptest.NewRequest("GET", "/api/agents/wS:p1/subagents/x/transcript"+query, nil), session, id)
		return w
	}
	w := get("a1234567", "?limit=10")
	if w.Code != 200 || w.Header().Get("ETag") == "" || !strings.Contains(w.Body.String(), `"text":"all done"`) || !strings.Contains(w.Body.String(), `"text":"do the task"`) {
		t.Fatalf("subagent: %d %s", w.Code, w.Body)
	}
	if w := get("a1234567", "?from=t1"); !strings.Contains(w.Body.String(), `"partial":true`) || strings.Contains(w.Body.String(), "do the task") {
		t.Fatalf("from: %s", w.Body)
	}
	for id, code := range map[string]int{"a7654321": 404, "../s": 400, "a1234567/../../s": 400, "s": 400, "": 400} {
		if w := get(id, ""); w.Code != code {
			t.Errorf("%q: %d %s, want %d", id, w.Code, w.Body, code)
		}
	}
}

// Browsers get slim agent records, an "agents" event only when something
// they see changed, and a gzip-compressed stream that still flushes per
// event. A hub (?full=1) gets full records, uncompressed.
func TestEventsSlimDedupedGzip(t *testing.T) {
	s := New(Config{ReadLines: 100, HostName: "desk", PingInterval: time.Hour}, nil, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	hub := s.fleet.Hub()

	agent := func(rev uint64, status herdr.AgentStatus) herdr.AgentInfo {
		return herdr.AgentInfo{Name: "a", PaneID: "wS:p1", ID: "desk/wS:p1", Host: "desk", Status: status, Revision: rev,
			AgentSession: &herdr.AgentSessionInfo{Kind: "path", Value: "/secret/session.jsonl"}, TerminalID: "t1"}
	}

	open := func(query string, gz bool) (*bufio.Reader, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events"+query, nil)
		if gz {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		resp, err := http.DefaultTransport.RoundTrip(req) // no transparent decompression
		if err != nil {
			t.Fatal(err)
		}
		var body io.Reader = resp.Body
		if gz {
			if resp.Header.Get("Content-Encoding") != "gzip" {
				t.Fatalf("not gzipped: %v", resp.Header)
			}
			zr, err := gzip.NewReader(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			body = zr
		} else if resp.Header.Get("Content-Encoding") != "" {
			t.Fatalf("hub stream compressed: %v", resp.Header)
		}
		return bufio.NewReader(body), func() { cancel(); resp.Body.Close() }
	}
	next := func(br *bufio.Reader) string {
		t.Helper()
		done := make(chan string, 1)
		go func() {
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					done <- "ERR " + err.Error()
					return
				}
				if strings.HasPrefix(line, "data: ") {
					done <- line
					return
				}
			}
		}()
		select {
		case l := <-done:
			return l
		case <-time.After(3 * time.Second):
			return ""
		}
	}

	browser, closeB := open("", true)
	defer closeB()
	full, closeF := open("?full=1", false)
	defer closeF()
	next(browser) // initial snapshots
	next(full)

	hub.Broadcast(state.Event{Type: "agents", Agents: []herdr.AgentInfo{agent(1, herdr.StatusWorking)}})
	b, f := next(browser), next(full)
	if !strings.Contains(b, `"agent_status":"working"`) || strings.Contains(b, "session") || strings.Contains(b, "terminal_id") || strings.Contains(b, "revision") {
		t.Fatalf("browser event = %s", b)
	}
	if !strings.Contains(f, "/secret/session.jsonl") {
		t.Fatalf("full event = %s", f)
	}
	// Only the revision changes: the hub hears it, the browser does not.
	hub.Broadcast(state.Event{Type: "agents", Agents: []herdr.AgentInfo{agent(2, herdr.StatusWorking)}})
	if f := next(full); !strings.Contains(f, `"revision":2`) {
		t.Fatalf("full event = %s", f)
	}
	hub.Broadcast(state.Event{Type: "agents", Agents: []herdr.AgentInfo{agent(3, herdr.StatusDone)}})
	if b := next(browser); !strings.Contains(b, `"agent_status":"done"`) {
		t.Fatalf("browser should skip the revision-only update and get the next one, got %s", b)
	}
}

func TestGzipLeaves304Bodiless(t *testing.T) {
	h := withGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"x"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	r := httptest.NewRequest("GET", "/api/agents/x/transcript", nil)
	r.Header.Set("Accept-Encoding", "gzip, deflate")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("%d %q %v", w.Code, w.Body, w.Header())
	}
}

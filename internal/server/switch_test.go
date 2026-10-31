package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/ledger"
	"github.com/jhjaggars/comebye/internal/state"
)

func TestSwitchCommands(t *testing.T) {
	pi := &modelCatalog{Kind: "pi", Models: []modelInfo{{ID: "homelab/fast"}}}
	cases := []struct {
		kind, model, thinking string
		want                  []string
		code                  string
	}{
		{"claude", "opus", "", []string{"/model opus"}, ""},
		{"pi", "homelab/fast", "high", []string{"/model homelab/fast", "/thinking high"}, ""},
		{"pi", "", "low", []string{"/thinking low"}, ""},
		{"pi", "homelab/nope", "", nil, "unknown_model"}, // would open pi's selector
		{"pi", "", "ultra", nil, "bad_thinking"},
		{"claude", "opus", "high", nil, "thinking_unsupported"},
		{"claude", "", "", nil, "model_required"},
		{"claude", "opus; id", "", nil, "bad_model"},
		{"codex", "gpt-5.5", "", nil, "switch_unsupported"},
	}
	for _, c := range cases {
		got, err := switchCommands(c.kind, c.model, c.thinking, pi)
		switch {
		case c.code != "" && (err == nil || err.code != c.code):
			t.Errorf("%v: got %v, %v; want %s", c, got, err, c.code)
		case c.code == "" && (err != nil || !reflect.DeepEqual(got, c.want)):
			t.Errorf("%v: got %q, %v", c, got, err)
		}
	}
}

func switchServer(t *testing.T, agents []herdr.AgentInfo) (*Server, *fakeLaunchHerdr) {
	fh, client := startFakeLaunchHerdr(t)
	fh.set(func(f *fakeLaunchHerdr) { f.agents = agents })
	m := state.New(client, nil)
	m.SetHost("desk")
	m.Refresh()
	for deadline := time.Now().Add(5 * time.Second); len(m.Agents()) < len(agents) && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	s := New(Config{ReadLines: 100, HostName: "desk", StateDir: t.TempDir()}, client, m)
	s.switchGap = time.Millisecond
	s.listModels = func(ctx context.Context, kind string) *modelCatalog {
		return &modelCatalog{Kind: kind, Exact: true, Models: []modelInfo{{ID: "homelab/fast"}, {ID: "homelab/thinking"}}}
	}
	return s, fh
}

func sentPrompts(fh *fakeLaunchHerdr) []string {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	var out []string
	for _, c := range fh.calls {
		if c.Method == "agent.prompt" {
			out = append(out, c.Params["target"].(string)+": "+c.Params["text"].(string))
		}
	}
	return out
}

func TestSwitchModelLocal(t *testing.T) {
	s, fh := switchServer(t, []herdr.AgentInfo{
		{Name: "p", PaneID: "w1:p1", Agent: "pi", Status: herdr.StatusIdle},
		{Name: "c", PaneID: "w2:p1", Agent: "claude", Status: herdr.StatusIdle},
		{Name: "busy", PaneID: "w3:p1", Agent: "claude", Status: herdr.StatusWorking},
		{Name: "x", PaneID: "w4:p1", Agent: "codex", Status: herdr.StatusIdle},
	})
	// A worker's ledger entry follows the switch.
	e, _ := s.ledger.Add(ledger.Entry{Host: "desk", Repo: "/r", RepoName: "r", Branch: "b", Kind: "pi", Model: "homelab/fast", AgentID: "desk/w1:p1", PaneID: "w1:p1", Status: ledger.Running})

	w := do(s, "POST", "/api/agents/p/model", nil, `{"model":"homelab/thinking","thinking":"high"}`)
	if w.Code != 200 {
		t.Fatalf("pi = %d %s", w.Code, w.Body)
	}
	if got := sentPrompts(fh); !reflect.DeepEqual(got, []string{"w1:p1: /model homelab/thinking", "w1:p1: /thinking high"}) {
		t.Fatalf("prompts = %q", got)
	}
	if got, _ := s.ledger.Get(e.ID); got.Model != "homelab/thinking" || got.Thinking != "high" {
		t.Fatalf("entry = %+v", got)
	}

	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	if w := do(s, "POST", "/api/hosts/desk/agents/w2:p1/model", nil, `{"model":"sonnet"}`); w.Code != 200 {
		t.Fatalf("claude = %d %s", w.Code, w.Body)
	}
	if got := sentPrompts(fh); !reflect.DeepEqual(got, []string{"w2:p1: /model sonnet"}) {
		t.Fatalf("prompts = %q", got)
	}

	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	for path, c := range map[string]struct {
		body string
		code int
		err  string
	}{
		"/api/agents/p/model":           {`{"model":"homelab/nope"}`, 400, "unknown_model"},
		"/api/agents/c/model":           {`{}`, 400, "model_required"},
		"/api/agents/x/model":           {`{"model":"gpt-5.5"}`, 400, "switch_unsupported"},
		"/api/agents/busy/model":        {`{"model":"opus"}`, 409, "agent_busy"},
		"/api/agents/nobody/model":      {`{"model":"opus"}`, 404, "agent_not_found"},
		"/api/hosts/zzz/agents/p/model": {`{"model":"opus"}`, 404, "unknown_host"},
	} {
		w := do(s, "POST", path, nil, c.body)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.err) {
			t.Errorf("%s: %d %s, want %d %s", path, w.Code, w.Body, c.code, c.err)
		}
	}
	if got := sentPrompts(fh); len(got) != 0 {
		t.Fatalf("a refused switch still sent %q", got)
	}
}

func TestSwitchModelViaPeer(t *testing.T) {
	var gotPath, gotBody string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: agents\ndata: {\"type\":\"agents\",\"agents\":[]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"agent":{"pane_id":"wS:p9","name":"feat-x","agent":"claude"},"commands":["/model opus"]}`)
	}))
	defer peer.Close()
	pu, _ := url.Parse(peer.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	s := New(Config{ReadLines: 100, Fleet: fl, StateDir: t.TempDir()}, nil, nil)
	e, _ := s.ledger.Add(ledger.Entry{Host: "kube", Repo: "/r", RepoName: "r", Branch: "b", Kind: "claude", AgentID: "kube/wS:p9", PaneID: "wS:p9", Status: ledger.Running})

	w := do(s, "POST", "/api/hosts/kube/agents/wS:p9/model", nil, `{"model":"opus"}`)
	if w.Code != 200 || gotPath != "/api/agents/wS:p9/model" {
		t.Fatalf("peer = %d %s (peer saw %s)", w.Code, w.Body, gotPath)
	}
	var b switchBody
	json.Unmarshal([]byte(gotBody), &b)
	if b.Model != "opus" {
		t.Fatalf("peer body = %s", gotBody)
	}
	if got, _ := s.ledger.Get(e.ID); got.Model != "opus" {
		t.Fatalf("hub ledger = %+v", got)
	}
}

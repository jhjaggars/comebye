package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/park"
	"github.com/jhjaggars/comebye/internal/scratch"
)

func parkReq(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	w := do(s, method, path, nil, body)
	var r map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return w.Code, r
}

// parkSetup is removalSetup with a state directory and a pi session file.
func parkSetup(t *testing.T) (s *Server, fh *fakeLaunchHerdr, wt, session string) {
	t.Helper()
	s, fh, _, wt = removalSetup(t)
	s.cfg.StateDir = t.TempDir()
	session = filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(session, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents[0].AgentSession = &herdr.AgentSessionInfo{Agent: "pi", Kind: "path", Value: session}
	})
	return s, fh, wt, session
}

func TestParkAndResumePi(t *testing.T) {
	s, fh, wt, session := parkSetup(t)
	// How the worker was launched is remembered by name.
	s.parkStore().NoteLaunch("w", "pi", []string{"--model", "m1", "--", "first prompt"}, timeNow())

	code, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1","name":"w"}`)
	if code != 200 || r["closed"] != "workspace" {
		t.Fatalf("park = %d %v", code, r)
	}
	rec := r["record"].(map[string]any)
	if rec["kind"] != "pi" || rec["session"] != session || rec["session_kind"] != "path" || rec["branch"] != "w" || rec["worktree"] != wt {
		t.Fatalf("record = %v", rec)
	}
	if args, _ := json.Marshal(rec["launch_args"]); string(args) != `["--model","m1"]` {
		t.Fatalf("launch args = %s (the first prompt must not be kept)", args)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("park removed the worktree")
	}
	// The record is on disk, before and after a restart of the store.
	st, err := park.Open(s.cfg.StateDir)
	if err != nil || len(st.List(true)) != 1 {
		t.Fatalf("store = %v, %v", st, err)
	}
	if code, r := parkReq(t, s, "GET", "/api/parked", ""); code != 200 || len(r["parked"].([]any)) != 1 {
		t.Fatalf("parked = %d %v", code, r)
	}

	// Resume: a new pane on the same session, with the launch args after the resume flag.
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	id := rec["id"].(string)
	code, r = parkReq(t, s, "POST", "/api/resume", `{"id":"`+id+`"}`)
	if code != 200 || r["agent"].(map[string]any)["name"] != "w" {
		t.Fatalf("resume = %d %v", code, r)
	}
	start := fh.call("agent.start")
	if start == nil {
		t.Fatal("no agent.start")
	}
	if got, _ := json.Marshal(start.Params["args"]); string(got) != `["--session","`+session+`","--model","m1"]` {
		t.Fatalf("agent.start args = %s", got)
	}
	if start.Params["kind"] != "pi" || start.Params["name"] != "w" {
		t.Fatalf("agent.start = %v", start.Params)
	}
	if ws := fh.call("workspace.create"); ws == nil || ws.Params["cwd"] != wt {
		t.Fatalf("workspace.create = %+v", ws)
	}
	if p := fh.call("agent.prompt"); p == nil || !strings.Contains(p.Params["text"].(string), "parked") {
		t.Fatalf("catch-up note not sent: %+v", p)
	}
	if code, r := parkReq(t, s, "GET", "/api/parked", ""); code != 200 || len(r["parked"].([]any)) != 0 {
		t.Fatalf("still parked after resume: %v", r)
	}
	// Parked again, the record holds the original launch arguments, not the resume flags.
	fh.set(func(f *fakeLaunchHerdr) {
		a := &f.agents[len(f.agents)-1]
		a.AgentSession, a.CWD = &herdr.AgentSessionInfo{Agent: "pi", Kind: "path", Value: session}, wt
	})
	code2, r2 := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1","name":"w"}`)
	if code2 != 200 {
		t.Fatalf("second park = %d %v", code2, r2)
	}
	if args, _ := json.Marshal(r2["record"].(map[string]any)["launch_args"]); string(args) != `["--model","m1"]` {
		t.Fatalf("second park launch args = %s", args)
	}
	// A second resume of the same record is refused, and so is one for a running name.
	if code, r := parkReq(t, s, "POST", "/api/resume", `{"id":"`+id+`"}`); code != 409 || r["code"] != "not_parked" {
		t.Fatalf("second resume = %d %v", code, r)
	}
}

func TestParkRefusals(t *testing.T) {
	s, fh, wt, _ := parkSetup(t)
	defer func() {
		if fh.call("workspace.close") != nil || fh.call("pane.close") != nil {
			t.Error("a refused park closed something")
		}
	}()
	// Working agents are not parked.
	fh.set(func(f *fakeLaunchHerdr) { f.agents[0].Status = herdr.StatusWorking })
	if code, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1"}`); code != 409 || r["code"] != "agent_working" {
		t.Fatalf("working = %d %v", code, r)
	}
	fh.set(func(f *fakeLaunchHerdr) { f.agents[0].Status = herdr.StatusIdle })
	// A renamed pane.
	if code, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1","name":"other"}`); code != 409 || r["code"] != "agent_changed" {
		t.Fatalf("renamed = %d %v", code, r)
	}
	if code, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p7"}`); code != 404 || r["code"] != "no_such_agent" {
		t.Fatalf("missing = %d %v", code, r)
	}
	// Uncommitted edits need the confirm; nothing is closed or written without it.
	os.WriteFile(wt+"/new.txt", []byte("x"), 0o644)
	if code, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1"}`); code != 409 || r["code"] != "dirty_worktree" {
		t.Fatalf("dirty = %d %v", code, r)
	}
	if n := len(s.parkStore().List(false)); n != 0 {
		t.Fatalf("%d records after refusals", n)
	}
	// No session to resume: refused before anything closes.
	fh.set(func(f *fakeLaunchHerdr) { f.agents[0].AgentSession = nil })
	if code, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1","dirty":true}`); code != 409 || r["code"] != "no_session" {
		t.Fatalf("no session = %d %v", code, r)
	}
	// An agent kind that cannot be parked.
	fh.set(func(f *fakeLaunchHerdr) { f.agents[0].Agent = "codex" })
	if code, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1","dirty":true}`); code != 409 || r["code"] != "kind_not_parkable" {
		t.Fatalf("codex = %d %v", code, r)
	}
}

func TestParkDirtyConfirmedKeepsEdits(t *testing.T) {
	s, _, wt, _ := parkSetup(t)
	os.WriteFile(wt+"/new.txt", []byte("x"), 0o644)
	code, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1","dirty":true}`)
	if code != 200 || r["record"].(map[string]any)["dirty"] != true {
		t.Fatalf("park = %d %v", code, r)
	}
	if b, err := os.ReadFile(wt + "/new.txt"); err != nil || string(b) != "x" {
		t.Fatal("the edit did not stay in place")
	}
}

func TestResumeRefusals(t *testing.T) {
	s, fh, wt, session := parkSetup(t)
	_, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1"}`)
	id := r["record"].(map[string]any)["id"].(string)
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })

	os.Remove(session)
	if code, r := parkReq(t, s, "POST", "/api/resume", `{"id":"`+id+`"}`); code != 409 || r["code"] != "session_gone" {
		t.Fatalf("session gone = %d %v", code, r)
	}
	os.WriteFile(session, []byte("{}\n"), 0o644)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = append(f.agents, herdr.AgentInfo{Name: "w", PaneID: "w3:p1", WorkspaceID: "w3", Agent: "pi", CWD: "/elsewhere"})
	})
	if code, r := parkReq(t, s, "POST", "/api/resume", `{"id":"`+id+`"}`); code != 409 || r["code"] != "agent_name_taken" {
		t.Fatalf("name taken = %d %v", code, r)
	}
	fh.set(func(f *fakeLaunchHerdr) { f.agents = nil })
	if code, r := parkReq(t, s, "POST", "/api/resume", `{"id":"pk_nope"}`); code != 404 {
		t.Fatalf("unknown id = %d %v", code, r)
	}
	os.RemoveAll(wt)
	if code, r := parkReq(t, s, "POST", "/api/resume", `{"id":"`+id+`"}`); code != 404 || !strings.Contains(r["message"].(string), "gone") {
		t.Fatalf("worktree gone = %d %v", code, r)
	}
	if fh.call("workspace.create") != nil || fh.call("agent.start") != nil {
		t.Error("a refused resume opened a pane")
	}
}

func TestParkClaudeMovesScratchAndBack(t *testing.T) {
	s, fh, wt, _ := parkSetup(t)
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	tmp := t.TempDir()
	s.cfg.ScratchDir = tmp
	const sid = "c3eb8f54-0000-4000-8000-000000000001"
	projDir := filepath.Join(cfgDir, "projects", scratch.ProjectDir(wt))
	os.MkdirAll(projDir, 0o755)
	os.WriteFile(filepath.Join(projDir, sid+".jsonl"), []byte("{}\n"), 0o644)
	pad := filepath.Join(tmp, "claude-"+itoa(os.Getuid()), scratch.ProjectDir(wt), sid, "scratchpad")
	os.MkdirAll(pad, 0o755)
	os.WriteFile(filepath.Join(pad, "big.txt"), []byte(strings.Repeat("z", 5000)), 0o644)
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents[0].Agent = "claude"
		f.agents[0].AgentSession = &herdr.AgentSessionInfo{Agent: "claude", Kind: "id", Value: sid}
	})
	s.parkStore().NoteLaunch("w", "claude", []string{"--model", "opus", "--allowedTools", "Bash", "--", "p"}, timeNow())

	code, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1"}`)
	if code != 200 {
		t.Fatalf("park = %d %v", code, r)
	}
	rec := r["record"].(map[string]any)
	if rec["session"] != sid || rec["session_kind"] != "id" || rec["scratch_bytes"].(float64) != 5000 {
		t.Fatalf("record = %v", rec)
	}
	if _, err := os.Stat(pad); err == nil {
		t.Fatal("the scratchpad is still in the temporary directory")
	}
	if b, err := os.ReadFile(filepath.Join(rec["scratch_to"].(string), sid, "scratchpad", "big.txt")); err != nil || len(b) != 5000 {
		t.Fatalf("scratchpad not on disk: %v", err)
	}

	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })
	if code, r := parkReq(t, s, "POST", "/api/resume", `{"id":"`+rec["id"].(string)+`","no_note":true}`); code != 200 || r["scratch"] != "scratchpad moved back" {
		t.Fatalf("resume = %d %v", code, r)
	}
	if b, err := os.ReadFile(filepath.Join(pad, "big.txt")); err != nil || len(b) != 5000 {
		t.Fatalf("scratchpad not back: %v", err)
	}
	// --resume comes first: claude's --allowedTools swallows what follows it.
	got, _ := json.Marshal(fh.call("agent.start").Params["args"])
	if string(got) != `["--resume","`+sid+`","--model","opus","--allowedTools","Bash"]` {
		t.Fatalf("args = %s", got)
	}
	if fh.call("agent.prompt") != nil {
		t.Error("no_note still sent a note")
	}
}

func timeNow() time.Time { return time.Now() }

func TestParkedAllMirrorsOfflinePeers(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/api/parked" {
			io.WriteString(w, `{"host":"kube","parked":[{"id":"pk_kube000001","name":"far","kind":"pi","parked_at":"2026-11-10T10:00:00Z"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)
	fl := fleet.New(nil, "desk", []fleet.Peer{{Name: "kube", URLs: []*url.URL{pu}}}, fleet.Options{Logf: func(string, ...any) {}})
	s := New(Config{AllowLogins: []string{"me@example.com"}, Fleet: fl, StateDir: t.TempDir()}, nil, nil)
	if err := s.parkStore().Add(park.Record{ID: "pk_local00001", Name: "near", Kind: "claude", ParkedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	get := func() []parkedHost {
		w := do(s, "GET", "/api/parked/all", map[string]string{"Tailscale-User-Login": "me@example.com"}, "")
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		var r struct {
			Hosts []parkedHost `json:"hosts"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r.Hosts
	}
	hs := get()
	if len(hs) != 2 || hs[0].Host != "desk" || !hs[0].Online || len(hs[0].Parked) != 1 || hs[0].Parked[0].Name != "near" ||
		hs[1].Host != "kube" || !hs[1].Online || len(hs[1].Parked) != 1 || hs[1].Parked[0].Name != "far" {
		t.Fatalf("hosts = %+v", hs)
	}
	// The peer goes away: its entries come from the hub's copy, marked offline.
	up.Store(false)
	hs = get()
	if hs[1].Online || hs[1].Seen == nil || hs[1].Error == "" || len(hs[1].Parked) != 1 || hs[1].Parked[0].Name != "far" {
		t.Fatalf("offline kube = %+v", hs[1])
	}
	if !hs[0].Online {
		t.Fatal("local host not online")
	}
}

func TestParkPin(t *testing.T) {
	s, _, _, _ := parkSetup(t)
	_, r := parkReq(t, s, "POST", "/api/park", `{"agent":"w9:p1","name":"w"}`)
	id := r["record"].(map[string]any)["id"].(string)
	if code, r := parkReq(t, s, "POST", "/api/park/pin", `{"id":"`+id+`","pinned":true}`); code != 200 || r["record"].(map[string]any)["pinned"] != true {
		t.Fatalf("pin = %d %v", code, r)
	}
	if rec, _ := s.parkStore().Get(id); !rec.Pinned {
		t.Fatal("pin not stored")
	}
	if code, _ := parkReq(t, s, "POST", "/api/park/pin", `{"id":"pk_nope","pinned":true}`); code != 404 {
		t.Fatalf("unknown = %d", code)
	}
}

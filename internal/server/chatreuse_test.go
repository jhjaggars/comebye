package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// chatReuseHub is chatHub with a fake herdr that closes what it is asked to
// close, and a pi session file for the chat helper.
func chatReuseHub(t *testing.T) (s *Server, fh *fakeLaunchHerdr, page, session string) {
	t.Helper()
	s, fh, page, _ = chatHub(t)
	session = filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(session, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fh.set(func(f *fakeLaunchHerdr) {
		f.hook = func(method string, p map[string]any) (any, *herdr.Error, bool) {
			switch method {
			case "workspace.list":
				return map[string]any{"type": "workspace_list", "workspaces": []any{}}, nil, true
			case "worktree.list":
				return map[string]any{"type": "worktree_list", "worktrees": []any{}}, nil, true
			case "workspace.close", "pane.close":
				kept := f.agents[:0]
				for _, a := range f.agents {
					if a.WorkspaceID != p["workspace_id"] && a.PaneID != p["pane_id"] {
						kept = append(kept, a)
					}
				}
				f.agents = kept
				return map[string]any{"type": "ok"}, nil, true
			}
			return nil, nil, false
		}
	})
	return s, fh, page, session
}

// settleChat gives the helper just started its working directory and
// session, as herdr reports them, and waits for the fleet to show them.
func settleChat(t *testing.T, s *Server, fh *fakeLaunchHerdr, name, dir, session string) {
	t.Helper()
	fh.set(func(f *fakeLaunchHerdr) {
		for i := range f.agents {
			if f.agents[i].Name == name {
				f.agents[i].CWD = dir
				f.agents[i].AgentSession = &herdr.AgentSessionInfo{Agent: "pi", Kind: "path", Value: session}
			}
		}
	})
	waitFleet(t, s, func(as []herdr.AgentInfo) bool {
		for _, a := range as {
			if a.Name == name && a.CWD == dir {
				return true
			}
		}
		return false
	})
}

func waitFleet(t *testing.T, s *Server, ok func([]herdr.AgentInfo) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.manager.Refresh()
		if ok(s.fleet.Agents()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fleet never reached the expected state: %+v", s.fleet.Agents())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// userReq is parkReq with the user's login, for the routes only the user may call.
func userReq(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	w := served(s, method, path, userHdr, body)
	var r map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return w.Code, r
}

func startsOf(fh *fakeLaunchHerdr) int {
	n := 0
	for _, m := range fh.methods() {
		if m == "agent.start" {
			n++
		}
	}
	return n
}

func firstChat(t *testing.T, s *Server, fh *fakeLaunchHerdr, page, session string) (name, dir string) {
	t.Helper()
	code, m := chatPost(s, page, userHdr, `{}`)
	if code != 200 || m["reused"] != nil || m["replaced"] != nil {
		t.Fatalf("first tap = %d %v", code, m)
	}
	name = m["name"].(string)
	dir = m["chat"].(map[string]any)["dir"].(string)
	settleChat(t, s, fh, name, dir, session)
	return name, dir
}

func TestPageChatSecondTapOpensTheLiveChat(t *testing.T) {
	s, fh, page, session := chatReuseHub(t)
	name, _ := firstChat(t, s, fh, page, session)
	first, _ := s.chats.get(page)
	for i := 0; i < chatRate+2; i++ { // reuse is not a new chat: the rate limit does not count it
		code, m := chatPost(s, page, userHdr, `{}`)
		if code != 200 || m["reused"] != "open" || m["name"] != name || m["replaced"] != nil {
			t.Fatalf("tap %d = %d %v", i+2, code, m)
		}
		if ag := m["agent"].(map[string]any); ag["id"] != first.Agent || ag["pane_id"] != first.Agent[len("desk/"):] {
			t.Fatalf("tap %d opened %v, not %s", i+2, ag, first.Agent)
		}
	}
	if n := startsOf(fh); n != 1 {
		t.Fatalf("agent.start ran %d times, want 1", n)
	}
	if rec, _ := s.chats.get(page); rec.Agent != first.Agent || len(rec.Prior) != 0 {
		t.Fatalf("record changed: %+v", rec)
	}
}

func TestPageChatSecondTapResumesAParkedChat(t *testing.T) {
	s, fh, page, session := chatReuseHub(t)
	name, dir := firstChat(t, s, fh, page, session)
	rec, _ := s.chats.get(page)

	code, r := userReq(t, s, "POST", "/api/park", `{"agent":"`+rec.Agent[len("desk/"):]+`","name":"`+name+`"}`)
	if code != 200 || r["closed"] == "" {
		t.Fatalf("park = %d %v", code, r)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("parking deleted the chat's directory: %v", err)
	}
	waitFleet(t, s, func(as []herdr.AgentInfo) bool {
		for _, a := range as {
			if a.Name == name {
				return false
			}
		}
		return true
	})
	fh.set(func(f *fakeLaunchHerdr) { f.calls = nil })

	code, m := chatPost(s, page, userHdr, `{}`)
	if code != 200 || m["reused"] != "resumed" || m["name"] != name {
		t.Fatalf("tap on a parked chat = %d %v", code, m)
	}
	if ws := fh.call("workspace.create"); ws == nil || ws.Params["cwd"] != dir {
		t.Fatalf("workspace.create = %+v, want the chat's directory", ws)
	}
	st := fh.call("agent.start")
	if st == nil || st.Params["name"] != name || !strings.Contains(strings.Join(toStrings(st.Params["args"]), " "), session) {
		t.Fatalf("agent.start = %+v, want the same session %s", st, session)
	}
	// No catch-up note about a worktree, and no brief a second time.
	if p := fh.call("agent.prompt"); p != nil {
		t.Fatalf("a prompt was sent: %v", p.Params)
	}
	now, _ := s.chats.get(page)
	if now.Agent != "desk/"+m["agent"].(map[string]any)["pane_id"].(string) || len(now.Prior) != 0 || now.Dir != dir {
		t.Fatalf("record after resume = %+v", now)
	}
	if r := s.liveRoles()[now.Agent]; r.Role != RoleChat || r.Page != page {
		t.Fatalf("role of the resumed chat = %+v", r)
	}
}

func TestPageChatGoneChatIsReplaced(t *testing.T) {
	s, fh, page, session := chatReuseHub(t)
	name, dir := firstChat(t, s, fh, page, session)
	first, _ := s.chats.get(page)
	// Gone: removed with nothing left (no park record), its directory forgotten.
	fh.set(func(f *fakeLaunchHerdr) { f.agents = f.agents[:2] })
	waitFleet(t, s, func(as []herdr.AgentInfo) bool { return len(as) == 2 })

	code, m := chatPost(s, page, userHdr, `{}`)
	if code != 200 || m["replaced"] != name || m["reused"] != nil || m["name"] != name {
		t.Fatalf("tap on a gone chat = %d %v", code, m)
	}
	rec, _ := s.chats.get(page)
	if rec.Name != name || rec.Dir == first.Dir || len(rec.Prior) != 1 {
		t.Fatalf("record = %+v", rec)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the gone chat's directory stays: %v", err)
	}
	if n := startsOf(fh); n != 2 {
		t.Fatalf("agent.start ran %d times, want 2", n)
	}
}

func TestPageChatNewChatReplacesTheRecordAndLeavesTheOldHelper(t *testing.T) {
	s, fh, page, session := chatReuseHub(t)
	name, dir := firstChat(t, s, fh, page, session)
	first, _ := s.chats.get(page)
	code, m := chatPost(s, page, userHdr, `{"new":true}`)
	if code != 200 || m["name"] != name+"-2" || m["reused"] != nil || m["replaced"] != nil {
		t.Fatalf("new chat = %d %v", code, m)
	}
	rec, _ := s.chats.get(page)
	if len(rec.Prior) != 1 || rec.Prior[0] != first.Agent || rec.Dir == dir {
		t.Fatalf("record = %+v", rec)
	}
	// The old helper is still there and still has its directory.
	found := false
	for _, a := range s.fleet.Agents() {
		found = found || a.Name == name
	}
	if !found {
		t.Fatal("the old helper was closed")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the old helper's directory went: %v", err)
	}
}

func TestPageChatVersionNoteIsQueuedOnce(t *testing.T) {
	s, fh, page, session := chatReuseHub(t)
	name, _ := firstChat(t, s, fh, page, session)
	// No note while the Page has not moved.
	if _, m := chatPost(s, page, userHdr, `{}`); m["page_note"] != nil {
		t.Fatalf("a note without a new version: %v", m)
	}
	w := do(s, "POST", "/api/pages", agentHdr("w2:p1"), pubJSON(map[string]any{"id": page, "body": "<title>Garage failover</title><p>v2", "if_version": 1}))
	if w.Code != 200 {
		t.Fatalf("publish v2 = %d %s", w.Code, w.Body)
	}
	for i, wantNote := range []bool{true, false, false} {
		code, m := chatPost(s, page, userHdr, `{}`)
		if code != 200 || m["reused"] != "open" || (m["page_note"] != nil) != wantNote {
			t.Fatalf("tap %d after v2 = %d %v", i+1, code, m)
		}
	}
	var notes []QueuedPrompt
	for _, it := range s.pq.list("", false) {
		if strings.Contains(it.Text, "is now version 2") {
			notes = append(notes, it)
		}
	}
	if len(notes) != 1 || notes[0].Name != name || !strings.Contains(notes[0].Text, "comebye pages show "+page+" --body --text") || !strings.Contains(notes[0].Text, "started on version 1") {
		t.Fatalf("queued notes = %+v", notes)
	}
	if rec, _ := s.chats.get(page); rec.Version != 2 {
		t.Fatalf("record version = %d, want 2", rec.Version)
	}
}

func TestRemovingAChatDeletesItsDirectory(t *testing.T) {
	s, fh, page, session := chatReuseHub(t)
	name, dir := firstChat(t, s, fh, page, session)
	rec, _ := s.chats.get(page)
	code, r := userReq(t, s, "POST", "/api/remove", `{"agent":"`+rec.Agent[len("desk/"):]+`","name":"`+name+`"}`)
	if code != 200 || r["closed"] == "" || r["chat_dir"] != dir {
		t.Fatalf("remove = %d %v", code, r)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the directory stays after the removal: %v", err)
	}
	// The record went with it.
	if _, ok := s.chats.get(page); ok {
		t.Fatal("the record stays")
	}
	b, _ := os.ReadFile(filepath.Join(s.cfg.StateDir, "chats.json"))
	if strings.Contains(string(b), page) {
		t.Fatalf("chats.json still names the page: %s", b)
	}
}

func TestRemoveChatDirOnlyDeletesAChatsDirectory(t *testing.T) {
	s, _, page, _ := chatReuseHub(t)
	base := filepath.Join(s.cfg.StateDir, "chats")
	mk := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := mk(filepath.Join(base, page+"-123"))
	outside := mk(filepath.Join(t.TempDir(), page+"-123"))
	odd := mk(filepath.Join(base, "notes"))
	nested := mk(filepath.Join(base, page+"-9", "inner"))
	link := filepath.Join(base, page+"-77")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, odd, nested, base, s.cfg.StateDir, link, "", "relative", filepath.Join(base, "..", "chats", "notes")} {
		if ok, err := s.removeChatDir(p); ok || err != nil {
			t.Errorf("removeChatDir(%q) = %v, %v: it must leave this alone", p, ok, err)
		}
		if p != "" && p != "relative" && p != link {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%q is gone", p)
			}
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("a symlink led the removal out of chats/")
	}
	if ok, err := s.removeChatDir(good); !ok || err != nil {
		t.Fatalf("removeChatDir(good) = %v, %v", ok, err)
	}
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Fatal("the chat's directory stays")
	}
}

func TestRemovingAnOrdinaryAgentTouchesNoChatDirectory(t *testing.T) {
	s, fh, page, session := chatReuseHub(t)
	_, dir := firstChat(t, s, fh, page, session)
	// A worker whose directory is not in chats/ is closed, and chats/ is left alone.
	fh.set(func(f *fakeLaunchHerdr) {
		f.agents = append(f.agents, herdr.AgentInfo{Name: "other", PaneID: "w5:p1", WorkspaceID: "w5", Agent: "pi", Status: herdr.StatusIdle, CWD: s.cfg.LaunchRoots[0]})
	})
	waitFleet(t, s, func(as []herdr.AgentInfo) bool {
		for _, a := range as {
			if a.Name == "other" {
				return true
			}
		}
		return false
	})
	if code, r := userReq(t, s, "POST", "/api/remove", `{"agent":"w5:p1","name":"other"}`); code != 200 || r["chat_dir"] != nil {
		t.Fatalf("remove = %d %v", code, r)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the chat's directory went: %v", err)
	}
}

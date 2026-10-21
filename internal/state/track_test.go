package state

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhjaggars/herdr-phone/internal/herdr"
)

func TestTracker(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	a := func(id string, s herdr.AgentStatus) herdr.AgentInfo {
		return herdr.AgentInfo{ID: id, PaneID: "p", Status: s}
	}

	if got := tr.Observe([]herdr.AgentInfo{a("h/p1", herdr.StatusBlocked)}, now); len(got) != 0 {
		t.Fatalf("first observation must only baseline: %+v", got)
	}
	got := tr.Observe([]herdr.AgentInfo{a("h/p1", herdr.StatusDone), a("h/p2", herdr.StatusBlocked)}, now.Add(time.Second))
	if len(got) != 1 || got[0].Agent.ID != "h/p1" || got[0].Before != herdr.StatusBlocked {
		t.Fatalf("transitions = %+v (a new agent baselines)", got)
	}
	if !tr.Since("h/p1").Equal(now.Add(time.Second)) {
		t.Fatalf("since = %v", tr.Since("h/p1"))
	}
	if before, changed := tr.Update("h/p2", herdr.StatusWorking, now); !changed || before != herdr.StatusBlocked {
		t.Fatalf("update = %v %v", before, changed)
	}
	if _, changed := tr.Update("h/p2", herdr.StatusWorking, now); changed {
		t.Fatal("same status is not a transition")
	}
	// Keys are host-qualified: the same pane on another host is distinct.
	if got := tr.Observe([]herdr.AgentInfo{a("h/p1", herdr.StatusDone), a("other/p1", herdr.StatusBlocked)}, now); len(got) != 0 {
		t.Fatalf("transitions = %+v", got)
	}
	tr.Reset()
	if _, changed := tr.Update("h/p1", herdr.StatusBlocked, now); changed {
		t.Fatal("after Reset nothing is baselined")
	}
	if got := tr.Observe([]herdr.AgentInfo{a("h/p1", herdr.StatusBlocked)}, now); len(got) != 0 {
		t.Fatalf("after Reset the next observation is a baseline: %+v", got)
	}
}

func TestAgentKeyAndRefs(t *testing.T) {
	local := &herdr.AgentInfo{PaneID: "wS:p1"}
	if AgentKey(local) != "wS:p1" || QualifiedID("", "wS:p1") != "wS:p1" {
		t.Fatal("no host: pane id")
	}
	local.Host, local.ID = "desk", QualifiedID("desk", "wS:p1")
	if r := LocalRef(local); r.Key != "desk/wS:p1" || r.Route != "wS:p1" || !r.Local {
		t.Fatalf("local ref = %+v", r)
	}
	peer := &herdr.AgentInfo{PaneID: "wS:p1", Host: "kube", ID: "kube/wS:p1"}
	if r := PeerRef(peer); r.Key != "kube/wS:p1" || r.Route != "kube/wS:p1" || r.Local {
		t.Fatalf("peer ref = %+v", r)
	}
}

func TestPresenceKeysOnQualifiedID(t *testing.T) {
	fp := &fakePusher{}
	d := &Dispatcher{Push: fp, Viewer: fakeViewer{"desk/wS:p1": true, "wS:p2": true, "kube/wS:p3": true}}
	notify := func(a *herdr.AgentInfo, ref Ref) { d.Notify(a, ref, nil) }
	mk := func(host, pane string) *herdr.AgentInfo {
		return &herdr.AgentInfo{Name: pane, PaneID: pane, Host: host, ID: QualifiedID(host, pane), Status: herdr.StatusDone}
	}
	notify(mk("desk", "wS:p1"), LocalRef(mk("desk", "wS:p1"))) // viewed by id
	notify(mk("desk", "wS:p2"), LocalRef(mk("desk", "wS:p2"))) // viewed by bare pane (older client)
	notify(mk("kube", "wS:p2"), PeerRef(mk("kube", "wS:p2")))  // a bare pane never matches a peer agent
	notify(mk("kube", "wS:p3"), PeerRef(mk("kube", "wS:p3")))  // viewed by id
	notify(mk("kube", "wS:p1"), PeerRef(mk("kube", "wS:p1")))  // same pane id as a viewed local agent, other host
	if len(fp.keys) != 2 || fp.keys[0] != "kube/wS:p2/done" || fp.keys[1] != "kube/wS:p1/done" {
		t.Fatalf("pushed %v", fp.keys)
	}
}

// fakeHerdr answers agent.list and session.snapshot on a unix socket.
type fakeHerdr struct {
	mu     sync.Mutex
	agents []herdr.AgentInfo
}

func (f *fakeHerdr) set(agents ...herdr.AgentInfo) {
	f.mu.Lock()
	f.agents = agents
	f.mu.Unlock()
}

func startFakeHerdr(t *testing.T) (*fakeHerdr, *herdr.Client) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hp") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	f := &fakeHerdr{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req struct{ ID, Method string }
				_ = json.Unmarshal(line, &req)
				var result any
				switch req.Method {
				case "agent.list":
					f.mu.Lock()
					result = map[string]any{"type": "agent_list", "agents": f.agents}
					f.mu.Unlock()
				default:
					result = map[string]any{"snapshot": map[string]any{"workspaces": []any{}}}
				}
				b, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
				c.Write(append(b, '\n'))
			}()
		}
	}()
	return f, herdr.NewClient(sock)
}

// A single host behaves as before: the same transitions notify, with the
// old pane-only deep link; agents now also carry host and id.
func TestManagerSingleHost(t *testing.T) {
	fh, client := startFakeHerdr(t)
	fh.set(herdr.AgentInfo{Name: "fixer", PaneID: "wS:p1", Status: herdr.StatusWorking, CWD: "/x/repo"})
	m := New(client, nil)
	m.SetHost("desk")
	fp := &fakePusher{}
	m.SetPush(fp, fakeViewer{})
	ctx := context.Background()
	if err := m.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	a := m.Agents()
	if len(a) != 1 || a[0].Host != "desk" || a[0].ID != "desk/wS:p1" || a[0].LastActive == nil {
		t.Fatalf("agents = %+v", a)
	}
	fh.set(herdr.AgentInfo{Name: "fixer", PaneID: "wS:p1", Status: herdr.StatusBlocked, CWD: "/x/repo"})
	if err := m.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fp.msgs) != 1 || fp.msgs[0].URL != "/#/agent/wS:p1" || fp.msgs[0].Body != "repo\nwaiting for a response" {
		t.Fatalf("pushes = %+v", fp.msgs)
	}
	// The event fast path shares the tracker: blocked -> done notifies once.
	m.applyStatus("wS:p1", herdr.StatusDone)
	if len(fp.msgs) != 2 || fp.msgs[1].Title != "fixer is done" {
		t.Fatalf("pushes = %+v", fp.msgs)
	}
	if err := m.refresh(ctx); err != nil { // herdr still says blocked: a real transition back
		t.Fatal(err)
	}
	if len(fp.msgs) != 3 {
		t.Fatalf("pushes = %d", len(fp.msgs))
	}
}

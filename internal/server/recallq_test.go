package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/archive"
	"github.com/jhjaggars/comebye/internal/fleet"
)

// `comebye recall` on a peer reads the hub's index over the API: the peer
// has no index of its own, and both an agent's question and the user's go to
// the hub, which answers from its index.
func TestRecallOnAPeerReadsTheHubsIndex(t *testing.T) {
	idx := recallIndex(t)
	hub := newServer(t, Config{InsecureNoAuth: true, HostName: "desk", StateDir: t.TempDir(), RecallIndex: idx}, nil, nil)
	hs := httptest.NewServer(hub.Handler())
	defer hs.Close()
	up, err := fleet.NewUpstream(hs.URL, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	peer := newServer(t, Config{AllowLogins: []string{"me@example.com"}, HostName: "kube", StateDir: t.TempDir(), Upstream: up, AgentToken: testToken,
		RecallIndex: "/nonexistent/index.db"}, nil, nil)
	user := map[string]string{"Tailscale-User-Login": "me@example.com"}
	for who, hdr := range map[string]map[string]string{"agent": agentHdr("w1:p1"), "user": user} {
		for _, q := range []string{`{"op":"search","args":["zebra"]}`, `{"op":"list","filter":{"Host":"desk"}}`} {
			w := do(peer, "POST", "/api/recall/query", hdr, q)
			if w.Code != http.StatusOK {
				t.Fatalf("%s %s: %d %s", who, q, w.Code, w.Body)
			}
			var a archive.RecallAnswer
			if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
				t.Fatal(err)
			}
			if len(a.Rows) != 1 || a.Rows[0]["id"] != recallSID {
				t.Fatalf("%s %s: rows = %+v", who, q, a.Rows)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s: Cache-Control = %q", who, w.Header().Get("Cache-Control"))
			}
		}
		// show, with the session's messages.
		w := do(peer, "POST", "/api/recall/query", hdr, `{"op":"show","args":["`+recallSID+`"],"text":5}`)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "zebra") {
			t.Fatalf("%s show: %d %s", who, w.Code, w.Body)
		}
	}
	// A bad question, and a hub with no index.
	if w := do(hub, "POST", "/api/recall/query", nil, `{"op":"drop"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown op = %d", w.Code)
	}
	if w := do(hub, "POST", "/api/recall/query", nil, `{"op":"search"}`); w.Code != http.StatusBadRequest {
		t.Errorf("search without words = %d", w.Code)
	}
	bare := newServer(t, Config{InsecureNoAuth: true, HostName: "x", StateDir: t.TempDir()}, nil, nil)
	if w := do(bare, "POST", "/api/recall/query", nil, `{"op":"list"}`); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "no_index") {
		t.Errorf("no index = %d %s", w.Code, w.Body)
	}
}

// The other recall routes stay the user's alone; the query route admits the
// agent role (a peer's forwarded agent), as the CLI is its only client.
func TestRecallQueryAdmitsAForwardedAgent(t *testing.T) {
	r := newReadRig(t)
	r.s.cfg.RecallIndex = recallIndex(t)
	for name, hdr := range map[string]map[string]string{
		"agent token": {HeaderAgentToken: testToken, HeaderAgent: "wH:p1"},
		"forwarded":   forwarded("kube", "wC:p1"),
	} {
		code, body := r.call(r.main, "POST", "/api/recall/query", `{"op":"search","args":["zebra"]}`, hdr)
		if code != http.StatusOK || !strings.Contains(body, recallSID) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	// A tagged worker never reaches the index.
	if code, body := r.asTagged("POST", "/api/recall/query", `{"op":"search","args":["zebra"]}`); code != http.StatusForbidden || strings.Contains(body, recallSID) {
		t.Errorf("tagged worker: %d %s", code, body)
	}
}

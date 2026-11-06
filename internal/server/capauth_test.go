package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/tsauth"
)

const (
	testCap    = "example.com/cap/peer"
	capValue   = `{"example.com/cap/peer":[{"role":"worker"}]}`
	workerAddr = "100.64.0.7"
)

// setHolder makes WhoIs report a node holding the capability.
func (f *fakeWhoIs) setHolder(node string, tags ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.id = tsauth.Identity{Node: node, Tags: tags, Caps: map[string]json.RawMessage{testCap: json.RawMessage(`[{"role":"worker"}]`)}}
	f.err = nil
}

func (f *fakeWhoIs) lastAddr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addr
}

func newCapRig(t *testing.T, selfRegister bool) *tagRig {
	t.Helper()
	return newRigWith(t, selfRegister, func(c *Config) { c.AllowCap = testCap })
}

// serve calls the normal handler with the headers Serve sends for a tagged
// caller holding the capability, plus extra.
func (r *tagRig) serve(method, path, body string, extra map[string]string) (int, string) {
	r.t.Helper()
	h := map[string]string{tsauth.HeaderAppCaps: capValue, "X-Forwarded-For": workerAddr}
	for k, v := range extra {
		h[k] = v
	}
	return r.call(r.main, method, path, body, h)
}

// peer is serve with no extra headers.
func (r *tagRig) peer(method, path, body string) (int, string) {
	return r.serve(method, path, body, nil)
}

func (r *tagRig) addPeer(name, url, tag string) {
	r.t.Helper()
	b, _ := json.Marshal(map[string]any{"name": name, "urls": []string{url}, "tag": tag})
	if code, body := r.call(r.main, "POST", "/api/peers", string(b), map[string]string{"Tailscale-User-Login": "me@example.com"}); code != http.StatusCreated {
		r.t.Fatalf("user adds the peer: %d %s", code, body)
	}
}

func TestCapThroughServeIsTheForwardedAgentOnly(t *testing.T) {
	r := newCapRig(t, false)
	r.addPeer("w1", "https://w1.t.ts.net", "tag:worker")
	r.who.setHolder("w1.t.ts.net", "tag:worker")

	// The peer's name is the host, whatever the request claims, and WhoIs
	// is asked about the address Serve vouched for.
	code, body := r.serve("GET", "/api/whoami", "", map[string]string{HeaderAgent: "w:1", HeaderAgentHost: "someone-else"})
	var who struct{ Role, Agent, Host string }
	json.Unmarshal([]byte(body), &who)
	if code != 200 || who.Role != "agent" || who.Agent != "w1/w:1" {
		t.Fatalf("whoami: %d %s", code, body)
	}
	if got := r.who.lastAddr(); got != workerAddr {
		t.Fatalf("WhoIs asked about %q", got)
	}
	if code, _ := r.serve("GET", "/api/hosts", "", nil); code != 200 {
		t.Fatalf("agent route: %d", code)
	}
	// Every kind of user route is refused.
	for _, c := range []struct{ m, p, b string }{
		{"POST", "/api/launch", `{}`},
		{"GET", "/api/peers", ""},
		{"POST", "/api/peers", `{"name":"x","urls":["https://x.t.ts.net"]}`},
		{"DELETE", "/api/peers/w1", ""},
		{"POST", "/api/removals/confirm", `{}`},
		{"GET", "/api/events", ""},
	} {
		if code, body := r.serve(c.m, c.p, c.b, nil); code != http.StatusForbidden || !strings.Contains(body, "agent_forbidden") {
			t.Errorf("%s %s: %d %s", c.m, c.p, code, body)
		}
	}
	// No UI, though the same URL serves it to the user.
	if code, _ := r.serve("GET", "/", "", nil); code != http.StatusNotFound {
		t.Errorf("UI for a tagged caller: %d", code)
	}
	// The agent token and forwarding claims don't change the attribution.
	for _, h := range []map[string]string{
		{HeaderAgentToken: "tok"},
		{HeaderAgentToken: "wrong"},
		{HeaderForwarded: "1", HeaderAgentHost: "someone-else"},
	} {
		h[HeaderAgent] = "w:1"
		code, body := r.serve("GET", "/api/whoami", "", h)
		var who struct{ Role, Agent string }
		json.Unmarshal([]byte(body), &who)
		if code != 200 || who.Role != "agent" || who.Agent != "w1/w:1" {
			t.Errorf("claims %v: %d %s", h, code, body)
		}
	}
}

func TestCapThroughServeRefusals(t *testing.T) {
	r := newCapRig(t, false)
	r.addPeer("w1", "https://w1.t.ts.net", "tag:worker")

	check := func(name string, wantCode int, wantBody string, hdr map[string]string) {
		t.Helper()
		code, body := r.serve("GET", "/api/whoami", "", hdr)
		if code != wantCode || !strings.Contains(body, wantBody) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	// WhoIs must agree with the header.
	r.who.set("w1.t.ts.net", "tag:worker")
	check("WhoIs shows no capability", http.StatusForbidden, "cap_not_held", nil)
	r.who.setHolder("laptop.t.ts.net")
	check("user-owned node, no login", http.StatusForbidden, "cap_not_held", nil)
	r.who.err = tsauth.ErrUnknown
	check("address unknown", http.StatusUnauthorized, "unauthorized", nil)
	r.who.err = errors.New("tailscaled down")
	check("tailscaled down", http.StatusUnauthorized, "unauthorized", nil)

	// Serve sets one plain address.
	r.who.setHolder("w1.t.ts.net", "tag:worker")
	check("no address", http.StatusUnauthorized, "unauthorized", map[string]string{"X-Forwarded-For": ""})
	check("address list", http.StatusUnauthorized, "unauthorized", map[string]string{"X-Forwarded-For": "100.64.0.7, 100.64.0.8"})
	check("not an address", http.StatusUnauthorized, "unauthorized", map[string]string{"X-Forwarded-For": "w1.t.ts.net"})
	if code, _ := r.call(r.main, "GET", "/api/whoami", "", map[string]string{tsauth.HeaderAppCaps: capValue}); code != http.StatusUnauthorized {
		t.Errorf("no address header: %d", code)
	}
	q, _ := http.NewRequest("GET", r.main.URL+"/api/whoami", nil)
	q.Header.Set(tsauth.HeaderAppCaps, capValue)
	q.Header.Add("X-Forwarded-For", workerAddr)
	q.Header.Add("X-Forwarded-For", workerAddr)
	resp, err := http.DefaultClient.Do(q)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("two address headers: %d", resp.StatusCode)
	}

	// Registered peers only; a peer's recorded tag must still be there.
	r.who.setHolder("other.t.ts.net", "tag:worker")
	check("not a peer", http.StatusForbidden, "not_a_peer", nil)
	r.who.setHolder("w1.t.ts.net", "tag:other")
	check("peer's tag gone", http.StatusForbidden, "tag_mismatch", nil)
}

// Without the capability in the header nothing changes: WhoIs isn't asked,
// the API is closed, the UI is what it always was.
func TestCapHeaderWithoutTheCapChangesNothing(t *testing.T) {
	r := newCapRig(t, false)
	r.addPeer("w1", "https://w1.t.ts.net", "tag:worker")
	r.who.setHolder("w1.t.ts.net", "tag:worker")
	for name, h := range map[string]map[string]string{
		"no header":     {"X-Forwarded-For": workerAddr},
		"other cap":     {"X-Forwarded-For": workerAddr, tsauth.HeaderAppCaps: `{"example.com/cap/other":[{}]}`},
		"none accepted": {"X-Forwarded-For": workerAddr, tsauth.HeaderAppCaps: `{}`},
		"not JSON":      {"X-Forwarded-For": workerAddr, tsauth.HeaderAppCaps: testCap},
		// Tailscale 1.104 lets this variant through; Go keeps it apart.
		"underscore variant": {"X-Forwarded-For": workerAddr, "Tailscale_App_Capabilities": capValue},
	} {
		if code, _ := r.call(r.main, "GET", "/api/whoami", "", h); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
		if got := r.who.lastAddr(); got != "" {
			t.Errorf("%s: WhoIs asked about %q", name, got)
		}
	}
	if code, _ := r.call(r.main, "GET", "/", "", nil); code != http.StatusOK {
		t.Errorf("UI without the capability: %d", code)
	}
}

func TestCapDoesNotTouchUsers(t *testing.T) {
	r := newCapRig(t, false)
	r.who.setHolder("w1.t.ts.net", "tag:worker")
	// A user whose device holds the grant is the user.
	code, body := r.serve("GET", "/api/whoami", "", map[string]string{"Tailscale-User-Login": "me@example.com"})
	if code != 200 || !strings.Contains(body, `"role":"user"`) {
		t.Fatalf("user: %d %s", code, body)
	}
	if code, _ := r.serve("GET", "/", "", map[string]string{"Tailscale-User-Login": "me@example.com"}); code != 200 {
		t.Fatalf("user's UI: %d", code)
	}
	// A login not on the allowlist gets no second chance through the grant.
	if code, _ := r.serve("GET", "/api/whoami", "", map[string]string{"Tailscale-User-Login": "stranger@example.com"}); code != http.StatusUnauthorized {
		t.Fatalf("stranger with the grant: %d", code)
	}
	if got := r.who.lastAddr(); got != "" {
		t.Fatalf("WhoIs asked for a login: %q", got)
	}
	// The local CLI's token works as before.
	code, body = r.call(r.main, "GET", "/api/whoami", "", map[string]string{HeaderAgentToken: "tok"})
	if code != 200 || !strings.Contains(body, `"role":"agent"`) {
		t.Fatalf("token: %d %s", code, body)
	}
}

func TestCapOffIgnoresTheHeader(t *testing.T) {
	r := newRigWith(t, false, func(*Config) {}) // no -ts-allow-cap
	r.addPeer("w1", "https://w1.t.ts.net", "tag:worker")
	r.who.setHolder("w1.t.ts.net", "tag:worker")
	if code, _ := r.serve("GET", "/api/whoami", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("capability off: %d", code)
	}
	if got := r.who.lastAddr(); got != "" {
		t.Fatalf("WhoIs asked: %q", got)
	}
}

// The trust model, as docs/security.md states it: the headers can be forged
// by a local process on the loopback port. Serve is what removes a tailnet
// client's copies, so this rig (no Serve in front) is that local process.
// It reaches the agent role, as a node really holding the grant, and no more.
func TestCapForgedOnLoopbackIsBounded(t *testing.T) {
	r := newCapRig(t, false)
	r.addPeer("w1", "https://w1.t.ts.net", "tag:worker")
	r.who.setHolder("w1.t.ts.net", "tag:worker")
	code, body := r.serve("GET", "/api/whoami", "", nil)
	if code != 200 || !strings.Contains(body, `"role":"agent"`) {
		t.Fatalf("forged for a holder: %d %s", code, body)
	}
	if code, body := r.serve("POST", "/api/launch", `{}`, nil); code != http.StatusForbidden || !strings.Contains(body, "agent_forbidden") {
		t.Fatalf("forged, user route: %d %s", code, body)
	}
	// A node without the grant can't be claimed: WhoIs has the last word.
	r.who.set("w1.t.ts.net", "tag:worker")
	if code, body := r.serve("GET", "/api/whoami", "", nil); code != http.StatusForbidden || !strings.Contains(body, "cap_not_held") {
		t.Fatalf("forged for a non-holder: %d %s", code, body)
	}
}

func TestCapAnnounceThroughServe(t *testing.T) {
	r := newCapRig(t, true)
	r.who.setHolder(r.node, "tag:worker")
	if code, body := r.serve("GET", "/api/whoami", "", nil); code != http.StatusForbidden || !strings.Contains(body, "not_a_peer") {
		t.Fatalf("before announcing: %d %s", code, body)
	}
	code, body := r.serve("POST", "/api/peers/announce", `{"name":"vm1","url":"`+r.workerURL()+`"}`, nil)
	if code != http.StatusCreated || !strings.Contains(body, `"tag":"tag:worker"`) || !strings.Contains(body, `"added_by":"tag:worker@`+r.node+`"`) {
		t.Fatalf("announce: %d %s", code, body)
	}
	if code, body := r.serve("GET", "/api/whoami", "", map[string]string{HeaderAgent: "w:1"}); code != 200 || !strings.Contains(body, `"agent":"vm1/w:1"`) {
		t.Fatalf("after announcing: %d %s", code, body)
	}
	// A node can register only itself.
	if code, _ := r.serve("POST", "/api/peers/announce", `{"name":"vm2","url":"https://other.t.ts.net"}`, nil); code == http.StatusCreated || code == http.StatusOK {
		t.Fatalf("announced another node: %d", code)
	}
	// Self-registration off refuses it.
	off := newCapRig(t, false)
	off.who.setHolder(off.node, "tag:worker")
	if code, body := off.serve("POST", "/api/peers/announce", `{"name":"vm1","url":"`+off.workerURL()+`"}`, nil); code != http.StatusForbidden || !strings.Contains(body, "self_register_off") {
		t.Fatalf("self-registration off: %d %s", code, body)
	}
}

package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

const settingsJSON = `{"host":"csb-vm","settings":{"default_kind":"pi","kinds":{"pi":{"model":"anthropic/sonnet","thinking":"high"}},"rev":4,"updated_by":"me@example.com","updated_at":"2026-11-05T10:00:00Z"},
 "effective":{"default_kind":"pi","default_source":"setting","allowed_kinds":["pi"],"allowed_source":"flag"},
 "installed_kinds":["pi","codex"],"launch":{"enabled":true}}`

func TestSettingsShow(t *testing.T) {
	e, out, _, reqs := peerEnv(t, nil, map[string]answer{"GET /api/hosts/csb-vm/settings": {200, settingsJSON}})
	if code := run(e, "settings", "--host", "csb-vm"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"Settings on csb-vm (revision 4, by me@example.com, 2026-11-05)", "default agent: pi", "allowed:       pi  (from -default-kind", "installed:     pi, codex", "pi:            anthropic/sonnet (thinking high)", "launching:     on"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// A coordinator reads with its agent token, never a login.
	if h := (*reqs)[0].hdr; h.Get(headerToken) == "" || h.Get("Tailscale-User-Login") != "" {
		t.Fatalf("headers = %v", h)
	}
}

func TestSettingsSetMergesAndSendsTheRevision(t *testing.T) {
	e, out, _, reqs := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, map[string]answer{
		"GET /api/settings": {200, settingsJSON},
		"PUT /api/settings": {200, settingsJSON},
	})
	if code := run(e, "settings", "set", "--allow", "pi,codex", "--model", "codex=gpt-5", "--thinking", "pi=low"); code != 0 {
		t.Fatalf("exit %d %s", code, out)
	}
	put := (*reqs)[1]
	var sent struct {
		DefaultKind  string                 `json:"default_kind"`
		AllowedKinds []string               `json:"allowed_kinds"`
		Kinds        map[string]apiKindConf `json:"kinds"`
		Rev          int                    `json:"rev"`
	}
	json.Unmarshal([]byte(put.body), &sent)
	// What was not mentioned is kept; pi's model stays when its thinking changes.
	if put.method != "PUT" || sent.Rev != 4 || sent.DefaultKind != "pi" || strings.Join(sent.AllowedKinds, ",") != "pi,codex" ||
		sent.Kinds["pi"].Model != "anthropic/sonnet" || sent.Kinds["pi"].Thinking != "low" || sent.Kinds["codex"].Model != "gpt-5" {
		t.Fatalf("sent %s", put.body)
	}
	if h := put.hdr; h.Get(headerToken) != "" || h.Get("Tailscale-User-Login") != "me@example.com" {
		t.Fatalf("a write goes out as the user: %v", h)
	}
}

func TestSettingsClearAndLogin(t *testing.T) {
	e, _, _, reqs := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, map[string]answer{
		"GET /api/hosts/kube/settings": {200, settingsJSON},
		"PUT /api/hosts/kube/settings": {200, settingsJSON},
	})
	if code := run(e, "settings", "clear", "--host", "kube", "--field", "models"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if b := (*reqs)[1].body; !strings.Contains(b, `"default_kind":"pi"`) || !strings.Contains(b, `"kinds":{}`) {
		t.Fatalf("clear models sent %s", b)
	}
	// Without a login, writing is refused before any request; so are bad flags.
	e, _, errb, reqs := peerEnv(t, nil, nil)
	if code := run(e, "settings", "set", "--default-kind", "pi"); code != 2 || !strings.Contains(errb.String(), "--login") || len(*reqs) != 0 {
		t.Fatalf("no login: %d %s", code, errb)
	}
	e, _, errb, _ = peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, nil)
	for _, args := range [][]string{{"settings", "frob"}, {"settings", "set"}, {"settings", "set", "--model", "nokind"}, {"settings", "clear", "--field", "x"}, {"settings", "set", "--field", "models", "--allow", "pi"}} {
		if code := run(e, args...); code != 2 {
			t.Errorf("%v: exit %d %s", args, code, errb)
		}
	}
}

func TestSettingsOnAnOldPeer(t *testing.T) {
	e, _, errb, _ := peerEnv(t, nil, nil) // every route is a 404
	if code := run(e, "settings", "--host", "old"); code != 1 || !strings.Contains(errb.String(), "too old") {
		t.Fatalf("exit %d %s", code, errb)
	}
}

func TestPeerListShowsDefaultsAndPeerShow(t *testing.T) {
	answers := map[string]answer{
		"GET /api/peers":               {200, peersJSON},
		"GET /api/hosts/kube/settings": {200, settingsJSON},
		"GET /api/hosts/kube/models":   {200, `{"host":"kube","catalogs":[{"kind":"pi","default":"m1","models":[{"id":"m1"},{"id":"m2"}],"source":"pi --list-models"}]}`},
		"GET /api/hosts/w1/settings":   {404, `{"code":"not_found"}`}, // too old
	}
	e, out, _, _ := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	if code := run(e, "peer", "list"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(out.String(), "\n")
	if !strings.Contains(lines[0], "DEFAULT") {
		t.Fatalf("header: %s", lines[0])
	}
	find := func(name string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, name+" ") {
				return l
			}
		}
		return ""
	}
	if l := find("kube"); !strings.Contains(l, "pi (anthropic/sonnet)") {
		t.Errorf("kube: %q", l)
	}
	for _, n := range []string{"w1", "w2"} { // too old, and down
		if l := find(n); !strings.Contains(l, " ? ") {
			t.Errorf("%s: %q", n, l)
		}
	}

	e, out, _, _ = peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	if code := run(e, "peer", "show", "kube"); code != 0 {
		t.Fatalf("show: exit %d", code)
	}
	for _, want := range []string{"Peer kube (static)", "status:   ok, 3 agents", "default agent: pi", "pi models: 2 (default m1)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	e, out, _, _ = peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	if code := run(e, "peer", "show", "w1"); code != 0 || !strings.Contains(out.String(), "too old to have host settings") {
		t.Fatalf("old peer: %d %s", code, out)
	}
	e, out, _, _ = peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	if code := run(e, "peer", "show", "w2"); code != 0 || !strings.Contains(out.String(), "not answering") {
		t.Fatalf("down peer: %d %s", code, out)
	}
	e, _, errb, _ := peerEnv(t, map[string]string{"COMEBYE_LOGIN": "me@example.com"}, answers)
	if code := run(e, "peer", "show", "nope"); code != 1 || !strings.Contains(errb.String(), "no peer named nope") {
		t.Fatalf("unknown: %d %s", code, errb)
	}
}

func TestHandoffPrintsHostSettingsWarningToStderr(t *testing.T) {
	e, out, errb, _ := peerEnv(t, nil, map[string]answer{"POST /api/handoffs": {200, `{"entry":{"id":"h1","host":"csb-vm","branch":"b","kind":"claude","kind_source":"request","agent_name":"b","agent_id":"csb-vm/w1:p1"},"worker":{"warnings":["claude was requested; this host's default is pi"]}}`}})
	e.Stdin = strings.NewReader("brief")
	if code := run(e, "handoff", "--host", "csb-vm", "--repo", "/r", "--branch", "b", "--kind", "claude", "--brief", "-"); code != 0 {
		t.Fatalf("exit %d %s", code, errb)
	}
	if !strings.Contains(errb.String(), "warning: claude was requested; this host's default is pi") || strings.Contains(out.String(), "was requested") {
		t.Fatalf("stderr %q stdout %q", errb, out)
	}
	if !strings.Contains(out.String(), "agent:    claude (asked for)") {
		t.Fatalf("stdout %q", out)
	}
}

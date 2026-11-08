package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
)

func TestParseKindArgs(t *testing.T) {
	got := parseKindArgs(hostList{"pi=--auto; claude=--model opus", "", "pi= --verbose ", "codex=--tools a,b"})
	want := map[string][]string{
		"pi":     {"--auto", "--verbose"},
		"claude": {"--model", "opus"},
		"codex":  {"--tools", "a,b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseKindArgs = %v, want %v", got, want)
	}
	if got := parseKindArgs(hostList{""}); len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
}

func TestWaitForHerdr(t *testing.T) {
	dir, err := os.MkdirTemp("", "waitforherdr")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "h.sock")
	client := herdr.NewClient(sock)
	fast := func(time.Duration) { time.Sleep(2 * time.Millisecond) }

	// No socket: herdr has not started yet — waitForHerdr keeps waiting
	// instead of exiting (the post-reboot crash loop).
	done := make(chan error, 1)
	go func() {
		waitForHerdr(client, sock, fast)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("returned while herdr's socket does not exist")
	case <-time.After(100 * time.Millisecond):
	}

	// A herdr appears on the socket: the wait ends.
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
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
				var req struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(line, &req)
				b, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]any{"type": "agent_list", "agents": []any{}}})
				c.Write(append(b, '\n'))
			}()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not return once herdr answered")
	}
}

func TestDefaultSocketPath(t *testing.T) {
	for _, tc := range []struct {
		comebye, herdrEnv, want string
	}{
		{"", "", herdr.DefaultSocketPath},
		{"", "/run/herdr.sock", "/run/herdr.sock"},
		{"/srv/h.sock", "/run/herdr.sock", "/srv/h.sock"},
		{"~/h.sock", "", "~/h.sock"},
	} {
		t.Setenv("COMEBYE_SOCK", tc.comebye)
		t.Setenv("HERDRPHONE_SOCK", "")
		t.Setenv("HERDR_SOCKET_PATH", tc.herdrEnv)
		if got := defaultSocketPath(); got != tc.want {
			t.Errorf("COMEBYE_SOCK=%q HERDR_SOCKET_PATH=%q: got %q, want %q", tc.comebye, tc.herdrEnv, got, tc.want)
		}
	}
}

func TestReleaseFlagsAreRefusedWithoutAnAllowlist(t *testing.T) {
	for _, c := range []struct {
		name          string
		noAuth        bool
		repo, host    string
		up, sim       bool
		addr          string
		noLedger, hub bool
		wantErr       bool
	}{
		{"repo with allowlist", false, "/x", "", false, false, "127.0.0.1:8741", false, false, false},
		{"upgrade with allowlist", false, "", "", true, false, "127.0.0.1:8741", false, false, false},
		{"repo, no auth", true, "/x", "", false, false, "127.0.0.1:8741", false, false, true},
		{"upgrade, no auth", true, "", "", true, false, "127.0.0.1:8741", false, false, true},
		{"nothing, no auth", true, "", "", false, false, "127.0.0.1:8741", false, false, false},
		{"simulate, no auth, loopback (the browser tests)", true, "", "", false, true, "127.0.0.1:18741", false, false, false},
		{"simulate off loopback", false, "", "", false, true, "0.0.0.0:8741", false, false, true},
		{"simulate with repo", false, "/x", "", false, true, "127.0.0.1:8741", false, false, true},
		{"simulate with upgrade", false, "", "", true, true, "127.0.0.1:8741", false, false, true},
		{"upgrade, no ledger, no hub", false, "", "", true, false, "127.0.0.1:8741", true, false, true},
		{"upgrade, no ledger, with hub (a peer)", false, "", "", true, false, "127.0.0.1:8741", true, true, false},
		{"upgrade, with ledger, hub or standalone", false, "", "", true, false, "127.0.0.1:8741", false, false, false},
		{"release host on the hub", false, "", "laharl", false, false, "127.0.0.1:8741", false, false, false},
		{"release host, no auth", true, "", "laharl", false, false, "127.0.0.1:8741", false, false, true},
		{"release host and a local repo are ambiguous", false, "/x", "laharl", false, false, "127.0.0.1:8741", false, false, true},
		{"release host on a peer (-hub)", false, "", "laharl", false, false, "127.0.0.1:8741", false, true, true},
		{"release host with no ledger", false, "", "laharl", false, false, "127.0.0.1:8741", true, false, true},
		{"release host with simulate", false, "", "laharl", false, true, "127.0.0.1:8741", false, false, true},
		{"repo on a peer (the hub tags through it)", false, "/x", "", false, false, "127.0.0.1:8741", true, true, false},
		{"repo, no ledger, no hub", false, "/x", "", false, false, "127.0.0.1:8741", true, false, true},
	} {
		if err := releaseFlagError(c.noAuth, c.repo, c.host, c.up, c.sim, c.addr, c.noLedger, c.hub); (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.wantErr)
		}
	}
}

func TestParseAutoTodo(t *testing.T) {
	ten, three := 10*time.Minute, 3*time.Minute
	for _, c := range []struct {
		spec     string
		blocked  time.Duration
		question time.Duration
		bad      bool
	}{
		{"blocked,question", ten, three, false},
		{"blocked", ten, 0, false},
		{"question", 0, three, false},
		{" Blocked , QUESTION ", ten, three, false},
		{"off", 0, 0, false},
		{"none", 0, 0, false},
		{"", 0, 0, false},
		{"blocked,sometimes", 0, 0, true},
	} {
		got, err := parseAutoTodo(c.spec, ten, three)
		if (err != nil) != c.bad || (err == nil && (got.Blocked != c.blocked || got.Question != c.question)) {
			t.Errorf("%q = %+v, %v", c.spec, got, err)
		}
	}
	if _, err := parseAutoTodo("question", ten, 0); err == nil {
		t.Error("a zero question delay is refused")
	}
}

func TestReleaseHostMustBeAKnownPeer(t *testing.T) {
	known := func(n string) bool { return n == "laharl" }
	for host, wantErr := range map[string]bool{"laharl": false, "laharlx": true, "desk": true, "Bad Name": true, "": true} {
		if err := releaseHostError(host, "desk", known); (err != nil) != wantErr {
			t.Errorf("%q: err = %v, want error %v", host, err, wantErr)
		}
	}
}

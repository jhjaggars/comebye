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
		name    string
		noAuth  bool
		repo    string
		up, sim bool
		addr    string
		wantErr bool
	}{
		{"repo with allowlist", false, "/x", false, false, "127.0.0.1:8741", false},
		{"upgrade with allowlist", false, "", true, false, "127.0.0.1:8741", false},
		{"repo, no auth", true, "/x", false, false, "127.0.0.1:8741", true},
		{"upgrade, no auth", true, "", true, false, "127.0.0.1:8741", true},
		{"nothing, no auth", true, "", false, false, "127.0.0.1:8741", false},
		{"simulate, no auth, loopback (the browser tests)", true, "", false, true, "127.0.0.1:18741", false},
		{"simulate off loopback", false, "", false, true, "0.0.0.0:8741", true},
		{"simulate with repo", false, "/x", false, true, "127.0.0.1:8741", true},
		{"simulate with upgrade", false, "", true, true, "127.0.0.1:8741", true},
	} {
		if err := releaseFlagError(c.noAuth, c.repo, c.up, c.sim, c.addr); (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.wantErr)
		}
	}
}

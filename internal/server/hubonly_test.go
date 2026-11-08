package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhjaggars/comebye/internal/fleet"
	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/hublock"
	"github.com/jhjaggars/comebye/internal/state"
)

// hubOnlyServer is a hub with no herdr over dir, named name.
func hubOnlyServer(t *testing.T, dir, name string) *Server {
	t.Helper()
	client := herdr.NewClient("")
	m := state.New(client, &state.Notifier{})
	f := fleet.New(m, name, nil, fleet.Options{HubMode: true, NoLocalAgents: true, Version: "v-test"})
	return New(Config{AllowLogins: []string{"me@example.com"}, ReadLines: 100, StateDir: dir, AgentToken: testToken,
		Fleet: f, HubOnly: true, Version: "v-test"}, client, m)
}

// copyDir copies src to dst the way the documented tar backup does,
// leaving out hub.lock and the health probe's file.
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == hublock.FileName || rel == ".healthz" {
			return nil
		}
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(filepath.Join(dst, rel), os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A hub with no herdr keeps the ledger-side features, says it hosts no agents,
// and a copy of its state directory brings the same hub up elsewhere.
func TestHubOnlyServesAndItsStateDirRestores(t *testing.T) {
	dir := t.TempDir()
	s := hubOnlyServer(t, dir, "hub")

	w := do(s, "GET", "/api/agents", userHdr, "")
	var got struct {
		Agents []any
		Hosts  []struct {
			Name     string
			Local    bool
			NoAgents bool `json:"no_agents"`
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Hosts) != 1 || !got.Hosts[0].NoAgents || !got.Hosts[0].Local || len(got.Agents) != 0 {
		t.Fatalf("agents = %d %s", w.Code, w.Body)
	}

	// To-dos and the briefing work with no herdr.
	w = do(s, "POST", "/api/todos", userHdr, `{"question":"keep the old VAPID key?","kind":"decision","waiting":"csb-vm/w1:p1"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("todo add = %d %s", w.Code, w.Body)
	}
	if w = do(s, "POST", "/api/brief", userHdr, `{"markdown":"# Morning"}`); w.Code != http.StatusOK {
		t.Fatalf("brief set = %d %s", w.Code, w.Body)
	}

	// Anything that needs a local herdr says so (409 no_herdr), not a 502.
	w = do(s, "POST", "/api/agents/w1:p1/keys", userHdr, `{"keys":["esc"]}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no_herdr") {
		t.Errorf("keys on a hub without herdr = %d %s", w.Code, w.Body)
	}
	if w = do(s, "POST", "/api/launch", userHdr, `{"coordinator":true}`); w.Code != http.StatusForbidden {
		t.Errorf("launch = %d %s", w.Code, w.Body)
	}

	// The backup: a copy of the directory (no lock file) starts the same hub.
	restored := filepath.Join(t.TempDir(), "restored")
	copyDir(t, dir, restored)
	s2 := hubOnlyServer(t, restored, "hub")
	w = do(s2, "GET", "/api/todos", userHdr, "")
	if !strings.Contains(w.Body.String(), "keep the old VAPID key?") || !strings.Contains(w.Body.String(), "csb-vm/w1:p1") {
		t.Fatalf("restored to-dos = %s", w.Body)
	}
	if w = do(s2, "GET", "/api/brief", userHdr, ""); !strings.Contains(w.Body.String(), "# Morning") {
		t.Fatalf("restored brief = %s", w.Body)
	}
}

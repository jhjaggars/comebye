package server

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jhjaggars/comebye/internal/herdr"
)

func TestContextUsageWindows(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(home, ".pi", "agent", "models.json"), []byte(`{"providers":{"homelab":{"models":[{"id":"thinking","contextWindow":180224}]}}}`), 0o600)
	t.Setenv("COMEBYE_CTX_CONFIG_HOME", home)
	ctxCfg.home = "" // forget a cached read of another test's home
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	s := &Server{}
	pi := write("pi.jsonl", `{"type":"message","timestamp":"2026-11-06T17:04:03Z","message":{"role":"assistant","provider":"homelab","model":"thinking","usage":{"input":15,"cacheRead":90000,"cacheWrite":0}}}`+"\n")
	if c := s.contextUsage(herdr.AgentInfo{Agent: "pi"}, pi); c == nil || c.Tokens != 90015 || c.Window != 180224 {
		t.Fatalf("pi: %+v", c)
	}
	// A model the config does not list has no window: the figure is shown without a size.
	pi2 := write("pi2.jsonl", `{"type":"message","message":{"role":"assistant","provider":"other","model":"x","usage":{"input":5,"cacheRead":1}}}`+"\n")
	if c := s.contextUsage(herdr.AgentInfo{Agent: "pi"}, pi2); c == nil || c.Tokens != 6 || c.Window != 0 {
		t.Fatalf("unknown pi model: %+v", c)
	}
	cl := write("c.jsonl", asstLine("claude-opus-5-5", 250000)+"\n")
	if c := s.contextUsage(herdr.AgentInfo{Agent: "claude"}, cl); c == nil || c.Window != 1_000_000 {
		t.Fatalf("claude: %+v", c)
	}
	if c := s.contextUsage(herdr.AgentInfo{Agent: "claude"}, write("none.jsonl", `{"type":"user"}`+"\n")); c != nil {
		t.Fatalf("no usage must give nothing: %+v", c)
	}
}

func asstLine(model string, cacheRead int) string {
	return `{"type":"assistant","message":{"model":"` + model + `","usage":{"input_tokens":1,"cache_read_input_tokens":` + strconv.Itoa(cacheRead) + `}}}`
}

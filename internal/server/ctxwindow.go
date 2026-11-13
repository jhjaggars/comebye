package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jhjaggars/comebye/internal/herdr"
	"github.com/jhjaggars/comebye/internal/opencode"
	"github.com/jhjaggars/comebye/internal/transcript"
	"github.com/jhjaggars/comebye/internal/usage"
)

// The context window of the model an agent runs: Claude's from a table
// (usage.ContextWindow), Pi's and OpenCode's from the agent's own config
// file on this host, which is where it learns the size too. Unknown stays 0.

var ctxCfg struct {
	mu   sync.Mutex
	at   time.Time
	pi   map[string]int // "provider/model"
	oc   map[string]int
	home string
}

// ctxConfigTTL bounds how often the config files are read again.
const ctxConfigTTL = time.Minute

func ctxWindows(home string) (pi, oc map[string]int) {
	ctxCfg.mu.Lock()
	defer ctxCfg.mu.Unlock()
	if ctxCfg.home == home && time.Since(ctxCfg.at) < ctxConfigTTL {
		return ctxCfg.pi, ctxCfg.oc
	}
	ctxCfg.home, ctxCfg.at = home, time.Now()
	ctxCfg.pi, ctxCfg.oc = map[string]int{}, map[string]int{}
	if b, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "models.json")); err == nil {
		var cfg struct {
			Providers map[string]struct {
				Models []struct {
					ID     string `json:"id"`
					Window int    `json:"contextWindow"`
				} `json:"models"`
			} `json:"providers"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			for p, pr := range cfg.Providers {
				for _, m := range pr.Models {
					if m.Window > 0 {
						ctxCfg.pi[p+"/"+m.ID] = m.Window
					}
				}
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json")); err == nil {
		var cfg struct {
			Provider map[string]struct {
				Models map[string]struct {
					Limit struct {
						Context int `json:"context"`
					} `json:"limit"`
				} `json:"models"`
			} `json:"provider"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			for p, pr := range cfg.Provider {
				for id, m := range pr.Models {
					if m.Limit.Context > 0 {
						ctxCfg.oc[p+"/"+id] = m.Limit.Context
					}
				}
			}
		}
	}
	return ctxCfg.pi, ctxCfg.oc
}

// contextUsage is how full a's context window is, from its session file at
// path; nil when the file says nothing usable.
func (s *Server) contextUsage(a herdr.AgentInfo, path string) *transcript.Context {
	c, ok, err := transcript.LastContext(path)
	if err != nil || !ok {
		return nil
	}
	switch a.Agent {
	case "claude":
		c.Window = usage.ContextWindow(c.Model, c.Tokens)
	case "pi", opencode.Kind:
		home := os.Getenv("COMEBYE_CTX_CONFIG_HOME") // the test harness points this at fixture config files
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		pi, oc := ctxWindows(home)
		tab := pi
		if a.Agent == opencode.Kind {
			tab = oc
		}
		c.Window = tab[c.Provider+"/"+c.Model]
		if c.Window == 0 && c.Provider == "" {
			c.Window = usage.ContextWindow(c.Model, c.Tokens)
		}
	}
	if c.Window > 0 && c.Tokens > c.Window {
		c.Window = 0 // the figure and the size disagree: show neither as a percentage
	}
	return &c
}

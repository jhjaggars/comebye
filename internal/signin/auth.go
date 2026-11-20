package signin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"time"
)

// Auth states of an installed agent, as the host's settings answer shows them.
const (
	AuthSignedIn  = "signed_in"
	AuthSignedOut = "signed_out"
	AuthUnknown   = "unknown"
)

// MethodKey is the method of an agent that runs on an API key or a gateway:
// there is nothing to sign in to.
const MethodKey = "api_key"

// AuthInfo is one agent's sign-in state: the state and the method, never an
// account name.
type AuthInfo struct {
	State  string `json:"state"`
	Method string `json:"method,omitempty"`
	// CanSignIn is true when the host can run this agent's sign-in.
	CanSignIn bool `json:"can_sign_in"`
}

// authTTL is how long a probe's answer is kept.
const authTTL = 60 * time.Second

type authEntry struct {
	at   time.Time
	info AuthInfo
}

// keyAgents never sign in: they run on a key or a gateway set up another way.
var keyAgents = map[string]bool{"pi": true}

// keyEnv are the variables that make claude run on a key or a gateway.
var keyEnv = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// AuthState answers each of kinds with its sign-in state. Probes run
// `claude auth status` and the like; an answer is kept for 60 s and dropped
// when a sign-in of that agent succeeds. A kind with no probe is "unknown".
func (m *Manager) AuthState(ctx context.Context, kinds []string) map[string]AuthInfo {
	out := make(map[string]AuthInfo, len(kinds))
	now := m.cfg.Now()
	for _, k := range kinds {
		m.mu.Lock()
		e, ok := m.auth[k]
		m.mu.Unlock()
		if ok && now.Sub(e.at) < authTTL {
			out[k] = e.info
			continue
		}
		info := m.probeAuth(ctx, k)
		m.mu.Lock()
		m.auth[k] = authEntry{at: m.cfg.Now(), info: info}
		m.mu.Unlock()
		out[k] = info
	}
	return out
}

func (m *Manager) probeAuth(ctx context.Context, kind string) AuthInfo {
	d, hasDriver := m.cfg.Drivers[kind]
	if keyAgents[kind] {
		return AuthInfo{State: AuthSignedIn, Method: MethodKey}
	}
	if !hasDriver {
		return AuthInfo{State: AuthUnknown}
	}
	if kind == Claude.Name {
		for _, v := range keyEnv {
			if m.cfg.LookupEnv(v) != "" {
				return AuthInfo{State: AuthSignedIn, Method: MethodKey}
			}
		}
	}
	info := AuthInfo{State: AuthUnknown, CanSignIn: true}
	if len(d.Status) == 0 {
		return info
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, d.Status[0], d.Status[1:]...)
	cmd.Env = m.cfg.Env()
	out, _ := cmd.Output() // a signed-out claude exits 1 and still prints its answer
	var st struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(out, &st) != nil || st.LoggedIn == nil {
		return info
	}
	if *st.LoggedIn {
		info.State, info.Method = AuthSignedIn, st.AuthMethod
	} else {
		info.State = AuthSignedOut
	}
	return info
}

func defaultLookupEnv(k string) string { return os.Getenv(k) }

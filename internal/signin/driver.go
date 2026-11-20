package signin

import (
	"context"
	"encoding/json"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// A Driver is what differs between agents: the command that starts a
// sign-in, where its link may point, and how the result is checked.
type Driver struct {
	Name string
	// Command is the program and its arguments (run on a pseudo-terminal).
	Command []string
	// Status is the probe: the program and arguments whose output says
	// whether the agent is signed in.
	Status []string
	// CodeInput is true when the user pastes a code back (claude); false for
	// a device flow that finishes by itself.
	CodeInput bool
	// Hosts are the host names the link may have (https only).
	Hosts []string
}

// Claude is `claude auth login --claudeai`.
var Claude = Driver{
	Name:      "claude",
	Command:   []string{"claude", "auth", "login", "--claudeai"},
	Status:    []string{"claude", "auth", "status"},
	CodeInput: true,
	Hosts:     []string{"claude.com", "claude.ai", "platform.claude.com", "console.anthropic.com"},
}

// URLAllowed reports whether u is an https link to one of the driver's hosts.
func (d Driver) URLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, a := range d.Hosts {
		if h == a {
			return true
		}
	}
	return false
}

// escapes: OSC (ESC ] ... BEL or ESC \) and CSI (ESC [ ... final byte), and
// a lone two-byte escape.
var escapes = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]|\x1b[@-Z\\-_]`)

// StripEscapes removes terminal escape sequences and carriage returns.
func StripEscapes(s string) string {
	return strings.ReplaceAll(escapes.ReplaceAllString(s, ""), "\r", "")
}

var linkRe = regexp.MustCompile(`https?://[^\s\x00-\x1f\x7f]+`)

// FindURL returns the first link in the child's output once it is complete
// (followed by whitespace), with escapes stripped.
func FindURL(raw string) (string, bool) {
	clean := StripEscapes(raw)
	loc := linkRe.FindStringIndex(clean)
	if loc == nil || loc[1] >= len(clean) { // the link may still be arriving
		return "", false
	}
	return clean[loc[0]:loc[1]], true
}

// probe runs the status command and reads loggedIn and authMethod from its
// JSON answer. The exit status is not used: a signed-out claude exits 1.
func (d Driver) probe(ctx context.Context, env []string) (signedIn bool, method string) {
	if len(d.Status) == 0 {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.Status[0], d.Status[1:]...)
	cmd.Env = env
	out, _ := cmd.Output()
	var st struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(out, &st) != nil {
		return false, ""
	}
	return st.LoggedIn, st.AuthMethod
}

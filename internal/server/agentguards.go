package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jhjaggars/comebye/internal/herdr"
)

// Guards on what one agent can make another agent read. An agent runs as
// the user, so the hub is where text from an agent is told apart from text
// from the user or from the hub itself.

// looksLikeHubText reports whether any line of text starts like one of the
// hub's own messages ("Comebye (hub notice): ...", "Comebye (hub relay)
// ..."), however it is dressed: case, spaces, control or invisible
// characters in between, a look-alike bracket.
func looksLikeHubText(text string) bool {
	for _, line := range strings.Split(herdr.CleanPromptText(text), "\n") {
		var rs []rune
		for _, r := range line {
			if unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
				continue
			}
			rs = append(rs, unicode.ToLower(r))
			if len(rs) == 24 {
				break
			}
		}
		head := string(rs)
		// "comebye" within the first few characters (a quote mark, "> " or a
		// leftover escape code may come before it), then an opening bracket.
		if i := strings.Index(head, "comebye"); i >= 0 && i <= 4 {
			if r, _ := utf8.DecodeRuneInString(head[i+len("comebye"):]); unicode.Is(unicode.Ps, r) || unicode.Is(unicode.Pi, r) {
				return true
			}
		}
	}
	return false
}

// agentLabel is what the hub puts before text an agent sent, so the
// receiving agent knows who spoke.
func (s *Server) agentLabel(r *http.Request) string {
	return "From agent " + oneLine(s.by(r, ""), 100) + ", not the user and not the hub: "
}

func lookalikeRefusal(w http.ResponseWriter) {
	writeJSONStatus(w, http.StatusForbidden, map[string]string{"code": "hub_lookalike", "message": "an agent's text cannot start like a Comebye hub message (\"Comebye (...\"): the hub labels who sent it; say it in your own words"})
}

// labelAgentPrompt makes an agent's prompt carry its sender: the hub puts a
// "From agent NAME (host/pane), not the user and not the hub:" line before
// the text, and refuses text that starts like a hub message. Prompts from
// the user are untouched. It runs on the host the agent's request first
// reaches the route on (also when the target is on a peer), once.
func (s *Server) labelAgentPrompt(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !callerOf(r).agent() {
			next(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		text, _ := body["text"].(string)
		if looksLikeHubText(text) {
			lookalikeRefusal(w)
			return
		}
		if strings.TrimSpace(text) != "" {
			body["text"] = s.agentLabel(r) + text
		}
		out, _ := json.Marshal(body)
		r.Body = io.NopCloser(bytes.NewReader(out))
		r.ContentLength = int64(len(out))
		next(w, r)
	}
}

// approvalSubjectRE: the one wording a host's request may carry for each
// action, a key's fingerprint included.
var approvalSubjectRE = map[string]*regexp.Regexp{
	"gpg-unlock": regexp.MustCompile(`^Unlock GPG key [0-9A-Fa-f]{8,64}$`),
	"gpg-setup":  regexp.MustCompile(`^Set up GPG unlock for key [0-9A-Fa-f]{8,64}$`),
}

var approvalSubjectDefault = map[string]string{
	"gpg-unlock": "Unlock the GPG key",
	"gpg-setup":  "Set up GPG unlock",
}

// serverSubject is the subject of an approval an agent or a host asked for:
// the agent's own text only when it is exactly the validated shape, else the
// server's wording for the action. The card's words are the server's.
func serverSubject(action, given string) string {
	given = oneLine(given, maxApprovalSubject)
	if re := approvalSubjectRE[action]; re != nil && re.MatchString(given) {
		return given
	}
	if d, ok := approvalSubjectDefault[action]; ok {
		return d
	}
	return ""
}

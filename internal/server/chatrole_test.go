package server

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// registeredPatterns reads every pattern the server registers on its mux
// from the package's source, so a new route joins the table below by itself.
func registeredPatterns(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\bs\.mux\.Handle(?:Func)?\("([A-Z]+ /[^"]*)",`)
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	// Registered in a loop over the operations (server.go).
	for _, op := range []string{"pause", "resume", "cancel"} {
		out = append(out, "POST /api/release/rollout/"+op)
	}
	if len(out) < 100 {
		t.Fatalf("found only %d routes: the pattern scan is broken", len(out))
	}
	return out
}

// Every route, called with a chat helper's agent token: all are refused 403
// chat_role except the few the helper needs, so a route added later is
// refused to it by default.
func TestChatRoleIsRefusedEveryRouteButItsOwn(t *testing.T) {
	s, _, page, userPage := chatHub(t)
	if code, m := chatPost(s, page, userHdr, `{}`); code != 200 {
		t.Fatalf("chat = %d %v", code, m)
	}
	helper := agentHdr("w9:p1")
	subst := regexp.MustCompile(`\{[^}]*\}`)
	listed := map[string]bool{}
	for _, pat := range registeredPatterns(t) {
		if pat == "GET /" || strings.HasPrefix(pat, "GET /stt/") { // static files, no API
			continue
		}
		method, path, _ := strings.Cut(pat, " ")
		own, allowed := chatRoutes[pat]
		listed[pat] = true
		call := func(p string) *httpRec {
			p = subst.ReplaceAllStringFunc(p, func(m string) string {
				if m == "{id}" && strings.Contains(path, "/pages/") {
					return "PAGE"
				}
				return "x"
			})
			return rec(do(s, method, strings.ReplaceAll(p, "PAGE", page), helper, "{}"))
		}
		w := call(path)
		isChatRole := w.Code == http.StatusForbidden && strings.Contains(w.Body, `"chat_role"`)
		if allowed && isChatRole {
			t.Errorf("%s: the helper's own route is refused: %d %s", pat, w.Code, w.Body)
		}
		if !allowed && !isChatRole {
			t.Errorf("%s: a chat helper gets %d %s, want 403 chat_role", pat, w.Code, w.Body)
		}
		if allowed && own {
			// The same route about another Page is refused.
			other := strings.ReplaceAll(subst.ReplaceAllString(path, "x"), "/pages/x", "/pages/"+userPage)
			if w := rec(do(s, method, other, helper, "{}")); w.Code != http.StatusForbidden || !strings.Contains(w.Body, `"chat_role"`) {
				t.Errorf("%s about another page: %d %s, want 403 chat_role", pat, w.Code, w.Body)
			}
		}
	}
	for pat := range chatRoutes {
		if !listed[pat] {
			t.Errorf("chatRoutes lists %q, which no route registers", pat)
		}
	}
}

// The role binds the agent, not the token: another agent and the user keep
// what they had.
func TestChatRoleLeavesOthersAlone(t *testing.T) {
	s, _, page, _ := chatHub(t)
	if code, m := chatPost(s, page, userHdr, `{}`); code != 200 {
		t.Fatalf("chat = %d %v", code, m)
	}
	if w := do(s, "GET", "/api/todos", agentHdr("w2:p1"), ""); w.Code != http.StatusOK {
		t.Errorf("another agent's todo list = %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", "/api/todos", userHdr, ""); w.Code != http.StatusOK {
		t.Errorf("the user's todo list = %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", "/api/todos", agentHdr("w9:p1"), `{"question":"q"}`); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "chat_role") {
		t.Errorf("the helper's todo add = %d %s", w.Code, w.Body)
	}
}

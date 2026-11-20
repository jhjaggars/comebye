package server

import "testing"

// A link may point at another To do item on this hub (the app's own route);
// every other non-http(s) URL is still refused.
func TestCleanLinksSelfTodoRoute(t *testing.T) {
	for _, u := range []string{"/#/todo/t0123456789", "#/todo/t0123456789", "https://hub.example.ts.net/#/todo/t0123456789"} {
		out, err := cleanLinks([]TodoLink{{Label: "x", URL: u}})
		if err != nil || len(out) != 1 || out[0].URL != u {
			t.Errorf("%q: %v %v", u, out, err)
		}
	}
	for _, u := range []string{"javascript:alert(1)", "#/agent/w1:p1", "/#/todo/nonsense", "/#/todo/t0123456789x", "#/todo/T0123456789", "/#/todo/t0123456789/../x", "data:text/html,x", "//evil.example/#/todo/t0123456789"} {
		if _, err := cleanLinks([]TodoLink{{URL: u}}); err == nil {
			t.Errorf("%q was accepted", u)
		}
	}
}

package server

import (
	"fmt"
	"strings"
	"testing"
)

// -test-mode tells the app to poll fast and lifts the to-do rate limit; a normal hub says neither.
func TestTestModeIsOffByDefault(t *testing.T) {
	off := New(Config{InsecureNoAuth: true, StateDir: t.TempDir()}, nil, nil)
	if w := do(off, "GET", "/api/test-mode", nil, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("off: %d %q", w.Code, w.Body.String())
	}
	on := New(Config{InsecureNoAuth: true, StateDir: t.TempDir(), TestMode: true}, nil, nil)
	if w := do(on, "GET", "/api/test-mode", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"poll_ms":300`) {
		t.Fatalf("on: %d %q", w.Code, w.Body.String())
	}
}

func TestTestModeLiftsTheTodoRateLimit(t *testing.T) {
	for _, tc := range []struct {
		test bool
		want int
	}{{false, todoRate}, {true, 3 * todoRate}} {
		s := New(Config{InsecureNoAuth: true, StateDir: t.TempDir(), TestMode: tc.test}, nil, nil)
		ok := 0
		for i := 0; i < 3*todoRate; i++ {
			if w := do(s, "POST", "/api/todos", map[string]string{"Content-Type": "application/json"}, fmt.Sprintf(`{"question":"question %d"}`, i)); w.Code == 200 || w.Code == 201 {
				ok++
			}
		}
		if ok != tc.want {
			t.Fatalf("test mode %v: %d adds accepted, want %d", tc.test, ok, tc.want)
		}
	}
}

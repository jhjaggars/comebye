package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func httptestReq(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, io.NopCloser(strings.NewReader(body)))
	r.RemoteAddr = "100.64.0.1:1234"
	return r
}

func serveTo(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// newCapRig2 is a hub behind Serve with the release buttons wired to fakes.
func newCapRig2(t *testing.T, tool *fakeTool, run *fakeRunner) *tagRig {
	t.Helper()
	return newRigWith(t, false, func(c *Config) {
		c.AllowCap = testCap
		c.Release = ReleaseConfig{Tool: tool, Upgrade: true, Runner: run}
	})
}

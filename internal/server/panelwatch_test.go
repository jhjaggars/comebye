package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncRec is a ResponseWriter that can be read while the handler still writes.
type syncRec struct {
	mu  sync.Mutex
	hdr http.Header
	buf bytes.Buffer
}

func (r *syncRec) Header() http.Header { return r.hdr }
func (r *syncRec) WriteHeader(int)     {}
func (r *syncRec) Flush()              {}
func (r *syncRec) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(b)
}
func (r *syncRec) text() string { r.mu.Lock(); defer r.mu.Unlock(); return r.buf.String() }

func (r *panelRig) stream(t *testing.T, path string) (*syncRec, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	req.RemoteAddr = "100.64.0.1:1234"
	for k, v := range userHdr {
		req.Header.Set(k, v)
	}
	rec := &syncRec{hdr: http.Header{}}
	done := make(chan struct{})
	go func() { r.s.mux.ServeHTTP(rec, req); close(done) }()
	return rec, func() { cancel(); <-done }
}

func awaitPanel(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPanelEventsPushAndShare(t *testing.T) {
	old := panelWatchEvery
	panelWatchEvery = 60 * time.Millisecond
	defer func() { panelWatchEvery = old }()
	r := newPanelRig(t)
	put(t, filepath.Join(r.wt, "docs", "plan.md"), "# Plan\n")
	if st, _ := r.req("GET", "/api/agents/w9:p1/panel/events", "", agentHdr("w8:p1")); st != 403 {
		t.Fatalf("another agent: %d", st)
	}
	if st, _ := r.req("GET", "/api/agents/w9:p1/panel/events", "", nil); st != 401 {
		t.Fatalf("no identity: %d", st)
	}
	a, stopA := r.stream(t, "/api/agents/w9:p1/panel/events?path=docs/plan.md")
	b, stopB := r.stream(t, "/api/agents/w9:p1/panel/events?path=docs/plan.md")
	awaitPanel(t, "both ready", func() bool { return strings.Contains(a.text(), "event: ready") && strings.Contains(b.text(), "event: ready") })
	if n := r.s.panels.count(); n != 1 {
		t.Fatalf("two tabs on one document share %d watchers, want 1", n)
	}
	time.Sleep(250 * time.Millisecond) // the baseline is set; nothing changed, nothing is pushed
	if strings.Contains(a.text(), "event: diff") || strings.Contains(a.text(), "event: doc") {
		t.Fatalf("pushed with no change: %q", a.text())
	}
	put(t, filepath.Join(r.wt, "c.go"), "package c\n\nvar C = 2\n")
	awaitPanel(t, "diff push", func() bool { return strings.Contains(a.text(), "event: diff") && strings.Contains(b.text(), "event: diff") })
	put(t, filepath.Join(r.wt, "docs", "plan.md"), "# Plan\n\nmore\n")
	awaitPanel(t, "doc push", func() bool { return strings.Contains(a.text(), "event: doc") })
	stopA()
	stopB()
	awaitPanel(t, "the watcher to stop", func() bool { return r.s.panels.count() == 0 })
}

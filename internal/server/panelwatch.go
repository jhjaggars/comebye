package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Live push for the side panels: GET /api/[hosts/H/]agents/T/panel/events is an
// SSE stream that says "diff" or "doc" the moment the agent's checkout or the
// shown document changes, so an open panel redraws at once instead of waiting
// for its slow fallback poll.
//
// The hosts have no file watcher (a recursive watch over every worktree is new
// code on every peer); the hub keeps ONE watcher per agent and document, however
// many browsers have the panel open, and the watcher asks the host the same cheap
// "unchanged since sig?" question the panel's own poll asks. The browsers' polls
// stop (they fall back to a slow check), so a peer sees one small git call per
// second per open agent, not one per tab every 3 s.

// panelWatchEvery is how often a watcher asks the host (a var for tests).
var panelWatchEvery = time.Second

type panelPing struct {
	Kind string // "diff" or "doc"
	Sig  string
}

type panelWatcher struct {
	mu     sync.Mutex
	subs   map[chan panelPing]struct{}
	cancel context.CancelFunc
}

// panelHub holds the running watchers, keyed by agent and document.
type panelHub struct {
	mu sync.Mutex
	m  map[string]*panelWatcher
}

// subscribe joins (starting if needed) the watcher for key. check is called
// every tick to see what changed; it is only the first subscriber's closure.
func (h *panelHub) subscribe(key string, check func(ctx context.Context, last map[string]string, emit func(panelPing))) (chan panelPing, func()) {
	h.mu.Lock()
	if h.m == nil {
		h.m = map[string]*panelWatcher{}
	}
	w := h.m[key]
	if w == nil {
		ctx, cancel := context.WithCancel(context.Background())
		w = &panelWatcher{subs: map[chan panelPing]struct{}{}, cancel: cancel}
		h.m[key] = w
		go w.run(ctx, check)
	}
	ch := make(chan panelPing, 8)
	w.mu.Lock()
	w.subs[ch] = struct{}{}
	w.mu.Unlock()
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		w.mu.Lock()
		delete(w.subs, ch)
		n := len(w.subs)
		w.mu.Unlock()
		if n == 0 {
			w.cancel()
			if h.m[key] == w {
				delete(h.m, key)
			}
		}
	}
}

func (h *panelHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.m)
}

func (w *panelWatcher) emit(p panelPing) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.subs {
		select {
		case ch <- p:
		default: // a slow reader misses a ping; the next one carries the same news
		}
	}
}

func (w *panelWatcher) run(ctx context.Context, check func(context.Context, map[string]string, func(panelPing))) {
	last := map[string]string{}
	t := time.NewTicker(panelWatchEvery)
	defer t.Stop()
	check(ctx, last, func(panelPing) {}) // the first look sets the baseline; the browser fetches on its own
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			check(cctx, last, w.emit)
			cancel()
		}
	}
}

// handlePanelEvents is the stream. ?path= is the document the user picked
// (default: the agent's declared one).
func (s *Server) handlePanelEvents(w http.ResponseWriter, r *http.Request) {
	a, host, ok := s.panelTarget(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if s.streams.Add(1) > maxStreams {
		s.streams.Add(-1)
		http.Error(w, "too many event streams", http.StatusServiceUnavailable)
		return
	}
	defer s.streams.Add(-1)
	pick := r.URL.Query().Get("path")
	dir := agentDir(a)
	key := a.ID + "|" + pick
	check := func(ctx context.Context, last map[string]string, emit func(panelPing)) {
		note := func(kind, sig string) {
			if sig == "" {
				return
			}
			if prev, seen := last[kind]; seen && prev != sig {
				emit(panelPing{kind, sig})
			}
			last[kind] = sig
		}
		q := url.Values{"dir": {dir}}
		if sig := last["diffsig"]; sig != "" {
			q.Set("since", sig)
		}
		if status, out := s.panelCall(ctx, host, "diff", q); status == http.StatusOK {
			if sig, _ := out["sig"].(string); sig != "" {
				last["diffsig"] = sig
				note("diff", sig)
			}
		}
		pin, hasPin := s.pins.get(a.ID)
		path := pick
		if path == "" && hasPin {
			path = pin.Path
		}
		if path == "" {
			note("doc", "none")
			return
		}
		fq := url.Values{"dir": {dir}, "path": {path}}
		if hasPin {
			fq.Set("pin", pin.Path)
		}
		if sig := last["docsig"]; sig != "" {
			fq.Set("since", sig)
		}
		if status, out := s.panelCall(ctx, host, "file", fq); status == http.StatusOK {
			if doc, _ := out["doc"].(map[string]any); doc != nil {
				if sig, _ := doc["sig"].(string); sig != "" {
					last["docsig"] = sig
				}
			}
		}
		// The declaration itself (file or section) counts as a change of the doc.
		token := last["docsig"]
		if pick == "" && hasPin {
			token += "|" + pin.Path + "|" + pin.Section + "|" + pin.At.Format(time.RFC3339Nano)
		}
		note("doc", token)
	}
	ch, leave := s.panels.subscribe(key, check)
	defer leave()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	hb := time.NewTicker(s.cfg.PingInterval)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-hb.C:
			fmt.Fprint(w, "event: ping\ndata: {}\n\n")
			flusher.Flush()
		case p := <-ch:
			fmt.Fprintf(w, "event: %s\ndata: {}\n\n", p.Kind)
			flusher.Flush()
		}
	}
}

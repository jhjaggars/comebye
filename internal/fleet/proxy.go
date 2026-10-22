package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/jhjaggars/comebye/internal/transcript"
)

// proxyOp is one per-agent API call the hub may forward. Nothing else is
// ever proxied: the hub is not a general-purpose proxy to its peers.
type proxyOp struct {
	method  string
	query   []string // query parameters passed through; all others dropped
	maxBody int64    // request body cap for POSTs
	timeout time.Duration
}

var proxyOps = map[string]proxyOp{
	"output":     {method: http.MethodGet, query: []string{"lines", "source"}, timeout: 20 * time.Second},
	"transcript": {method: http.MethodGet, query: []string{"limit", "skip", "from"}, timeout: 20 * time.Second},
	"prompt":     {method: http.MethodPost, maxBody: 1 << 20, timeout: 30 * time.Second},
	"keys":       {method: http.MethodPost, maxBody: 64 << 10, timeout: 15 * time.Second},
	"dialog":     {method: http.MethodGet, timeout: 20 * time.Second},
	"answer":     {method: http.MethodPost, maxBody: 64 << 10, timeout: 20 * time.Second},
	// image: an 8 MB image as base64 JSON, stored on the agent's host.
	"image": {method: http.MethodPost, maxBody: 12 << 20, timeout: 60 * time.Second},
	// subagent-transcript: /agents/{target}/subagents/{sub}/transcript.
	"subagent-transcript": {method: http.MethodGet, query: []string{"limit", "skip", "from"}, timeout: 20 * time.Second},
}

// ValidTarget reports whether target (an agent name or pane ID) can be
// forwarded as a single path segment.
func ValidTarget(target string) bool {
	if target == "" || len(target) > 256 || target == "." || target == ".." {
		return false
	}
	for _, r := range target {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Proxy forwards one per-agent API call (op: output, transcript,
// subagent-transcript, prompt, keys, dialog, answer or image) for target
// to the peer named host and writes the peer's answer:
// status code and JSON body pass through (e.g. 409 agent_blocked), only
// the content type is kept from the peer's headers, and no browser header
// is forwarded.
func (f *Fleet) Proxy(w http.ResponseWriter, r *http.Request, host, target, op string) {
	p := f.byName[host]
	if p == nil {
		writeJSON(w, http.StatusNotFound, "unknown_host", "no such host: "+host)
		return
	}
	spec, ok := proxyOps[op]
	if !ok || r.Method != spec.method {
		writeJSON(w, http.StatusNotFound, "not_found", "not a proxied operation")
		return
	}
	if !ValidTarget(target) {
		writeJSON(w, http.StatusBadRequest, "bad_target", "invalid agent")
		return
	}

	u := p.endpoint("api", "agents", target, op)
	if op == "subagent-transcript" {
		sub := r.PathValue("sub")
		if !transcript.ValidAgentID(sub) {
			writeJSON(w, http.StatusBadRequest, "bad_subagent", "invalid subagent id")
			return
		}
		u = p.endpoint("api", "agents", target, "subagents", sub, "transcript")
	}
	q := url.Values{}
	for _, k := range spec.query {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()

	timeout := spec.timeout
	var body io.Reader
	if spec.method == http.MethodPost {
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, spec.maxBody))
		if err != nil || !json.Valid(b) {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if op == "prompt" {
			timeout = promptTimeout(b)
		}
		body = bytes.NewReader(b)
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, spec.method, u.String(), body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, "proxy_error", err.Error())
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	// Conditional GETs (transcript polls) pass through, so an unchanged
	// transcript costs the phone a bodiless 304 on a hub too.
	if inm := r.Header.Get("If-None-Match"); inm != "" && spec.method == http.MethodGet {
		req.Header.Set("If-None-Match", inm)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.opts.Client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeJSON(w, http.StatusGatewayTimeout, "host_timeout", host+" did not answer in time")
			return
		}
		writeJSON(w, http.StatusBadGateway, "host_unreachable", host+" is unreachable")
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Not the browser's credentials: passing a 401 through would make
		// the app claim the user is not allowed on the hub.
		writeJSON(w, http.StatusBadGateway, "peer_unauthorized", host+" rejected the hub; its -ts-allow-login must include the hub device owner's login")
		return
	case resp.StatusCode == http.StatusNotModified:
		copyCacheHeaders(w, resp)
		w.WriteHeader(http.StatusNotModified)
		return
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		writeJSON(w, http.StatusBadGateway, "peer_redirect", host+" answered with a redirect")
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, f.opts.MaxResponse+1))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, "host_unreachable", host+": "+err.Error())
		return
	}
	if int64(len(data)) > f.opts.MaxResponse {
		writeJSON(w, http.StatusBadGateway, "peer_response_too_large", host+" sent too much data")
		return
	}
	ct := "text/plain; charset=utf-8"
	if mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";"); strings.TrimSpace(strings.ToLower(mt)) == "application/json" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	copyCacheHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

// promptTimeout mirrors the peer's own limit for a prompt (+wait) call.
func promptTimeout(body []byte) time.Duration {
	var b struct {
		Wait      bool  `json:"wait"`
		TimeoutMs int64 `json:"timeout_ms"`
	}
	_ = json.Unmarshal(body, &b)
	if !b.Wait {
		return 30 * time.Second
	}
	ms := max(b.TimeoutMs, 0)
	if ms > int64(30*time.Minute/time.Millisecond) {
		ms = int64(30 * time.Minute / time.Millisecond)
	}
	return min(time.Duration(ms)*time.Millisecond+35*time.Second, 30*time.Minute+5*time.Second)
}

func writeJSON(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": message})
}

// copyCacheHeaders passes the peer's validator through (only the ETag and
// its no-cache policy: nothing else from the peer reaches the browser).
func copyCacheHeaders(w http.ResponseWriter, resp *http.Response) {
	if etag := resp.Header.Get("ETag"); etag != "" {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
	}
}

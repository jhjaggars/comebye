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
	// bodyTimeout, when set, picks the timeout from the request body.
	bodyTimeout func(body []byte) time.Duration
}

var proxyOps = map[string]proxyOp{
	"output":     {method: http.MethodGet, query: []string{"lines", "source"}, timeout: 20 * time.Second},
	"transcript": {method: http.MethodGet, query: []string{"limit", "skip", "from"}, timeout: 20 * time.Second},
	"prompt":     {method: http.MethodPost, maxBody: 1 << 20, bodyTimeout: promptTimeout},
	"keys":       {method: http.MethodPost, maxBody: 64 << 10, timeout: 15 * time.Second},
	"dialog":     {method: http.MethodGet, timeout: 20 * time.Second},
	"activity":   {method: http.MethodGet, timeout: 15 * time.Second},
	"answer":     {method: http.MethodPost, maxBody: 64 << 10, timeout: 20 * time.Second},
	// image: an 8 MB image as base64 JSON, stored on the agent's host.
	"image": {method: http.MethodPost, maxBody: 12 << 20, timeout: 60 * time.Second},
	// subagent-transcript: /agents/{target}/subagents/{sub}/transcript.
	"subagent-transcript": {method: http.MethodGet, query: []string{"limit", "skip", "from"}, timeout: 20 * time.Second},
}

// hostProxyOps are the per-host (not per-agent) calls the hub may make to a
// peer: launching an agent there (browser calls, through ProxyHost), and
// the worker operations (see CallHost).
var hostProxyOps = map[string]struct {
	proxyOp
	path []string
	// hubOnly: only the hub itself makes this call (CallHost); a browser
	// cannot have it forwarded.
	hubOnly bool
}{
	"launch-options": {proxyOp{method: http.MethodGet, timeout: 20 * time.Second}, []string{"api", "launch", "options"}, false},
	"launch-dirs":    {proxyOp{method: http.MethodGet, query: []string{"path"}, timeout: 20 * time.Second}, []string{"api", "launch", "dirs"}, false},
	// launch: a prompt of up to 1 MB plus the directory and name; the
	// peer waits up to 90s for the agent to start.
	"launch": {proxyOp{method: http.MethodPost, maxBody: 1<<20 + 64<<10, timeout: 100 * time.Second}, []string{"api", "launch"}, false},
	// Workers (see CallHost): the git repositories for the handoff form,
	// a worktree's safety checks, starting a worker in a new or reused
	// worktree (git plus herdr, up to 150s on the peer), and removing a
	// worker's session.
	"repos":  {proxyOp{method: http.MethodGet, timeout: 30 * time.Second}, []string{"api", "repos"}, false},
	"checks": {proxyOp{method: http.MethodGet, query: []string{"path", "branch", "base", "detail", "diff", "within"}, timeout: 45 * time.Second}, []string{"api", "checks"}, false},
	"worker": {proxyOp{method: http.MethodPost, maxBody: 1<<20 + 64<<10, timeout: 160 * time.Second}, []string{"api", "worker"}, true},
	"remove": {proxyOp{method: http.MethodPost, maxBody: 64 << 10, timeout: 100 * time.Second}, []string{"api", "remove"}, true},
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
// to the peer named host and writes the peer's answer (see forward).
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
	f.forward(w, r, p, u, spec)
}

// ProxyHost forwards one per-host API call of a browser (an op of
// hostProxyOps) to the peer named host, hardened like Proxy.
func (f *Fleet) ProxyHost(w http.ResponseWriter, r *http.Request, host, op string) {
	p := f.byName[host]
	if p == nil {
		writeJSON(w, http.StatusNotFound, "unknown_host", "no such host: "+host)
		return
	}
	spec, ok := hostProxyOps[op]
	if !ok || spec.hubOnly || r.Method != spec.method {
		writeJSON(w, http.StatusNotFound, "not_found", "not a proxied operation")
		return
	}
	f.forward(w, r, p, p.endpoint(spec.path...), spec.proxyOp)
}

// CallHost makes one per-host API call (an op of hostProxyOps) to the peer
// named host on the hub's own behalf, not a browser's: the ledger starting a
// worker or removing one. Only the op's query parameters are sent. It
// returns the peer's status and JSON body; a peer that rejects the hub
// (401/403) or redirects is an error, like for Proxy.
func (f *Fleet) CallHost(ctx context.Context, host, op string, query url.Values, body []byte) (int, []byte, error) {
	p := f.byName[host]
	if p == nil {
		return 0, nil, errors.New("no such host: " + host)
	}
	spec, ok := hostProxyOps[op]
	if !ok {
		return 0, nil, errors.New("not a proxied operation: " + op)
	}
	u := p.endpoint(spec.path...)
	q := url.Values{}
	for _, k := range spec.query {
		if v := query.Get(k); v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	var rd io.Reader
	if spec.method == http.MethodPost {
		if int64(len(body)) > spec.maxBody || !json.Valid(body) {
			return 0, nil, errors.New("invalid body")
		}
		rd = bytes.NewReader(body)
	}
	ctx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, spec.method, u.String(), rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.opts.Client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, nil, errors.New(host + " did not answer in time")
		}
		return 0, nil, errors.New(host + " is unreachable")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden && resp.Header.Get("Content-Type") != "application/json":
		return 0, nil, errors.New(host + " rejected the hub; its -ts-allow-login must include the hub device owner's login")
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return 0, nil, errors.New(host + " answered with a redirect")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, f.opts.MaxResponse+1))
	if err != nil {
		return 0, nil, errors.New(host + ": " + err.Error())
	}
	if int64(len(data)) > f.opts.MaxResponse {
		return 0, nil, errors.New(host + " sent too much data")
	}
	return resp.StatusCode, data, nil
}

// FetchLatest reads the tail of a peer agent's transcript (the last
// assistant text), for the ledger's final reports.
func (f *Fleet) FetchLatest(ctx context.Context, host, pane string) *transcript.Latest {
	p := f.byName[host]
	if p == nil || !ValidTarget(pane) {
		return nil
	}
	return p.fetchLatest(ctx, pane)
}

// forward makes the call described by spec to u on peer p and writes the
// peer's answer: status code and JSON body pass through (e.g. 409
// agent_blocked), only the content type and cache validator are kept from
// the peer's headers, and no browser header is forwarded.
func (f *Fleet) forward(w http.ResponseWriter, r *http.Request, p *peer, u *url.URL, spec proxyOp) {
	host := p.name
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
		if spec.bodyTimeout != nil {
			timeout = spec.bodyTimeout(b)
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

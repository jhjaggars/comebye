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
)

// Upstream is a peer's link to its hub (-hub): the coordinator CLI's
// requests on a peer (ledger, handoffs, marks, the briefing, prompts) are
// forwarded there, because the hub keeps the one ledger. It is the mirror
// of the hub's calls to its peers and hardened the same way: no redirects,
// no proxy from the environment, bounded bodies and answers, and only the
// paths the caller names. The hub authenticates the forwarded request by
// the Tailscale identity Serve attaches (the peer device owner's login),
// so its -ts-allow-login must include that login.
type Upstream struct {
	base        *url.URL
	client      *http.Client
	maxResponse int64
}

// NewUpstream parses the hub's URL (https, its Tailscale Serve URL; plain
// http only on loopback with allowHTTPLoopback, for tests). A nil client
// gets the fleet's default (no redirects, no proxy).
func NewUpstream(raw string, allowHTTPLoopback bool, client *http.Client) (*Upstream, error) {
	u, err := parsePeerURL(strings.TrimSpace(raw), allowHTTPLoopback)
	if err != nil {
		return nil, err
	}
	if client == nil {
		o := Options{}
		o.defaults()
		client = o.Client
	}
	return &Upstream{base: u, client: client, maxResponse: 8 << 20}, nil
}

// URL is the hub's base URL.
func (u *Upstream) URL() string { return u.base.String() }

// UpstreamRequest describes one forwarded call.
type UpstreamRequest struct {
	Method string
	// Segments are the path's segments, each escaped (so nothing climbs
	// out of the path the caller built).
	Segments []string
	Query    url.Values
	// Body is sent for POSTs (it must be JSON within MaxBody).
	Body    []byte
	MaxBody int64
	Timeout time.Duration
	// Header is added to the request (attribution headers).
	Header http.Header
}

// Forward makes the call and writes the hub's answer to w: status and
// JSON body pass through; a hub that rejects the peer (401, or a 403 that
// is not one of its JSON answers) or redirects is reported as 502.
func (u *Upstream) Forward(ctx context.Context, w http.ResponseWriter, req UpstreamRequest) {
	status, ct, data, err := u.Do(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, "hub_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// Do makes the call and returns the hub's status, content type (JSON or
// plain text) and body.
func (u *Upstream) Do(ctx context.Context, req UpstreamRequest) (int, string, []byte, error) {
	p := &url.URL{Scheme: u.base.Scheme, Host: u.base.Host}
	var b strings.Builder
	for _, s := range req.Segments {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(s))
	}
	p.RawPath = b.String()
	p.Path, _ = url.PathUnescape(p.RawPath)
	if len(req.Query) > 0 {
		p.RawQuery = req.Query.Encode()
	}
	var body io.Reader
	if req.Method == http.MethodPost {
		if int64(len(req.Body)) > req.MaxBody || !json.Valid(req.Body) {
			return 0, "", nil, errors.New("invalid body")
		}
		body = bytes.NewReader(req.Body)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, req.Method, p.String(), body)
	if err != nil {
		return 0, "", nil, err
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hr.Header.Add(k, v)
		}
	}
	hr.Header.Set("Accept", "application/json")
	hr.Header.Set("User-Agent", "comebye-peer/1")
	if body != nil {
		hr.Header.Set("Content-Type", "application/json")
	}
	resp, err := u.client.Do(hr)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return 0, "", nil, errors.New("the hub did not answer in time")
		}
		return 0, "", nil, errors.New("the hub (" + u.base.Host + ") is unreachable")
	}
	defer resp.Body.Close()
	mt, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	isJSON := strings.TrimSpace(strings.ToLower(mt)) == "application/json"
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden && !isJSON:
		return 0, "", nil, errors.New("the hub rejected this peer; the hub's -ts-allow-login must include this device owner's login")
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return 0, "", nil, errors.New("the hub answered with a redirect")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, u.maxResponse+1))
	if err != nil {
		return 0, "", nil, errors.New("hub: " + err.Error())
	}
	if int64(len(data)) > u.maxResponse {
		return 0, "", nil, errors.New("the hub sent too much data")
	}
	ct := "text/plain; charset=utf-8"
	if isJSON {
		ct = "application/json"
	}
	return resp.StatusCode, ct, data, nil
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// hubClient talks to the hub's API the way a user on the tailnet does: the
// request arrives through the hub's Tailscale Serve, which tags it with the
// workstation's login. No token is involved.
type hubClient struct {
	base string
	http httpDoer
}

func (h *hubClient) do(ctx context.Context, method, path string) ([]byte, http.Header, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("hub %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.Header, resp.StatusCode, nil
}

// Version is the hub's comebye version: /api/whoami's "version", else the
// X-Comebye-Version header every API response carries.
func (h *hubClient) Version(ctx context.Context) (string, error) {
	b, hdr, code, err := h.do(ctx, "GET", "/api/whoami")
	if err != nil {
		return "", err
	}
	var w struct {
		Version string `json:"version"`
	}
	if code == 200 && json.Unmarshal(b, &w) == nil && w.Version != "" {
		return w.Version, nil
	}
	if v := hdr.Get("X-Comebye-Version"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("hub %s reports no version (HTTP %d): is it new enough (v0.3.0+)?", h.base, code)
}

type hubPeer struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Tag     string `json:"tag"`
	Status  string `json:"status"`
	Error   string `json:"error"`
	Agents  int    `json:"agents"`
	Version string `json:"version"`
}

// Peer returns the hub's entry for a peer, nil when there is none.
func (h *hubClient) Peer(ctx context.Context, name string) (*hubPeer, error) {
	b, _, code, err := h.do(ctx, "GET", "/api/peers")
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("hub GET /api/peers: HTTP %d", code)
	}
	var r struct {
		Peers []hubPeer `json:"peers"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("hub /api/peers: %w", err)
	}
	for i := range r.Peers {
		if r.Peers[i].Name == name {
			return &r.Peers[i], nil
		}
	}
	return nil, nil
}

// SetInstance tells the hub which machine the peer is: the cloud's instance
// id. Worker names are reused, and the hub ties ledger entries and to-do
// items to this id so that a new machine under an old name is not mistaken
// for the old one (its old unfinished entries become "lost").
func (h *hubClient) SetInstance(ctx context.Context, name, instance string) error {
	body, _ := json.Marshal(map[string]string{"instance": instance})
	req, err := http.NewRequestWithContext(ctx, "PUT", h.base+"/api/peers/"+url.PathEscape(name)+"/instance", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.http.Do(req)
	if err != nil {
		return fmt.Errorf("hub PUT instance: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != 200 {
		return fmt.Errorf("hub PUT /api/peers/%s/instance: HTTP %d %s", name, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// RemovePeer deletes the hub's entry; a missing one is fine. A static peer
// (set in COMEBYE_PEERS) cannot be removed through the API: the error says so.
func (h *hubClient) RemovePeer(ctx context.Context, name string) error {
	_, _, code, err := h.do(ctx, "DELETE", "/api/peers/"+url.PathEscape(name))
	if err != nil {
		return err
	}
	if code == 200 || code == 204 || code == 404 {
		return nil
	}
	return fmt.Errorf("hub DELETE /api/peers/%s: HTTP %d", name, code)
}

func peerURL(c *Config, name string) string {
	u, _ := url.Parse(c.HubURL)
	host := u.Hostname()
	if i := strings.Index(host, "."); i > 0 {
		return "https://" + name + host[i:]
	}
	return ""
}

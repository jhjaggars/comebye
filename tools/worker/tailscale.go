package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// tsClient is the slice of the Tailscale API the provisioner needs: mint a
// tagged single-use key, list devices, delete one. It authenticates with an
// OAuth client (client credentials), so there is no long-lived API key.
type tsClient struct {
	base       string
	http       httpDoer
	id, secret string
	token      string
}

// LoadOAuth reads client_id / client_secret from path. The file must not be
// readable by group or others, and must not be inside a git checkout we can
// tell about from here (the repo ignores it either way).
func LoadOAuth(path string) (id, secret string, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", "", fmt.Errorf("%s is mode %o: chmod 600 it", path, st.Mode().Perm())
	}
	kv, err := parseKV(path)
	if err != nil {
		return "", "", err
	}
	id, secret = kv["client_id"], kv["client_secret"]
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("%s needs client_id and client_secret", path)
	}
	return id, secret, nil
}

func (t *tsClient) do(ctx context.Context, method, path string, body io.Reader, ctype string, bearer bool) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, body)
	if err != nil {
		return nil, 0, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if bearer {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, nil
}

// login exchanges the OAuth client for a short-lived token.
func (t *tsClient) login(ctx context.Context) error {
	if t.token != "" {
		return nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"auth_keys devices:core"}}
	req, err := http.NewRequestWithContext(ctx, "POST", t.base+"/api/v2/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(t.id, t.secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("tailscale oauth: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		// The body is not echoed: keep anything a server might reflect out of logs.
		return fmt.Errorf("tailscale oauth: HTTP %d (check client id/secret and that the client has auth_keys and devices:core)", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(b, &tok) != nil || tok.AccessToken == "" {
		return fmt.Errorf("tailscale oauth: no access_token in the response")
	}
	t.token = tok.AccessToken
	return nil
}

// MintKey makes a single-use, pre-authorized, tagged key that expires in
// ten minutes. The returned key is a secret: pass it on over stdin only.
func (t *tsClient) MintKey(ctx context.Context, tag, desc string) (id, key string, err error) {
	return t.MintKeyOpts(ctx, tag, desc, false, 10)
}

// MintKeyOpts is MintKey with the node's ephemerality and the key's lifetime
// in minutes chosen. An ephemeral node is removed by Tailscale soon after it
// goes offline: for tests, not for workers that stop and start.
func (t *tsClient) MintKeyOpts(ctx context.Context, tag, desc string, ephemeral bool, minutes int) (id, key string, err error) {
	if err := t.login(ctx); err != nil {
		return "", "", err
	}
	body, _ := json.Marshal(map[string]any{
		"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{
			"reusable": false, "ephemeral": ephemeral, "preauthorized": true, "tags": []string{tag},
		}}},
		"expirySeconds": minutes * 60,
		"description":   desc,
	})
	b, code, err := t.do(ctx, "POST", "/api/v2/tailnet/-/keys", bytes.NewReader(body), "application/json", true)
	if err != nil {
		return "", "", err
	}
	if code != 200 {
		return "", "", fmt.Errorf("tailscale: minting a key failed: HTTP %d (does the OAuth client own %s?)", code, tag)
	}
	var r struct{ ID, Key string }
	if json.Unmarshal(b, &r) != nil || r.Key == "" {
		return "", "", fmt.Errorf("tailscale: no key in the response")
	}
	return r.ID, r.Key, nil
}

type tsDevice struct {
	ID                 string   `json:"id"`
	Hostname           string   `json:"hostname"`
	Name               string   `json:"name"`
	Tags               []string `json:"tags"`
	ConnectedToControl bool     `json:"connectedToControl"`
	LastSeen           string   `json:"lastSeen"`
	Addresses          []string `json:"addresses"`
}

func (t *tsClient) Devices(ctx context.Context) ([]tsDevice, error) {
	if err := t.login(ctx); err != nil {
		return nil, err
	}
	b, code, err := t.do(ctx, "GET", "/api/v2/tailnet/-/devices?fields=default", nil, "", true)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("tailscale: listing devices failed: HTTP %d", code)
	}
	var r struct {
		Devices []tsDevice `json:"devices"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("tailscale: bad device list: %w", err)
	}
	return r.Devices, nil
}

func (t *tsClient) DeleteDevice(ctx context.Context, id string) error {
	if err := t.login(ctx); err != nil {
		return err
	}
	_, code, err := t.do(ctx, "DELETE", "/api/v2/device/"+url.PathEscape(id), nil, "", true)
	if err != nil {
		return err
	}
	if code != 200 && code != 204 && code != 404 {
		return fmt.Errorf("tailscale: deleting device %s failed: HTTP %d", id, code)
	}
	return nil
}

func hasTag(d tsDevice, tag string) bool {
	for _, t := range d.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// ownDevices are the devices this tool may touch for a worker: the exact
// hostname AND the worker tag. An untagged device (someone's laptop) is
// never returned, whatever its name.
func ownDevices(devs []tsDevice, name, tag string) []tsDevice {
	var out []tsDevice
	for _, d := range devs {
		if d.Hostname == name && hasTag(d, tag) {
			out = append(out, d)
		}
	}
	return out
}

// staleDevices are the own devices that are offline: the leftover of an
// earlier VM with the same name. They would make the new node get "-1".
func staleDevices(devs []tsDevice, name, tag string) []tsDevice {
	var out []tsDevice
	for _, d := range ownDevices(devs, name, tag) {
		if !d.ConnectedToControl {
			out = append(out, d)
		}
	}
	return out
}

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
	"sort"
	"strings"
)

// The LLM gateway is a LiteLLM proxy on the tailnet. Each worker gets its
// own virtual key (alias worker-<name>) with a budget, a rate limit and a
// model allow-list. The provisioning key that mints them is only for
// key-management routes; it stays on this workstation. The worker's key goes
// to the worker over ssh stdin into pi's auth.json (mode 0600) and is never
// in an argument list, user-data, a log line or this program's state.

const (
	llmProvider    = "homelab-enmaas" // pi provider name on the worker
	llmAliasPrefix = "worker-"
	llmCreatedBy   = "comebye-worker"
	// user_id on every minted key; the provisioning key owns only the keys it creates.
	llmProvisionerUser = "comebye-worker-provisioner"
)

func llmAlias(name string) string { return llmAliasPrefix + name }

// llmScope identifies the cloud account area a key's worker lives in, so
// `worker keys --prune` never judges another operator's workers by this
// operator's cloud.
func llmScope(c *Config) string {
	if c.Provider == "aws" {
		return "aws:" + c.AWSRegion
	}
	return "gcp:" + c.Project
}

// readSecretFile reads a one-token secret from a file only we may read.
func readSecretFile(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is mode %o: chmod 600 it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if k == "" || strings.ContainsAny(k, " \t\r\n") {
		return "", fmt.Errorf("%s does not hold exactly one key", path)
	}
	return k, nil
}

type llmClient struct {
	base    string
	http    httpDoer
	provKey string // the provisioning key; never printed
}

// llm builds the client from the configured provisioning key file.
func (e *env) llm() (*llmClient, error) {
	k, err := readSecretFile(e.Cfg.LLMProvisionerKeyFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no LLM provisioning key at %s (llm_provisioner_key_file); ask for one, then chmod 600 it", e.Cfg.LLMProvisionerKeyFile)
		}
		return nil, err
	}
	return &llmClient{base: e.Cfg.LLMGatewayURL, http: e.HTTP, provKey: k}, nil
}

func (c *llmClient) scrub(s string, extra ...string) string {
	for _, x := range append([]string{c.provKey}, extra...) {
		if x != "" {
			s = strings.ReplaceAll(s, x, "[redacted]")
		}
	}
	return s
}

// do sends one request. bearer defaults to the provisioning key. The body of
// an error response is shortened and scrubbed before it can reach a message.
func (c *llmClient) do(ctx context.Context, method, path string, body any, bearer string) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("LLM gateway %s %s: %s", method, path, c.scrub(urlErrText(err), bearer))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, nil
}

func urlErrText(err error) string {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err.Error()
	}
	return err.Error()
}

// apiErr turns a non-2xx answer into an error that cannot carry a secret.
func (c *llmClient) apiErr(what string, code int, body []byte, extra ...string) error {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Detail any `json:"detail"`
	}
	msg := ""
	if json.Unmarshal(body, &e) == nil {
		msg = e.Error.Message
		if msg == "" && e.Detail != nil {
			msg = fmt.Sprint(e.Detail)
		}
	}
	msg = c.scrub(firstLine(msg), extra...)
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	if msg == "" {
		return fmt.Errorf("LLM gateway: %s: HTTP %d", what, code)
	}
	return fmt.Errorf("LLM gateway: %s: HTTP %d: %s", what, code, msg)
}

// Ping checks the gateway answers at all (no key needed).
func (c *llmClient) Ping(ctx context.Context) error {
	code, _, err := c.do(ctx, "GET", "/health/liveliness", nil, "-")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("LLM gateway %s: /health/liveliness answered HTTP %d", c.base, code)
	}
	return nil
}

type llmKey struct {
	Alias     string         `json:"key_alias"`
	Spend     float64        `json:"spend"`
	MaxBudget *float64       `json:"max_budget"`
	Metadata  map[string]any `json:"metadata"`
}

func (k llmKey) meta(name string) string {
	s, _ := k.Metadata[name].(string)
	return s
}

// Keys lists every key the provisioning key can see (all pages).
func (c *llmClient) Keys(ctx context.Context) ([]llmKey, error) {
	var out []llmKey
	for page := 1; page <= 200; page++ {
		code, b, err := c.do(ctx, "GET", fmt.Sprintf("/key/list?return_full_object=true&size=100&page=%d", page), nil, c.provKey)
		if err != nil {
			return nil, err
		}
		if code != 200 {
			return nil, c.apiErr("listing keys", code, b)
		}
		var r struct {
			Keys       []json.RawMessage `json:"keys"`
			TotalPages int               `json:"total_pages"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("LLM gateway: listing keys: not JSON")
		}
		for _, raw := range r.Keys {
			var k llmKey
			if json.Unmarshal(raw, &k) == nil && k.Alias != "" {
				out = append(out, k)
			}
		}
		if page >= r.TotalPages {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil
}

// Generate mints the worker's key. The returned key is the only copy this
// program ever holds; it is passed on to the worker and dropped.
func (c *llmClient) Generate(ctx context.Context, cfg *Config, name string) (string, error) {
	// The gateway enforces nothing on a minted key: no models means ALL
	// models, and every limit is whatever is sent. So refuse to mint without
	// each of them, whatever the config says.
	if len(cfg.LLMModels) == 0 || cfg.LLMMaxBudget <= 0 || cfg.LLMBudgetDuration == "" || cfg.LLMRPM <= 0 {
		return "", fmt.Errorf("refusing to mint %s without explicit models, max_budget, budget_duration and rpm_limit", llmAlias(name))
	}
	body := map[string]any{
		"user_id":         llmProvisionerUser,
		"key_alias":       llmAlias(name),
		"models":          cfg.LLMModels,
		"max_budget":      cfg.LLMMaxBudget,
		"budget_duration": cfg.LLMBudgetDuration,
		"rpm_limit":       cfg.LLMRPM,
		"metadata":        map[string]string{"created_by": llmCreatedBy, "worker": name, "scope": llmScope(cfg)},
	}
	if cfg.LLMKeyDuration != "" {
		body["duration"] = cfg.LLMKeyDuration
	}
	code, b, err := c.do(ctx, "POST", "/key/generate", body, c.provKey)
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", c.apiErr("generating the key for "+llmAlias(name), code, b)
	}
	var r struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(b, &r) != nil || r.Key == "" {
		return "", fmt.Errorf("LLM gateway: the key for %s came back without a key", llmAlias(name))
	}
	return r.Key, nil
}

// DeleteAlias deletes the key with that alias. A missing key is fine.
func (c *llmClient) DeleteAlias(ctx context.Context, alias string) (existed bool, err error) {
	keys, err := c.Keys(ctx)
	if err != nil {
		return false, err
	}
	found := false
	for _, k := range keys {
		if k.Alias == alias {
			found = true
		}
	}
	if !found {
		return false, nil
	}
	code, b, err := c.do(ctx, "POST", "/key/delete", map[string]any{"key_aliases": []string{alias}}, c.provKey)
	if err != nil {
		return true, err
	}
	if code != 200 {
		return true, c.apiErr("deleting "+alias, code, b)
	}
	return true, nil
}

// Refused reports whether the gateway now turns the key away. Anything but a
// clear 401/403 on a model listing is "unknown" (err) or "still valid".
func (c *llmClient) Refused(ctx context.Context, key string) (bool, error) {
	code, b, err := c.do(ctx, "GET", "/v1/models", nil, key)
	if err != nil {
		return false, err
	}
	switch {
	case code == 401 || code == 403:
		return true, nil
	case code == 200:
		return false, nil
	}
	return false, c.apiErr("checking that the deleted key is refused", code, b, key)
}

// ---- on the worker ----

// piConfigFiles is the non-secret part of pi's setup: the gateway as a custom
// provider (no apiKey: auth.json supplies it) and the default model. No
// maxTokens is set: glm-style models spend a small output limit on
// reasoning and return nothing.
func piConfigFiles(c *Config) (models, settings string) {
	var ms []map[string]string
	for _, m := range c.LLMModels {
		ms = append(ms, map[string]string{"id": m})
	}
	mj, _ := json.Marshal(map[string]any{"providers": map[string]any{llmProvider: map[string]any{
		"baseUrl": c.LLMGatewayURL + "/v1",
		"api":     "openai-completions",
		"models":  ms,
	}}})
	sj, _ := json.Marshal(map[string]string{"defaultProvider": llmProvider, "defaultModel": c.LLMModels[0]})
	return string(mj), string(sj)
}

// piConfigScript merges those two fragments into the worker's pi directory,
// keeping whatever else is there. It carries no secret.
func piConfigScript(c *Config) string {
	m, s := piConfigFiles(c)
	return "#!/bin/bash\nset -euo pipefail\nd=\"$HOME/.pi/agent\"; mkdir -p \"$d\"\n" +
		"merge() { f=\"$d/$1\"; [ -s \"$f\" ] || echo '{}' >\"$f\"; printf '%s' \"$2\" | jq -s '.[0] * .[1]' \"$f\" - >\"$f.tmp\" && mv \"$f.tmp\" \"$f\"; }\n" +
		"merge models.json " + shellQuote(m) + "\n" +
		"merge settings.json " + shellQuote(s) + "\n"
}

// authMergeCmd merges the JSON fragment on stdin into auth.json, which is
// created and kept at mode 0600. The command line holds no secret.
const authMergeCmd = `set -e; umask 077; d="$HOME/.pi/agent"; mkdir -p "$d"; f="$d/auth.json"; [ -s "$f" ] || echo '{}' >"$f"; t=$(mktemp "$d/.auth.XXXXXX"); cat >"$t"; jq -s '.[0] * .[1]' "$f" "$t" >"$t.m" && mv "$t.m" "$f"; rm -f "$t" "$t.m"; chmod 600 "$f"`

const authHasKeyCmd = `jq -e --arg p ` + llmProvider + ` '.[$p].key | type == "string" and length > 0' "$HOME/.pi/agent/auth.json" >/dev/null`
const authModeCmd = `stat -c %a "$HOME/.pi/agent/auth.json"`
const authReadKeyCmd = `jq -r --arg p ` + llmProvider + ` '.[$p].key // empty' "$HOME/.pi/agent/auth.json" 2>/dev/null`

// llmPreflight runs before anything is created: a worker with no way to get
// its key would be built for nothing.
func (e *env) llmPreflight(ctx context.Context) error {
	if e.Cfg.LLMGatewayURL == "" {
		return nil
	}
	c, err := e.llm()
	if err != nil {
		return err
	}
	if err := c.Ping(ctx); err != nil {
		return fmt.Errorf("%w\nnothing was created: the LLM gateway has to be reachable from this machine to mint the worker's key (or unset llm_gateway_url)", err)
	}
	return nil
}

// provisionLLM puts the gateway config and a fresh per-worker key on the
// worker. It is safe to run again: a key already delivered is kept.
func (e *env) provisionLLM(ctx context.Context, name string) error {
	if e.Cfg.LLMGatewayURL == "" {
		return nil
	}
	e.printf("  llm gateway %s: pi provider %s, models %s\n", e.Cfg.LLMGatewayURL, llmProvider, strings.Join(e.Cfg.LLMModels, ", "))
	if _, err := ssh(ctx, e, name, "bash -s", []byte(piConfigScript(e.Cfg))); err != nil {
		return fmt.Errorf("writing pi's models.json and settings.json: %w", err)
	}
	if _, err := ssh(ctx, e, name, authHasKeyCmd, nil); err == nil {
		e.printf("  the worker already has its key: kept\n")
		return nil
	}
	c, err := e.llm()
	if err != nil {
		return err
	}
	alias := llmAlias(name)
	if existed, err := c.DeleteAlias(ctx, alias); err != nil {
		return err
	} else if existed {
		e.printf("  a key %s existed on the gateway but was never delivered: replaced\n", alias)
	}
	key, err := c.Generate(ctx, e.Cfg, name)
	if err != nil {
		return err
	}
	frag, _ := json.Marshal(map[string]any{llmProvider: map[string]string{"type": "api_key", "key": key}})
	if _, err := ssh(ctx, e, name, authMergeCmd, frag); err != nil {
		c.DeleteAlias(ctx, alias) // never delivered: do not leave it live
		return fmt.Errorf("delivering the key: %w", redact(err, key))
	}
	out, err := ssh(ctx, e, name, authModeCmd, nil)
	if err != nil || strings.TrimSpace(out) != "600" {
		c.DeleteAlias(ctx, alias)
		return fmt.Errorf("auth.json on %s is not mode 600 (got %q): key revoked", name, strings.TrimSpace(out))
	}
	e.printf("  minted %s (budget $%g per %s, %d requests/min) and wrote it to ~/.pi/agent/auth.json, mode 600\n",
		alias, e.Cfg.LLMMaxBudget, e.Cfg.LLMBudgetDuration, e.Cfg.LLMRPM)
	return nil
}

// revokeLLM deletes the worker's key at the start of destroy and, if it can
// read the key back from the running worker, checks the gateway refuses it.
func (e *env) revokeLLM(ctx context.Context, name string, running bool) (problem string) {
	if e.Cfg.LLMGatewayURL == "" {
		return ""
	}
	alias := llmAlias(name)
	c, err := e.llm()
	if err != nil {
		e.printf("  llm key %s: %v\n  it stays valid: delete it with `worker keys --prune` once you have the provisioning key\n", alias, err)
		return "llm key"
	}
	held := ""
	if running {
		if err := e.connect(ctx, name); err == nil {
			out, _ := ssh(ctx, e, name, authReadKeyCmd, nil)
			held = strings.TrimSpace(out)
		}
	}
	existed, err := c.DeleteAlias(ctx, alias)
	if err != nil {
		e.printf("  llm key %s: %v\n  it stays valid: `worker keys --prune` deletes it once the VM is gone\n", alias, redact(err, held))
		return "llm key"
	}
	if !existed {
		e.printf("  llm key %s: none on the gateway\n", alias)
		return ""
	}
	if held == "" {
		e.printf("  llm key %s: deleted (could not read it back from the VM to test it)\n", alias)
		return ""
	}
	refused, err := c.Refused(ctx, held)
	switch {
	case err != nil:
		e.printf("  llm key %s: deleted; the refusal check failed: %v\n", alias, redact(err, held))
		return "llm key refusal check"
	case !refused:
		e.printf("  llm key %s: deleted, but the gateway still accepts it\n", alias)
		return "llm key still accepted"
	}
	e.printf("  llm key %s: deleted, and the gateway refuses it\n", alias)
	return ""
}

// ---- worker keys ----

func (e *env) keys(ctx context.Context, prune bool) error {
	if e.Cfg.LLMGatewayURL == "" {
		return fmt.Errorf("llm_gateway_url is not set: there are no worker keys to list")
	}
	c, err := e.llm()
	if err != nil {
		return err
	}
	keys, err := c.Keys(ctx)
	if err != nil {
		return err
	}
	scope := llmScope(e.Cfg)
	e.printf("%-22s %-9s %-14s %s\n", "KEY", "VM", "SPEND/BUDGET", "SCOPE")
	n, pruned, failed := 0, 0, 0
	for _, k := range keys {
		if !strings.HasPrefix(k.Alias, llmAliasPrefix) {
			continue
		}
		n++
		name := strings.TrimPrefix(k.Alias, llmAliasPrefix)
		ours := k.meta("created_by") == llmCreatedBy && k.meta("scope") == scope
		vm := "unknown"
		var inst *Inst
		var derr error
		if nameRe.MatchString(name) && ours {
			if inst, derr = e.prov().Describe(ctx, name); derr == nil {
				vm = "yes"
				if inst == nil {
					vm = "none"
				}
			}
		}
		budget := "-"
		if k.MaxBudget != nil {
			budget = fmt.Sprintf("$%.2f/$%.2f", k.Spend, *k.MaxBudget)
		} else {
			budget = fmt.Sprintf("$%.2f/-", k.Spend)
		}
		sc := k.meta("scope")
		if !ours {
			sc = orDash(sc) + " (not ours: never touched)"
		}
		e.printf("%-22s %-9s %-14s %s\n", k.Alias, vm, budget, sc)
		if prune && ours && vm == "none" {
			if _, err := c.DeleteAlias(ctx, k.Alias); err != nil {
				e.printf("  could not delete %s: %v\n", k.Alias, err)
				failed++
			} else {
				e.printf("  deleted %s (no VM named %s)\n", k.Alias, name)
				pruned++
			}
		}
	}
	if n == 0 {
		e.printf("(no %s* keys on the gateway)\n", llmAliasPrefix)
	}
	if prune {
		e.printf("%d deleted\n", pruned)
	} else if n > 0 {
		e.printf("`worker keys --prune` deletes the ones marked VM none.\n")
	}
	if failed > 0 {
		return fmt.Errorf("%d key(s) could not be deleted", failed)
	}
	return nil
}

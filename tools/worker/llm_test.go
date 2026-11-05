package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	llmHost = "llm.tail1.ts.net"
	provKey = "sk-PROVISIONER-DONOTLEAK"
)

type fakeKey struct {
	Alias    string
	Value    string
	Body     map[string]any
	Spend    float64
	Metadata map[string]any
}

// fakeLiteLLM is the slice of a LiteLLM proxy the provisioner uses: it only
// honours the provisioning key on key routes, and enforces nothing on a
// minted key (so the tool has to send every limit itself).
type fakeLiteLLM struct {
	down        bool
	deleteFail  bool
	keys        []*fakeKey
	active      map[string]bool // key value -> still accepted by /v1/models
	gens        []map[string]any
	n           int
	calls       []string
	listBadAuth bool
}

func newFakeLLM() *fakeLiteLLM { return &fakeLiteLLM{active: map[string]bool{}} }

func (l *fakeLiteLLM) add(alias string, meta map[string]any) *fakeKey {
	l.n++
	k := &fakeKey{Alias: alias, Value: fmt.Sprintf("sk-worker-%d-DONOTLEAK", l.n), Metadata: meta}
	l.keys = append(l.keys, k)
	l.active[k.Value] = true
	return k
}

func (l *fakeLiteLLM) find(alias string) *fakeKey {
	for _, k := range l.keys {
		if k.Alias == alias {
			return k
		}
	}
	return nil
}

func (l *fakeLiteLLM) serve(r *http.Request) (*http.Response, error) {
	l.calls = append(l.calls, r.Method+" "+r.URL.Path)
	if l.down {
		return nil, errors.New("dial tcp: connect: connection refused")
	}
	auth := r.Header.Get("Authorization")
	switch {
	case r.URL.Path == "/health/liveliness":
		return reply(200, `"I'm alive!"`), nil
	case r.URL.Path == "/v1/models":
		if v := strings.TrimPrefix(auth, "Bearer "); l.active[v] && v != "" {
			return reply(200, `{"data":[]}`), nil
		}
		return reply(401, `{"error":{"message":"Authentication Error, Invalid proxy server token passed"}}`), nil
	}
	if auth != "Bearer "+provKey {
		return reply(401, `{"error":{"message":"bad provisioning key"}}`), nil
	}
	var body map[string]any
	if r.Body != nil {
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			json.Unmarshal(b, &body)
		}
	}
	switch r.URL.Path {
	case "/key/generate":
		alias, _ := body["key_alias"].(string)
		if l.find(alias) != nil {
			return reply(400, `{"error":{"message":"Unique key aliases across all keys are required. Key alias=`+alias+` already exists"}}`), nil
		}
		l.gens = append(l.gens, body)
		meta, _ := body["metadata"].(map[string]any)
		k := l.add(alias, meta)
		k.Body = body
		return reply(200, fmt.Sprintf(`{"key":%q,"key_alias":%q}`, k.Value, alias)), nil
	case "/key/list":
		var objs []map[string]any
		for _, k := range l.keys {
			o := map[string]any{"key_alias": k.Alias, "spend": k.Spend, "metadata": k.Metadata}
			if k.Body != nil {
				o["max_budget"] = k.Body["max_budget"]
			}
			objs = append(objs, o)
		}
		b, _ := json.Marshal(map[string]any{"keys": objs, "total_count": len(objs), "current_page": 1, "total_pages": 1})
		return reply(200, string(b)), nil
	case "/key/delete":
		if l.deleteFail {
			return reply(500, `{"error":{"message":"db is down (token `+provKey+`)"}}`), nil
		}
		as, _ := body["key_aliases"].([]any)
		for _, a := range as {
			alias, _ := a.(string)
			for i, k := range l.keys {
				if k.Alias == alias {
					l.active[k.Value] = false
					l.keys = append(l.keys[:i], l.keys[i+1:]...)
					break
				}
			}
		}
		return reply(200, `{"deleted_keys":1}`), nil
	}
	return reply(404, ""), nil
}

// fakeVM is what the worker's disk holds, as far as the tool can tell.
type fakeVM struct {
	authKey    string // the key written to auth.json
	mode       string
	modelsJSON string
	mergeFail  bool
	authCalls  int
}

// llmEnv is an AWS create/destroy environment with the gateway on.
func llmEnv(t *testing.T, f *awsFake, w *fakeWorld) (*env, *fakeRunner, *Config, *fakeLiteLLM, *fakeVM) {
	t.Helper()
	e, r, c := awsEnv(t, f, w)
	c.LLMGatewayURL = "https://" + llmHost
	c.LLMProvisionerKeyFile = filepath.Join(t.TempDir(), "llm-provisioner-key")
	if err := os.WriteFile(c.LLMProvisionerKeyFile, []byte(provKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	l := newFakeLLM()
	w.llm = l
	vm := &fakeVM{mode: "600"}
	inner := r.handle
	r.handle = func(cl call, fr *fakeRunner) (string, string, error) {
		if cl.name == "ssh" {
			switch remote := cl.args[len(cl.args)-1]; {
			case remote == authHasKeyCmd:
				if vm.authKey == "" {
					return "", "", errors.New("exit status 1")
				}
				return "", "", nil
			case remote == authMergeCmd:
				vm.authCalls++
				if vm.mergeFail {
					return "", "write failed", errors.New("exit status 1")
				}
				var frag map[string]map[string]string
				if err := json.Unmarshal([]byte(cl.stdin), &frag); err != nil || frag[llmProvider]["type"] != "api_key" {
					return "", "bad fragment", errors.New("exit status 1")
				}
				vm.authKey = frag[llmProvider]["key"]
				return "", "", nil
			case remote == authModeCmd:
				return vm.mode + "\n", "", nil
			case remote == authReadKeyCmd:
				return vm.authKey + "\n", "", nil
			case remote == "bash -s" && strings.Contains(cl.stdin, "merge models.json"):
				vm.modelsJSON = cl.stdin
				return "", "", nil
			}
		}
		return inner(cl, fr)
	}
	return e, r, c, l, vm
}

func newWorld() *fakeWorld {
	return &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
}

func TestLLMConfig(t *testing.T) {
	c := testAWSCfg(t)
	if c.LLMGatewayURL != "" || !c.hasAgent("pi") || c.PiVersion != "0.85.1" || c.LLMModels[0] != "rits/zai-org/glm-5-3" {
		t.Errorf("defaults: %+v", c)
	}
	base := "provider=aws\naws_region=us-east-1\naws_az=us-east-1b\nssh_source_cidr=198.51.100.1/32\nts_allow_login=a@b.co\nhub_url=https://h.t.ts.net\nhub_peer_url=https://h.t.ts.net\n"
	p := filepath.Join(t.TempDir(), "config")
	os.WriteFile(p, []byte(base+"llm_gateway_url=https://llm.t.ts.net/\nllm_models=a/b, c\nllm_max_budget=2.5\nllm_budget_duration=7d\nllm_rpm=10\nagents=pi,codex\npi_version=0.85.2\n"), 0o600)
	lc, err := LoadConfig(p)
	if err != nil || lc.LLMGatewayURL != "https://llm.t.ts.net" || len(lc.LLMModels) != 2 || lc.LLMMaxBudget != 2.5 || lc.LLMRPM != 10 || lc.PiVersion != "0.85.2" || !lc.hasAgent("codex") {
		t.Fatalf("load: %+v %v", lc, err)
	}
	for name, line := range map[string]string{
		"http gateway":    "llm_gateway_url=http://llm.t.ts.net\n",
		"not tailnet":     "llm_gateway_url=https://llm.example.com\n",
		"no pi":           "llm_gateway_url=https://llm.t.ts.net\nagents=claude\n",
		"bad model":       "llm_gateway_url=https://llm.t.ts.net\nllm_models=a;rm -rf /\n",
		"zero budget":     "llm_gateway_url=https://llm.t.ts.net\nllm_max_budget=0\n",
		"bad duration":    "llm_gateway_url=https://llm.t.ts.net\nllm_budget_duration=forever\n",
		"key expiry":      "llm_gateway_url=https://llm.t.ts.net\nllm_key_duration=30d\n",
		"zero rpm":        "llm_gateway_url=https://llm.t.ts.net\nllm_rpm=0\n",
		"unknown agent":   "agents=pi,gemini\n",
		"unpinned pi":     "pi_version=latest\n",
		"spaced key file": "llm_gateway_url=https://llm.t.ts.net\nllm_models=\n",
	} {
		os.WriteFile(p, []byte(base+line), 0o600)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLLMCreateDeliversAKeyAndNeverLeaksIt(t *testing.T) {
	w := newWorld()
	f := &awsFake{console: consoleKey}
	e, r, c, l, vm := llmEnv(t, f, w)
	var out, errb strings.Builder
	e.Out, e.Err = &out, &errb
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatalf("create: %v\n%s", err, out.String())
	}
	if len(l.gens) != 1 {
		t.Fatalf("keys minted: %d", len(l.gens))
	}
	g := l.gens[0]
	if g["user_id"] != llmProvisionerUser || g["key_alias"] != "worker-w1" || g["budget_duration"] != "30d" || g["max_budget"] != 5.0 || g["rpm_limit"] != 30.0 {
		t.Errorf("generate body: %v", g)
	}
	if ms, _ := g["models"].([]any); len(ms) != 1 || ms[0] != "rits/zai-org/glm-5-3" {
		t.Errorf("models: %v", g["models"])
	}
	if _, has := g["duration"]; has {
		t.Error("a key duration was sent without being configured")
	}
	if m, _ := g["metadata"].(map[string]any); m["created_by"] != "comebye-worker" || m["worker"] != "w1" || m["scope"] != "aws:us-east-1" {
		t.Errorf("metadata: %v", g["metadata"])
	}
	key := l.find("worker-w1").Value
	if vm.authKey != key {
		t.Error("the worker did not get the key that was minted")
	}
	// the key travelled on stdin of exactly one ssh call, and is nowhere else
	onStdin := 0
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), key) || strings.Contains(cl.line(), provKey) {
			t.Errorf("a secret is in argv: %s", cl.line())
		}
		if strings.Contains(cl.stdin, key) {
			onStdin++
			if cl.name != "ssh" || cl.args[len(cl.args)-1] != authMergeCmd {
				t.Errorf("the key went to %s %v", cl.name, cl.args[len(cl.args)-1])
			}
		}
		if strings.Contains(cl.stdin, provKey) {
			t.Error("the provisioning key was sent to a host")
		}
	}
	if onStdin != 1 {
		t.Errorf("key on stdin of %d calls", onStdin)
	}
	for name, s := range map[string]string{"out": out.String(), "err": errb.String(), "user-data": f.userData, "models.json script": vm.modelsJSON} {
		if strings.Contains(s, key) || strings.Contains(s, provKey) {
			t.Errorf("a secret is in %s", name)
		}
	}
	filepath.Walk(c.StateDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(key)) || bytes.Contains(b, []byte(provKey)) {
				t.Errorf("a secret is in %s", p)
			}
		}
		return nil
	})
	// pi's config: the gateway as a provider, no apiKey, no token limit
	for _, want := range []string{`"baseUrl":"https://` + llmHost + `/v1"`, `"api":"openai-completions"`, `"id":"rits/zai-org/glm-5-3"`, `"defaultProvider":"homelab-enmaas"`} {
		if !strings.Contains(vm.modelsJSON, want) {
			t.Errorf("pi config lacks %s:\n%s", want, vm.modelsJSON)
		}
	}
	for _, bad := range []string{"maxTokens", "apiKey"} {
		if strings.Contains(vm.modelsJSON, bad) {
			t.Errorf("pi config has %s", bad)
		}
	}
	// pi is installed by the user bootstrap at the pinned version
	us, _ := userScript(c, "w1", "v0.3.1")
	if !strings.Contains(us, "WK_AGENTS='pi'") || !strings.Contains(us, "WK_PI_VERSION='0.85.1'") {
		t.Error("the user bootstrap does not carry the pi pin")
	}
}

func TestLLMResumeKeepsTheDeliveredKey(t *testing.T) {
	w := newWorld()
	f := &awsFake{console: consoleKey}
	e, _, _, l, _ := llmEnv(t, f, w)
	e.Out = io.Discard
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	if len(l.gens) != 1 || l.find("worker-w1") == nil {
		t.Errorf("a resumed create re-minted: %d keys minted", len(l.gens))
	}
}

func TestLLMUndeliveredKeyIsReplacedNotReused(t *testing.T) {
	w := newWorld()
	f := &awsFake{console: consoleKey}
	e, _, _, l, vm := llmEnv(t, f, w)
	e.Out = io.Discard
	old := l.add("worker-w1", map[string]any{"created_by": llmCreatedBy})
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	if l.active[old.Value] || vm.authKey == old.Value || vm.authKey == "" {
		t.Error("the old, never-delivered key was kept or reused")
	}
}

func TestLLMGatewayDownFailsBeforeAnythingIsBuilt(t *testing.T) {
	w := newWorld()
	f := &awsFake{console: consoleKey}
	e, r, _, l, _ := llmEnv(t, f, w)
	e.Out = io.Discard
	l.down = true
	err := e.create(context.Background(), "w1", createOpts{})
	if err == nil || !strings.Contains(err.Error(), "LLM gateway") || !strings.Contains(err.Error(), "nothing was created") {
		t.Fatalf("error: %v", err)
	}
	for _, cl := range r.calls {
		if cl.name == "aws" && !strings.Contains(cl.line(), "sts") {
			t.Fatalf("an aws call was made before the gateway check: %s", cl.line())
		}
	}
	if f.did("run-instances") || f.did("create-vpc") {
		t.Error("cloud resources were created")
	}
}

func TestLLMProvisionerKeyFileProblems(t *testing.T) {
	for name, setup := range map[string]func(string){
		"missing":    func(p string) { os.Remove(p) },
		"group read": func(p string) { os.Chmod(p, 0o640) },
		"two words":  func(p string) { os.WriteFile(p, []byte("a b\n"), 0o600) },
	} {
		w := newWorld()
		e, _, c, _, _ := llmEnv(t, &awsFake{console: consoleKey}, w)
		e.Out = io.Discard
		setup(c.LLMProvisionerKeyFile)
		err := e.create(context.Background(), "w1", createOpts{})
		if err == nil || strings.Contains(err.Error(), provKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLLMRefusesToMintWithoutLimits(t *testing.T) {
	c := &llmClient{base: "https://" + llmHost, http: newWorld(), provKey: provKey}
	w := c.http.(*fakeWorld)
	w.llm = newFakeLLM()
	for name, mut := range map[string]func(*Config){
		"no models":   func(c *Config) { c.LLMModels = nil },
		"no budget":   func(c *Config) { c.LLMMaxBudget = 0 },
		"no duration": func(c *Config) { c.LLMBudgetDuration = "" },
		"no rpm":      func(c *Config) { c.LLMRPM = 0 },
	} {
		cfg := defaults()
		mut(&cfg)
		if _, err := c.Generate(context.Background(), &cfg, "w1"); err == nil || !strings.Contains(err.Error(), "refusing to mint") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(w.llm.calls) != 0 {
		t.Errorf("a request was sent: %v", w.llm.calls)
	}
}

func TestLLMDeliveryFailureRevokesTheKeyAndHidesIt(t *testing.T) {
	w := newWorld()
	e, _, _, l, vm := llmEnv(t, &awsFake{console: consoleKey}, w)
	e.Out = io.Discard
	vm.mergeFail = true
	err := e.create(context.Background(), "w1", createOpts{})
	if err == nil || !strings.Contains(err.Error(), "delivering the key") {
		t.Fatalf("error: %v", err)
	}
	if len(l.keys) != 0 {
		t.Error("a key that never reached the worker is still live")
	}
	if strings.Contains(err.Error(), "sk-worker-") {
		t.Errorf("the key is in the error: %v", err)
	}
}

func TestLLMLooseAuthFileRevokesTheKey(t *testing.T) {
	w := newWorld()
	e, _, _, l, vm := llmEnv(t, &awsFake{console: consoleKey}, w)
	e.Out = io.Discard
	vm.mode = "644"
	if err := e.create(context.Background(), "w1", createOpts{}); err == nil || !strings.Contains(err.Error(), "not mode 600") {
		t.Fatalf("error: %v", err)
	}
	if len(l.keys) != 0 {
		t.Error("the key stayed live after a bad file mode")
	}
}

func destroyLLM(t *testing.T, f *awsFake) (*env, *fakeRunner, *fakeLiteLLM, *fakeVM, *strings.Builder) {
	w := newWorld()
	w.peers["w1"] = true
	e, r, _, l, vm := llmEnv(t, f, w)
	e.Out = io.Discard
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	e.Out = &out
	e.In = strings.NewReader("w1\n")
	return e, r, l, vm, &out
}

func TestLLMDestroyDeletesTheKeyFirstAndChecksItIsRefused(t *testing.T) {
	f := &awsFake{console: consoleKey}
	e, _, l, vm, out := destroyLLM(t, f)
	key := vm.authKey
	if !l.active[key] {
		t.Fatal("setup: the key is not live")
	}
	if err := e.destroy(context.Background(), "w1", destroyOpts{}); err != nil {
		t.Fatalf("destroy: %v\n%s", err, out)
	}
	if l.active[key] || l.find("worker-w1") != nil {
		t.Error("the key is still live")
	}
	if !strings.Contains(out.String(), "the gateway refuses it") {
		t.Errorf("no refusal check:\n%s", out)
	}
	if !f.did("terminate-instances") {
		t.Error("the instance was not terminated")
	}
	if strings.Contains(out.String(), key) || strings.Contains(out.String(), provKey) {
		t.Error("a secret is in the output")
	}
	idx := func(s string) int {
		for i, c := range l.calls {
			if c == s {
				return i
			}
		}
		return -1
	}
	if idx("POST /key/delete") < 0 || idx("GET /v1/models") < idx("POST /key/delete") {
		t.Errorf("gateway calls out of order: %v", l.calls)
	}
}

func TestLLMDestroyWarnsAndLeavesTheKeyForPruneWhenDeleteFails(t *testing.T) {
	f := &awsFake{console: consoleKey}
	e, _, l, vm, out := destroyLLM(t, f)
	key := vm.authKey
	l.deleteFail = true
	err := e.destroy(context.Background(), "w1", destroyOpts{})
	if err == nil || !strings.Contains(err.Error(), "llm key") {
		t.Fatalf("destroy error: %v", err)
	}
	if !f.did("terminate-instances") {
		t.Error("the rest of destroy did not run")
	}
	if !l.active[key] || l.find("worker-w1") == nil {
		t.Error("setup: the key should still be live")
	}
	for _, s := range []string{out.String(), err.Error()} {
		if strings.Contains(s, key) || strings.Contains(s, provKey) {
			t.Errorf("a secret is in: %s", s)
		}
	}
	if !strings.Contains(out.String(), "worker keys --prune") {
		t.Errorf("no pointer to prune:\n%s", out)
	}
	// prune cleans it up once the gateway works and the VM is gone
	l.deleteFail = false
	e.P = stubProv{}
	out.Reset()
	if err := e.keys(context.Background(), true); err != nil {
		t.Fatalf("prune: %v\n%s", err, out)
	}
	if l.find("worker-w1") != nil || l.active[key] {
		t.Errorf("prune left the key:\n%s", out)
	}
}

// stubProv knows which workers have a VM.
type stubProv struct {
	Provider
	vms map[string]bool
	err error
}

func (s stubProv) Describe(_ context.Context, name string) (*Inst, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.vms[name] {
		return &Inst{Name: name, State: stateRunning, Managed: true}, nil
	}
	return nil, nil
}

func TestLLMKeysListsAndPrunesOnlyOrphansOfThisCloud(t *testing.T) {
	w := newWorld()
	e, _, c, l, _ := llmEnv(t, &awsFake{}, w)
	var out strings.Builder
	e.Out = &out
	mine := map[string]any{"created_by": llmCreatedBy, "scope": llmScope(c)}
	live := l.add("worker-live", mine)
	orphan := l.add("worker-gone", mine)
	foreign := l.add("worker-theirs", map[string]any{"created_by": llmCreatedBy, "scope": "aws:eu-west-1"})
	unmarked := l.add("worker-nometa", nil)
	other := l.add("someone-elses-key", nil)
	e.P = stubProv{vms: map[string]bool{"live": true}}

	if err := e.keys(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "worker-gone") || strings.Contains(out.String(), "someone-elses-key") || len(l.keys) != 5 {
		t.Errorf("listing:\n%s", out.String())
	}
	out.Reset()
	if err := e.keys(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if l.active[orphan.Value] {
		t.Error("the orphan survived")
	}
	for name, k := range map[string]*fakeKey{"live VM": live, "another region": foreign, "no metadata": unmarked, "not a worker key": other} {
		if !l.active[k.Value] {
			t.Errorf("prune deleted the key of %s", name)
		}
	}
	// a cloud error is not "no VM"
	l.add("worker-maybe", mine)
	e.P = stubProv{err: errors.New("cloud api down")}
	out.Reset()
	if err := e.keys(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if l.find("worker-maybe") == nil {
		t.Error("deleted a key whose VM could not be checked")
	}
}

func TestLLMOffMeansNoGatewayCalls(t *testing.T) {
	w := newWorld()
	e, _, _ := awsEnv(t, &awsFake{console: consoleKey}, w)
	e.Out = io.Discard
	w.llm = newFakeLLM()
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	if len(w.llm.calls) != 0 {
		t.Errorf("gateway calls with llm_gateway_url unset: %v", w.llm.calls)
	}
	if err := e.keys(context.Background(), false); err == nil {
		t.Error("keys worked with the gateway off")
	}
}

func TestLLMDryRunShowsThePlanWithoutCalls(t *testing.T) {
	w := newWorld()
	e, r, _, l, _ := llmEnv(t, &awsFake{}, w)
	var out strings.Builder
	e.Out = &out
	e.DryRun = true
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"worker-w1", llmProvisionerUser, "rits/zai-org/glm-5-3", "ssh stdin", "mode 600", "pi pinned at 0.85.1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan lacks %q", want)
		}
	}
	if strings.Contains(out.String(), provKey) || len(l.calls) != 0 {
		t.Error("dry run called the gateway or printed a key")
	}
	_ = r
}

func TestPiBootstrapScriptPinsAndFails(t *testing.T) {
	if !strings.Contains(bootstrapUser, `@earendil-works/pi-coding-agent@$WK_PI_VERSION`) || !strings.Contains(bootstrapUser, `: "${WK_PI_VERSION:?}"`) {
		t.Error("pi is not installed at the pinned version")
	}
	if !strings.Contains(bootstrapUser, `installing pi $WK_PI_VERSION failed`) {
		t.Error("a failed pi install must stop the bootstrap")
	}
}

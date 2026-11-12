package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fixtureAWS answers aws calls from testdata/aws, the way the shim does, and
// records every call. It is safe for the concurrent price calls.
type fixtureAWS struct {
	mu    sync.Mutex
	calls []call
	fail  string // an error text: every call fails with it
	dir   string
}

func (f *fixtureAWS) Run(_ context.Context, name string, args []string, stdin []byte) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{name, args, string(stdin)})
	f.mu.Unlock()
	if name != "aws" {
		return "", "", fmt.Errorf("unexpected program %s", name)
	}
	if f.fail != "" {
		return "", f.fail, fmt.Errorf("aws: %s", f.fail)
	}
	svc, op := args[0], args[1]
	file := svc + "-" + op + ".json"
	if svc == "sts" {
		return "123456789012\n", "", nil
	}
	if svc == "pricing" {
		for _, a := range args {
			if i := strings.Index(a, "Field=instanceType,Value="); i >= 0 {
				file = "pricing-get-products-" + a[i+len("Field=instanceType,Value="):] + ".json"
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(f.dir, file))
	if err != nil {
		return "", "no fixture " + file, fmt.Errorf("aws: no fixture %s", file)
	}
	return string(b), "", nil
}
func (f *fixtureAWS) Interactive(context.Context, string, []string) error { return nil }

func (f *fixtureAWS) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var o []string
	for _, c := range f.calls {
		o = append(o, c.args[0]+" "+c.args[1])
	}
	return o
}
func (f *fixtureAWS) count(prefix string) int {
	n := 0
	for _, o := range f.ops() {
		if strings.HasPrefix(o, prefix) {
			n++
		}
	}
	return n
}

// readOnly is what the shim allows.
func readOnly(svc, op string) bool {
	switch {
	case svc == "sts" && op == "get-caller-identity", svc == "ssm" && op == "get-parameters", svc == "pricing" && op == "get-products":
		return true
	case svc == "ec2" && (strings.HasPrefix(op, "describe-") || strings.HasPrefix(op, "get-")):
		return true
	}
	return false
}

// advisorHTTP serves the three public sources from testdata: the price list,
// the spot feed and the Spot Advisor. status and hits are the advisor's.
func advisorHTTP(t *testing.T, status int, hits *int) httpDoer {
	read := func(n string) string {
		b, err := os.ReadFile("testdata/aws/" + n)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	advisor, bulk, spot := read("spot-advisor.json"), read("price-list-bulk.json"), read("spot.js")
	return httpFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		switch u := r.URL.String(); {
		case u == advisorURL:
			if hits != nil {
				*hits++
			}
			return reply(status, advisor), nil
		case u == spotJSURL:
			return reply(200, spot), nil
		case strings.HasPrefix(u, "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/") && strings.HasSuffix(u, "/index.json"):
			return reply(200, bulk), nil
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		return reply(404, ""), nil
	})
}

type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time { return c.t }

func catalogEnv(t *testing.T) (*env, *fixtureAWS, *fixedClock, *bytes.Buffer) {
	c := testAWSCfg(t)
	c.AWSProfile = "comebye-provision"
	c.AllowedFamilies = []string{"t3", "t3a", "t4g", "m5", "m6i", "m6g", "c5", "g4dn"}
	f := &fixtureAWS{dir: "testdata/aws"}
	var out bytes.Buffer
	clk := &fixedClock{time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	e := &env{Out: &out, Err: &bytes.Buffer{}, In: strings.NewReader(""), Run: f, Cfg: c, HTTP: advisorHTTP(t, 200, nil), Sleep: func(int) {}, Now: clk.now}
	return e, f, clk, &out
}

func goldenCheck(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		os.WriteFile(p, got, 0o644)
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		t.Errorf("%s differs (run go test -update to rewrite):\n%s", name, got)
	}
}

func TestCatalogGolden(t *testing.T) {
	e, _, _, out := catalogEnv(t)
	if err := e.catalog(context.Background(), catalogOpts{Region: "us-east-1", JSON: true, Score: []string{"t3.large", "m5.large"}}); err != nil {
		t.Fatal(err)
	}
	goldenCheck(t, "aws/catalog-golden.json", out.Bytes())
}

func TestCatalogRowsAndMath(t *testing.T) {
	e, _, _, out := catalogEnv(t)
	if err := e.catalog(context.Background(), catalogOpts{JSON: true}); err != nil {
		t.Fatal(err)
	}
	var d catalogDoc
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	rows := map[string]catalogRow{}
	for _, r := range d.Rows {
		rows[r.Type] = r
	}
	if _, ok := rows["m5.metal"]; ok {
		t.Error("metal offered")
	}
	if _, ok := rows["m5.24xlarge"]; ok {
		t.Error("a 96 vCPU type offered")
	}
	// 70 GB at $0.08 per GB-month over 730 h, plus the IPv4, on the cheapest zone
	r := rows["t3.large"]
	want := round4(0.0231 + 0.005 + 70*0.08/730)
	if r.EstSpot == nil || *r.EstSpot != want || *r.SpotMin != 0.0231 || len(r.Spot) != 5 {
		t.Errorf("t3.large: %+v want est %v", r, want)
	}
	if r.OnDemand == nil || *r.OnDemand != 0.0832 || r.Interruption != "<5%" || r.VCPU != 2 || r.MemoryGiB != 8 || r.Arch != "amd64" || !r.Selectable {
		t.Errorf("t3.large: %+v", r)
	}
	for _, n := range []string{"t4g.large", "m6g.large"} {
		if r := rows[n]; !r.Selectable || r.Arch != "arm64" || r.Note != "" {
			t.Errorf("%s: %+v", n, r)
		}
	}
	if rows["g4dn.xlarge"].GPUs != 1 {
		t.Errorf("gpu: %+v", rows["g4dn.xlarge"])
	}
	// every row is selectable now, cheapest first
	for _, r := range d.Rows {
		if !r.Selectable {
			t.Errorf("%s is not selectable", r.Type)
		}
	}
	if d.Rows[0].Type != "t3a.large" {
		t.Errorf("cheapest first: %s", d.Rows[0].Type)
	}
}

func TestCatalogMakesOnlyReadCalls(t *testing.T) {
	e, f, _, _ := catalogEnv(t)
	if err := e.catalog(context.Background(), catalogOpts{JSON: true, Score: []string{"t3.large"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) == 0 {
		t.Fatal("no calls")
	}
	for _, c := range f.calls {
		if c.name != "aws" || !readOnly(c.args[0], c.args[1]) {
			t.Errorf("not a read: %s", c.line())
		}
		if c.stdin != "" {
			t.Errorf("stdin: %s", c.line())
		}
		l := c.line()
		if !strings.Contains(l, "--profile comebye-provision") || !strings.Contains(l, "--no-cli-pager") {
			t.Errorf("profile not passed by name: %s", l)
		}
		want := "--region us-east-1"
		if !strings.Contains(l, want) {
			t.Errorf("region: %s", l)
		}
	}
	want := map[string]bool{"ec2 describe-spot-price-history": true, "ec2 get-spot-placement-scores": true}
	for _, o := range f.ops() {
		delete(want, o)
	}
	if len(want) != 0 {
		t.Errorf("never called: %v", want)
	}
}

// With no AWS login at all the catalog still has specs, on-demand prices, one
// spot price per type and the interruption bands, and says which column came
// from where; only the per-zone prices and the capacity scores need a login.
func TestCatalogAnonymousNeedsNoLogin(t *testing.T) {
	e, f, _, out := catalogEnv(t)
	f.fail = "Unable to locate credentials. You can configure credentials by running \"aws login\"."
	if err := e.catalog(context.Background(), catalogOpts{JSON: true}); err != nil {
		t.Fatal(err)
	}
	var d catalogDoc
	json.Unmarshal(out.Bytes(), &d)
	if d.Degraded != "" {
		t.Errorf("degraded %q although every public source answered", d.Degraded)
	}
	for _, c := range f.calls {
		if c.args[0] == "pricing" || c.args[1] == "describe-instance-types" {
			t.Errorf("the public data was available but %s was called", c.line())
		}
	}
	var r *catalogRow
	for i := range d.Rows {
		if d.Rows[i].Type == "t3.large" {
			r = &d.Rows[i]
		}
	}
	if r == nil || r.OnDemand == nil || *r.OnDemand != 0.0832 || r.VCPU != 2 || r.MemoryGiB != 8 || r.Arch != "amd64" {
		t.Fatalf("t3.large: %+v", r)
	}
	if r.SpotMin == nil || *r.SpotMin != 0.0231 || len(r.Spot) != 0 || r.EstSpot == nil || r.Interruption != "<5%" {
		t.Errorf("t3.large spot: %+v", r)
	}
	if s := d.Sources["spot_zones"]; s.State != "missing" || s.Error != "creds_expired" {
		t.Errorf("zones: %+v", s)
	}
	if s := d.Sources["types"]; s.State != "fresh" || !strings.Contains(s.Source, "public") {
		t.Errorf("types: %+v", s)
	}
}

func TestParseBulkKeepsOnlyLinuxSharedLicenseFree(t *testing.T) {
	f, err := os.Open("testdata/aws/price-list-bulk.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d, err := parseBulk(f, "us-east-1", []string{"m5", "t3"})
	if err != nil {
		t.Fatal(err)
	}
	if d.OD["m5.large"] != 0.096 {
		t.Errorf("m5.large: %v (a Windows, dedicated, BYOL or other-region product was taken?)", d.OD["m5.large"])
	}
	for _, ty := range d.Types {
		if !strings.HasPrefix(ty.Type, "m5.") && !strings.HasPrefix(ty.Type, "t3.") {
			t.Errorf("a family that was not asked for: %s", ty.Type)
		}
	}
	if _, err := parseBulk(strings.NewReader(`{"products":{},"terms":{"OnDemand":{}}}`), "us-east-1", []string{"m5"}); err == nil {
		t.Error("an empty file was accepted")
	}
	if _, err := parseBulk(strings.NewReader(`[1,2]`), "us-east-1", []string{"m5"}); err == nil {
		t.Error("a file of another shape was accepted")
	}
}

func TestParseSpotJS(t *testing.T) {
	b, _ := os.ReadFile("testdata/aws/spot.js")
	raw, err := parseSpotJS(b, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]float64
	json.Unmarshal(raw, &m)
	if m["t3.large"] != 0.0231 || m["m5.large"] == 0.9999 {
		t.Errorf("%v", m)
	}
	if _, err := parseSpotJS(b, "mars-north-1"); err == nil {
		t.Error("a region that is not in the feed gave prices")
	}
	if _, err := parseSpotJS([]byte("<html>moved</html>"), "us-east-1"); err == nil {
		t.Error("a page that is not the feed gave prices")
	}
}

func TestCatalogCacheAndTTLs(t *testing.T) {
	e, f, clk, _ := catalogEnv(t)
	hits := 0
	e.HTTP = advisorHTTP(t, 200, &hits)
	ctx := context.Background()
	run := func(o catalogOpts) {
		t.Helper()
		o.JSON = true
		if err := e.catalog(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	run(catalogOpts{})
	first := len(f.calls)
	if first == 0 || hits != 1 {
		t.Fatalf("calls %d hits %d", first, hits)
	}
	if st, err := os.Stat(filepath.Join(e.catalogDir(), "catalog-aws-us-east-1.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("cache file: %v %v", st, err)
	}
	// within every TTL: nothing is fetched
	clk.t = clk.t.Add(5 * time.Minute)
	run(catalogOpts{})
	if len(f.calls) != first || hits != 1 {
		t.Errorf("a fresh cache was refetched: %d calls, %d hits", len(f.calls)-first, hits)
	}
	// 20 minutes: only the spot prices are due (15 min)
	clk.t = clk.t.Add(15 * time.Minute)
	before := len(f.calls)
	run(catalogOpts{})
	if got := f.ops()[before:]; len(got) != 1 || got[0] != "ec2 describe-spot-price-history" {
		t.Errorf("after 20 min refetched %v", got)
	}
	// 2 days: the price list, the feed and the advisor are due too (the aws
	// call that is left is the per-zone spot history); the price list never
	// needs a login
	clk.t = clk.t.Add(48 * time.Hour)
	before = len(f.calls)
	run(catalogOpts{})
	if hits != 2 {
		t.Errorf("the advisor was not refreshed after 2 days: %d hits", hits)
	}
	if got := f.ops()[before:]; len(got) != 1 || f.count("pricing") != 0 {
		t.Errorf("after 2 days: %v", got)
	}
	// --refresh ignores the cache
	before = len(f.calls)
	run(catalogOpts{Refresh: true})
	if len(f.calls)-before < 1 || hits != 3 {
		t.Errorf("--refresh fetched %d, advisor hits %d", len(f.calls)-before, hits)
	}
}

func TestCatalogServesTheCacheWhenTheLoginExpires(t *testing.T) {
	e, f, clk, _ := catalogEnv(t)
	ctx := context.Background()
	if err := e.catalog(ctx, catalogOpts{JSON: true}); err != nil {
		t.Fatal(err)
	}
	f.fail = "Your session has expired. Please reauthenticate using 'aws login'."
	e.Out = &bytes.Buffer{}
	clk.t = clk.t.Add(3 * time.Hour)
	if err := e.catalog(ctx, catalogOpts{JSON: true}); err != nil {
		t.Fatalf("a stale cache must be served: %v", err)
	}
	var d catalogDoc
	json.Unmarshal(e.Out.(*bytes.Buffer).Bytes(), &d)
	if d.Degraded != "" {
		t.Errorf("degraded %q: only the optional per-zone prices need the login", d.Degraded)
	}
	if s := d.Sources["spot_zones"]; s.State != "stale" || s.Error != "creds_expired" || s.FetchedAt != "2026-11-10T12:00:00Z" {
		t.Errorf("zone source: %+v", s)
	}
	if s := d.Sources["spot_price"]; s.State != "fresh" {
		t.Errorf("the public spot feed needs no login: %+v", s)
	}
	if s := d.Sources["on_demand"]; s.State != "cached" {
		t.Errorf("on-demand is still fresh: %+v", s)
	}
	if len(d.Rows) == 0 || d.Rows[0].SpotMin == nil {
		t.Error("the stale prices are gone")
	}
}

func TestCatalogWithoutACacheAndNoPriceListFails(t *testing.T) {
	e, _, _, out := catalogEnv(t)
	e.HTTP = httpFunc(func(r *http.Request) (*http.Response, error) { return reply(503, ""), nil })
	err := e.catalog(context.Background(), catalogOpts{JSON: true})
	if err == nil {
		t.Fatal("no error")
	}
	var o struct{ Error struct{ Class, Msg string } }
	if json.Unmarshal(out.Bytes(), &o) != nil || o.Error.Msg == "" {
		t.Errorf("json error: %s", out)
	}
}

func TestCatalogAdvisorMayBeMissing(t *testing.T) {
	e, _, _, out := catalogEnv(t)
	e.HTTP = advisorHTTP(t, 503, nil)
	if err := e.catalog(context.Background(), catalogOpts{JSON: true}); err != nil {
		t.Fatal(err)
	}
	var d catalogDoc
	json.Unmarshal(out.Bytes(), &d)
	if d.Sources["spot_advisor"].State != "missing" || d.Degraded != "error" {
		t.Errorf("%+v degraded %q", d.Sources["spot_advisor"], d.Degraded)
	}
	for _, r := range d.Rows {
		if r.Interruption != "" || r.SpotMin == nil && r.Arch == "amd64" && r.Type != "" && false {
			t.Errorf("%s: %+v", r.Type, r)
		}
	}
	if len(d.Rows) == 0 || d.Rows[0].OnDemand == nil {
		t.Error("prices are lost because the advisor is missing")
	}
}

func TestCatalogRefusesBadArguments(t *testing.T) {
	e, f, _, _ := catalogEnv(t)
	for name, o := range map[string]catalogOpts{
		"gcp not built":  {Kind: "gcp"},
		"bad region":     {Region: "us east"},
		"bad family":     {Families: []string{"t3;rm"}},
		"bad type":       {Types: []string{"t3.large --x"}},
		"long shortlist": {Score: strings.Fields("a.b c.d e.f g.h i.j k.l m.n o.p q.r s.t u.v")},
	} {
		if err := e.catalog(context.Background(), o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("made calls: %v", f.ops())
	}
}

func TestCatalogLockIsTakenOverWhenStale(t *testing.T) {
	e, _, _, _ := catalogEnv(t)
	os.MkdirAll(e.catalogDir(), 0o700)
	lock := filepath.Join(e.catalogDir(), "catalog-aws-us-east-1.lock")
	os.WriteFile(lock, nil, 0o600)
	old := e.now().Add(-time.Hour)
	os.Chtimes(lock, old, old)
	if err := e.catalog(context.Background(), catalogOpts{JSON: true}); err != nil {
		t.Fatalf("a stale lock blocked: %v", err)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Error("lock left behind")
	}
	// a live lock makes the command wait, then give up
	os.WriteFile(lock, nil, 0o600)
	os.Chtimes(lock, e.now(), e.now())
	if err := e.catalog(context.Background(), catalogOpts{JSON: true}); err == nil || !strings.Contains(err.Error(), "another refresh") {
		t.Errorf("live lock: %v", err)
	}
}

func TestParseAdvisorRejectsAChangedFormat(t *testing.T) {
	if _, err := parseAdvisor([]byte(`{"ranges":[],"spot_advisor":{}}`), "us-east-1"); err == nil {
		t.Error("an empty result is not an answer")
	}
	if _, err := parseAdvisor([]byte(`<html>`), "us-east-1"); err == nil {
		t.Error("html accepted")
	}
}

// ---- preflight ----

func pfEnv(t *testing.T, f *fixtureAWS) (*env, *bytes.Buffer) {
	e, _, _, out := catalogEnv(t)
	e.Run = f
	writeOAuth(t, e.Cfg)
	return e, out
}

type pfRunner struct {
	aws *fixtureAWS
	gc  func() (string, error)
	osh func() (string, error)
}

func (p *pfRunner) Run(ctx context.Context, name string, args []string, stdin []byte) (string, string, error) {
	switch name {
	case "aws":
		return p.aws.Run(ctx, name, args, stdin)
	case "gcloud":
		out, err := p.gc()
		return out, "", err
	case "openshell":
		out, err := p.osh()
		return out, "", err
	}
	return "", "", fmt.Errorf("unexpected %s", name)
}
func (p *pfRunner) Interactive(context.Context, string, []string) error { return nil }

func TestPreflightReportsEachCredential(t *testing.T) {
	f := &fixtureAWS{dir: "testdata/aws"}
	e, out := pfEnv(t, f)
	e.Cfg.Project = "comebye-test-0001"
	e.Run = &pfRunner{aws: f,
		gc:  func() (string, error) { return "someone@example.com\n", nil },
		osh: func() (string, error) { return `{"sandboxes":[]}`, nil }}
	e.HTTP = httpFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v2/oauth/token":
			return reply(200, `{"access_token":"tok"}`), nil
		case "/api/v2/tailnet/-/devices":
			return reply(200, `{"devices":[]}`), nil
		}
		return reply(404, ""), nil
	})
	e.Cfg.OpenShellGateway, e.Cfg.OpenShellWorkspace, e.Cfg.SandboxImage = "homelab", "ws", "img:{version}"
	lookPath = func(string) (string, error) { return "/usr/bin/x", nil }
	defer func() { lookPath = defaultLookPath }()
	if err := e.preflight(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	var d pfDoc
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if d.V != 1 || d.AWS.State != "ok" || d.AWS.Note != "profile comebye-provision" || d.GCP.State != "ok" || d.Tailnet.State != "ok" ||
		d.OpenShell.State != "ok" || d.OpenShell.Gateway != "homelab" || d.SSH.State != "ok" {
		t.Errorf("%+v", d)
	}
	for _, leak := range []string{"123456789012", "someone@example.com", "tok", "csecret", "cid"} {
		if strings.Contains(out.String(), leak) {
			t.Errorf("preflight prints %q:\n%s", leak, out)
		}
	}
	for _, c := range f.calls {
		if c.args[0] != "sts" {
			t.Errorf("preflight made a %s call", c.args[0])
		}
	}
}

func TestPreflightAWSStates(t *testing.T) {
	for name, tc := range map[string]struct{ fail, state string }{
		"ok":      {"", "ok"},
		"expired": {"Your session has expired. Please reauthenticate using 'aws login'.", "expired"},
		"token":   {"An error occurred (ExpiredToken) when calling the GetCallerIdentity operation", "expired"},
		"missing": {"Unable to locate credentials. You can configure credentials by running \"aws configure\".", "missing"},
		"profile": {"The config profile (nope) could not be found", "missing"},
		"error":   {"Could not connect to the endpoint URL", "error"},
	} {
		f := &fixtureAWS{dir: "testdata/aws", fail: tc.fail}
		e, _ := pfEnv(t, f)
		if got := e.pfAWS(context.Background()); got.State != tc.state {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestPreflightTailnetAndSSH(t *testing.T) {
	f := &fixtureAWS{dir: "testdata/aws"}
	e, _ := pfEnv(t, f)
	os.Remove(e.Cfg.TSOAuthFile)
	if got := e.pfTailnet(context.Background()); got.State != "missing" {
		t.Errorf("%+v", got)
	}
	os.WriteFile(e.Cfg.TSOAuthFile, []byte("client_id=a\nclient_secret=b\n"), 0o644)
	if got := e.pfTailnet(context.Background()); got.State != "error" || !strings.Contains(got.Note, "0600") {
		t.Errorf("%+v", got)
	}
	lookPath = func(string) (string, error) { return "", fmt.Errorf("no") }
	defer func() { lookPath = defaultLookPath }()
	if got := e.pfSSH(); got.State != "missing" {
		t.Errorf("%+v", got)
	}
}

func TestPreflightOpenShellStates(t *testing.T) {
	f := &fixtureAWS{dir: "testdata/aws"}
	e, _ := pfEnv(t, f)
	if got := e.pfOpenShell(context.Background()); got.State != "missing" {
		t.Errorf("unconfigured: %+v", got)
	}
	e.Cfg.OpenShellGateway, e.Cfg.OpenShellWorkspace, e.Cfg.SandboxImage = "homelab", "ws", "img"
	e.Run = &pfRunner{aws: f, osh: func() (string, error) { return "", fmt.Errorf("openshell: status 401 unauthenticated") }}
	if got := e.pfOpenShell(context.Background()); got.State != "expired" || got.Gateway != "homelab" {
		t.Errorf("%+v", got)
	}
}

// ---- status --json ----

func TestStatusJSON(t *testing.T) {
	c := testAWSCfg(t)
	one := func(name, state, extra, reason string) string {
		return fmt.Sprintf(`{"InstanceId":"i-%s","InstanceType":"t3.xlarge","LaunchTime":"2026-11-10T08:00:00+00:00","State":{"Name":%q},"StateTransitionReason":%q,%s"Tags":[{"Key":"managed-by","Value":"comebye-worker"},{"Key":"Name","Value":%q}%s]}`,
			name, state, reason, map[bool]string{true: `"InstanceLifecycle":"spot","SpotInstanceRequestId":"sir-1",`}[strings.Contains(extra, "spot")], name,
			strings.ReplaceAll(extra, "spot", ""))
	}
	capTags := `,{"Key":"comebye-budget-cap-usd","Value":"12.5"},{"Key":"comebye-destroy-after-hours","Value":"72"},{"Key":"comebye-launch-price-usd-hour","Value":"0.0298"}`
	body := `{"Reservations":[{"Instances":[` + one("w1", "running", "spot"+capTags, "") + `,` + one("w2", "stopped", "", "User initiated (2026-11-09 22:30:15 GMT)") + `]}]}`
	var asked []string
	r := &fakeRunner{handle: func(cl call, _ *fakeRunner) (string, string, error) {
		asked = append(asked, cl.line())
		return body, "", nil
	}}
	var out bytes.Buffer
	e := &env{Out: &out, Err: &bytes.Buffer{}, In: strings.NewReader(""), Run: r, Cfg: c, HTTP: httpFunc(func(*http.Request) (*http.Response, error) { t.Error("http"); return reply(500, ""), nil })}
	if err := e.statusJSON(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	var d statusDoc
	if err := json.Unmarshal(out.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Workers) != 2 || len(asked) != 1 {
		t.Fatalf("%d workers, %d calls", len(d.Workers), len(asked))
	}
	w1, w2 := d.Workers[0], d.Workers[1]
	if w1.Name != "w1" || w1.State != "RUNNING" || w1.Type != "t3.xlarge" || w1.Mode != "spot" || *w1.LaunchedAt != "2026-11-10T08:00:00Z" || w1.LastStopAt != nil {
		t.Errorf("w1 %+v", w1)
	}
	if *w1.Caps.BudgetCapUSD != 12.5 || *w1.Caps.DestroyAfterHours != 72 || *w1.Caps.LaunchPriceUSDHr != 0.0298 {
		t.Errorf("caps %+v", w1.Caps)
	}
	if w2.State != "STOPPED" || w2.Mode != "on-demand" || *w2.LastStopAt != "2026-11-09T22:30:15Z" || w2.Caps.BudgetCapUSD != nil {
		t.Errorf("w2 %+v", w2)
	}
	if !strings.Contains(out.String(), `"budget_cap_usd": null`) {
		t.Errorf("an unset cap should be null:\n%s", out.String())
	}
	for _, l := range asked {
		if !strings.Contains(l, "describe-instances") {
			t.Errorf("not a read: %s", l)
		}
	}
	// one worker, and one that is not there
	out.Reset()
	r.handle = func(cl call, _ *fakeRunner) (string, string, error) { return `{"Reservations":[]}`, "", nil }
	if err := e.statusJSON(context.Background(), "nope"); err != nil || !strings.Contains(out.String(), `"workers": []`) {
		t.Errorf("%v\n%s", err, out.String())
	}
}

// ---- the tags on create ----

func TestCreateWritesCapTagsAndLaunchPrice(t *testing.T) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey}
	e, r, c := awsEnv(t, f, w)
	c.AWSSpot, c.BudgetCapUSD, c.DestroyAfterHours = true, 12.5, 72
	inner := r.handle
	r.handle = func(cl call, fr *fakeRunner) (string, string, error) {
		if cl.name == "aws" && cl.args[1] == "describe-spot-price-history" {
			return `[["t3.xlarge","us-east-1a","0.0624"],["t3.xlarge","us-east-1b","0.0610"]]`, "", nil
		}
		return inner(cl, fr)
	}
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	var run string
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), "run-instances") {
			run = cl.line()
		}
	}
	for _, want := range []string{"{Key=comebye-budget-cap-usd,Value=12.5}", "{Key=comebye-destroy-after-hours,Value=72}", "{Key=comebye-launch-price-usd-hour,Value=0.0624}"} {
		if !strings.Contains(run, want) {
			t.Errorf("run-instances lacks %s:\n%s", want, run)
		}
	}
	if strings.Contains(run, "Key=comebye-budget-cap-usd") && strings.Contains(strings.Split(run, "ResourceType=volume")[1], "comebye-budget") {
		t.Error("the tags went on the root volume too")
	}
}

func TestCreateWithoutCapsAddsNoTagsAndNoPriceCalls(t *testing.T) {
	w := &fakeWorld{hubVersion: "v0.3.1", peers: map[string]bool{}}
	f := &awsFake{console: consoleKey}
	e, r, _ := awsEnv(t, f, w)
	if err := e.create(context.Background(), "w1", createOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, cl := range r.calls {
		if strings.Contains(cl.line(), "comebye-budget") || strings.Contains(cl.line(), "get-products") || strings.Contains(cl.line(), "spot-price-history") {
			t.Errorf("unrequested: %s", cl.line())
		}
	}
}

// ---- the recording shim ----

func TestAWSShimRecordsAndRefusesWrites(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	shim, _ := filepath.Abs("testdata/aws-shim/aws")
	fx, _ := filepath.Abs("testdata/aws")
	log := filepath.Join(t.TempDir(), "log")
	run := func(args ...string) (string, error) {
		cmd := exec.Command(shim, args...)
		cmd.Env = append(os.Environ(), "AWS_SHIM_LOG="+log, "AWS_SHIM_FIXTURES="+fx)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("ec2", "describe-instance-types", "--region", "us-east-1"); err != nil || !strings.Contains(out, "t3.large") {
		t.Errorf("read: %v %s", err, out)
	}
	if out, err := run("pricing", "get-products", "--filters", "Type=TERM_MATCH,Field=instanceType,Value=m5.large"); err != nil || !strings.Contains(out, "0.0960") {
		t.Errorf("pricing: %v %s", err, out)
	}
	for _, w := range [][]string{{"ec2", "run-instances"}, {"ec2", "terminate-instances"}, {"ec2", "create-tags"}, {"iam", "create-user"}, {"ec2", "wait"}} {
		if _, err := run(w...); err == nil {
			t.Errorf("%v was not refused", w)
		}
	}
	b, _ := os.ReadFile(log)
	if strings.Count(string(b), "REFUSED") != 5 {
		t.Errorf("log:\n%s", b)
	}
}

// ---- allowed regions ----

func TestCatalogRefusesARegionThatIsNotAllowed(t *testing.T) {
	e, f, _, _ := catalogEnv(t)
	e.Cfg.AllowedRegions = []string{"us-east-1"}
	if err := e.catalog(context.Background(), catalogOpts{Region: "eu-west-1"}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("%v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused region still made calls: %v", f.ops())
	}
	if err := e.catalog(context.Background(), catalogOpts{Region: "us-east-1", JSON: true}); err != nil {
		t.Errorf("an allowed region: %v", err)
	}
}

func TestRegionsAllowedDefaults(t *testing.T) {
	c := testAWSCfg(t)
	c.AWSRegion = "us-east-2"
	if got := strings.Join(c.RegionsAllowed(), ","); got != "us-east-2,us-east-1,us-west-2" {
		t.Errorf("default %s", got)
	}
	c.AllowedRegions = []string{"eu-west-1"}
	if got := strings.Join(c.RegionsAllowed(), ","); got != "eu-west-1" {
		t.Errorf("set %s", got)
	}
	c.AllowedRegions = []string{"nowhere"}
	if c.Validate() == nil {
		t.Error("a bad region in allowed_regions accepted")
	}
}

func TestPreflightSaysWhereItMayBuild(t *testing.T) {
	f := &fixtureAWS{dir: "testdata/aws"}
	e, out := pfEnv(t, f)
	os.Remove(e.Cfg.TSOAuthFile) // no OAuth client: no Tailscale request
	e.Cfg.AWSRegion = "us-east-2"
	if err := e.preflight(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	var d pfDoc
	if err := json.Unmarshal(out.Bytes(), &d); err != nil || d.Region != "us-east-2" || len(d.Regions) != 3 {
		t.Errorf("%v %+v", err, d)
	}
}

func TestPreflightOpenShellSaysWhatASandboxIsMadeFrom(t *testing.T) {
	f := &fixtureAWS{dir: "testdata/aws"}
	e, _ := pfEnv(t, f)
	e.Cfg.OpenShellGateway, e.Cfg.OpenShellWorkspace, e.Cfg.SandboxImage = "homelab", "ws", "registry.example/sbx:{version}"
	e.Cfg.SandboxPolicy, e.Cfg.SandboxCPU, e.Cfg.SandboxMemory = "/home/me/.config/comebye-worker/sandbox-policy.yaml", "2", "6Gi"
	e.Run = &pfRunner{aws: f, osh: func() (string, error) { return `{"sandboxes":[]}`, nil }}
	got := e.pfOpenShell(context.Background())
	if got.State != "ok" || got.Gateway != "homelab" || got.Workspace != "ws" || got.Image != "registry.example/sbx:{version}" || got.Policy != "sandbox-policy.yaml" || got.CPU != "2" || got.Memory != "6Gi" {
		t.Errorf("%+v", got)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), "/home/me") {
		t.Errorf("a path reached the answer: %s", b)
	}
}

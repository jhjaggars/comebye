package provision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goldenCatalog = "../../tools/worker/testdata/aws/catalog-golden.json"

func TestParsePreflight(t *testing.T) {
	b, _ := os.ReadFile("testdata/preflight.json")
	p, err := ParsePreflight(b)
	if err != nil || p.AWS.State != StateOK || p.AWS.Note != "profile comebye-provision" || p.Region != "us-east-2" || len(p.Regions) != 3 {
		t.Fatalf("%+v %v", p, err)
	}
	if p.GCP.State != StateMissing || p.OpenShell.State != StateMissing {
		t.Errorf("%+v", p)
	}
}

func TestPreflightIsBoundedAndScrubbed(t *testing.T) {
	in := `{"v":1,"checked_at":"yesterday","extra":"x",
	 "aws":{"state":"ok","note":"profile p, account 641875867446, arn:aws:iam::641875867446:user/jhjaggars, jhjaggars@gmail.com, key AKIAABCDEFGHIJKLMNOP, tskey-auth-SECRET123, token=abc\nsecond"},
	 "gcp":{"state":"fine"},
	 "tailnet":{"state":"expired","note":"` + strings.Repeat("x", 500) + `"},
	 "openshell":{"state":"ok","gateway":"gw"},"ssh":{"state":"ok"},
	 "region":"us east","regions":["us-east-1","../etc","us-west-2"]}`
	p, err := ParsePreflight([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"641875867446", "jhjaggars", "AKIA", "tskey", "abc", "arn:", "\n"} {
		if strings.Contains(p.AWS.Note, bad) {
			t.Errorf("note keeps %q: %q", bad, p.AWS.Note)
		}
	}
	if p.GCP.State != StateError {
		t.Errorf("an unknown state is an error, got %q", p.GCP.State)
	}
	if len([]rune(p.Tailnet.Note)) > 201 {
		t.Errorf("note not bounded: %d", len(p.Tailnet.Note))
	}
	if p.CheckedAt != "" || p.Region != "" || len(p.Regions) != 2 {
		t.Errorf("%+v", p)
	}
	if _, err := ParsePreflight([]byte(`{"v":2}`)); err == nil {
		t.Error("another version accepted")
	}
	if _, err := ParsePreflight([]byte(`<html>`)); err == nil {
		t.Error("html accepted")
	}
}

func TestParseCatalogGolden(t *testing.T) {
	b, err := os.ReadFile(goldenCatalog)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseCatalog(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Region != "us-east-1" || len(c.Rows) != 10 || c.Placement == nil || len(c.Placement.Scores) != 5 {
		t.Fatalf("%s %d rows %+v", c.Region, len(c.Rows), c.Placement)
	}
	if c.Sources["spot_price"].State != "fresh" || c.Estimate.DiskGB != 70 {
		t.Errorf("%+v %+v", c.Sources, c.Estimate)
	}
	kept, hidden := c.HideSmall()
	if hidden != 0 || len(kept) != 10 { // the recorded fixture holds no tiny sizes
		t.Errorf("kept %d hidden %d", len(kept), hidden)
	}
}

func TestParseCatalogDropsWhatIsNotWellFormed(t *testing.T) {
	in := `{"v":1,"kind":"aws","region":"us-east-1","degraded":"weird","sources":{"types":{"state":"fresh","source":"x"},"evil":{"state":"fresh"}},
	 "rows":[
	  {"type":"t3.large","vcpu":2,"memory_gib":8,"arch":"amd64","on_demand_usd_hour":0.08,"spot_usd_hour":{"us-east-1a":0.03,"bad zone":1,"us-east-1b":-1},"selectable":true,"interruption":"<5%"},
	  {"type":"t3.nano; rm -rf","vcpu":2,"memory_gib":1},
	  {"type":"m5.large","vcpu":2,"memory_gib":8,"on_demand_usd_hour":-3},
	  {"type":"c5.large","vcpu":2,"memory_gib":4,"on_demand_usd_hour":1e9},
	  {"type":"t3.nano","vcpu":2,"memory_gib":0.5,"arch":"amd64","selectable":true},
	  {"type":"x1.large","vcpu":2,"memory_gib":4,"arch":"riscv","selectable":true}
	 ]}`
	c, err := ParseCatalog([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range c.Rows {
		names = append(names, r.Type)
	}
	if strings.Join(names, ",") != "t3.large,t3.nano,x1.large" {
		t.Errorf("rows %v", names)
	}
	if c.Degraded != "error" || len(c.Sources) != 1 {
		t.Errorf("degraded %q sources %v", c.Degraded, c.Sources)
	}
	if got := c.Rows[0].Spot; len(got) != 1 || got["us-east-1a"] != 0.03 {
		t.Errorf("a bad zone and a negative price must be dropped: %v", got)
	}
	if c.Rows[2].Selectable || c.Rows[2].Arch != "" {
		t.Errorf("an unknown arch must not be selectable: %+v", c.Rows[2])
	}
	kept, hidden := c.HideSmall()
	if hidden != 1 || len(kept) != 2 {
		t.Errorf("t3.nano should be hidden: kept %d hidden %d", len(kept), hidden)
	}
	for _, in := range []string{`{"v":1,"kind":"azure","region":"us-east-1"}`, `{"v":1,"kind":"aws","region":"nope"}`, `{"v":3}`, `nope`} {
		if _, err := ParseCatalog([]byte(in)); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
}

// fakeWorker writes a stand-in for comebye-worker: it logs its argv and its
// environment, and answers from the files next to it.
func fakeWorker(t *testing.T, body string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "comebye-worker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + dir + "/argv\nenv > " + dir + "/env\n" + body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func TestToolRunsPreflightWithAFilteredEnvironment(t *testing.T) {
	pf, _ := filepath.Abs("testdata/preflight.json")
	bin, dir := fakeWorker(t, "cat "+pf+"\n")
	tool := &Tool{Bin: bin, Config: "/etc/worker.conf", Environ: func() []string {
		return []string{"PATH=/usr/bin", "HOME=/home/x", "AWS_PROFILE=comebye-provision", "COMEBYE_AGENT_TOKEN=SECRET", "COMEBYE_HUB=https://hub", "TS_AUTHKEY=tskey-x", "ANTHROPIC_API_KEY=sk-x"}
	}}
	p, err := tool.Preflight(context.Background())
	if err != nil || p.AWS.State != StateOK {
		t.Fatalf("%+v %v", p, err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if strings.TrimSpace(string(argv)) != "-config /etc/worker.conf -no-prompt preflight --json" {
		t.Errorf("argv %q", argv)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	for _, bad := range []string{"SECRET", "COMEBYE_", "TS_AUTHKEY", "ANTHROPIC", "tskey", "sk-x"} {
		if strings.Contains(string(env), bad) {
			t.Errorf("the tool's environment holds %q:\n%s", bad, env)
		}
	}
	for _, want := range []string{"AWS_PROFILE=comebye-provision", "HOME=/home/x"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("the tool's environment lacks %q:\n%s", want, env)
		}
	}
}

func TestToolCatalogArgvIsBuiltFromValidatedValues(t *testing.T) {
	g, _ := filepath.Abs(goldenCatalog)
	bin, dir := fakeWorker(t, "cat "+g+"\n")
	tool := &Tool{Bin: bin, Config: "/c"}
	if _, err := tool.Catalog(context.Background(), CatalogReq{Kind: "aws", Region: "us-east-1", Score: []string{"t3.large", "m5.large"}, Refresh: true}); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if strings.TrimSpace(string(argv)) != "-config /c -no-prompt catalog --kind aws --region us-east-1 --json --score t3.large,m5.large --refresh" {
		t.Errorf("argv %q", argv)
	}
	for name, r := range map[string]CatalogReq{
		"azure":      {Kind: "azure", Region: "us-east-1"},
		"bad region": {Kind: "aws", Region: "us-east-1; rm"},
		"bad type":   {Kind: "aws", Region: "us-east-1", Score: []string{"t3.large --x"}},
		"long score": {Kind: "aws", Region: "us-east-1", Score: strings.Fields("a.b c.d e.f g.h i.j k.l m.n o.p q.r s.t u.v")},
	} {
		if _, err := tool.Catalog(context.Background(), r); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestToolReportsTheToolsOwnError(t *testing.T) {
	bin, _ := fakeWorker(t, `echo '{"v":1,"error":{"class":"creds_expired","msg":"catalog: the instance types could not be read (creds_expired)"}}'
echo "worker: catalog: x" >&2
exit 1
`)
	_, err := (&Tool{Bin: bin, Config: "/c"}).Catalog(context.Background(), CatalogReq{Kind: "aws", Region: "us-east-1"})
	ce, ok := err.(*CatalogError)
	if !ok || ce.Class != "creds_expired" {
		t.Fatalf("%T %v", err, err)
	}
}

func TestToolKillsAHungRunAndItsChildren(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-alive")
	bin, _ := fakeWorker(t, "(sleep 30; touch "+marker+") &\nsleep 30\n")
	tool := &Tool{Bin: bin, Config: "/c", PreflightWait: 300 * time.Millisecond}
	start := time.Now()
	if _, err := tool.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestToolWithoutABinaryFailsCleanly(t *testing.T) {
	_, err := (&Tool{Bin: "/nonexistent/comebye-worker", Config: "/c"}).Preflight(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not start") {
		t.Errorf("%v", err)
	}
}

func TestToolOutputIsCapped(t *testing.T) {
	bin, _ := fakeWorker(t, "head -c 20000000 /dev/zero | tr '\\0' 'a'\n")
	if _, err := (&Tool{Bin: bin, Config: "/c"}).Preflight(context.Background()); err == nil {
		t.Error("a 20 MB answer was accepted")
	}
}

func TestToolSerialisesRunsOfOneKind(t *testing.T) {
	pf, _ := filepath.Abs("testdata/preflight.json")
	log := filepath.Join(t.TempDir(), "log")
	bin, _ := fakeWorker(t, "echo start >> "+log+"\nsleep 0.3\ncat "+pf+"\necho end >> "+log+"\n")
	tool := &Tool{Bin: bin, Config: "/c"}
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := tool.Preflight(context.Background()); done <- err }()
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(log)
	if strings.Fields(string(b))[1] != "end" {
		t.Errorf("two runs overlapped: %v", strings.Fields(string(b)))
	}
}

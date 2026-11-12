package provision

import (
	"os"
	"strings"
	"testing"
)

func TestParseSpecIsStrict(t *testing.T) {
	good := `{"kind":"aws","name":"demo1","type":"t3.large","spot":true,"boot_disk_gb":20,"data_disk_gb":50,"idle_minutes":120,"budget_cap_usd":10,"destroy_after_hours":72}`
	if _, err := ParseSpec([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"unknown field": strings.Replace(good, `"spot":true`, `"spot":true,"shell":"x"`, 1),
		"two objects":   good + good,
		"trailing":      good + ` x`,
		"not json":      `kind=aws`,
		"gcp":           strings.Replace(good, `"aws"`, `"gcp"`, 1),
		"upper name":    strings.Replace(good, "demo1", "Demo1", 1),
		"one letter":    strings.Replace(good, "demo1", "d", 1),
		"dash end":      strings.Replace(good, "demo1", "demo-", 1),
		"quote":         strings.Replace(good, "demo1", `a"b`, 1),
		"type flag":     strings.Replace(good, "t3.large", "t3.large --x", 1),
		"zone":          strings.Replace(good, `"spot":true`, `"spot":true,"zone":"nope"`, 1),
		"cap 0":         strings.Replace(good, `"budget_cap_usd":10`, `"budget_cap_usd":0`, 1),
		"cap huge":      strings.Replace(good, `"budget_cap_usd":10`, `"budget_cap_usd":101`, 1),
		"idle 1":        strings.Replace(good, `"idle_minutes":120`, `"idle_minutes":1`, 1),
		"destroy 0":     strings.Replace(good, `"destroy_after_hours":72`, `"destroy_after_hours":0`, 1),
		"max_run":       strings.Replace(good, `"spot":true`, `"spot":true,"max_run":"1 day"`, 1),
		"max_run huge":  strings.Replace(good, `"spot":true`, `"spot":true,"max_run":"999h"`, 1),
	} {
		if _, err := ParseSpec([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the g4dn GPU family is Intel and the g5g one is Arm: both are accepted now
	if _, err := ParseSpec([]byte(strings.Replace(good, "t3.large", "g4dn.xlarge", 1))); err != nil {
		t.Errorf("g4dn refused: %v", err)
	}
	var se *SpecError
	_, err := ParseSpec([]byte(strings.Replace(good, "demo1", "Demo", 1)))
	if e, ok := err.(*SpecError); !ok || e.Field != "name" {
		t.Errorf("error: %#v %v", err, se)
	}
}

func TestEstimateCost(t *testing.T) {
	b, err := os.ReadFile("../../tools/worker/testdata/aws/catalog-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	cat, err := ParseCatalog(b)
	if err != nil {
		t.Fatal(err)
	}
	sp := DefaultSpec("demo1", "t3.large")
	c, err := EstimateCost(sp, cat)
	if err != nil || c.Mode != "spot" {
		t.Fatalf("%+v %v", c, err)
	}
	// the cheapest zone 0.0231 + 0.005 IPv4 + 70 GB at 0.08 per GB-month over 730 h
	want := 0.0231 + 0.005 + 70*0.08/730
	if d := c.USDHour - want; d < -0.0001 || d > 0.0001 {
		t.Errorf("hour %v want %v", c.USDHour, want)
	}
	if c.USDDay < c.USDHour*23.9 || c.BudgetHours <= 0 || c.OnDemandHour <= c.USDHour {
		t.Errorf("%+v", c)
	}
	sp.Spot = false
	od, _ := EstimateCost(sp, cat)
	if od.Mode != "on_demand" || od.USDHour <= c.USDHour {
		t.Errorf("on-demand %+v vs spot %+v", od, c)
	}
	sp.Type = "p9.nothing"
	if _, err := EstimateCost(sp, cat); err == nil {
		t.Error("an unlisted type was priced")
	}
}

func TestParseEventKeepsOnlyKnownThings(t *testing.T) {
	it, err := ParseEvent([]byte(`{"v":1,"seq":4,"stage":"instance","step":"4/9","state":"failed","class":"spot_capacity","zone":"us-east-1b","msg":"no capacity tskey-abc123def456","extra":"x"}`))
	if err != nil || it.Class != ClassSpotCapacity || it.Zone != "us-east-1b" || strings.Contains(it.Msg, "tskey-abc123") {
		t.Fatalf("%+v %v", it, err)
	}
	it, _ = ParseEvent([]byte(`{"v":1,"stage":"instance","state":"failed","class":"made-up","zone":"; rm -rf"}`))
	if it.Class != ClassOther || it.Zone != "" {
		t.Errorf("%+v", it)
	}
	for _, bad := range []string{`{"v":2,"stage":"instance","state":"ok"}`, `{"v":1,"stage":"rm","state":"ok"}`, `{"v":1,"stage":"instance","state":"maybe"}`, `nope`} {
		if _, err := ParseEvent([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestSandboxSpec(t *testing.T) {
	good := `{"kind":"openshell","name":"box1","idle_minutes":120,"destroy_after_hours":72}`
	s, err := ParseSpec([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(s.JSON()); got != `{"destroy_after_hours":72,"idle_minutes":120,"kind":"openshell","name":"box1"}` {
		t.Errorf("a sandbox spec carries only what a sandbox may have: %s", got)
	}
	for name, in := range map[string]string{
		"type":         strings.Replace(good, `"name"`, `"type":"t3.large","name"`, 1),
		"spot":         strings.Replace(good, `"name"`, `"spot":true,"name"`, 1),
		"disk":         strings.Replace(good, `"name"`, `"data_disk_gb":50,"name"`, 1),
		"zone":         strings.Replace(good, `"name"`, `"zone":"us-east-1a","name"`, 1),
		"cap":          strings.Replace(good, `"name"`, `"budget_cap_usd":10,"name"`, 1),
		"max_run":      strings.Replace(good, `"name"`, `"max_run":"24h","name"`, 1),
		"bad name":     strings.Replace(good, "box1", "Box 1", 1),
		"idle 1":       strings.Replace(good, `"idle_minutes":120`, `"idle_minutes":1`, 1),
		"destroy 0":    strings.Replace(good, `"destroy_after_hours":72`, `"destroy_after_hours":0`, 1),
		"unknown kind": strings.Replace(good, "openshell", "podman", 1),
	} {
		if _, err := ParseSpec([]byte(in)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	d := DefaultSandboxSpec("box1")
	if err := d.Validate(); err != nil {
		t.Errorf("default: %v", err)
	}
	c, err := EstimateCost(d, nil)
	if err != nil || c.Mode != "sandbox" || c.USDHour != 0 || c.Basis == "" {
		t.Errorf("cost: %+v %v", c, err)
	}
}

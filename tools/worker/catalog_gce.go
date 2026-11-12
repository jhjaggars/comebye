package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The price list of kind gcp: the machine types of the config's zone from
// gcloud (read-only: `gcloud compute machine-types list`), priced from a
// built-in table of Google's list prices. The table is NOT live: Google's
// price list needs the Cloud Billing API and a billing account's login, which
// the tool does not ask for. Every figure is therefore an estimate, and the
// source says so; a Spot VM is priced at a typical share of the on-demand
// price, which Google changes without notice.

var gceRegionRe = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)

// gceTypeRe is a machine type name like e2-standard-4 or e2-micro.
var gceTypeRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)+$`)

// gcePriceTable is the list price per hour in us-central1, by family: per vCPU
// and per GiB. Shared-core types have one price each.
const gcePriceTableDate = "2025"

type gceRates struct{ perVCPU, perGiB, spotShare float64 }

var gceFamilyRates = map[string]gceRates{
	"e2":  {0.021811, 0.002923, 0.4},
	"n2":  {0.031611, 0.004237, 0.35},
	"n2d": {0.027502, 0.003686, 0.35},
	"t2d": {0.027, 0.0035, 0.4},
	"c3":  {0.03465, 0.003875, 0.4},
}

var gceSharedCore = map[string]float64{"e2-micro": 0.00838, "e2-small": 0.01675, "e2-medium": 0.03351}

// gceSpotShare is what a Spot VM typically costs of the on-demand price.
const gceSpotShareOf = "about 40% (35% for n2) of the on-demand price"

// gceDiskUSDGBMonth is pd-balanced in us-central1.
const gceDiskUSDGBMonth = 0.10

var defaultGCEFamilies = []string{"e2", "n2", "n2d", "t2d"}

type gceMachineType struct {
	Name        string  `json:"name"`
	GuestCPUs   int     `json:"guestCpus"`
	MemoryMb    float64 `json:"memoryMb"`
	IsSharedCPU bool    `json:"isSharedCpu"`
}

func gceFamily(typ string) string {
	if i := strings.IndexByte(typ, '-'); i > 0 {
		return typ[:i]
	}
	return typ
}

// gceOnDemand prices a machine type from the table; ok is false for a family
// the table does not know.
func gceOnDemand(t gceMachineType) (price, spotShare float64, ok bool) {
	fam := gceFamily(t.Name)
	rates, known := gceFamilyRates[fam]
	if p, shared := gceSharedCore[t.Name]; shared {
		return p, gceFamilyRates["e2"].spotShare, true
	}
	if !known {
		return 0, 0, false
	}
	return rates.perVCPU*float64(t.GuestCPUs) + rates.perGiB*t.MemoryMb/1024, rates.spotShare, true
}

func (e *env) buildGCECatalog(ctx context.Context, o catalogOpts) (*catalogDoc, error) {
	c := e.Cfg
	if c.Project == "" || c.Zone == "" {
		return nil, fmt.Errorf("catalog: kind gcp needs project and zone in the config")
	}
	region := o.Region
	if region == "" {
		region = regionOf(c.Zone)
	}
	switch {
	case !gceRegionRe.MatchString(region):
		return nil, fmt.Errorf("catalog: region %q is not a region like us-central1", region)
	case region != regionOf(c.Zone):
		return nil, fmt.Errorf("catalog: this node builds in %s (the config's zone %s), not %s", regionOf(c.Zone), c.Zone, region)
	}
	fams := o.Families
	if len(fams) == 0 {
		fams = defaultGCEFamilies
	}
	for _, f := range fams {
		if !familyRe.MatchString(f) {
			return nil, fmt.Errorf("catalog: bad machine family %q", f)
		}
	}
	for _, t := range o.Types {
		if !gceTypeRe.MatchString(t) {
			return nil, fmt.Errorf("catalog: bad machine type %q", t)
		}
	}
	if len(o.Score) > 0 {
		return nil, fmt.Errorf("catalog: --score is for aws; Google reports no spot placement score")
	}
	g := &gcloud{r: e.Run, cfg: c}
	out, err := g.run(ctx, g.argv("compute", "machine-types", "list", "--zones="+c.Zone, "--format=json"))
	now := e.now().UTC()
	if err != nil {
		return nil, fmt.Errorf("catalog: the machine types could not be read: %w", err)
	}
	var types []gceMachineType
	if err := json.Unmarshal([]byte(out), &types); err != nil {
		return nil, fmt.Errorf("catalog: gcloud's machine-types list is not JSON: %v", err)
	}
	famOK := map[string]bool{}
	for _, f := range fams {
		famOK[f] = true
	}
	want := map[string]bool{}
	for _, t := range o.Types {
		want[t] = true
	}
	stamp := now.Format(time.RFC3339)
	doc := &catalogDoc{V: 1, Kind: "gcp", Region: region, GeneratedAt: stamp,
		Sources: map[string]sourceInfo{
			"types":      {State: "fresh", FetchedAt: stamp, Source: "gcloud compute machine-types list (" + c.Zone + ")"},
			"on_demand":  {State: "cached", FetchedAt: stamp, Source: "built-in list prices for us-central1 (" + gcePriceTableDate + "), not live: an estimate"},
			"spot_price": {State: "cached", FetchedAt: stamp, Source: "a Spot VM is priced at " + gceSpotShareOf + ": an estimate, Google changes it without notice"},
		},
		Estimate: estimateTerms{IPv4USDHour: ipv4USDHour, DiskUSDGBMonth: gceDiskUSDGBMonth, DiskGB: c.BootDiskGB + c.DataDiskGB}}
	diskMonth := float64(doc.Estimate.DiskGB) * gceDiskUSDGBMonth / hoursPerMonth
	for _, t := range types {
		if !gceTypeRe.MatchString(t.Name) || t.GuestCPUs <= 0 || t.GuestCPUs > catalogMaxVCPU || t.MemoryMb <= 0 {
			continue
		}
		if len(want) > 0 && !want[t.Name] {
			continue
		}
		if len(want) == 0 && !famOK[gceFamily(t.Name)] {
			continue
		}
		r := catalogRow{Type: t.Name, VCPU: t.GuestCPUs, MemoryGiB: round4(t.MemoryMb / 1024), Arch: "amd64", Selectable: true}
		if od, share, ok := gceOnDemand(t); ok {
			r.OnDemand = ptr(round4(od))
			r.EstOnDemand = ptr(round4(od + ipv4USDHour + diskMonth))
			spot := od * share
			r.SpotMin = ptr(round4(spot))
			r.EstSpot = ptr(round4(spot + ipv4USDHour + diskMonth))
		} else {
			r.Note = "no price in the built-in table"
		}
		doc.Rows = append(doc.Rows, r)
	}
	sort.Slice(doc.Rows, func(i, j int) bool {
		a, b := doc.Rows[i], doc.Rows[j]
		if (a.EstSpot == nil) != (b.EstSpot == nil) {
			return a.EstSpot != nil
		}
		if a.EstSpot != nil && *a.EstSpot != *b.EstSpot {
			return math.Round(*a.EstSpot*1e6) < math.Round(*b.EstSpot*1e6)
		}
		return a.Type < b.Type
	})
	return doc, nil
}

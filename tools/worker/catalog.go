package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// `worker catalog` is read-only. For aws it runs only the calls below, for one
// region, and caches each source on disk in the state dir:
//
//	instance types   ec2 describe-instance-types            24 h
//	spot price       ec2 describe-spot-price-history        15 min
//	on-demand price  pricing get-products (us-east-1)        7 d
//	typical stop     the public Spot Advisor data file       24 h  (may be missing)
//	capacity score   ec2 get-spot-placement-scores           30 min, only with --score
//
// A refresh that fails (login expired, no network) serves the cache and says
// so in "degraded"; prices are never invented.

const (
	ttlTypes    = 24 * time.Hour
	ttlSpot     = 15 * time.Minute
	ttlOnDemand = 7 * 24 * time.Hour
	ttlAdvisor  = 24 * time.Hour
	ttlScore    = 30 * time.Minute

	advisorURL = "https://spot-bid-advisor.s3.amazonaws.com/spot-advisor-data.json"
	// The estimate: price + the public IPv4 + the disks.
	ipv4USDHour    = 0.005
	diskUSDGBMonth = 0.08
	hoursPerMonth  = 730

	catalogMaxVCPU = 32
)

// defaultFamilies are priced when neither --families nor allowed_families says
// otherwise. The Arm ones are listed so the table can show them as not
// selectable; the bootstrap is tested on amd64 only.
var defaultFamilies = []string{"t3", "t3a", "t4g", "m5", "m6i", "m6g", "c5", "c6i", "r5"}

var familyRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

type catalogOpts struct {
	Kind, Region           string
	Families, Types, Score []string
	Refresh                bool
	JSON                   bool
}

type catalogRow struct {
	Type         string             `json:"type"`
	VCPU         int                `json:"vcpu"`
	MemoryGiB    float64            `json:"memory_gib"`
	Arch         string             `json:"arch"` // amd64 or arm64
	GPUs         int                `json:"gpus"`
	OnDemand     *float64           `json:"on_demand_usd_hour"`
	Spot         map[string]float64 `json:"spot_usd_hour,omitempty"` // by zone
	SpotMin      *float64           `json:"spot_min_usd_hour"`
	Interruption string             `json:"interruption,omitempty"` // "<5%": AWS Spot Advisor, a monthly band
	EstSpot      *float64           `json:"estimate_spot_usd_hour"`
	EstOnDemand  *float64           `json:"estimate_on_demand_usd_hour"`
	Selectable   bool               `json:"selectable"`
	Note         string             `json:"note,omitempty"`
}

type sourceInfo struct {
	FetchedAt string `json:"fetched_at,omitempty"`
	State     string `json:"state"` // fresh, cached, stale or missing
	Error     string `json:"error,omitempty"`
	Source    string `json:"source"`
}

type placement struct {
	Types     []string    `json:"types"`
	Scores    []zoneScore `json:"scores"`
	FetchedAt string      `json:"fetched_at"`
	Source    string      `json:"source"`
}

type zoneScore struct {
	ZoneID string `json:"zone_id"`
	Score  int    `json:"score"`
}

type catalogDoc struct {
	V           int                   `json:"v"`
	Kind        string                `json:"kind"`
	Region      string                `json:"region"`
	GeneratedAt string                `json:"generated_at"`
	Degraded    string                `json:"degraded,omitempty"` // creds_expired, error
	Sources     map[string]sourceInfo `json:"sources"`
	Estimate    estimateTerms         `json:"estimate"`
	Rows        []catalogRow          `json:"rows"`
	Placement   *placement            `json:"placement,omitempty"`
}

type estimateTerms struct {
	IPv4USDHour    float64 `json:"ipv4_usd_hour"`
	DiskUSDGBMonth float64 `json:"disk_usd_gb_month"`
	DiskGB         int     `json:"disk_gb"`
}

// ---- the cache file ----

type cacheEntry struct {
	FetchedAt time.Time       `json:"fetched_at"`
	Data      json.RawMessage `json:"data"`
}

type catalogCache struct {
	path    string
	Sources map[string]cacheEntry `json:"sources"`
}

func (e *env) catalogDir() string { return filepath.Join(e.Cfg.StateDir, "provision") }

func loadCatalogCache(path string) *catalogCache {
	c := &catalogCache{path: path, Sources: map[string]cacheEntry{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, c)
		if c.Sources == nil {
			c.Sources = map[string]cacheEntry{}
		}
	}
	return c
}

func (c *catalogCache) save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(c)
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".catalog-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	os.Chmod(tmp.Name(), 0o600)
	return os.Rename(tmp.Name(), c.path)
}

// ---- the command ----

func (e *env) catalog(ctx context.Context, o catalogOpts) error {
	doc, err := e.buildCatalog(ctx, o)
	if err != nil {
		if o.JSON {
			b, _ := json.Marshal(map[string]any{"v": 1, "error": map[string]string{"class": classify(err, "catalog"), "msg": e.sanitize(err.Error())}})
			e.printf("%s\n", b)
		}
		return err
	}
	if o.JSON {
		b := marshalJSON(doc)
		e.printf("%s\n", b)
		return nil
	}
	e.printCatalog(doc)
	return nil
}

func (e *env) printCatalog(d *catalogDoc) {
	e.printf("%s %s, prices in USD per hour (estimate adds the IPv4 and %d GB of disk)\n", d.Kind, d.Region, d.Estimate.DiskGB)
	if d.Degraded != "" {
		e.printf("DEGRADED (%s): some figures are from the cache\n", d.Degraded)
	}
	e.printf("%-14s %4s %7s %-6s %9s %9s %-8s %9s  %s\n", "type", "vCPU", "GiB", "arch", "on-demand", "spot min", "stops", "est.spot", "")
	for _, r := range d.Rows {
		e.printf("%-14s %4d %7.1f %-6s %9s %9s %-8s %9s  %s\n", r.Type, r.VCPU, r.MemoryGiB, r.Arch, money(r.OnDemand), money(r.SpotMin), orDash(r.Interruption), money(r.EstSpot), r.Note)
	}
	for _, k := range []string{"types", "spot_price", "on_demand", "spot_advisor", "placement"} {
		if s, ok := d.Sources[k]; ok {
			e.printf("source %-12s %-7s %s %s\n", k, s.State, s.FetchedAt, s.Source)
		}
	}
}

func money(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.4f", *p)
}

func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }

func (e *env) buildCatalog(ctx context.Context, o catalogOpts) (*catalogDoc, error) {
	kind := o.Kind
	if kind == "" {
		kind = e.Cfg.Provider
	}
	if kind != "aws" {
		return nil, fmt.Errorf("catalog: kind %q is not built yet (aws only for now)", kind)
	}
	region := o.Region
	if region == "" {
		region = e.Cfg.AWSRegion
	}
	if !regionRe.MatchString(region) {
		return nil, fmt.Errorf("catalog: region %q is not a region like us-east-1", region)
	}
	fams := o.Families
	if len(fams) == 0 {
		fams = e.Cfg.AllowedFamilies
	}
	if len(fams) == 0 {
		fams = defaultFamilies
	}
	for _, f := range fams {
		if !familyRe.MatchString(f) {
			return nil, fmt.Errorf("catalog: bad instance family %q", f)
		}
	}
	for _, t := range append(append([]string{}, o.Types...), o.Score...) {
		if !awsTypeRe.MatchString(t) {
			return nil, fmt.Errorf("catalog: bad instance type %q", t)
		}
	}
	if len(o.Score) > 10 {
		return nil, fmt.Errorf("catalog: --score takes a shortlist of at most 10 types")
	}
	// A copy of the config pointed at the requested region: every call goes
	// through the same awsCLI, so the profile is passed by name and nothing else.
	cfg := *e.Cfg
	cfg.AWSRegion = region
	cli := &awsCLI{r: e.Run, cfg: &cfg}

	unlock, err := e.lockCatalog(region)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cache := loadCatalogCache(filepath.Join(e.catalogDir(), "catalog-aws-"+region+".json"))
	now := e.now().UTC()
	doc := &catalogDoc{V: 1, Kind: kind, Region: region, GeneratedAt: now.Format(time.RFC3339), Sources: map[string]sourceInfo{},
		Estimate: estimateTerms{IPv4USDHour: ipv4USDHour, DiskUSDGBMonth: diskUSDGBMonth, DiskGB: e.Cfg.BootDiskGB + e.Cfg.DataDiskGB}}

	// get serves a source from the cache while fresh, else refreshes it, else
	// serves the stale copy. The key includes what the data depends on.
	get := func(name, key, srcLabel string, ttl time.Duration, fetch func() (json.RawMessage, error)) (json.RawMessage, bool) {
		ent, have := cache.Sources[key]
		fresh := have && now.Sub(ent.FetchedAt) < ttl && !o.Refresh
		info := sourceInfo{Source: srcLabel}
		if fresh {
			info.State, info.FetchedAt = "cached", ent.FetchedAt.UTC().Format(time.RFC3339)
			doc.Sources[name] = info
			return ent.Data, true
		}
		data, err := fetch()
		if err == nil {
			cache.Sources[key] = cacheEntry{FetchedAt: now, Data: data}
			info.State, info.FetchedAt = "fresh", now.Format(time.RFC3339)
			doc.Sources[name] = info
			return data, true
		}
		class := classify(err, "catalog")
		if class == classOther {
			class = "error"
		}
		info.Error = class
		if doc.Degraded == "" || class == "creds_expired" {
			doc.Degraded = class
		}
		if have {
			info.State, info.FetchedAt = "stale", ent.FetchedAt.UTC().Format(time.RFC3339)
			doc.Sources[name] = info
			return ent.Data, true
		}
		info.State = "missing"
		doc.Sources[name] = info
		return nil, false
	}

	famKey := strings.Join(sortedCopy(fams), ",")
	typesRaw, ok := get("types", "types:"+famKey, "ec2 describe-instance-types", ttlTypes, func() (json.RawMessage, error) {
		return fetchInstanceTypes(ctx, cli, fams)
	})
	if !ok {
		cl := doc.Sources["types"].Error
		if cl == "error" {
			cl = classOther
		}
		return nil, &classedError{class: cl, msg: fmt.Sprintf("catalog: the instance types could not be read (%s)", doc.Sources["types"].Error)}
	}
	var types []typeInfo
	if err := json.Unmarshal(typesRaw, &types); err != nil {
		return nil, fmt.Errorf("catalog: cached instance types are unreadable: %v", err)
	}
	// Prices are fetched, and cached, for every type the families give, so a
	// narrowed view (--types) reuses them.
	var priceTypes []string
	for _, t := range filterTypes(types, nil) {
		priceTypes = append(priceTypes, t.Type)
	}
	types = filterTypes(types, o.Types)

	spot := map[string]map[string]float64{}
	if raw, ok := get("spot_price", "spot:"+famKey, "ec2 describe-spot-price-history", ttlSpot, func() (json.RawMessage, error) {
		return fetchSpotPrices(ctx, cli, e.now(), priceTypes)
	}); ok {
		json.Unmarshal(raw, &spot)
	}
	od := map[string]float64{}
	if raw, ok := get("on_demand", "ondemand:"+famKey, "pricing get-products", ttlOnDemand, func() (json.RawMessage, error) {
		return fetchOnDemand(ctx, cli, region, priceTypes)
	}); ok {
		json.Unmarshal(raw, &od)
	}
	adv := map[string]string{}
	if raw, ok := get("spot_advisor", "advisor:"+region, "AWS Spot Advisor (public data file)", ttlAdvisor, func() (json.RawMessage, error) {
		return e.fetchAdvisor(ctx, region)
	}); ok {
		json.Unmarshal(raw, &adv)
	}
	if len(o.Score) > 0 {
		skey := "score:" + strings.Join(sortedCopy(o.Score), ",")
		if raw, ok := get("placement", skey, "ec2 get-spot-placement-scores", ttlScore, func() (json.RawMessage, error) {
			return fetchPlacement(ctx, cli, region, o.Score)
		}); ok {
			var sc []zoneScore
			if json.Unmarshal(raw, &sc) == nil {
				doc.Placement = &placement{Types: sortedCopy(o.Score), Scores: sc, FetchedAt: doc.Sources["placement"].FetchedAt, Source: "ec2 get-spot-placement-scores"}
			}
		}
	}
	if err := cache.save(); err != nil {
		return nil, fmt.Errorf("catalog: saving the cache: %w", err)
	}

	diskMonth := float64(doc.Estimate.DiskGB) * diskUSDGBMonth / hoursPerMonth
	for _, t := range types {
		r := catalogRow{Type: t.Type, VCPU: t.VCPU, MemoryGiB: round4(t.MiB / 1024), Arch: t.Arch, GPUs: t.GPUs, Interruption: adv[t.Type], Selectable: true}
		if p, ok := od[t.Type]; ok {
			r.OnDemand = ptr(p)
			r.EstOnDemand = ptr(round4(p + ipv4USDHour + diskMonth))
		}
		if zs := spot[t.Type]; len(zs) > 0 {
			r.Spot = zs
			min := math.MaxFloat64
			for _, p := range zs {
				min = math.Min(min, p)
			}
			r.SpotMin = ptr(min)
			r.EstSpot = ptr(round4(min + ipv4USDHour + diskMonth))
		}
		if gravitonRe.MatchString(t.Type) || t.Arch != "amd64" {
			r.Selectable, r.Note = false, "needs work: arm64, the bootstrap is tested on amd64 only"
		}
		doc.Rows = append(doc.Rows, r)
	}
	sort.Slice(doc.Rows, func(i, j int) bool {
		a, b := doc.Rows[i], doc.Rows[j]
		if a.Selectable != b.Selectable {
			return a.Selectable
		}
		if (a.EstSpot == nil) != (b.EstSpot == nil) {
			return a.EstSpot != nil
		}
		if a.EstSpot != nil && *a.EstSpot != *b.EstSpot {
			return *a.EstSpot < *b.EstSpot
		}
		return a.Type < b.Type
	})
	return doc, nil
}

func ptr(f float64) *float64 { return &f }

func sortedCopy(s []string) []string {
	c := append([]string{}, s...)
	sort.Strings(c)
	return c
}

// ---- one refresh at a time ----

// lockCatalog keeps two catalog commands on one state dir from refreshing at
// the same time: the second waits, then reads what the first wrote. A lock
// older than two minutes is a crashed process's and is taken over.
func (e *env) lockCatalog(region string) (func(), error) {
	if err := os.MkdirAll(e.catalogDir(), 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(e.catalogDir(), "catalog-aws-"+region+".lock")
	for i := 0; i < 60; i++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(p) }, nil
		}
		if st, serr := os.Stat(p); serr == nil && e.now().Sub(st.ModTime()) > 2*time.Minute {
			os.Remove(p)
			continue
		}
		e.wait(1)
	}
	return nil, fmt.Errorf("catalog: another refresh holds %s", p)
}

// ---- fetchers (each is read-only) ----

type typeInfo struct {
	Type string  `json:"type"`
	VCPU int     `json:"vcpu"`
	MiB  float64 `json:"mib"`
	Arch string  `json:"arch"`
	GPUs int     `json:"gpus"`
}

func fetchInstanceTypes(ctx context.Context, a *awsCLI, fams []string) (json.RawMessage, error) {
	vals := make([]string, len(fams))
	for i, f := range fams {
		vals[i] = f + ".*"
	}
	out, _, err := a.r.Run(ctx, "aws", a.json("ec2", "describe-instance-types", "--filters", "Name=instance-type,Values="+strings.Join(vals, ","),
		"--query", "InstanceTypes[].[InstanceType,VCpuInfo.DefaultVCpus,MemoryInfo.SizeInMiB,ProcessorInfo.SupportedArchitectures[0],GpuInfo.Gpus[0].Count]"), nil)
	if err != nil {
		return nil, err
	}
	var rows [][]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("describe-instance-types: not JSON: %v", err)
	}
	var ts []typeInfo
	for _, r := range rows {
		if len(r) < 4 {
			continue
		}
		name, _ := r[0].(string)
		vcpu, _ := r[1].(float64)
		mib, _ := r[2].(float64)
		arch, _ := r[3].(string)
		var gpus float64
		if len(r) > 4 {
			gpus, _ = r[4].(float64)
		}
		if !awsTypeRe.MatchString(name) {
			continue
		}
		if arch == "x86_64" {
			arch = "amd64"
		}
		ts = append(ts, typeInfo{Type: name, VCPU: int(vcpu), MiB: mib, Arch: arch, GPUs: int(gpus)})
	}
	b, _ := json.Marshal(ts)
	return b, nil
}

// filterTypes drops what the picker never offers (metal, very large) and
// narrows to --types.
func filterTypes(ts []typeInfo, only []string) []typeInfo {
	want := map[string]bool{}
	for _, t := range only {
		want[t] = true
	}
	var out []typeInfo
	for _, t := range ts {
		if strings.HasSuffix(t.Type, ".metal") || t.VCPU > catalogMaxVCPU || t.VCPU == 0 {
			continue
		}
		if len(want) > 0 && !want[t.Type] {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

func fetchSpotPrices(ctx context.Context, a *awsCLI, now time.Time, types []string) (json.RawMessage, error) {
	res := map[string]map[string]float64{}
	for i := 0; i < len(types); i += 40 {
		chunk := types[i:min(i+40, len(types))]
		args := []string{"ec2", "describe-spot-price-history", "--start-time", now.UTC().Format(time.RFC3339), "--product-descriptions", "Linux/UNIX", "--instance-types"}
		args = append(args, chunk...)
		args = append(args, "--query", "SpotPriceHistory[].[InstanceType,AvailabilityZone,SpotPrice]")
		out, _, err := a.r.Run(ctx, "aws", a.json(args...), nil)
		if err != nil {
			return nil, err
		}
		var rows [][]string
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			return nil, fmt.Errorf("describe-spot-price-history: not JSON: %v", err)
		}
		for _, r := range rows {
			if len(r) < 3 {
				continue
			}
			p, err := strconv.ParseFloat(r[2], 64)
			if err != nil {
				continue
			}
			if res[r[0]] == nil {
				res[r[0]] = map[string]float64{}
			}
			res[r[0]][r[1]] = p
		}
	}
	b, _ := json.Marshal(res)
	return b, nil
}

// fetchOnDemand asks the Pricing API (its endpoint is in us-east-1 whatever the
// region priced), one type at a time, a few at once.
func fetchOnDemand(ctx context.Context, a *awsCLI, region string, types []string) (json.RawMessage, error) {
	pcfg := *a.cfg
	pcfg.AWSRegion = "us-east-1"
	pa := &awsCLI{r: a.r, cfg: &pcfg}
	var (
		mu       sync.Mutex
		res      = map[string]float64{}
		firstErr error
		wg       sync.WaitGroup
		sem      = make(chan struct{}, 6)
	)
	for _, t := range types {
		wg.Add(1)
		sem <- struct{}{}
		go func(t string) {
			defer wg.Done()
			defer func() { <-sem }()
			filt := func(field, v string) string { return "Type=TERM_MATCH,Field=" + field + ",Value=" + v }
			out, _, err := pa.r.Run(ctx, "aws", pa.json("pricing", "get-products", "--service-code", "AmazonEC2", "--filters",
				filt("instanceType", t), filt("regionCode", region), filt("operatingSystem", "Linux"), filt("tenancy", "Shared"),
				filt("preInstalledSw", "NA"), filt("capacitystatus", "Used")), nil)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			if p, ok := parseOnDemand(out); ok {
				res[t] = p
			}
		}(t)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	b, _ := json.Marshal(res)
	return b, nil
}

// parseOnDemand reads the hourly USD price out of a get-products answer.
func parseOnDemand(out string) (float64, bool) {
	var r struct{ PriceList []string }
	if json.Unmarshal([]byte(out), &r) != nil {
		return 0, false
	}
	for _, item := range r.PriceList {
		var it struct {
			Terms struct {
				OnDemand map[string]struct {
					PriceDimensions map[string]struct {
						Unit         string
						PricePerUnit map[string]string
					}
				}
			}
		}
		if json.Unmarshal([]byte(item), &it) != nil {
			continue
		}
		for _, term := range it.Terms.OnDemand {
			for _, d := range term.PriceDimensions {
				if d.Unit != "Hrs" {
					continue
				}
				if p, err := strconv.ParseFloat(d.PricePerUnit["USD"], 64); err == nil && p > 0 {
					return p, true
				}
			}
		}
	}
	return 0, false
}

func fetchPlacement(ctx context.Context, a *awsCLI, region string, types []string) (json.RawMessage, error) {
	args := append([]string{"ec2", "get-spot-placement-scores", "--instance-types"}, types...)
	args = append(args, "--target-capacity", "1", "--target-capacity-unit-type", "units", "--single-availability-zone",
		"--region-names", region, "--query", "SpotPlacementScores[].[AvailabilityZoneId,Score]")
	out, _, err := a.r.Run(ctx, "aws", a.json(args...), nil)
	if err != nil {
		return nil, err
	}
	var rows [][]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("get-spot-placement-scores: not JSON: %v", err)
	}
	var sc []zoneScore
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		id, _ := r[0].(string)
		s, _ := r[1].(float64)
		sc = append(sc, zoneScore{ZoneID: id, Score: int(s)})
	}
	b, _ := json.Marshal(sc)
	return b, nil
}

// fetchAdvisor reads AWS's public Spot Advisor file and keeps the one region's
// Linux bands. It is the data behind the console's "frequency of interruption",
// not an API with a contract: the format may change, and then the column is
// empty rather than wrong.
func (e *env) fetchAdvisor(ctx context.Context, region string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", advisorURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("spot advisor: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("spot advisor: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	return parseAdvisor(b, region)
}

func parseAdvisor(b []byte, region string) (json.RawMessage, error) {
	var d struct {
		Ranges []struct {
			Index int
			Label string
		}
		SpotAdvisor map[string]map[string]map[string]struct{ R int } `json:"spot_advisor"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("spot advisor: not JSON: %v", err)
	}
	labels := map[int]string{}
	for _, r := range d.Ranges {
		labels[r.Index] = r.Label
	}
	out := map[string]string{}
	for t, v := range d.SpotAdvisor[region]["Linux"] {
		if l, ok := labels[v.R]; ok {
			out[t] = l
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("spot advisor: nothing for %s (the file's format may have changed)", region)
	}
	j, _ := json.Marshal(out)
	return j, nil
}

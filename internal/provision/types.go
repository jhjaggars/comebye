// Package provision is the hub-and-peer side of adding a host: what a node
// that can provision answers (a read-only preflight of its credentials and the
// price catalog), and the subprocess that gets it from tools/worker.
//
// Nothing here creates a cloud resource. Everything that comes back from the
// tool or from a peer is a peer's word: it is decoded into the types below
// (only the fields named, nothing else survives), checked, bounded and
// scrubbed before the hub caches it or a browser sees it.
package provision

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// States of one credential check.
const (
	StateOK      = "ok"
	StateMissing = "missing"
	StateExpired = "expired"
	StateError   = "error"
)

// Check is one credential, as present or missing. Note is a profile or gateway
// name or a fixed phrase; never a key, a token or an account id.
type Check struct {
	State   string `json:"state"`
	Note    string `json:"note,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

// Preflight is what a node says about the credentials it holds and where it
// may build.
type Preflight struct {
	V         int      `json:"v"`
	CheckedAt string   `json:"checked_at"`
	AWS       Check    `json:"aws"`
	GCP       Check    `json:"gcp"`
	Tailnet   Check    `json:"tailnet"`
	OpenShell Check    `json:"openshell"`
	SSH       Check    `json:"ssh"`
	Region    string   `json:"region,omitempty"`
	Regions   []string `json:"regions,omitempty"`
}

var (
	regionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	typeRe   = regexp.MustCompile(`^[a-z][a-z0-9]*\.[a-z0-9]+$`)
	zoneRe   = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9][a-z]$`)
	zoneIDRe = regexp.MustCompile(`^[a-z]+[0-9]-az[0-9]+$`)
	// Anything shaped like a credential is removed from a note, whoever sent it.
	secretShape = regexp.MustCompile(`tskey-[A-Za-z0-9_-]+|sk-[A-Za-z0-9_-]{8,}|\b(AKIA|ASIA)[0-9A-Z]{16}\b|Bearer [A-Za-z0-9._~+/=-]+|\b[0-9]{12}\b|arn:aws[a-z-]*:[^ ]*|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}|(?i:secret|token|password|key)=\S+`)
)

// Text makes a peer's free text safe to show: one line, no control characters,
// credentials removed, at most max runes.
func Text(s string, max int) string {
	s = secretShape.ReplaceAllString(s, "[removed]")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
		default:
			b.WriteRune(r)
		}
	}
	s = strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

func cleanCheck(c Check) Check {
	switch c.State {
	case StateOK, StateMissing, StateExpired, StateError:
	default:
		c.State = StateError
	}
	c.Note, c.Gateway = Text(c.Note, 200), Text(c.Gateway, 64)
	return c
}

// ParsePreflight decodes a node's preflight answer. Fields it does not know
// are dropped; a state it does not know becomes "error".
func ParsePreflight(b []byte) (Preflight, error) {
	var p Preflight
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("the preflight is not understood: %w", err)
	}
	if p.V != 1 {
		return p, fmt.Errorf("the preflight is version %d, not 1", p.V)
	}
	p.AWS, p.GCP, p.Tailnet, p.OpenShell, p.SSH = cleanCheck(p.AWS), cleanCheck(p.GCP), cleanCheck(p.Tailnet), cleanCheck(p.OpenShell), cleanCheck(p.SSH)
	if _, err := time.Parse(time.RFC3339, p.CheckedAt); err != nil {
		p.CheckedAt = ""
	}
	if !regionRe.MatchString(p.Region) {
		p.Region = ""
	}
	var rs []string
	for _, r := range p.Regions {
		if regionRe.MatchString(r) && len(rs) < 40 {
			rs = append(rs, r)
		}
	}
	p.Regions = rs
	return p, nil
}

// ---- the catalog ----

const maxRows = 600

// Source says where one column's data came from and how old it is.
type Source struct {
	FetchedAt string `json:"fetched_at,omitempty"`
	State     string `json:"state"` // fresh, cached, stale, missing
	Error     string `json:"error,omitempty"`
	Source    string `json:"source"`
}

// Row is one instance type.
type Row struct {
	Type         string             `json:"type"`
	VCPU         int                `json:"vcpu"`
	MemoryGiB    float64            `json:"memory_gib"`
	Arch         string             `json:"arch"`
	GPUs         int                `json:"gpus"`
	OnDemand     *float64           `json:"on_demand_usd_hour"`
	Spot         map[string]float64 `json:"spot_usd_hour,omitempty"`
	SpotMin      *float64           `json:"spot_min_usd_hour"`
	Interruption string             `json:"interruption,omitempty"`
	EstSpot      *float64           `json:"estimate_spot_usd_hour"`
	EstOnDemand  *float64           `json:"estimate_on_demand_usd_hour"`
	Selectable   bool               `json:"selectable"`
	Note         string             `json:"note,omitempty"`
}

// ZoneScore is one zone's capacity score for a shortlist.
type ZoneScore struct {
	ZoneID string `json:"zone_id"`
	Score  int    `json:"score"`
}

// Placement is the capacity answer for a shortlist of types.
type Placement struct {
	Types     []string    `json:"types"`
	Scores    []ZoneScore `json:"scores"`
	FetchedAt string      `json:"fetched_at,omitempty"`
	Source    string      `json:"source,omitempty"`
}

// Estimate holds the terms of the cost estimate.
type Estimate struct {
	IPv4USDHour    float64 `json:"ipv4_usd_hour"`
	DiskUSDGBMonth float64 `json:"disk_usd_gb_month"`
	DiskGB         int     `json:"disk_gb"`
}

// Catalog is the instance table of one region.
type Catalog struct {
	V           int               `json:"v"`
	Kind        string            `json:"kind"`
	Region      string            `json:"region"`
	GeneratedAt string            `json:"generated_at,omitempty"`
	Degraded    string            `json:"degraded,omitempty"`
	Sources     map[string]Source `json:"sources"`
	Estimate    Estimate          `json:"estimate"`
	Rows        []Row             `json:"rows"`
	Placement   *Placement        `json:"placement,omitempty"`
}

func price(p *float64) (*float64, bool) {
	if p == nil {
		return nil, true
	}
	if math.IsNaN(*p) || math.IsInf(*p, 0) || *p < 0 || *p > 10000 {
		return nil, false
	}
	return p, true
}

func okTime(s string) string {
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		return ""
	}
	return s
}

// ParseCatalog decodes a node's catalog answer and keeps only what is
// well-formed: bad rows are dropped, bad prices become unknown.
func ParseCatalog(b []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("the catalog is not understood: %w", err)
	}
	if c.V != 1 {
		return nil, fmt.Errorf("the catalog is version %d, not 1", c.V)
	}
	if c.Kind != "aws" {
		return nil, fmt.Errorf("the catalog is for %q, not aws", Text(c.Kind, 20))
	}
	if !regionRe.MatchString(c.Region) {
		return nil, errors.New("the catalog names no valid region")
	}
	c.GeneratedAt = okTime(c.GeneratedAt)
	switch c.Degraded {
	case "", "creds_expired", "error":
	default:
		c.Degraded = "error"
	}
	srcs := map[string]Source{}
	for k, s := range c.Sources {
		switch k {
		case "types", "spot_price", "on_demand", "spot_advisor", "placement":
		default:
			continue
		}
		switch s.State {
		case "fresh", "cached", "stale", "missing":
		default:
			s.State = "missing"
		}
		switch s.Error {
		case "", "creds_expired", "error":
		default:
			s.Error = "error"
		}
		s.FetchedAt, s.Source = okTime(s.FetchedAt), Text(s.Source, 80)
		srcs[k] = s
	}
	c.Sources = srcs
	c.Estimate.DiskGB = clampInt(c.Estimate.DiskGB, 0, 100000)
	if c.Estimate.IPv4USDHour < 0 || c.Estimate.IPv4USDHour > 10 {
		c.Estimate.IPv4USDHour = 0
	}
	if c.Estimate.DiskUSDGBMonth < 0 || c.Estimate.DiskUSDGBMonth > 10 {
		c.Estimate.DiskUSDGBMonth = 0
	}
	var rows []Row
	for _, r := range c.Rows {
		if len(rows) >= maxRows {
			break
		}
		if !typeRe.MatchString(r.Type) || r.VCPU < 0 || r.VCPU > 1000 || r.MemoryGiB < 0 || r.MemoryGiB > 100000 {
			continue
		}
		switch r.Arch {
		case "amd64", "arm64":
		default:
			r.Arch = ""
			r.Selectable = false
		}
		r.GPUs = clampInt(r.GPUs, 0, 64)
		var ok1, ok2, ok3, ok4 bool
		r.OnDemand, ok1 = price(r.OnDemand)
		r.SpotMin, ok2 = price(r.SpotMin)
		r.EstSpot, ok3 = price(r.EstSpot)
		r.EstOnDemand, ok4 = price(r.EstOnDemand)
		if !ok1 || !ok2 || !ok3 || !ok4 {
			continue
		}
		zs := map[string]float64{}
		for z, p := range r.Spot {
			if zoneRe.MatchString(z) && p >= 0 && p <= 10000 && !math.IsNaN(p) && len(zs) < 30 {
				zs[z] = p
			}
		}
		r.Spot = zs
		r.Interruption = Text(r.Interruption, 12)
		r.Note = Text(r.Note, 120)
		rows = append(rows, r)
	}
	c.Rows = rows
	if pl := c.Placement; pl != nil {
		var types []string
		for _, t := range pl.Types {
			if typeRe.MatchString(t) && len(types) < 10 {
				types = append(types, t)
			}
		}
		var sc []ZoneScore
		for _, z := range pl.Scores {
			if zoneIDRe.MatchString(z.ZoneID) && z.Score >= 0 && z.Score <= 10 && len(sc) < 30 {
				sc = append(sc, z)
			}
		}
		pl.Types, pl.Scores, pl.FetchedAt, pl.Source = types, sc, okTime(pl.FetchedAt), Text(pl.Source, 80)
	}
	return &c, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Small sizes are hidden from the picker by default: a worker runs herdr, the
// agent CLIs and a build, and a nano or micro (a fraction of a GiB) cannot.
const (
	MinVCPU      = 2
	MinMemoryGiB = 4.0
)

// HideSmall returns the rows that meet the minimum size, and how many it left out.
func (c *Catalog) HideSmall() (kept []Row, hidden int) {
	for _, r := range c.Rows {
		if r.VCPU < MinVCPU || r.MemoryGiB < MinMemoryGiB {
			hidden++
			continue
		}
		kept = append(kept, r)
	}
	return kept, hidden
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

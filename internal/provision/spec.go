package provision

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
)

// Spec is one request to build a host, as the hub validates it and sends it
// to a provisioner, and as the provisioner validates it again and hands it to
// the worker tool (--spec -). It is closed: unknown fields are refused at
// every hop, and nothing in it reaches a shell (the tool is run with argv
// only and reads the spec from stdin).
type Spec struct {
	Kind              string  `json:"kind"`
	Name              string  `json:"name"`
	Type              string  `json:"type"`
	Spot              bool    `json:"spot"`
	Zone              string  `json:"zone,omitempty"`
	BootDiskGB        int     `json:"boot_disk_gb"`
	DataDiskGB        int     `json:"data_disk_gb"`
	IdleMinutes       int     `json:"idle_minutes"`
	MaxRun            string  `json:"max_run,omitempty"`
	BudgetCapUSD      float64 `json:"budget_cap_usd"`
	DestroyAfterHours int     `json:"destroy_after_hours"`
}

// Defaults the wizard starts from (accepted in the plan: spot by default,
// stop after 2 h idle, destroy 3 days after, a $10 budget cap).
const (
	DefaultBootGB       = 20
	DefaultDataGB       = 50
	DefaultIdleMinutes  = 120
	DefaultMaxRun       = "24h"
	DefaultBudgetUSD    = 10.0
	DefaultDestroyHours = 72
)

// Ceilings the hub's request is held to here as well as on the tool.
const (
	MinDisk        = 10
	MaxBootDisk    = 200
	MaxDataDisk    = 1000
	MinIdle        = 5
	MaxIdle        = 1440
	MaxBudgetUSD   = 100.0
	MaxDestroyHrs  = 720
	MaxRunHoursCap = 168
)

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,28}[a-z0-9]$`)
	maxRunRe = regexp.MustCompile(`^[1-9][0-9]{0,2}h$`)
)

// SpecError is a refused request: Field names what to fix.
type SpecError struct{ Field, Msg string }

func (e *SpecError) Error() string { return e.Msg }

func bad(field, format string, a ...any) error {
	return &SpecError{Field: field, Msg: fmt.Sprintf(format, a...)}
}

// DefaultSpec is a spec with the defaults filled in, for a size and a name.
func DefaultSpec(name, typ string) Spec {
	return Spec{Kind: "aws", Name: name, Type: typ, Spot: true, BootDiskGB: DefaultBootGB, DataDiskGB: DefaultDataGB,
		IdleMinutes: DefaultIdleMinutes, MaxRun: DefaultMaxRun, BudgetCapUSD: DefaultBudgetUSD, DestroyAfterHours: DefaultDestroyHours}
}

// ParseSpec decodes strictly: one object, no unknown fields, nothing after it.
func ParseSpec(b []byte) (Spec, error) {
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, bad("", "the request is not understood: %s", Text(err.Error(), 120))
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return s, bad("", "the request has data after the object")
	}
	return s, s.Validate()
}

// Validate checks every field against its range. A spec that passes can be
// handed to the tool; the tool checks again against its config's ceilings.
func (s *Spec) Validate() error {
	switch {
	case s.Kind != "aws":
		return bad("kind", "only aws can be built so far")
	case !nameRe.MatchString(s.Name):
		return bad("name", "the name is 2 to 30 lowercase letters, digits and dashes, starting with a letter")
	case !typeRe.MatchString(s.Type):
		return bad("type", "choose an instance type like t3.xlarge")
	case s.Zone != "" && !zoneRe.MatchString(s.Zone):
		return bad("zone", "the zone is like us-east-1b")
	case s.BootDiskGB < MinDisk || s.BootDiskGB > MaxBootDisk:
		return bad("boot_disk_gb", "the boot disk is %d to %d GB", MinDisk, MaxBootDisk)
	case s.DataDiskGB < MinDisk || s.DataDiskGB > MaxDataDisk:
		return bad("data_disk_gb", "the data disk is %d to %d GB", MinDisk, MaxDataDisk)
	case s.IdleMinutes < MinIdle || s.IdleMinutes > MaxIdle:
		return bad("idle_minutes", "stop after idle is %d to %d minutes", MinIdle, MaxIdle)
	case s.MaxRun != "" && !maxRunRe.MatchString(s.MaxRun):
		return bad("max_run", "the longest run is like 24h")
	case math.IsNaN(s.BudgetCapUSD) || s.BudgetCapUSD < 1 || s.BudgetCapUSD > MaxBudgetUSD:
		return bad("budget_cap_usd", "the budget cap is $1 to $%.0f", MaxBudgetUSD)
	case s.DestroyAfterHours < 1 || s.DestroyAfterHours > MaxDestroyHrs:
		return bad("destroy_after_hours", "destroy after is 1 to %d hours", MaxDestroyHrs)
	}
	if s.MaxRun != "" {
		var h int
		fmt.Sscanf(s.MaxRun, "%dh", &h)
		if h > MaxRunHoursCap {
			return bad("max_run", "the longest run is at most %dh", MaxRunHoursCap)
		}
	}
	return nil
}

// JSON is the spec as the tool reads it (the same field names).
func (s *Spec) JSON() []byte {
	b, _ := json.Marshal(s)
	return b
}

// ---- cost ----

// Cost is what a spec is expected to cost: an estimate from the node's own
// price list, never a promise.
type Cost struct {
	Mode          string  `json:"mode"` // spot or on_demand
	USDHour       float64 `json:"usd_hour"`
	USDDay        float64 `json:"usd_day"`
	OnDemandHour  float64 `json:"on_demand_usd_hour,omitempty"`
	BudgetHours   float64 `json:"budget_hours,omitempty"` // how long the cap lasts at this rate
	Basis         string  `json:"basis"`
	PricesFetched string  `json:"prices_fetched_at,omitempty"`
}

const hoursPerMonth = 730

// EstimateCost prices a spec from a catalog row: the lowest spot price (or the
// on-demand price) plus the public IPv4 and the disks.
func EstimateCost(s Spec, c *Catalog) (*Cost, error) {
	if c == nil {
		return nil, errors.New("no price list")
	}
	var row *Row
	for i := range c.Rows {
		if c.Rows[i].Type == s.Type {
			row = &c.Rows[i]
			break
		}
	}
	if row == nil {
		return nil, fmt.Errorf("%s is not in the price list", s.Type)
	}
	disks := float64(s.BootDiskGB+s.DataDiskGB) * c.Estimate.DiskUSDGBMonth / hoursPerMonth
	extra := c.Estimate.IPv4USDHour + disks
	out := &Cost{Mode: "on_demand", PricesFetched: c.GeneratedAt}
	if row.OnDemand != nil {
		out.OnDemandHour = round4(*row.OnDemand + extra)
	}
	var base *float64
	if s.Spot {
		out.Mode = "spot"
		if s.Zone != "" {
			if p, ok := row.Spot[s.Zone]; ok {
				base = &p
			}
		}
		if base == nil {
			base = row.SpotMin
		}
	} else {
		base = row.OnDemand
	}
	if base == nil {
		return nil, fmt.Errorf("no %s price for %s", strings.ReplaceAll(out.Mode, "_", "-"), s.Type)
	}
	out.USDHour = round4(*base + extra)
	out.USDDay = round2(out.USDHour * 24)
	if out.USDHour > 0 {
		out.BudgetHours = round2(s.BudgetCapUSD / out.USDHour)
	}
	out.Basis = fmt.Sprintf("%s price + $%.3f/h public IPv4 + %d GB of disk at $%.2f per GB-month", strings.ReplaceAll(out.Mode, "_", "-"), c.Estimate.IPv4USDHour, s.BootDiskGB+s.DataDiskGB, c.Estimate.DiskUSDGBMonth)
	return out, nil
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

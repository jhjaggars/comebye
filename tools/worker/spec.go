package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Spec is one request laid over the config: what differs per host. The
// config stays the home of what is per node (profile, project, hub_url,
// ts_allow_login, the ceilings). Every field is optional; a field that is
// absent leaves the config's value. Unknown fields are an error, so a typo
// or a field from a newer caller never goes unnoticed.
type Spec struct {
	Kind              string   `json:"kind"` // gcp, aws or openshell
	Name              string   `json:"name"`
	Type              string   `json:"type"` // instance or machine type
	Spot              *bool    `json:"spot"`
	Zone              string   `json:"zone"`
	BootDiskGB        *int     `json:"boot_disk_gb"`
	DataDiskGB        *int     `json:"data_disk_gb"`
	IdleMinutes       *int     `json:"idle_minutes"`
	MaxRun            *string  `json:"max_run"` // "24h"; "" disables
	Agents            []string `json:"agents"`
	LLMKey            *bool    `json:"llm_key"`
	BudgetCapUSD      *float64 `json:"budget_cap_usd"`
	DestroyAfterHours *int     `json:"destroy_after_hours"`
}

const maxSpecBytes = 64 << 10

// ReadSpec reads a spec from a file, or from in when path is "-".
func ReadSpec(path string, in io.Reader) (*Spec, error) {
	var r io.Reader = in
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("spec: %w", err)
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxSpecBytes+1))
	if err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	if len(b) > maxSpecBytes {
		return nil, fmt.Errorf("spec: larger than %d bytes", maxSpecBytes)
	}
	return ParseSpec(b)
}

// ParseSpec decodes strictly: one JSON object, no unknown fields, nothing after it.
func ParseSpec(b []byte) (*Spec, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("spec: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("spec: more than one JSON value")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("spec: data after the JSON object")
	}
	return &s, nil
}

var specKinds = map[string]bool{"gcp": true, "aws": true, "openshell": true}

// Apply lays the spec over c and re-validates. The result is a whole config
// that passes the same checks as one read from the file, so the spec cannot
// reach anything the file could not.
func (s *Spec) Apply(c *Config) error {
	if s.Kind != "" {
		if !specKinds[s.Kind] {
			return fmt.Errorf("spec: kind %q: want gcp, aws or openshell", s.Kind)
		}
		c.Provider = s.Kind
	}
	if s.Name != "" {
		if err := checkName(s.Name); err != nil {
			return fmt.Errorf("spec: %w", err)
		}
	}
	// A sandbox has no machine, zone or disks: refuse rather than ignore.
	if c.Provider == "openshell" {
		for field, set := range map[string]bool{
			"type": s.Type != "", "spot": s.Spot != nil, "zone": s.Zone != "",
			"boot_disk_gb": s.BootDiskGB != nil, "data_disk_gb": s.DataDiskGB != nil,
			"max_run": s.MaxRun != nil, "agents": s.Agents != nil,
		} {
			if set {
				return fmt.Errorf("spec: %s does not apply to kind openshell", field)
			}
		}
	}
	if s.Type != "" {
		if c.Provider == "aws" && !c.familyAllowed(s.Type) {
			return fmt.Errorf("spec: type %q is not in an allowed family (allowed_families)", s.Type)
		}
		c.MachineType, c.machineSet = s.Type, true
	}
	if s.Spot != nil {
		if *s.Spot && c.Provider != "aws" {
			return fmt.Errorf("spec: spot is only for kind aws")
		}
		c.AWSSpot = *s.Spot
	}
	if s.Zone != "" {
		switch c.Provider {
		case "aws":
			c.AWSAZ = s.Zone
		case "gcp":
			c.Zone = s.Zone
		}
	}
	if s.BootDiskGB != nil {
		c.BootDiskGB = *s.BootDiskGB
	}
	if s.DataDiskGB != nil {
		c.DataDiskGB = *s.DataDiskGB
	}
	if s.IdleMinutes != nil {
		c.IdleMinutes = *s.IdleMinutes
	}
	if s.MaxRun != nil {
		c.MaxRunDuration = *s.MaxRun
	}
	if s.Agents != nil {
		c.Agents = append([]string(nil), s.Agents...)
	}
	if s.LLMKey != nil {
		switch {
		case !*s.LLMKey:
			c.LLMGatewayURL = ""
		case c.LLMGatewayURL == "":
			return fmt.Errorf("spec: llm_key is true, but the config has no llm_gateway_url")
		}
	}
	if s.BudgetCapUSD != nil {
		if *s.BudgetCapUSD > c.budgetCeiling() {
			return fmt.Errorf("spec: budget_cap_usd %.2f is over this node's ceiling of %.2f (max_budget_cap_usd)", *s.BudgetCapUSD, c.budgetCeiling())
		}
		c.BudgetCapUSD = *s.BudgetCapUSD
	}
	if s.DestroyAfterHours != nil {
		c.DestroyAfterHours = *s.DestroyAfterHours
	}
	c.finish()
	if err := c.Validate(); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	return nil
}

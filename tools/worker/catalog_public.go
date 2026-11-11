package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The price browser's anonymous data. None of this needs an AWS login:
//
//	specs and on-demand prices  AWS's public Price List bulk file for the region
//	                            (a few hundred MB; streamed, only Linux shared
//	                            instances of the allowed families are kept)
//	spot prices                 the JSON behind the spot-pricing page (spot.js):
//	                            one price per type for the region, undocumented
//	interruption bands          the public Spot Advisor file (catalog.go)
//
// With a login the catalog adds per-zone spot prices and capacity scores. A
// public source that changes shape gives an empty column, never a wrong one.

const (
	bulkURLFmt = "https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/AmazonEC2/current/%s/index.json"
	spotJSURL  = "https://website.spot.ec2.aws.a2z.com/spot.js"

	ttlBulk   = 24 * time.Hour
	ttlSpotJS = 15 * time.Minute

	bulkTimeout = 12 * time.Minute
	maxSpotJS   = 64 << 20
)

// bulkData is what is kept from the bulk file.
type bulkData struct {
	Types []typeInfo         `json:"types"`
	OD    map[string]float64 `json:"on_demand"`
}

func (e *env) httpNoTimeout() httpDoer {
	if c, ok := e.HTTP.(*http.Client); ok {
		cc := *c
		cc.Timeout = 0
		return &cc
	}
	return e.HTTP
}

func (e *env) fetchBulk(ctx context.Context, region string, fams []string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, bulkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf(bulkURLFmt, region), nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.httpNoTimeout().Do(req)
	if err != nil {
		return nil, fmt.Errorf("price list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("price list: HTTP %d", resp.StatusCode)
	}
	d, err := parseBulk(resp.Body, region, fams)
	if err != nil {
		return nil, err
	}
	return json.Marshal(d)
}

type bulkProduct struct {
	ProductFamily string
	Attributes    map[string]string
}

func objectStart(dec *json.Decoder, what string) error {
	t, err := dec.Token()
	if d, ok := t.(json.Delim); err != nil || !ok || d != '{' {
		return fmt.Errorf("price list: %s is not an object (the file's format may have changed)", what)
	}
	return nil
}

// skipValue reads past the next JSON value without holding it.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

var memRe = strings.NewReplacer(",", "", " GiB", "")

// parseBulk streams the bulk file: it never holds more than one product or
// one term at a time, and keeps only Linux, shared-tenancy, license-free
// instances of the given families.
func parseBulk(r io.Reader, region string, fams []string) (*bulkData, error) {
	want := map[string]bool{}
	for _, f := range fams {
		want[f] = true
	}
	dec := json.NewDecoder(r)
	if err := objectStart(dec, "the file"); err != nil {
		return nil, err
	}
	skus := map[string]typeInfo{}
	od := map[string]float64{}
	sawProducts, sawTerms := false, false
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("price list: %v", err)
		}
		key, _ := kt.(string)
		switch key {
		case "products":
			sawProducts = true
			if err := objectStart(dec, "products"); err != nil {
				return nil, err
			}
			for dec.More() {
				st, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("price list: %v", err)
				}
				var p bulkProduct
				if err := dec.Decode(&p); err != nil {
					return nil, fmt.Errorf("price list: %v", err)
				}
				a := p.Attributes
				typ := a["instanceType"]
				if p.ProductFamily != "Compute Instance" || !awsTypeRe.MatchString(typ) {
					continue
				}
				fam, _, _ := strings.Cut(typ, ".")
				if !want[fam] || a["operatingSystem"] != "Linux" || a["tenancy"] != "Shared" || a["preInstalledSw"] != "NA" ||
					a["capacitystatus"] != "Used" || a["licenseModel"] != "No License required" || a["operation"] != "RunInstances" || (a["regionCode"] != "" && a["regionCode"] != region) {
					continue
				}
				vcpu, _ := strconv.Atoi(a["vcpu"])
				gib, _ := strconv.ParseFloat(memRe.Replace(a["memory"]), 64)
				arch := "amd64"
				if strings.Contains(strings.ToLower(a["physicalProcessor"]), "graviton") || gravitonRe.MatchString(typ) {
					arch = "arm64"
				}
				gpus, _ := strconv.Atoi(a["gpu"])
				skus[st.(string)] = typeInfo{Type: typ, VCPU: vcpu, MiB: gib * 1024, Arch: arch, GPUs: gpus}
			}
			dec.Token() // }
		case "terms":
			sawTerms = true
			if err := objectStart(dec, "terms"); err != nil {
				return nil, err
			}
			for dec.More() {
				tk, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("price list: %v", err)
				}
				if tk != "OnDemand" {
					// Reserved terms are most of the file: skip them token by
					// token, since Decode would buffer the whole value.
					if err := skipValue(dec); err != nil {
						return nil, fmt.Errorf("price list: %v", err)
					}
					continue
				}
				if err := objectStart(dec, "OnDemand"); err != nil {
					return nil, err
				}
				for dec.More() {
					st, err := dec.Token()
					if err != nil {
						return nil, fmt.Errorf("price list: %v", err)
					}
					ti, ok := skus[st.(string)]
					if !ok {
						var skip struct{}
						if err := dec.Decode(&skip); err != nil {
							return nil, fmt.Errorf("price list: %v", err)
						}
						continue
					}
					var term map[string]struct {
						PriceDimensions map[string]struct {
							Unit         string
							PricePerUnit map[string]string
						}
					}
					if err := dec.Decode(&term); err != nil {
						return nil, fmt.Errorf("price list: %v", err)
					}
				found:
					for _, t := range term {
						for _, d := range t.PriceDimensions {
							if p, err := strconv.ParseFloat(d.PricePerUnit["USD"], 64); err == nil && p > 0 && d.Unit == "Hrs" {
								od[ti.Type] = p
								break found
							}
						}
					}
				}
				dec.Token() // }
			}
			dec.Token() // }
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, fmt.Errorf("price list: %v", err)
			}
		}
	}
	if !sawProducts || !sawTerms || len(skus) == 0 {
		return nil, fmt.Errorf("price list: nothing usable for %s (the file's format may have changed)", region)
	}
	seen := map[string]bool{}
	out := &bulkData{OD: od}
	for _, t := range skus {
		if !seen[t.Type] {
			seen[t.Type] = true
			out.Types = append(out.Types, t)
		}
	}
	return out, nil
}

// fetchSpotJS reads the spot-pricing page's data feed: one Linux price per
// instance type for the region.
func (e *env) fetchSpotJS(ctx context.Context, region string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", spotJSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("spot feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("spot feed: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSpotJS))
	if err != nil {
		return nil, err
	}
	return parseSpotJS(b, region)
}

func parseSpotJS(b []byte, region string) (json.RawMessage, error) {
	b = bytes.TrimSpace(b)
	i, j := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if i < 0 || j < i {
		return nil, fmt.Errorf("spot feed: not the expected callback (the format may have changed)")
	}
	var d struct {
		Config struct {
			Regions []struct {
				Region        string
				InstanceTypes []struct {
					Sizes []struct {
						Size         string
						ValueColumns []struct {
							Name   string
							Prices map[string]string
						}
					}
				}
			}
		}
	}
	if err := json.Unmarshal(b[i+1:j], &d); err != nil {
		return nil, fmt.Errorf("spot feed: not JSON: %v", err)
	}
	out := map[string]float64{}
	for _, r := range d.Config.Regions {
		if r.Region != region {
			continue
		}
		for _, it := range r.InstanceTypes {
			for _, s := range it.Sizes {
				for _, c := range s.ValueColumns {
					if c.Name != "linux" {
						continue
					}
					if p, err := strconv.ParseFloat(c.Prices["USD"], 64); err == nil && p > 0 && awsTypeRe.MatchString(s.Size) {
						out[s.Size] = p
					}
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("spot feed: nothing for %s (the format may have changed)", region)
	}
	return json.Marshal(out)
}

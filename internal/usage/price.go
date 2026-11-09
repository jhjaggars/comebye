package usage

import (
	"sort"
	"strings"
)

// Price is dollars per million tokens.
type Price struct {
	In  float64 `json:"in"`
	Out float64 `json:"out"`
	CW  float64 `json:"cw"`
	CR  float64 `json:"cr"`
}

// DefaultPrices are list prices for the Claude models, as of 2026-11-03
// (Anthropic's published first-party API rates). They are estimates: cache
// writes are priced at the 5-minute rate (1.25x input; the 1-hour tier is
// dearer), a prompt over a model's long-context threshold is not repriced,
// and a discount or a different platform is not known here. Models without
// a published price (Pi's, whatever provider) have none: their tokens are
// counted and no dollars are estimated until the user enters a price.
var DefaultPrices = map[string]Price{
	"claude-fable-5-2":  {8, 40, 10, 0.20},
	"claude-fable-5-1":  {10, 50, 12.5, 1},
	"claude-fable-5":    {10, 50, 12.5, 1},
	"claude-opus-5-5":   {4, 20, 5, 0.20},
	"claude-opus-5":     {5, 25, 6.25, 0.50},
	"claude-opus-4-8":   {5, 25, 6.25, 0.50},
	"claude-opus-4-7":   {5, 25, 6.25, 0.50},
	"claude-opus-4-6":   {5, 25, 6.25, 0.50},
	"claude-sonnet-5-5": {2, 10, 2.5, 0.10},
	"claude-sonnet-5":   {2, 10, 2.5, 0.20},
	"claude-sonnet-4-6": {3, 15, 3.75, 0.30},
	"claude-haiku-5-5":  {0.10, 0.50, 0.125, 0.01},
	"claude-haiku-4-5":  {1, 5, 1.25, 0.10},
}

// PriceTable is the effective prices: the defaults with the user's entries
// laid over them. An entry with every field zero removes a default.
type PriceTable map[string]Price

// Effective overlays over onto the defaults.
func Effective(over map[string]Price) PriceTable {
	t := PriceTable{}
	for k, v := range DefaultPrices {
		t[k] = v
	}
	for k, v := range over {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if v == (Price{}) {
			delete(t, k)
			continue
		}
		t[k] = v
	}
	return t
}

// Lookup finds model's price: the exact name, else the longest listed name
// that prefixes it ("claude-opus-4-8-20260101" is "claude-opus-4-8").
func (t PriceTable) Lookup(model string) (Price, bool) {
	if p, ok := t[model]; ok {
		return p, true
	}
	best := ""
	for k := range t {
		if strings.HasPrefix(model, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return Price{}, false
	}
	return t[best], true
}

// USD estimates the cost of t at model's price (false: no price).
func (t PriceTable) USD(model string, tok Tokens) (float64, bool) {
	p, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	return (float64(tok.In)*p.In + float64(tok.Out)*p.Out + float64(tok.CW)*p.CW + float64(tok.CR)*p.CR) / 1e6, true
}

// Models lists the table's model names.
func (t PriceTable) Models() []string {
	var out []string
	for k := range t {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

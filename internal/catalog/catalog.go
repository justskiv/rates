// Package catalog exposes the currency universe and the peg table from the
// embedded contract export, plus the bundled rates snapshot used as the
// jump baseline before anything has been published.
package catalog

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/justskiv/rates/contracts"
	"github.com/justskiv/rates/internal/rates"
)

// Data is the catalog part of contracts/rates-contract.json.
type Data struct {
	Codes     []rates.Code            `json:"codes"`
	MultiRate []rates.Code            `json:"multiRate"`
	USDPegs   map[rates.Code]float64  `json:"usdPegs"`
	CrossPegs map[rates.Code]CrossPeg `json:"crossPegs"`
}

// CrossPeg pins a currency to an anchor other than USD:
// value = Factor * value(Anchor).
type CrossPeg struct {
	Anchor rates.Code `json:"anchor"`
	Factor float64    `json:"factor"`
}

// Catalog is an immutable view of the currency universe.
type Catalog struct {
	codes     []rates.Code
	index     map[rates.Code]struct{}
	multiRate map[rates.Code]struct{}
	usdPegs   map[rates.Code]float64
	crossPegs map[rates.Code]CrossPeg
}

// Default parses the catalog embedded at build time.
func Default() (*Catalog, error) {
	var d Data
	if err := json.Unmarshal(contracts.Contract(), &d); err != nil {
		return nil, fmt.Errorf("catalog: decode embedded data: %w", err)
	}
	return New(d)
}

// Bundled returns the app's bundled snapshot (contracts/rates-snapshot.json).
func Bundled() (rates.Snapshot, error) {
	s, err := rates.Decode(contracts.Snapshot())
	if err != nil {
		return rates.Snapshot{}, fmt.Errorf("catalog: bundled snapshot: %w", err)
	}
	return s, nil
}

// New validates d and builds a catalog from it.
func New(d Data) (*Catalog, error) {
	c := &Catalog{
		codes:     slices.Clone(d.Codes),
		index:     make(map[rates.Code]struct{}, len(d.Codes)),
		multiRate: make(map[rates.Code]struct{}, len(d.MultiRate)),
		usdPegs:   maps.Clone(d.USDPegs),
		crossPegs: maps.Clone(d.CrossPegs),
	}
	for _, code := range c.codes {
		if !rates.ValidCode(string(code)) {
			return nil, fmt.Errorf("catalog: invalid code %q", code)
		}
		if _, dup := c.index[code]; dup {
			return nil, fmt.Errorf("catalog: duplicate code %s", code)
		}
		c.index[code] = struct{}{}
	}
	slices.Sort(c.codes)
	if !c.Contains(rates.Base) {
		return nil, fmt.Errorf("catalog: base %s is missing", rates.Base)
	}
	for _, code := range d.MultiRate {
		if !c.Contains(code) {
			return nil, fmt.Errorf("catalog: multiRate code %s is not in the catalog", code)
		}
		c.multiRate[code] = struct{}{}
	}
	for code, v := range c.usdPegs {
		if !c.Contains(code) || code == rates.Base {
			return nil, fmt.Errorf("catalog: usd peg %s is not a catalog currency", code)
		}
		if !rates.ValidRate(v) {
			return nil, fmt.Errorf("catalog: usd peg %s has invalid value %v", code, v)
		}
	}
	for code, p := range c.crossPegs {
		if !c.Contains(code) || !c.Contains(p.Anchor) {
			return nil, fmt.Errorf("catalog: cross peg %s->%s outside the catalog", code, p.Anchor)
		}
		if _, ok := c.usdPegs[code]; ok {
			return nil, fmt.Errorf("catalog: %s is pegged twice", code)
		}
		if _, ok := c.crossPegs[p.Anchor]; ok {
			return nil, fmt.Errorf("catalog: cross peg %s chains through %s", code, p.Anchor)
		}
		if !rates.ValidRate(p.Factor) {
			return nil, fmt.Errorf("catalog: cross peg %s has invalid factor %v", code, p.Factor)
		}
	}
	return c, nil
}

// Codes returns all catalog codes in ascending order.
func (c *Catalog) Codes() []rates.Code {
	return slices.Clone(c.codes)
}

// Len returns the number of catalog currencies.
func (c *Catalog) Len() int {
	return len(c.codes)
}

// Contains reports whether code belongs to the catalog.
func (c *Catalog) Contains(code rates.Code) bool {
	_, ok := c.index[code]
	return ok
}

// MultiRate reports whether code is flagged multiRate in the catalog.
func (c *Catalog) MultiRate(code rates.Code) bool {
	_, ok := c.multiRate[code]
	return ok
}

// USDPeg returns the hard peg of code to USD, if any.
func (c *Catalog) USDPeg(code rates.Code) (float64, bool) {
	v, ok := c.usdPegs[code]
	return v, ok
}

// CrossPegOf returns the peg of code to a non-USD anchor, if any.
func (c *Catalog) CrossPegOf(code rates.Code) (CrossPeg, bool) {
	p, ok := c.crossPegs[code]
	return p, ok
}

// Pegged reports whether code has any peg.
func (c *Catalog) Pegged(code rates.Code) bool {
	_, usd := c.usdPegs[code]
	_, cross := c.crossPegs[code]
	return usd || cross
}

// Data returns a copy of the serialized form.
func (c *Catalog) Data() Data {
	multi := slices.Sorted(maps.Keys(c.multiRate))
	return Data{
		Codes:     slices.Clone(c.codes),
		MultiRate: multi,
		USDPegs:   maps.Clone(c.usdPegs),
		CrossPegs: maps.Clone(c.crossPegs),
	}
}

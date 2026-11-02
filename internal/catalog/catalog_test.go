package catalog_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/justskiv/rates/internal/catalog"
	"github.com/justskiv/rates/internal/rates"
)

func TestDefault(t *testing.T) {
	cat, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	if cat.Len() != 153 {
		t.Errorf("catalog has %d currencies, want 153", cat.Len())
	}
	if v, ok := cat.USDPeg("AED"); !ok || v != 3.6725 {
		t.Errorf("AED peg = %v %v", v, ok)
	}
	if p, ok := cat.CrossPegOf("XOF"); !ok || p.Anchor != "EUR" || p.Factor != 655.957 {
		t.Errorf("XOF peg = %+v %v", p, ok)
	}
	if !cat.MultiRate("CUP") || cat.MultiRate("ARS") {
		t.Error("multiRate flags wrong")
	}
	snap, err := catalog.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Rates) != 153 {
		t.Errorf("bundled snapshot has %d rates", len(snap.Rates))
	}
}

func TestNewRejectsInconsistentData(t *testing.T) {
	valid := func() catalog.Data {
		return catalog.Data{
			Codes:     []rates.Code{"EUR", "USD", "XOF", "AED"},
			MultiRate: []rates.Code{},
			USDPegs:   map[rates.Code]float64{"AED": 3.6725},
			CrossPegs: map[rates.Code]catalog.CrossPeg{"XOF": {Anchor: "EUR", Factor: 655.957}},
		}
	}
	if _, err := catalog.New(valid()); err != nil {
		t.Fatalf("valid data rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(d *catalog.Data)
	}{
		{"lower-case code", func(d *catalog.Data) { d.Codes = append(d.Codes, "gbp") }},
		{"duplicate code", func(d *catalog.Data) { d.Codes = append(d.Codes, "EUR") }},
		{"no base", func(d *catalog.Data) { d.Codes = d.Codes[:1] }},
		{"peg outside catalog", func(d *catalog.Data) { d.USDPegs["SAR"] = 3.75 }},
		{"pegged twice", func(d *catalog.Data) { d.USDPegs["XOF"] = 600 }},
		{"cross chain", func(d *catalog.Data) {
			d.CrossPegs["EUR"] = catalog.CrossPeg{Anchor: "AED", Factor: 4}
		}},
		{"zero factor", func(d *catalog.Data) { d.CrossPegs["XOF"] = catalog.CrossPeg{Anchor: "EUR"} }},
		{"multiRate outside catalog", func(d *catalog.Data) { d.MultiRate = []rates.Code{"VES"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := valid()
			tt.mutate(&d)
			if _, err := catalog.New(d); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestDataRoundTrip(t *testing.T) {
	cat, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cat.Data())
	if err != nil {
		t.Fatal(err)
	}
	var d catalog.Data
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	again, err := catalog.New(d)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Data(), cat.Data()) {
		t.Error("data changed in a round trip")
	}
}

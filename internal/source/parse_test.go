package source_test

import (
	"math"
	"testing"
	"time"

	"github.com/justskiv/rates/internal/fixture"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
)

func TestParseRecordedResponses(t *testing.T) {
	f, w, e := fixture.Quotes(t)
	day := func(s string) time.Time {
		d, _ := time.Parse(time.DateOnly, s)
		return d
	}
	tests := []struct {
		name  string
		q     source.Quote
		codes int
		date  time.Time
	}{
		{"frankfurter", f, 153, day("2026-11-01")},
		{"fawazahmed0", w, 153, day("2026-11-01")},
		{"exchangerate-api (synthetic)", e, 152, day("2026-11-01")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.q.Rates) != tt.codes {
				t.Errorf("%d codes, want %d", len(tt.q.Rates), tt.codes)
			}
			if !tt.q.Date.Equal(tt.date) {
				t.Errorf("date %s, want %s", tt.q.Date, tt.date)
			}
		})
	}
	if got := f.Dates["AUD"]; !got.Equal(day("2026-11-02")) {
		t.Errorf("AUD date %s, want 2026-11-02", got)
	}
	if w.Rates["SYP"] < 13000 {
		t.Errorf("fawazahmed0 SYP = %v, the recorded x100 error is gone", w.Rates["SYP"])
	}
	if e.Meta.EOL {
		t.Error("EOL set")
	}
}

// T20: codes outside the catalog are dropped.
func TestParseDropsNonCatalogCodes(t *testing.T) {
	cat := fixture.Catalog(t)
	q, err := source.ParseFrankfurter(fixture.Read(t, fixture.FrankfurterFile), cat.Contains)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []rates.Code{"BTC", "XAU", "ANG", "XDR", "CNH"} {
		if _, ok := q.Rates[code]; ok {
			t.Errorf("%s kept", code)
		}
	}
}

func TestParseFrankfurterStrict(t *testing.T) {
	keep := func(c rates.Code) bool { return c != "XAU" }
	tests := []struct {
		name    string
		give    string
		wantErr bool
	}{
		{"ok", `[{"date":"2026-11-01","base":"USD","quote":"EUR","rate":0.9}]`, false},
		{"extra fields allowed", `[{"date":"2026-11-01","base":"USD","quote":"EUR","rate":0.9,"x":1}]`, false},
		{"filtered code not checked", `[{"date":"bad","base":"USD","quote":"XAU","rate":-1}]`, false},
		{"non-ISO code ignored", `[{"date":"2026-11-01","base":"USD","quote":"1INCH","rate":0}]`, false},
		{"empty", `[]`, true},
		{"null", `null`, true},
		{"object", `{"rates":{}}`, true},
		{"missing rate", `[{"date":"2026-11-01","base":"USD","quote":"EUR"}]`, true},
		{"missing base", `[{"date":"2026-11-01","quote":"EUR","rate":0.9}]`, true},
		{"EUR base", `[{"date":"2026-11-01","base":"EUR","quote":"USD","rate":1.1}]`, true},
		{"base checked on filtered rows", `[{"date":"2026-11-01","base":"EUR","quote":"XAU","rate":1}]`, true},
		{"zero rate", `[{"date":"2026-11-01","base":"USD","quote":"EUR","rate":0}]`, true},
		{"huge rate", `[{"date":"2026-11-01","base":"USD","quote":"EUR","rate":1e9}]`, true},
		{"overflow", `[{"date":"2026-11-01","base":"USD","quote":"EUR","rate":1e999}]`, true},
		{"string rate", `[{"date":"2026-11-01","base":"USD","quote":"EUR","rate":"0.9"}]`, true},
		{"bad date", `[{"date":"01.11.2026","base":"USD","quote":"EUR","rate":0.9}]`, true},
		{"duplicate", `[{"date":"2026-11-01","base":"USD","quote":"EUR","rate":0.9},` +
			`{"date":"2026-11-01","base":"USD","quote":"eur","rate":0.9}]`, true},
		{"trailing garbage", `[]x`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := source.ParseFrankfurter([]byte(tt.give), keep)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseFawazStrict(t *testing.T) {
	tests := []struct {
		name    string
		give    string
		wantErr bool
	}{
		{"ok", `{"date":"2026-11-01","usd":{"eur":0.9,"btc":0.00001,"1inch":"x"}}`, false},
		{"EUR base", `{"date":"2026-11-01","eur":{"usd":1.1}}`, true},
		{"missing date", `{"usd":{"eur":0.9}}`, true},
		{"negative", `{"date":"2026-11-01","usd":{"eur":-0.9}}`, true},
		{"case duplicate", `{"date":"2026-11-01","usd":{"eur":0.9,"EUR":0.9}}`, true},
		{"string value", `{"date":"2026-11-01","usd":{"eur":"0.9"}}`, true},
	}
	keep := func(c rates.Code) bool { return c == "EUR" }
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := source.ParseFawaz([]byte(tt.give), keep)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseERAPIStrict(t *testing.T) {
	const ok = `{"result":"success","base_code":"USD","time_last_update_unix":1793491351,` +
		`"time_eol_unix":0,"rates":{"EUR":0.9}}`
	tests := []struct {
		name    string
		give    string
		wantErr bool
		wantEOL bool
	}{
		{"ok", ok, false, false},
		{"eol announced", `{"result":"success","base_code":"USD","time_last_update_unix":1793491351,` +
			`"time_eol_unix":1800000000,"rates":{"EUR":0.9}}`, false, true},
		{"error result", `{"result":"error","error-type":"unsupported-code"}`, true, false},
		{"EUR base", `{"result":"success","base_code":"EUR","time_last_update_unix":1,"rates":{}}`, true, false},
		{"no timestamp", `{"result":"success","base_code":"USD","rates":{"EUR":0.9}}`, true, false},
		{"no rates", `{"result":"success","base_code":"USD","time_last_update_unix":1}`, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, err := source.ParseERAPI([]byte(tt.give), nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if q.Meta.EOL != tt.wantEOL {
				t.Errorf("EOL = %v, want %v", q.Meta.EOL, tt.wantEOL)
			}
			if q.Body != nil {
				t.Error("the voter's body must never be retained")
			}
		})
	}
}

func checkParsed(t *testing.T, q source.Quote, err error) {
	t.Helper()
	if err != nil {
		return
	}
	for code, v := range q.Rates {
		if !rates.ValidCode(string(code)) {
			t.Fatalf("invalid code %q", code)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || !rates.ValidRate(v) {
			t.Fatalf("%s = %v", code, v)
		}
	}
	for code := range q.Dates {
		if _, ok := q.Rates[code]; !ok {
			t.Fatalf("date without rate for %s", code)
		}
	}
}

func FuzzFrankfurter(f *testing.F) {
	f.Add(fixture.Read(f, fixture.FrankfurterFile))
	f.Add([]byte(`[{"date":"2026-11-01","base":"USD","quote":"eur","rate":0.9}]`))
	f.Add([]byte(`[{"date":"2026-11-01","base":"USD","quote":"EUR","rate":-0}]`))
	f.Fuzz(func(t *testing.T, body []byte) {
		q, err := source.ParseFrankfurter(body, nil)
		checkParsed(t, q, err)
	})
}

func FuzzFawaz(f *testing.F) {
	f.Add(fixture.Read(f, fixture.FawazFile))
	f.Add([]byte(`{"date":"2026-11-01","usd":{"eur":0.9,"EUR":1}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		q, err := source.ParseFawaz(body, nil)
		checkParsed(t, q, err)
	})
}

func FuzzERAPI(f *testing.F) {
	f.Add(fixture.Read(f, fixture.ERAPIFile))
	f.Add([]byte(`{"result":"success","base_code":"USD","time_last_update_unix":1,"rates":{"eur":1e-5}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		q, err := source.ParseERAPI(body, nil)
		checkParsed(t, q, err)
		if q.Body != nil {
			t.Fatal("voter body retained")
		}
	})
}

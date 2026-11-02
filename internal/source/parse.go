package source

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/justskiv/rates/internal/rates"
)

// ParseFrankfurter parses the Frankfurter v2 array
// [{"date","base","quote","rate"}, ...]. Every row must carry all four
// fields and base USD; kept codes must have a valid rate and date, and may
// not repeat.
func ParseFrankfurter(body []byte, keep Filter) (Quote, error) {
	var rows []struct {
		Date  *string  `json:"date"`
		Base  *string  `json:"base"`
		Quote *string  `json:"quote"`
		Rate  *float64 `json:"rate"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return Quote{}, err
	}
	if len(rows) == 0 {
		return Quote{}, errors.New("no rows")
	}
	q := Quote{
		Dates: make(map[rates.Code]time.Time, len(rows)),
		Rates: make(map[rates.Code]float64, len(rows)),
	}
	for i, r := range rows {
		if r.Date == nil || r.Base == nil || r.Quote == nil || r.Rate == nil {
			return Quote{}, fmt.Errorf("row %d: missing field", i)
		}
		if *r.Base != string(rates.Base) {
			return Quote{}, fmt.Errorf("row %d: base %q", i, *r.Base)
		}
		code, ok := normalize(*r.Quote, keep)
		if !ok {
			continue
		}
		if _, dup := q.Rates[code]; dup {
			return Quote{}, fmt.Errorf("duplicate code %s", code)
		}
		if !rates.ValidRate(*r.Rate) {
			return Quote{}, fmt.Errorf("%s: rate %v out of range", code, *r.Rate)
		}
		d, err := time.Parse(time.DateOnly, *r.Date)
		if err != nil {
			return Quote{}, fmt.Errorf("%s: %w", code, err)
		}
		q.Rates[code] = *r.Rate
		q.Dates[code] = d
	}
	q.Date = modeDate(q.Dates)
	return q, nil
}

// ParseFawaz parses {"date": "...", "usd": {"eur": ...}}. Codes come in
// lower case and include crypto and withdrawn currencies, which the filter
// drops before their values are looked at.
func ParseFawaz(body []byte, keep Filter) (Quote, error) {
	var doc struct {
		Date *string                    `json:"date"`
		USD  map[string]json.RawMessage `json:"usd"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Quote{}, err
	}
	if doc.Date == nil {
		return Quote{}, errors.New("missing date")
	}
	if doc.USD == nil {
		return Quote{}, errors.New("missing usd table: base is not USD")
	}
	d, err := time.Parse(time.DateOnly, *doc.Date)
	if err != nil {
		return Quote{}, err
	}
	m, err := decodeRates(doc.USD, keep)
	if err != nil {
		return Quote{}, err
	}
	return Quote{Date: d, Rates: m}, nil
}

// ParseERAPI parses the open.er-api v6 document. The values serve only as
// a vote: callers must never persist or publish them.
func ParseERAPI(body []byte, keep Filter) (Quote, error) {
	var doc struct {
		Result     *string                    `json:"result"`
		BaseCode   *string                    `json:"base_code"`
		LastUpdate *int64                     `json:"time_last_update_unix"`
		EOL        *int64                     `json:"time_eol_unix"`
		Rates      map[string]json.RawMessage `json:"rates"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Quote{}, err
	}
	switch {
	case doc.Result == nil || *doc.Result != "success":
		return Quote{}, errors.New("result is not success")
	case doc.BaseCode == nil || *doc.BaseCode != string(rates.Base):
		return Quote{}, errors.New("base_code is not USD")
	case doc.LastUpdate == nil || *doc.LastUpdate <= 0:
		return Quote{}, errors.New("missing time_last_update_unix")
	case doc.Rates == nil:
		return Quote{}, errors.New("missing rates")
	}
	m, err := decodeRates(doc.Rates, keep)
	if err != nil {
		return Quote{}, err
	}
	updated := time.Unix(*doc.LastUpdate, 0).UTC()
	return Quote{
		Date:  updated.Truncate(24 * time.Hour),
		Rates: m,
		Meta:  Meta{EOL: doc.EOL != nil && *doc.EOL != 0},
	}, nil
}

func decodeRates(raw map[string]json.RawMessage, keep Filter) (map[rates.Code]float64, error) {
	m := make(map[rates.Code]float64, len(raw))
	for key, msg := range raw {
		code, ok := normalize(key, keep)
		if !ok {
			continue
		}
		if _, dup := m[code]; dup {
			return nil, fmt.Errorf("duplicate code %s", code)
		}
		var v float64
		if err := json.Unmarshal(msg, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", code, err)
		}
		if !rates.ValidRate(v) {
			return nil, fmt.Errorf("%s: rate %v out of range", code, v)
		}
		m[code] = v
	}
	return m, nil
}

// normalize upper-cases s and reports whether it is a kept ISO-shaped
// code. A nil filter keeps every ISO-shaped code.
func normalize(s string, keep Filter) (rates.Code, bool) {
	code := rates.Code(strings.ToUpper(s))
	if !rates.ValidCode(string(code)) || (keep != nil && !keep(code)) {
		return "", false
	}
	return code, true
}

// modeDate returns the most frequent date; ties go to the later date so
// the result does not depend on map order.
func modeDate(dates map[rates.Code]time.Time) time.Time {
	counts := make(map[time.Time]int, 4)
	for _, d := range dates {
		counts[d]++
	}
	var best time.Time
	bestN := 0
	for d, n := range counts {
		if n > bestN || (n == bestN && d.After(best)) {
			best, bestN = d, n
		}
	}
	return best
}

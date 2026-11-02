package merge

import (
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
)

// Screen applies the whole-source checks the parsers cannot do alone:
// catalog coverage, observation date window and systemic drift against
// prev. A failing source is set to nil and reported. Format and base
// checks happen in the parsers.
func Screen(in Inputs, prev rates.Snapshot, cfg Config, now time.Time) (Inputs, []Warning) {
	var warns []Warning
	check := func(q *source.Quote) *source.Quote {
		if q == nil {
			return nil
		}
		if w, ok := screenQuote(*q, cfg, now); !ok {
			warns = append(warns, w)
			return nil
		}
		return q
	}
	out := Inputs{
		Primary:   check(in.Primary),
		Secondary: check(in.Secondary),
		Voter:     check(in.Voter),
	}

	// A silent change of base (EUR data labelled USD) moves every value by
	// the same factor, which per-currency jump protection does not catch
	// everywhere. A real dollar move of that size shows up in both
	// publishable sources at once.
	if out.Primary != nil && out.Secondary != nil {
		pm, pDrift := drift(*out.Primary, prev, cfg)
		sm, sDrift := drift(*out.Secondary, prev, cfg)
		switch {
		case pDrift && !sDrift:
			warns = append(warns, driftWarning(source.Frankfurter, pm))
			out.Primary = nil
		case sDrift && !pDrift:
			warns = append(warns, driftWarning(source.Fawaz, sm))
			out.Secondary = nil
		}
	}
	return out, warns
}

func screenQuote(q source.Quote, cfg Config, now time.Time) (Warning, bool) {
	covered := 0
	for _, code := range cfg.Catalog.Codes() {
		if _, ok := q.Rates[code]; ok || code == rates.Base {
			covered++
		}
	}
	p := cfg.Policy
	if covered < p.SourceMinCoverage {
		return Warning{
			Kind:   WarnSourceLowCoverage,
			Source: q.Source,
			Detail: strconv.Itoa(covered) + " of " + strconv.Itoa(cfg.Catalog.Len()) + " catalog codes",
		}, false
	}
	today := now.UTC().Truncate(_day)
	date := q.Date.UTC().Truncate(_day)
	switch {
	case date.Sub(today) > p.SourceMaxFuture:
		return Warning{Kind: WarnSourceFutureDate, Source: q.Source, Detail: "date " + date.Format(time.DateOnly)}, false
	case today.Sub(date) > p.SourceMaxAge:
		return Warning{Kind: WarnSourceStaleDate, Source: q.Source, Detail: "date " + date.Format(time.DateOnly)}, false
	}
	return Warning{}, true
}

// drift returns the median ratio of q to prev over common codes and
// whether it exceeds the systemic drift threshold.
func drift(q source.Quote, prev rates.Snapshot, cfg Config) (float64, bool) {
	ratios := make([]float64, 0, len(q.Rates))
	for code, v := range q.Rates {
		p, ok := prev.Rates[code]
		if !ok || code == rates.Base || !cfg.Catalog.Contains(code) {
			continue
		}
		ratios = append(ratios, v/p)
	}
	if len(ratios) == 0 {
		return 1, false
	}
	slices.Sort(ratios)
	n := len(ratios)
	median := ratios[n/2]
	if n%2 == 0 {
		median = (ratios[n/2-1] + ratios[n/2]) / 2
	}
	return median, math.Abs(median-1) > cfg.Policy.SystemicDrift
}

func driftWarning(id source.ID, median float64) Warning {
	return Warning{
		Kind:   WarnSourceDrift,
		Source: id,
		Detail: "median ratio to previous " + strconv.FormatFloat(median, 'f', 4, 64),
	}
}

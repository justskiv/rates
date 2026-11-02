// Package validate checks snapshots and published trees with the same
// rules the iOS client applies, plus the producer's size budgets. It does
// no I/O beyond the fs.FS it is given.
package validate

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/justskiv/rates/internal/catalog"
	"github.com/justskiv/rates/internal/policy"
	"github.com/justskiv/rates/internal/rates"
)

// MaxWireBytes is the budget for any file a client downloads (the DPI
// threshold measured by devops).
const MaxWireBytes = 16 << 10

var _altLabel = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// Snapshot checks the time-independent rules of a snapshot.
func Snapshot(s rates.Snapshot, cat *catalog.Catalog, p policy.Policy) error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	if s.SchemaVersion != rates.SchemaVersion {
		add("schemaVersion %d", s.SchemaVersion)
	}
	if s.Base != rates.Base {
		add("base %q", s.Base)
	}
	if s.AsOf.IsZero() {
		add("asOf is missing")
	}
	if len(s.Source) > 64 {
		add("source longer than 64 bytes")
	}
	if v, ok := s.Rates[rates.Base]; !ok || v != 1 {
		add("rates[%s] must be 1", rates.Base)
	}
	if n := len(s.Rates); n < p.MinCoverage || n > cat.Len() {
		add("%d rates, want %d..%d", n, p.MinCoverage, cat.Len())
	}
	for _, code := range slices.Sorted(maps.Keys(s.Rates)) {
		if !cat.Contains(code) {
			add("rates: %q is not a catalog currency", code)
		}
		if v := s.Rates[code]; !rates.ValidRate(v) {
			add("rates[%s] = %v out of range", code, v)
		}
	}
	for _, code := range slices.Sorted(maps.Keys(s.Flags)) {
		errs = append(errs, checkFlags(code, s.Flags[code], s.Rates)...)
	}
	for _, code := range slices.Sorted(maps.Keys(s.Alt)) {
		a := s.Alt[code]
		if !cat.Contains(code) || !rates.ValidRate(a.Rate) || !_altLabel.MatchString(a.Label) {
			add("alt[%s] invalid", code)
		}
	}
	if err := checkEndpoints(s.Endpoints); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func checkFlags(code rates.Code, flags []rates.Flag, rs map[rates.Code]float64) []error {
	var errs []error
	if _, ok := rs[code]; !ok {
		errs = append(errs, fmt.Errorf("flags[%s]: currency has no rate", code))
	}
	if len(flags) == 0 {
		errs = append(errs, fmt.Errorf("flags[%s] is empty", code))
	}
	if !slices.IsSorted(flags) || len(slices.Compact(slices.Clone(flags))) != len(flags) {
		errs = append(errs, fmt.Errorf("flags[%s] not sorted and unique", code))
	}
	for _, f := range flags {
		if !rates.KnownFlag(f) {
			errs = append(errs, fmt.Errorf("flags[%s]: unknown flag %q", code, f))
		}
	}
	return errs
}

func checkEndpoints(eps []string) error {
	if len(eps) > 3 {
		return errors.New("more than 3 endpoints")
	}
	seen := make(map[string]bool, len(eps))
	for _, e := range eps {
		if !strings.HasPrefix(e, "https://") || len(e) > 200 || seen[e] {
			return fmt.Errorf("endpoint %q invalid", e)
		}
		seen[e] = true
	}
	return nil
}

// Fresh checks the time-dependent rules of a candidate: asOf no later than
// now plus MaxAsOfFuture, no older than MaxAsOfAge and never behind
// prevAsOf (zero disables that check).
func Fresh(s rates.Snapshot, prevAsOf time.Time, p policy.Policy, now time.Time) error {
	asOf := s.AsOf.Time
	switch {
	case asOf.After(now.Add(p.MaxAsOfFuture)):
		return fmt.Errorf("asOf %s is in the future", asOf.Format(time.RFC3339))
	case asOf.Before(now.Add(-p.MaxAsOfAge)):
		return fmt.Errorf("asOf %s is too old", asOf.Format(time.RFC3339))
	case !prevAsOf.IsZero() && asOf.Before(prevAsOf):
		return fmt.Errorf("asOf %s is behind the previous %s",
			asOf.Format(time.RFC3339), prevAsOf.Format(time.RFC3339))
	}
	return nil
}

// Encoded checks published bytes: size budget, canonical form (decoding
// and re-encoding gives the same bytes) and Snapshot.
func Encoded(b []byte, cat *catalog.Catalog, p policy.Policy) (rates.Snapshot, error) {
	if len(b) > p.MaxLatestBytes {
		return rates.Snapshot{}, fmt.Errorf("%d bytes, budget %d", len(b), p.MaxLatestBytes)
	}
	s, err := rates.Decode(b)
	if err != nil {
		return rates.Snapshot{}, err
	}
	again, err := rates.Encode(s)
	if err != nil {
		return rates.Snapshot{}, err
	}
	if !bytes.Equal(again, b) {
		return rates.Snapshot{}, errors.New("bytes are not in canonical form")
	}
	if err := Snapshot(s, cat, p); err != nil {
		return rates.Snapshot{}, err
	}
	return s, nil
}

// Package rates defines wire format v1 shared with the iOS app
// (contracts/rates.schema.json): codes, flags, timestamps and the snapshot
// document together with its canonical encoding.
package rates

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"
)

// SchemaVersion is the only wire format version this package produces.
const SchemaVersion = 1

// Base is the base currency of every snapshot in v1.
const Base Code = "USD"

// Bounds for a plausible rate in units per 1 USD. Both are exclusive. The
// encoder prints every float64 inside them without an exponent.
const (
	MinRate = 1e-4
	MaxRate = 1e8
)

// Code is an ISO 4217 alphabetic code in upper case.
type Code string

// ValidCode reports whether s is three ASCII upper-case letters.
func ValidCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := range len(s) {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// ValidRate reports whether v is finite and inside (MinRate, MaxRate).
func ValidRate(v float64) bool {
	return !math.IsNaN(v) && v > MinRate && v < MaxRate
}

// Round returns v rounded to digits significant decimal digits. The result
// is the float64 nearest to the shortest decimal with that many digits, so
// encoding it never prints more than digits significant digits.
func Round(v float64, digits int) float64 {
	r, err := strconv.ParseFloat(strconv.FormatFloat(v, 'g', digits, 64), 64)
	if err != nil {
		// FormatFloat output always parses; keep v rather than panic.
		return v
	}
	return r
}

// Flag is a per-currency quality flag; clients ignore unknown values.
type Flag string

// Known flags, see contracts/rates.schema.json.
const (
	FlagDisputed  Flag = "disputed"
	FlagHeld      Flag = "held"
	FlagMultiRate Flag = "multiRate"
	FlagOverride  Flag = "override"
	FlagPegged    Flag = "pegged"
	FlagSecondary Flag = "secondary"
	FlagStale     Flag = "stale"
)

// KnownFlag reports whether f is one of the flags defined in v1.
func KnownFlag(f Flag) bool {
	switch f {
	case FlagDisputed, FlagHeld, FlagMultiRate, FlagOverride, FlagPegged,
		FlagSecondary, FlagStale:
		return true
	default:
		return false
	}
}

// _timeLayout is the only accepted timestamp shape: UTC, second precision.
const _timeLayout = "2006-01-02T15:04:05Z"

// Time is a UTC instant encoded with second precision as
// "2006-01-02T15:04:05Z". The zero value encodes as absent with omitzero.
type Time struct{ time.Time }

// NewTime truncates t to whole seconds in UTC.
func NewTime(t time.Time) Time {
	return Time{t.UTC().Truncate(time.Second)}
}

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return nil, errors.New("rates: zero time is not encodable")
	}
	return []byte(`"` + t.UTC().Format(_timeLayout) + `"`), nil
}

// UnmarshalJSON implements json.Unmarshaler. Only the exact layout with a
// literal Z is accepted, matching the pattern in the JSON schema.
func (t *Time) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("rates: time: %w", err)
	}
	parsed, err := time.Parse(_timeLayout, s)
	if err != nil {
		return fmt.Errorf("rates: time %q: %w", s, err)
	}
	// time.Parse tolerates fractional seconds the layout does not mention.
	if parsed.Format(_timeLayout) != s {
		return fmt.Errorf("rates: time %q: want second precision", s)
	}
	t.Time = parsed
	return nil
}

// Alt is an alternative official rate (for example the CUP floating rate).
type Alt struct {
	Rate  float64 `json:"rate"`
	Label string  `json:"label"`
}

// Snapshot is one rates document: latest.json, a history day or the bundled
// snapshot.
type Snapshot struct {
	SchemaVersion int              `json:"schemaVersion"`
	Base          Code             `json:"base"`
	AsOf          Time             `json:"asOf"`
	GeneratedAt   Time             `json:"generatedAt,omitzero"`
	Source        string           `json:"source,omitempty"`
	Rates         map[Code]float64 `json:"rates"`
	Flags         map[Code][]Flag  `json:"flags,omitempty"`
	Alt           map[Code]Alt     `json:"alt,omitempty"`
	Endpoints     []string         `json:"endpoints,omitempty"`
	Revised       Time             `json:"revised,omitzero"`
}

// Primary reports whether the value for code counts as first-hand data:
// published and flagged neither secondary, stale nor held.
func (s Snapshot) Primary(code Code) bool {
	if _, ok := s.Rates[code]; !ok {
		return false
	}
	for _, f := range s.Flags[code] {
		if f == FlagSecondary || f == FlagStale || f == FlagHeld {
			return false
		}
	}
	return true
}

// PrimaryCount returns the number of codes for which Primary is true.
func (s Snapshot) PrimaryCount() int {
	n := 0
	for code := range s.Rates {
		if s.Primary(code) {
			n++
		}
	}
	return n
}

// HasFlag reports whether code carries flag f.
func (s Snapshot) HasFlag(code Code, f Flag) bool {
	return slices.Contains(s.Flags[code], f)
}

// Encode returns the canonical bytes of s: keys sorted, no insignificant
// whitespace, no HTML escaping, numbers in shortest decimal form and no
// trailing newline. Values must already be rounded by the producer.
func Encode(s Snapshot) ([]byte, error) {
	for code, flags := range s.Flags {
		if !slices.IsSorted(flags) {
			return nil, fmt.Errorf("rates: flags of %s are not sorted", code)
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, fmt.Errorf("rates: encode: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Decode parses a snapshot. It checks only the shape; semantic checks live
// in package validate.
func Decode(b []byte) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return Snapshot{}, fmt.Errorf("rates: decode: %w", err)
	}
	return s, nil
}

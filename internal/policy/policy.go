// Package policy holds the thresholds of the merge algorithm and of the
// whole-source checks. They are data under version control, compiled into
// the binary, not runtime config.
package policy

import (
	"time"

	"github.com/justskiv/rates/internal/rates"
)

// Policy is the full set of algorithm parameters.
type Policy struct {
	// Agree is the relative distance under which two values agree.
	Agree float64
	// PegTolerance is how far the primary may stray from a peg before the
	// peg is suspected to have broken.
	PegTolerance float64
	// Jump and JumpHighInflation bound the per-run move against the
	// previously published value.
	Jump              float64
	JumpHighInflation float64
	// ConfirmRuns is how many consecutive runs a new level must hold before
	// a jump is accepted.
	ConfirmRuns int
	// MaxHeld aborts the run when more currencies are held.
	MaxHeld int
	// MinCoverage is the minimum number of fresh published currencies.
	MinCoverage int
	// SigDigits rounds unpegged values; CrossPegDigits rounds cross pegs.
	SigDigits      int
	CrossPegDigits int
	// HighInflation lists currencies allowed the larger jump.
	HighInflation map[rates.Code]bool

	// SourceMinCoverage is the catalog coverage a source needs to be used.
	SourceMinCoverage int
	// SourceMaxAge and SourceMaxFuture bound a source's observation date.
	SourceMaxAge    time.Duration
	SourceMaxFuture time.Duration
	// SystemicDrift is the median move of a whole source against the
	// previous snapshot that disqualifies it when the other publishable
	// source stays calm.
	SystemicDrift float64
	// PrimaryCodeMaxLag drops a primary value whose own date lags the
	// primary's mode date by more than this.
	PrimaryCodeMaxLag time.Duration
	// DegradedDrop aborts a same-day candidate with this many fewer
	// first-hand values than the previous snapshot.
	DegradedDrop int

	// OverrideMaxAhead caps how far into the future an override may run.
	OverrideMaxAhead time.Duration
	// OverrideExpiresSoon raises attention before an override expires.
	OverrideExpiresSoon time.Duration
	// PrimaryDownAfter raises attention when the primary has been failing
	// this long.
	PrimaryDownAfter time.Duration

	// MaxLatestBytes is the hard limit for a snapshot before compression:
	// the 16 KB wire budget. TargetLatestBytes is the normal-case budget;
	// exceeding it only warns, because the fallback to the secondary flags
	// every value and legitimately grows the file to about 5.3 KB.
	MaxLatestBytes    int
	TargetLatestBytes int
	// MaxHealthBytes is the size budget of health.json.
	MaxHealthBytes int
	// MaxAsOfFuture and MaxAsOfAge bound a candidate's asOf against now.
	MaxAsOfFuture time.Duration
	MaxAsOfAge    time.Duration
}

// Default returns the production policy.
func Default() Policy {
	const day = 24 * time.Hour
	return Policy{
		Agree:             0.02,
		PegTolerance:      0.05,
		Jump:              0.15,
		JumpHighInflation: 0.50,
		ConfirmRuns:       3,
		MaxHeld:           10,
		MinCoverage:       140,
		SigDigits:         6,
		CrossPegDigits:    7,
		HighInflation: map[rates.Code]bool{
			"AOA": true, "ARS": true, "EGP": true, "ETB": true, "IRR": true,
			"LBP": true, "MMK": true, "MWK": true, "NGN": true, "SDG": true,
			"SSP": true, "SYP": true, "TRY": true, "VES": true, "ZWG": true,
		},

		SourceMinCoverage: 140,
		SourceMaxAge:      7 * day,
		SourceMaxFuture:   day,
		SystemicDrift:     0.03,
		PrimaryCodeMaxLag: 7 * day,
		DegradedDrop:      4,

		OverrideMaxAhead:    30 * day,
		OverrideExpiresSoon: 3 * day,
		PrimaryDownAfter:    6 * time.Hour,

		MaxLatestBytes:    16 << 10,
		TargetLatestBytes: 4096,
		MaxHealthBytes:    4096,
		MaxAsOfFuture:     time.Hour,
		MaxAsOfAge:        30 * day,
	}
}

// JumpLimit returns the allowed relative move for code.
func (p Policy) JumpLimit(code rates.Code) float64 {
	if p.HighInflation[code] {
		return p.JumpHighInflation
	}
	return p.Jump
}

// Agrees reports whether a and b are within Agree of each other, measured
// as |a/b - 1|.
func (p Policy) Agrees(a, b float64) bool {
	return RelDiff(a, b) <= p.Agree
}

// RelDiff returns |a/b - 1|.
func RelDiff(a, b float64) float64 {
	d := a/b - 1
	if d < 0 {
		return -d
	}
	return d
}

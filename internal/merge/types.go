// Package merge reconciles the upstream quotes into one snapshot: votes
// between sources, pegs, manual overrides, jump protection and catalog
// flags. It is pure: no I/O, no clock, the same arguments give the same
// result.
package merge

import (
	"time"

	"github.com/justskiv/rates/internal/catalog"
	"github.com/justskiv/rates/internal/policy"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
)

// SourceName is the value of the snapshot's source field.
const SourceName = "frankfurter-v2-blend"

// Inputs separates publishable quotes from the vote-only one, so the type
// system, not discipline, keeps exchangerate-api values out of the output.
// A nil quote means the source is unavailable for this run.
type Inputs struct {
	Primary   *source.Quote // Frankfurter: published
	Secondary *source.Quote // fawazahmed0: second vote and fallback value
	Voter     *source.Quote // exchangerate-api: agreement checks only
}

// Config bundles the static inputs of the algorithm.
type Config struct {
	Catalog *catalog.Catalog
	Policy  policy.Policy
}

// Origin records where a published value came from. There is deliberately
// no variant for the voter.
type Origin uint8

// Origins of published values.
const (
	OriginPrimary Origin = iota + 1
	OriginSecondary
	OriginPeg
	OriginCrossPeg
	OriginCarried // previous value kept: held or stale
	OriginOverride
	OriginBase // USD itself
)

func (o Origin) String() string {
	switch o {
	case OriginPrimary:
		return "primary"
	case OriginSecondary:
		return "secondary"
	case OriginPeg:
		return "peg"
	case OriginCrossPeg:
		return "crossPeg"
	case OriginCarried:
		return "carried"
	case OriginOverride:
		return "override"
	case OriginBase:
		return "base"
	default:
		return "unknown"
	}
}

// AbortReason explains why no candidate was produced. Empty means a
// candidate exists.
type AbortReason string

// Abort reasons of the merge step.
const (
	AbortLowCoverage    AbortReason = "low_coverage"
	AbortMassJump       AbortReason = "mass_jump"
	AbortAllSourcesDown AbortReason = "all_sources_down"
)

// WarningKind classifies a warning.
type WarningKind string

// Warning kinds produced by Screen and Merge.
const (
	WarnPrimaryDown         WarningKind = "primary_source_down"
	WarnPrimaryCodeStale    WarningKind = "primary_code_stale"
	WarnPegMismatch         WarningKind = "peg_mismatch"
	WarnAnchorMissing       WarningKind = "anchor_missing"
	WarnJumpHeld            WarningKind = "jump_held"
	WarnJumpConfirmed       WarningKind = "jump_confirmed"
	WarnOverrideActive      WarningKind = "override_active"
	WarnOverrideExpiresSoon WarningKind = "override_expires_soon"
	WarnOverrideRejected    WarningKind = "override_rejected"
	WarnOverrideExpired     WarningKind = "override_expired"
	WarnSourceLowCoverage   WarningKind = "source_low_coverage"
	WarnSourceStaleDate     WarningKind = "source_stale_date"
	WarnSourceFutureDate    WarningKind = "source_future_date"
	WarnSourceDrift         WarningKind = "source_systemic_drift"
)

// Warning is a notable event of a run. Details never contain values of the
// voter: they end up in public logs and health.json.
type Warning struct {
	Kind   WarningKind
	Code   rates.Code // empty for source-level warnings
	Source source.ID  // empty for currency-level warnings
	Detail string
	// Since is set for held currencies: when the pending level first appeared.
	Since time.Time
}

// Decision is the outcome for one currency.
type Decision struct {
	Code   rates.Code
	Value  float64
	Origin Origin
	Flags  []rates.Flag
	Note   string
}

// Result is the output of Merge.
type Result struct {
	Abort     AbortReason
	Snapshot  rates.Snapshot
	Decisions []Decision // one per published code, sorted by code
	Warnings  []Warning
}

// Pending tracks a currency whose new level jumped too far and waits for
// confirmation.
type Pending struct {
	Value float64    `json:"value"`
	Runs  int        `json:"runs"`
	Since rates.Time `json:"since"`
}

// State is the part of the run-to-run state owned by the algorithm.
type State struct {
	Pending map[rates.Code]Pending `json:"pending,omitempty"`
}

// Override is a manual value for one currency, active through Until
// (inclusive, UTC day).
type Override struct {
	Rate  float64
	Until time.Time
	Note  string
}

// Overrides maps currencies to manual values.
type Overrides map[rates.Code]Override

package merge

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/justskiv/rates/internal/policy"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
)

const _day = 24 * time.Hour

// Merge reconciles the sources into a candidate snapshot. prev is
// the last published snapshot (or the bundled one before the first
// publication) and serves as the jump baseline and the stale fallback.
// The returned State carries jump confirmation counters; it is meaningful
// even when the result is an abort, so that a real mass move can still be
// confirmed over the following runs.
func Merge(in Inputs, prev rates.Snapshot, st State, cfg Config, ov Overrides, now time.Time) (Result, State) {
	m := newMerger(in, prev, st, cfg, now)
	if in.Primary == nil && in.Secondary == nil {
		return Result{Abort: AbortAllSourcesDown}, st
	}
	if in.Primary == nil {
		m.warn(Warning{Kind: WarnPrimaryDown, Source: source.Frankfurter})
	}
	m.overrides = m.activeOverrides(ov)

	cat := cfg.Catalog
	for _, code := range cat.Codes() {
		if code == rates.Base {
			m.decide(Decision{Code: code, Value: 1, Origin: OriginBase})
			continue
		}
		if cat.Pegged(code) {
			continue
		}
		m.unpegged(code)
	}
	for _, code := range cat.Codes() {
		if v, ok := cat.USDPeg(code); ok {
			m.pegged(code, v, OriginPeg)
		}
	}
	for _, code := range cat.Codes() {
		p, ok := cat.CrossPegOf(code)
		if !ok {
			continue
		}
		anchor, ok := m.decisions[p.Anchor]
		if !ok {
			m.warn(Warning{Kind: WarnAnchorMissing, Code: code, Detail: "anchor " + string(p.Anchor)})
			m.carry(code)
			continue
		}
		m.pegged(code, rates.Round(p.Factor*anchor.Value, cfg.Policy.CrossPegDigits), OriginCrossPeg)
	}
	return m.finish()
}

type merger struct {
	cfg       Config
	pol       policy.Policy
	prev      rates.Snapshot
	now       time.Time
	primary   map[rates.Code]float64 // after dropping frozen codes
	secondary map[rates.Code]float64
	voter     map[rates.Code]float64
	in        Inputs
	pending   map[rates.Code]Pending
	overrides Overrides
	decisions map[rates.Code]Decision
	warnings  []Warning
}

func newMerger(in Inputs, prev rates.Snapshot, st State, cfg Config, now time.Time) *merger {
	m := &merger{
		cfg:       cfg,
		pol:       cfg.Policy,
		prev:      prev,
		now:       now,
		in:        in,
		pending:   maps.Clone(st.Pending),
		decisions: make(map[rates.Code]Decision, cfg.Catalog.Len()),
	}
	if m.pending == nil {
		m.pending = make(map[rates.Code]Pending)
	}
	if in.Secondary != nil {
		m.secondary = in.Secondary.Rates
	}
	if in.Voter != nil {
		m.voter = in.Voter.Rates
	}
	if in.Primary != nil {
		m.primary = m.freshPrimary(*in.Primary)
	}
	return m
}

// freshPrimary drops primary values whose own observation date lags the
// mode by more than PrimaryCodeMaxLag: an aggregator that froze one
// currency must not keep publishing it as fresh.
func (m *merger) freshPrimary(q source.Quote) map[rates.Code]float64 {
	out := make(map[rates.Code]float64, len(q.Rates))
	for code, v := range q.Rates {
		if d, ok := q.Dates[code]; ok && q.Date.Sub(d) > m.pol.PrimaryCodeMaxLag {
			m.warn(Warning{
				Kind:   WarnPrimaryCodeStale,
				Code:   code,
				Source: q.Source,
				Detail: "date " + d.Format(time.DateOnly) + ", mode " + q.Date.Format(time.DateOnly),
			})
			continue
		}
		out[code] = v
	}
	return out
}

// activeOverrides keeps overrides in force today and reports the others.
func (m *merger) activeOverrides(ov Overrides) Overrides {
	today := m.now.UTC().Truncate(_day)
	active := make(Overrides, len(ov))
	for _, code := range slices.Sorted(maps.Keys(ov)) {
		o := ov[code]
		until := o.Until.UTC().Truncate(_day)
		reject := func(why string) {
			m.warn(Warning{Kind: WarnOverrideRejected, Code: code, Detail: why})
		}
		switch {
		case !m.cfg.Catalog.Contains(code) || code == rates.Base:
			reject("not a catalog currency")
		case !rates.ValidRate(o.Rate):
			reject("rate out of range")
		case until.Before(today):
			m.warn(Warning{Kind: WarnOverrideExpired, Code: code, Detail: "until " + until.Format(time.DateOnly)})
		case until.Sub(today) > m.pol.OverrideMaxAhead:
			reject("until " + until.Format(time.DateOnly) + " is too far ahead")
		default:
			active[code] = o
			detail := "until " + until.Format(time.DateOnly)
			m.warn(Warning{Kind: WarnOverrideActive, Code: code, Detail: detail})
			if until.Sub(today) <= m.pol.OverrideExpiresSoon {
				m.warn(Warning{Kind: WarnOverrideExpiresSoon, Code: code, Detail: detail})
			}
		}
	}
	return active
}

// unpegged votes, applies an override and guards jumps for a floating
// currency.
func (m *merger) unpegged(code rates.Code) {
	if m.override(code) {
		return
	}
	d, ok := m.vote(code)
	if !ok {
		return
	}
	m.decide(m.guardJump(d))
}

// vote is step 1: pick a value by agreement between sources.
func (m *merger) vote(code rates.Code) (Decision, bool) {
	f, hasF := m.primary[code]
	w, hasW := m.secondary[code]
	e, hasE := m.voter[code]
	digits := m.pol.SigDigits
	d := Decision{Code: code}
	switch {
	case hasF && ((hasW && m.pol.Agrees(f, w)) || (hasE && m.pol.Agrees(f, e))):
		d.Value, d.Origin = rates.Round(f, digits), OriginPrimary
	case hasF && hasW && hasE && m.pol.Agrees(w, e):
		// The primary is the odd one out: publish the CC0 value.
		d.Value, d.Origin = rates.Round(w, digits), OriginSecondary
		d.Flags = []rates.Flag{rates.FlagDisputed}
		d.Note = "primary disagrees with both votes"
	case hasF:
		d.Value, d.Origin = rates.Round(f, digits), OriginPrimary
		if hasW || hasE {
			d.Flags = []rates.Flag{rates.FlagDisputed}
			d.Note = "no vote agrees with primary"
		}
	case hasW:
		d.Value, d.Origin = rates.Round(w, digits), OriginSecondary
		d.Flags = []rates.Flag{rates.FlagSecondary}
		if hasE && !m.pol.Agrees(w, e) {
			d.Flags = append(d.Flags, rates.FlagDisputed)
		}
		d.Note = "primary has no value"
	default:
		// Values of the voter are never published, not even as a fallback.
		return m.stale(code)
	}
	return d, true
}

// stale carries the previous value forward, if there is one.
func (m *merger) stale(code rates.Code) (Decision, bool) {
	v, ok := m.prev.Rates[code]
	if !ok {
		return Decision{}, false
	}
	return Decision{
		Code:   code,
		Value:  v,
		Origin: OriginCarried,
		Flags:  []rates.Flag{rates.FlagStale},
		Note:   "no publishable source",
	}, true
}

func (m *merger) carry(code rates.Code) {
	if d, ok := m.stale(code); ok {
		m.decide(d)
	}
}

// pegged is step 2 for a currency with a peg value (USD or cross).
func (m *merger) pegged(code rates.Code, peg float64, origin Origin) {
	if m.override(code) {
		return
	}
	d := Decision{Code: code, Value: peg, Origin: origin, Flags: []rates.Flag{rates.FlagPegged}}
	f, hasF := m.primary[code]
	if !hasF || policy.RelDiff(f, peg) <= m.pol.PegTolerance {
		delete(m.pending, code)
		m.decide(d)
		return
	}
	// The primary strays from the peg. Publish it only when a second
	// voice confirms it: one aggregator glitch must not unpeg a currency.
	w, hasW := m.secondary[code]
	e, hasE := m.voter[code]
	confirmed := (hasW && m.pol.Agrees(f, w)) || (hasE && m.pol.Agrees(f, e))
	detail := "primary " + formatFloat(f) + ", peg " + formatFloat(peg)
	if !confirmed {
		m.warn(Warning{Kind: WarnPegMismatch, Code: code, Detail: detail + ", unconfirmed, peg kept"})
		delete(m.pending, code)
		m.decide(d)
		return
	}
	m.warn(Warning{Kind: WarnPegMismatch, Code: code, Detail: detail + ", confirmed, primary published"})
	m.decide(m.guardJump(Decision{
		Code:   code,
		Value:  rates.Round(f, m.pol.SigDigits),
		Origin: OriginPrimary,
		Flags:  []rates.Flag{rates.FlagDisputed},
		Note:   "peg broken: " + detail,
	}))
}

// override is step 3. It reports whether an override decided the code.
func (m *merger) override(code rates.Code) bool {
	o, ok := m.overrides[code]
	if !ok {
		return false
	}
	delete(m.pending, code)
	m.decide(Decision{
		Code:   code,
		Value:  o.Rate,
		Origin: OriginOverride,
		Flags:  []rates.Flag{rates.FlagOverride},
		Note:   o.Note,
	})
	return true
}

// guardJump is step 4: hold a value that moved too far from the previous
// one until the new level is confirmed.
func (m *merger) guardJump(d Decision) Decision {
	code := d.Code
	prev, ok := m.prev.Rates[code]
	if !ok || d.Origin == OriginCarried {
		delete(m.pending, code)
		return d
	}
	jump := policy.RelDiff(d.Value, prev)
	if jump <= m.pol.JumpLimit(code) {
		delete(m.pending, code)
		return d
	}

	p, ok := m.pending[code]
	if ok && m.pol.Agrees(d.Value, p.Value) {
		p.Runs++
	} else {
		p = Pending{Value: d.Value, Runs: 1, Since: rates.NewTime(m.now)}
	}
	need, confirmable := m.confirmRuns(code)
	detail := "prev " + formatFloat(prev) + ", new " + formatFloat(d.Value) +
		", delta " + strconv.FormatFloat(jump, 'f', 3, 64) +
		", runs " + strconv.Itoa(p.Runs) + "/" + strconv.Itoa(need)
	if confirmable && p.Runs >= need {
		delete(m.pending, code)
		m.warn(Warning{Kind: WarnJumpConfirmed, Code: code, Detail: detail})
		d.Note = "jump confirmed"
		return d
	}
	m.pending[code] = p
	m.warn(Warning{Kind: WarnJumpHeld, Code: code, Detail: detail, Since: p.Since.Time})
	return Decision{
		Code:   code,
		Value:  prev,
		Origin: OriginCarried,
		Flags:  []rates.Flag{rates.FlagHeld},
		Note:   "jump held: " + detail,
	}
}

// confirmRuns returns how many consecutive runs confirm a jump of code and
// whether this run can count as confirmation at all: the primary and the
// secondary must agree at the new level; with only one of them present the
// requirement doubles.
func (m *merger) confirmRuns(code rates.Code) (int, bool) {
	f, hasF := m.primary[code]
	w, hasW := m.secondary[code]
	switch {
	case hasF && hasW:
		return m.pol.ConfirmRuns, m.pol.Agrees(f, w)
	default:
		return 2 * m.pol.ConfirmRuns, true
	}
}

func (m *merger) decide(d Decision) {
	if m.cfg.Catalog.MultiRate(d.Code) {
		d.Flags = append(d.Flags, rates.FlagMultiRate)
	}
	slices.Sort(d.Flags)
	d.Flags = slices.Compact(d.Flags)
	m.decisions[d.Code] = d
}

func (m *merger) warn(w Warning) {
	m.warnings = append(m.warnings, w)
}

// finish applies the publication checks of step 6 and builds the snapshot.
func (m *merger) finish() (Result, State) {
	res := Result{
		Decisions: make([]Decision, 0, len(m.decisions)),
	}
	held, fresh := 0, 0
	snap := rates.Snapshot{
		SchemaVersion: rates.SchemaVersion,
		Base:          rates.Base,
		Source:        SourceName,
		Rates:         make(map[rates.Code]float64, len(m.decisions)),
	}
	for _, code := range slices.Sorted(maps.Keys(m.decisions)) {
		d := m.decisions[code]
		res.Decisions = append(res.Decisions, d)
		snap.Rates[code] = d.Value
		if len(d.Flags) > 0 {
			if snap.Flags == nil {
				snap.Flags = make(map[rates.Code][]rates.Flag)
			}
			snap.Flags[code] = slices.Clone(d.Flags)
		}
		if slices.Contains(d.Flags, rates.FlagHeld) {
			held++
		}
		if !slices.Contains(d.Flags, rates.FlagStale) {
			fresh++
		}
	}
	snap.AsOf = rates.NewTime(m.asOf())
	res.Snapshot = snap
	res.Warnings = m.sortedWarnings()
	st := State{Pending: m.pending}
	if len(st.Pending) == 0 {
		st.Pending = nil
	}

	switch {
	case held > m.pol.MaxHeld:
		res.Abort = AbortMassJump
	case fresh < m.pol.MinCoverage:
		res.Abort = AbortLowCoverage
	}
	return res, st
}

// asOf is the most common observation date among values taken from the
// primary; without any, the secondary's date. It never goes back behind
// the previous snapshot.
func (m *merger) asOf() time.Time {
	var asOf time.Time
	if m.in.Primary != nil {
		counts := make(map[time.Time]int)
		for code, d := range m.decisions {
			if d.Origin != OriginPrimary {
				continue
			}
			if day, ok := m.in.Primary.Dates[code]; ok {
				counts[day]++
			} else {
				counts[m.in.Primary.Date]++
			}
		}
		best := 0
		for day, n := range counts {
			if n > best || (n == best && day.After(asOf)) {
				asOf, best = day, n
			}
		}
	}
	if asOf.IsZero() && m.in.Secondary != nil {
		asOf = m.in.Secondary.Date
	}
	if prev := m.prev.AsOf.Time; asOf.IsZero() || asOf.Before(prev) {
		asOf = prev
	}
	return asOf.UTC().Truncate(_day)
}

func (m *merger) sortedWarnings() []Warning {
	ws := slices.Clone(m.warnings)
	slices.SortStableFunc(ws, func(a, b Warning) int {
		return cmp.Or(
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.Code, b.Code),
		)
	})
	return ws
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', 7, 64)
}

// String renders a decision for logs and golden reports.
func (d Decision) String() string {
	return fmt.Sprintf("%s %s %s %v %s", d.Code, formatFloat(d.Value), d.Origin, d.Flags, d.Note)
}

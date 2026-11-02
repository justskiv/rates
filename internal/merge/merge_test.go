package merge_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justskiv/rates/internal/catalog"
	"github.com/justskiv/rates/internal/fixture"
	"github.com/justskiv/rates/internal/merge"
	"github.com/justskiv/rates/internal/policy"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
	"github.com/justskiv/rates/internal/validate"
)

// env is the default world of a merge test: the recorded sources of
// 2026-11-01 and the bundled snapshot as the previous one.
type env struct {
	f, w, e source.Quote
	prev    rates.Snapshot
	st      merge.State
	ov      merge.Overrides
	cfg     merge.Config
	now     time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f, w, e := fixture.Quotes(t)
	prev, err := catalog.Bundled()
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		f: f, w: w, e: e,
		prev: prev,
		cfg:  merge.Config{Catalog: fixture.Catalog(t), Policy: policy.Default()},
		now:  fixture.Now,
	}
}

func (e *env) inputs() merge.Inputs {
	f, w, v := e.f.Clone(), e.w.Clone(), e.e.Clone()
	return merge.Inputs{Primary: &f, Secondary: &w, Voter: &v}
}

func (e *env) merge(t *testing.T, in merge.Inputs) merge.Result {
	t.Helper()
	res, st := merge.Merge(in, e.prev, e.st, e.cfg, e.ov, e.now)
	e.st = st
	return res
}

func decision(t *testing.T, res merge.Result, code rates.Code) merge.Decision {
	t.Helper()
	for _, d := range res.Decisions {
		if d.Code == code {
			return d
		}
	}
	t.Fatalf("no decision for %s", code)
	return merge.Decision{}
}

func hasWarning(res merge.Result, kind merge.WarningKind, code rates.Code) bool {
	return slices.ContainsFunc(res.Warnings, func(w merge.Warning) bool {
		return w.Kind == kind && w.Code == code
	})
}

func flags(fs ...rates.Flag) []rates.Flag { return fs }

// T1: the real inputs give a full, valid, small snapshot.
func TestMergeRealInputs(t *testing.T) {
	e := newEnv(t)
	res := e.merge(t, e.inputs())
	if res.Abort != "" {
		t.Fatalf("abort %s", res.Abort)
	}
	if got := len(res.Snapshot.Rates); got != 153 {
		t.Errorf("published %d currencies, want 153", got)
	}
	if err := validate.Snapshot(res.Snapshot, e.cfg.Catalog, e.cfg.Policy); err != nil {
		t.Errorf("snapshot invalid: %v", err)
	}
	res.Snapshot.GeneratedAt = rates.NewTime(e.now)
	b, err := rates.Encode(res.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 4096 {
		t.Errorf("latest.json is %d bytes, budget 4096", len(b))
	}
	if want := "2026-11-01T00:00:00Z"; res.Snapshot.AsOf.Format(time.RFC3339) != want {
		t.Errorf("asOf %s, want %s", res.Snapshot.AsOf.Format(time.RFC3339), want)
	}
}

// T2-T5, T7: per-currency outcomes on the real inputs.
func TestMergeRealDecisions(t *testing.T) {
	e := newEnv(t)
	res := e.merge(t, e.inputs())
	tests := []struct {
		name   string
		code   rates.Code
		value  float64
		origin merge.Origin
		flags  []rates.Flag
	}{
		{"T2 SYP: W x100, F agrees with E", "SYP", 121.78, merge.OriginPrimary, nil},
		{"T3 SDG: E off, F agrees with W", "SDG", 597.38, merge.OriginPrimary, flags(rates.FlagMultiRate)},
		{"T3 IRR: E off, F agrees with W", "IRR", 1865170, merge.OriginPrimary, flags(rates.FlagMultiRate)},
		{"T4 BWP: W and E agree against F", "BWP", 13.9262, merge.OriginSecondary, flags(rates.FlagDisputed)},
		{"T5 BBD: blend noise under the peg", "BBD", 2, merge.OriginPeg, flags(rates.FlagPegged)},
		{"T7 XOF: cross peg via EUR", "XOF", rates.Round(655.957*0.89478, 7), merge.OriginCrossPeg, flags(rates.FlagPegged)},
		{"CUP: peg plus catalog flag", "CUP", 24, merge.OriginPeg, flags(rates.FlagMultiRate, rates.FlagPegged)},
		{"KPW: W x7, no E", "KPW", 130.02, merge.OriginPrimary, flags(rates.FlagDisputed)},
		{"USD is the base", "USD", 1, merge.OriginBase, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := decision(t, res, tt.code)
			if d.Value != tt.value || d.Origin != tt.origin || !slices.Equal(d.Flags, tt.flags) {
				t.Errorf("got %v %v %v, want %v %v %v",
					d.Value, d.Origin, d.Flags, tt.value, tt.origin, tt.flags)
			}
		})
	}
}

// T6: the primary leaves the peg and the secondary confirms: unpeg.
func TestMergePegBrokenConfirmed(t *testing.T) {
	e := newEnv(t)
	e.f.Rates["AED"], e.w.Rates["AED"] = 3.90, 3.91
	res := e.merge(t, e.inputs())
	d := decision(t, res, "AED")
	if d.Value != 3.9 || !slices.Equal(d.Flags, flags(rates.FlagDisputed)) {
		t.Errorf("AED = %v %v, want 3.9 [disputed]", d.Value, d.Flags)
	}
	if !hasWarning(res, merge.WarnPegMismatch, "AED") {
		t.Error("no peg_mismatch warning")
	}
}

// T23: the primary alone leaves the peg: keep the peg, warn.
func TestMergePegBrokenUnconfirmed(t *testing.T) {
	e := newEnv(t)
	e.f.Rates["AED"] = 3.90
	res := e.merge(t, e.inputs())
	d := decision(t, res, "AED")
	if d.Value != 3.6725 || !slices.Equal(d.Flags, flags(rates.FlagPegged)) {
		t.Errorf("AED = %v %v, want 3.6725 [pegged]", d.Value, d.Flags)
	}
	if !hasWarning(res, merge.WarnPegMismatch, "AED") {
		t.Error("no peg_mismatch warning")
	}
}

// T8, T9: fallbacks when the primary or both publishable sources lack a code.
func TestMergeFallbacks(t *testing.T) {
	e := newEnv(t)
	delete(e.f.Rates, "THB")
	delete(e.f.Rates, "GEL")
	delete(e.w.Rates, "GEL")
	e.e.Rates["GEL"] = 9.99 // must never surface
	res := e.merge(t, e.inputs())

	thb := decision(t, res, "THB")
	if thb.Origin != merge.OriginSecondary || thb.Value != rates.Round(e.w.Rates["THB"], 6) ||
		!slices.Equal(thb.Flags, flags(rates.FlagSecondary)) {
		t.Errorf("T8 THB = %v", thb)
	}
	gel := decision(t, res, "GEL")
	if gel.Origin != merge.OriginCarried || gel.Value != e.prev.Rates["GEL"] ||
		!slices.Equal(gel.Flags, flags(rates.FlagStale)) {
		t.Errorf("T9 GEL = %v", gel)
	}
}

// T10: a jump is held until the new level holds CONFIRM_RUNS runs with the
// primary and the secondary agreeing.
func TestMergeJumpConfirmation(t *testing.T) {
	e := newEnv(t)
	prevKZT := e.prev.Rates["KZT"]
	jumped := prevKZT * 1.2
	e.f.Rates["KZT"], e.w.Rates["KZT"], e.e.Rates["KZT"] = jumped, jumped*1.001, jumped
	for run := 1; run <= 3; run++ {
		res := e.merge(t, e.inputs())
		d := decision(t, res, "KZT")
		if run < 3 {
			if d.Value != prevKZT || !slices.Equal(d.Flags, flags(rates.FlagHeld)) {
				t.Fatalf("run %d: KZT = %v, want held at %v", run, d, prevKZT)
			}
			if e.st.Pending["KZT"].Runs != run {
				t.Fatalf("run %d: pending runs %d", run, e.st.Pending["KZT"].Runs)
			}
			continue
		}
		if d.Value != rates.Round(jumped, 6) || len(d.Flags) != 0 {
			t.Fatalf("run 3: KZT = %v, want accepted %v", d, rates.Round(jumped, 6))
		}
		if !hasWarning(res, merge.WarnJumpConfirmed, "KZT") {
			t.Error("no jump_confirmed warning")
		}
		if _, ok := e.st.Pending["KZT"]; ok {
			t.Error("pending kept after confirmation")
		}
	}
}

// With the secondary missing the confirmation takes twice as many runs;
// with the two disagreeing it never comes.
func TestMergeJumpConfirmationNeedsAgreement(t *testing.T) {
	tests := []struct {
		name     string
		w        func(e *env, jumped float64)
		runs     int
		accepted bool
	}{
		{"no secondary: 2x runs", func(e *env, _ float64) { delete(e.w.Rates, "KZT") }, 6, true},
		{"no secondary: not yet", func(e *env, _ float64) { delete(e.w.Rates, "KZT") }, 5, false},
		{"secondary disagrees", func(e *env, _ float64) {}, 10, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			jumped := e.prev.Rates["KZT"] * 1.2
			e.f.Rates["KZT"], e.e.Rates["KZT"] = jumped, jumped
			tt.w(e, jumped)
			var d merge.Decision
			for range tt.runs {
				d = decision(t, e.merge(t, e.inputs()), "KZT")
			}
			if accepted := d.Origin != merge.OriginCarried; accepted != tt.accepted {
				t.Errorf("after %d runs accepted=%v, want %v (%v)", tt.runs, accepted, tt.accepted, d)
			}
		})
	}
}

// T11: high-inflation currencies may move up to 50% at once.
func TestMergeHighInflationJump(t *testing.T) {
	e := newEnv(t)
	v := e.prev.Rates["ARS"] * 1.4
	e.f.Rates["ARS"], e.w.Rates["ARS"] = v, v
	d := decision(t, e.merge(t, e.inputs()), "ARS")
	if d.Value != rates.Round(v, 6) {
		t.Errorf("ARS = %v, want accepted %v", d, rates.Round(v, 6))
	}
}

// T12: more than MAX_HELD jumps abort the run.
func TestMergeMassJump(t *testing.T) {
	e := newEnv(t)
	for _, code := range []rates.Code{"AUD", "BRL", "CAD", "CHF", "CNY", "CZK", "DKK", "GBP", "HKD", "HUF", "ILS"} {
		e.f.Rates[code] *= 1.3
		e.w.Rates[code] *= 1.3
	}
	res := e.merge(t, e.inputs())
	if res.Abort != merge.AbortMassJump {
		t.Errorf("abort %q, want %q", res.Abort, merge.AbortMassJump)
	}
	if len(e.st.Pending) != 11 {
		t.Errorf("pending %d, want 11 kept for confirmation", len(e.st.Pending))
	}
}

// T13: too few fresh values abort; T14: no publishable source aborts.
func TestMergeCoverageAborts(t *testing.T) {
	e := newEnv(t)
	trimmed := e.w.Clone()
	codes := e.cfg.Catalog.Codes()
	for _, code := range codes[120:] {
		delete(trimmed.Rates, code)
	}
	res := e.merge(t, merge.Inputs{Secondary: &trimmed})
	if res.Abort != merge.AbortLowCoverage {
		t.Errorf("T13 abort %q, want %q", res.Abort, merge.AbortLowCoverage)
	}
	if !hasWarning(res, merge.WarnPrimaryDown, "") {
		t.Error("T13 no primary_source_down warning")
	}

	v := e.e.Clone()
	res = e.merge(t, merge.Inputs{Voter: &v})
	if res.Abort != merge.AbortAllSourcesDown {
		t.Errorf("T14 abort %q, want %q", res.Abort, merge.AbortAllSourcesDown)
	}
}

// T15, T16 and the override horizon.
func TestMergeOverrides(t *testing.T) {
	day := func(s string) time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	tests := []struct {
		name     string
		ov       merge.Override
		active   bool
		warnings []merge.WarningKind
	}{
		{"T15 active", merge.Override{Rate: 121.8, Until: day("2026-11-20")}, true,
			[]merge.WarningKind{merge.WarnOverrideActive}},
		{"active, expires soon", merge.Override{Rate: 121.8, Until: day("2026-11-03")}, true,
			[]merge.WarningKind{merge.WarnOverrideActive, merge.WarnOverrideExpiresSoon}},
		{"active through until, inclusive", merge.Override{Rate: 121.8, Until: day("2026-11-01")}, true,
			[]merge.WarningKind{merge.WarnOverrideActive, merge.WarnOverrideExpiresSoon}},
		{"T16 expired", merge.Override{Rate: 121.8, Until: day("2026-10-31")}, false,
			[]merge.WarningKind{merge.WarnOverrideExpired}},
		{"too far ahead", merge.Override{Rate: 121.8, Until: day("2026-12-31")}, false,
			[]merge.WarningKind{merge.WarnOverrideRejected}},
		{"invalid rate", merge.Override{Rate: -1, Until: day("2026-11-20")}, false,
			[]merge.WarningKind{merge.WarnOverrideRejected}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.ov = merge.Overrides{"SYP": tt.ov}
			res := e.merge(t, e.inputs())
			d := decision(t, res, "SYP")
			if active := d.Origin == merge.OriginOverride; active != tt.active {
				t.Fatalf("active=%v, want %v (%v)", active, tt.active, d)
			}
			if tt.active && (d.Value != 121.8 || !slices.Equal(d.Flags, flags(rates.FlagOverride))) {
				t.Errorf("SYP = %v", d)
			}
			var got []merge.WarningKind
			for _, w := range res.Warnings {
				if w.Code == "SYP" {
					got = append(got, w.Kind)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.warnings) {
				t.Errorf("warnings %v, want %v", got, tt.warnings)
			}
		})
	}
}

// An override on an anchor moves its cross pegs with it.
func TestMergeOverrideOnAnchor(t *testing.T) {
	e := newEnv(t)
	e.ov = merge.Overrides{"EUR": {Rate: 0.9, Until: e.now}}
	res := e.merge(t, e.inputs())
	if d := decision(t, res, "XOF"); d.Value != rates.Round(655.957*0.9, 7) {
		t.Errorf("XOF = %v, want 655.957 x overridden EUR", d)
	}
}

// T17: the same inputs give the same bytes.
func TestMergeDeterministic(t *testing.T) {
	e := newEnv(t)
	encode := func() string {
		res, _ := merge.Merge(e.inputs(), e.prev, merge.State{}, e.cfg, nil, e.now)
		b, err := rates.Encode(res.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	first := encode()
	for range 20 {
		if got := encode(); got != first {
			t.Fatal("two merges of the same inputs differ")
		}
	}
}

// T18: asOf never goes back behind the previous snapshot.
func TestMergeAsOfMonotonic(t *testing.T) {
	e := newEnv(t)
	e.prev.AsOf = rates.NewTime(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	res := e.merge(t, e.inputs())
	if got := res.Snapshot.AsOf.Format(time.DateOnly); got != "2026-11-02" {
		t.Errorf("asOf %s, want 2026-11-02", got)
	}
}

// Without the primary asOf comes from the secondary.
func TestMergeAsOfFromSecondary(t *testing.T) {
	e := newEnv(t)
	e.prev.AsOf = rates.NewTime(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	w := e.w.Clone()
	res := e.merge(t, merge.Inputs{Secondary: &w})
	if got := res.Snapshot.AsOf.Format(time.DateOnly); got != "2026-11-01" {
		t.Errorf("asOf %s, want the secondary's 2026-11-01", got)
	}
	if d := decision(t, res, "EUR"); !slices.Equal(d.Flags, flags(rates.FlagSecondary)) {
		t.Errorf("EUR = %v, want secondary", d)
	}
}

// T25: a currency frozen inside the primary counts as missing there.
func TestMergePrimaryFrozenCurrency(t *testing.T) {
	e := newEnv(t)
	e.f.Dates["GEL"] = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	res := e.merge(t, e.inputs())
	d := decision(t, res, "GEL")
	if d.Origin != merge.OriginSecondary || !slices.Equal(d.Flags, flags(rates.FlagSecondary)) {
		t.Errorf("GEL = %v, want the secondary", d)
	}
	if !hasWarning(res, merge.WarnPrimaryCodeStale, "GEL") {
		t.Error("no primary_code_stale warning")
	}
}

// T24: no published value equals a value of the voter.
func TestMergeVoterNeverPublished(t *testing.T) {
	e := newEnv(t)
	// Make the voter the only source for one code: it must still not appear.
	delete(e.f.Rates, "MNT")
	delete(e.w.Rates, "MNT")
	delete(e.prev.Rates, "MNT")
	res := e.merge(t, e.inputs())
	if _, ok := res.Snapshot.Rates["MNT"]; ok {
		t.Error("MNT published from the voter alone")
	}
	assertNoVoterValues(t, res, e.e)
}

func assertNoVoterValues(t *testing.T, res merge.Result, voter source.Quote) {
	t.Helper()
	for code, v := range res.Snapshot.Rates {
		ev, ok := voter.Rates[code]
		if !ok || code == rates.Base {
			continue
		}
		for _, digits := range []int{6, 7, 15} {
			if v == rates.Round(ev, digits) {
				t.Errorf("%s = %v equals the voter's value", code, v)
			}
		}
	}
	b, err := rates.Encode(res.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for code, ev := range voter.Rates {
		if code == rates.Base {
			continue
		}
		// encoding/json prints floats in this range with 'f' and -1.
		needle := `"` + string(code) + `":` + strconv.FormatFloat(ev, 'f', -1, 64)
		if strings.Contains(string(b), needle+",") || strings.Contains(string(b), needle+"}") {
			t.Errorf("voter value %s found in the output", needle)
		}
	}
}

package merge_test

import (
	"math/rand/v2"
	"testing"

	"github.com/justskiv/rates/internal/merge"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
	"github.com/justskiv/rates/internal/validate"
)

// FuzzMerge perturbs the recorded inputs at random (drop codes, scale
// values, drop whole sources) and checks the invariants of the algorithm:
// every published value is traceable to the primary, the secondary, a
// peg, a cross peg of the published anchor or the previous snapshot, and
// never to the voter; a result without abort passes validation.
func FuzzMerge(f *testing.F) {
	for seed := range uint64(8) {
		f.Add(seed, uint8(10), uint8(0))
	}
	f.Add(uint64(42), uint8(200), uint8(7))
	f.Fuzz(func(t *testing.T, seed uint64, intensity, dropSources uint8) {
		e := newEnv(t)
		rng := rand.New(rand.NewPCG(seed, uint64(intensity)))
		perturb := func(q *source.Quote) {
			for code := range q.Rates {
				switch r := rng.IntN(256); {
				case r < int(intensity)/4:
					delete(q.Rates, code)
				case r < int(intensity)/2:
					q.Rates[code] *= 0.5 + 1.5*rng.Float64()
				}
			}
		}
		perturb(&e.f)
		perturb(&e.w)
		perturb(&e.e)
		in := e.inputs()
		if dropSources&1 != 0 {
			in.Primary = nil
		}
		if dropSources&2 != 0 {
			in.Secondary = nil
		}
		if dropSources&4 != 0 {
			in.Voter = nil
		}

		res := e.merge(t, in)
		if res.Abort != "" {
			return
		}
		if err := validate.Snapshot(res.Snapshot, e.cfg.Catalog, e.cfg.Policy); err != nil {
			t.Fatalf("snapshot invalid: %v", err)
		}
		for code, v := range res.Snapshot.Rates {
			if !traceable(e, in, res, code, v) {
				t.Fatalf("%s = %v is not traceable to a publishable origin", code, v)
			}
		}
	})
}

func traceable(e *env, in merge.Inputs, res merge.Result, code rates.Code, v float64) bool {
	pol, cat := e.cfg.Policy, e.cfg.Catalog
	if code == rates.Base {
		return v == 1
	}
	if in.Primary != nil {
		if f, ok := in.Primary.Rates[code]; ok && v == rates.Round(f, pol.SigDigits) {
			return true
		}
	}
	if in.Secondary != nil {
		if w, ok := in.Secondary.Rates[code]; ok && v == rates.Round(w, pol.SigDigits) {
			return true
		}
	}
	if p, ok := cat.USDPeg(code); ok && v == p {
		return true
	}
	if p, ok := cat.CrossPegOf(code); ok {
		if a, ok := res.Snapshot.Rates[p.Anchor]; ok && v == rates.Round(p.Factor*a, pol.CrossPegDigits) {
			return true
		}
	}
	return v == e.prev.Rates[code]
}

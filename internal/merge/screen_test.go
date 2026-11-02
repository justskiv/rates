package merge_test

import (
	"testing"
	"time"

	"github.com/justskiv/rates/internal/merge"
	"github.com/justskiv/rates/internal/rates"
	"github.com/justskiv/rates/internal/source"
)

func TestScreen(t *testing.T) {
	scale := func(q *source.Quote, k float64) {
		for code := range q.Rates {
			q.Rates[code] *= k
		}
	}
	trim := func(q *source.Quote, keep int) {
		n := 0
		for code := range q.Rates {
			if n++; n > keep {
				delete(q.Rates, code)
			}
		}
	}
	tests := []struct {
		name        string
		mutate      func(in merge.Inputs)
		wantDropped []source.ID
		wantKind    merge.WarningKind
	}{
		{
			name:   "clean inputs pass",
			mutate: func(merge.Inputs) {},
		},
		{
			// T22: EUR-based data labelled USD moves everything by ~11.8%.
			name:        "T22 primary in EUR base",
			mutate:      func(in merge.Inputs) { scale(in.Primary, 1/0.89478) },
			wantDropped: []source.ID{source.Frankfurter},
			wantKind:    merge.WarnSourceDrift,
		},
		{
			name:        "secondary drifts alone",
			mutate:      func(in merge.Inputs) { scale(in.Secondary, 0.95) },
			wantDropped: []source.ID{source.Fawaz},
			wantKind:    merge.WarnSourceDrift,
		},
		{
			name: "both drift together: a real dollar move",
			mutate: func(in merge.Inputs) {
				scale(in.Primary, 1.05)
				scale(in.Secondary, 1.05)
			},
		},
		{
			name:        "low coverage",
			mutate:      func(in merge.Inputs) { trim(in.Secondary, 100) },
			wantDropped: []source.ID{source.Fawaz},
			wantKind:    merge.WarnSourceLowCoverage,
		},
		{
			name: "observation older than 7 days",
			mutate: func(in merge.Inputs) {
				in.Voter.Date = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
			},
			wantDropped: []source.ID{source.ERAPI},
			wantKind:    merge.WarnSourceStaleDate,
		},
		{
			name: "observation more than a day ahead",
			mutate: func(in merge.Inputs) {
				in.Primary.Date = time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
			},
			wantDropped: []source.ID{source.Frankfurter},
			wantKind:    merge.WarnSourceFutureDate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			in := e.inputs()
			tt.mutate(in)
			out, warns := merge.Screen(in, e.prev, e.cfg, e.now)

			kept := map[source.ID]bool{
				source.Frankfurter: out.Primary != nil,
				source.Fawaz:       out.Secondary != nil,
				source.ERAPI:       out.Voter != nil,
			}
			for _, id := range tt.wantDropped {
				if kept[id] {
					t.Errorf("%s kept, want dropped", id)
				}
				kept[id] = true // the rest must be kept
			}
			for id, ok := range kept {
				if !ok {
					t.Errorf("%s dropped unexpectedly", id)
				}
			}
			if len(warns) != len(tt.wantDropped) {
				t.Fatalf("warnings %v, want %d", warns, len(tt.wantDropped))
			}
			for _, w := range warns {
				if w.Kind != tt.wantKind {
					t.Errorf("warning %s, want %s", w.Kind, tt.wantKind)
				}
			}
		})
	}
}

// Screening plus merge on a primary in EUR base publishes the secondary
// instead of an 11% shift.
func TestScreenThenMergeEURBase(t *testing.T) {
	e := newEnv(t)
	in := e.inputs()
	for code := range in.Primary.Rates {
		in.Primary.Rates[code] /= 0.89478
	}
	in, _ = merge.Screen(in, e.prev, e.cfg, e.now)
	res := e.merge(t, in)
	if res.Abort != "" {
		t.Fatalf("abort %s", res.Abort)
	}
	if d := decision(t, res, "EUR"); d.Origin != merge.OriginSecondary {
		t.Errorf("EUR = %v, want the secondary", d)
	}
	if !res.Snapshot.HasFlag("JPY", rates.FlagSecondary) {
		t.Error("JPY not flagged secondary")
	}
}
